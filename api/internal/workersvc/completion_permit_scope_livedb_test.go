package workersvc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// A stale pool-level read would supply a different scope/revision. The permit
// service must derive both from the transaction-bound run, under its claim lock.
type completionPermitScopeSource struct {
	Store
	stale store.Run
	reads int
}

func (s *completionPermitScopeSource) GetRunOwnedByWorker(context.Context, store.GetRunOwnedByWorkerParams) (store.Run, error) {
	s.reads++
	return s.stale, nil
}

// A normal pause or clarification cannot authorize a contract revision, even
// when the requested partial/accept decision is otherwise valid.
func TestCompletionScopeDecisionOutsideCompletionHoldRefusedLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	wid := e.seedWorker(t, []string{"completion_interlock_v1"})

	for _, state := range []string{"plain_pause", "ordinary_clarification"} {
		t.Run(state, func(t *testing.T) {
			for _, decision := range []CompletionDecisionInput{
				{Decision: "partial", Keep: []string{"m1"}, Reason: "defer m2", ContractRevision: 1},
				{Decision: "accept", Criteria: []string{"m2.c1"}, Reason: "waive m2.c1", ContractRevision: 1},
			} {
				t.Run(decision.Decision, func(t *testing.T) {
					runID := e.seedFrozenRun(t, wid, []string{"m1", "m2"}, []string{"m1"}, false)
					if state == "plain_pause" {
						e.exec(t, `UPDATE runs SET status = 'paused', completion_attempts = 2 WHERE id = $1`, runID)
					} else {
						// An attempt and open question alone must not make this a completion question.
						e.exec(t, `UPDATE runs SET status = 'awaiting_input', completion_attempts = 2,
							open_question_id = 'ordinary-scope-question' WHERE id = $1`, runID)
					}
					// Compare all input-row contents, not just audit counts: refusal must
					// neither enqueue guidance nor consume or alter an existing row.
					e.exec(t, `INSERT INTO run_user_inputs (run_id, kind, body)
						VALUES ($1, 'follow_up', 'retain existing guidance')`, runID)
					snapshot := func() string {
						t.Helper()
						var got string
						if err := e.pool.QueryRow(e.ctx, `SELECT jsonb_build_object(
							'contract', r.completion_contract, 'revision', r.contract_revision,
							'frozen', r.milestones_frozen, 'status', r.status,
							'question', r.open_question_id, 'marker', r.completion_question_at,
							'inputs', (SELECT jsonb_agg(to_jsonb(i) ORDER BY i.id)
								FROM run_user_inputs i WHERE i.run_id = r.id))::text
							FROM runs r WHERE r.id = $1`, runID).Scan(&got); err != nil {
							t.Fatalf("snapshot scope and input rows: %v", err)
						}
						return got
					}
					before := snapshot()
					if _, err := svc.DecideCompletion(e.ctx, e.userID, runID, decision); !errors.Is(err, ErrCompletionNotBlocked) {
						t.Fatalf("%s outside completion hold: want ErrCompletionNotBlocked, got %v", decision.Decision, err)
					}
					if after := snapshot(); after != before {
						t.Fatalf("refused %s changed contract/revision/input rows or question state:\nbefore %s\nafter  %s",
							decision.Decision, before, after)
					}
				})
			}
		})
	}
}

// Milestone IDs in an ordinary owner answer remain prose, including after the
// worker consumes the answer and acknowledges running. Only a formal completion
// decision may defer or waive frozen work.
func TestCompletionPermitOrdinaryAnswerCannotReduceScopeLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	wid := e.seedWorker(t, []string{"completion_interlock_v1"})
	wkr := store.Worker{ID: wid}

	for _, tc := range []struct {
		name, prose string
	}{
		{"defer", "Defer m2; keep m1 and ship without m2."},
		{"waive", "Waive m2.c1 and accept m2 as delivered."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runID := e.seedFrozenRun(t, wid, []string{"m1", "m2"}, []string{"m1"}, false)
			const qid = "ordinary-answer-scope-question"
			e.exec(t, `UPDATE runs SET status = 'awaiting_input', completion_attempts = 1,
				open_question_id = $2 WHERE id = $1`, runID, qid)
			before, rev := e.readContract(t, runID)
			if rev == nil || *rev != 1 {
				t.Fatalf("initial contract revision = %v, want 1", rev)
			}
			body := answerBody(t, qid, tc.prose)
			if _, err := svc.SubmitInput(e.ctx, e.userID, runID, "answer", body, nil); err != nil {
				t.Fatalf("submit ordinary owner answer: %v", err)
			}
			consumed, err := svc.ConsumeInputs(e.ctx, wkr, runID)
			if err != nil {
				t.Fatalf("consume ordinary owner answer: %v", err)
			}
			if consumed.Receipts || len(consumed.Inputs) != 1 || consumed.Inputs[0].Kind != "answer" ||
				consumed.Inputs[0].Body == nil || *consumed.Inputs[0].Body != body {
				t.Fatalf("worker must consume the ordinary answer unchanged: %+v", consumed)
			}
			running, applied, err := svc.SetState(e.ctx, wkr, runID, StateRequest{State: "running"})
			if err != nil || !applied || running.Status != "running" {
				t.Fatalf("ordinary answer running ACK: run=%+v applied=%v err=%v", running, applied, err)
			}
			assertUnchanged := func() {
				t.Helper()
				after, revision := e.readContract(t, runID)
				if revision == nil || *revision != 1 || !bytes.Equal(after, before) {
					t.Fatalf("ordinary answer changed frozen contract: revision=%v before=%s after=%s", revision, before, after)
				}
				var c completionContract
				if err := json.Unmarshal(after, &c); err != nil {
					t.Fatalf("decode unchanged contract: %v", err)
				}
				if c.Revision != 1 || c.Scope != nil || len(c.Accepted) != 0 {
					t.Fatalf("ordinary answer must leave revision 1 scope nil and no waivers: %+v", c)
				}
			}
			assertUnchanged()
			res, err := svc.RequestCompletionPermit(e.ctx, wkr, runID,
				CompletionPermitRequest{ContractRevision: 1, Branch: "agent/issue-1", Head: "deadbeef"})
			if err != nil || res.Granted || res.Permit != nil || res.DenyReason != CompletionDenyMissingMilestones ||
				len(res.Unmet) != 1 || res.Unmet[0] != "m2" {
				t.Fatalf("ordinary answer must leave m2 unmet and deny missing_milestones: result=%+v err=%v", res, err)
			}
			assertUnchanged()
			if got := e.countInputs(t, runID, "completion_decision"); got != 0 {
				t.Fatalf("ordinary answer must create no completion decision audit: got %d", got)
			}
		})
	}
}

func TestCompletionPermitLiveOwnerPartialScopeLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	wid := e.seedWorker(t, []string{"completion_interlock_v1"})
	wkr := store.Worker{ID: wid}
	runID := e.seedFrozenRun(t, wid, []string{"m1", "m2", "m3", "m4"}, []string{"m1", "m3"}, false)
	ms := []Milestone{
		{ID: "m1", Title: "Core implementation"},
		{ID: "m2", Title: "Deferred reporting"},
		{ID: "m3", Title: "Required validation"},
		{ID: "m4", Title: "Deferred export"},
	}
	frozen := milestonesJSON(t, ms...)
	contract, err := buildCompletionContract(frozen)
	if err != nil {
		t.Fatalf("build titled contract: %v", err)
	}
	const questionID = "completion-permit-scope-question"
	e.exec(t, `UPDATE runs SET milestones_frozen = $2, completion_contract = $3,
		status = 'awaiting_input', completion_attempts = 1, open_question_id = $4,
		completion_question_at = now() WHERE id = $1`, runID, frozen, contract, questionID)

	const reason = "deliver core and validation; defer reporting and export"
	run, err := svc.DecideCompletion(e.ctx, e.userID, runID, CompletionDecisionInput{
		Decision: "partial", Keep: []string{"m1", "m3"}, Reason: reason, ContractRevision: 1,
	})
	if err != nil {
		t.Fatalf("formal owner partial decision: %v", err)
	}
	if run.ID != runID || run.Status != "awaiting_input" || !run.ContractRevision.Valid || run.ContractRevision.Int32 != 2 {
		t.Fatalf("owner decision must revise the same live run in place: %+v", run)
	}
	body, qid := e.pendingAnswer(t, runID)
	var answer AnswerBody
	if err := json.Unmarshal([]byte(body), &answer); err != nil {
		t.Fatalf("decode owner answer: %v", err)
	}
	if qid != questionID || answer.QuestionID != questionID || len(answer.Answers) != 1 || answer.Answers[0] != reason {
		t.Fatalf("owner decision answer = %s (question %q), want original question and reason", body, qid)
	}

	// The live worker consumes and applies the real answer before acknowledging running.
	consumed, err := svc.ConsumeInputs(e.ctx, wkr, runID)
	if err != nil {
		t.Fatalf("consume owner answer: %v", err)
	}
	if consumed.Receipts || len(consumed.Inputs) != 1 || consumed.Inputs[0].Kind != "answer" ||
		consumed.Inputs[0].Body == nil || *consumed.Inputs[0].Body != body {
		t.Fatalf("worker must consume the same owner answer: %+v", consumed)
	}

	running, applied, err := svc.SetState(e.ctx, wkr, runID, StateRequest{State: "running"})
	if err != nil || !applied || running.Status != "running" {
		t.Fatalf("worker running ACK: run=%+v applied=%v err=%v", running, applied, err)
	}
	if e.completionMarkerSet(t, runID) {
		t.Fatal("running ACK must clear the completion-question marker")
	}

	// Keep a stale revision-1 source available outside the transaction. It is
	// deliberately complete and has no owner decision, so it cannot supply this grant.
	stale := interlockedRun(wkr)
	stale.ID = runID
	stale.CompletionContract = contract
	stale.MilestonesFrozen = frozen
	stale.MilestonesCompleted = idsJSON(t, "m1", "m2", "m3", "m4")
	source := &completionPermitScopeSource{Store: e.q, stale: stale}
	permitSvc := New(source, newBox(t), testParams())
	permitSvc.SetTxBeginner(e.pool)
	permitSvc.SetBackground(func(func()) {})

	req := CompletionPermitRequest{ContractRevision: 1, Branch: "agent/issue-1", Head: "deadbeef"}
	old, err := permitSvc.RequestCompletionPermit(e.ctx, wkr, runID, req)
	if err != nil || old.Granted || old.Permit != nil || old.DenyReason != CompletionDenyRevisionDrift {
		t.Fatalf("old revision must be denied revision_drift: result=%+v err=%v", old, err)
	}
	req.ContractRevision = 2
	granted, err := permitSvc.RequestCompletionPermit(e.ctx, wkr, runID, req)
	if err != nil || !granted.Granted || granted.Permit == nil {
		t.Fatalf("revised scope must grant with retained milestones complete: result=%+v err=%v", granted, err)
	}
	if source.reads != 0 {
		t.Errorf("permit must use the locked transaction's run, got %d reads from stale outer store", source.reads)
	}
	assertCompletionPermitScopeJSON(t, granted, 2,
		`{"deferred":[{"milestone_id":"m2","title":"Deferred reporting","reason":"deliver core and validation; defer reporting and export"},{"milestone_id":"m4","title":"Deferred export","reason":"deliver core and validation; defer reporting and export"}]}`)
}
