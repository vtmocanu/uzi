package workersvc

import (
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #1226 M5 (D7) LiveDB coverage of the owner completion-decision endpoint's service method
// (ContinueCompletionDecision) and the SetRunRunning hold-clear. These drive the real Service over
// a throwaway Postgres (skipped unless UZI_TEST_DATABASE_URL) reusing the interlockLiveDB harness
// (setup + seed helpers from completion_interlock_livedb_test.go).

// holdColumns reads the run's three hold annotations. Each *string is nil when the column is NULL.
func (e interlockLiveDB) holdColumns(t *testing.T, runID uuid.UUID) (reason, capturedHead *string, exhaustedSet bool) {
	t.Helper()
	var r, h pgtype.Text
	if err := e.pool.QueryRow(e.ctx,
		`SELECT hold_reason, hold_captured_head, completion_budget_exhausted_at IS NOT NULL FROM runs WHERE id = $1`, runID).
		Scan(&r, &h, &exhaustedSet); err != nil {
		t.Fatalf("read hold columns: %v", err)
	}
	if r.Valid {
		reason = &r.String
	}
	if h.Valid {
		capturedHead = &h.String
	}
	return reason, capturedHead, exhaustedSet
}

// countInputs returns how many run_user_inputs rows of the given kind exist for the run.
func (e interlockLiveDB) countInputs(t *testing.T, runID uuid.UUID, kind string) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(e.ctx,
		`SELECT count(*) FROM run_user_inputs WHERE run_id = $1 AND kind = $2`, runID, kind).Scan(&n); err != nil {
		t.Fatalf("count %s inputs: %v", kind, err)
	}
	return n
}

// pendingFollowUpBody returns the body of the run's single UNCONSUMED follow_up (the guidance the
// resumed claim/live worker will drain), failing if there is not exactly one.
func (e interlockLiveDB) pendingFollowUpBody(t *testing.T, runID uuid.UUID) string {
	t.Helper()
	var body pgtype.Text
	if err := e.pool.QueryRow(e.ctx,
		`SELECT body FROM run_user_inputs WHERE run_id = $1 AND kind = 'follow_up' AND consumed_at IS NULL`, runID).
		Scan(&body); err != nil {
		t.Fatalf("read pending follow_up: %v", err)
	}
	return body.String
}

// TestCompletionDecisionPausedResumesLiveDB (PRD #1226 M5, D7): a paused run held on the completion
// interlock (hold_reason='completion_blocked') accepts a continue decision — it resumes THROUGH
// queued via ResumePausedRun, the hold columns are STILL set (cleared only on the first running
// report, not prematurely), a completion_decision audit row and the guidance follow_up are
// enqueued. THEN a SetRunRunning report clears hold_reason/hold_captured_head/
// completion_budget_exhausted_at — the D7 "clears the hold on the first accepted running report".
func TestCompletionDecisionPausedResumesLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	wid := e.seedWorker(t, []string{"completion_interlock_v1"})
	runID := e.seedFrozenRun(t, wid, []string{"m1", "m2"}, []string{"m1"}, false)
	// Park it on the completion hold exactly as SetRunCompletionHold would: paused,
	// hold_reason='completion_blocked', a captured head, an attempt recorded, and the one-shot
	// served-steer flag set (so the running-report clear is observable).
	e.exec(t, `UPDATE runs SET status = 'paused', hold_reason = 'completion_blocked',
	               hold_captured_head = 'capturedhead1', completion_attempts = 1,
	               completion_budget_exhausted_at = now() WHERE id = $1`, runID)

	const guidance = "also cover the error path before finishing"
	run, err := svc.ContinueCompletionDecision(e.ctx, e.userID, runID, guidance)
	if err != nil {
		t.Fatalf("ContinueCompletionDecision (paused): %v", err)
	}
	if run.Status != "queued" {
		t.Fatalf("a paused completion-blocked run must resume THROUGH queued; status = %q", run.Status)
	}

	// The hold columns are STILL set — D7 clears them on the first running report, NOT in the resume.
	reason, head, exhausted := e.holdColumns(t, runID)
	if reason == nil || *reason != "completion_blocked" {
		t.Fatalf("hold_reason must survive the resume (cleared only on running); got %v", reason)
	}
	if head == nil || *head != "capturedhead1" {
		t.Fatalf("hold_captured_head must survive the resume; got %v", head)
	}
	if !exhausted {
		t.Fatal("completion_budget_exhausted_at must survive the resume (cleared only on running)")
	}

	// Exactly one completion_decision audit row, and the guidance follow_up enqueued unconsumed.
	if got := e.countInputs(t, runID, "completion_decision"); got != 1 {
		t.Fatalf("completion_decision audit rows = %d, want 1", got)
	}
	if got := e.countInputs(t, runID, "follow_up"); got != 1 {
		t.Fatalf("guidance follow_up rows = %d, want 1", got)
	}
	if body := e.pendingFollowUpBody(t, runID); body != guidance {
		t.Fatalf("guidance follow_up body = %q, want %q", body, guidance)
	}

	// The first accepted running report clears the hold. Simulate the post-resume claim
	// (queued → claimed by the affinity worker) then the running report.
	e.exec(t, `UPDATE runs SET status = 'claimed' WHERE id = $1`, runID)
	rows, err := e.q.SetRunRunning(e.ctx, store.SetRunRunningParams{
		IterationCount:           1,
		RunMaxIterations:         5,
		MilestoneBudgetCap:       milestoneBudgetCap,
		RunTimeoutSeconds:        7200,
		BudgetWallCeilingSeconds: budgetWallCeilingSeconds,
		ID:                       runID,
		WorkerID:                 pgtype.UUID{Bytes: wid, Valid: true},
	})
	if err != nil {
		t.Fatalf("SetRunRunning (resume report): %v", err)
	}
	if rows != 1 {
		t.Fatalf("SetRunRunning affected %d rows, want 1", rows)
	}
	if s := e.runStatus(t, runID); s != "running" {
		t.Fatalf("run status after the running report = %q, want running", s)
	}
	reason, head, exhausted = e.holdColumns(t, runID)
	if reason != nil {
		t.Fatalf("hold_reason must be CLEARED on the first running report; got %q", *reason)
	}
	if head != nil {
		t.Fatalf("hold_captured_head must be CLEARED on the first running report; got %q", *head)
	}
	if exhausted {
		t.Fatal("completion_budget_exhausted_at must be CLEARED on the first running report")
	}
}

// TestCompletionDecisionAwaitingInputResumesInPlaceLiveDB (PRD #1226 M5, D7): a run in the LIVE
// completion-question window (awaiting_input, interlocked, with a recorded attempt) accepts a
// continue decision that writes the worker-consumed guidance follow_up + the completion_decision
// audit row and does NOT force-transition the status — the live worker reports running itself.
func TestCompletionDecisionAwaitingInputResumesInPlaceLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	wid := e.seedWorker(t, []string{"completion_interlock_v1"})
	runID := e.seedFrozenRun(t, wid, []string{"m1"}, []string{"m1"}, false)
	e.exec(t, `UPDATE runs SET status = 'awaiting_input', completion_attempts = 2 WHERE id = $1`, runID)

	const guidance = "resolve the open TODO in handler.go"
	run, err := svc.ContinueCompletionDecision(e.ctx, e.userID, runID, guidance)
	if err != nil {
		t.Fatalf("ContinueCompletionDecision (awaiting_input): %v", err)
	}
	if run.Status != "awaiting_input" {
		t.Fatalf("the live window must NOT be force-transitioned; status = %q, want awaiting_input", run.Status)
	}
	if got := e.countInputs(t, runID, "completion_decision"); got != 1 {
		t.Fatalf("completion_decision audit rows = %d, want 1", got)
	}
	if got := e.countInputs(t, runID, "follow_up"); got != 1 {
		t.Fatalf("worker-consumed follow_up rows = %d, want 1", got)
	}
	if body := e.pendingFollowUpBody(t, runID); body != guidance {
		t.Fatalf("follow_up body = %q, want %q", body, guidance)
	}
}

// TestCompletionDecisionNotBlockedRefusedLiveDB (PRD #1226 M5, D7): a run in NEITHER
// completion-blocked state is refused with ErrCompletionNotBlocked, and no rows are written. Three
// shapes: a plain running interlocked run with no attempt, a paused run held for a DIFFERENT reason
// (owner pause, hold_reason NULL), and an awaiting_input run that is not in the completion window
// (no recorded attempt — an ordinary clarification question).
func TestCompletionDecisionNotBlockedRefusedLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	wid := e.seedWorker(t, []string{"completion_interlock_v1"})

	assertRefused := func(t *testing.T, runID uuid.UUID) {
		t.Helper()
		_, err := svc.ContinueCompletionDecision(e.ctx, e.userID, runID, "guidance")
		if !errors.Is(err, ErrCompletionNotBlocked) {
			t.Fatalf("want ErrCompletionNotBlocked; got %v", err)
		}
		if got := e.countInputs(t, runID, "completion_decision"); got != 0 {
			t.Fatalf("a refused decision must write no completion_decision row; got %d", got)
		}
		if got := e.countInputs(t, runID, "follow_up"); got != 0 {
			t.Fatalf("a refused decision must write no follow_up; got %d", got)
		}
	}

	t.Run("plain running interlocked run, no attempt", func(t *testing.T) {
		runID := e.seedFrozenRun(t, wid, []string{"m1"}, []string{"m1"}, false) // running, attempts=0
		assertRefused(t, runID)
		if s := e.runStatus(t, runID); s != "running" {
			t.Fatalf("a refused decision must leave status unchanged; got %q", s)
		}
	})

	t.Run("paused for a different reason (owner pause)", func(t *testing.T) {
		runID := e.seedFrozenRun(t, wid, []string{"m1"}, []string{"m1"}, false)
		// paused with NO completion hold reason — an owner pause, not a completion hold.
		e.exec(t, `UPDATE runs SET status = 'paused' WHERE id = $1`, runID)
		assertRefused(t, runID)
		if s := e.runStatus(t, runID); s != "paused" {
			t.Fatalf("a refused decision must leave status unchanged; got %q", s)
		}
	})

	t.Run("awaiting_input with no completion attempt (ordinary question)", func(t *testing.T) {
		runID := e.seedFrozenRun(t, wid, []string{"m1"}, []string{"m1"}, false)
		e.exec(t, `UPDATE runs SET status = 'awaiting_input' WHERE id = $1`, runID) // attempts=0
		assertRefused(t, runID)
		if s := e.runStatus(t, runID); s != "awaiting_input" {
			t.Fatalf("a refused decision must leave status unchanged; got %q", s)
		}
	})
}
