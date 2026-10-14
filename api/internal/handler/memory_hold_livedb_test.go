package handler

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Cancel uses the real handler, service and SQL, with a fresh online worker:
// recovery_wait has no poller for this run even while its worker heartbeats.
// Unlike ResumeMemoryHold, CancelRunServerSide ends the run without opening a
// memory episode, refunding interventions or resetting the released incarnation.
func TestMemoryHoldOwnerOnlyCancelLiveDB(t *testing.T) {
	fx := newEphemeralFixture(t, false)
	owner := store.User{ID: fx.userID, IsActive: true}
	foreign := store.User{ID: uuid.New(), IsActive: true}
	admin := store.User{ID: uuid.New(), IsActive: true, IsAdmin: true}
	// Registered after newEphemeralFixture: delete only our unique users before
	// the pool closes, including on an assertion or seed failure.
	t.Cleanup(func() {
		if _, err := fx.pool.Exec(fx.ctx, "DELETE FROM users WHERE id IN ($1,$2,$3)", owner.ID, foreign.ID, admin.ID); err != nil {
			t.Errorf("cleanup memory hold cancel users: %v", err)
		}
	})
	for _, user := range []store.User{foreign, admin} {
		if _, err := fx.pool.Exec(fx.ctx,
			"INSERT INTO users (id,email,password_hash,is_admin) VALUES ($1,$2,'x',$3)",
			user.ID, "memory-cancel-"+user.ID.String()+"@e2e", user.IsAdmin); err != nil {
			t.Fatal(err)
		}
	}
	worker := fx.onlineWorker("memory cancel hold", true)
	id := fx.queuedRun([]string{})
	if _, err := fx.pool.Exec(fx.ctx, `UPDATE runs SET status='recovery_wait',
        recovery_wait_cause='worker_memory_pressure',claim_generation=2,worker_id=$2,
        claim_released_at=now(),released_worker_id=$2,released_worker_nonce='held',
        memory_episode=2,memory_policy='{"version":1,"max_interventions":3}',
        memory_intervention_count=3,
        requeue_count=4,requeue_episode_baseline=2,worker_recovery_episode=1,
        budget_wall_seconds=21600,budget_extension_seconds=3600,
        budget_paused_seconds=120,started_at=now()-interval '1 hour',
        recovery_retry_not_before=now()-interval '1 minute' WHERE id=$1`, id, worker); err != nil {
		t.Fatal(err)
	}
	held, err := fx.q.GetRunByID(fx.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{q: fx.q, wsvc: workersvc.New(fx.q, nil, workersvc.Params{
		RunTimeout: 2 * time.Hour, RunWallCeiling: 8 * time.Hour,
		WorkerHeartbeatStale: time.Minute,
	})}
	for _, user := range []store.User{foreign, admin} {
		rec := httptest.NewRecorder()
		h.CreateRunInput(rec, inputReq(user, id, `{"kind":"cancel"}`))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("non-owner Cancel (admin=%t)=%d %s", user.IsAdmin, rec.Code, rec.Body.String())
		}
		run, err := fx.q.GetRunByID(fx.ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(run, held) {
			t.Fatalf("non-owner Cancel (admin=%t) changed held run: %+v", user.IsAdmin, run)
		}
	}
	rec := httptest.NewRecorder()
	h.CreateRunInput(rec, inputReq(owner, id, `{"kind":"cancel","body":"end memory hold"}`))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("owner Cancel=%d %s", rec.Code, rec.Body.String())
	}
	var response apitypes.RunInputResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.ServerSide || response.Resumed {
		t.Fatalf("owner Cancel response=%+v, want server-side terminal cancellation", response)
	}
	run, err := fx.q.GetRunByID(fx.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != "cancelled" || !run.FinishedAt.Valid ||
		!run.StopKind.Valid || run.StopKind.String != "cancelled" ||
		!run.StopReason.Valid || run.StopReason.String != "end memory hold" {
		t.Fatalf("owner Cancel did not persist terminal cancellation: %+v", run)
	}
	if run.MemoryEpisode != 2 || run.MemoryInterventionCount != 3 ||
		!bytes.Equal(run.MemoryPolicy, held.MemoryPolicy) {
		t.Fatalf("Cancel reset memory episode/policy or refunded spent interventions: %+v", run)
	}
	if run.RequeueCount != 4 || run.RequeueEpisodeBaseline != 2 || run.WorkerRecoveryEpisode != 1 ||
		run.ClaimGeneration != held.ClaimGeneration || run.WorkerID != held.WorkerID ||
		run.ClaimReleasedAt != held.ClaimReleasedAt || run.ReleasedWorkerID != held.ReleasedWorkerID ||
		run.ReleasedWorkerNonce != held.ReleasedWorkerNonce {
		t.Fatalf("Cancel changed retry counters or released-claim fences: %+v", run)
	}
	if run.BudgetWallSeconds != held.BudgetWallSeconds ||
		run.BudgetExtensionSeconds != held.BudgetExtensionSeconds ||
		run.BudgetPausedSeconds != held.BudgetPausedSeconds || run.StartedAt != held.StartedAt {
		t.Fatalf("Cancel changed frozen wall budget or banked held time: %+v", run)
	}
	var inputs int
	if err := fx.pool.QueryRow(fx.ctx, "SELECT count(*) FROM run_user_inputs WHERE run_id=$1", id).Scan(&inputs); err != nil {
		t.Fatal(err)
	}
	if inputs != 0 {
		t.Fatalf("Cancel left %d steering inputs for an absent poller", inputs)
	}
}

func TestMemoryHoldOwnerOnlyReleaseLiveDB(t *testing.T) {
	fx := newEphemeralFixture(t, false)
	// Cleanup runs before the pool closes; this test's resumed run must not
	// become an unrelated candidate for later shared-database health scans.
	t.Cleanup(func() {
		if _, err := fx.pool.Exec(fx.ctx, "DELETE FROM users WHERE id=$1", fx.userID); err != nil {
			t.Errorf("cleanup memory hold owner: %v", err)
		}
	})
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
