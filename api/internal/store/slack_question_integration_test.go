package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// PRD #88 M3 against a REAL Postgres. This is a gate, not a nicety: `sqlc generate`,
// `go build` and `go vet` all pass on statements the server rejects at prepare time,
// and both queries here are only knowable by executing them —
//
//   - GetLatestRunQuestion filters on a text kind and orders by seq DESC, so "the
//     newest question" is a claim about the SQL, not about the Go around it;
//   - SetSlackRunQuestion writes a column added by migration 00092, on a table whose
//     row the generated RETURNING * must still scan.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; the store-IT
// runner (e2e/run-store-it.sh) provides one.

// insertQuestionMessage appends a `question` run-message the way the worker does, at
// the given seq, and returns the question id it carries.
func insertQuestionMessage(ctx context.Context, t *testing.T, q *store.Queries, f *awaitingInputFixture, seq int32, questionID, text string) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"question_id": questionID,
		"generation":  seq,
		"questions":   []map[string]any{{"question": text, "header": "h"}},
	})
	if err != nil {
		t.Fatalf("marshal question payload: %v", err)
	}
	rows, err := q.InsertRunMessage(ctx, store.InsertRunMessageParams{
		RunID: f.runID, Seq: seq, Kind: "question", Agent: pgT("lead"), Payload: payload,
	})
	if err != nil || rows != 1 {
		t.Fatalf("insert question message seq=%d: rows=%d err=%v", seq, rows, err)
	}
	return questionID
}

// The notifier's read: the LATEST question, by seq — not the first, and never a plan
// or any other kind. Getting this wrong would post question 1 again when the run has
// moved on to question 2, and bind nothing correctly thereafter.
func TestGetLatestRunQuestionLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	f, done := setupAwaitingInput(ctx, t, dsn)
	defer done()

	// A run with no question yet must report no row, so the notifier waits rather than
	// posting a park it cannot explain.
	if _, err := f.q.GetLatestRunQuestion(ctx, f.runID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("a run with no question must report no row, got err=%v", err)
	}

	// A NON-question message must not satisfy the query: the kind filter is what makes
	// this "the question", and a run's feed is mostly other kinds.
	if rows, err := f.q.InsertRunMessage(ctx, store.InsertRunMessageParams{
		RunID: f.runID, Seq: 1, Kind: "text", Agent: pgT("lead"), Payload: []byte(`{"text":"hello"}`),
	}); err != nil || rows != 1 {
		t.Fatalf("insert text message: rows=%d err=%v", rows, err)
	}
	if _, err := f.q.GetLatestRunQuestion(ctx, f.runID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("a non-question message must not satisfy the query, got err=%v", err)
	}

	insertQuestionMessage(ctx, t, f.q, f, 2, "q-first", "which store?")
	insertQuestionMessage(ctx, t, f.q, f, 3, "q-second", "which cache?")
	// A later message of another kind must not displace the newest QUESTION.
	if rows, err := f.q.InsertRunMessage(ctx, store.InsertRunMessageParams{
		RunID: f.runID, Seq: 4, Kind: "status", Agent: pgT("lead"), Payload: []byte(`{"text":"parked"}`),
	}); err != nil || rows != 1 {
		t.Fatalf("insert status message: rows=%d err=%v", rows, err)
	}

	raw, err := f.q.GetLatestRunQuestion(ctx, f.runID)
	if err != nil {
		t.Fatalf("GetLatestRunQuestion: %v", err)
	}
	var got struct {
		QuestionID string `json:"question_id"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("payload must round-trip as JSON: %v (%s)", err, raw)
	}
	if got.QuestionID != "q-second" {
		t.Fatalf("want the NEWEST question by seq (q-second), got %q", got.QuestionID)
	}
}

// The anchor write the dedupe reads. Its whole job is to make a re-park on the SAME
// question a no-op, so what matters is that the value persists and is legible on a
// later read — and that a run with no anchor row updates nothing rather than erroring
// into a silent skip of the post.
func TestSetSlackRunQuestionLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	f, done := setupAwaitingInput(ctx, t, dsn)
	defer done()

	// No anchor yet: the UPDATE matches nothing and reports no row.
	if _, err := f.q.SetSlackRunQuestion(ctx, store.SetSlackRunQuestionParams{
		RunID: f.runID, QuestionID: pgT("q-1"),
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("no anchor row ⇒ no row updated, got err=%v", err)
	}

	anchor, err := f.q.UpsertSlackRunMessage(ctx, store.UpsertSlackRunMessageParams{
		RunID: f.runID, ChannelID: "D1", RootTs: "root1",
	})
	if err != nil {
		t.Fatalf("UpsertSlackRunMessage: %v", err)
	}
	if anchor.QuestionID.Valid {
		t.Fatalf("a fresh anchor carries no question yet: %+v", anchor.QuestionID)
	}

	if _, err := f.q.SetSlackRunQuestion(ctx, store.SetSlackRunQuestionParams{
		RunID: f.runID, QuestionID: pgT("q-1"),
	}); err != nil {
		t.Fatalf("SetSlackRunQuestion: %v", err)
	}
	reread, err := f.q.GetSlackRunMessage(ctx, f.runID)
	if err != nil {
		t.Fatalf("GetSlackRunMessage: %v", err)
	}
	if !reread.QuestionID.Valid || reread.QuestionID.String != "q-1" {
		t.Fatalf("the posted question must be legible on the anchor: %+v", reread.QuestionID)
	}
	// The gate anchor is a separate concern and must not be disturbed: a question
	// posted mid-run cannot be allowed to clear or advance an approval gate's state.
	if reread.GateTs.Valid || reread.GateState.Valid || reread.GateGeneration.Valid {
		t.Fatalf("recording a question must not touch the gate anchor: %+v", reread)
	}

	// Advancing to a second question overwrites, so the dedupe compares against the
	// question actually on screen rather than the first one ever posted.
	if _, err := f.q.SetSlackRunQuestion(ctx, store.SetSlackRunQuestionParams{
		RunID: f.runID, QuestionID: pgT("q-2"),
	}); err != nil {
		t.Fatalf("SetSlackRunQuestion (second): %v", err)
	}
	reread, err = f.q.GetSlackRunMessage(ctx, f.runID)
	if err != nil {
		t.Fatalf("GetSlackRunMessage (second): %v", err)
	}
	if reread.QuestionID.String != "q-2" {
		t.Fatalf("anchor must advance to the newest posted question, got %q", reread.QuestionID.String)
	}
}
