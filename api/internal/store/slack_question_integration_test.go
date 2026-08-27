package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/store"
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

// The anchor write both the dedupe and the inbound binding read. It carries two
// values that are one fact — the question, and the ts of the card that delivered it —
// so this pins that BOTH persist together and are legible on a later read. The id
// alone would dedupe correctly and leave every reply unbindable; the ts alone would
// leave the notifier unable to tell a re-park from a new question.
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
		RunID: f.runID, QuestionID: pgT("q-1"), QuestionTs: pgT("1700000100.000200"),
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("no anchor row ⇒ no row updated, got err=%v", err)
	}

	anchor, err := f.q.UpsertSlackRunMessage(ctx, store.UpsertSlackRunMessageParams{
		RunID: f.runID, ChannelID: "D1", RootTs: "root1",
	})
	if err != nil {
		t.Fatalf("UpsertSlackRunMessage: %v", err)
	}
	if anchor.QuestionID.Valid || anchor.QuestionTs.Valid {
		t.Fatalf("a fresh anchor carries no question yet: %+v", anchor)
	}

	if _, err := f.q.SetSlackRunQuestion(ctx, store.SetSlackRunQuestionParams{
		RunID: f.runID, QuestionID: pgT("q-1"), QuestionTs: pgT("1700000100.000200"),
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
	if !reread.QuestionTs.Valid || reread.QuestionTs.String != "1700000100.000200" {
		t.Fatalf("the card's ts must persist beside it — it is what inbound replies are ordered against: %+v", reread.QuestionTs)
	}
	// The gate anchor is a separate concern and must not be disturbed: a question
	// posted mid-run cannot be allowed to clear or advance an approval gate's state.
	if reread.GateTs.Valid || reread.GateState.Valid || reread.GateGeneration.Valid {
		t.Fatalf("recording a question must not touch the gate anchor: %+v", reread)
	}

	// Advancing to a second question overwrites BOTH values, so the dedupe compares
	// against the question actually on screen and replies are ordered against the card
	// actually carrying it. A stale ts left behind would make every reply to question 2
	// look like it followed question 1's card.
	if _, err := f.q.SetSlackRunQuestion(ctx, store.SetSlackRunQuestionParams{
		RunID: f.runID, QuestionID: pgT("q-2"), QuestionTs: pgT("1700000200.000300"),
	}); err != nil {
		t.Fatalf("SetSlackRunQuestion (second): %v", err)
	}
	reread, err = f.q.GetSlackRunMessage(ctx, f.runID)
	if err != nil {
		t.Fatalf("GetSlackRunMessage (second): %v", err)
	}
	if reread.QuestionID.String != "q-2" || reread.QuestionTs.String != "1700000200.000300" {
		t.Fatalf("anchor must advance to the newest posted question AND its card: %+v", reread)
	}
}
