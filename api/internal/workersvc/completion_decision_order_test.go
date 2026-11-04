package workersvc

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #3841 — the write order inside resumeCompletionBlocked.
//
// On the live (answer) branch the completion_decision audit row carries the answer's question_id,
// and SnapshotOwnerSteering excludes an answer from owner steering only when such a row exists.
// The continue path is not transactional, so the audit row MUST be written before the answer or a
// concurrent snapshot can see the answer unmarked and freeze completion guidance as owner steering.

// orderStore records the writes resumeCompletionBlocked issues, in call order. It embeds Store so
// any other method panics rather than returning a zero value.
type orderStore struct {
	Store
	calls     []string
	decision  *store.CreateCompletionDecisionInputParams
	answer    *store.CreateRunAnswerInputParams
	runInputs []store.CreateRunInputParams
}

func (o *orderStore) CreateCompletionDecisionInput(_ context.Context, arg store.CreateCompletionDecisionInputParams) (store.RunUserInput, error) {
	o.calls = append(o.calls, "completion_decision")
	o.decision = &arg
	return store.RunUserInput{}, nil
}

func (o *orderStore) CreateRunAnswerInput(_ context.Context, arg store.CreateRunAnswerInputParams) (store.RunUserInput, error) {
	o.calls = append(o.calls, "answer")
	o.answer = &arg
	return store.RunUserInput{}, nil
}

func (o *orderStore) CreateRunInput(_ context.Context, arg store.CreateRunInputParams) (store.RunUserInput, error) {
	o.calls = append(o.calls, "run_input:"+arg.Kind)
	o.runInputs = append(o.runInputs, arg)
	return store.RunUserInput{}, nil
}

func (o *orderStore) ClearCompletionBudgetExhausted(context.Context, uuid.UUID) (int64, error) {
	return 0, nil
}

func (o *orderStore) ResumePausedRun(context.Context, store.ResumePausedRunParams) (store.ResumePausedRunRow, error) {
	return store.ResumePausedRunRow{}, nil
}

func TestResumeCompletionBlockedLiveWritesAuditRowBeforeAnswer(t *testing.T) {
	qid := uuid.NewString()
	run := store.Run{
		ID:                   uuid.New(),
		Status:               "awaiting_input",
		CompletionQuestionAt: pgtype.Timestamptz{Valid: true},
		OpenQuestionID:       pgtype.Text{String: qid, Valid: true},
	}
	fs := &orderStore{}

	if err := resumeCompletionBlocked(context.Background(), fs, run, uuid.New(), "go on", "go on", 0, pgtype.Timestamptz{}); err != nil {
		t.Fatalf("resumeCompletionBlocked: %v", err)
	}

	want := []string{"completion_decision", "answer"}
	if len(fs.calls) != len(want) || fs.calls[0] != want[0] || fs.calls[1] != want[1] {
		t.Fatalf("write order = %v, want %v (audit row first, so the answer is never visible unmarked)", fs.calls, want)
	}
	if fs.decision.QuestionID.String != qid || !fs.decision.QuestionID.Valid {
		t.Fatalf("audit row question_id = %+v, want %q", fs.decision.QuestionID, qid)
	}
	if fs.answer.QuestionID != fs.decision.QuestionID {
		t.Fatalf("answer question_id %+v differs from audit row %+v", fs.answer.QuestionID, fs.decision.QuestionID)
	}
}

// The paused branch has no live answer to mark: its audit row is a plain completion_decision
// input with no question_id, written through CreateRunInput after the optional follow_up.
func TestResumeCompletionBlockedPausedAuditRowHasNoQuestionID(t *testing.T) {
	run := store.Run{
		ID:         uuid.New(),
		Status:     "paused",
		HoldReason: pgtype.Text{String: "completion_blocked", Valid: true},
	}
	fs := &orderStore{}

	if err := resumeCompletionBlocked(context.Background(), fs, run, uuid.New(), "go on", "audit", 0, pgtype.Timestamptz{}); err != nil {
		t.Fatalf("resumeCompletionBlocked: %v", err)
	}

	want := []string{"run_input:follow_up", "run_input:completion_decision"}
	if len(fs.calls) != len(want) || fs.calls[0] != want[0] || fs.calls[1] != want[1] {
		t.Fatalf("writes = %v, want %v", fs.calls, want)
	}
	if fs.decision != nil || fs.answer != nil {
		t.Fatal("paused branch must not use the live answer/audit writers")
	}
	audit := fs.runInputs[1]
	if audit.Body.String != "audit" {
		t.Fatalf("audit body = %q, want %q", audit.Body.String, "audit")
	}
}
