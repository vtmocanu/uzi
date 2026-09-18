package workersvc

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #1247 MAJOR-5 live-DB coverage of the generation fence extended to the FOUR completion /
// pending-request paths that carried no generation before this milestone: (1) pause_failed's
// ClearPauseRequest, (2) SetRunCompletionHold's park, (3) RecordCompletionAttempt's attempt, and
// (4) RequestCompletionPermit's issue. Each mirrors the EXISTING fence already on the interlocked
// completion / SetState paths: a report from a STALE same-worker flight (its claim RELEASED by a
// held-state switch, or SUPERSEDED by a reclaim) must mutate NOTHING when it carries the OLD
// generation, while a CURRENT-generation report succeeds, and a CAPABILITY worker that OMITS the
// generation is refused (fail-closed). Every test here reddens if the fence conjunct is removed
// from its query (or the Go fail-closed check is dropped). These run against a real throwaway
// Postgres (skipped without UZI_TEST_DATABASE_URL); the lead runs the live-DB sweep.

// pauseRequested reports whether runs.pause_requested_at is still SET (the pending-pause marker).
func (e interlockLiveDB) pauseRequested(t *testing.T, runID uuid.UUID) bool {
	t.Helper()
	var set bool
	if err := e.pool.QueryRow(e.ctx, `SELECT pause_requested_at IS NOT NULL FROM runs WHERE id = $1`, runID).Scan(&set); err != nil {
		t.Fatalf("read pause_requested_at: %v", err)
	}
	return set
}

// seedPausePending arms a run's pending-pause columns as SetRunPauseRequested would, so a
// pause_failed report has something to withdraw.
func (e interlockLiveDB) seedPausePending(t *testing.T, runID uuid.UUID, gen int64) {
	t.Helper()
	e.exec(t, `UPDATE runs SET pause_requested_at = now(), pause_mode = 'milestone', claim_generation = $2 WHERE id = $1`, runID, gen)
}

// TestMajor5PauseFailedGenerationFenceLiveDB (MAJOR-5 path 1) pins the ClearPauseRequest fence: a
// pause_failed report from a STALE flight cannot withdraw the NEW flight's pending pause, a
// current-generation report does, and a capability worker omitting the generation is refused.
// pause_failed intentionally keeps its return-success behavior (the run stays running) — the FENCE,
// not the rowcount, is the protection. Mutation-check: drop the fence conjunct from ClearPauseRequest
// and the stale sub-tests redden (the pause would be cleared); drop the Go fail-closed check and the
// fail-closed sub-test reddens.
func TestMajor5PauseFailedGenerationFenceLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	wid := e.seedWorker(t, nil) // no capability: the generation-present cases fence on presence alone
	wkr := store.Worker{ID: wid}
	g := int64(4)

	// (a) STALE by a SUPERSEDING reclaim (generation advanced past the reported one).
	superseded := e.seedFrozenRun(t, wid, []string{"m1"}, []string{"m1"}, false)
	e.seedPausePending(t, superseded, g+1) // the run now lives at g+1
	old := g
	run, applied, err := svc.SetState(e.ctx, wkr, superseded, StateRequest{State: "pause_failed", ClaimGeneration: &old})
	if err != nil {
		t.Fatalf("stale (superseded) pause_failed: %v", err)
	}
	if !applied || run.Status != "running" {
		t.Fatalf("pause_failed keeps the run running; applied=%v status=%q", applied, run.Status)
	}
	if !e.pauseRequested(t, superseded) {
		t.Fatal("a STALE (superseded-generation) pause_failed must NOT clear the NEW flight's pending pause")
	}

	// (b) STALE by a RELEASED claim (matching generation, but claim_released_at set).
	released := e.seedFrozenRun(t, wid, []string{"m1"}, []string{"m1"}, false)
	e.seedPausePending(t, released, g)
	e.exec(t, `UPDATE runs SET claim_released_at = now() WHERE id = $1`, released)
	gg := g
	if _, _, err := svc.SetState(e.ctx, wkr, released, StateRequest{State: "pause_failed", ClaimGeneration: &gg}); err != nil {
		t.Fatalf("stale (released) pause_failed: %v", err)
	}
	if !e.pauseRequested(t, released) {
		t.Fatal("a pause_failed on a RELEASED claim must NOT clear the pending pause (claim_released_at IS NULL conjunct)")
	}

	// (c) CURRENT generation, unreleased claim: the pause is cleared.
	current := e.seedFrozenRun(t, wid, []string{"m1"}, []string{"m1"}, false)
	e.seedPausePending(t, current, g)
	gcur := g
	if _, applied2, err := svc.SetState(e.ctx, wkr, current, StateRequest{State: "pause_failed", ClaimGeneration: &gcur}); err != nil || !applied2 {
		t.Fatalf("current pause_failed: applied=%v err=%v", applied2, err)
	}
	if e.pauseRequested(t, current) {
		t.Fatal("a CURRENT-generation pause_failed must clear the pending pause")
	}

	// (d) FAIL-CLOSED: a capability worker that OMITS the generation is refused.
	capWkr := store.Worker{ID: wid, ProtocolCapabilities: []string{capability.CredentialSwitchV1}}
	failClosed := e.seedFrozenRun(t, wid, []string{"m1"}, []string{"m1"}, false)
	e.seedPausePending(t, failClosed, g)
	_, appliedFC, err := svc.SetState(e.ctx, capWkr, failClosed, StateRequest{State: "pause_failed"})
	if !errors.Is(err, ErrMissingClaimGeneration) {
		t.Fatalf("capability worker omitting generation: err = %v, want ErrMissingClaimGeneration", err)
	}
	if appliedFC {
		t.Fatal("a fail-closed pause_failed must not be applied")
	}
	if !e.pauseRequested(t, failClosed) {
		t.Fatal("a fail-closed pause_failed must not clear the pending pause")
	}
}

// TestMajor5CompletionHoldGenerationFenceLiveDB (MAJOR-5 path 2) pins the SetRunCompletionHold
// fence: a park order from a STALE flight cannot park the reclaimed run (0 rows -> applied=false ->
// the worker retains the run live), a current-generation park lands, and a capability worker
// omitting the generation is refused. Mutation-check: drop the fence conjunct from
// SetRunCompletionHold and the stale sub-tests redden (the run would park); drop the Go fail-closed
// check and the fail-closed sub-test reddens.
func TestMajor5CompletionHoldGenerationFenceLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	wid := e.seedWorker(t, nil)
	wkr := store.Worker{ID: wid}
	g := int64(6)

	// (a) STALE by a SUPERSEDING reclaim.
	superseded := e.seedFrozenRun(t, wid, []string{"m1"}, []string{"m1"}, false)
	e.exec(t, `UPDATE runs SET completion_attempts = 1, claim_generation = $2 WHERE id = $1`, superseded, g+1)
	old := g
	run, applied, err := svc.SetRunCompletionHold(e.ctx, wkr, superseded, "h1", &old)
	if err != nil {
		t.Fatalf("stale (superseded) hold: %v", err)
	}
	if applied {
		t.Fatal("a STALE (superseded-generation) hold must NOT park the reclaimed run")
	}
	if run.Status != "running" {
		t.Fatalf("a refused stale hold must leave status unchanged; got %q, want running", run.Status)
	}
	if s := e.runStatus(t, superseded); s != "running" {
		t.Fatalf("db status after refused stale hold = %q, want running", s)
	}

	// (b) STALE by a RELEASED claim (matching generation, claim_released_at set).
	released := e.seedFrozenRun(t, wid, []string{"m1"}, []string{"m1"}, false)
	e.exec(t, `UPDATE runs SET completion_attempts = 1, claim_generation = $2, claim_released_at = now() WHERE id = $1`, released, g)
	gg := g
	_, appliedR, err := svc.SetRunCompletionHold(e.ctx, wkr, released, "h2", &gg)
	if err != nil {
		t.Fatalf("stale (released) hold: %v", err)
	}
	if appliedR {
		t.Fatal("a hold on a RELEASED claim must NOT park (claim_released_at IS NULL conjunct)")
	}
	if s := e.runStatus(t, released); s != "running" {
		t.Fatalf("db status after refused released hold = %q, want running", s)
	}

	// (c) CURRENT generation, unreleased claim: the hold lands.
	current := e.seedFrozenRun(t, wid, []string{"m1"}, []string{"m1"}, false)
	e.exec(t, `UPDATE runs SET completion_attempts = 1, claim_generation = $2 WHERE id = $1`, current, g)
	gcur := g
	run3, applied3, err := svc.SetRunCompletionHold(e.ctx, wkr, current, "h3", &gcur)
	if err != nil {
		t.Fatalf("current hold: %v", err)
	}
	if !applied3 || run3.Status != "paused" {
		t.Fatalf("a CURRENT-generation hold must park the run; applied=%v status=%q", applied3, run3.Status)
	}

	// (d) FAIL-CLOSED: a capability worker that OMITS the generation is refused.
	capWkr := store.Worker{ID: wid, ProtocolCapabilities: []string{capability.CredentialSwitchV1}}
	failClosed := e.seedFrozenRun(t, wid, []string{"m1"}, []string{"m1"}, false)
	e.exec(t, `UPDATE runs SET completion_attempts = 1, claim_generation = $2 WHERE id = $1`, failClosed, g)
	_, appliedFC, err := svc.SetRunCompletionHold(e.ctx, capWkr, failClosed, "h4", nil)
	if !errors.Is(err, ErrMissingClaimGeneration) {
		t.Fatalf("capability worker omitting generation: err = %v, want ErrMissingClaimGeneration", err)
	}
	if appliedFC {
		t.Fatal("a fail-closed hold must not be applied")
	}
	if s := e.runStatus(t, failClosed); s != "running" {
		t.Fatalf("db status after fail-closed hold = %q, want running", s)
	}
}

// TestMajor5CompletionAttemptGenerationFenceLiveDB (MAJOR-5 path 3) pins the RecordCompletionAttempt
// fence: an attempt from a STALE flight records NOTHING (both the ins CTE and the counter/summary
// UPDATE fence out -> pgx.ErrNoRows -> ErrCompletionStaleClaim), a current-generation attempt is
// recorded, and a capability worker omitting the generation is refused. Mutation-check: drop the
// fence conjunct from EITHER guard in RecordCompletionAttempt and the stale sub-tests redden (an
// attempt would be recorded); drop the Go fail-closed check and the fail-closed sub-test reddens.
func TestMajor5CompletionAttemptGenerationFenceLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	wid := e.seedWorker(t, nil)
	wkr := store.Worker{ID: wid}
	g := int64(8)

	assertNoAttempt := func(runID uuid.UUID) {
		t.Helper()
		if got := e.attemptRowCount(t, runID); got != 0 {
			t.Fatalf("a fenced-out attempt must record no row; got %d", got)
		}
		if count, latest := e.runCompletionAttempts(t, runID); count != 0 || latest {
			t.Fatalf("a fenced-out attempt must not bump the counter/summary; count=%d latestSet=%v", count, latest)
		}
	}

	// (a) STALE by a SUPERSEDING reclaim.
	superseded := e.seedFrozenRun(t, wid, []string{"m1", "m2"}, []string{"m1"}, false)
	e.exec(t, `UPDATE runs SET claim_generation = $2 WHERE id = $1`, superseded, g+1)
	old := g
	if _, err := svc.RecordCompletionAttempt(e.ctx, wkr, superseded, CompletionAttemptRequest{Head: "h1", WorktreeFingerprint: "wf1", ClaimGeneration: &old}); !errors.Is(err, ErrCompletionStaleClaim) {
		t.Fatalf("stale (superseded) attempt: err = %v, want ErrCompletionStaleClaim", err)
	}
	assertNoAttempt(superseded)

	// (b) STALE by a RELEASED claim (matching generation, claim_released_at set).
	released := e.seedFrozenRun(t, wid, []string{"m1", "m2"}, []string{"m1"}, false)
	e.exec(t, `UPDATE runs SET claim_generation = $2, claim_released_at = now() WHERE id = $1`, released, g)
	gg := g
	if _, err := svc.RecordCompletionAttempt(e.ctx, wkr, released, CompletionAttemptRequest{Head: "h2", WorktreeFingerprint: "wf2", ClaimGeneration: &gg}); !errors.Is(err, ErrCompletionStaleClaim) {
		t.Fatalf("stale (released) attempt: err = %v, want ErrCompletionStaleClaim", err)
	}
	assertNoAttempt(released)

	// (c) CURRENT generation, unreleased claim: the attempt is recorded.
	current := e.seedFrozenRun(t, wid, []string{"m1", "m2"}, []string{"m1"}, false)
	e.exec(t, `UPDATE runs SET claim_generation = $2 WHERE id = $1`, current, g)
	gcur := g
	res, err := svc.RecordCompletionAttempt(e.ctx, wkr, current, CompletionAttemptRequest{Head: "h3", WorktreeFingerprint: "wf3", ClaimGeneration: &gcur})
	if err != nil {
		t.Fatalf("current attempt: %v", err)
	}
	if res.AttemptCount != 1 {
		t.Fatalf("a CURRENT-generation attempt must be recorded; attempt_count = %d, want 1", res.AttemptCount)
	}
	if got := e.attemptRowCount(t, current); got != 1 {
		t.Fatalf("run_completion_attempts rows = %d, want 1", got)
	}

	// (d) FAIL-CLOSED: a capability worker that OMITS the generation is refused.
	capWkr := store.Worker{ID: wid, ProtocolCapabilities: []string{capability.CredentialSwitchV1}}
	failClosed := e.seedFrozenRun(t, wid, []string{"m1", "m2"}, []string{"m1"}, false)
	e.exec(t, `UPDATE runs SET claim_generation = $2 WHERE id = $1`, failClosed, g)
	if _, err := svc.RecordCompletionAttempt(e.ctx, capWkr, failClosed, CompletionAttemptRequest{Head: "h4"}); !errors.Is(err, ErrMissingClaimGeneration) {
		t.Fatalf("capability worker omitting generation: err = %v, want ErrMissingClaimGeneration", err)
	}
	assertNoAttempt(failClosed)
}

// TestMajor5CompletionPermitGenerationFenceLiveDB (MAJOR-5 path 4) pins the RequestCompletionPermit
// Go-level fence: a permit request from a STALE flight is refused ErrCompletionStaleClaim and issues
// NO permit, a current-generation request grants, and a capability worker omitting the generation is
// refused ErrMissingClaimGeneration. Mutation-check: drop the Go fence switch in RequestCompletionPermit
// and the stale sub-tests redden (a permit would be issued for the stale flight); drop the fail-closed
// arm and the fail-closed sub-test reddens.
func TestMajor5CompletionPermitGenerationFenceLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	wid := e.seedWorker(t, nil)
	wkr := store.Worker{ID: wid}
	g := int64(10)
	const (
		branch = "agent/issue-1"
		head   = "cafef00d"
	)

	permitCount := func(runID uuid.UUID) int {
		t.Helper()
		var n int
		if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM run_completion_permits WHERE run_id = $1`, runID).Scan(&n); err != nil {
			t.Fatalf("count permits: %v", err)
		}
		return n
	}

	// (a) STALE by a SUPERSEDING reclaim.
	superseded := e.seedFrozenRun(t, wid, []string{"m1"}, []string{"m1"}, false)
	e.exec(t, `UPDATE runs SET claim_generation = $2 WHERE id = $1`, superseded, g+1)
	old := g
	if _, err := svc.RequestCompletionPermit(e.ctx, wkr, superseded, CompletionPermitRequest{ContractRevision: 1, Branch: branch, Head: head, ClaimGeneration: &old}); !errors.Is(err, ErrCompletionStaleClaim) {
		t.Fatalf("stale (superseded) permit: err = %v, want ErrCompletionStaleClaim", err)
	}
	if n := permitCount(superseded); n != 0 {
		t.Fatalf("a STALE (superseded-generation) permit request must issue no permit; got %d rows", n)
	}

	// (b) STALE by a RELEASED claim (matching generation, claim_released_at set).
	released := e.seedFrozenRun(t, wid, []string{"m1"}, []string{"m1"}, false)
	e.exec(t, `UPDATE runs SET claim_generation = $2, claim_released_at = now() WHERE id = $1`, released, g)
	gg := g
	if _, err := svc.RequestCompletionPermit(e.ctx, wkr, released, CompletionPermitRequest{ContractRevision: 1, Branch: branch, Head: head, ClaimGeneration: &gg}); !errors.Is(err, ErrCompletionStaleClaim) {
		t.Fatalf("stale (released) permit: err = %v, want ErrCompletionStaleClaim", err)
	}
	if n := permitCount(released); n != 0 {
		t.Fatalf("a permit request on a RELEASED claim must issue no permit; got %d rows", n)
	}

	// (c) CURRENT generation, unreleased claim: the permit is granted.
	current := e.seedFrozenRun(t, wid, []string{"m1"}, []string{"m1"}, false)
	e.exec(t, `UPDATE runs SET claim_generation = $2 WHERE id = $1`, current, g)
	gcur := g
	res, err := svc.RequestCompletionPermit(e.ctx, wkr, current, CompletionPermitRequest{ContractRevision: 1, Branch: branch, Head: head, ClaimGeneration: &gcur})
	if err != nil {
		t.Fatalf("current permit: %v", err)
	}
	if !res.Granted || res.Permit == nil {
		t.Fatalf("a CURRENT-generation permit request must grant: %+v", res)
	}
	if n := permitCount(current); n != 1 {
		t.Fatalf("a granted permit must write exactly one row; got %d", n)
	}

	// (d) FAIL-CLOSED: a capability worker that OMITS the generation is refused.
	capWkr := store.Worker{ID: wid, ProtocolCapabilities: []string{capability.CredentialSwitchV1}}
	failClosed := e.seedFrozenRun(t, wid, []string{"m1"}, []string{"m1"}, false)
	e.exec(t, `UPDATE runs SET claim_generation = $2 WHERE id = $1`, failClosed, g)
	if _, err := svc.RequestCompletionPermit(e.ctx, capWkr, failClosed, CompletionPermitRequest{ContractRevision: 1, Branch: branch, Head: head}); !errors.Is(err, ErrMissingClaimGeneration) {
		t.Fatalf("capability worker omitting generation: err = %v, want ErrMissingClaimGeneration", err)
	}
	if n := permitCount(failClosed); n != 0 {
		t.Fatalf("a fail-closed permit request must issue no permit; got %d rows", n)
	}
}
