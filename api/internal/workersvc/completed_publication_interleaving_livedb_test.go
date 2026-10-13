package workersvc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// These are store transaction interleavings, not a paused composed handler.
// Completion identity and receipt replay use the public service seam.
func TestCompletedPublicationInterleavingLiveDB(t *testing.T) {
	for _, binding := range []string{"repo", "connection", "delete"} {
		for _, writerFirst := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/writer_first_%t", binding, writerFirst), func(t *testing.T) {
				e := setupInterlockLiveDB(t)
				ctx, cancel := context.WithTimeout(e.ctx, 12*time.Second)
				defer cancel()
				caps := []string{capability.RecoveryCompletedPublicationV1}
				wid := e.seedWorker(t, caps)
				run := e.seedLegacyRunningRun(t, wid)
				e.exec(t, "UPDATE runs SET claim_generation=1 WHERE id=$1", run)
				hold := uuid.New()
				e.exec(t, `INSERT INTO recovery_custody_holds(id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,live_worker_id,live_run_id,inventory_guarded)
VALUES($1,$2,$3,$4,1,'open',$5,'ident',$5,$4,true)`, hold, e.userID, e.repoID, run, wid)
				gen, mr := int64(1), int64(7)
				head, branch := strings.Repeat("a", 40), "agent/issue-1"
				f := &publicationForge{t: t, projectID: 1, mrIID: mr, expectedBranch: branch, branch: branch, head: head, summaryHead: head, ancestry: forge.AncestryUnknown}
				svc := e.permitService(t)
				svc.SetForges(settleUnitBuilder{f: f})
				w := store.Worker{ID: wid, UserID: e.userID, ProtocolCapabilities: caps}
				req := StateRequest{State: "completed", ClaimGeneration: &gen, CompletionFinalHead: &head, Branch: &branch, MrIID: &mr}
				initial, err := svc.SetStateReportWithReconciliation(ctx, w, run, req)
				if err != nil || initial.CompletedPublicationReason != "ancestry_unknown" || f.compareCalls != 1 {
					t.Fatalf("public completion/proof: %+v %v", initial, err)
				}
				hp := store.GetCompletedPublicationHoldParams{RunID: run, UserID: e.userID, WorkerID: wid, Generation: gen}
				frozen, err := e.q.GetCompletedPublicationHold(ctx, hp)
				if err != nil {
					t.Fatal(err)
				}
				capture := uuid.New()
				e.exec(t, `INSERT INTO recovery_captures(id,hold_id,run_id,user_id,original_worker_id,original_worker_identity,source_sha,idempotency_key,state,expires_at,ready_retention_seconds)
VALUES($1,$2,$3,$4,$5,'ident',$6,$7,'available',now()+interval '7 days',604800)`, capture, hold, run, e.userID, wid, head, uuid.NewString())
				var archiveBefore string
				if err := e.pool.QueryRow(ctx, "SELECT row_to_json(c)::text FROM recovery_captures c WHERE id=$1", capture).Scan(&archiveBefore); err != nil {
					t.Fatal(err)
				}
				bindingRow, err := e.q.GetCompletedPublicationBinding(ctx, store.GetCompletedPublicationBindingParams{RepoID: e.repoID, UserID: e.userID})
				if err != nil {
					t.Fatal(err)
				}
				begin := func() (pgx.Tx, int32) {
					t.Helper()
					tx, err := e.pool.Begin(ctx)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
					var pid int32
					if err := tx.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
						t.Fatal(err)
					}
					return tx, pid
				}
				release, releasePID := begin()
				writer, writerPID := begin()
				rq, wq := store.New(release), store.New(writer)
				lockCanonical := func(q *store.Queries) error {
					if _, err := q.GetWorkerForUpdate(ctx, wid); err != nil {
						return err
					}
					if _, err := q.GetRunOwnedByWorkerForUpdate(ctx, store.GetRunOwnedByWorkerForUpdateParams{ID: run, WorkerID: pgconv.UUID(wid)}); err != nil {
						return err
					}
					_, err := q.GetFinalInventoryHold(ctx, store.GetFinalInventoryHoldParams{RunID: run, UserID: e.userID, WorkerID: wid, Generation: gen})
					return err
				}
				mutate := func() error {
					switch binding {
					case "repo":
						_, err := writer.Exec(ctx, "UPDATE repos SET forge_project_id=2 WHERE id=$1", e.repoID)
						return err
					case "connection":
						_, err := writer.Exec(ctx, "UPDATE forge_connections SET base_url='https://changed.e2e' WHERE id=$1", bindingRow.ConnectionID)
						return err
					default:
						n, err := wq.DeleteForgeConnectionForUser(ctx, store.DeleteForgeConnectionForUserParams{ID: bindingRow.ConnectionID, UserID: e.userID})
						if err == nil && n != 1 {
							return fmt.Errorf("delete affected %d rows", n)
						}
						return err
					}
				}
				releaseCAS := func() (int64, error) {
					if err := lockCanonical(rq); err != nil {
						return 0, err
					}
					if _, err := rq.LockCompletedPublicationBinding(ctx, store.LockCompletedPublicationBindingParams{RepoID: e.repoID, UserID: e.userID}); err != nil {
						return 0, err
					}
					return rq.ReleaseCompletedPublicationHold(ctx, store.ReleaseCompletedPublicationHoldParams{HoldID: hold, RunID: run, UserID: e.userID, WorkerID: wid, Generation: gen, Identity: frozen.CompletionIdentity, ObservedBranchHead: head})
				}
				waitBlocked := func(waiter, blocker int32) {
					t.Helper()
					// Poll for at most the shared 12-second deadline; no timing-only assertion.
					ticker := time.NewTicker(10 * time.Millisecond)
					defer ticker.Stop()
					for {
						var blocked bool
						if err := e.pool.QueryRow(ctx, "SELECT $2::int = ANY(pg_blocking_pids($1::int))", waiter, blocker).Scan(&blocked); err != nil {
							t.Fatal(err)
						}
						if blocked {
							return
						}
						select {
						case <-ctx.Done():
							t.Fatal("expected PostgreSQL blocker not observed")
						case <-ticker.C:
						}
					}
				}
				type outcome struct {
					n   int64
					err error
				}
				done := make(chan outcome, 1)
				if writerFirst {
					if binding == "delete" {
						// The production deletion query takes this parent lock before cascading.
						if _, err := wq.GetRunOwnedByWorkerForUpdate(ctx, store.GetRunOwnedByWorkerForUpdateParams{ID: run, WorkerID: pgconv.UUID(wid)}); err != nil {
							t.Fatal(err)
						}
					} else if err := mutate(); err != nil {
						t.Fatal(err)
					}
					go func() { n, err := releaseCAS(); done <- outcome{n, err} }()
					waitBlocked(releasePID, writerPID)
					if binding == "delete" {
						err := mutate()
						var pe *pgconn.PgError
						if !errors.As(err, &pe) || pe.Code != "23503" {
							t.Fatalf("open custody must refuse deletion with FK violation: %v", err)
						}
						current, err := e.q.GetCompletedPublicationHold(ctx, hp)
						if err != nil || current.State != "open" || len(current.CompletedPublicationReceipt) != 0 {
							t.Fatalf("refused deletion must retain without receipt: %+v %v", current, err)
						}
						if err := writer.Rollback(ctx); err != nil {
							t.Fatal(err)
						}
					} else if err := writer.Commit(ctx); err != nil {
						t.Fatal(err)
					}
					result := <-done
					want := int64(0)
					if binding == "delete" {
						want = 1
					}
					if result.err != nil || result.n != want {
						t.Fatalf("release CAS rows=%d want=%d err=%v", result.n, want, result.err)
					}
					if err := release.Commit(ctx); err != nil {
						t.Fatal(err)
					}
				} else {
					n, err := releaseCAS()
					if err != nil || n != 1 {
						t.Fatalf("release CAS: %d %v", n, err)
					}
					go func() { done <- outcome{err: mutate()} }()
					waitBlocked(writerPID, releasePID)
					if err := release.Commit(ctx); err != nil {
						t.Fatal(err)
					}
					result := <-done
					if result.err != nil {
						var pe *pgconn.PgError
						if binding != "delete" || !errors.As(result.err, &pe) || pe.Code != "23503" {
							t.Fatal(result.err)
						}
						if err := writer.Rollback(ctx); err != nil {
							t.Fatal(err)
						}
					} else if err := writer.Commit(ctx); err != nil {
						t.Fatal(err)
					}
				}
				stored, err := e.q.GetCompletedPublicationHold(ctx, hp)
				if err != nil {
					t.Fatal(err)
				}
				released := !writerFirst || binding == "delete"
				if !released {
					if stored.State != "open" || len(stored.CompletedPublicationReceipt) != 0 {
						t.Fatalf("changed identity must retain: %+v", stored)
					}
					replay, err := svc.SetStateReportWithReconciliation(ctx, w, run, req)
					if err != nil || replay.CompletedPublicationReceipt != nil || replay.CompletedPublicationReason != "identity_changed" {
						t.Fatalf("changed binding service refusal: %+v %v", replay, err)
					}
				} else {
					if stored.State != "released" || len(stored.CompletedPublicationReceipt) == 0 || !bytes.Equal(stored.CompletionIdentity, frozen.CompletionIdentity) {
						t.Fatalf("coherent frozen receipt release: %+v", stored)
					}
					svc.SetForges(nil)
					replay, err := svc.SetStateReportWithReconciliation(ctx, w, run, req)
					if err != nil || replay.CompletedPublicationReceipt == nil {
						t.Fatalf("exact stored receipt replay after writer/forge loss must succeed (ErrRunNotFound is a regression): %+v %v", replay, err)
					}
					var expected apitypes.CompletedPublicationReceipt
					if err := json.Unmarshal(stored.CompletedPublicationReceipt, &expected); err != nil {
						t.Fatal(err)
					}
					got, _ := json.Marshal(replay.CompletedPublicationReceipt)
					want, _ := json.Marshal(expected)
					if !bytes.Equal(got, want) || replay.Run.ID != run || replay.Run.Status != "completed" || replay.Run.ClaimGeneration != gen || replay.Run.WorkerID != pgconv.UUID(wid) || !replay.Applied {
						t.Fatalf("exact stored receipt and frozen completed ACK: %+v", replay)
					}
					after, err := e.q.GetCompletedPublicationHold(ctx, hp)
					if err != nil || !bytes.Equal(after.CompletedPublicationReceipt, stored.CompletedPublicationReceipt) {
						t.Fatalf("writer/replay revoked receipt: %v", err)
					}
				}
				var archiveAfter string
				if err := e.pool.QueryRow(ctx, "SELECT row_to_json(c)::text FROM recovery_captures c WHERE id=$1", capture).Scan(&archiveAfter); err != nil {
					t.Fatal(err)
				}
				if archiveAfter != archiveBefore {
					t.Fatal("publication interleaving changed archive retention")
				}
			})
		}
	}
}
