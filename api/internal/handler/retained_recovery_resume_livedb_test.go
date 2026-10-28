package handler

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Exercise owner admission as well as the SQL cause filters. A pending retry is
// resumable now; the execution fence and source-worker affinity survive it.
func TestRetainedRecoveryOwnerResumeLiveDB(t *testing.T) {
	for _, cause := range []string{"retained_recovery_exhausted", "retained_recovery_adoption_failed"} {
		t.Run(cause, func(t *testing.T) {
			testRetainedRecoveryOwnerResumeLiveDB(t, cause)
		})
	}
}

func testRetainedRecoveryOwnerResumeLiveDB(t *testing.T, cause string) {
	for _, timed := range []bool{false, true} {
		t.Run(fmt.Sprintf("timed=%v", timed), func(t *testing.T) {
			fx := newEphemeralFixture(t, false)
			h := &Handler{q: fx.q}
			h.cfg.RunTimeout = time.Hour
			w := fx.onlineWorker("retained-resume", true)
			id := fx.queuedRun([]string{})
			_, err := fx.pool.Exec(fx.ctx, `UPDATE runs SET status='recovery_wait',
		worker_id=$2, claim_generation=7, claim_released_at=now(),
		recovery_wait_cause=$4,
		recovery_retry_not_before=CASE WHEN $3::boolean THEN now()+interval '1 hour' END,
		retained_park_count=1, worker_recovery_episode=0, idle_stall_park_count=4,
		requeue_count=7, requeue_episode_baseline=5,
		started_at=now()-interval '10 minutes', budget_wall_seconds=3600,
		status_since=now()-interval '2 minutes' WHERE id=$1`, id, w, timed, cause)
			if err != nil {
				t.Fatal(err)
			}
			before, err := fx.q.GetRunByID(fx.ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if rec := recoveryResumeRequest(h, id, uuid.New()); rec.Code != http.StatusNotFound {
				t.Fatalf("foreign Resume status=%d", rec.Code)
			}
			rec := recoveryResumeRequest(h, id, fx.userID)
			if rec.Code != http.StatusOK {
				t.Fatalf("Resume status=%d body=%s", rec.Code, rec.Body.String())
			}
			after, err := fx.q.GetRunByID(fx.ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if after.Status != "queued" || after.WorkerRecoveryEpisode != 1 || after.RetainedParkCount != 1 ||
				after.RequeueCount != 7 || after.RequeueEpisodeBaseline != 7 || after.IdleStallParkCount != before.IdleStallParkCount ||
				after.ClaimReleasedAt != before.ClaimReleasedAt || after.WorkerID != before.WorkerID ||
				after.StartedAt != before.StartedAt || after.BudgetPausedSeconds < 120 ||
				after.RecoveryRetryNotBefore.Valid || after.RecoveryWaitCause.Valid {
				t.Fatal("Resume changed counter, affinity, execution fence or wall accounting incorrectly")
			}
			// The claim grant jumps from the journal's pre-park 0 to 2; the worker
			// still renews once by the existing higher-episode/new-generation rule.
			if after.WorkerRecoveryEpisode+int64(after.RetainedParkCount) != 2 {
				t.Fatal("Resume lost the pending automatic grant")
			}
			if rec := recoveryResumeRequest(h, id, fx.userID); rec.Code != http.StatusConflict {
				t.Fatalf("duplicate Resume status=%d", rec.Code)
			}
			// Other timed parks must not become owner-resumable through this endpoint.
			if _, err := fx.pool.Exec(fx.ctx, "UPDATE runs SET status='recovery_wait', recovery_wait_cause='forge_unreachable' WHERE id=$1", id); err != nil {
				t.Fatal(err)
			}
			if rec := recoveryResumeRequest(h, id, fx.userID); rec.Code != http.StatusConflict {
				t.Fatalf("unrelated cause Resume status=%d", rec.Code)
			}
		})
	}
}
