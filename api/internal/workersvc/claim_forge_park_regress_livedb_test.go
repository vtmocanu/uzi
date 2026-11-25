package workersvc

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/privcheck"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// claim_forge_park_regress_livedb_test.go pins issue #4689 end to end on the REAL claim path
// (Service.Claim -> ClaimRun -> assembleClaim -> finishRunClaim) against a real Postgres: a STARTED
// run re-claimed while the forge is unreachable must park on forge_unreachable instead of failing
// terminally with guardrail_blocked. It uses only symbols that predate the fix (the fake guard, the
// existing fixtures and claimTestHooks.afterAssembly), so the same file compiles against the
// pre-fix tree and fails there on the status assertion. The guard result comes from
// transientGuardBlock (claim_forge_park_fixture_test.go). Skipped unless UZI_TEST_DATABASE_URL
// points at a throwaway Postgres; run via ./e2e/run-store-it.sh.

// cfprLifecycle records the statuses the service notifies the board automation of.
type cfprLifecycle struct {
	mu  sync.Mutex
	got []string
}

func (l *cfprLifecycle) Notify(_ uuid.UUID, status string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.got = append(l.got, status)
}

func (l *cfprLifecycle) statuses() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.got...)
}

// cfprReach is what the afterAssembly seam saw of the claim under test.
type cfprReach struct {
	reached bool
	runID   uuid.UUID
	gen     int64
	err     error
	epoch   int64 // codex_claim_epoch read from the database right after ClaimRun
}

// cfprObserve arms afterAssembly on svc and returns what it recorded. The epoch is the value
// ClaimRun committed, read before finishRunClaim writes anything.
func cfprObserve(t *testing.T, env codexTestEnv, svc *Service) *cfprReach {
	t.Helper()
	got := &cfprReach{}
	svc.claimHooks = &claimTestHooks{afterAssembly: func(_ context.Context, run store.Run, payload *ClaimPayload, err error) {
		got.reached, got.runID, got.gen, got.err = true, run.ID, run.ClaimGeneration, err
		if payload != nil {
			t.Errorf("assembly returned a payload despite the guard block")
		}
		if qerr := env.pool.QueryRow(env.ctx, `SELECT codex_claim_epoch FROM runs WHERE id = $1`, run.ID).Scan(&got.epoch); qerr != nil {
			t.Errorf("read epoch: %v", qerr)
		}
	}}
	return got
}

// cfprRestoreAudits counts the one-claim Fresh restore audit rows of a run.
func cfprRestoreAudits(t *testing.T, env codexTestEnv, id uuid.UUID) int {
	t.Helper()
	var n int
	if err := env.pool.QueryRow(env.ctx, `SELECT count(*) FROM run_user_inputs WHERE run_id=$1 AND kind='resume' AND body LIKE '{"policy_fresh":"restored",%'`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// cfprStartedQueuedRun seeds a started run requeued as a parked forge run would be (queued, worker
// affinity kept, history stamp in the past, an open predecessor hold at generation 1), with a
// pending one-claim Fresh grant that the next claim consumes and a non-ok health flag.
func cfprStartedQueuedRun(t *testing.T, f *custodyContinuationFixture) uuid.UUID {
	t.Helper()
	f.wkr.ProtocolCapabilities = []string{capability.RecoveryArchiveV1, capability.ProviderPolicyHoldV1}
	f.env.exec(`UPDATE workers SET protocol_capabilities=$2 WHERE id=$1`, f.workerID, f.wkr.ProtocolCapabilities)
	id := f.seedRun("queued", 1)
	f.env.exec(`UPDATE runs SET worker_id=$2, started_at=now()-interval '10 minutes', first_started_at=now()-interval '10 minutes',
		forge_park_count=1, recovery_wait_count=2, recovery_retry_not_before=now()-interval '1 hour',
		health='slow', health_reason='fixture', health_since=now(),
		policy_preserve_required=true, policy_pending_fresh=true, policy_outcome_id=gen_random_uuid(),
		policy_continuation_mode='same_conversation', policy_applying_worker_id=$2,
		policy_source_generation=1, policy_applying_generation=1, policy_resolved_at=now()
		WHERE id=$1`, id, f.workerID)
	f.seedHold(f.userID, id, 1, "open")
	return id
}

func TestClaimForgeTransientParksOrdinaryContinuationLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	f := newCustodyContinuationFixture(t, env)
	runID := cfprStartedQueuedRun(t, f)
	f.svc.SetRepoGuard(&fakeGuard{res: transientGuardBlock()})
	rec, life := newReadmitRecorder(), &cfprLifecycle{}
	f.svc.SetBroadcaster(rec)
	f.svc.SetLifecycle(life)
	before := mustRun(t, env, runID)
	seen := cfprObserve(t, env, f.svc)

	payload, err := f.svc.Claim(env.ctx, f.wkr, nil)
	if err != nil || payload != nil {
		t.Fatalf("Claim = (%v, %v), want an idle park (nil payload, nil error)", payload != nil, err)
	}
	if !seen.reached || seen.runID != runID || seen.gen != before.ClaimGeneration+1 || !errors.Is(seen.err, errGuardrailBlockedClaim) {
		t.Fatalf("claim did not reach the guard block of the target run: %+v", seen)
	}

	r := mustRun(t, env, runID)
	if r.Status != "recovery_wait" || r.RecoveryWaitCause.String != "forge_unreachable" {
		t.Fatalf("status=%s cause=%v fail_origin=%v failure_reason=%v, want recovery_wait/forge_unreachable",
			r.Status, r.RecoveryWaitCause, r.FailOrigin, r.FailureReason)
	}
	if r.FailOrigin.Valid || r.FailureReason.Valid {
		t.Fatalf("a park carries fail_origin=%v failure_reason=%v, want both NULL", r.FailOrigin, r.FailureReason)
	}
	if r.ForgeParkCount != before.ForgeParkCount+1 || r.RecoveryWaitCount != before.RecoveryWaitCount+1 {
		t.Fatalf("forge_park_count=%d recovery_wait_count=%d, want %d/%d", r.ForgeParkCount, r.RecoveryWaitCount, before.ForgeParkCount+1, before.RecoveryWaitCount+1)
	}
	if !r.RecoveryRetryNotBefore.Valid || !r.RecoveryRetryNotBefore.Time.After(time.Now()) {
		t.Fatalf("recovery_retry_not_before=%v, want a future stamp", r.RecoveryRetryNotBefore)
	}
	if !r.PolicyPendingFresh || r.PolicyFreshConsumedGeneration.Valid || cfprRestoreAudits(t, env, runID) != 1 {
		t.Fatalf("Fresh not restored: %s audits=%d", freshStamps(r), cfprRestoreAudits(t, env, runID))
	}
	if r.CodexClaimEpoch != seen.epoch+1 || len(r.CodexCapHash) != 0 {
		t.Fatalf("epoch=%d hash=%x, want %d and no capability", r.CodexClaimEpoch, r.CodexCapHash, seen.epoch+1)
	}
	if r.Health != "ok" || r.HealthReason.Valid {
		t.Fatalf("health=%s reason=%v, want ok", r.Health, r.HealthReason)
	}
	if _, states := rec.published(runID); len(states) != 1 || states[0] != "recovery_wait" {
		t.Fatalf("broadcast states = %v, want exactly [recovery_wait]", states)
	}
	if got := life.statuses(); len(got) != 1 || got[0] != "recovery_wait" {
		t.Fatalf("lifecycle notifications = %v, want exactly [recovery_wait] and no failed", got)
	}
}

func TestClaimForgeTransientParksEnrolledPublicationLiveDB(t *testing.T) {
	f := newPublicationParkFixture(t, time.Hour)
	env := f.env
	env.exec(`UPDATE runs SET first_started_at=now()-interval '1 hour' WHERE id=$1`, f.runID)

	// The run parks at publish (timed), then the promoter returns it to queued.
	if run, applied, err := f.park(1, "git"); err != nil || !applied || run.RecoveryWaitPhase.String != "publication" {
		t.Fatalf("park at publish: phase=%v applied=%v err=%v", run.RecoveryWaitPhase, applied, err)
	}
	promote := func() {
		t.Helper()
		f.expireCooldown()
		env.exec(`UPDATE runs SET recovery_retry_not_before=now()-interval '1 hour' WHERE id=$1`, f.runID)
		if _, err := env.q.PromoteRecoveryWaitRuns(env.ctx, pgconv.Time(time.Now())); err != nil {
			t.Fatal(err)
		}
		if got := f.run(t).Status; got != "queued" {
			t.Fatalf("promoted status = %s, want queued", got)
		}
	}
	promote()
	sideBefore, runBefore := f.side(t), f.run(t)
	if len(sideBefore.Intent) == 0 {
		t.Fatalf("fixture side row carries no intent bytes: %+v", sideBefore)
	}
	if !sideBefore.WaitStartedAt.Valid || sideBefore.ParkTotal != 1 || !sideBefore.WorkerID.Valid || uuid.UUID(sideBefore.WorkerID.Bytes) != f.workerID {
		t.Fatalf("fixture side row = %+v", sideBefore)
	}

	f.svc.SetRepoGuard(&fakeGuard{res: transientGuardBlock()})
	rec, life := newReadmitRecorder(), &cfprLifecycle{}
	f.svc.SetBroadcaster(rec)
	f.svc.SetLifecycle(life)
	seen := cfprObserve(t, env, f.svc)

	payload, err := f.svc.Claim(env.ctx, f.wkr, nil)
	if err != nil || payload != nil {
		t.Fatalf("Claim = (%v, %v), want an idle park (nil payload, nil error)", payload != nil, err)
	}
	if !seen.reached || seen.runID != f.runID || seen.gen != runBefore.ClaimGeneration+1 || !errors.Is(seen.err, errGuardrailBlockedClaim) {
		t.Fatalf("claim did not reach the guard block of the target run: %+v", seen)
	}

	r := f.run(t)
	if r.Status != "recovery_wait" || r.RecoveryWaitCause.String != "forge_unreachable" || r.RecoveryWaitPhase.String != "publication" {
		t.Fatalf("status=%s cause=%v phase=%v fail_origin=%v, want recovery_wait/forge_unreachable/publication",
			r.Status, r.RecoveryWaitCause, r.RecoveryWaitPhase, r.FailOrigin)
	}
	if r.ForgeParkCount != runBefore.ForgeParkCount || r.FailOrigin.Valid {
		t.Fatalf("forge_park_count=%d (was %d) fail_origin=%v", r.ForgeParkCount, runBefore.ForgeParkCount, r.FailOrigin)
	}
	if !r.RecoveryRetryNotBefore.Valid {
		t.Fatal("a young publication park must carry a retry stamp")
	}
	side := f.side(t)
	if side.State != "active" || side.EnrollmentSeq != sideBefore.EnrollmentSeq || side.WorkerID != sideBefore.WorkerID ||
		string(side.Handoff) != string(sideBefore.Handoff) || side.Episode != sideBefore.Episode ||
		string(side.Intent) != string(sideBefore.Intent) || side.CandidateSeq != sideBefore.CandidateSeq ||
		!side.WaitStartedAt.Time.Equal(sideBefore.WaitStartedAt.Time) || side.ParkTotal != sideBefore.ParkTotal+1 {
		t.Fatalf("side row moved beyond one park:\nbefore %+v\nafter  %+v", sideBefore, side)
	}
	var open int
	if err := env.pool.QueryRow(env.ctx, `SELECT count(*) FROM recovery_custody_holds WHERE run_id=$1 AND generation=1 AND state='open'`, f.runID).Scan(&open); err != nil || open != 1 {
		t.Fatalf("predecessor custody hold open = %d err=%v, want 1", open, err)
	}
	if _, states := rec.published(f.runID); len(states) != 1 || states[0] != "recovery_wait" {
		t.Fatalf("broadcast states = %v, want exactly [recovery_wait]", states)
	}
	if got := life.statuses(); len(got) != 1 || got[0] != "recovery_wait" {
		t.Fatalf("lifecycle notifications = %v, want exactly [recovery_wait]", got)
	}

	// The forge recovers: the promoted custodian claims a real payload.
	f.svc.SetRepoGuard(&fakeGuard{res: privcheck.GuardResult{}})
	f.svc.claimHooks = nil
	promote()
	got, gen := f.claim(t)
	if got != f.runID || gen != runBefore.ClaimGeneration+2 {
		t.Fatalf("custodian claim after recovery = %s gen %d, want %s gen %d", got, gen, f.runID, runBefore.ClaimGeneration+2)
	}
}
