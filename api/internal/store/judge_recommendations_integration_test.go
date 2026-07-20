package store_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// TestJudgeBacklogRunAnchorLiveDB pins what the fake-store unit tests structurally cannot:
// the SQL of ListJudgeRecommendationRowsForUser (PRD #98 M1). Three guarantees live only in
// that query and nowhere in Go:
//
//  1. Owner scoping — another user's reviews never appear, even when their recommendation
//     sits on the very same (category, target) coordinate.
//  2. The ?run= anchor is a SEMI-join, not an equality: anchoring on one run returns that
//     coordinate's occurrences in EVERY run the caller owns (the cross-run evidence the
//     dedup exists for), while dropping coordinates absent from the anchor run.
//  3. The anchor is itself owner-scoped, and a NULL anchor is a no-op — a run id belonging
//     to ANOTHER user matches nothing rather than reaching across the tenant boundary.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (the store-IT runner
// e2e/run-store-it.sh provides one); `go test ./...` without it SKIPs.
func TestJudgeBacklogRunAnchorLiveDB(t *testing.T) {
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

	owner, other := uuid.New(), uuid.New()
	connID, repoID := uuid.New(), uuid.New()
	for _, u := range []uuid.UUID{owner, other} {
		mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
			u, fmt.Sprintf("judgebacklog-%s@e2e", u))
	}
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, owner, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)

	iid := int64(0)
	// judgedRun creates a finished run owned by userID, its review, and one recommendation
	// per coordinate. Returns the run id — the anchor a notification deep-links to.
	judgedRun := func(userID uuid.UUID, title string, coords ...[2]string) uuid.UUID {
		runID, reviewID := uuid.New(), uuid.New()
		iid++
		mustExec(ctx, t, pool,
			`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status)
			 VALUES ($1, $2, $3, $4, $5, 'd', 'completed')`, runID, userID, repoID, iid, title)
		mustExec(ctx, t, pool,
			`INSERT INTO run_reviews (id, target_run_id, user_id, verdict) VALUES ($1, $2, $3, 'issues')`,
			reviewID, runID, userID)
		for _, c := range coords {
			mustExec(ctx, t, pool,
				`INSERT INTO review_recommendations (review_id, category, target, rationale_md)
				 VALUES ($1, $2, $3, 'because')`, reviewID, c[0], c[1])
		}
		return runID
	}

	rg := [2]string{"install_worker_tool", "rg"}
	coder := [2]string{"improve_agent", "coder"}
	docs := [2]string{"improve_uzi", "docs"}

	anchorRun := judgedRun(owner, "anchor run", rg, coder)
	otherOwnRun := judgedRun(owner, "another of my runs", rg, docs)
	// Same coordinate, DIFFERENT user: must never appear in the owner's backlog, and must
	// not be reachable by anchoring on that user's run either.
	foreignRun := judgedRun(other, "not my run", rg)

	fetch := func(userID uuid.UUID, anchor pgtype.UUID) []store.ListJudgeRecommendationRowsForUserRow {
		t.Helper()
		rows, err := q.ListJudgeRecommendationRowsForUser(ctx, store.ListJudgeRecommendationRowsForUserParams{
			UserID: userID, RunAnchor: anchor, Lim: 1000,
		})
		if err != nil {
			t.Fatalf("ListJudgeRecommendationRowsForUser: %v", err)
		}
		return rows
	}
	// coordRuns maps "category/target" → the set of run ids it was returned for.
	coordRuns := func(rows []store.ListJudgeRecommendationRowsForUserRow) map[string]map[uuid.UUID]bool {
		out := map[string]map[uuid.UUID]bool{}
		for _, r := range rows {
			k := r.Category + "/" + r.Target
			if out[k] == nil {
				out[k] = map[uuid.UUID]bool{}
			}
			out[k][r.RunID] = true
		}
		return out
	}
	anchorOf := func(id uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: id, Valid: true} }

	// ---- 1. unanchored: owner-scoped, and a NULL anchor is a no-op ---------------------
	all := coordRuns(fetch(owner, pgtype.UUID{}))
	if len(all) != 3 {
		t.Fatalf("unanchored backlog has %d coordinates, want 3 (a NULL anchor must not filter): %+v", len(all), all)
	}
	if got := all["install_worker_tool/rg"]; len(got) != 2 || !got[anchorRun] || !got[otherOwnRun] {
		t.Fatalf("rg appeared in runs %v, want exactly the owner's two runs", got)
	}
	if got := all["install_worker_tool/rg"]; got[foreignRun] {
		t.Fatal("another user's review leaked into the owner's backlog — the read is owner-scoped")
	}
	// The run_title projection is the runs join this query added over #94's.
	titles := map[uuid.UUID]string{}
	for _, r := range fetch(owner, pgtype.UUID{}) {
		titles[r.RunID] = r.RunTitle
	}
	if titles[anchorRun] != "anchor run" {
		t.Errorf("run_title for the anchor run = %q, want its issue_title", titles[anchorRun])
	}

	// ---- 2. the anchor is a SEMI-join, not an equality ---------------------------------
	anchored := coordRuns(fetch(owner, anchorOf(anchorRun)))
	if _, ok := anchored["improve_uzi/docs"]; ok {
		t.Error("docs does not occur in the anchor run, so its group must be filtered out")
	}
	if got := anchored["improve_agent/coder"]; len(got) != 1 || !got[anchorRun] {
		t.Errorf("coder occurs only in the anchor run, want just it, got %v", got)
	}
	// The load-bearing one: rg is anchored, but its OTHER-run occurrence still comes back,
	// so the group can show "seen in 2 runs" when opened from the notification.
	if got := anchored["install_worker_tool/rg"]; len(got) != 2 || !got[anchorRun] || !got[otherOwnRun] {
		t.Fatalf("anchored rg returned runs %v, want BOTH the anchor run and the other run "+
			"(the anchor selects coordinates, it does not trim occurrences)", got)
	}

	// ---- 3. the anchor is owner-scoped too ---------------------------------------------
	if rows := fetch(owner, anchorOf(foreignRun)); len(rows) != 0 {
		t.Fatalf("anchoring on ANOTHER user's run returned %d rows, want 0 (no cross-tenant reach, "+
			"no existence oracle)", len(rows))
	}
	if rows := fetch(owner, anchorOf(uuid.New())); len(rows) != 0 {
		t.Fatalf("anchoring on an unknown run returned %d rows, want 0", len(rows))
	}

	// ---- 4. the LIMIT is honoured ------------------------------------------------------
	capped, err := q.ListJudgeRecommendationRowsForUser(ctx, store.ListJudgeRecommendationRowsForUserParams{
		UserID: owner, Lim: 2,
	})
	if err != nil {
		t.Fatalf("capped read: %v", err)
	}
	if len(capped) != 2 {
		t.Fatalf("LIMIT 2 returned %d rows, want 2 — the hard row cap must bind in SQL", len(capped))
	}
}

// TestJudgeBacklogPreviewRecencyLiveDB pins the ORDER BY that decides which occurrence
// supplies a group's rationale_preview (PRD #98 Decision 1). The grouper takes a group's
// first row, so "most-recent occurrence" is whatever this query's ordering says it is —
// which makes the choice of timestamp a real behavioural decision, not a formatting one.
//
// The failure it guards is specific: UpsertRunReviewWithRecommendations (judge.sql) makes a
// RE-JUDGE an in-place upsert that rewrites rationale_md and bumps updated_at but LEAVES
// created_at at the first judging. So run A judged Monday, run B judged Tuesday, run A
// re-judged Wednesday with new text — ordering by created_at puts B first and the preview
// quotes Tuesday's text, which the judge has already superseded. Ordering by updated_at
// puts the re-judged A first, which is what a reader means by "the latest".
//
// This can only be tested against a real database: the ordering lives entirely in SQL.
func TestJudgeBacklogPreviewRecencyLiveDB(t *testing.T) {
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

	owner, connID, repoID := uuid.New(), uuid.New(), uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		owner, fmt.Sprintf("preview-%s@e2e", owner))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, owner, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)

	// Explicit timestamps, so the scenario is exact rather than dependent on insert speed.
	mon := "2026-07-06 09:00:00+00"
	tue := "2026-07-07 09:00:00+00"
	wed := "2026-07-08 09:00:00+00"
	seed := func(iid int, title, createdAt, updatedAt, rationale string) {
		runID, reviewID := uuid.New(), uuid.New()
		mustExec(ctx, t, pool,
			`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status)
			 VALUES ($1, $2, $3, $4, $5, 'd', 'completed')`, runID, owner, repoID, iid, title)
		mustExec(ctx, t, pool,
			`INSERT INTO run_reviews (id, target_run_id, user_id, verdict, created_at, updated_at)
			 VALUES ($1, $2, $3, 'issues', $4::timestamptz, $5::timestamptz)`,
			reviewID, runID, owner, createdAt, updatedAt)
		mustExec(ctx, t, pool,
			`INSERT INTO review_recommendations (review_id, category, target, rationale_md)
			 VALUES ($1, 'improve_uzi', 'docs', $2)`, reviewID, rationale)
	}
	// Run A: first judged Monday, RE-judged Wednesday — created_at stays Monday.
	seed(1, "run A", mon, wed, "wednesday re-judge text")
	// Run B: judged Tuesday and never re-judged. Newer by created_at, older by updated_at.
	seed(2, "run B", tue, tue, "tuesday text")

	rows, err := q.ListJudgeRecommendationRowsForUser(ctx, store.ListJudgeRecommendationRowsForUserParams{
		UserID: owner, Lim: 100,
	})
	if err != nil {
		t.Fatalf("ListJudgeRecommendationRowsForUser: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %d", len(rows))
	}
	if rows[0].RationaleMd != "wednesday re-judge text" {
		t.Fatalf("first row's rationale = %q, want the RE-JUDGED text.\n"+
			"The group preview takes the first row, so ordering by created_at here would quote "+
			"text the judge already replaced. Order by rv.updated_at.", rows[0].RationaleMd)
	}
	if rows[0].RunTitle != "run A" {
		t.Errorf("first row = %q, want run A (re-judged most recently)", rows[0].RunTitle)
	}
}
