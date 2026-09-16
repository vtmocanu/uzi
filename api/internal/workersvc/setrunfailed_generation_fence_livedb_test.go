package workersvc

import (
	"testing"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// This file is the live-DB half of PRD #1247 M5a-1 rework (m6): it EXECUTES the per-query
// generation fence now on SetRunFailed against a REAL Postgres. The limit_wait NON-park path
// (setLimitWait with d.Park==false — the opt-out / budget-exhausted branch) calls SetRunFailed
// OUTSIDE the SetState FOR UPDATE fence (stateUsesForUpdateFence is false for limit_wait), so a
// LATE gen-G opt-out report could otherwise fail a run that was already RELEASED (queued after a
// held-state credential switch, same generation, claim_released_at set) or already RECLAIMED by
// the same worker to G+1 (running) — clobbering the reclaiming flight. The nil check alone does not
// help: a stale worker supplies a non-nil generation. The fence (claim_generation = G AND
// claim_released_at IS NULL) is the protection. sqlc's type deduction is not Postgres's, so this
// guarded statement can pass `sqlc generate` yet fail at prepare/execute — hence the live-DB test.
// Skipped unless UZI_TEST_DATABASE_URL is set (setupCodexLiveDB skips); the lead runs the sweep.

// TestSetLimitWaitNonParkOptOutGenerationFenceLiveDB drives the limit_wait NON-park path (a run with
// wait_on_limit=false opts every report out of the park, so setLimitWait coerces it to the
// server-composed SetRunFailed instead of parking) and pins the four cases:
//
//	(a) released-before-reclaim: run at gen G, claim_released_at SET (queued after release) — a gen-G
//	    fail is REJECTED (0 rows, not applied; the run stays queued, not failed).
//	(b) stale-after-reclaim: run reclaimed to G+1 (running) — a report carrying gen-G is REJECTED
//	    (the run stays running at G+1, not failed).
//	(c) legitimate in-generation, unreleased: run at G, claim_released_at NULL — a gen-G fail SUCCEEDS
//	    (the run becomes failed).
//	(d) legacy nil: a nil-generation fail applies UNFENCED even on a released claim (the run becomes
//	    failed), proving the outer-lock/legacy callers are unaffected by the new conjunct.
//
// Mutation-check: removing ONLY the `claim_generation = ...` conjunct reddens (b) (the stale report
// would fail the reclaimed run); removing ONLY `claim_released_at IS NULL` reddens (a) (the released
// report would fail the requeued run).
func TestSetLimitWaitNonParkOptOutGenerationFenceLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	// Real store + the pool as the tx beginner (the FOR UPDATE fence opens a tx when a report
	// carries a generation — even though limit_wait itself does NOT nest under it, other arms could,
	// and the wiring must be present). Background work is dropped so a terminal transition's detached
	// automation cannot outlive the test's pool.
	svc := New(env.q, env.box, testParams())
	svc.SetTxBeginner(env.pool)
	svc.SetBackground(func(func()) {})

	o := seedReevalOwner(t, env, BindModeAuto, false)
	wkr := store.Worker{ID: o.workerID, UserID: o.userID}
	g := int64(4)

	// (a) released-before-reclaim: the claim was released (a held-state switch requeued the run to
	// 'queued' at the SAME generation with claim_released_at set). A late gen-G opt-out fail must NOT
	// fail it — the claim_released_at IS NULL conjunct rejects it.
	released := seedHeldRun(t, env, o, 6200, "queued", g, false, true /* released */)
	env.exec(`UPDATE runs SET wait_on_limit = false WHERE id = $1`, released)
	gRel := g
	_, applied, err := svc.SetState(env.ctx, wkr, released, StateRequest{State: "limit_wait", ClaimGeneration: &gRel})
	if err != nil {
		t.Fatalf("released-claim gen-G opt-out: err = %v, want nil (0-row no-op ack)", err)
	}
	if applied {
		t.Fatal("a gen-G opt-out fail against a RELEASED claim must NOT apply (claim_released_at IS NULL conjunct)")
	}
	if got := statusOf(t, env, released); got != "queued" {
		t.Fatalf("status = %q, want the released run UNCHANGED at queued (a fenced-out fail must not fail the requeued run)", got)
	}

	// (b) stale-after-reclaim: the same worker reclaimed the run to G+1 (running, claim_released_at
	// cleared). A report carrying the OLD gen-G must NOT fail the reclaiming flight — the
	// claim_generation conjunct rejects it.
	reclaimed := seedHeldRun(t, env, o, 6201, "running", g+1, false, false)
	env.exec(`UPDATE runs SET wait_on_limit = false WHERE id = $1`, reclaimed)
	gStale := g
	_, applied, err = svc.SetState(env.ctx, wkr, reclaimed, StateRequest{State: "limit_wait", ClaimGeneration: &gStale})
	if err != nil {
		t.Fatalf("stale gen-G opt-out: err = %v, want nil (0-row no-op ack)", err)
	}
	if applied {
		t.Fatal("a stale gen-G opt-out fail must NOT apply against a run reclaimed to G+1 (claim_generation conjunct)")
	}
	if got := statusOf(t, env, reclaimed); got != "running" {
		t.Fatalf("status = %q, want the reclaimed run UNCHANGED at running (a stale fail must not clobber G+1)", got)
	}

	// (c) legitimate in-generation, unreleased: run at G, claim_released_at NULL. A gen-G opt-out
	// fail is the still-held run's own report and SUCCEEDS.
	current := seedHeldRun(t, env, o, 6202, "running", g, false, false)
	env.exec(`UPDATE runs SET wait_on_limit = false WHERE id = $1`, current)
	gCur := g
	_, applied, err = svc.SetState(env.ctx, wkr, current, StateRequest{State: "limit_wait", ClaimGeneration: &gCur})
	if err != nil {
		t.Fatalf("in-generation opt-out: %v", err)
	}
	if !applied {
		t.Fatal("a current-generation, unreleased opt-out fail must apply")
	}
	if got := statusOf(t, env, current); got != "failed" {
		t.Fatalf("status = %q, want failed after the in-generation opt-out", got)
	}

	// (d) legacy nil: a nil-generation opt-out fail applies UNFENCED even on the SAME released-claim
	// state as (a), proving the legacy / outer-lock-fenced callers are unaffected by the new conjunct.
	legacy := seedHeldRun(t, env, o, 6203, "queued", g, false, true /* released */)
	env.exec(`UPDATE runs SET wait_on_limit = false WHERE id = $1`, legacy)
	_, applied, err = svc.SetState(env.ctx, wkr, legacy, StateRequest{State: "limit_wait"}) // nil generation
	if err != nil {
		t.Fatalf("legacy nil opt-out: %v", err)
	}
	if !applied {
		t.Fatal("a legacy nil-generation opt-out fail must apply UNFENCED")
	}
	if got := statusOf(t, env, legacy); got != "failed" {
		t.Fatalf("status = %q, want failed (a legacy nil-generation fail bypasses the fence, even on a released claim)", got)
	}
}
