package workersvc

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// claim_finish_transient_livedb_test.go pins finishRunClaim's TRANSIENT arm (PRD #1590 D2, N6)
// against a real Postgres: RequeueClaimAssemblyExact's queued and pool_wait arms, its exact-claim
// fence, its judge refusal, and the lock-time authority override of a transient outcome. The
// transient outcome is the real one: the owner's Codex lane flips to a custom model between
// ClaimRun's gate and assembly, and claimant B lacks codex_custom_model_v1, so assembleClaim
// returns errCustomModelCapabilityMissing. Skipped unless UZI_TEST_DATABASE_URL is set; run via
// ./e2e/run-store-it.sh.

// flipToCustomLane is the beforeAssembly race: the owner's Codex lane becomes a custom model
// after ClaimRun admitted a worker that cannot run one.
func (fx *codexClaimFix) flipToCustomLane(t *testing.T) {
	t.Helper()
	fx.env.exec(`UPDATE users SET default_codex_model = $2 WHERE id = $1`, fx.userID, customCodexModel)
}

// transientHooks induces the custom-model transient outcome, checks assembly produced it, and
// runs atLock (the lock-time authority change, when any) before finishRunClaim. It records the
// row as assembly left it.
func (fx *codexClaimFix) transientHooks(t *testing.T, atLock func(), assembled *store.Run) *claimTestHooks {
	return &claimTestHooks{
		beforeAssembly: func(context.Context, store.Run) { fx.flipToCustomLane(t) },
		afterAssembly: func(_ context.Context, _ store.Run, p *ClaimPayload, err error) {
			if !errors.Is(err, errCustomModelCapabilityMissing) || p != nil {
				t.Fatalf("assembly = (%v, %v), want (nil, errCustomModelCapabilityMissing)", p != nil, err)
			}
			*assembled = mustRun(t, fx.env, fx.runID)
			if atLock != nil {
				atLock()
			}
		},
	}
}

// TestClaimTransientRequeueQueuedLiveDB (PRD #1590 D2): the custom-model transient outcome with
// valid authority goes through RequeueClaimAssemblyExact's queued arm: status queued, the wall
// (started_at, budget_paused_seconds) kept, the claim's capability revoked, worker_id kept for
// affinity, no fail or park fields, and only the current generation's hold released.
func TestClaimTransientRequeueQueuedLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fx := newCodexClaimFix(t, env, true)
	var assembled store.Run
	fx.svc.claimHooks = fx.transientHooks(t, nil, &assembled)

	payload, err := fx.svc.Claim(env.ctx, fx.claimant(t, true), nil)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	fx.assertNoClaudeFallback(t, payload)
	r := mustRun(t, env, fx.runID)
	if r.Status != "queued" || r.FailOrigin.Valid || r.FailureReason.Valid || r.RecoveryWaitCause.Valid {
		t.Fatalf("status=%s origin=%v reason=%v cause=%v, want a clean queued requeue",
			r.Status, r.FailOrigin, r.FailureReason, r.RecoveryWaitCause)
	}
	if !r.StartedAt.Valid || !r.StartedAt.Time.Equal(assembled.StartedAt.Time) || r.BudgetPausedSeconds != assembled.BudgetPausedSeconds ||
		r.BudgetPausedSeconds == 0 {
		t.Fatalf("queued arm touched the wall: started_at=%v paused=%d, want %v/%d",
			r.StartedAt, r.BudgetPausedSeconds, assembled.StartedAt, assembled.BudgetPausedSeconds)
	}
	if len(r.CodexCapHash) != 0 || r.CodexClaimEpoch != assembled.CodexClaimEpoch+1 {
		t.Fatalf("capability not revoked: hash=%x epoch=%d (assembly left %d)", r.CodexCapHash, r.CodexClaimEpoch, assembled.CodexClaimEpoch)
	}
	if r.WorkerID != pgconv.UUID(fx.workerB) || r.ClaimGeneration != assembled.ClaimGeneration || r.Health != "ok" {
		t.Fatalf("worker=%v gen=%d health=%s, want B/%d/ok", r.WorkerID, r.ClaimGeneration, r.Health, assembled.ClaimGeneration)
	}
	if !r.StatusSince.Time.After(assembled.StatusSince.Time) {
		t.Fatalf("status_since not advanced: %v (claimed at %v)", r.StatusSince, assembled.StatusSince)
	}
	assertClaimRecoveryHold(t, env, fx.holdA, "open", false)
	assertClaimRecoveryHold(t, env, fx.holdB(t), "released", true)
}

// TestClaimTransientRevocationFailsLiveDB (PRD #1590 N6 (a)): a transient assembly outcome plus
// a credential_revision bump at lock time is terminal. The run would fail the same database-only
// authority check on its next claim, so it is failed credential_unavailable with the revision
// error as its reason instead of being requeued, and the current hold is released.
//
// MUTATION: delete finishRunClaimTx's `if !holdClass { transient = false }` override; the run is
// then requeued to queued and this test fails.
func TestClaimTransientRevocationFailsLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fx := newCodexClaimFix(t, env, true)
	var assembled store.Run
	fx.svc.claimHooks = fx.transientHooks(t, func() {
		fx.setAccount(t, "credential_revision = credential_revision + 1")
	}, &assembled)

	payload, err := fx.svc.Claim(env.ctx, fx.claimant(t, true), nil)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	fx.assertNoClaudeFallback(t, payload)
	r := mustRun(t, env, fx.runID)
	if r.Status != "failed" || r.FailOrigin.String != "credential_unavailable" || r.RecoveryWaitCause.Valid {
		t.Fatalf("status=%s origin=%v cause=%v, want failed/credential_unavailable", r.Status, r.FailOrigin, r.RecoveryWaitCause)
	}
	if !strings.Contains(r.FailureReason.String, ErrCodexAccountRevisionStale.Error()) ||
		strings.Contains(r.FailureReason.String, errCustomModelCapabilityMissing.Error()) {
		t.Fatalf("failure_reason = %q, want the revision error that decided it", r.FailureReason.String)
	}
	if len(r.CodexCapHash) != 0 || r.CodexClaimEpoch != assembled.CodexClaimEpoch+1 {
		t.Fatalf("capability not revoked: hash=%x epoch=%d", r.CodexCapHash, r.CodexClaimEpoch)
	}
	assertClaimRecoveryHold(t, env, fx.holdA, "open", false)
	assertClaimRecoveryHold(t, env, fx.holdB(t), "released", true)
}

// TestClaimTransientQuarantineParksLiveDB (PRD #1590 N6 (b)): a transient assembly outcome plus a
// quarantine at lock time parks the run on codex_account_unavailable (the account-driven promoter
// resumes it), not queued and not pool_wait.
//
// MUTATION: drop `|| transient` from finishRunClaimTx's holdClass admission; the quarantine then
// fails the run instead of parking it and this test fails.
func TestClaimTransientQuarantineParksLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fx := newCodexClaimFix(t, env, true)
	before := mustRun(t, env, fx.runID)
	var assembled store.Run
	fx.svc.claimHooks = fx.transientHooks(t, func() {
		fx.setAccount(t, "coord_state = 'quarantined'")
	}, &assembled)

	payload, err := fx.svc.Claim(env.ctx, fx.claimant(t, true), nil)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	fx.assertNoClaudeFallback(t, payload)
	fx.assertParked(t, before, fx.workerA)
	assertClaimRecoveryHold(t, env, fx.holdA, "open", false)
	assertClaimRecoveryHold(t, env, fx.holdB(t), "released", true)
}

// TestFinishRunClaimPoolWaitArmLiveDB (PRD #1590 D2, PRD #754 M4): an empty auto pool on the
// exact claim goes through RequeueClaimAssemblyExact's pool_wait arm: status pool_wait, a fresh
// wall (started_at NULL, budget_paused_seconds 0), the capability revoked (hash NULL, epoch
// bumped), worker_id kept, and only the current generation's hold released.
func TestFinishRunClaimPoolWaitArmLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fx := newCodexClaimFix(t, env, true)
	run, err := env.q.ClaimRun(env.ctx, claimRunParamsFor(fx.claimant(t, true)))
	if err != nil {
		t.Fatalf("ClaimRun: %v", err)
	}
	before := mustRun(t, env, fx.runID)
	if !before.StartedAt.Valid || before.BudgetPausedSeconds == 0 {
		t.Fatalf("fixture wall is already fresh (started_at=%v paused=%d); the assertions would be vacuous",
			before.StartedAt, before.BudgetPausedSeconds)
	}
	payload, err := fx.svc.finishRunClaim(env.ctx, run, nil, errAutoPoolEmpty,
		claimRecoveryIdentity{workerID: fx.workerB, recoveryCapable: true})
	if err != nil || payload != nil {
		t.Fatalf("finishRunClaim = (%v, %v), want idle", payload != nil, err)
	}
	r := mustRun(t, env, fx.runID)
	if r.Status != "pool_wait" || r.FailOrigin.Valid || r.RecoveryWaitCause.Valid {
		t.Fatalf("status=%s origin=%v cause=%v, want pool_wait", r.Status, r.FailOrigin, r.RecoveryWaitCause)
	}
	if r.StartedAt.Valid || r.BudgetPausedSeconds != 0 || r.Health != "ok" {
		t.Fatalf("wall not reset: started_at=%v paused=%d health=%s", r.StartedAt, r.BudgetPausedSeconds, r.Health)
	}
	if len(r.CodexCapHash) != 0 || r.CodexClaimEpoch != before.CodexClaimEpoch+1 {
		t.Fatalf("capability not revoked: hash=%x epoch=%d (before %d)", r.CodexCapHash, r.CodexClaimEpoch, before.CodexClaimEpoch)
	}
	if r.WorkerID != pgconv.UUID(fx.workerB) || r.ClaimGeneration != run.ClaimGeneration {
		t.Fatalf("worker=%v gen=%d, want B/%d", r.WorkerID, r.ClaimGeneration, run.ClaimGeneration)
	}
	assertClaimRecoveryHold(t, env, fx.holdA, "open", false)
	assertClaimRecoveryHold(t, env, fx.holdB(t), "released", true)
}

// skewedFinishStore opens the exact-claim transaction on the real pool, but hands
// RequeueClaimAssemblyExact a skewed generation or worker. The row lock sees the exact claim, so
// the fenced UPDATE itself is what must match 0 rows.
type skewedFinishStore struct {
	Store
	pool interface {
		Begin(context.Context) (pgx.Tx, error)
	}
	skew func(*store.RequeueClaimAssemblyExactParams)
}

func (s skewedFinishStore) BeginClaimFinish(ctx context.Context) (claimFinishTx, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return skewedFinishTx{pgxClaimFinishTx{Queries: store.New(tx), tx: tx}, s.skew}, nil
}

type skewedFinishTx struct {
	pgxClaimFinishTx
	skew func(*store.RequeueClaimAssemblyExactParams)
}

func (t skewedFinishTx) RequeueClaimAssemblyExact(ctx context.Context, arg store.RequeueClaimAssemblyExactParams) (int64, error) {
	t.skew(&arg)
	return t.pgxClaimFinishTx.RequeueClaimAssemblyExact(ctx, arg)
}

// TestRequeueClaimAssemblyExactFenceLiveDB (PRD #1590 D2): RequeueClaimAssemblyExact is fenced to
// the exact claim. A generation or worker mismatch matches 0 rows in both arms, so finishRunClaim
// reports errClaimRecoveryStale and rolls back the whole transaction: the run stays claimed and
// untouched, and the hold release that preceded the UPDATE is undone.
func TestRequeueClaimAssemblyExactFenceLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cause error
		skew  func(*store.RequeueClaimAssemblyExactParams)
	}{
		{"queued, generation mismatch", errVaultLocked, func(p *store.RequeueClaimAssemblyExactParams) { p.ClaimGeneration++ }},
		{"queued, worker mismatch", errVaultLocked, func(p *store.RequeueClaimAssemblyExactParams) { p.WorkerID = pgconv.UUID(uuid.New()) }},
		{"pool_wait, generation mismatch", errAutoPoolEmpty, func(p *store.RequeueClaimAssemblyExactParams) { p.ClaimGeneration-- }},
		{"pool_wait, worker mismatch", errAutoPoolEmpty, func(p *store.RequeueClaimAssemblyExactParams) { p.WorkerID = pgconv.UUID(uuid.New()) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := setupCodexLiveDB(t)
			fx := newCodexClaimFix(t, env, true)
			run, err := env.q.ClaimRun(env.ctx, claimRunParamsFor(fx.claimant(t, true)))
			if err != nil {
				t.Fatalf("ClaimRun: %v", err)
			}
			before := mustRun(t, env, fx.runID)
			svc := New(skewedFinishStore{Store: env.q, pool: env.pool, skew: tc.skew}, env.box, testParams())
			payload, err := svc.finishRunClaim(env.ctx, run, nil, tc.cause,
				claimRecoveryIdentity{workerID: fx.workerB, recoveryCapable: true})
			if payload != nil || !errors.Is(err, errClaimRecoveryStale) {
				t.Fatalf("finishRunClaim = (%v, %v), want (nil, errClaimRecoveryStale)", payload != nil, err)
			}
			r := mustRun(t, env, fx.runID)
			if r.Status != "claimed" || !r.UpdatedAt.Time.Equal(before.UpdatedAt.Time) ||
				r.CodexClaimEpoch != before.CodexClaimEpoch || !r.StartedAt.Time.Equal(before.StartedAt.Time) {
				t.Fatalf("a fenced-out requeue mutated the run: status=%s epoch=%d (before %d)", r.Status, r.CodexClaimEpoch, before.CodexClaimEpoch)
			}
			assertClaimRecoveryHold(t, env, fx.holdB(t), "open", false)
			assertClaimRecoveryHold(t, env, fx.holdA, "open", false)
		})
	}
}

// TestFinishRunClaimJudgePoolWaitRefusedLiveDB (PRD #1590 D2, Decision 14): a judge never holds
// in pool_wait. The pool_wait arm's `kind <> 'judge'` guard matches 0 rows for a claimed judge,
// so finishRunClaim reports errClaimRecoveryStale, commits nothing and leaves the judge claimed.
func TestFinishRunClaimJudgePoolWaitRefusedLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fx := newCodexClaimFix(t, env, false)
	env.exec(`UPDATE runs SET status = 'completed' WHERE id = $1`, fx.runID)
	judgeID := env.seedCodexJudgeRun(t, fx.userID, fx.workerB, fx.runID)
	if err := fx.svc.FreezeCodexBinding(env.ctx, fx.userID, judgeID, fx.aliasID, codexAuthModeSubscription); err != nil {
		t.Fatalf("FreezeCodexBinding on the judge: %v", err)
	}
	judge := mustRun(t, env, judgeID)
	payload, err := fx.svc.finishRunClaim(env.ctx, judge, nil, errAutoPoolEmpty,
		claimRecoveryIdentity{workerID: fx.workerB, recoveryCapable: true})
	if payload != nil || !errors.Is(err, errClaimRecoveryStale) {
		t.Fatalf("finishRunClaim = (%v, %v), want (nil, errClaimRecoveryStale)", payload != nil, err)
	}
	r := mustRun(t, env, judgeID)
	if r.Status != "claimed" || !r.UpdatedAt.Time.Equal(judge.UpdatedAt.Time) || r.CodexClaimEpoch != judge.CodexClaimEpoch {
		t.Fatalf("a judge pool_wait mutated the run: status=%s epoch=%d (before %d)", r.Status, r.CodexClaimEpoch, judge.CodexClaimEpoch)
	}
}
