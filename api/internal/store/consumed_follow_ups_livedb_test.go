package store_test

// Issue #1660: a worker rehydrates its operator constraints on every claim from the run's
// ALREADY-CONSUMED follow_up inputs (ListConsumedFollowUpInputsForRun), so a follow-up a
// previous claim consumed still reaches the subagents a later claim dispatches. Only a real
// Postgres can prove the filter: follow_up only, consumed only, this run only, oldest first.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; run via
// e2e/run-store-it.sh. A package that prints `ok` with PASS=0 is INVALID, not green.

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
)

func TestListConsumedFollowUpInputsForRunLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	f, done := setupFollowup(ctx, t, dsn)
	defer done()
	wkr := f.seedWorker(ctx, t, "online", pgtype.Int4{}, false)
	run := f.seedInteractiveTaskRun(ctx, t, "running", &wkr)
	other := f.seedInteractiveTaskRun(ctx, t, "running", &wkr)

	insert := `INSERT INTO run_user_inputs (run_id, kind, body, consumed_at, created_at)
	           VALUES ($1, $2, $3, CASE WHEN $4::boolean THEN now() END, now() + make_interval(secs => $5))`
	seed := []struct {
		kind     string
		body     string
		consumed bool
	}{
		{"follow_up", "screen strings only", true}, // claim 1's follow-up
		{"revise_plan", "not a constraint", true},  // other kinds are not constraints
		{"approve_plan", "{}", true},
		{"cancel", "", true},
		{"follow_up", "use port 5433", true},  // claim 2's follow-up
		{"follow_up", "still pending", false}, // delivered by the live drain, not here
	}
	for i, s := range seed {
		mustExec(ctx, t, f.pool, insert, run, s.kind, s.body, s.consumed, float64(i))
	}
	mustExec(ctx, t, f.pool, insert, other, "follow_up", "another run's follow-up", true, float64(0))

	rows, err := f.q.ListConsumedFollowUpInputsForRun(ctx, run)
	if err != nil {
		t.Fatalf("ListConsumedFollowUpInputsForRun: %v", err)
	}
	var got []string
	for _, r := range rows {
		got = append(got, r.Body.String)
	}
	want := []string{"screen strings only", "use port 5433"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("consumed follow-ups = %q, want %q (follow_up only, consumed only, this run only, oldest first)", got, want)
	}
	if rows[0].ID >= rows[1].ID {
		t.Fatalf("ids %d, %d: want ascending creation order", rows[0].ID, rows[1].ID)
	}

	none, err := f.q.ListConsumedFollowUpInputsForRun(ctx, f.seedInteractiveTaskRun(ctx, t, "running", &wkr))
	if err != nil {
		t.Fatalf("ListConsumedFollowUpInputsForRun(empty run): %v", err)
	}
	if none == nil || len(none) != 0 {
		t.Fatalf("empty run = %#v, want a non-nil empty slice", none)
	}
}
