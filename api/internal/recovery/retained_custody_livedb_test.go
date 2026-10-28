package recovery

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// The run holder must still be able to lock custody while v1 release waits.
// Deadlines and the operation helpers bound every wait and drain the release.
func TestRetainedCustodyV1ReleaseRunBeforeHoldLiveDB(t *testing.T) {
	for _, cause := range []string{"retained_recovery_exhausted", "retained_recovery_adoption_failed"} {
		t.Run(cause, func(t *testing.T) {
			testRetainedCustodyV1ReleaseRunBeforeHoldLiveDB(t, cause)
		})
	}
}

func testRetainedCustodyV1ReleaseRunBeforeHoldLiveDB(t *testing.T, cause string) {
	for _, timed := range []bool{false, true} {
		t.Run(fmt.Sprintf("timed=%v", timed), func(t *testing.T) {
			e := newInventoryEnv(t)
			e.exec("UPDATE recovery_custody_holds SET inventory_guarded=false WHERE id=$1", e.hold)
			ctx, cancel := context.WithTimeout(e.ctx, 8*time.Second)
			defer cancel()
			tx, err := e.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(context.Background()) }()
			var id uuid.UUID
			if err := tx.QueryRow(ctx, "SELECT id FROM runs WHERE id=$1 FOR UPDATE", e.run).Scan(&id); err != nil {
				t.Fatal(err)
			}
			release := startReconcileOperation(t, func(ctx context.Context) (apitypes.RecoveryReleaseResponse, error) {
				return e.svc.Release(ctx, e.w, e.run, apitypes.RecoveryReleaseRequest{})
			})
			// Observe the actual wait behind our run lock, even on the unfixed path.
			waitReconcileLock(t, e, int32(tx.Conn().PgConn().PID()), "FOR UPDATE") //nolint:gosec // G115: PostgreSQL backend PID fits int32.
			if err := tx.QueryRow(ctx, "SELECT id FROM recovery_custody_holds WHERE id=$1 FOR UPDATE NOWAIT", e.hold).Scan(&id); err != nil {
				t.Fatalf("v1 release locked hold before run: %v", err)
			}
			if _, err := tx.Exec(ctx, "UPDATE runs SET status='recovery_wait', recovery_wait_cause=$3, recovery_retry_not_before=CASE WHEN $2::boolean THEN now()+interval '1 hour' END, claim_released_at=now() WHERE id=$1", e.run, timed, cause); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			got := awaitReconcileOperation(t, release)
			if got.err != nil || got.value.Released || got.value.HoldsReleased != 0 {
				t.Fatalf("v1 release after retained park: %+v err=%v", got.value, got.err)
			}
			var retained bool
			if err := e.pool.QueryRow(e.ctx, "SELECT state='open' AND live_worker_id=$2 AND live_run_id=$3 AND released_at IS NULL FROM recovery_custody_holds WHERE id=$1", e.hold, e.w.ID, e.run).Scan(&retained); err != nil || !retained {
				t.Fatalf("retained custody retained=%v err=%v", retained, err)
			}
		})
	}
}

func TestRetainedCustodyWorkerFinalReleaseRefusedLiveDB(t *testing.T) {
	for _, cause := range []string{"retained_recovery_exhausted", "retained_recovery_adoption_failed"} {
		t.Run(cause, func(t *testing.T) {
			testRetainedCustodyWorkerFinalReleaseRefusedLiveDB(t, cause)
		})
	}
}

func testRetainedCustodyWorkerFinalReleaseRefusedLiveDB(t *testing.T, cause string) {
	for _, timed := range []bool{false, true} {
		t.Run(fmt.Sprintf("timed=%v", timed), func(t *testing.T) {
			for _, kind := range []string{"archive", "settled"} {
				t.Run(kind, func(t *testing.T) {
					e := newInventoryEnv(t)
					id := e.reserve("retained-final")
					body := e.upload(id)
					e.exec("UPDATE runs SET status='recovery_wait', recovery_wait_cause=$3, recovery_retry_not_before=CASE WHEN $2::boolean THEN now()+interval '1 hour' END, claim_released_at=now() WHERE id=$1", e.run, timed, cause)
					req := e.request(id)
					if kind == "settled" {
						evidence := "forge_no_output"
						req = apitypes.RecoveryReleaseRequest{Generation: &e.gen, ReleaseEvidence: &evidence,
							FinalDisposition: &apitypes.RecoveryFinalDisposition{Kind: "settled", CoverageDigest: emptyInventoryDigest}}
					}
					e.reject(e.w, req, ErrNotAuthorized)
					var state string
					var retained bool
					if err := e.pool.QueryRow(e.ctx, "SELECT state, live_worker_id=$2 AND live_run_id=$3 AND released_at IS NULL AND release_evidence IS NULL FROM recovery_custody_holds WHERE id=$1", e.hold, e.w.ID, e.run).Scan(&state, &retained); err != nil {
						t.Fatal(err)
					}
					if state != "open" || !retained {
						t.Fatalf("worker %s released retained custody: state=%s retained=%v", kind, state, retained)
					}
					e.bytes(id, body)
					// Once the owner ends the hold, ordinary final release remains available.
					e.exec("UPDATE runs SET status='cancelled', recovery_wait_cause=NULL WHERE id=$1", e.run)
					e.release(req)
				})
			}
		})
	}
}

func TestRetainedCustodyLegacyWorkerReleaseRefusedLiveDB(t *testing.T) {
	for _, cause := range []string{"retained_recovery_exhausted", "retained_recovery_adoption_failed"} {
		t.Run(cause, func(t *testing.T) {
			testRetainedCustodyLegacyWorkerReleaseRefusedLiveDB(t, cause)
		})
	}
}

func testRetainedCustodyLegacyWorkerReleaseRefusedLiveDB(t *testing.T, cause string) {
	for _, timed := range []bool{false, true} {
		t.Run(fmt.Sprintf("timed=%v", timed), func(t *testing.T) {
			e := newInventoryEnv(t)
			e.exec("UPDATE recovery_custody_holds SET inventory_guarded=false WHERE id=$1", e.hold)
			id := e.reserve("legacy-retained")
			body := e.upload(id)
			e.exec("UPDATE runs SET status='recovery_wait', recovery_wait_cause=$3, recovery_retry_not_before=CASE WHEN $2::boolean THEN now()+interval '1 hour' END, claim_released_at=now() WHERE id=$1", e.run, timed, cause)
			for _, generation := range []*int64{&e.gen, nil} {
				ack, err := e.svc.Release(e.ctx, e.w, e.run, apitypes.RecoveryReleaseRequest{Generation: generation})
				if err != nil || ack.Released || ack.HoldsReleased != 0 {
					t.Fatalf("legacy worker released retained custody: ack=%+v err=%v", ack, err)
				}
			}
			var state string
			if err := e.pool.QueryRow(e.ctx, "SELECT state FROM recovery_custody_holds WHERE id=$1", e.hold).Scan(&state); err != nil {
				t.Fatal(err)
			}
			if state != "open" {
				t.Fatalf("legacy custody state=%s", state)
			}
			e.bytes(id, body)
		})
	}
}

func TestRetainedCustodyQueuedProofBoundaryLiveDB(t *testing.T) {
	for _, promotion := range []string{"timer", "owner"} {
		t.Run(promotion, func(t *testing.T) {
			e := newInventoryEnv(t)
			capture := e.reserve("queued-proof")
			body := e.upload(capture)
			preparing := e.reserve("queued-preparing")
			e.exec("UPDATE runs SET status='recovery_wait', recovery_wait_cause='retained_recovery_exhausted', retained_park_count=1, claim_released_at=now(), recovery_retry_not_before=now()-interval '1 second' WHERE id=$1", e.run)
			if promotion == "timer" {
				if _, err := e.q.PromoteRecoveryWaitRuns(e.ctx, pgconv.Time(time.Now())); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := e.q.ResumeWorkerRecoveryEpisode(e.ctx, store.ResumeWorkerRecoveryEpisodeParams{
					ID: e.run, UserID: e.w.UserID, GlobalTimeoutSeconds: 3600,
				}); err != nil {
					t.Fatal(err)
				}
			}
			run, err := e.q.GetRunByID(e.ctx, e.run)
			if err != nil || run.Status != "queued" || !run.ClaimReleasedAt.Valid {
				t.Fatalf("queued fence status=%s err=%v", run.Status, err)
			}
			custodyAndBytes := func() string {
				t.Helper()
				var snapshot string
				if err := e.pool.QueryRow(e.ctx, `SELECT json_build_array(row_to_json(h),
                    (SELECT json_agg(row_to_json(c) ORDER BY c.capture_id,c.chunk_index)
                     FROM recovery_capture_chunks c JOIN recovery_captures a ON a.id=c.capture_id WHERE a.hold_id=h.id))::text
                    FROM recovery_custody_holds h WHERE h.id=$1`, e.hold).Scan(&snapshot); err != nil {
					t.Fatal(err)
				}
				return snapshot
			}
			before := custodyAndBytes()
			assertPreserved := func() {
				t.Helper()
				if custodyAndBytes() != before {
					t.Fatal("unproven queued release changed custody or encrypted bytes")
				}
			}
			// Both requests bind the existing worker, generation and hold. The
			// queued execution fence does not make unfinished or expired proof final.
			e.reject(e.w, e.request(preparing), ErrManifestConflict)
			assertPreserved()
			e.bytes(capture, body)
			e.exec("UPDATE recovery_captures SET expires_at=now()-interval '1 second' WHERE id=$1", capture)
			e.reject(e.w, e.request(capture), ErrManifestConflict)
			assertPreserved()
			e.exec("UPDATE recovery_captures SET expires_at=now()+interval '1 hour' WHERE id=$1", capture)
			e.bytes(capture, body)
			var open bool
			if err := e.pool.QueryRow(e.ctx, "SELECT state='open' FROM recovery_custody_holds WHERE id=$1", e.hold).Scan(&open); err != nil || !open {
				t.Fatalf("unproven queued release open=%v err=%v", open, err)
			}
			// Once the park has ended, existing complete FINAL archive proof
			// remains sufficient; no new custody-lifetime marker is introduced.
			e.release(e.request(capture))
		})
	}
}
