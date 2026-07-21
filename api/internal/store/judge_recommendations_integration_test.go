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
	for _, r := range rows {
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
	if hand.FiledIssueIid.Valid || hand.FiledIssueUrl.Valid {
		t.Errorf("the UNFILED row carries a filed link (iid=%v url=%q) — the columns are not row-scoped",
			hand.FiledIssueIid.Int64, hand.FiledIssueUrl.String)
	}
	// And filed_at drives filed_settled, so a settled link must read as settled.
	if !auto.FiledSettled {
		t.Error("the filed+settled row reports filed_settled=false")
	}
	if hand.FiledSettled {
		t.Error("the unfiled row reports filed_settled=true")
	}
}
