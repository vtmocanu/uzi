package store_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"

	"gitlab.example.com/vtmocanu/uzi/api/internal/secretbox"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
	"gitlab.example.com/vtmocanu/uzi/api/internal/workersvc"
)

// TestJudgeCategoryStatsMatrixUncappedAndDedupedLiveDB pins the two properties PRD #270
// requires of the Judge chip-count matrix, and which a fake store structurally CANNOT show —
// both are properties of the UNCAPPED whole-backlog load feeding the shared Go rollup, not of
// any single hand-written row set:
//
//  1. UNCAPPED. JudgeCategoryStats loads the rows with Lim: 0 (the LIMIT NULLIF sentinel), so
//     a category whose groups sit ENTIRELY past the backlog's 2000-row cap is still rolled up
//     in full — the chip reads the true figure while the truncated list shows none of that
//     category's cards.
//  2. DEDUPED TO GROUPS. GroupJudgeRecommendations dedups by (category, target), so a
//     coordinate recurring across ≥2 reviews/runs counts ONCE — not once per row. A raw-row
//     count would over-report.
//
// It exercises the REAL service (workersvc.JudgeCategoryStats → the shared rollup → the
// matrix), because PRD #94 Decision 2 forbids re-expressing the bucket ladder in SQL: there is
// no aggregate query to test in isolation any more, and the matrix's `all` count is the thing
// the chip renders. Every seeded group is open (no disposition, no filed link), so it rolls up
// todo; `all` and `todo` therefore carry the same true count.
//
// The contrast is drawn against the UNFILTERED (?bucket=all, no ?category=) backlog, because
// #235 pushed the category predicate BELOW the LIMIT: a category-filtered request can never
// truncate that label off-page, so only the all-labels list demonstrates the rollup doing work
// the delivered list cannot.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (the store-IT runner
// e2e/run-store-it.sh provides one); `go test ./...` without it SKIPs.
func TestJudgeCategoryStatsMatrixUncappedAndDedupedLiveDB(t *testing.T) {
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

	owner := uuid.New()
	connID, repoID := uuid.New(), uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		owner, fmt.Sprintf("catstats-%s@e2e", owner))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, owner, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)

	// targetCat below is improve_uzi, the ONE category with an instance-wide second consumer:
	// ListOpenImproveUziRecommendations selects it across the WHOLE table (not owner-scoped,
	// oldest-first, LIMIT @lim). The OLD improve_uzi groups this test seeds otherwise linger
	// in the shared live-DB and evict a newer decoy from another package's top-N guard
	// (TestFiledIssueCloseAutoDonesOnceLiveDB) — the exact cross-fixture bleed the sibling
	// recommendation_dispositions_integration_test.go documents and defends against with the
	// same defer. Deleting this owner's run_reviews cascades its recommendations
	// (review_recommendations.review_id ON DELETE CASCADE, 00059_run_reviews.sql:46), so the
	// instance-wide improve_uzi backlog returns to its prior state. Runs before pool.Close
	// (LIFO), and on failure too (Fatalf runs defers).
	defer func() {
		mustExec(ctx, t, pool, `DELETE FROM run_reviews WHERE user_id = $1`, owner)
	}()

	// bulkSeed inserts one run + one review + one recommendation per issue_iid in
	// [lo, hi], all under the SAME category, in a single round trip. Each gets a distinct
	// target ('<cat>-<iid>') so it is its own group. updatedAt controls the backlog's
	// DESC ordering, which is what decides who survives the row cap. issue_iid is unique
	// so the ranges must not overlap across calls.
	bulkSeed := func(cat string, lo, hi int, updatedAt string) {
		t.Helper()
		mustExec(ctx, t, pool, `
			WITH gs AS (SELECT g FROM generate_series($4::int, $5::int) AS g),
			r AS (
				INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status)
				SELECT gen_random_uuid(), $1, $2, gs.g, 'run '||gs.g, 'd', 'completed' FROM gs
				RETURNING id
			),
			rv AS (
				INSERT INTO run_reviews (id, target_run_id, user_id, verdict, updated_at)
				SELECT gen_random_uuid(), r.id, $1, 'issues', $6::timestamptz FROM r
				RETURNING id, target_run_id
			)
			INSERT INTO review_recommendations (review_id, category, target, rationale_md)
			SELECT rv.id, $3, $3||'-'||rv.target_run_id::text, 'because' FROM rv`,
			owner, repoID, cat, lo, hi, updatedAt)
	}

	const (
		fillerCat = "adjust_template"
		targetCat = "improve_uzi"
		// improveUziGroups distinct targetCat groups, seeded OLD so the whole category
		// sorts BELOW the newer filler and falls past the row cap.
		improveUziGroups = 50
		// fillerRows newer rows, enough to fill the entire first page (the row cap), so no
		// targetCat row survives the truncation.
		fillerRows = workersvc.JudgeBacklogMaxRows
	)

	// Filler: fillerRows newest rows in another category, issue_iids 1..fillerRows.
	bulkSeed(fillerCat, 1, fillerRows, "2026-08-01 09:00:00+00")
	// The target category: improveUziGroups OLD groups, issue_iids 10001..10050.
	bulkSeed(targetCat, 10001, 10000+improveUziGroups, "2026-01-01 09:00:00+00")

	// A cross-run DUPLICATE coordinate: the SAME (improve_uzi, improve_uzi-dup) target seeded
	// in TWO reviews/runs. It is ONE new distinct target, so it adds ONE to the distinct-group
	// count but TWO to the raw-row count — a raw-row counter would read the category as one
	// higher than COUNT(DISTINCT target) does. That gap is what makes the wantTargetCount
	// assertion below a real dedupe proof rather than a tautology.
	dupTarget := targetCat + "-dup"
	seedCoordTwice := func() {
		for i := 0; i < 2; i++ {
			runID, reviewID := uuid.New(), uuid.New()
			mustExec(ctx, t, pool,
				`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status)
				 VALUES ($1, $2, $3, $4, 'dup run', 'd', 'completed')`, runID, owner, repoID, 20001+i)
			mustExec(ctx, t, pool,
				`INSERT INTO run_reviews (id, target_run_id, user_id, verdict, updated_at)
				 VALUES ($1, $2, $3, 'issues', '2026-01-02 09:00:00+00'::timestamptz)`, reviewID, runID, owner)
			mustExec(ctx, t, pool,
				`INSERT INTO review_recommendations (review_id, category, target, rationale_md)
				 VALUES ($1, $2, $3, 'because')`, reviewID, targetCat, dupTarget)
		}
	}
	seedCoordTwice()

	// The TRUE distinct-group count for the target category: the improveUziGroups distinct
	// bulk targets PLUS the one duplicated coordinate (counted once), so:
	wantTargetCount := improveUziGroups + 1

	box, err := secretbox.New(make([]byte, secretbox.KeySize))
	if err != nil {
		t.Fatalf("secretbox: %v", err)
	}
	svc := workersvc.New(q, box, workersvc.Params{})

	// ---- 1. the matrix is UNCAPPED and DEDUPED -----------------------------------------
	stats, err := svc.JudgeCategoryStats(ctx, owner, uuid.Nil)
	if err != nil {
		t.Fatalf("JudgeCategoryStats: %v", err)
	}
	// Every seeded group is open → rolls up todo, so `all` and `todo` agree; assert on `all`,
	// the whole-backlog fallback the chip reads.
	counts := stats.CountsByBucket["all"]
	if got := counts[targetCat]; got != wantTargetCount {
		t.Fatalf("category-stats all[%s] = %d, want %d — the rollup must count EVERY group "+
			"(uncapped, past the %d-row backlog cap) and count a cross-run coordinate ONCE "+
			"(dedup by (category, target))", targetCat, got, wantTargetCount, workersvc.JudgeBacklogMaxRows)
	}
	// The open groups roll up todo, never a settled rung — a second, tab-scoped assertion.
	if got := stats.CountsByBucket["todo"][targetCat]; got != wantTargetCount {
		t.Fatalf("category-stats todo[%s] = %d, want %d (every seeded group is open → todo)", targetCat, got, wantTargetCount)
	}
	// The filler category's own count is its distinct targets — a secondary check that the
	// rollup separates the categories rather than lumping them.
	if got := counts[fillerCat]; got != fillerRows {
		t.Errorf("category-stats all[%s] = %d, want %d (each filler row is its own group)", fillerCat, got, fillerRows)
	}

	// ---- 2. the UNFILTERED backlog list truncates and drops the target category off-page
	backlog, err := svc.JudgeRecommendationBacklog(ctx, owner, "all", uuid.Nil, nil)
	if err != nil {
		t.Fatalf("JudgeRecommendationBacklog: %v", err)
	}
	if !backlog.Truncated {
		t.Fatalf("the unfiltered backlog must be truncated with %d filler rows ahead of the "+
			"target category (cap is %d)", fillerRows, workersvc.JudgeBacklogMaxRows)
	}
	deliveredTarget := 0
	for _, g := range backlog.Groups {
		if g.Category == targetCat {
			deliveredTarget++
		}
	}
	if deliveredTarget != 0 {
		t.Fatalf("the truncated all-labels list delivered %d %s groups, want 0 — the whole "+
			"category sits past the row cap, which is exactly the case the uncapped aggregate "+
			"(true count %d) exists to cover", deliveredTarget, targetCat, wantTargetCount)
	}

	// The load-bearing contrast, stated as one assertion: the chip count is the true figure
	// while the delivered list shows none of that category's cards.
	if counts[targetCat] <= deliveredTarget {
		t.Fatalf("aggregate %s count (%d) must exceed what the truncated list delivers (%d) — "+
			"otherwise the aggregate is not doing work the list cannot", targetCat, counts[targetCat], deliveredTarget)
	}
}
