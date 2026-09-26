package workersvc

import (
	"context"
	"encoding/binary"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// credential_disabled_livedb_test.go is the live-DB half of PRD #1732 M2a (D3/D14): the
// claim-time credential_disabled park against real row locks and custody, and the promoter's
// lock order, requirement re-evaluation and triggers. Skipped unless UZI_TEST_DATABASE_URL is
// set; run via ./e2e/run-store-it.sh.

// cdFix is one owner with a repo, a live, registered, recovery-capable worker pinned to pinTok,
// a second enabled token otherTok, an enabled Anthropic default defTok, and a Service wired as
// main.go wires it with a synchronous background dispatcher, so a requested promoter pass is
// observable on return. The worker's heartbeat is fresh (it holds resume affinity) and it has a
// registration nonce, the incarnation ClaimRun's D19 fence compares.
type cdFix struct {
	env                      codexTestEnv
	svc                      *Service
	userID, repoID, workerID uuid.UUID
	pinTok, otherTok, defTok uuid.UUID
	requests                 int
}

func newCDFix(t *testing.T) *cdFix {
	t.Helper()
	env := setupCodexLiveDB(t)
	o := seedReevalOwner(t, env, BindModePinned, false)
	env.sealBotPAT(t, o.userID)
	fx := &cdFix{env: env, userID: o.userID, repoID: o.repoID, workerID: o.workerID, pinTok: o.deadTok, otherTok: o.altTok}
	fx.defTok = env.seedAnthropicSecret(t, o.userID, "default-"+uuid.NewString(), true)
	env.exec(`UPDATE workers SET anthropic_secret_id = $2, last_heartbeat_at = now(),
	    snapshot_register_nonce = $3, protocol_capabilities = $4 WHERE id = $1`,
		o.workerID, fx.pinTok, "nonce-"+uuid.NewString(),
		[]string{capability.RecoveryArchiveV1, capability.CredentialSwitchV1})
	fx.svc = New(env.q, env.box, testParams())
	fx.svc.SetTxBeginner(env.pool)
	fx.svc.SetBackground(func(fn func()) { fx.requests++; fn() })
	return fx
}

func (fx *cdFix) setEnabled(id uuid.UUID, enabled bool) {
	if enabled {
		fx.env.exec(`UPDATE user_secrets SET disabled_at = NULL, enablement_rev = enablement_rev + 1 WHERE id = $1`, id)
		return
	}
	fx.env.exec(`UPDATE user_secrets SET disabled_at = now(), enablement_rev = enablement_rev + 1 WHERE id = $1`, id)
}

// queuedRun seeds a queued issue run on the fixture's worker with a live wall (started 15
// minutes ago, one hour budget) and a resumable session.
func (fx *cdFix) queuedRun(t *testing.T, kind string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	fx.env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description,
	    status, status_since, worker_id, started_at, budget_wall_seconds, session_id)
	    VALUES ($1, $2, $3, $4, $5, 't', 'd', 'queued', now(), $6, now() - interval '15 minutes', 3600, 'sess-cd')`,
		id, fx.userID, fx.repoID, kind, int64(binary.BigEndian.Uint32(id[:4])), fx.workerID)
	return id
}

// parkedRun seeds a run already held on credential_disabled by the fixture's worker, parked
// ten minutes ago after five active minutes, in the shape ParkCredentialDisabledRun writes: the
// claim released, worker_id kept as affinity, no D19 released incarnation.
func (fx *cdFix) parkedRun(t *testing.T, kind string) uuid.UUID {
	t.Helper()
	id := fx.queuedRun(t, kind)
	fx.env.exec(`UPDATE runs SET status = 'paused', hold_reason = 'credential_disabled',
	    status_since = now() - interval '10 minutes', claim_released_at = now() - interval '10 minutes'
	    WHERE id = $1`, id)
	return id
}

// worker is the fixture worker as it registered: the stored row, with the protocol
// capabilities it advertises on the claim request.
func (fx *cdFix) worker(t *testing.T) store.Worker {
	t.Helper()
	return wkrRow(t, fx.env, fx.workerID)
}

// openCustodyHolds counts the run's unresolved custody holds.
func openCustodyHolds(t *testing.T, env codexTestEnv, runID uuid.UUID) int {
	t.Helper()
	var n int
	if err := env.pool.QueryRow(env.ctx, `SELECT count(*) FROM recovery_custody_holds
	    WHERE run_id = $1 AND state = 'open'`, runID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func assertHeld(t *testing.T, env codexTestEnv, runID uuid.UUID, want bool) store.Run {
	t.Helper()
	r := mustRun(t, env, runID)
	held := r.Status == "paused" && r.HoldReason.Valid && r.HoldReason.String == "credential_disabled"
	if held != want {
		t.Fatalf("run %s: status=%s hold=%v, want held=%t", runID, r.Status, r.HoldReason, want)
	}
	if !want && r.Status != "queued" {
		t.Fatalf("run %s: status=%s, want queued", runID, r.Status)
	}
	return r
}

// TestClaimCredentialDisabledParksClaudeLiveDB: a disable that commits after the claim opened
// the pinned token but before the decision is seen by the finisher's locked re-check. The
// undelivered claim parks on paused/credential_disabled with its session, wall and recorded
// worker kept; enabling the token resumes it with no manual action, banking the held interval.
func TestClaimCredentialDisabledParksClaudeLiveDB(t *testing.T) {
	fx := newCDFix(t)
	runID := fx.queuedRun(t, "issue")
	fx.svc.claimHooks = &claimTestHooks{afterAssembly: func(_ context.Context, _ store.Run, p *ClaimPayload, err error) {
		if err != nil || p == nil {
			t.Fatalf("assembly = (%v, %v), want a payload", p != nil, err)
		}
		fx.setEnabled(fx.pinTok, false)
	}}
	payload, err := fx.svc.Claim(fx.env.ctx, fx.worker(t), nil)
	if err != nil || payload != nil {
		t.Fatalf("Claim = (%v, %v), want an idle claim", payload != nil, err)
	}
	r := assertHeld(t, fx.env, runID, true)
	if uuid.UUID(r.AnthropicSecretID.Bytes) != fx.pinTok {
		t.Fatalf("recorded credential %v, want the opened pin %s", r.AnthropicSecretID, fx.pinTok)
	}
	if !r.ClaimReleasedAt.Valid || r.WorkerID != pgconv.UUID(fx.workerID) || r.SessionID.String != "sess-cd" ||
		!r.StartedAt.Valid || r.FailOrigin.Valid || r.BudgetPausedSeconds != 0 {
		t.Fatalf("park state: released=%v worker=%v session=%q started=%v origin=%v paused=%d",
			r.ClaimReleasedAt, r.WorkerID, r.SessionID.String, r.StartedAt, r.FailOrigin, r.BudgetPausedSeconds)
	}
	fx.env.exec(`UPDATE runs SET status_since = now() - interval '10 minutes' WHERE id = $1`, runID)

	// A pass while the pin is still disabled keeps the run held (the enabled default does not satisfy a pin).
	fx.svc.RequestCredentialDisabledPromotion(fx.userID)
	assertHeld(t, fx.env, runID, true)

	fx.setEnabled(fx.pinTok, true)
	fx.svc.RequestCredentialDisabledPromotion(fx.userID)
	r = assertHeld(t, fx.env, runID, false)
	if r.BudgetPausedSeconds < 600 || r.BudgetPausedSeconds > 660 || !r.StartedAt.Valid || r.SessionID.String != "sess-cd" {
		t.Fatalf("resume: paused=%d (want the ~600s hold banked) started=%v session=%q", r.BudgetPausedSeconds, r.StartedAt, r.SessionID.String)
	}
}

// TestClaimCredentialDisabledHeldSecretLockRetriesLiveDB: a credential row locked by a
// concurrent writer during claim finishing is a NOWAIT refusal, retried and finally returned
// as 55P03 without waiting on the writer and without any mutation.
func TestClaimCredentialDisabledHeldSecretLockRetriesLiveDB(t *testing.T) {
	defer func(d time.Duration) { finishRunClaimRetryDelay = d }(finishRunClaimRetryDelay)
	finishRunClaimRetryDelay = time.Millisecond
	fx := newCDFix(t)
	runID := fx.queuedRun(t, "issue")
	holder, err := fx.env.pool.Begin(fx.env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(fx.env.ctx) }()
	fx.svc.claimHooks = &claimTestHooks{afterAssembly: func(context.Context, store.Run, *ClaimPayload, error) {
		if _, err := holder.Exec(fx.env.ctx, `SELECT 1 FROM user_secrets WHERE id = $1 FOR UPDATE`, fx.pinTok); err != nil {
			t.Fatalf("hold credential row: %v", err)
		}
	}}
	start := time.Now()
	payload, err := fx.svc.Claim(fx.env.ctx, fx.worker(t), nil)
	if payload != nil || !isLockNotAvailable(err) {
		t.Fatalf("Claim = (%v, %v), want (nil, 55P03)", payload != nil, err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("claim finishing blocked on the held credential row for %v", elapsed)
	}
	if r := mustRun(t, fx.env, runID); r.Status != "claimed" || r.HoldReason.Valid {
		t.Fatalf("a refused finish mutated the run: status=%s hold=%v", r.Status, r.HoldReason)
	}
}

// TestCodexClaimCredentialDisabledCustodyLiveDB: a Codex claim whose frozen alias is disabled
// after the capability mint parks on credential_disabled, releases ONLY the hold this claim
// opened (exact run+generation+worker, no_adopted_source) and keeps the earlier generation's
// retained custody.
func TestCodexClaimCredentialDisabledCustodyLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fx := newCodexClaimFix(t, env, true)
	fx.svc.claimHooks = &claimTestHooks{afterAssembly: func(_ context.Context, _ store.Run, p *ClaimPayload, err error) {
		if err != nil || p == nil || p.Secrets.Codex == nil {
			t.Fatalf("assembly = (%v, %v), want a Codex payload", p != nil, err)
		}
		env.exec(`UPDATE user_secrets SET disabled_at = now(), enablement_rev = enablement_rev + 1 WHERE id = $1`, fx.aliasID)
	}}
	payload, err := fx.svc.Claim(env.ctx, fx.claimant(t, true), nil)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	fx.assertNoClaudeFallback(t, payload)
	r := assertHeld(t, env, fx.runID, true)
	if r.FailOrigin.Valid || r.RecoveryWaitCause.Valid || len(r.CodexCapHash) != 0 || r.WorkerID != pgconv.UUID(fx.workerB) {
		t.Fatalf("park: origin=%v cause=%v cap=%x worker=%v", r.FailOrigin, r.RecoveryWaitCause, r.CodexCapHash, r.WorkerID)
	}
	assertClaimRecoveryHold(t, env, fx.holdA, "open", false)
	assertClaimRecoveryHold(t, env, fx.holdB(t), "released", true)

	env.exec(`UPDATE user_secrets SET disabled_at = NULL WHERE id = $1`, fx.aliasID)
	fx.svc.SetBackground(func(fn func()) { fn() })
	// The fixture's wall (started three hours ago, global two-hour budget) is spent: the
	// promoter never bypasses budget exhaustion, even with the alias enabled again.
	fx.svc.RequestCredentialDisabledPromotion(fx.userID)
	assertHeld(t, env, fx.runID, true)
	env.exec(`UPDATE runs SET budget_wall_seconds = 36000 WHERE id = $1`, fx.runID)
	fx.svc.RequestCredentialDisabledPromotion(fx.userID)
	assertHeld(t, env, fx.runID, false)
	assertClaimRecoveryHold(t, env, fx.holdA, "open", false)
}

// TestFinishRunClaimCredentialDisabledStaleLiveDB: a stale worker, a stale generation, a stale
// capability epoch, or a claim that already became a running flight each leave the run and
// both custody holds untouched.
func TestFinishRunClaimCredentialDisabledStaleLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stale func(env codexTestEnv, fx *codexClaimFix, run *store.Run, id *claimRecoveryIdentity)
	}{
		{"stale worker", func(_ codexTestEnv, _ *codexClaimFix, _ *store.Run, id *claimRecoveryIdentity) {
			id.workerID = uuid.New()
		}},
		{"stale generation", func(_ codexTestEnv, _ *codexClaimFix, run *store.Run, _ *claimRecoveryIdentity) {
			run.ClaimGeneration--
		}},
		{"stale epoch", func(env codexTestEnv, fx *codexClaimFix, _ *store.Run, _ *claimRecoveryIdentity) {
			env.exec(`UPDATE runs SET codex_claim_epoch = codex_claim_epoch + 1 WHERE id = $1`, fx.runID)
		}},
		{"running flight", func(env codexTestEnv, fx *codexClaimFix, _ *store.Run, _ *claimRecoveryIdentity) {
			env.exec(`UPDATE runs SET status = 'running' WHERE id = $1`, fx.runID)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := setupCodexLiveDB(t)
			fx := newCodexClaimFix(t, env, true)
			run, err := env.q.ClaimRun(env.ctx, claimRunParamsFor(fx.claimant(t, true)))
			if err != nil {
				t.Fatalf("ClaimRun: %v", err)
			}
			id := claimRecoveryIdentity{workerID: fx.workerB, recoveryCapable: true}
			tc.stale(env, fx, &run, &id)
			before := mustRun(t, env, fx.runID)
			payload, err := fx.svc.finishRunClaim(env.ctx, run, nil, errCredentialDisabled, id)
			if payload != nil || err != nil {
				t.Fatalf("finishRunClaim = (%v, %v), want idle", payload != nil, err)
			}
			after := mustRun(t, env, fx.runID)
			if after.Status != before.Status || after.HoldReason.Valid || !after.UpdatedAt.Time.Equal(before.UpdatedAt.Time) ||
				after.CodexClaimEpoch != before.CodexClaimEpoch {
				t.Fatalf("stale claim mutated the run: status %s->%s hold=%v", before.Status, after.Status, after.HoldReason)
			}
			assertClaimRecoveryHold(t, env, fx.holdA, "open", false)
			assertClaimRecoveryHold(t, env, fx.holdB(t), "open", false)
		})
	}
}

// parkFenceSkewStore opens the exact-claim transaction on the real pool but hands
// ParkCredentialDisabledRun a skewed generation, so the park's own fence matches 0 rows after
// the custody release already ran in the same transaction.
type parkFenceSkewStore struct {
	Store
	pool TxBeginner
}

func (s parkFenceSkewStore) BeginClaimFinish(ctx context.Context) (claimFinishTx, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return parkFenceSkewTx{pgxClaimFinishTx{Queries: store.New(tx), tx: tx}}, nil
}

type parkFenceSkewTx struct{ pgxClaimFinishTx }

func (t parkFenceSkewTx) ParkCredentialDisabledRun(ctx context.Context, arg store.ParkCredentialDisabledRunParams) (int64, error) {
	arg.ClaimGeneration++
	return t.pgxClaimFinishTx.ParkCredentialDisabledRun(ctx, arg)
}

// TestFinishRunClaimCredentialDisabledRollbackLiveDB: a park whose fence misses rolls the whole
// transaction back. No partial park: the run stays claimed and the custody release that
// preceded the park is undone.
func TestFinishRunClaimCredentialDisabledRollbackLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fx := newCodexClaimFix(t, env, true)
	run, err := env.q.ClaimRun(env.ctx, claimRunParamsFor(fx.claimant(t, true)))
	if err != nil {
		t.Fatalf("ClaimRun: %v", err)
	}
	before := mustRun(t, env, fx.runID)
	svc := New(parkFenceSkewStore{Store: env.q, pool: env.pool}, env.box, testParams())
	payload, err := svc.finishRunClaim(env.ctx, run, nil, errCredentialDisabled,
		claimRecoveryIdentity{workerID: fx.workerB, recoveryCapable: true})
	if payload != nil || !errors.Is(err, errClaimRecoveryStale) {
		t.Fatalf("finishRunClaim = (%v, %v), want (nil, errClaimRecoveryStale)", payload != nil, err)
	}
	after := mustRun(t, env, fx.runID)
	if after.Status != "claimed" || after.HoldReason.Valid || !after.UpdatedAt.Time.Equal(before.UpdatedAt.Time) {
		t.Fatalf("partial park: status=%s hold=%v", after.Status, after.HoldReason)
	}
	assertClaimRecoveryHold(t, env, fx.holdB(t), "open", false)
	assertClaimRecoveryHold(t, env, fx.holdA, "open", false)
}

// TestCredentialPromoterRequirementLiveDB is the requirement table: each parked run resumes
// only when the exact credential its next claim needs is enabled. A different default never
// wakes an explicit pin, and an owner pause keeps the run paused.
//
// MUTATION: make credentialRequirementMet return true unconditionally; the pinned rows are
// then promoted while their pin is disabled and this test fails.
func TestCredentialPromoterRequirementLiveDB(t *testing.T) {
	fx := newCDFix(t)
	fx.setEnabled(fx.pinTok, false)

	workerPin := fx.parkedRun(t, "issue")
	overridePin := fx.parkedRun(t, "issue")
	fx.env.exec(`UPDATE runs SET credential_override_mode = 'pinned', credential_override_secret_id = $2 WHERE id = $1`, overridePin, fx.pinTok)
	overrideDefault := fx.parkedRun(t, "issue")
	fx.env.exec(`UPDATE runs SET credential_override_mode = 'default' WHERE id = $1`, overrideDefault)
	ownerPaused := fx.parkedRun(t, "issue")
	fx.env.exec(`UPDATE runs SET credential_override_mode = 'default', pause_requested_at = now(), pause_mode = 'now' WHERE id = $1`, ownerPaused)
	judgePin := fx.parkedRun(t, "self_improve")
	fx.env.exec(`UPDATE users SET judge_anthropic_bind_mode = 'pinned', judge_anthropic_secret_id = $2 WHERE id = $1`, fx.userID, fx.pinTok)

	// A default hand-off onto another enabled token wakes only the default requirement.
	fx.env.exec(`UPDATE user_secrets SET is_default = false WHERE id = $1`, fx.defTok)
	fx.env.exec(`UPDATE user_secrets SET is_default = true WHERE id = $1`, fx.otherTok)
	fx.svc.RequestCredentialDisabledPromotion(fx.userID)
	for _, id := range []uuid.UUID{workerPin, overridePin, ownerPaused, judgePin} {
		assertHeld(t, fx.env, id, true)
	}
	assertHeld(t, fx.env, overrideDefault, false)
	if r := mustRun(t, fx.env, ownerPaused); !r.PauseRequestedAt.Valid {
		t.Fatal("the owner pause was consumed")
	}

	fx.setEnabled(fx.pinTok, true)
	fx.svc.RequestCredentialDisabledPromotion(fx.userID)
	for _, id := range []uuid.UUID{workerPin, overridePin, judgePin} {
		assertHeld(t, fx.env, id, false)
	}
	assertHeld(t, fx.env, ownerPaused, true)
}

// TestCredentialPromoterSweepAfterRestartLiveDB: a credential enabled while no pass was
// requested (the process restarted between park and enable) is promoted by a fresh Service's
// Sweep, the periodic fallback.
func TestCredentialPromoterSweepAfterRestartLiveDB(t *testing.T) {
	fx := newCDFix(t)
	fx.setEnabled(fx.pinTok, false)
	runID := fx.parkedRun(t, "issue")
	fx.setEnabled(fx.pinTok, true)

	restarted := New(fx.env.q, fx.env.box, testParams())
	restarted.SetTxBeginner(fx.env.pool)
	res, err := restarted.Sweep(fx.env.ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if res.CredentialPromoted < 1 {
		t.Fatalf("CredentialPromoted = %d, want >= 1", res.CredentialPromoted)
	}
	r := assertHeld(t, fx.env, runID, false)
	if r.BudgetPausedSeconds < 600 {
		t.Fatalf("held interval not banked: paused=%d", r.BudgetPausedSeconds)
	}
}

// TestCredentialPromoterTriggersLiveDB: a worker rebind, a Judge rebind and a per-run
// reassignment each request a pass after their commit, and the rebind onto an enabled
// credential resumes the runs that waited on the old one.
func TestCredentialPromoterTriggersLiveDB(t *testing.T) {
	fx := newCDFix(t)
	fx.setEnabled(fx.pinTok, false)

	workerRun := fx.parkedRun(t, "issue")
	before := fx.requests
	if _, err := fx.svc.SetWorkerAnthropicToken(fx.env.ctx, fx.userID, fx.workerID, BindModePinned, &fx.otherTok); err != nil {
		t.Fatalf("SetWorkerAnthropicToken: %v", err)
	}
	if fx.requests != before+1 {
		t.Fatalf("worker rebind requests = %d, want 1", fx.requests-before)
	}
	assertHeld(t, fx.env, workerRun, false)

	fx.env.exec(`UPDATE users SET judge_anthropic_bind_mode = 'pinned', judge_anthropic_secret_id = $2 WHERE id = $1`, fx.userID, fx.pinTok)
	judgeRun := fx.parkedRun(t, "self_improve")
	before = fx.requests
	if _, err := fx.svc.SetUserJudgeBinding(fx.env.ctx, fx.userID, BindModePinned, &fx.otherTok); err != nil {
		t.Fatalf("SetUserJudgeBinding: %v", err)
	}
	if fx.requests != before+1 {
		t.Fatalf("judge rebind requests = %d, want 1", fx.requests-before)
	}
	assertHeld(t, fx.env, judgeRun, false)

	reassigned := fx.parkedRun(t, "issue")
	fx.env.exec(`UPDATE workers SET anthropic_secret_id = $2 WHERE id = $1`, fx.workerID, fx.pinTok)
	before = fx.requests
	res, err := fx.svc.SetRunCredential(fx.env.ctx, fx.userID, reassigned, CredentialOverrideModePinned, &fx.otherTok)
	if err != nil || res.Run.Status != "queued" {
		t.Fatalf("SetRunCredential = (%v, %v), want queued", res.Run.Status, err)
	}
	if fx.requests != before+1 {
		t.Fatalf("reassignment requests = %d, want 1", fx.requests-before)
	}
}

// waitForAdvisoryWaiter blocks until some backend is waiting on THIS owner's secret mutation
// advisory lock, i.e. the goroutine under test reached lock-order step 1 and is queued behind
// the holder. The two-key advisory lock shows in pg_locks as classid = key 1, objid = key 2
// (both oids, so the signed int32 keys are compared as their unsigned 32-bit values) and
// objsubid = 2; any other advisory waiter on the shared test database is ignored.
func waitForAdvisoryWaiter(t *testing.T, env codexTestEnv, userID uuid.UUID) {
	t.Helper()
	// Widen, then mask to the low 32 bits: the oid view of a signed int32 key.
	classID := int64(store.SecretMutationLockClass) & 0xFFFFFFFF
	objID := int64(store.SecretMutationLockObjID(userID)) & 0xFFFFFFFF
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := env.pool.QueryRow(env.ctx, `SELECT count(*) FROM pg_locks
		    WHERE locktype = 'advisory' AND NOT granted AND objsubid = 2
		      AND classid::bigint = $1 AND objid::bigint = $2`, classID, objID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no backend queued on the advisory lock")
}

// TestCredentialPromoterLockOrderVsReassignmentLiveDB: a reassignment holding the user lock
// and then writing the run, against a promoter queued on that lock. The promoter takes the
// user lock BEFORE the run row, so the reassignment's run write never waits on it: no
// deadlock, the reassignment wins, and the promoter then sees the run already queued.
//
// MUTATION: move store.LockSecretMutation after LockCredentialDisabledRunForPromotion in
// promoteCredentialDisabledRun; the promoter then holds the run row while waiting on the user
// lock, the reassignment's run write waits on it, and Postgres aborts one side with 40P01.
func TestCredentialPromoterLockOrderVsReassignmentLiveDB(t *testing.T) {
	fx := newCDFix(t)
	runID := fx.parkedRun(t, "issue") // pinTok is enabled: promotable

	tx, err := fx.env.pool.Begin(fx.env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(fx.env.ctx) }()
	if err := store.LockSecretMutation(fx.env.ctx, tx, fx.userID); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var promoted bool
	var promoteErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		promoted, promoteErr = fx.svc.promoteCredentialDisabledRun(fx.env.ctx, fx.userID, runID)
	}()
	waitForAdvisoryWaiter(t, fx.env, fx.userID)
	ctx, cancel := context.WithTimeout(fx.env.ctx, 10*time.Second)
	defer cancel()
	if _, err := store.New(tx).ReassignCredentialDisabledRun(ctx, store.ReassignCredentialDisabledRunParams{
		ID: runID, UserID: fx.userID, Mode: pgconv.TextOrNull(BindModePinned), SecretID: pgconv.UUID(fx.otherTok),
		GlobalTimeoutSeconds: 7200,
	}); err != nil {
		t.Fatalf("reassignment under the user lock: %v", err)
	}
	if err := tx.Commit(fx.env.ctx); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if promoteErr != nil || promoted {
		t.Fatalf("promoter = (%t, %v), want (false, nil) after losing to the reassignment", promoted, promoteErr)
	}
	r := assertHeld(t, fx.env, runID, false)
	if uuid.UUID(r.CredentialOverrideSecretID.Bytes) != fx.otherTok {
		t.Fatalf("reassignment lost: override=%v", r.CredentialOverrideSecretID)
	}
}

// TestCredentialPromoterWinsReassignmentLiveDB: the other ordering. A promoter that already
// holds the user lock and has decided commits first; the concurrent reassignment then matches
// no held row and is refused with nothing written. Exactly one wins; no lost write.
func TestCredentialPromoterWinsReassignmentLiveDB(t *testing.T) {
	fx := newCDFix(t)
	runID := fx.parkedRun(t, "issue")
	var reassignErr error
	var wg sync.WaitGroup
	fx.svc.credPromoteHooks = &credentialPromoteTestHooks{beforePromote: func(context.Context, uuid.UUID) error {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, reassignErr = fx.svc.SetRunCredential(fx.env.ctx, fx.userID, runID, CredentialOverrideModePinned, &fx.otherTok)
		}()
		waitForAdvisoryWaiter(t, fx.env, fx.userID)
		return nil
	}}
	promoted, err := fx.svc.promoteCredentialDisabledRun(fx.env.ctx, fx.userID, runID)
	fx.svc.credPromoteHooks = nil
	wg.Wait()
	if err != nil || !promoted {
		t.Fatalf("promoter = (%t, %v), want it to win", promoted, err)
	}
	if !errors.Is(reassignErr, ErrCredentialSwitchRaced) {
		t.Fatalf("reassignment err = %v, want ErrCredentialSwitchRaced", reassignErr)
	}
	r := assertHeld(t, fx.env, runID, false)
	if r.CredentialOverrideMode.Valid || r.CredentialOverrideSecretID.Valid {
		t.Fatalf("the losing reassignment wrote an override: %v/%v", r.CredentialOverrideMode, r.CredentialOverrideSecretID)
	}
}

// TestCredentialPromoterConcurrentRequirementChangesLiveDB: a disable, a worker rebind onto a
// disabled pin and a Judge rebind onto a disabled pin, each committed while a promotion pass
// is queued behind the writer's user lock, leave their run held: the promoter evaluates the
// requirement only after the writer committed.
func TestCredentialPromoterConcurrentRequirementChangesLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name  string
		kind  string
		setup func(fx *cdFix)
		write func(fx *cdFix, q *store.Queries) error
	}{
		{"disable the pin", "issue", func(*cdFix) {}, func(fx *cdFix, q *store.Queries) error {
			_, err := q.SetSecretEnablement(fx.env.ctx, store.SetSecretEnablementParams{ID: fx.pinTok, UserID: fx.userID, Enabled: false})
			return err
		}},
		{"worker rebind onto a disabled pin", "issue", func(fx *cdFix) {
			fx.env.exec(`UPDATE workers SET anthropic_secret_id = $2 WHERE id = $1`, fx.workerID, fx.otherTok)
			fx.setEnabled(fx.pinTok, false)
		}, func(fx *cdFix, q *store.Queries) error {
			_, err := q.SetWorkerAnthropicSecret(fx.env.ctx, store.SetWorkerAnthropicSecretParams{
				ID: fx.workerID, UserID: fx.userID, AnthropicSecretID: pgconv.UUID(fx.pinTok), AnthropicBindMode: BindModePinned,
			})
			return err
		}},
		{"judge rebind onto a disabled pin", "self_improve", func(fx *cdFix) {
			fx.env.exec(`UPDATE users SET judge_anthropic_bind_mode = 'pinned', judge_anthropic_secret_id = $2 WHERE id = $1`, fx.userID, fx.otherTok)
			fx.setEnabled(fx.pinTok, false)
		}, func(fx *cdFix, q *store.Queries) error {
			_, err := q.SetUserJudgeAnthropicBinding(fx.env.ctx, store.SetUserJudgeAnthropicBindingParams{
				ID: fx.userID, JudgeAnthropicBindMode: BindModePinned, JudgeAnthropicSecretID: pgconv.UUID(fx.pinTok),
			})
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newCDFix(t)
			tc.setup(fx)
			runID := fx.parkedRun(t, tc.kind)
			tx, err := fx.env.pool.Begin(fx.env.ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(fx.env.ctx) }()
			if err := store.LockSecretMutation(fx.env.ctx, tx, fx.userID); err != nil {
				t.Fatal(err)
			}
			if err := tc.write(fx, store.New(tx)); err != nil {
				t.Fatalf("requirement write: %v", err)
			}
			var wg sync.WaitGroup
			var promoted bool
			var promoteErr error
			wg.Add(1)
			go func() {
				defer wg.Done()
				promoted, promoteErr = fx.svc.promoteCredentialDisabledRun(fx.env.ctx, fx.userID, runID)
			}()
			waitForAdvisoryWaiter(t, fx.env, fx.userID)
			if err := tx.Commit(fx.env.ctx); err != nil {
				t.Fatal(err)
			}
			wg.Wait()
			if promoteErr != nil || promoted {
				t.Fatalf("promoter = (%t, %v), want the run kept held on the new requirement", promoted, promoteErr)
			}
			assertHeld(t, fx.env, runID, true)
		})
	}
}

// TestCredentialPromoterMissingRunLiveDB: a candidate that vanished or belongs to another
// owner is skipped without error.
func TestCredentialPromoterMissingRunLiveDB(t *testing.T) {
	fx := newCDFix(t)
	if ok, err := fx.svc.promoteCredentialDisabledRun(fx.env.ctx, fx.userID, uuid.New()); ok || err != nil {
		t.Fatalf("missing run = (%t, %v)", ok, err)
	}
	runID := fx.parkedRun(t, "issue")
	if ok, err := fx.svc.promoteCredentialDisabledRun(fx.env.ctx, uuid.New(), runID); ok || err != nil {
		t.Fatalf("foreign owner = (%t, %v)", ok, err)
	}
	if _, err := fx.env.q.GetRunByID(fx.env.ctx, runID); errors.Is(err, pgx.ErrNoRows) {
		t.Fatal("run vanished")
	}
	assertHeld(t, fx.env, runID, true)
}
