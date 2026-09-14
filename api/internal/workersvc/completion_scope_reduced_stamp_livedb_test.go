package workersvc

import (
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #1227 M2 LiveDB coverage of the SERVER-DERIVED scope_reduced completion stamp (service.go's
// `completed` case). Runs against a real throwaway Postgres (skipped unless UZI_TEST_DATABASE_URL),
// reusing interlockLiveDB and the seedFrozenRun / permitService / parkCompletionBlocked / seedPermit
// helpers. A package that prints `ok` with PASS=0 is INVALID, not green.

// stopKind reads a run's stop_kind, returning ("", false) for a NULL column.
func (e interlockLiveDB) stopKind(t *testing.T, runID uuid.UUID) (string, bool) {
	t.Helper()
	var sk *string
	if err := e.pool.QueryRow(e.ctx, `SELECT stop_kind FROM runs WHERE id = $1`, runID).Scan(&sk); err != nil {
		t.Fatalf("read stop_kind: %v", err)
	}
	if sk == nil {
		return "", false
	}
	return *sk, true
}

// TestCompletionScopeReducedStampLiveDB: a run whose frozen contract carries an owner-deferred set
// (a `partial` decision revised it) lands stop_kind='scope_reduced' when it completes via the permit
// path. The stamp is SERVER-DERIVED from the contract — the worker sends no wire signal for it.
func TestCompletionScopeReducedStampLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	wid := e.seedWorker(t, []string{"completion_interlock_v1"})
	runID := e.seedFrozenRun(t, wid, []string{"m1", "m2"}, nil, false)
	e.parkCompletionBlocked(t, runID)
	e.seedPermit(t, runID, wid, 1, "oldhead")

	// The owner defers m2 (keeps m1 only): revision 2 with scope.out={m2}, the prior permit
	// invalidated, the run resumed through queued.
	if _, err := svc.DecideCompletion(e.ctx, e.userID, runID, CompletionDecisionInput{
		Decision: "partial", Keep: []string{"m1"}, Reason: "ship m1 only; defer m2", ContractRevision: 1,
	}); err != nil {
		t.Fatalf("DecideCompletion partial: %v", err)
	}

	// Stand in for the worker resuming the reduced-scope run and re-reporting running with the
	// in-scope milestone m1 declared complete. The stamp under test is exercised by SetState below;
	// this UPDATE only returns the run to the claimed/running state the resume+re-claim reaches.
	e.exec(t, `UPDATE runs SET status='running', worker_id=$2, milestones_completed=$3 WHERE id=$1`,
		runID, wid, idsJSON(t, "m1"))

	wkr := store.Worker{ID: wid}
	const head = "cafef00d"
	permit, err := svc.RequestCompletionPermit(e.ctx, wkr, runID,
		CompletionPermitRequest{ContractRevision: 2, Branch: "agent/issue-1", Head: head})
	if err != nil || !permit.Granted {
		t.Fatalf("permit at revision 2 must be granted (in-scope m1 done, m2 deferred): %+v err=%v", permit, err)
	}

	run, applied, err := svc.SetState(e.ctx, wkr, runID,
		StateRequest{State: "completed", Head: strPtr(head), Branch: strPtr("agent/issue-1")})
	if err != nil {
		t.Fatalf("SetState completed: %v", err)
	}
	if !applied || run.Status != "completed" {
		t.Fatalf("a permitted reduced-scope completion must terminate the run; applied=%v status=%q", applied, run.Status)
	}

	// THE assertion: a deferrals-bearing completion lands stop_kind='scope_reduced'.
	if sk, valid := e.stopKind(t, runID); !valid || sk != "scope_reduced" {
		t.Fatalf("stop_kind = (%q, valid=%v), want 'scope_reduced'", sk, valid)
	}
	if !run.StopKind.Valid || run.StopKind.String != "scope_reduced" {
		t.Fatalf("returned run.StopKind = %+v, want scope_reduced", run.StopKind)
	}
}

// TestCompletionNoDeferralStopKindNullLiveDB: a normal (revision-1, no scope.out) interlocked
// completion leaves stop_kind NULL — the byte-identical guarantee. This is the negative control the
// scope_reduced stamp must not fire on.
func TestCompletionNoDeferralStopKindNullLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	wid := e.seedWorker(t, []string{"completion_interlock_v1"})
	runID := e.seedFrozenRun(t, wid, []string{"m1"}, []string{"m1"}, false)
	wkr := store.Worker{ID: wid}
	const head = "beadfeed"

	permit, err := svc.RequestCompletionPermit(e.ctx, wkr, runID,
		CompletionPermitRequest{ContractRevision: 1, Branch: "agent/issue-1", Head: head})
	if err != nil || !permit.Granted {
		t.Fatalf("permit must be granted: %+v err=%v", permit, err)
	}
	run, applied, err := svc.SetState(e.ctx, wkr, runID,
		StateRequest{State: "completed", Head: strPtr(head), Branch: strPtr("agent/issue-1")})
	if err != nil {
		t.Fatalf("SetState completed: %v", err)
	}
	if !applied || run.Status != "completed" {
		t.Fatalf("completion must terminate; applied=%v status=%q", applied, run.Status)
	}
	if sk, valid := e.stopKind(t, runID); valid {
		t.Fatalf("a no-deferral completion must leave stop_kind NULL; got %q", sk)
	}
	if run.StopKind.Valid {
		t.Fatalf("returned run.StopKind must be NULL; got %+v", run.StopKind)
	}
}

// scopeDisposition reads the settled disposition of the run's single kind='scope' audit row,
// returning ("", false) for a NULL (still-pending) disposition. After one CreateScopeCeilingInput
// there is exactly one 'scope' row for the run (the partial decision writes a 'completion_decision'
// row, not a 'scope' one), so this is unambiguous.
func (e interlockLiveDB) scopeDisposition(t *testing.T, runID uuid.UUID) (string, bool) {
	t.Helper()
	var d *string
	if err := e.pool.QueryRow(e.ctx,
		`SELECT disposition FROM run_user_inputs WHERE run_id = $1 AND kind = 'scope'`, runID).Scan(&d); err != nil {
		t.Fatalf("read scope disposition: %v", err)
	}
	if d == nil {
		return "", false
	}
	return *d, true
}

// TestCompletionScopeReducedDeclinesScopeCeilingAuditLiveDB pins the PRD #1227 M2a guard on the
// #634 scope-ceiling SETTLE (service.go's `completed` case, the `&& scopeStopKind.String ==
// "scope_capped"` clause). A run carries BOTH a live #634 scope_ceiling directive (so
// owned.ScopeCeiling.Valid is true and a pending 'scope' audit row exists) AND an owner-deferred
// contract set (a `partial` decision, so contractHasDeferrals is true). It completes via the
// permit path WITHOUT req.ScopeCapped=true, so the stamp resolves to scope_reduced, NOT
// scope_capped. The #634 scope-ceiling audit row must then settle 'declined' — the scope_ceiling
// directive did not drive this finalize — never 'applied'.
//
// This closes the M2a coverage gap: with the guard reverted to a bare `if scopeStopKind.Valid`,
// a scope_reduced completion (scopeStopKind.Valid == true) would settle the #634 audit row
// 'applied', and the whole workersvc suite would still pass. The 'declined' assertion below is
// what reddens on that revert.
func TestCompletionScopeReducedDeclinesScopeCeilingAuditLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	wid := e.seedWorker(t, []string{"completion_interlock_v1"})
	runID := e.seedFrozenRun(t, wid, []string{"m1", "m2"}, nil, false)
	e.parkCompletionBlocked(t, runID)
	e.seedPermit(t, runID, wid, 1, "oldhead")

	// The owner defers m2 (keeps m1 only): revision 2 with scope.out={m2} — contractHasDeferrals
	// becomes true, so the completion below resolves to scope_reduced.
	if _, err := svc.DecideCompletion(e.ctx, e.userID, runID, CompletionDecisionInput{
		Decision: "partial", Keep: []string{"m1"}, Reason: "ship m1 only; defer m2", ContractRevision: 1,
	}); err != nil {
		t.Fatalf("DecideCompletion partial: %v", err)
	}

	// Stand in for the worker resuming the reduced-scope run with the in-scope m1 declared complete.
	e.exec(t, `UPDATE runs SET status='running', worker_id=$2, milestones_completed=$3 WHERE id=$1`,
		runID, wid, idsJSON(t, "m1"))

	// ALSO give the run a live #634 scope_ceiling directive. CreateScopeCeilingInput is the exact
	// atomic mechanism SubmitInput's scope path uses: it sets runs.scope_ceiling (so
	// owned.ScopeCeiling.Valid is true at completion) AND writes the pending kind='scope' audit row
	// the settle path resolves. The ceiling VALUE is immaterial to the guard — only .Valid matters —
	// and scope_ceiling does not gate the permit (computeUnmetCriteria ignores it), so the granted
	// completion below is unaffected.
	if _, err := e.q.CreateScopeCeilingInput(e.ctx, store.CreateScopeCeilingInputParams{
		RunID:        runID,
		Body:         pgconv.TextOrNull("1"),
		ScopeCeiling: pgInt4(1),
	}); err != nil {
		t.Fatalf("CreateScopeCeilingInput: %v", err)
	}
	// Precondition: the audit row is pending (NULL disposition) before completion, so a later
	// non-NULL disposition can only have been written by the completion's settle path.
	if disp, settled := e.scopeDisposition(t, runID); settled {
		t.Fatalf("scope audit row must be pending (NULL disposition) before completion; got %q", disp)
	}

	wkr := store.Worker{ID: wid}
	const head = "cafef00d"
	permit, err := svc.RequestCompletionPermit(e.ctx, wkr, runID,
		CompletionPermitRequest{ContractRevision: 2, Branch: "agent/issue-1", Head: head})
	if err != nil || !permit.Granted {
		t.Fatalf("permit at revision 2 must be granted (in-scope m1 done, m2 deferred): %+v err=%v", permit, err)
	}

	// Complete via the permit path WITHOUT ScopeCapped=true, so the stamp resolves to scope_reduced,
	// never scope_capped.
	run, applied, err := svc.SetState(e.ctx, wkr, runID,
		StateRequest{State: "completed", Head: strPtr(head), Branch: strPtr("agent/issue-1")})
	if err != nil {
		t.Fatalf("SetState completed: %v", err)
	}
	if !applied || run.Status != "completed" {
		t.Fatalf("a permitted reduced-scope completion must terminate the run; applied=%v status=%q", applied, run.Status)
	}

	// Assertion 1: the completion stamped stop_kind='scope_reduced' (NOT scope_capped) — the
	// precondition that puts scopeStopKind in the "reduced, not capped" state the guard discriminates.
	if sk, valid := e.stopKind(t, runID); !valid || sk != "scope_reduced" {
		t.Fatalf("stop_kind = (%q, valid=%v), want 'scope_reduced'", sk, valid)
	}

	// Assertion 2 (THE M2a guard): the #634 scope-ceiling audit row settled 'declined', NOT
	// 'applied'. Reverting the settle guard to a bare `if scopeStopKind.Valid` settles this
	// scope_reduced completion's #634 row 'applied' — reddening this assertion.
	disp, settled := e.scopeDisposition(t, runID)
	if !settled {
		t.Fatal("the #634 scope-ceiling audit row must be settled at completion, not left pending")
	}
	if disp != "declined" {
		t.Fatalf("scope-ceiling audit row disposition = %q, want 'declined' "+
			"(a scope_reduced completion must NOT settle the #634 row 'applied')", disp)
	}
}
