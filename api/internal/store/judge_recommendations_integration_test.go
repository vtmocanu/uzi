package store_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

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

// TestJudgeBacklogProjectsEveryColumnLiveDB pins the backlog read's PROJECTION — that each
// selected column carries the value from the column it names, and not a constant, a NULL, or
// a neighbouring column.
//
// WHY THIS EXISTS, measured rather than argued (PRD #98 review). The read projects 16
// columns; before this test the two backlog LiveDB tests touched five (run_title, run_id,
// rationale_md, target, category). Constant-folding the other six SIMULTANEOUSLY —
// verdict→'ideal', confidence→”, dismiss_reason→NULL, set_via→NULL, filed_issue_iid→NULL,
// filed_issue_url→NULL — and regenerating left the ENTIRE live-DB suite green, plus
// `go test ./...`, typecheck and every vitest. Folding them together makes it unambiguous:
// had any one been pinned, that run would have been red.
//
// A fake cannot stand in for this. Every Go-level test hands the grouper rows whose fields
// are already populated, so it passes whether or not SQL ever produced them — the same
// structural blindness the file header describes for the owner predicate.
//
// Two of these carry B3's headline label TOGETHER: "Done via #91" needs set_via AND
// filed_issue_iid. Pinning only set_via would leave that feature one silent SQL edit from
// rendering the unnamed "Done via issue close" fallback for every auto-done — the fallback
// firing for the wrong reason, with every gate green.
//
// ASSERTIONS ARE PAIRWISE-DIFFERENT WHERE THEY CAN BE, not equal-to-a-constant. Two reviews
// carry different verdicts, two recommendations different confidences, two dispositions
// different reasons and different provenance. A fold to ANY single constant collapses a pair
// and fails, whichever constant is chosen — an equality assertion only catches folds to a
// value other than the one the fixture happens to use.
//
// A NOTE ON THE LADDER'S TWO INPUTS, deliberately not covered here: folding
// disposition_status or filed_settled DOES fail today, but only via
// TestBulkDispositionFansOutAcrossRunsLiveDB — a BULK-DISPOSITION test, which catches it
// because M2's post-write re-read happens to run this query. That is coverage by accident,
// not by design: deleting or narrowing that M2 test would silently unpin both columns here.
func TestJudgeBacklogProjectsEveryColumnLiveDB(t *testing.T) {
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
		owner, fmt.Sprintf("judgeproj-%s@e2e", owner))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, owner, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)

	// Two runs, each judged with a DIFFERENT verdict, so a fold to any constant collapses
	// the pair. iids are unique per repo (issues is keyed on (repo_id, forge_issue_iid)).
	type review struct {
		runID, reviewID uuid.UUID
	}
	newReview := func(iid int64, verdict string) review {
		r := review{uuid.New(), uuid.New()}
		mustExec(ctx, t, pool,
			`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status)
			 VALUES ($1, $2, $3, $4, $5, 'd', 'completed')`, r.runID, owner, repoID, iid,
			fmt.Sprintf("run %d", iid))
		mustExec(ctx, t, pool,
			`INSERT INTO run_reviews (id, target_run_id, user_id, verdict) VALUES ($1, $2, $3, $4)`,
			r.reviewID, r.runID, owner, verdict)
		return r
	}
	autoRev := newReview(9001, "issues")
	handRev := newReview(9002, "ideal")

	// autoRev's coordinate: filed as #4242, that issue CLOSED, so the M6 sync marks it done
	// with set_via='issue_close'. handRev's: dismissed by a person, with a different reason
	// and no provenance. Confidences differ too.
	const cat = "install_worker_tool"
	autoTarget, handTarget := "rg-auto", "rg-hand"
	mustExec(ctx, t, pool,
		`INSERT INTO review_recommendations (review_id, category, target, rationale_md, confidence)
		 VALUES ($1, $2, $3, 'because', 'high')`, autoRev.reviewID, cat, autoTarget)
	mustExec(ctx, t, pool,
		`INSERT INTO review_recommendations (review_id, category, target, rationale_md, confidence)
		 VALUES ($1, $2, $3, 'because', 'low')`, handRev.reviewID, cat, handTarget)

	// A THIRD coordinate, unfiled, INSIDE autoRev — the review that owns the filed link.
	//
	// Without it the filed and unfiled coordinates sat in DIFFERENT reviews, so
	// `f.review_id = rv.id` alone separated them and the filed join's coordinate halves never
	// carried any weight: dropping `AND f.target = rr.target` left the whole suite GREEN
	// (measured). With an unfiled sibling in the same review, that fold makes THIS row
	// inherit autoRev's filed link, so filed_at / filed_settled / filed_issue_iid /
	// filed_issue_url all become observable — four columns on one fixture change.
	//
	// Same root cause as the rationale literal next door: every fixture row looked alike, so
	// the assertions had nothing to discriminate. The fix is in the fixture, not the
	// assertions.
	//
	// MEASURED, one fold per run against a FRESH database (PRD #98, 2026-07-21). Each line
	// mutates ListJudgeRecommendationRowsForUser — THE FIRST QUERY BODY in
	// judge_recommendations.sql, at its lines 69 and 71 — regenerated through sqlc, and each
	// names the assertion that caught it:
	//
	//   drop `AND f.target = rr.target`      RED  — the sibling's filed-link assertion
	//   drop `AND f.category = rr.category`  RED  — the cross-category coordinate's, below
	//   drop BOTH coordinate halves          RED  — both of the above fire together
	//   drop `AND d.target = rr.target`      RED  — the sibling's disposition assertion
	//   drop `AND d.category = rr.category`  RED  — the cross-category disposition assertion
	//   `f.filed_at` -> `d.set_at`           RED  — the no-filed-row filed_at assertion
	//
	// SCOPE — READ THIS BEFORE CONCLUDING "THE COORDINATE PREDICATE IS PINNED". The SAME two
	// three-part joins appear a SECOND time in that file, on ListJudgeTriageRowsForRuns (M4's
	// /runs badge count, lines 144 and 146). NOTHING ABOVE TOUCHES IT — every fold in the
	// table is a mutation of the FIRST body, and the second one was measured entirely
	// unpinned: dropping BOTH coordinate halves off its `f` join, and separately off its `d`
	// join, each left the ENTIRE live-DB suite green.
	// TestJudgeRunTodoTriageRowsAreCoordinateScopedLiveDB at the end of this file now covers
	// it — read ITS verification-status block before treating that query as pinned, because
	// it was written without a database slot and its folds are still owed.
	//
	// PROVENANCE OF THE "was GREEN" CLAIMS, because they are not all the same strength: the
	// category-half GREEN was measured here, on this fixture with the sibling but before the
	// fourth coordinate. The target-half and both-halves GREENs are INHERITED from the M3
	// checkpoint's earlier sweep and were not re-run against the pre-fix tree.
	const unfiledInAutoRev = "rg-unfiled-sibling"
	mustExec(ctx, t, pool,
		`INSERT INTO review_recommendations (review_id, category, target, rationale_md, confidence)
		 VALUES ($1, $2, $3, 'because', 'medium')`, autoRev.reviewID, cat, unfiledInAutoRev)

	// A FOURTH coordinate: autoRev again, the filed coordinate's OWN target, a DIFFERENT
	// category. The sibling above pins only the join's TARGET half — measured: dropping
	// `AND f.category = rr.category` alone left the whole live-DB suite GREEN, because every
	// coordinate in the fixture carried the same category, so nothing could observe a
	// category mismatch. Same root cause one level down: the sibling was the obvious
	// mutation's antidote, not the predicate's.
	//
	// This is a real state, not a contrived one — a target is a tool/file/agent name and
	// recurs across categories (`improve_uzi/docs` and `adjust_template/docs`). With the
	// category half dropped, filing an issue on one category's coordinate would mark the
	// SAME target under every other category as filed: a wrong `filed` bucket, a wrong
	// "filed as #IID" chip, and an M6 close edge attributed to a coordinate nobody filed.
	const otherCat = "improve_uzi"
	mustExec(ctx, t, pool,
		`INSERT INTO review_recommendations (review_id, category, target, rationale_md, confidence)
		 VALUES ($1, $2, $3, 'because', 'medium')`, autoRev.reviewID, otherCat, autoTarget)

	// The filed link + the cached CLOSED issue that makes it an M6 close edge.
	filedID := uuid.New()
	const filedIID = int64(4242)
	const filedURL = "https://forge.e2e/g/r/-/issues/4242"
	mustExec(ctx, t, pool,
		`INSERT INTO issues (repo_id, forge_issue_iid, title, state, web_url, forge_updated_at, synced_at)
		 VALUES ($1, $2, 'filed from a recommendation', 'closed', $3, now(), now())`,
		repoID, filedIID, filedURL)
	mustExec(ctx, t, pool,
		`INSERT INTO recommendation_filed_issues
		   (id, review_id, category, target, filed_repo_id, filed_issue_iid, filed_issue_url, filed_by_user_id, filed_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, now())`,
		filedID, autoRev.reviewID, cat, autoTarget, repoID, filedIID, filedURL, owner)

	// Settle the auto coordinate through the REAL M6 path, not by inserting the disposition
	// by hand: ApplyFiledIssueCloseEdge is the query that writes set_via='issue_close' as a
	// literal. That makes this a producer -> consumer pin — M6 writes the column, the backlog
	// read projects it — rather than a projection pin standing on a hand-made row.
	edges, err := q.ListFiledIssueCloseEdges(ctx, store.ListFiledIssueCloseEdgesParams{RepoID: repoID, Lim: 50})
	if err != nil {
		t.Fatalf("list close edges: %v", err)
	}
	var edge *store.ListFiledIssueCloseEdgesRow
	for i := range edges {
		if edges[i].FiledID == filedID {
			edge = &edges[i]
			break
		}
	}
	if edge == nil {
		t.Fatalf("the seeded filed+closed coordinate produced no M6 close edge (got %d edges) — "+
			"the fixture, not the projection, is wrong", len(edges))
	}
	if _, err := q.ApplyFiledIssueCloseEdge(ctx, store.ApplyFiledIssueCloseEdgeParams{
		ReviewID: edge.ReviewID, Category: edge.Category, Target: edge.Target,
		RationaleHash: "hash-does-not-matter-here", FiledID: edge.FiledID,
	}); err != nil {
		t.Fatalf("apply close edge: %v", err)
	}

	// The hand-set one: a person dismisses it, so set_via stays NULL and the reason differs.
	mustExec(ctx, t, pool,
		`INSERT INTO recommendation_dispositions
		   (review_id, category, target, status, dismiss_reason, rationale_hash, set_by_user_id)
		 VALUES ($1, $2, $3, 'dismissed', 'not_an_issue', 'h', $4)`,
		handRev.reviewID, cat, handTarget, owner)

	rows, err := q.ListJudgeRecommendationRowsForUser(ctx, store.ListJudgeRecommendationRowsForUserParams{
		UserID: owner,
		Lim:    500,
	})
	if err != nil {
		t.Fatalf("list backlog rows: %v", err)
	}
	byTarget := map[string]store.ListJudgeRecommendationRowsForUserRow{}
	byCoord := map[[2]string]store.ListJudgeRecommendationRowsForUserRow{}
	for _, r := range rows {
		byCoord[[2]string{r.Category, r.Target}] = r
		if r.Category == cat {
			byTarget[r.Target] = r
		}
	}
	auto, ok := byTarget[autoTarget]
	if !ok {
		t.Fatalf("no backlog row for %s/%s", cat, autoTarget)
	}
	hand, ok := byTarget[handTarget]
	if !ok {
		t.Fatalf("no backlog row for %s/%s", cat, handTarget)
	}

	// 1+2. verdict — different per review, so ANY constant fold collapses them.
	if auto.Verdict == hand.Verdict {
		t.Errorf("both rows carry verdict %q — the column is folded to a constant", auto.Verdict)
	}
	if auto.Verdict != "issues" || hand.Verdict != "ideal" {
		t.Errorf("verdict = (%q, %q), want (issues, ideal)", auto.Verdict, hand.Verdict)
	}

	// 3. confidence — likewise a pair.
	if auto.Confidence == hand.Confidence {
		t.Errorf("both rows carry confidence %q — the column is folded", auto.Confidence)
	}
	if auto.Confidence != "high" || hand.Confidence != "low" {
		t.Errorf("confidence = (%q, %q), want (high, low)", auto.Confidence, hand.Confidence)
	}

	// 4. set_via — the PF-4 distinction, and the one whose failure is NOT fail-safe: a wrong
	// value here renders a system inference as the user's own decision.
	if auto.SetVia.String == hand.SetVia.String {
		t.Errorf("auto-done and hand-set carry the same set_via %q — provenance never reaches a client",
			auto.SetVia.String)
	}
	if !auto.SetVia.Valid || auto.SetVia.String != "issue_close" {
		t.Errorf("auto-done set_via = %q (valid=%v), want issue_close — written by the M6 close edge",
			auto.SetVia.String, auto.SetVia.Valid)
	}
	if hand.SetVia.Valid {
		t.Errorf("a human's dismissal carries set_via %q, want NULL", hand.SetVia.String)
	}

	// 5. dismiss_reason — carries false_positives on the ladder.
	if auto.DismissReason.Valid {
		t.Errorf("the auto-DONE row carries dismiss_reason %q, want NULL", auto.DismissReason.String)
	}
	if !hand.DismissReason.Valid || hand.DismissReason.String != "not_an_issue" {
		t.Errorf("hand dismiss_reason = %q (valid=%v), want not_an_issue",
			hand.DismissReason.String, hand.DismissReason.Valid)
	}

	// 6+7. the filed link — filed_issue_iid is B3's other half, and filed_issue_url is what
	// the occurrence's external link renders.
	if !auto.FiledIssueIid.Valid || auto.FiledIssueIid.Int64 != filedIID {
		t.Errorf("filed_issue_iid = %v (valid=%v), want %d",
			auto.FiledIssueIid.Int64, auto.FiledIssueIid.Valid, filedIID)
	}
	if !auto.FiledIssueUrl.Valid || auto.FiledIssueUrl.String != filedURL {
		t.Errorf("filed_issue_url = %q (valid=%v), want %s",
			auto.FiledIssueUrl.String, auto.FiledIssueUrl.Valid, filedURL)
	}
	// The unfiled row must NOT inherit them — otherwise a projection returning a constant
	// iid would satisfy the assertions above.
	//
	// BE PRECISE ABOUT WHAT THIS CATCHES. `hand` lives in a DIFFERENT review that owns no
	// filed row at all, so `f.review_id = rv.id` alone already separates it: this fires on a
	// wrong VALUE, NOT on the join's coordinate halves. The coordinate halves are pinned by
	// the same-review sibling below, and this message said "not row-scoped" while that hole
	// was open.
	//
	// PROVENANCE OF THE TWO CLAIMS IN THAT PARAGRAPH, since they are not equally strong:
	//   - "dropping `AND f.target = rr.target` leaves this green" — MEASURED here, fresh
	//     database. That fold reddened only the sibling's assertion; this one stayed silent.
	//   - "a fold to a constant would fire this" — INHERITED, not re-run: the checkpoint
	//     records `filed_issue_iid -> 4242` on fc8763f9 being caught by exactly this
	//     unfiled-row absence check, after satisfying every positive assertion. No fold of
	//     filed_issue_iid or filed_issue_url was executed in the 2026-07-21 sweep.
	if hand.FiledIssueIid.Valid || hand.FiledIssueUrl.Valid {
		t.Errorf("a coordinate with NO filed row anywhere carries a filed link (iid=%v url=%q) — "+
			"the projection is not reading the joined filed row (a constant fold, or a join that "+
			"matches across reviews)",
			hand.FiledIssueIid.Int64, hand.FiledIssueUrl.String)
	}
	// And filed_at drives filed_settled, so a settled link must read as settled.
	if !auto.FiledSettled {
		t.Error("the filed+settled row reports filed_settled=false")
	}
	if hand.FiledSettled {
		t.Error("the unfiled row reports filed_settled=true")
	}

	// The unfiled sibling IN THE SAME REVIEW as the filed coordinate. This is what makes the
	// filed join's coordinate half observable: with only cross-review comparisons above,
	// `f.review_id = rv.id` alone accounted for every difference.
	sibling, ok := byTarget[unfiledInAutoRev]
	if !ok {
		t.Fatalf("no backlog row for the unfiled sibling %s/%s", cat, unfiledInAutoRev)
	}
	if sibling.ReviewID != auto.ReviewID {
		t.Fatalf("fixture broken: the sibling must share autoRev with the filed coordinate, got %s vs %s",
			sibling.ReviewID, auto.ReviewID)
	}
	if sibling.FiledSettled || sibling.FiledAt.Valid || sibling.FiledIssueIid.Valid || sibling.FiledIssueUrl.Valid {
		t.Errorf("an unfiled coordinate sharing autoRev AND its category with the filed one inherited "+
			"its link (settled=%v at=%v iid=%v url=%q) — the filed join's TARGET half is gone, so every "+
			"coordinate in a review sharing a category with any filed issue reads as filed",
			sibling.FiledSettled, sibling.FiledAt.Valid, sibling.FiledIssueIid.Valid, sibling.FiledIssueUrl.String)
	}
	// And its disposition columns stay clear too — the disposition join is coordinate-keyed
	// for the same reason.
	if sibling.SetVia.Valid || sibling.DispositionStatus.Valid {
		t.Errorf("the undisposed sibling inherited a disposition (set_via=%q status=%q) — the disposition "+
			"join is not coordinate-scoped", sibling.SetVia.String, sibling.DispositionStatus.String)
	}

	// The CATEGORY half, which the sibling above cannot see: same review, same target as the
	// filed coordinate, different category. Measured — with only the sibling in place,
	// dropping `AND f.category = rr.category` left the ENTIRE live-DB suite green.
	crossCat, ok := byCoord[[2]string{otherCat, autoTarget}]
	if !ok {
		t.Fatalf("no backlog row for the cross-category coordinate %s/%s", otherCat, autoTarget)
	}
	if crossCat.ReviewID != auto.ReviewID {
		t.Fatalf("fixture broken: the cross-category coordinate must share autoRev with the filed one, got %s vs %s",
			crossCat.ReviewID, auto.ReviewID)
	}
	if crossCat.FiledSettled || crossCat.FiledAt.Valid || crossCat.FiledIssueIid.Valid || crossCat.FiledIssueUrl.Valid {
		t.Errorf("an unfiled coordinate sharing autoRev AND its target with the filed one inherited "+
			"its link (settled=%v at=%v iid=%v url=%q) — the filed join's CATEGORY half is gone, so "+
			"filing under one category marks the SAME target filed under every other category",
			crossCat.FiledSettled, crossCat.FiledAt.Valid, crossCat.FiledIssueIid.Valid, crossCat.FiledIssueUrl.String)
	}
	if crossCat.SetVia.Valid || crossCat.DispositionStatus.Valid {
		t.Errorf("the cross-category coordinate inherited a disposition (set_via=%q status=%q) — the "+
			"disposition join's category half is gone", crossCat.SetVia.String, crossCat.DispositionStatus.String)
	}

	// 8. rec_id — THE ONE THAT WRITES, and the reason these three were worth a second pass.
	//
	// Every other column pinned here is a display or attribution error when it drifts. rec_id
	// is what OccurrenceFileIssue hands the #68 draft/file endpoints as `recId`, and what
	// deleteDisposition(run_id, rec_id) uses for Undo — so a folded or cross-wired rec_id
	// files an issue against the WRONG recommendation and undoes the WRONG one. Both are
	// writes, one of them to the forge, and both are silent.
	//
	// Read back from the table rather than spelled: the assertion is that the projection
	// returns THE recommendation row's own id for its own coordinate.
	autoRecID := recIDFor(ctx, t, pool, autoRev.reviewID, cat, autoTarget)
	handRecID := recIDFor(ctx, t, pool, handRev.reviewID, cat, handTarget)
	if auto.RecID != autoRecID {
		t.Errorf("rec_id = %s, want the recommendation row's own id %s — a wrong rec_id FILES and UNDOES against another recommendation",
			auto.RecID, autoRecID)
	}
	if hand.RecID != handRecID {
		t.Errorf("rec_id = %s, want %s", hand.RecID, handRecID)
	}
	// Distinct per row, and never the review id — the two folds that would otherwise satisfy
	// a laxer assertion (a constant, or rv.id cross-wired into this column).
	if auto.RecID == hand.RecID {
		t.Errorf("both rows carry rec_id %s — the column is folded to a constant", auto.RecID)
	}
	if auto.RecID == auto.ReviewID {
		t.Errorf("rec_id equals review_id (%s) — the review id is cross-wired into the recommendation column", auto.RecID)
	}

	// 9. review_id — the coordinate's identity half; dispositions are keyed on it.
	if auto.ReviewID != autoRev.reviewID {
		t.Errorf("review_id = %s, want %s", auto.ReviewID, autoRev.reviewID)
	}
	if hand.ReviewID != handRev.reviewID {
		t.Errorf("review_id = %s, want %s", hand.ReviewID, handRev.reviewID)
	}
	if auto.ReviewID == hand.ReviewID {
		t.Errorf("both rows carry review_id %s — the column is folded", auto.ReviewID)
	}

	// 10. filed_at — the filed chip's timestamp, and what filed_settled is derived from.
	if !auto.FiledAt.Valid {
		t.Error("the filed+settled row carries a NULL filed_at")
	}
	// Same correction as the filed-link pair above: `hand` is in a review with no filed row,
	// so this fires when filed_at is a VALUE the projection invented, rather than one read off
	// the joined row. It is NOT a row-scoping check: the coordinate halves of the filed join
	// are pinned by the same-review unfiled sibling above.
	//
	// MEASURED (fresh database, one fold per run): folding `f.filed_at` to `d.set_at` reddens
	// THIS assertion. And dropping `AND f.target = rr.target` leaves it green while reddening
	// the sibling's — the two ran in the same sweep and only the sibling's message appeared,
	// which is the evidence that this assertion is a value check and not a scoping one.
	//
	// DO NOT REACH FOR `f.filed_at -> now()`, the obvious fold. It does not work here and its
	// failure is not this assertion firing: sqlc types the folded column as NOT NULL, the
	// generated struct field becomes `FiledAt interface{}`, and the package no longer
	// COMPILES (`sibling.FiledAt.Valid undefined`). That is loud, but a build error is not a
	// red assertion — the line below never executes. `d.set_at` is the fold that works,
	// because a nullable timestamptz off the other LEFT JOIN preserves the type AND looks
	// like data. (An earlier version of this comment credited the `now()` fold with the
	// result `d.set_at` actually produced; it could not have run.)
	if hand.FiledAt.Valid {
		t.Errorf("a coordinate with NO filed row anywhere carries filed_at %v — the projection is "+
			"not reading the joined filed row", hand.FiledAt.Time)
	}
}

// recIDFor reads a recommendation's own id straight from the table, so the rec_id assertion
// above compares the projection against the database rather than against a value the test
// also chose.
func recIDFor(ctx context.Context, t *testing.T, pool *pgxpool.Pool, reviewID uuid.UUID, category, target string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT id FROM review_recommendations WHERE review_id = $1 AND category = $2 AND target = $3`,
		reviewID, category, target).Scan(&id); err != nil {
		t.Fatalf("read rec id for %s/%s: %v", category, target, err)
	}
	return id
}

// ---- the /runs badge count query: ListJudgeTriageRowsForRuns -------------------------

// TestJudgeRunTodoTriageRowsAreCoordinateScopedLiveDB pins the SECOND query body in
// judge_recommendations.sql — ListJudgeTriageRowsForRuns, which feeds M4's per-run judge
// badge on /runs (workersvc.JudgeTodoCountsForRuns).
//
// WHY IT EXISTS, measured rather than argued (PRD #98, 2026-07-21). Everything the big
// projection test above proves concerns the FIRST body. This one carries the SAME two
// three-part LEFT JOINs, and NOTHING observed them: dropping BOTH coordinate halves off its
// `f` join, and separately off its `d` join, each left the ENTIRE live-DB suite green. Its
// only other coverage is judge_run_badge_test.go, which hands hand-built rows to a fake
// store — and a fake cannot observe which column the real query put in which field. That is
// the third recurrence of the class the checkpoint's live-DB-pin rule names.
//
// The production effect is a WRONG NUMBER, not a missing one. The consumer buckets each row
// with the shared ladder — BucketOf(disposition_status, filed_settled) == "todo" — so a
// disposition or filed row cross-attached from a NEIGHBOURING coordinate silently pushes
// occurrences off the todo rung and the badge under-counts. This query's own ORDER BY
// comment makes exactly that argument about truncation.
//
// SHAPE, and it is the whole point: the row struct projects only
// (run_id, disposition_status, filed_settled) — no category, no target — so a single review
// cannot tell you WHICH coordinate inherited. So each of the four join halves gets its OWN
// RUN, holding exactly two coordinates that share one half of the coordinate and differ in
// the other. Each fold then moves the tally in exactly one run, against its own assertion:
//
//	run   coordinates                      fold it is BUILT to catch
//	FT    (catA,tgt1) filed + (catA,tgt2)   drop `f.target = rr.target`
//	FC    (catA,tgt1) filed + (catB,tgt1)   drop `f.category = rr.category`
//	DT    (catA,tgt1) done  + (catA,tgt2)   drop `d.target = rr.target`
//	DC    (catA,tgt1) dism. + (catB,tgt1)   drop `d.category = rr.category`
//
// The pairs are deliberately NOT uniform. A fixture whose rows all look alike is what made
// both of the holes this branch just closed invisible, twice, one level down each time.
//
// A FIFTH fold is owed alongside those four, on the same query: `d.status` ->
// `'anything'::text`, which assertion 6 exists to catch. It is not a coordinate half but it
// is the one value fold here that is NOT fail-safe (see that assertion).
//
// 🔴 VERIFICATION STATUS: WRITTEN, NOT YET FOLDED. The "fold it is BUILT to catch" entries
// above are DESIGN INTENT, not measurements — this test was authored without a database slot
// (the validators held it), so no fold has been executed against it. Only the pre-existing
// GREENs in the paragraph above were measured. "Fix written" is not "closed", and a table
// that reads like results is exactly the proxy this branch keeps correcting.
//
// Replace this block with the measured per-fold results — one mutation per run, fresh
// database, mutation asserted present in both the .sql and the regenerated .sql.go before
// and gone from both after — and do not record this query as pinned until that is done.
// When you write those results, state what was executed: the fold reddened THIS named
// assertion, and the mutation was confirmed present before the run and gone after. Do not
// lean on "a contended run cannot produce a false green" — a run that prints no result, a
// mutation that silently fails to apply, and a suite that skips all yield "no failures", and
// "no failures observed" is not "the assertion passed". Two of those three have already
// happened on this branch.
func TestJudgeRunTodoTriageRowsAreCoordinateScopedLiveDB(t *testing.T) {
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
		owner, fmt.Sprintf("judgebadge-%s@e2e", owner))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, owner, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)

	const (
		catA = "install_worker_tool"
		catB = "improve_uzi"
		tgt1 = "rg"
		tgt2 = "fd"
	)

	// iids must be unique per repo; one counter serves runs and filed issues alike.
	iid := int64(0)
	newRun := func() (uuid.UUID, uuid.UUID) {
		iid++
		runID, reviewID := uuid.New(), uuid.New()
		mustExec(ctx, t, pool,
			`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status)
			 VALUES ($1, $2, $3, $4, $5, 'd', 'completed')`,
			runID, owner, repoID, iid, fmt.Sprintf("run %d", iid))
		mustExec(ctx, t, pool,
			`INSERT INTO run_reviews (id, target_run_id, user_id, verdict) VALUES ($1, $2, $3, 'issues')`,
			reviewID, runID, owner)
		return runID, reviewID
	}
	// Distinct rationale per row, for the same reason bulkFixture now does it.
	rec := func(reviewID uuid.UUID, category, target string) {
		mustExec(ctx, t, pool,
			`INSERT INTO review_recommendations (review_id, category, target, rationale_md, confidence)
			 VALUES ($1, $2, $3, $4, 'high')`, reviewID, category, target,
			fmt.Sprintf("rationale for %s/%s in review %s", category, target, reviewID))
	}
	fileIt := func(reviewID uuid.UUID, category, target string) {
		iid++
		mustExec(ctx, t, pool,
			`INSERT INTO recommendation_filed_issues
			   (id, review_id, category, target, filed_repo_id, filed_issue_iid, filed_issue_url, filed_by_user_id, filed_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, now())`,
			uuid.New(), reviewID, category, target, repoID, iid,
			fmt.Sprintf("https://forge.e2e/g/r/-/issues/%d", iid), owner)
	}
	disposeDone := func(reviewID uuid.UUID, category, target string) {
		mustExec(ctx, t, pool,
			`INSERT INTO recommendation_dispositions
			   (review_id, category, target, status, rationale_hash, set_by_user_id)
			 VALUES ($1, $2, $3, 'done', 'h', $4)`, reviewID, category, target, owner)
	}
	disposeDismissed := func(reviewID uuid.UUID, category, target string) {
		mustExec(ctx, t, pool,
			`INSERT INTO recommendation_dispositions
			   (review_id, category, target, status, dismiss_reason, rationale_hash, set_by_user_id)
			 VALUES ($1, $2, $3, 'dismissed', 'not_an_issue', 'h', $4)`, reviewID, category, target, owner)
	}

	// WHAT THESE PAIRS SHARE — asked and answered deliberately, because the sibling fixture
	// next door got exactly this wrong twice, one level down each time. Derived by READING
	// the fixture against the query, not measured.
	//
	// Shared ON PURPOSE, and load-bearing: within a run the two coordinates share the review
	// (so `f.review_id = rv.id` cannot be what separates them — the whole point) and exactly
	// ONE coordinate half. Across runs everything shares one owner and one repo, which is
	// what keeps the read owner-scoped.
	//
	// Shared BY ACCIDENT, and NOT currently observable — flagged so the next author does not
	// inherit an inert fixture the way this branch already has:
	//   - every review carries verdict 'issues';
	//   - every recommendation carries confidence 'high';
	//   - every run carries status 'completed'.
	// None of those three is projected by THIS query, so nothing here is weakened today. But
	// the moment a column is added to the SELECT, a fold of it would be invisible on this
	// fixture. Vary the value before you pin a new column, not after.
	//
	// Deliberately NOT uniform, because these ARE projected: the two dispositions differ
	// ('done' vs 'dismissed', pinned in assertion 6), the filed and disposed coordinates sit
	// in different runs, and each recommendation carries its own rationale text.
	runFT, revFT := newRun()
	rec(revFT, catA, tgt1)
	rec(revFT, catA, tgt2)
	fileIt(revFT, catA, tgt1)

	runFC, revFC := newRun()
	rec(revFC, catA, tgt1)
	rec(revFC, catB, tgt1)
	fileIt(revFC, catA, tgt1)

	runDT, revDT := newRun()
	rec(revDT, catA, tgt1)
	rec(revDT, catA, tgt2)
	disposeDone(revDT, catA, tgt1)

	runDC, revDC := newRun()
	rec(revDC, catA, tgt1)
	rec(revDC, catB, tgt1)
	disposeDismissed(revDC, catA, tgt1)

	rows, err := q.ListJudgeTriageRowsForRuns(ctx, store.ListJudgeTriageRowsForRunsParams{
		UserID: owner,
		RunIds: []uuid.UUID{runFT, runFC, runDT, runDC},
		Lim:    500,
	})
	if err != nil {
		t.Fatalf("list triage rows: %v", err)
	}

	type tally struct {
		rows, filed, disposed int
		status                string // the one non-NULL disposition_status seen in this run
	}
	got := map[uuid.UUID]*tally{}
	for _, r := range rows {
		tl := got[r.RunID]
		if tl == nil {
			tl = &tally{}
			got[r.RunID] = tl
		}
		tl.rows++
		if r.FiledSettled {
			tl.filed++
		}
		if r.DispositionStatus.Valid {
			tl.disposed++
			tl.status = r.DispositionStatus.String
		}
	}
	at := func(name string, runID uuid.UUID) *tally {
		t.Helper()
		tl := got[runID]
		if tl == nil {
			t.Fatalf("run %s (%s) returned NO rows — the fixture or the run_ids filter is wrong, "+
				"so nothing below proves anything", name, runID)
		}
		return tl
	}

	// 0. run_id, and the no-fan-out contract. Each review holds exactly two recommendations
	// and both side joins are UNIQUE on the coordinate, so each run must come back with
	// exactly two rows. A cross-wired run_id collapses the four tallies into the wrong
	// buckets and every assertion below becomes meaningless, so this is checked first.
	for name, runID := range map[string]uuid.UUID{"FT": runFT, "FC": runFC, "DT": runDT, "DC": runDC} {
		if n := at(name, runID).rows; n != 2 {
			t.Errorf("run %s returned %d rows, want 2 — either run_id is not projecting this "+
				"review's own run, or a side join fanned out", name, n)
		}
	}

	// 1. THE FILED JOIN'S TARGET HALF. runFT holds (catA,tgt1) filed and (catA,tgt2) unfiled:
	// same category, different target. Dropping `AND f.target = rr.target` makes tgt2 inherit
	// tgt1's filed issue, and both rows read settled.
	if n := at("FT", runFT).filed; n != 1 {
		t.Errorf("runFT: %d of 2 coordinates read filed_settled, want exactly 1 — the filed join's "+
			"TARGET half is gone, so every coordinate sharing a category with a filed one reads as "+
			"filed and drops off the todo rung", n)
	}

	// 2. THE FILED JOIN'S CATEGORY HALF. runFC holds (catA,tgt1) filed and (catB,tgt1) unfiled:
	// same target, different category. This is the half the big test above could not see until
	// its own fourth coordinate was added.
	if n := at("FC", runFC).filed; n != 1 {
		t.Errorf("runFC: %d of 2 coordinates read filed_settled, want exactly 1 — the filed join's "+
			"CATEGORY half is gone, so filing under one category marks the SAME target filed under "+
			"every other category", n)
	}

	// 3. THE DISPOSITION JOIN'S TARGET HALF. runDT holds (catA,tgt1) marked done and
	// (catA,tgt2) undisposed.
	if n := at("DT", runDT).disposed; n != 1 {
		t.Errorf("runDT: %d of 2 coordinates carry a disposition, want exactly 1 — the disposition "+
			"join's TARGET half is gone, so resolving one coordinate silently resolves every other "+
			"one in the review that shares its category", n)
	}

	// 4. THE DISPOSITION JOIN'S CATEGORY HALF. runDC holds (catA,tgt1) dismissed and
	// (catB,tgt1) undisposed.
	if n := at("DC", runDC).disposed; n != 1 {
		t.Errorf("runDC: %d of 2 coordinates carry a disposition, want exactly 1 — the disposition "+
			"join's CATEGORY half is gone, so dismissing one category's coordinate silently "+
			"dismisses the same target under every other category", n)
	}

	// 5. The columns must stay in their own lanes. A filed row is not a disposition and vice
	// versa: without these, a join collapsed so far that BOTH side tables attach everywhere
	// could still satisfy the four counts above.
	if n := at("FT", runFT).disposed + at("FC", runFC).disposed; n != 0 {
		t.Errorf("the two filed-only runs report %d dispositions, want 0 — disposition_status is "+
			"not reading the dispositions table for this coordinate", n)
	}
	if n := at("DT", runDT).filed + at("DC", runDC).filed; n != 0 {
		t.Errorf("the two disposition-only runs report %d settled filed links, want 0 — "+
			"filed_settled is not reading the filed table for this coordinate", n)
	}

	// 6. disposition_status's VALUE, not just its nullness. Found by asking the auditor's
	// question of this fixture — "do the pairs share anything you did not intend?" — and the
	// answer was yes, in my own assertions: everything above counts `.Valid`, so folding
	// `d.status` to a non-NULL constant would have left all five green.
	//
	// It is not inert. BucketOf switches on the STRING: an unrecognised value falls through
	// both cases, filed_settled is false for these runs, and the coordinate comes back
	// "todo" — so a settled recommendation is COUNTED as outstanding and the /runs badge
	// OVER-counts. That is the set_via class again: a value fold that is not fail-safe.
	//
	// DT is marked done and DC dismissed, so this is a pairwise check — it fails under a fold
	// to ANY single constant, whichever one is chosen, rather than only to a value the
	// fixture does not contain.
	dt, dc := at("DT", runDT), at("DC", runDC)
	if dt.status == dc.status {
		t.Errorf("both disposed runs report disposition_status %q — the column is folded to a "+
			"constant, and BucketOf reads the string, so an unrecognised one buckets a SETTLED "+
			"coordinate as todo and the badge over-counts", dt.status)
	}
	if dt.status != "done" {
		t.Errorf("runDT disposition_status = %q, want done — the value written by the fixture",
			dt.status)
	}
	if dc.status != "dismissed" {
		t.Errorf("runDC disposition_status = %q, want dismissed", dc.status)
	}
}
