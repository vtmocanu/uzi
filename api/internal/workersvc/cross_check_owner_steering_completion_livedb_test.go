package workersvc

import (
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #3841: the owner completion-continue is delivered as an answer by resumeCompletionBlocked and
// ContinueCompletionDecision runs it NON-transactionally on s.q, so the answer and the
// completion_decision audit row carry different timestamps. The shared question_id is the only link,
// and SnapshotOwnerSteering must exclude that answer while keeping ordinary owner answers.
func TestOwnerSteeringExcludesCompletionContinueAnswerLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	wid := e.seedWorker(t, []string{"completion_interlock_v1"})
	runID := e.seedFrozenRun(t, wid, []string{"m1"}, []string{"m1"}, false)
	const ordinary, completion = "q-ordinary", "completion-q-3841"
	// An ordinary owner answer given before the completion window opens.
	e.exec(t, `INSERT INTO run_user_inputs (run_id, kind, body, question_id)
		VALUES ($1, 'answer', '{"question_id":"q-ordinary","answers":["use postgres"]}', $2)`, runID, ordinary)
	e.exec(t, `UPDATE runs SET status = 'awaiting_input', completion_attempts = 2,
	               open_question_id = $2, completion_question_at = now() WHERE id = $1`, runID, completion)

	if _, err := svc.ContinueCompletionDecision(e.ctx, e.userID, runID, "finish the error path"); err != nil {
		t.Fatalf("ContinueCompletionDecision: %v", err)
	}

	var auditQID pgtype.Text
	var sameInstant bool
	if err := e.pool.QueryRow(e.ctx, `SELECT d.question_id, d.created_at = a.created_at
		FROM run_user_inputs d, run_user_inputs a
		WHERE d.run_id=$1 AND d.kind='completion_decision' AND a.run_id=$1 AND a.kind='answer' AND a.question_id=$2`,
		runID, completion).Scan(&auditQID, &sameInstant); err != nil {
		t.Fatalf("read audit row: %v", err)
	}
	if !auditQID.Valid || auditQID.String != completion {
		t.Fatalf("completion_decision audit row question_id = %+v, want %q", auditQID, completion)
	}
	if sameInstant {
		t.Log("answer and audit row share a timestamp here; the exclusion must not depend on it")
	}

	rows, err := e.q.SnapshotOwnerSteering(e.ctx, store.SnapshotOwnerSteeringParams{RunID: runID, MaxEntries: 32})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Total != 1 || rows[0].QuestionID.String != ordinary {
		t.Fatalf("snapshot rows=%d total=%d: want only the ordinary answer %q", len(rows), rows[0].Total, ordinary)
	}
}

// The paused branch writes no answer, so its completion_decision audit row keeps a NULL
// question_id and cannot exclude any answer.
func TestCompletionDecisionPausedAuditRowHasNoQuestionIDLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	wid := e.seedWorker(t, []string{"completion_interlock_v1"})
	runID := e.seedFrozenRun(t, wid, []string{"m1"}, []string{"m1"}, false)
	e.exec(t, `UPDATE runs SET status = 'paused', hold_reason = 'completion_blocked',
	               hold_captured_head = 'capturedhead1', completion_attempts = 1 WHERE id = $1`, runID)
	if _, err := svc.ContinueCompletionDecision(e.ctx, e.userID, runID, "go on"); err != nil {
		t.Fatal(err)
	}
	var q pgtype.Text
	if err := e.pool.QueryRow(e.ctx, `SELECT question_id FROM run_user_inputs WHERE run_id=$1 AND kind='completion_decision'`, runID).Scan(&q); err != nil {
		t.Fatal(err)
	}
	if q.Valid {
		t.Fatalf("paused-branch audit row carries question_id %q", q.String)
	}
}
