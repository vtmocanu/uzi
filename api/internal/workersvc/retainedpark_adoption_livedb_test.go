package workersvc

import (
	"testing"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Exercise the worker report, owner episode renewal and actual claim/custody writer.
func TestRetainedAdoptionFailureResumeClaimLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	f := newCustodyContinuationFixture(t, env)
	f.svc.SetBackground(func(func()) {})
	id := f.seedRun("queued", 0)
	claimedID, gen := f.claim(t)
	if claimedID != id || gen != 1 || f.ownOpenHolds(t, id) != 1 {
		t.Fatal("initial claim did not open custody")
	}
	env.exec("UPDATE runs SET status='running', session_id='retained-session', retained_park_count=2, recovery_wait_count=9, worker_recovery_episode=3, requeue_count=7, requeue_episode_baseline=5, started_at=now()-interval '10 minutes', budget_wall_seconds=3600, budget_paused_seconds=17 WHERE id=$1", id)
	before := exhaustionRun(t, env, id)
	req := retainedReportCause(gen, recoveryCauseRetainedRecoveryAdoptionFailed)
	held, applied, err := f.svc.SetState(env.ctx, f.wkr, id, req)
	if err != nil || !applied {
		t.Fatalf("park applied=%v err=%v", applied, err)
	}
	if held.Status != "recovery_wait" || held.RecoveryWaitCause.String != recoveryCauseRetainedRecoveryAdoptionFailed ||
		held.RecoveryRetryNotBefore.Valid || held.RetainedParkCount != 2 || held.RecoveryWaitCount != 10 ||
		held.WorkerRecoveryEpisode != 3 || held.RequeueEpisodeBaseline != 5 || held.RequeueCount != 7 ||
		!held.ClaimReleasedAt.Valid || held.WorkerID != before.WorkerID || held.SessionID != before.SessionID ||
		held.ReleasedWorkerID != before.ReleasedWorkerID || held.ReleasedWorkerNonce != before.ReleasedWorkerNonce ||
		f.ownOpenHolds(t, id) != 1 {
		t.Fatal("adoption failure changed allowance, affinity or custody")
	}
	env.exec("UPDATE runs SET status_since=now()-interval '2 minutes', hold_reason='budget_exhausted', hold_captured_head='fixture', stale_requeue_generation=1, worker_recovery_evidence=jsonb_build_object('checkpoint_tip',NULL,'available_capture',false,'publication_uncertain',true,'capture_uncertain',true,'custody_uncertain',true,'unknown',true,'recorded_at',now()), codex_cap_hash=$2, health='stalled', health_reason='fixture', health_since=now() WHERE id=$1", id, []byte{1})
	held = exhaustionRun(t, env, id)
	_, err = env.q.ResumeWorkerRecoveryEpisode(env.ctx, store.ResumeWorkerRecoveryEpisodeParams{ID: id, UserID: f.userID, GlobalTimeoutSeconds: 3600})
	if err != nil {
		t.Fatal(err)
	}
	resumed := exhaustionRun(t, env, id)
	if resumed.Status != "queued" || resumed.WorkerRecoveryEpisode != 4 || resumed.RequeueEpisodeBaseline != 7 ||
		resumed.RequeueCount != 7 || resumed.RetainedParkCount != 2 || resumed.RecoveryWaitCount != 10 ||
		resumed.ClaimReleasedAt != held.ClaimReleasedAt || resumed.WorkerID != held.WorkerID ||
		resumed.SessionID != held.SessionID || resumed.StartedAt != held.StartedAt || resumed.BudgetPausedSeconds < 137 ||
		resumed.HoldReason.Valid || resumed.HoldCapturedHead.Valid || resumed.StaleRequeueGeneration.Valid ||
		len(resumed.WorkerRecoveryEvidence) != 0 || resumed.RecoveryWaitCause.Valid || resumed.RecoveryRetryNotBefore.Valid ||
		len(resumed.CodexCapHash) != 0 || resumed.CodexClaimEpoch != held.CodexClaimEpoch+1 ||
		resumed.Health != "ok" || resumed.HealthReason.Valid || resumed.HealthSince.Valid {
		t.Fatal("owner Resume did not apply complete episode mutation")
	}
	payload, err := f.svc.Claim(env.ctx, f.wkr, nil)
	if err != nil || payload == nil {
		t.Fatalf("fresh Claim payload=%v err=%v", payload != nil, err)
	}
	assertClaimRecoveryEpisode(t, payload, 6)
	assertClaimRecoveryOwnerEpisode(t, payload, 4)
	fresh := exhaustionRun(t, env, id)
	if fresh.ClaimGeneration != gen+1 || fresh.ClaimReleasedAt.Valid || f.ownOpenHolds(t, id) != 2 {
		t.Fatal("fresh claim did not advance execution and add custody")
	}
	var counted int
	if err := env.pool.QueryRow(env.ctx, "SELECT count(*) FROM recovery_custody_hold_facts WHERE user_id=$1", f.userID).Scan(&counted); err != nil || counted != 2 {
		t.Fatalf("custody facts=%d err=%v", counted, err)
	}
}
