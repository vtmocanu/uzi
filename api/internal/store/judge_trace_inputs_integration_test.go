package store_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestListRunInputsForRunLiveDB exercises judge.sql's ListRunInputsForRun against a REAL
// Postgres. Until this landed, the query had NO live exercise at all: its only production
// caller is workersvc/judge_trace.go, and every test that reaches JudgeTrace runs against
// workersvc's fakeStore, which returns a canned slice — so the SQL text had never executed.
//
// FOUND BY THE QUERY INVENTORY (judge_query_inventory_test.go), which declared it UNPINNED.
// Worth recording how: the inventory did not confirm something someone already suspected, it
// named a query nobody was looking at, outside the work that motivated building it.
//
// The three guarantees below are chosen so each can FAIL — an exercise that merely runs the
// query would flip the inventory entry from UNPINNED to pinned while proving nothing, which
// is worse than the gap, because the gap is visible and a false pin is not.
//
// It is the deliberate MIRROR of its sibling TestListFollowUpInputsForRunLiveDB, and the
// contrasts are the point: that query filters to kind='follow_up', orders newest-first, and
// is uncapped; this one takes EVERY kind, orders OLDEST-first, and is capped. Confusing the
// two is what the sibling's own comment warns about ("the newest are never dropped behind a
// cap the way the judge's oldest-first ListRunInputsForRun would", Decision 4).
func TestListRunInputsForRunLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer pool.Close()
	q := store.New(pool)

	userID, connID, repoID, runID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("judgetrace-%s@e2e", userID))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)
	mustExec(ctx, t, pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status)
		 VALUES ($1, $2, $3, 1, 't', 'd', 'running')`, runID, userID, repoID)

	// DISTINCT body per row, and the position encoded in the text. A fixture whose rows carry
	// the same value cannot tell "the right rows in the right order" from "some rows" — the
	// memberRow lesson this branch keeps relearning. The kinds are mixed deliberately: this
	// query must return ALL of them.
	//
	// 🔴 WHAT THIS FIXTURE CANNOT PIN, stated so the inventory entry is not read as covering
	// it: `consumed_at` and `created_at` are projected by the query and asserted by NOTHING
	// here — and the fixture could not assert them even if it tried, because the insert
	// supplies only (run_id, kind, body), so every row takes `consumed_at = NULL` and a
	// `created_at` of now() within the same second (00020_workers_runs.sql). A fold of either
	// column is invisible for two independent reasons, and either one alone would be enough.
	// Closing it needs varied values per row, not another assertion.
	insert := func(kind, body string) {
		mustExec(ctx, t, pool,
			`INSERT INTO run_user_inputs (run_id, kind, body) VALUES ($1, $2, $3)`, runID, kind, body)
	}
	insert("follow_up", "1-oldest")
	insert("approve_plan", "2-second")
	insert("cancel", "3-third")
	insert("follow_up", "4-newest")

	// A second run's steering log must never bleed in.
	otherRun := uuid.New()
	mustExec(ctx, t, pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status)
		 VALUES ($1, $2, $3, 2, 't', 'd', 'running')`, otherRun, userID, repoID)
	mustExec(ctx, t, pool,
		`INSERT INTO run_user_inputs (run_id, kind, body) VALUES ($1, 'follow_up', 'other-run')`, otherRun)

	bodies := func(rows []store.RunUserInput) []string {
		out := make([]string, 0, len(rows))
		for _, r := range rows {
			out = append(out, r.Body.String)
		}
		return out
	}

	// (1) RUN-SCOPED, and (2) EVERY KIND, and (3) OLDEST FIRST — one read, three properties.
	all, err := q.ListRunInputsForRun(ctx, store.ListRunInputsForRunParams{RunID: runID, Lim: 10})
	if err != nil {
		t.Fatalf("ListRunInputsForRun: %v", err)
	}
	want := []string{"1-oldest", "2-second", "3-third", "4-newest"}
	if got := bodies(all); len(got) != len(want) {
		t.Fatalf("got %d rows %v, want exactly %d — the other run's input must not bleed in "+
			"and no kind may be filtered out", len(got), got, len(want))
	}
	for i, w := range want {
		if bodies(all)[i] != w {
			t.Fatalf("row %d = %q, want %q — the trace is OLDEST-first (ORDER BY id ASC); "+
				"full order was %v", i, bodies(all)[i], w, bodies(all))
		}
	}
	for _, r := range all {
		if r.RunID != runID {
			t.Fatalf("a row from run %s leaked into run %s's trace", r.RunID, runID)
		}
	}
	// The kinds the follow-up queue deliberately EXCLUDES are present here.
	//
	// HONEST NOTE ON WHICH ASSERTION ACTUALLY FIRES: under the fold that adds
	// `AND kind = 'follow_up'`, the COUNT check above reds first (2 rows, not 4), not this
	// loop — measured, not assumed. So this loop is documentation of the contrast with
	// ListFollowUpInputsForRun rather than the discriminator for it. It is kept because it
	// names the property by kind, which a count cannot, and it WOULD fire on a filter that
	// swapped one kind for another while leaving the count at 4. Recorded rather than
	// credited: this branch has repeatedly credited assertions that sit behind an earlier
	// failure.
	kinds := map[string]bool{}
	for _, r := range all {
		kinds[r.Kind] = true
	}
	for _, k := range []string{"follow_up", "approve_plan", "cancel"} {
		if !kinds[k] {
			t.Fatalf("kind %q missing — the judge trace carries the WHOLE steering log, "+
				"unlike ListFollowUpInputsForRun which filters to follow_up", k)
		}
	}

	// (4) THE CAP TAKES THE OLDEST N — Decision 4's stated behaviour: under a cap the judge
	// keeps the longest-waiting inputs, not the latest.
	//
	// MEASURED REACHABLE, because "strongest property" was asserted here before it was shown.
	// None of the first three folds executes this line — `:102` and `:96` are t.Fatalf and one
	// of them dies first every time — so it was credited from behind an earlier failure, the
	// mistake this file's own note about the by-kind loop already calls out. A fourth fold
	// isolates it: keep the presentation order correct and cut from the wrong END, i.e.
	// `(SELECT … ORDER BY id DESC LIMIT @lim) t ORDER BY t.id ASC`. That leaves the uncapped
	// read entirely correct — `:96` and `:102` pass — and reddens ONLY this assertion, with
	// `capped read = [3-third 4-newest]`.
	//
	// The obvious fold does NOT work, which is why the shape above is specific: applying
	// `LIMIT` before any `ORDER BY` returns heap order, and on a table this small that is
	// insertion order, so it would return the right two BY LUCK and prove nothing.
	capped, err := q.ListRunInputsForRun(ctx, store.ListRunInputsForRunParams{RunID: runID, Lim: 2})
	if err != nil {
		t.Fatalf("ListRunInputsForRun capped: %v", err)
	}
	if got, wantCap := bodies(capped), []string{"1-oldest", "2-second"}; len(got) != 2 ||
		got[0] != wantCap[0] || got[1] != wantCap[1] {
		t.Fatalf("capped read = %v, want %v — @lim must cut from the NEWEST end, keeping the "+
			"oldest inputs (Decision 4). Getting the newest two means the cap was applied to a "+
			"descending scan, whether or not the rows are PRESENTED in ascending order", got, wantCap)
	}
}
