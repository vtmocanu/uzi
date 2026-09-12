package workersvc

import (
	"encoding/json"
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

// pendingAnswer returns the body + question_id column of the run's single UNCONSUMED `answer`
// (the delivery the live worker's steering.awaitAnswer resolves on), failing if there is not
// exactly one.
func (e interlockLiveDB) pendingAnswer(t *testing.T, runID uuid.UUID) (body, questionID string) {
	t.Helper()
	var b, q pgtype.Text
	if err := e.pool.QueryRow(e.ctx,
		`SELECT body, question_id FROM run_user_inputs WHERE run_id = $1 AND kind = 'answer' AND consumed_at IS NULL`, runID).
		Scan(&b, &q); err != nil {
		t.Fatalf("read pending answer: %v", err)
	}
	return b.String, q.String
}

// exhaustedSet reports whether completion_budget_exhausted_at is currently set on the run.
func (e interlockLiveDB) exhaustedSet(t *testing.T, runID uuid.UUID) bool {
	t.Helper()
	var set bool
	if err := e.pool.QueryRow(e.ctx,
		`SELECT completion_budget_exhausted_at IS NOT NULL FROM runs WHERE id = $1`, runID).Scan(&set); err != nil {
		t.Fatalf("read completion_budget_exhausted_at: %v", err)
	}
	return set
}

// completionMarkerSet reports whether the PRD #1226 M5 completion-question marker
// (completion_question_at) is currently set on the run.
func (e interlockLiveDB) completionMarkerSet(t *testing.T, runID uuid.UUID) bool {
	t.Helper()
	var set bool
	if err := e.pool.QueryRow(e.ctx,
		`SELECT completion_question_at IS NOT NULL FROM runs WHERE id = $1`, runID).Scan(&set); err != nil {
		t.Fatalf("read completion_question_at: %v", err)
	}
	return set
}

// TestCompletionDecisionPausedResumesLiveDB (PRD #1226 M5, D7/D3): a paused run held on the
// completion interlock (hold_reason='completion_blocked') accepts a continue decision — it resumes
// THROUGH queued via ResumePausedRun, the hold columns are STILL set (cleared only on the first
// running report, not prematurely), and a completion_decision audit row plus the guidance follow_up
// are enqueued. The owner decision CLEARS completion_budget_exhausted_at (D3: "a new owner decision
// clears it"); on the paused branch a real completion hold carries the flag NULL already
// (SetRunCompletionHold cleared it on hold entry), so this test seeds it SET to make the decision's
// clear observable. THEN a SetRunRunning report clears hold_reason/hold_captured_head — the D7
// "clears the hold on the first accepted running report".
func TestCompletionDecisionPausedResumesLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	wid := e.seedWorker(t, []string{"completion_interlock_v1"})
	runID := e.seedFrozenRun(t, wid, []string{"m1", "m2"}, []string{"m1"}, false)
	// Park it on the completion hold: paused, hold_reason='completion_blocked', a captured head, and
	// an attempt recorded. completion_budget_exhausted_at is set here (SET, not NULL) so the owner
	// decision's D3 clear is observable — in production SetRunCompletionHold already NULLs it on hold
	// entry, so a real paused completion hold carries it NULL and the clear is a no-op there.
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
	// completion_budget_exhausted_at is CLEARED by the owner decision (D3).
	reason, head, exhausted := e.holdColumns(t, runID)
	if reason == nil || *reason != "completion_blocked" {
		t.Fatalf("hold_reason must survive the resume (cleared only on running); got %v", reason)
	}
	if head == nil || *head != "capturedhead1" {
		t.Fatalf("hold_captured_head must survive the resume; got %v", head)
	}
	if exhausted {
		t.Fatal("completion_budget_exhausted_at must be CLEARED by the owner CONTINUE decision (D3: a new " +
			"owner decision clears the served budget_exhausted steer)")
	}

	// Exactly one completion_decision audit row, and the guidance follow_up enqueued unconsumed.
	// The paused branch delivers guidance as a follow_up (the resumed claim's pullFollowUp drains
	// it) — NOT an answer, which is the live-window delivery.
	if got := e.countInputs(t, runID, "completion_decision"); got != 1 {
		t.Fatalf("completion_decision audit rows = %d, want 1", got)
	}
	if got := e.countInputs(t, runID, "follow_up"); got != 1 {
		t.Fatalf("guidance follow_up rows = %d, want 1", got)
	}
	if got := e.countInputs(t, runID, "answer"); got != 0 {
		t.Fatalf("the paused branch must NOT write an answer (no live await); answer rows = %d, want 0", got)
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
	reason, head, _ = e.holdColumns(t, runID)
	if reason != nil {
		t.Fatalf("hold_reason must be CLEARED on the first running report; got %q", *reason)
	}
	if head != nil {
		t.Fatalf("hold_captured_head must be CLEARED on the first running report; got %q", *head)
	}
}

// TestCompletionDecisionAwaitingInputResumesInPlaceLiveDB (PRD #1226 M5, D7/D3): a run in the LIVE
// completion-question window (awaiting_input, interlocked, with a recorded attempt AND an
// open_question_id) accepts a continue decision. The delivery is an ANSWER — NOT a follow_up —
// because the worker's completion-question await is steering.awaitAnswer(questionId), which route()
// resolves ONLY on an `answer`-kind input (parseAnswerBody requires {question_id, answers}); a
// follow_up would never resolve it. The answer names the run's OWN open_question_id and carries the
// guidance as its single answer. The completion_decision audit row is still written, the served
// budget_exhausted steer is CLEARED (D3), and the status is NOT force-transitioned — the live
// worker reports running itself.
func TestCompletionDecisionAwaitingInputResumesInPlaceLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	wid := e.seedWorker(t, []string{"completion_interlock_v1"})
	runID := e.seedFrozenRun(t, wid, []string{"m1"}, []string{"m1"}, false)
	const qid = "completion-q-abc123"
	// Seed the live window: awaiting_input, a recorded attempt, the completion question's open id,
	// the DEDICATED completion-question marker SET (completionQuestionOpen keys on it), and the
	// served steer flag SET so the D3 clear is observable.
	e.exec(t, `UPDATE runs SET status = 'awaiting_input', completion_attempts = 2,
	               open_question_id = $2, completion_question_at = now(),
	               completion_budget_exhausted_at = now() WHERE id = $1`, runID, qid)

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
	// The delivery is an ANSWER, not a follow_up: a follow_up would never resolve awaitAnswer.
	if got := e.countInputs(t, runID, "follow_up"); got != 0 {
		t.Fatalf("the live window must NOT write a follow_up (it can't resolve awaitAnswer); follow_up rows = %d, want 0", got)
	}
	if got := e.countInputs(t, runID, "answer"); got != 1 {
		t.Fatalf("worker-consumed answer rows = %d, want 1", got)
	}
	body, questionID := e.pendingAnswer(t, runID)
	if questionID != qid {
		t.Fatalf("answer question_id column = %q, want the run's open_question_id %q", questionID, qid)
	}
	var ab struct {
		QuestionID string   `json:"question_id"`
		Answers    []string `json:"answers"`
	}
	if err := json.Unmarshal([]byte(body), &ab); err != nil {
		t.Fatalf("answer body %q is not the {question_id, answers} wire shape parseAnswerBody expects: %v", body, err)
	}
	if ab.QuestionID != qid {
		t.Fatalf("answer body question_id = %q, want %q", ab.QuestionID, qid)
	}
	if len(ab.Answers) != 1 || ab.Answers[0] != guidance {
		t.Fatalf("answer body answers = %v, want [%q]", ab.Answers, guidance)
	}
	// D3: the owner decision cleared the served budget_exhausted steer, so a resumed worker is not
	// immediately re-steered into the hold off a since-consumed ACK.
	if e.exhaustedSet(t, runID) {
		t.Fatal("completion_budget_exhausted_at must be CLEARED by the owner CONTINUE decision (D3)")
	}
}

// TestCompletionDecisionAwaitingInputEmptyGuidanceSendsContinueSentinelLiveDB (PRD #1226 M5, D7):
// an EMPTY guidance in the live window still delivers an answer, with a non-empty continue sentinel
// (["continue"]) so steering.awaitAnswer resolves and the worker gets an unambiguous continue.
func TestCompletionDecisionAwaitingInputEmptyGuidanceSendsContinueSentinelLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	wid := e.seedWorker(t, []string{"completion_interlock_v1"})
	runID := e.seedFrozenRun(t, wid, []string{"m1"}, []string{"m1"}, false)
	const qid = "completion-q-empty"
	e.exec(t, `UPDATE runs SET status = 'awaiting_input', completion_attempts = 1, open_question_id = $2,
	               completion_question_at = now() WHERE id = $1`, runID, qid)

	if _, err := svc.ContinueCompletionDecision(e.ctx, e.userID, runID, ""); err != nil {
		t.Fatalf("ContinueCompletionDecision (awaiting_input, empty guidance): %v", err)
	}
	if got := e.countInputs(t, runID, "answer"); got != 1 {
		t.Fatalf("empty guidance must still write exactly one answer; answer rows = %d, want 1", got)
	}
	body, questionID := e.pendingAnswer(t, runID)
	if questionID != qid {
		t.Fatalf("answer question_id column = %q, want %q", questionID, qid)
	}
	var ab struct {
		QuestionID string   `json:"question_id"`
		Answers    []string `json:"answers"`
	}
	if err := json.Unmarshal([]byte(body), &ab); err != nil {
		t.Fatalf("answer body %q is not valid JSON: %v", body, err)
	}
	if len(ab.Answers) != 1 || ab.Answers[0] != "continue" {
		t.Fatalf("empty guidance must send the continue sentinel; answers = %v, want [continue]", ab.Answers)
	}
}

// TestCompletionDecisionNotBlockedRefusedLiveDB (PRD #1226 M5, D7): a run in NEITHER
// completion-blocked state is refused with ErrCompletionNotBlocked, and no rows are written. Four
// shapes: a plain running interlocked run with no attempt, a paused run held for a DIFFERENT reason
// (owner pause, hold_reason NULL), an awaiting_input run with NO completion-question marker (an
// ordinary clarification), and a marker-set awaiting_input run missing its open_question_id.
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
		if got := e.countInputs(t, runID, "answer"); got != 0 {
			t.Fatalf("a refused decision must write no answer; got %d", got)
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

	t.Run("awaiting_input with no completion marker (ordinary question)", func(t *testing.T) {
		// No completion-question marker (completion_question_at NULL) — an ordinary clarification,
		// which completionQuestionOpen no longer admits regardless of the attempt count.
		runID := e.seedFrozenRun(t, wid, []string{"m1"}, []string{"m1"}, false)
		e.exec(t, `UPDATE runs SET status = 'awaiting_input' WHERE id = $1`, runID) // marker NULL, attempts=0
		assertRefused(t, runID)
		if s := e.runStatus(t, runID); s != "awaiting_input" {
			t.Fatalf("a refused decision must leave status unchanged; got %q", s)
		}
	})

	t.Run("awaiting_input completion window (marker set) but no open_question_id", func(t *testing.T) {
		// The completion-question marker is SET (so completionQuestionOpen admits it) but there is NO
		// open_question_id — the answer could never resolve the worker's await, so it is refused.
		runID := e.seedFrozenRun(t, wid, []string{"m1"}, []string{"m1"}, false)
		e.exec(t, `UPDATE runs SET status = 'awaiting_input', completion_attempts = 2,
		               completion_question_at = now(), open_question_id = NULL WHERE id = $1`, runID)
		assertRefused(t, runID)
		if s := e.runStatus(t, runID); s != "awaiting_input" {
			t.Fatalf("a refused decision must leave status unchanged; got %q", s)
		}
	})
}

// TestCompletionDecisionOrdinaryClarificationRefusedLiveDB (PRD #1226 M5) is the wrong-question-
// refusal test the dedicated completion-question marker fixes. A run that is INTERLOCKED, past a
// completion attempt (completion_attempts > 0) AND carries an open_question_id, but parked on an
// ORDINARY PRD #88 ask_user clarification (completion_question_at NULL), used to match the old
// interlock+attempts proxy — so an owner completion-continue would write an `answer` naming the
// ordinary clarification's open_question_id and WRONGLY resolve it (a state mutation on the wrong
// question). Keyed on the marker, completionQuestionOpen no longer admits it, so the decision is
// REFUSED with ErrCompletionNotBlocked and NO answer/follow_up/completion_decision row is written.
// This is the mutation-check target: neuter the marker (make completionQuestionOpen ignore
// completion_question_at, or stop stamping it) and this test reddens (the decision is wrongly
// accepted and an answer is written).
func TestCompletionDecisionOrdinaryClarificationRefusedLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	wid := e.seedWorker(t, []string{"completion_interlock_v1"})
	runID := e.seedFrozenRun(t, wid, []string{"m1"}, []string{"m1"}, false)
	// Interlocked, past an attempt, WITH an open_question_id — but NO completion-question marker:
	// an ordinary clarification. The old proxy (interlock && attempts>0) would have accepted this.
	e.exec(t, `UPDATE runs SET status = 'awaiting_input', completion_attempts = 2,
	               open_question_id = 'ordinary-clarification-q' WHERE id = $1`, runID)

	if _, err := svc.ContinueCompletionDecision(e.ctx, e.userID, runID, "guidance for the wrong question"); !errors.Is(err, ErrCompletionNotBlocked) {
		t.Fatalf("an ordinary clarification (marker NULL) must be REFUSED with ErrCompletionNotBlocked; got %v", err)
	}
	if got := e.countInputs(t, runID, "answer"); got != 0 {
		t.Fatalf("the refused decision must NOT write an answer (that would resolve the wrong question); answer rows = %d, want 0", got)
	}
	if got := e.countInputs(t, runID, "completion_decision"); got != 0 {
		t.Fatalf("a refused decision must write no completion_decision row; got %d", got)
	}
	if got := e.countInputs(t, runID, "follow_up"); got != 0 {
		t.Fatalf("a refused decision must write no follow_up; got %d", got)
	}
	if s := e.runStatus(t, runID); s != "awaiting_input" {
		t.Fatalf("a refused decision must leave status unchanged; got %q", s)
	}
}

// TestCompletionDecisionForeignUserRefusedLiveDB (PRD #1226 M5) pins the user_id authorization
// predicate: ContinueCompletionDecision reads the run owner-scoped via GetRun FIRST, so a user who
// is NOT the run's owner is refused with ErrRunNotFound BEFORE any answer/follow_up/completion_decision
// write. Seed a completion-BLOCKED (paused, hold_reason='completion_blocked') run owned by e.userID,
// then call the decision as a DIFFERENT user id: it errors, the run stays paused, and no input rows
// are written. Mutation-check: drop the user_id scope in GetRun and the foreign user resolves the
// run, the decision is accepted, and this test reddens.
func TestCompletionDecisionForeignUserRefusedLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	wid := e.seedWorker(t, []string{"completion_interlock_v1"})
	runID := e.seedFrozenRun(t, wid, []string{"m1", "m2"}, []string{"m1"}, false)
	// Park it on the completion hold owned by e.userID, mirroring TestCompletionDecisionPausedResumesLiveDB.
	e.exec(t, `UPDATE runs SET status = 'paused', hold_reason = 'completion_blocked',
	               hold_captured_head = 'capturedhead1', completion_attempts = 1 WHERE id = $1`, runID)

	foreignUserID := uuid.New() // a different owner than e.userID
	if _, err := svc.ContinueCompletionDecision(e.ctx, foreignUserID, runID, "guidance"); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("a foreign user must be refused ErrRunNotFound by GetRun; got %v", err)
	}
	if s := e.runStatus(t, runID); s != "paused" {
		t.Fatalf("a refused foreign decision must leave status unchanged; got %q, want paused", s)
	}
	for _, kind := range []string{"completion_decision", "follow_up", "answer"} {
		if got := e.countInputs(t, runID, kind); got != 0 {
			t.Fatalf("a foreign-user decision must write no %s row; got %d", kind, got)
		}
	}
}

// TestSetRunAwaitingInputCompletionMarkerLiveDB (PRD #1226 M5) pins the SetState → SetRunAwaitingInput
// stamping: a report that flags a COMPLETION question (StateRequest.CompletionQuestion=true) stamps
// completion_question_at, while an ORDINARY ask_user report (the flag absent/false) leaves it NULL —
// byte-identical to the pre-marker park.
func TestSetRunAwaitingInputCompletionMarkerLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	wid := e.seedWorker(t, []string{"completion_interlock_v1"})
	wkr := store.Worker{ID: wid}

	completionRun := e.seedFrozenRun(t, wid, []string{"m1"}, []string{"m1"}, false)
	if _, applied, err := svc.SetState(e.ctx, wkr, completionRun, StateRequest{
		State:              "awaiting_input",
		OpenQuestionID:     strPtr("completion-q-1"),
		CompletionQuestion: true,
	}); err != nil || !applied {
		t.Fatalf("SetState(awaiting_input, completion): applied=%v err=%v", applied, err)
	}
	if !e.completionMarkerSet(t, completionRun) {
		t.Fatal("a completion-question report (CompletionQuestion=true) must stamp completion_question_at")
	}

	ordinaryRun := e.seedFrozenRun(t, wid, []string{"m1"}, []string{"m1"}, false)
	if _, applied, err := svc.SetState(e.ctx, wkr, ordinaryRun, StateRequest{
		State:          "awaiting_input",
		OpenQuestionID: strPtr("ordinary-q-1"),
	}); err != nil || !applied {
		t.Fatalf("SetState(awaiting_input, ordinary): applied=%v err=%v", applied, err)
	}
	if e.completionMarkerSet(t, ordinaryRun) {
		t.Fatal("an ordinary ask_user report (CompletionQuestion absent) must leave completion_question_at NULL")
	}
}

// TestSetRunRunningClearsCompletionMarkerLiveDB (PRD #1226 M5): the completion-question marker is
// RESOLVED when the run resumes to running (the owner's continue was accepted and the worker picked
// up the answer), so SetRunRunning clears it — the "no setter leaves a resolved park marker behind"
// convention. Seeds the live-window resume: awaiting_input with the marker + open_question_id and a
// CONSUMED answer naming that id (which satisfies SetRunRunning's awaiting_input→running guard), then
// the running report clears the marker.
func TestSetRunRunningClearsCompletionMarkerLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	wid := e.seedWorker(t, []string{"completion_interlock_v1"})
	runID := e.seedFrozenRun(t, wid, []string{"m1"}, []string{"m1"}, false)
	const qid = "completion-q-resume"
	e.exec(t, `UPDATE runs SET status = 'awaiting_input', completion_attempts = 1,
	               open_question_id = $2, completion_question_at = now() WHERE id = $1`, runID, qid)
	// A consumed answer naming the open question — the awaiting_input→running guard requires it.
	e.exec(t, `INSERT INTO run_user_inputs (run_id, kind, body, question_id, consumed_at)
	           VALUES ($1, 'answer', '{"question_id":"`+qid+`","answers":["continue"]}', $2, now())`,
		runID, qid)
	if !e.completionMarkerSet(t, runID) {
		t.Fatal("precondition: the marker must be set before the running report")
	}

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
		t.Fatalf("SetRunRunning (live-window resume): %v", err)
	}
	if rows != 1 {
		t.Fatalf("SetRunRunning affected %d rows, want 1 (the answer guard must admit the resume)", rows)
	}
	if s := e.runStatus(t, runID); s != "running" {
		t.Fatalf("status after the running report = %q, want running", s)
	}
	if e.completionMarkerSet(t, runID) {
		t.Fatal("SetRunRunning must CLEAR completion_question_at on the first running report")
	}
}

// TestSetRunCompletionHoldClearsCompletionMarkerLiveDB (PRD #1226 M5): a run in the live
// completion-question window (awaiting_input, interlocked, past an attempt, marker set) that the
// worker decides to hold enters the completion hold — SetRunCompletionHold resolves the question and
// clears the marker alongside open_question_id, by the same convention.
func TestSetRunCompletionHoldClearsCompletionMarkerLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	wid := e.seedWorker(t, []string{"completion_interlock_v1"})
	runID := e.seedFrozenRun(t, wid, []string{"m1"}, []string{"m1"}, false)
	e.exec(t, `UPDATE runs SET status = 'awaiting_input', completion_attempts = 1,
	               open_question_id = 'completion-q-hold', completion_question_at = now() WHERE id = $1`, runID)
	if !e.completionMarkerSet(t, runID) {
		t.Fatal("precondition: the marker must be set before the hold")
	}

	held, err := e.q.SetRunCompletionHold(e.ctx, store.SetRunCompletionHoldParams{
		ID:               runID,
		WorkerID:         pgtype.UUID{Bytes: wid, Valid: true},
		HoldCapturedHead: pgtype.Text{String: "capturedhead-hold", Valid: true},
	})
	if err != nil {
		t.Fatalf("SetRunCompletionHold (awaiting_input source): %v", err)
	}
	if held.Status != "paused" {
		t.Fatalf("SetRunCompletionHold must park the run at paused; got %q", held.Status)
	}
	if held.CompletionQuestionAt.Valid {
		t.Fatal("SetRunCompletionHold must CLEAR completion_question_at on hold entry")
	}
	if e.completionMarkerSet(t, runID) {
		t.Fatal("completion_question_at must be NULL in the row after the hold")
	}
}
