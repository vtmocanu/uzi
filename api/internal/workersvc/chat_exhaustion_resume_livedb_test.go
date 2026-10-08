package workersvc

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Each case owns its user and workers. No repo is created: this exercises the chat lane.
func TestChatExhaustionResumeLiveDB(t *testing.T) {
	for _, path := range []string{"stale", "registration"} {
		for _, nonce := range []string{"nonnull", "null"} {
			for _, reclaim := range []string{"different_worker", "reregister"} {
				t.Run(path+"/"+nonce+"/"+reclaim, func(t *testing.T) {
					env := setupCodexLiveDB(t)
					user := env.seedUser(t)
					worker := seedSnapshotWorker(t, env, user, "incarnation-A")
					if nonce == "null" {
						env.exec("UPDATE workers SET snapshot_register_nonce=NULL WHERE id=$1", worker)
					}
					id := uuid.New()
					_, err := env.q.CreateChatRun(env.ctx, store.CreateChatRunParams{
						RunID: id, UserID: user, IssueTitle: "chat", IssueDescription: "hello", Title: pgtype.Text{String: "chat", Valid: true},
					})
					if err != nil {
						t.Fatal(err)
					}
					claim := func(w uuid.UUID) (store.Run, error) {
						return env.q.ClaimChatRun(env.ctx, store.ClaimChatRunParams{
							WorkerID: pgconv.UUID(w), UserID: user,
							AffinityCutoff: pgconv.Time(time.Now().Add(-time.Hour)),
						})
					}
					initial, err := claim(worker)
					if err != nil {
						t.Fatal(err)
					}
					if initial.ClaimGeneration != 1 {
						t.Errorf("fresh chat claim generation=%d, want 1", initial.ClaimGeneration)
					}
					// Seed an exhausted flight at generation 2 independently of the first-claim check.
					env.exec("UPDATE runs SET status='running',started_at=now(),claim_generation=2,requeue_count=2 WHERE id=$1", id)
					svc := snapshotSvc(env, testParams())
					hook := &exhaustionTestBeginner{pool: env.pool, mode: "read"}
					switch path {
					case "stale":
						env.exec("UPDATE workers SET last_heartbeat_at=now()-interval '10 minutes' WHERE id=$1", worker)
						tx, err := hook.Begin(env.ctx)
						if err != nil {
							t.Fatal(err)
						}
						defer func() { _ = tx.Rollback(env.ctx) }()
						rows, err := store.New(tx).FailRunsOfStaleWorkersOverCap(env.ctx, store.FailRunsOfStaleWorkersOverCapParams{
							FailureReason: pgtype.Text{String: "worker requeue budget exhausted", Valid: true},
							MaxRequeues:   1, FailCutoff: pgconv.Time(time.Now().Add(-time.Minute)),
						})
						if err != nil {
							t.Fatalf("stale exhaustion: %v", err)
						}
						matches := 0
						for _, row := range rows {
							if row.ID != id {
								continue
							}
							matches++
							if row.Status != "recovery_wait" {
								t.Fatalf("stale exhaustion run=%s status=%s, want recovery_wait", id, row.Status)
							}
						}
						if matches != 1 {
							t.Fatalf("stale exhaustion run=%s matches=%d, want 1", id, matches)
						}
						if err := tx.Commit(env.ctx); err != nil {
							t.Fatal(err)
						}
					case "registration":
						svc.SetTxBeginner(hook)
						if _, _, err := svc.Register(env.ctx, hbWorker(t, env, worker), "fixture", "", nil, nil, nil, nil); err != nil {
							t.Fatal(err)
						}
						svc.SetTxBeginner(env.pool)
					}
					held := exhaustionRun(t, env, id)
					var evidence store.WorkerRecoveryEvidence
					if err := json.Unmarshal(held.WorkerRecoveryEvidence, &evidence); err != nil {
						t.Fatal(err)
					}
					if held.Status != "recovery_wait" || held.RecoveryWaitCause.String != "worker_requeue_exhausted" ||
						!held.ClaimReleasedAt.Valid || !evidence.Unknown || held.ReleasedWorkerID != pgconv.UUID(worker) {
						t.Fatalf("unknown exhaustion did not fence chat: %+v", held)
					}
					if held.ReleasedWorkerNonce.Valid != (nonce == "nonnull") ||
						(nonce == "nonnull" && held.ReleasedWorkerNonce.String != "incarnation-A") {
						t.Fatalf("released nonce=%v", held.ReleasedWorkerNonce)
					}
					if rows, err := env.q.SetRunCompleted(env.ctx, store.SetRunCompletedParams{ID: id, WorkerID: pgconv.UUID(worker)}); err != nil || rows != 0 {
						t.Fatalf("SQL completed released claim: rows=%d err=%v", rows, err)
					}
					oldGeneration := int64(2)
					if row, err := env.q.InsertRunMessage(env.ctx, store.InsertRunMessageParams{
						RunID: id, ClaimGeneration: pgtype.Int8{Int64: 2, Valid: true},
						Seq: 1, Kind: "text", Payload: []byte(`{"text":"released"}`),
					}); err != nil || row.Inserted || row.GenerationLive {
						t.Fatalf("released incarnation message accepted: row=%+v err=%v", row, err)
					}
					if _, applied, err := svc.SetState(env.ctx, hbWorker(t, env, worker), id, StateRequest{State: "completed", ClaimGeneration: &oldGeneration}); applied || (err != nil && !errors.Is(err, ErrStaleClaim)) {
						t.Fatalf("service completed released claim: applied=%v err=%v", applied, err)
					}
					if _, err := env.q.ResumeWorkerRecoveryEpisode(env.ctx, store.ResumeWorkerRecoveryEpisodeParams{
						ID: id, UserID: user, GlobalTimeoutSeconds: 3600,
					}); err != nil {
						t.Fatal(err)
					}
					resumed := exhaustionRun(t, env, id)
					if resumed.Status != "queued" || resumed.WorkerID.Valid || !resumed.ClaimReleasedAt.Valid ||
						resumed.ClaimGeneration != 2 || resumed.ReleasedWorkerID != held.ReleasedWorkerID ||
						resumed.ReleasedWorkerNonce != held.ReleasedWorkerNonce {
						t.Fatalf("owner Resume lost fence: %+v", resumed)
					}
					// Register rotates the nonce before parking. Restore the released incarnation
					// to prove exclusion of the exact pair, including NULL IS NOT DISTINCT FROM NULL.
					env.exec("UPDATE workers SET snapshot_register_nonce=$2,last_heartbeat_at=now() WHERE id=$1", worker, held.ReleasedWorkerNonce)
					if got, err := claim(worker); !errors.Is(err, pgx.ErrNoRows) {
						t.Errorf("released incarnation reclaimed chat: id=%s err=%v", got.ID, err)
						return
					}
					excluded := exhaustionRun(t, env, id)
					if excluded.ClaimReleasedAt != resumed.ClaimReleasedAt || excluded.ReleasedWorkerID != held.ReleasedWorkerID ||
						excluded.ReleasedWorkerNonce != held.ReleasedWorkerNonce || excluded.ClaimGeneration != 2 {
						t.Fatal("failed claim changed release markers")
					}
					claimant := worker
					if reclaim == "different_worker" {
						claimant = seedSnapshotWorker(t, env, user, "incarnation-B")
					} else {
						if _, _, err := svc.Register(env.ctx, hbWorker(t, env, worker), "fixture", "", nil, nil, nil, nil); err != nil {
							t.Fatal(err)
						}
					}
					// These display/provenance fields must describe only the old flight.
					env.exec("UPDATE runs SET stale_requeue_generation=2,checkpoint_contains_latest=true,worker_recovery_evidence=$2 WHERE id=$1", id, held.WorkerRecoveryEvidence)
					fresh, err := claim(claimant)
					if err != nil {
						t.Fatal(err)
					}
					if fresh.ID != id || fresh.Kind != "chat" || fresh.RepoID.Valid || fresh.ClaimGeneration != 3 ||
						fresh.ClaimReleasedAt.Valid || fresh.ReleasedWorkerID.Valid || fresh.ReleasedWorkerNonce.Valid ||
						fresh.StaleRequeueGeneration.Valid || fresh.CheckpointContainsLatest.Valid || len(fresh.WorkerRecoveryEvidence) != 0 {
						t.Errorf("fresh chat claim retained old flight: %+v", fresh)
					}
					appendMessage := func(g int64, seq int32) store.InsertRunMessageRow {
						row, err := env.q.InsertRunMessage(env.ctx, store.InsertRunMessageParams{
							RunID: id, ClaimGeneration: pgtype.Int8{Int64: g, Valid: true},
							Seq: seq, Kind: "text", Payload: []byte(`{"text":"hello"}`),
						})
						if err != nil {
							t.Fatal(err)
						}
						return row
					}
					if row := appendMessage(2, 1); row.Inserted || row.GenerationLive {
						t.Fatalf("old generation message accepted: %+v", row)
					}
					if reclaim == "different_worker" {
						if _, applied, err := svc.SetState(env.ctx, hbWorker(t, env, worker), id, StateRequest{State: "completed", ClaimGeneration: &oldGeneration}); applied || !errors.Is(err, ErrRunNotOwned) {
							t.Fatalf("old worker completion accepted: applied=%v err=%v", applied, err)
						}
					}
					claimantWorker := hbWorker(t, env, claimant)
					claimantWorker.ProtocolCapabilities = append(claimantWorker.ProtocolCapabilities, capability.CredentialSwitchV1)
					if _, applied, err := svc.SetState(env.ctx, claimantWorker, id, StateRequest{State: "running"}); err != nil || !applied {
						t.Fatalf("fresh chat nil-generation capability state rejected: applied=%v err=%v", applied, err)
					}
					if err := svc.AppendMessagesForClaim(env.ctx, claimantWorker, id, []IncomingMessage{
						{Seq: 2, Kind: "text", Payload: []byte(`{"text":"stale"}`)},
					}, &oldGeneration); !errors.Is(err, ErrStaleClaim) {
						t.Fatalf("old generation service message accepted: err=%v", err)
					}
					if _, applied, err := svc.SetState(env.ctx, claimantWorker, id, StateRequest{State: "completed", ClaimGeneration: &oldGeneration}); applied || !errors.Is(err, ErrStaleClaim) {
						t.Fatalf("old generation service completion accepted: applied=%v err=%v", applied, err)
					}
					if row := appendMessage(3, 1); !row.Inserted || !row.GenerationLive {
						t.Fatalf("fresh generation message rejected: %+v", row)
					}
					generation := int64(3)
					if err := svc.AppendMessagesForClaim(env.ctx, claimantWorker, id, []IncomingMessage{
						{Seq: 2, Kind: "text", Payload: []byte(`{"text":"legacy"}`)},
					}, nil); err != nil {
						t.Fatalf("fresh chat nil-generation capability message rejected: %v", err)
					}
					if err := svc.AppendMessagesForClaim(env.ctx, claimantWorker, id, []IncomingMessage{
						{Seq: 3, Kind: "text", Payload: []byte(`{"text":"fresh"}`)},
					}, &generation); err != nil {
						t.Fatalf("fresh chat stamped service message rejected: %v", err)
					}
					completed, applied, err := svc.SetState(env.ctx, hbWorker(t, env, claimant), id, StateRequest{State: "completed", ClaimGeneration: &generation})
					if err != nil || !applied || completed.Status != "completed" {
						t.Fatalf("fresh completion status=%s applied=%v err=%v", completed.Status, applied, err)
					}
				})
			}
		}
	}
}
