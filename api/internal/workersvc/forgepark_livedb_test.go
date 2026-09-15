package workersvc

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #1392 M1 forge pre-clone park, against a REAL Postgres. A transient forge failure at
// clone parks the run on 'recovery_wait' with recovery_wait_cause='forge_unreachable',
// settling the exact-generation custody hold in the SAME locked transaction (release evidence
// 'no_adopted_source'), or fails it past the forge cap, or — if a cancel was stamped during
// the retries — cancels it (still releasing the hold). These drive the real Service (real
// *store.Queries + the pool as the tx beginner), not fakes.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (run via
// ./e2e/run-store-it.sh). A package that prints `ok` with PASS=0 is INVALID, not green.

func i64Ptr(v int64) *int64 { return &v }

// forgeParkService builds a Service over the live store with the pool wired as the tx beginner
// (the forge park transaction needs it) and a caller-chosen forge park cap.
func (e interlockLiveDB) forgeParkService(t *testing.T, maxParks int) *Service {
	t.Helper()
	p := testParams()
	p.RunForgeUnreachableMaxParks = maxParks
	svc := New(e.q, newBox(t), p)
	svc.SetTxBeginner(e.pool)
	svc.SetBackground(func(func()) {})
	return svc
}

// fpSeedRunning seeds a running issue run owned by workerID at claim_generation=gen.
func (e interlockLiveDB) fpSeedRunning(t *testing.T, workerID uuid.UUID, gen int64) uuid.UUID {
	t.Helper()
	runID := e.seedLegacyRunningRun(t, workerID)
	e.exec(t, `UPDATE runs SET claim_generation = $2 WHERE id = $1`, runID, gen)
	return runID
}

// enableJudge wires the just-built service AND the owner's DB rows so maybeEnqueueJudge can
// actually REACH the fail-origin skip gate on a terminal failure. Without it the service's
// settings==nil (Gate 2) returns BEFORE the skip is ever evaluated, so a judges==0 assertion
// passes vacuously and exercises the exclusion not at all. It mirrors eligibleFixture's offline
// wiring (judge_m3_test.go): the global kill-switch on, the owner opted in (users.judge_enabled),
// and an Anthropic token present (Gate 4's presence-only check). With this in place a
// NON-excluded terminal failure DOES enqueue a judge for this owner — which is exactly what
// makes a judges==0 assertion a genuine test of the fail-origin exclusion (and what makes the
// iteration_count>0 case redden on the pre-FIX-1 gate, where the judge is wrongly enqueued).
func (e interlockLiveDB) enableJudge(t *testing.T, svc *Service) {
	t.Helper()
	svc.SetSettings(fakeSettings{enabled: true})
	e.exec(t, `UPDATE users SET judge_enabled = true WHERE id = $1`, e.userID)
	e.exec(t, `INSERT INTO user_secrets (id, user_id, kind, label, is_default, ciphertext, sealed_with)
	           VALUES ($1, $2, 'anthropic_token', 'judge-token', true, $3, 'master')`,
		uuid.New(), e.userID, []byte("ct"))
}

// fpRun reads the forge-park-relevant run columns.
func (e interlockLiveDB) fpRun(t *testing.T, runID uuid.UUID) (status string, cause *string, forgeParkCount int32, retrySet bool, recoveryWaitCount int32) {
	t.Helper()
	var c pgtype.Text
	var retry pgtype.Timestamptz
	if err := e.pool.QueryRow(e.ctx,
		`SELECT status, recovery_wait_cause, forge_park_count, recovery_retry_not_before, recovery_wait_count FROM runs WHERE id = $1`, runID).
		Scan(&status, &c, &forgeParkCount, &retry, &recoveryWaitCount); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if c.Valid {
		s := c.String
		cause = &s
	}
	return status, cause, forgeParkCount, retry.Valid, recoveryWaitCount
}

// fpHoldEvidence reads a hold's state and release_evidence.
func (e interlockLiveDB) fpHoldEvidence(t *testing.T, holdID uuid.UUID) (state string, evidence *string) {
	t.Helper()
	var ev pgtype.Text
	if err := e.pool.QueryRow(e.ctx,
		`SELECT state, release_evidence FROM recovery_custody_holds WHERE id = $1`, holdID).Scan(&state, &ev); err != nil {
		t.Fatalf("read hold: %v", err)
	}
	if ev.Valid {
		s := ev.String
		evidence = &s
	}
	return state, evidence
}

// TestForgeParkReleasesGenerationHoldAndStampsLiveDB is the happy path: a forge_unreachable
// park releases EXACTLY the current generation's hold with evidence 'no_adopted_source' and
// stamps recovery_wait_cause='forge_unreachable', recovery_retry_not_before, and increments
// forge_park_count.
func TestForgeParkReleasesGenerationHoldAndStampsLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.forgeParkService(t, 6)

	w := e.seedWorker(t, nil)
	runID := e.fpSeedRunning(t, w, 1)
	hold := mhOpenHold(t, e, runID, 1, w)

	run, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, runID,
		StateRequest{State: "recovery_wait", RecoveryCause: strPtr("forge_unreachable"), ClaimGeneration: i64Ptr(1)})
	if err != nil {
		t.Fatalf("SetState(forge park): %v", err)
	}
	if !applied || run.Status != "recovery_wait" {
		t.Fatalf("forge park must apply as recovery_wait; applied=%v status=%q", applied, run.Status)
	}

	status, cause, forgeParkCount, retrySet, _ := e.fpRun(t, runID)
	if status != "recovery_wait" {
		t.Fatalf("run status = %q, want recovery_wait", status)
	}
	if cause == nil || *cause != "forge_unreachable" {
		t.Fatalf("recovery_wait_cause = %v, want forge_unreachable", cause)
	}
	if forgeParkCount != 1 {
		t.Fatalf("forge_park_count = %d, want 1 (incremented on the first forge park)", forgeParkCount)
	}
	if !retrySet {
		t.Fatal("recovery_retry_not_before was not stamped")
	}

	state, evidence := e.fpHoldEvidence(t, hold)
	if state != "released" {
		t.Fatalf("hold state = %q, want released (the park settles the generation's hold)", state)
	}
	if evidence == nil || *evidence != "no_adopted_source" {
		t.Fatalf("release_evidence = %v, want no_adopted_source", evidence)
	}
}

// TestForgeParkWrongGenerationStaleClaimLiveDB: a report whose claim_generation does not match
// the locked run's is 409 stale_claim with NOTHING mutated (the run stays running, the hold
// stays open, forge_park_count stays 0).
func TestForgeParkWrongGenerationStaleClaimLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.forgeParkService(t, 6)

	w := e.seedWorker(t, nil)
	runID := e.fpSeedRunning(t, w, 2) // current generation is 2
	hold := mhOpenHold(t, e, runID, 2, w)

	_, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, runID,
		StateRequest{State: "recovery_wait", RecoveryCause: strPtr("forge_unreachable"), ClaimGeneration: i64Ptr(1)}) // stale gen 1
	if !errors.Is(err, ErrForgeParkStaleClaim) {
		t.Fatalf("SetState err = %v, want ErrForgeParkStaleClaim", err)
	}
	if applied {
		t.Fatal("applied = true for a stale-claim refusal")
	}
	status, cause, forgeParkCount, retrySet, _ := e.fpRun(t, runID)
	if status != "running" || cause != nil || forgeParkCount != 0 || retrySet {
		t.Fatalf("stale_claim mutated the run: status=%q cause=%v forge_park_count=%d retrySet=%v", status, cause, forgeParkCount, retrySet)
	}
	if state, _ := e.fpHoldEvidence(t, hold); state != "open" {
		t.Fatalf("hold state = %q after a stale-claim refusal, want open (nothing settled)", state)
	}
}

// TestForgeParkReleasesOnlyCurrentGenerationLiveDB: a run with an OLDER-generation orphan hold
// plus the CURRENT generation's hold, both open. The park releases ONLY the current
// generation's hold; the older orphan stays open (D3 / #1349 D3 — one hold per generation).
func TestForgeParkReleasesOnlyCurrentGenerationLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.forgeParkService(t, 6)

	w := e.seedWorker(t, nil)
	runID := e.fpSeedRunning(t, w, 2)
	older := mhOpenHold(t, e, runID, 1, w) // orphan: protects an earlier generation's copy
	current := mhOpenHold(t, e, runID, 2, w)

	if _, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, runID,
		StateRequest{State: "recovery_wait", RecoveryCause: strPtr("forge_unreachable"), ClaimGeneration: i64Ptr(2)}); err != nil || !applied {
		t.Fatalf("SetState(forge park): applied=%v err=%v", applied, err)
	}
	if state, ev := e.fpHoldEvidence(t, current); state != "released" || ev == nil || *ev != "no_adopted_source" {
		t.Fatalf("current-generation hold state=%q evidence=%v, want released/no_adopted_source", state, ev)
	}
	if state, _ := e.fpHoldEvidence(t, older); state != "open" {
		t.Fatalf("older-generation orphan hold state = %q, want OPEN — the park must release ONLY the current generation", state)
	}
}

// TestForgeParkTwoHoldsSameTupleCustodyUnsettledLiveDB: two open holds for the SAME (run,
// worker, generation) tuple make the exact-hold cardinality != 1, so the park rolls back with
// 409 custody_unsettled and NOTHING is mutated (both holds stay open, the run stays running).
func TestForgeParkTwoHoldsSameTupleCustodyUnsettledLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.forgeParkService(t, 6)

	w := e.seedWorker(t, nil)
	runID := e.fpSeedRunning(t, w, 1)
	h1 := mhOpenHold(t, e, runID, 1, w)
	h2 := mhOpenHold(t, e, runID, 1, w) // a second row for the exact tuple

	_, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, runID,
		StateRequest{State: "recovery_wait", RecoveryCause: strPtr("forge_unreachable"), ClaimGeneration: i64Ptr(1)})
	if !errors.Is(err, ErrForgeParkCustodyUnsettled) {
		t.Fatalf("SetState err = %v, want ErrForgeParkCustodyUnsettled", err)
	}
	if applied {
		t.Fatal("applied = true for a custody-unsettled rollback")
	}
	if status, _, cnt, _, _ := e.fpRun(t, runID); status != "running" || cnt != 0 {
		t.Fatalf("custody-unsettled rollback mutated the run: status=%q forge_park_count=%d", status, cnt)
	}
	if s1, _ := e.fpHoldEvidence(t, h1); s1 != "open" {
		t.Fatalf("hold h1 state = %q, want open (rollback)", s1)
	}
	if s2, _ := e.fpHoldEvidence(t, h2); s2 != "open" {
		t.Fatalf("hold h2 state = %q, want open (rollback)", s2)
	}
}

// TestForgeParkCancelStampedBeforeParkLiveDB: an owner cancel stamped (stop_kind='cancelled')
// during the retries makes the park transition to CANCELLED instead, still releasing the
// generation's hold — so a cancelled run leaves zero open current-generation holds (D4).
func TestForgeParkCancelStampedBeforeParkLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.forgeParkService(t, 6)

	w := e.seedWorker(t, nil)
	runID := e.fpSeedRunning(t, w, 1)
	e.exec(t, `UPDATE runs SET stop_kind = 'cancelled' WHERE id = $1`, runID) // cancel stamped, status still running
	hold := mhOpenHold(t, e, runID, 1, w)

	run, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, runID,
		StateRequest{State: "recovery_wait", RecoveryCause: strPtr("forge_unreachable"), ClaimGeneration: i64Ptr(1)})
	if err != nil || !applied {
		t.Fatalf("SetState(cancel-during-forge-park): applied=%v err=%v", applied, err)
	}
	if run.Status != "cancelled" {
		t.Fatalf("run status = %q, want cancelled (a stamped cancel wins the park)", run.Status)
	}
	if status, _, cnt, _, _ := e.fpRun(t, runID); status != "cancelled" || cnt != 0 {
		t.Fatalf("run status=%q forge_park_count=%d, want cancelled/0 (a cancel does not forge-park)", status, cnt)
	}
	if state, ev := e.fpHoldEvidence(t, hold); state != "released" || ev == nil || *ev != "no_adopted_source" {
		t.Fatalf("hold state=%q evidence=%v after cancel, want released/no_adopted_source (a cancel still settles custody)", state, ev)
	}
}

// TestForgeParkCapExceededFailsLiveDB: the (cap+1)th forge park fails the run with
// fail_origin='forge_unreachable' (server-derived) and a reason naming the count, and enqueues
// no judge run. The hold is still released with evidence 'no_adopted_source'.
//
// enableJudge wires the judge path so the judges==0 assertion is GENUINE: without it the
// service's settings==nil short-circuits maybeEnqueueJudge at Gate 2 before the fail-origin
// skip is reached, and judges==0 would hold no matter what the exclusion did. The
// iteration_count>0 companion (TestForgeParkCapExceededResumedRunNoJudgeLiveDB) is what pins
// the FIX-1 regression; this iteration_count==0 case stays as the base cap-fail case.
func TestForgeParkCapExceededFailsLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.forgeParkService(t, 2) // cap 2
	e.enableJudge(t, svc)

	w := e.seedWorker(t, nil)
	runID := e.fpSeedRunning(t, w, 1)
	// This run has already forge-parked twice; the next park would be the 3rd (> cap 2) → fail.
	e.exec(t, `UPDATE runs SET forge_park_count = 2 WHERE id = $1`, runID)
	hold := mhOpenHold(t, e, runID, 1, w)

	run, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, runID,
		StateRequest{State: "recovery_wait", RecoveryCause: strPtr("forge_unreachable"), ClaimGeneration: i64Ptr(1)})
	if err != nil || !applied {
		t.Fatalf("SetState(cap-exceeded): applied=%v err=%v", applied, err)
	}
	if run.Status != "failed" {
		t.Fatalf("run status = %q, want failed (past the cap)", run.Status)
	}
	var origin, reason pgtype.Text
	if err := e.pool.QueryRow(e.ctx, `SELECT fail_origin, failure_reason FROM runs WHERE id = $1`, runID).Scan(&origin, &reason); err != nil {
		t.Fatalf("read fail_origin: %v", err)
	}
	if !origin.Valid || origin.String != "forge_unreachable" {
		t.Fatalf("fail_origin = %v, want forge_unreachable", origin)
	}
	if !reason.Valid || reason.String != "the forge stayed unreachable at clone across 3 parks" {
		t.Fatalf("failure_reason = %q, want the count-naming reason", reason.String)
	}
	if state, ev := e.fpHoldEvidence(t, hold); state != "released" || ev == nil || *ev != "no_adopted_source" {
		t.Fatalf("hold state=%q evidence=%v after cap-fail, want released/no_adopted_source", state, ev)
	}
	// No judge run targets this run: forge_unreachable is in neverJudgeFailOrigins, so it skips
	// the judge (SC3). The judge path is reachable here (enableJudge), so this is a real assertion
	// of the exclusion, not the settings==nil early-return the pre-#1392 helper left it as.
	var judges int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM runs WHERE kind = 'judge' AND target_run_id = $1`, runID).Scan(&judges); err != nil {
		t.Fatalf("count judge runs: %v", err)
	}
	if judges != 0 {
		t.Fatalf("judge runs targeting the forge-failed run = %d, want 0", judges)
	}
}

// TestForgeParkCapExceededResumedRunNoJudgeLiveDB is the FIX-1 regression against a REAL
// Postgres (PRD #1392 M1, SC3): a RESUMED run that already did work (iteration_count > 0), then
// hit a forge outage and forge-parked to the cap, cap-fails with fail_origin='forge_unreachable'
// — and must enqueue NO judge, even though its iteration_count is not 0. This is the case the
// pre-fix gate got wrong: forge_unreachable lived in preStartInfraFailOrigins, gated on
// iteration_count==0, so a run with iteration_count>0 fell through the skip and WAS judged.
//
// It reddens on the unfixed code (judges==1 — the judge wrongly enqueued for the resumed run)
// and passes once forge_unreachable skips the judge regardless of iteration_count
// (neverJudgeFailOrigins). enableJudge makes the judge path genuinely reachable, so judges==0 is
// a real assertion of the exclusion rather than a settings==nil early-return.
func TestForgeParkCapExceededResumedRunNoJudgeLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.forgeParkService(t, 2) // cap 2
	e.enableJudge(t, svc)

	w := e.seedWorker(t, nil)
	runID := e.fpSeedRunning(t, w, 1)
	// A resumed run that DID work: iteration_count advanced to 5 (SetRunRunning uses GREATEST and
	// never resets it across park/promote/claim/fail), and it has already forge-parked twice — so
	// the next park is the 3rd (> cap 2) → cap-fail with iteration_count still 5.
	e.exec(t, `UPDATE runs SET forge_park_count = 2, iteration_count = 5 WHERE id = $1`, runID)
	hold := mhOpenHold(t, e, runID, 1, w)

	run, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, runID,
		StateRequest{State: "recovery_wait", RecoveryCause: strPtr("forge_unreachable"), ClaimGeneration: i64Ptr(1)})
	if err != nil || !applied {
		t.Fatalf("SetState(cap-exceeded, resumed): applied=%v err=%v", applied, err)
	}
	if run.Status != "failed" {
		t.Fatalf("run status = %q, want failed (past the cap)", run.Status)
	}
	// The failed run still carries the work it did — the iteration_count>0 the pre-fix gate keyed
	// off — and the server-derived forge origin. Asserting iteration_count>0 pins the precondition
	// this regression turns on: if it were 0 the case would collapse into the base cap-fail test.
	var origin pgtype.Text
	var iter int32
	if err := e.pool.QueryRow(e.ctx, `SELECT fail_origin, iteration_count FROM runs WHERE id = $1`, runID).Scan(&origin, &iter); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if !origin.Valid || origin.String != "forge_unreachable" {
		t.Fatalf("fail_origin = %v, want forge_unreachable", origin)
	}
	if iter == 0 {
		t.Fatal("iteration_count = 0, want > 0 (the resumed-run precondition this test turns on)")
	}
	if state, ev := e.fpHoldEvidence(t, hold); state != "released" || ev == nil || *ev != "no_adopted_source" {
		t.Fatalf("hold state=%q evidence=%v after cap-fail, want released/no_adopted_source", state, ev)
	}
	// NO judge run targets this run: forge_unreachable skips the judge REGARDLESS of
	// iteration_count. On the pre-FIX-1 gate this count is 1 (the judge wrongly enqueued for the
	// resumed run), which is exactly how this test reddens without FIX 1.
	var judges int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM runs WHERE kind = 'judge' AND target_run_id = $1`, runID).Scan(&judges); err != nil {
		t.Fatalf("count judge runs: %v", err)
	}
	if judges != 0 {
		t.Fatalf("judge runs targeting the resumed forge-failed run = %d, want 0 (SC3, regardless of iteration_count)", judges)
	}
}

// TestForgeParkCapZeroDisablesLiveDB: RUN_FORGE_UNREACHABLE_MAX_PARKS=0 is unlimited — even a
// run that has already parked many times parks again rather than failing.
func TestForgeParkCapZeroDisablesLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.forgeParkService(t, 0) // unlimited

	w := e.seedWorker(t, nil)
	runID := e.fpSeedRunning(t, w, 1)
	e.exec(t, `UPDATE runs SET forge_park_count = 99 WHERE id = $1`, runID)
	mhOpenHold(t, e, runID, 1, w)

	run, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, runID,
		StateRequest{State: "recovery_wait", RecoveryCause: strPtr("forge_unreachable"), ClaimGeneration: i64Ptr(1)})
	if err != nil || !applied {
		t.Fatalf("SetState(cap 0): applied=%v err=%v", applied, err)
	}
	if run.Status != "recovery_wait" {
		t.Fatalf("run status = %q, want recovery_wait (cap 0 = unlimited, never fails on count)", run.Status)
	}
	if _, _, cnt, _, _ := e.fpRun(t, runID); cnt != 100 {
		t.Fatalf("forge_park_count = %d, want 100 (incremented, not capped)", cnt)
	}
}

// TestForgeParkIdempotentDuplicateLiveDB: a duplicate forge-park report onto an already-parked
// run (a lost ack) is a 409 no-op with status recovery_wait, NOT a double increment.
func TestForgeParkIdempotentDuplicateLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.forgeParkService(t, 6)

	w := e.seedWorker(t, nil)
	runID := e.fpSeedRunning(t, w, 1)
	mhOpenHold(t, e, runID, 1, w)

	// First park.
	if _, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, runID,
		StateRequest{State: "recovery_wait", RecoveryCause: strPtr("forge_unreachable"), ClaimGeneration: i64Ptr(1)}); err != nil || !applied {
		t.Fatalf("first forge park: applied=%v err=%v", applied, err)
	}
	// Duplicate report (lost ack): the run is already recovery_wait/forge_unreachable at this gen.
	run, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, runID,
		StateRequest{State: "recovery_wait", RecoveryCause: strPtr("forge_unreachable"), ClaimGeneration: i64Ptr(1)})
	if err != nil {
		t.Fatalf("duplicate forge park err = %v, want nil (idempotent 409)", err)
	}
	if applied {
		t.Fatal("applied = true on a duplicate forge park (should be a 409 no-op)")
	}
	if run.Status != "recovery_wait" {
		t.Fatalf("duplicate report run status = %q, want recovery_wait", run.Status)
	}
	if _, _, cnt, _, _ := e.fpRun(t, runID); cnt != 1 {
		t.Fatalf("forge_park_count = %d after a duplicate, want 1 (never double-incremented)", cnt)
	}
}

// TestEmptyTurnParkLeavesForgeCountAndClearsCauseLiveDB: the empty-turn (untyped) recovery park
// leaves forge_park_count UNCHANGED and CLEARS recovery_wait_cause to NULL, so a run that
// forge-parked earlier keeps its lifetime count through a later empty-turn park and renders the
// generic wording (SC5 / D9). Set up directly: a running run carrying a stale forge-park count
// and cause, then an untyped recovery_wait report.
func TestEmptyTurnParkLeavesForgeCountAndClearsCauseLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.forgeParkService(t, 6)

	w := e.seedWorker(t, nil)
	runID := e.fpSeedRunning(t, w, 1)
	e.exec(t, `UPDATE runs SET forge_park_count = 2, recovery_wait_cause = 'forge_unreachable' WHERE id = $1`, runID)

	// An untyped empty-turn park (no recovery_cause).
	run, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, runID, StateRequest{State: "recovery_wait"})
	if err != nil || !applied {
		t.Fatalf("SetState(empty-turn park): applied=%v err=%v", applied, err)
	}
	if run.Status != "recovery_wait" {
		t.Fatalf("run status = %q, want recovery_wait", run.Status)
	}
	status, cause, forgeParkCount, _, _ := e.fpRun(t, runID)
	if status != "recovery_wait" {
		t.Fatalf("status = %q, want recovery_wait", status)
	}
	if cause != nil {
		t.Fatalf("recovery_wait_cause = %v after an empty-turn park, want NULL (the untyped park clears it)", *cause)
	}
	if forgeParkCount != 2 {
		t.Fatalf("forge_park_count = %d after an empty-turn park, want 2 (unchanged — the empty-turn park never touches it)", forgeParkCount)
	}
}

// TestForgeParkThenPromoteThenEmptyTurnLiveDB is the full-lifecycle SC5 case: forge-park (count
// 1, cause forge_unreachable) → promote to queued → reclaim (running) → empty-turn park. The
// cause ends NULL and forge_park_count stays at its prior value (1).
func TestForgeParkThenPromoteThenEmptyTurnLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.forgeParkService(t, 6)

	w := e.seedWorker(t, nil)
	runID := e.fpSeedRunning(t, w, 1)
	mhOpenHold(t, e, runID, 1, w)

	if _, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, runID,
		StateRequest{State: "recovery_wait", RecoveryCause: strPtr("forge_unreachable"), ClaimGeneration: i64Ptr(1)}); err != nil || !applied {
		t.Fatalf("forge park: applied=%v err=%v", applied, err)
	}
	// Promote: force the retry stamp into the past, then run the promotion pass.
	e.exec(t, `UPDATE runs SET recovery_retry_not_before = now() - interval '1 minute' WHERE id = $1`, runID)
	if _, err := e.q.PromoteRecoveryWaitRuns(e.ctx, pgconv.Time(time.Now())); err != nil {
		t.Fatalf("PromoteRecoveryWaitRuns: %v", err)
	}
	if status, _, _, _, _ := e.fpRun(t, runID); status != "queued" {
		t.Fatalf("status after promote = %q, want queued", status)
	}
	// Reclaim: back to running (worker kept).
	e.exec(t, `UPDATE runs SET status = 'running', worker_id = $2 WHERE id = $1`, runID, w)

	// Empty-turn park.
	if _, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, runID, StateRequest{State: "recovery_wait"}); err != nil || !applied {
		t.Fatalf("empty-turn park: applied=%v err=%v", applied, err)
	}
	status, cause, forgeParkCount, _, _ := e.fpRun(t, runID)
	if status != "recovery_wait" || cause != nil || forgeParkCount != 1 {
		t.Fatalf("after forge-park→promote→empty-turn: status=%q cause=%v forge_park_count=%d, want recovery_wait/NULL/1", status, cause, forgeParkCount)
	}
}
