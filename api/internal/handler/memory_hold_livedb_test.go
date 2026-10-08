package handler

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestMemoryHoldOwnerOnlyReleaseLiveDB(t *testing.T) {
	fx := newEphemeralFixture(t, false)
	worker := fx.onlineWorker("memory owner hold", true)
	id := fx.queuedRun([]string{})
	if _, err := fx.pool.Exec(fx.ctx, `UPDATE runs SET status='recovery_wait',
        recovery_wait_cause='worker_memory_pressure',claim_generation=2,worker_id=$2,
        claim_released_at=now(),released_worker_id=$2,released_worker_nonce='held',
        memory_policy='{"version":1,"max_interventions":3}',memory_intervention_count=3,
        requeue_count=4,requeue_episode_baseline=2,worker_recovery_episode=1,
        recovery_retry_not_before=now()-interval '1 minute' WHERE id=$1`, id, worker); err != nil {
		t.Fatal(err)
	}
	assertHeld := func() {
		t.Helper()
		r, err := fx.q.GetRunByID(fx.ctx, id)
		if err != nil || r.Status != "recovery_wait" || r.MemoryEpisode != 0 || r.MemoryInterventionCount != 3 {
			t.Fatalf("hold released: status=%s episode=%d used=%d err=%v", r.Status, r.MemoryEpisode, r.MemoryInterventionCount, err)
		}
	}
	h := &Handler{q: fx.q}
	if r := recoveryResumeRequest(h, id, uuid.New()); r.Code != http.StatusNotFound {
		t.Fatalf("foreign Resume=%d %s", r.Code, r.Body.String())
	}
	assertHeld()
	rows, err := fx.q.PromoteRecoveryWaitRuns(fx.ctx, pgconv.Time(time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.ID == id {
			t.Fatal("timer released memory hold")
		}
	}
	n, err := fx.q.PromoteRecoveryWaitRunNow(fx.ctx, store.PromoteRecoveryWaitRunNowParams{ID: id, UserID: fx.userID})
	if err != nil || n != 0 {
		t.Fatalf("credential release=%d %v", n, err)
	}
	if _, err = fx.q.CreateApprovePlanInput(fx.ctx, store.CreateApprovePlanInputParams{RunID: id}); err != nil {
		t.Fatal(err)
	}
	if _, err = fx.q.ResumeWorkerRecoveryEpisode(fx.ctx, store.ResumeWorkerRecoveryEpisodeParams{ID: id, UserID: fx.userID}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("worker-death Resume released memory hold: %v", err)
	}
	assertHeld()
	if r := recoveryResumeRequest(h, id, fx.userID); r.Code != http.StatusOK {
		t.Fatalf("owner Resume=%d %s", r.Code, r.Body.String())
	}
	r, err := fx.q.GetRunByID(fx.ctx, id)
	if err != nil || r.Status != "queued" || r.MemoryEpisode != 1 || r.MemoryInterventionCount != 0 ||
		r.RequeueCount != 4 || r.RequeueEpisodeBaseline != 2 || r.WorkerRecoveryEpisode != 1 ||
		!r.ClaimReleasedAt.Valid || r.ReleasedWorkerNonce.String != "held" {
		t.Fatalf("owner memory Resume counters/fences: %+v err=%v", r, err)
	}
}
