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
	// MEASURED, one fold per run against a FRESH database, RE-DERIVED IN FULL at `ad6c63d9`
	// (2026-07-21) rather than carried forward — FIVE migrations from main landed ahead of PRD
	// #98's own, whose file was renumbered 00075 -> 00081 with identical content, so every
	// earlier fold on this fixture had run against DDL the suite no longer applies.
	//
	// THE COUNT TOOK THREE ATTEMPTS AND THE ERROR IS THE SAME ONE EACH TIME: a count taken off a
	// diff without asking what each line MEANS. This comment first said "four new migrations
	// (00078-00081)" — wrong twice over, counting 00081 (which is ours) and dropping 00075 and
	// 00077. The correction to it said "six migration files changed" — true of the DIFF, and
	// still wrong for the sentence, because one of the six is our own file being renamed.
	// `git diff --name-status -M 31080a40..8515cfab -- api/internal/store/migrations/` separates
	// them in one flag: five `A` lines, one `R100`. A count over a diff is a claim about CHANGE;
	// only a count over `A` lines is a claim about ARRIVAL, and "landed ahead of ours" is an
	// arrival claim. A pure renumber applies identical DDL, so the schema moved by exactly five. Each line mutates ListJudgeRecommendationRowsForUser — THE
	// FIRST QUERY BODY in judge_recommendations.sql, at its lines 61, 64, 69 and 71 —
	// regenerated through sqlc, `go vet` clean, the change confirmed present in BOTH the .sql
	// and the .sql.go by `git diff --numstat` before each run and gone after. Every run
	// carried its own positive control: RUN=1, SKIP=0, and the named test appearing as
	// --- PASS / --- FAIL. Each line names the assertion that caught it, by MESSAGE:
	//
	//   drop `AND f.target = rr.target`      RED  — "sharing autoRev AND its category" (the
	//                                              sibling below), and that one ONLY
	//   drop `AND f.category = rr.category`  RED  — "sharing autoRev AND its target" (the
	//                                              cross-category coordinate), that one ONLY
	//   drop BOTH coordinate halves          RED  — "two backlog rows share the coordinate":
	//                                              the DUPLICATE-COORDINATE Fatalf at the map
	//                                              build, NOT either assertion above. With
	//                                              both halves gone the join runs on review_id
	//                                              alone, autoRev holds two filed rows, and
	//                                              every autoRev coordinate matches both — a
	//                                              fan-out, which fatals before any filed-link
	//                                              assertion executes. Recorded because the
	//                                              previous version of this table claimed
	//                                              "both of the above fire together", and they
	//                                              do not: neither runs.
	//   drop `AND d.target = rr.target`      RED  — "the undisposed sibling inherited a
	//                                              disposition", AND the cross-review one. That
	//                                              second message USED TO blame the REVIEW_ID
	//                                              half, which this fold never touches; it now
	//                                              names both candidates and points at the
	//                                              discriminator below.
	//   drop `AND d.review_id = rv.id`       RED  — the cross-review disposition assertion, and
	//                                              THAT ONE ONLY. Measured, not reasoned: the
	//                                              sibling stays clean because it shares autoRev
	//                                              and needs no cross-review match. So the pair
	//                                              is separable — target reddens BOTH, review_id
	//                                              reddens ONE — which is exactly what lets that
	//                                              assertion's message stop guessing. (The
	//                                              inherited values differ too: `issue_close`/
	//                                              `done` from autoRev under this fold, versus
	//                                              ``/`dismissed` from handRev's own under the
	//                                              target fold.)
	//   drop `AND d.category = rr.category`  RED  — "the cross-category coordinate inherited a
	//                                              disposition", that one ONLY
	//   `f.filed_at` -> `d.set_at`           RED  — "carries filed_at …", that one ONLY
	//   `(f.filed_at IS NOT NULL)::bool`
	//     -> `(f.id IS NOT NULL)::bool`      RED  — "a coordinate that is only CLAIMED …", the
	//                                              claimed coordinate's assertion. This one was
	//                                              ASSERTED by the claim-row comment below and
	//                                              had never been folded on this body; it is
	//                                              measured now.
	//
	// SCOPE — READ THIS BEFORE CONCLUDING "THE COORDINATE PREDICATE IS PINNED". The SAME two
	// three-part joins appear a SECOND time in that file, on ListJudgeTriageRowsForRuns (M4's
	// /runs badge count, lines 144 and 146). Nothing above touches it — PROVIDED YOU MUTATE
	// LINE 71 (or 69), NOT EVERY MATCH. The join text is BYTE-IDENTICAL between the two
	// bodies, so a plain `sed` for `AND f.target = rr.target` changes FOUR lines rather than
	// two and silently folds body 2 as well; a reproducer doing that would conclude the table
	// covered both bodies. Address the line by NUMBER and assert the changed-line count.
	// (The reviewer's reproduction hit exactly this on all eleven of its runs.) The second
	// body was measured entirely
	// unpinned: dropping BOTH coordinate halves off its `f` join, and separately off its `d`
	// join, each left the ENTIRE live-DB suite green.
	// TestJudgeRunTodoTriageRowsAreCoordinateScopedLiveDB at the end of this file now covers
	// it, and its folds HAVE since been run — this sentence used to end "its folds are still
	// owed", which stopped being true when they landed and which nothing would ever have
	// flagged. Read its own block for the results rather than this pointer to it.
	//
	// PROVENANCE OF THE "was GREEN" CLAIMS, because they are not all the same strength: the
	// category-half GREEN was measured here, on this fixture with the sibling but before the
	// fourth coordinate. The target-half and both-halves GREENs are INHERITED from the M3
	// checkpoint's earlier sweep and were not re-run against the pre-fix tree.
	//
	// WHY THE TABLE ABOVE WAS RE-DERIVED RATHER THAN TRUSTED, since it had been measured
	// carefully twice: a fixture row added AFTER it — the claimed coordinate, which arrived in a
	// later commit for an unrelated property — silently moved where two of these folds land.
	// Nothing in the table's own text was edited when that row arrived, and nothing could have
	// noticed: the folds still went RED, so no gate could see that the assertion being credited
	// had stopped executing. That is the whole argument for re-folding rather than inheriting.
	// A fold result is a claim about a fixture AND a schema, and both move underneath it.
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
	// SAME target under every other category as filed: a wrong `filed` bucket and a wrong
	// "filed as #IID" chip.
	//
	// 🔴 CORRECTION — the commit message of 45381961 states a THIRD consequence, "and an M6
	// close edge attributed to a coordinate nobody filed". THAT IS FALSE and it cannot be
	// amended out of a dispatched commit, so the correction lives here. ListFiledIssueCloseEdges
	// (judge_issue_close.sql) starts FROM recommendation_filed_issues and correlates rr on all
	// three of review_id, category and target inside its own scalar subquery — it does not
	// read this query at all, so no fold of judge_recommendations.sql can reach the close-edge
	// path. Verified at source, not reasoned. The first two consequences are confirmed.
	//
	// Note the shape, because it is the branch's signature failure: two true clauses and one
	// false one in a single sentence, and the false one rode in on the credibility of the
	// other two. Apply the screen PER CLAIM, not per comment block.
	const otherCat = "improve_uzi"
	mustExec(ctx, t, pool,
		`INSERT INTO review_recommendations (review_id, category, target, rationale_md, confidence)
		 VALUES ($1, $2, $3, 'because', 'medium')`, autoRev.reviewID, otherCat, autoTarget)

	// A FIFTH coordinate: autoRev's filed coordinate EXACTLY — same category, same target —
	// but in handRev. This is the REVIEW_ID half of both joins, and it is the tenant boundary.
	//
	// Neither side table has a tenant column: recommendation_filed_issues (00071) and
	// recommendation_dispositions (00073) carry only filed_by_user_id / set_by_user_id, which
	// are ON DELETE SET NULL attribution pointers — nullable, and nulled when a user is
	// deleted — not ownership. Ownership reaches those tables ONLY through
	// review_id -> run_reviews.user_id, and the query's `WHERE rv.user_id = @user_id` scopes
	// `rv`, not the joined side table. So the category and target halves keep a caller inside
	// their own data; THE REVIEW_ID HALF IS THE ONLY THING KEEPING THEM OUT OF EVERYONE ELSE'S.
	// Coordinates are shared across users BY DESIGN — that recurrence is this PRD's premise —
	// so a real second user filing the same coordinate is the ordinary case, not a rare one.
	//
	// 🟢 THE PRODUCTION CODE IS CORRECT. All three predicates are present and right in both
	// query bodies. This is a TEST-COVERAGE gap: before this row, dropping either
	// `f.review_id = rv.id` or `d.review_id = rv.id` left the ENTIRE live-DB suite green, so
	// the suite could not observe a break in the one predicate carrying the tenant boundary.
	// Nothing here was ever shipped leaking; do not read it as a live vulnerability.
	//
	// Same review as handRev deliberately: handRev is the caller's OWN other review, which
	// makes this a within-tenant proxy for the cross-tenant break. A cross-USER fixture is
	// what the auditor used to demonstrate the severity; this one pins the predicate that
	// prevents it, without needing a second user in this test's fixture.
	crossReviewTarget := autoTarget
	mustExec(ctx, t, pool,
		`INSERT INTO review_recommendations (review_id, category, target, rationale_md, confidence)
		 VALUES ($1, $2, $3, 'because', 'medium')`, handRev.reviewID, cat, crossReviewTarget)

	// A SIXTH: a CLAIMED-but-not-filed coordinate in autoRev. The claim-first flow (PRD #68
	// Decision 7, 00071) INSERTs the row with filing_since stamped and filed_* still NULL,
	// then SettleRecommendationFiledIssue stamps filed_* on forge success. So a row that
	// EXISTS but is not settled is a documented, reachable, in-flight state.
	//
	// 🔴 IT SITS UNDER ITS OWN THIRD CATEGORY, AND THAT IS LOAD-BEARING — DO NOT MOVE IT INTO
	// cat OR otherCat. It carries the fixture's SECOND recommendation_filed_issues row, and
	// keeping that row in a category no other coordinate uses is what keeps the two filed rows
	// from colliding under a folded join. Measured 2026-07-21 at this tree, one fold per run
	// against a fresh database; both alternatives were run, not reasoned:
	//
	//   claim under cat (where it was):  `drop AND f.target = rr.target` reddens at the
	//     DUPLICATE-COORDINATE t.Fatalf, not at the sibling's filed-link assertion below. Two f
	//     rows then share (autoRev, cat), so the fold leaves the join running on the non-unique
	//     (review_id, category) prefix, rg-auto matches BOTH, and the map build fatals with
	//     "the fixture is ambiguous" — a message that blames the FIXTURE for a broken production
	//     join, while the assertion written for that fold never executes. A red naming the wrong
	//     thing is not the same pin, and an assertion behind an earlier Fatalf is documentation
	//     rather than a gate.
	//   claim under otherCat (which IS `improve_uzi` — see the const below; these are two
	//     objections to ONE option, not two options):  the fold reaches the sibling's assertion,
	//     but ALSO reddens the
	//     cross-category one — whose message then blames the CATEGORY half, which was never
	//     mutated. Not a false positive: the cross-category coordinate really does inherit this
	//     claim row under that fold. The mechanism is filed_issue_url being `NOT NULL DEFAULT ''`
	//     (00071:43), so `FiledIssueUrl.Valid` is TRUE for a bare claim row and that assertion
	//     therefore detects ANY f match rather than a settled one. Same lesson as the cast fold
	//     recorded further down: a fold that reddens a spread tells you less than one that
	//     reddens the right assertion.
	//
	// A third category makes the target-half fold reach exactly the sibling's assertion and
	// nothing else. Its own purpose is unaffected: it pins filed_settled's SOURCE, which is
	// about filed_at vs row-existence and never about the coordinate.
	//
	// A SECOND, INDEPENDENT reason for the same verdict — not a third candidate. otherCat IS
	// improve_uzi, so this reinforces the rejection above rather than ruling out a new option:
	// improve_uzi is the one category with a second, table-wide consumer
	// (ListOpenImproveUziRecommendations, selfimprove.sql), so an open improve_uzi row here can
	// fail an M6 test in another package, as the badge fixture at the end of this file records.
	//
	// It pins the derived boolean's SOURCE. `filed_settled` is
	// `(f.filed_at IS NOT NULL)::bool`, and the natural wrong implementation is "did the LEFT
	// JOIN match?" — `(f.id IS NOT NULL)` or `(f.review_id IS NOT NULL)`. Every OTHER f row in
	// this fixture is fully settled, so that fold was invisible: row-exists and filed-at-set
	// agreed everywhere. With a live claim they disagree, and the production effect is real —
	// a coordinate mid-filing would read as `filed`, drop out of `todo`, and show a "filed"
	// chip for an issue that does not exist yet.
	//
	// NOT pinned, and deliberately so: folding `f.filed_at` to `f.filed_issue_iid` stays
	// invisible and NO legitimate fixture can catch it. The sole writer stamps
	// filed_repo_id/filed_issue_iid/filed_issue_url/filed_at in ONE UPDATE and the claim
	// INSERT sets none of them, so the two columns are always both NULL or both set. The
	// projection is correct only BECAUSE of that writer invariant — an instance of "state the
	// invariant where it is enforced" — and a fixture contriving filed_at NULL with a non-NULL
	// iid would pin an unreachable state. Named here so the next reader does not go build it.
	const claimCat = "adjust_template"
	const claimedTarget = "rg-mid-filing"
	mustExec(ctx, t, pool,
		`INSERT INTO review_recommendations (review_id, category, target, rationale_md, confidence)
		 VALUES ($1, $2, $3, 'because', 'medium')`, autoRev.reviewID, claimCat, claimedTarget)
	mustExec(ctx, t, pool,
		`INSERT INTO recommendation_filed_issues (id, review_id, category, target, filed_by_user_id, filing_since)
		 VALUES ($1, $2, $3, $4, $5, now())`,
		uuid.New(), autoRev.reviewID, claimCat, claimedTarget, owner)

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
	// KEYED ON THE FULL COORDINATE — review AND category AND target. It used to be two maps,
	// one keyed on target alone and one on (category, target), and both were a latent trap
	// rather than a convenience: the moment a fixture holds the SAME coordinate in two
	// reviews — which is the whole premise of this PRD, and which the cross-review row below
	// deliberately creates — the second row silently OVERWRITES the first and every assertion
	// built on the map quietly changes what it is about. Flagged independently by both
	// validators. Keying on the review removes the class instead of dodging the instance.
	type coord struct {
		review           uuid.UUID
		category, target string
	}
	byCoord := map[coord]store.ListJudgeRecommendationRowsForUserRow{}
	for _, r := range rows {
		k := coord{r.ReviewID, r.Category, r.Target}
		if prev, dup := byCoord[k]; dup {
			t.Fatalf("two backlog rows share the coordinate %s/%s/%s (rec_ids %s and %s) — the "+
				"fixture is ambiguous and every lookup below would be arbitrary",
				r.ReviewID, r.Category, r.Target, prev.RecID, r.RecID)
		}
		byCoord[k] = r
	}
	at := func(what string, review uuid.UUID, category, target string) store.ListJudgeRecommendationRowsForUserRow {
		t.Helper()
		r, ok := byCoord[coord{review, category, target}]
		if !ok {
			t.Fatalf("no backlog row for %s (%s/%s in review %s)", what, category, target, review)
		}
		return r
	}
	auto := at("autoRev's filed coordinate", autoRev.reviewID, cat, autoTarget)
	hand := at("handRev's hand-dismissed coordinate", handRev.reviewID, cat, handTarget)

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
		t.Errorf("a coordinate with NO filed row anywhere carries a filed link (iid=%v urlValid=%v url=%q) — "+
			"the projection is not reading the joined filed row (a constant fold, or a join that "+
			"matches across reviews)",
			hand.FiledIssueIid.Int64, hand.FiledIssueUrl.Valid, hand.FiledIssueUrl.String)
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
	sibling := at("the unfiled sibling", autoRev.reviewID, cat, unfiledInAutoRev)
	if sibling.FiledSettled || sibling.FiledAt.Valid || sibling.FiledIssueIid.Valid || sibling.FiledIssueUrl.Valid {
		t.Errorf("an unfiled coordinate sharing autoRev AND its category with the filed one inherited "+
			"its link (settled=%v at=%v iid=%v urlValid=%v url=%q) — the filed join's TARGET half is gone, so "+
			"coordinate in a review sharing a category with any filed issue reads as filed",
			sibling.FiledSettled, sibling.FiledAt.Valid, sibling.FiledIssueIid.Valid, sibling.FiledIssueUrl.Valid,
			sibling.FiledIssueUrl.String)
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
	// PRINT `.Valid`, NOT THE STRING, FOR THE URL — the condition tests Valid and the string is
	// not the same fact. filed_issue_url is `NOT NULL DEFAULT ''` (00071:43), so a matched row
	// that is merely CLAIMED carries Valid=true with String="". The old message printed
	// `url=""` there, which reads as "no url inherited" at exactly the moment a url column IS
	// what fired — a reader debugging that red goes looking in the wrong place. Same defect the
	// cross-review disposition message carried, and the same one the fold table records: a
	// diagnostic must report the condition it evaluated, not a neighbouring value.
	crossCat := at("the cross-category coordinate", autoRev.reviewID, otherCat, autoTarget)
	if crossCat.FiledSettled || crossCat.FiledAt.Valid || crossCat.FiledIssueIid.Valid || crossCat.FiledIssueUrl.Valid {
		t.Errorf("an unfiled coordinate sharing autoRev AND its target with the filed one inherited "+
			"its link (settled=%v at=%v iid=%v urlValid=%v url=%q) — the filed join's CATEGORY half "+
			"is gone, so filing under one category marks the SAME target filed under every other "+
			"category. NOTE urlValid can be true with url empty: filed_issue_url is NOT NULL "+
			"DEFAULT '', so a merely-CLAIMED row still satisfies it",
			crossCat.FiledSettled, crossCat.FiledAt.Valid, crossCat.FiledIssueIid.Valid,
			crossCat.FiledIssueUrl.Valid, crossCat.FiledIssueUrl.String)
	}
	// THE REVIEW_ID HALF — the tenant boundary. Same category AND same target as autoRev's
	// filed+disposed coordinate, in a DIFFERENT review. The category and target halves cannot
	// separate these two rows: only `f.review_id = rv.id` / `d.review_id = rv.id` can. Before
	// this row, dropping either left the whole live-DB suite green.
	//
	// Ownership reaches both side tables ONLY through review_id, so this is the predicate that
	// keeps one caller's backlog out of every other caller's filed issues and dispositions.
	// The production code is correct; this closes the hole in the SUITE's ability to see it.
	crossReview := at("the cross-review coordinate", handRev.reviewID, cat, crossReviewTarget)
	if crossReview.FiledSettled || crossReview.FiledAt.Valid || crossReview.FiledIssueIid.Valid || crossReview.FiledIssueUrl.Valid {
		t.Errorf("a coordinate identical to autoRev's filed one but in ANOTHER review inherited its "+
			"filed link (settled=%v at=%v iid=%v urlValid=%v url=%q) — the filed join's REVIEW_ID half is gone. "+
			"That half is the ONLY tenant boundary on recommendation_filed_issues, which has no "+
			"owner column of its own, so one user's filed issue would surface in another's backlog",
			crossReview.FiledSettled, crossReview.FiledAt.Valid, crossReview.FiledIssueIid.Valid,
			crossReview.FiledIssueUrl.Valid, crossReview.FiledIssueUrl.String)
	}
	// 🔴 THIS MESSAGE MUST NOT NAME A SINGLE HALF, and the reason is measured rather than
	// cautious. It used to end "the disposition join's REVIEW_ID half is gone" — and dropping
	// `AND d.target = rr.target` fires it, with review_id untouched. The detection is right and
	// that diagnosis was wrong: handRev owns its OWN disposition (on cat/rg-hand), so once the
	// target half goes, this coordinate inherits it WITHOUT any cross-review match. A reader
	// sent to the tenant boundary would be debugging a predicate nobody had touched.
	//
	// The discriminator is the sibling assertion above, which is why the message points at it
	// instead of guessing: the target half reddens BOTH; the review_id half reddens only this
	// one, because the sibling shares autoRev and needs no cross-review match to stay clean.
	//
	// Its filed-link twin above is deliberately left naming REVIEW_ID: handRev owns no filed
	// row at all, so no coordinate-half fold can reach it and review_id really is the only
	// explanation — verified, not assumed, across all six join folds. That asymmetry is the
	// whole lesson: the same sentence is precise on one join and false on the other, and only
	// the fixture says which.
	if crossReview.SetVia.Valid || crossReview.DispositionStatus.Valid {
		t.Errorf("a coordinate identical to autoRev's disposed one but in ANOTHER review inherited a "+
			"disposition (set_via=%q status=%q) — the disposition join is matching across a boundary it "+
			"must not. EITHER the REVIEW_ID half is gone (the ONLY tenant boundary on "+
			"recommendation_dispositions, so another user settling their copy of a shared coordinate "+
			"would drop this one out of todo) OR the TARGET half is, which lets handRev's own "+
			"disposition reach this coordinate without leaving the review. Check the undisposed "+
			"sibling's assertion to tell them apart: the target half reddens both, review_id only this",
			crossReview.SetVia.String, crossReview.DispositionStatus.String)
	}

	// THE DERIVED BOOLEAN'S SOURCE. A claimed-but-not-filed coordinate HAS an f row, so
	// "the LEFT JOIN matched" and "filed_at is set" disagree here and nowhere else in this
	// fixture. filed_settled must follow filed_at, not row existence.
	claimed := at("the claimed-but-not-filed coordinate", autoRev.reviewID, claimCat, claimedTarget)
	if claimed.FiledSettled {
		t.Error("a coordinate that is only CLAIMED (filing_since set, filed_at still NULL) reports " +
			"filed_settled=true — filed_settled is testing whether the filed row EXISTS rather than " +
			"whether it is settled, so a coordinate mid-filing reads as filed and drops out of todo")
	}
	if claimed.FiledAt.Valid {
		t.Errorf("the claimed coordinate carries filed_at %v, but the claim INSERT never sets it",
			claimed.FiledAt.Time)
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
	// CHOOSING THE FOLD FOR THIS ASSERTION — two corrections deep, and each correction had the
	// conclusion right and the mechanism wrong, which is the harder failure to catch.
	//
	// `f.filed_at -> now()` does not build. The FIRST version of this comment credited that
	// fold with a result it could not have produced. The SECOND blamed nullability: "sqlc
	// types the folded column NOT NULL". That is also wrong, and it is disproved by one
	// experiment — `now()::timestamptz` is EQUALLY not-null and it COMPILES
	// (`FiledAt pgtype.Timestamptz`), while bare `now()` yields `interface{}`. So nullability
	// is not the differentiator: THE MISSING EXPLICIT CAST IS. sqlc falls back to
	// `interface{}` for a bare function expression it cannot resolve. The obvious fold is not
	// unusable — it is one cast away from usable, and a reader sent to look at nullability
	// finds nothing there.
	//
	// `d.set_at` is still the right choice, for a reason neither earlier version gave. It is
	// not that it "preserves the type" — the cast does that too. IT IS SELECTIVE. Measured by
	// the reviewer and re-run here, one fold per run against a fresh database:
	//
	//   f.filed_at -> now()::timestamptz   RED at this assertion AND at every other one that
	//                                      ORs `FiledAt.Valid` in with other fields — the
	//                                      sibling, cross-category, cross-review and claimed
	//                                      coordinates. Several of those messages then blame
	//                                      a join half that was never mutated, and the
	//                                      giveaway is in their own output:
	//                                      settled=false at=true iid=false url="" — only the
	//                                      timestamp moved.
	//   f.filed_at -> d.set_at             RED at this assertion ONLY
	//
	// `now()::timestamptz` is non-NULL for EVERY row; `d.set_at` is NULL exactly where the
	// disposition join did not match. So the neighbour column is SELECTIVE and the cast is
	// not, and a fold that reddens a spread of assertions tells you less than one that
	// reddens the right one — worse, it manufactures several confidently-wrong diagnoses.
	//
	// CITE THE SHAPE, NOT THE TALLY. An earlier version of this comment said "three
	// assertions", a later one "five"; both were right for their own tree, because the
	// fixture gained assertions in between. An assertion COUNT drifts exactly like a line
	// number.
	//
	// THE GENERAL RULE IS IN CLAUDE.md's api section — read it there rather than trusting a
	// restatement here, because this paragraph has now been wrong THREE times and each version
	// sounded more authoritative than the last. It credited the wrong fold, then blamed
	// nullability, then claimed "any expression with an explicit cast" works — which is also
	// false: `'x'::text` has a cast and does NOT compile, because sqlc types a literal as NOT
	// NULL cast or no cast, while it types a function result as nullable. The reliable shape
	// is ANOTHER NULLABLE COLUMN OFF THE SAME LEFT JOIN, and it is also the SELECTIVE one for
	// the reason above. Everything else must be COMPILED before it is believed — which is the
	// actual rule, and the one this comment's own history is the argument for.
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
// The pairs are deliberately NOT uniform. A fixture whose rows all look alike is what made
// both of the holes this branch just closed invisible, twice, one level down each time.
//
// ✅ MEASURED 2026-07-21. Every fold in the table below is of THIS query body, one mutation
// per run, each against a FRESH database. (The count is deliberately not stated: an earlier
// version of this line said "Seven folds" above a table of EIGHT rows, which is the same
// defect as a bare suite tally — the rows are the claim, a number over them is a second,
// unchecked copy of it.) Every run additionally asserted the POSITIVE CONTROL — that this test
// actually appeared as RUN and PASS/FAIL, and that the suite's SKIP count was 0 — because a
// live-DB test that silently skips looks exactly like a green. Every mutation was confirmed
// present in BOTH the .sql and the regenerated .sql.go before the run and gone from both
// after, with `sqlc generate` producing a zero diff as the proof.
//
//	fold                                          result
//	drop `f.target = rr.target`                   RED at the runFT assertion
//	drop `f.category = rr.category`               RED at the runFC assertion
//	drop `d.target = rr.target`                   RED at the runDT assertion
//	drop `d.category = rr.category`               RED at the runDC assertion
//	drop `f.review_id = rv.id`                    RED — row-count fan-out, then more
//	drop `d.review_id = rv.id`                    RED — row-count fan-out, then more
//	`d.status` -> `d.dismiss_reason`              RED at the runDT/runDC VALUE assertions
//	`(f.filed_at ...)` -> `(f.id IS NOT NULL)`    RED at the runCL claimed assertion
//
// 🟡 BOUND, because the table above is the strongest-looking artifact in this file and its
// binding is the thing a reader will not check: it was measured on the BRANCH, before the
// landing merge put five migrations from main ahead of ours. It has NOT been re-run against the
// merged schema. The sibling table on the M1 body was re-derived at `ad6c63d9` for exactly
// that reason and every fold still reddened — which is evidence that re-folding is worth the
// minutes, NOT evidence that this one is fine. Re-fold before citing it.
//
// 🔴 THE TWO review_id RESULTS ARE AN ACCIDENT OF THIS FIXTURE — DO NOT TIDY IT AWAY. They
// redden only because runFT/runFC both hold a filed row on the SAME coordinate (catA,tgt1),
// and runDT/runDC both hold a disposition on it, so dropping a review_id half lets one
// recommendation match several side rows and the join FANS OUT — caught by the row-count
// assertion, not by any coverage designed for it. The query's own comment claims "neither
// side-table join can fan out: both are UNIQUE on the coordinate", which is true ONLY because
// the join uses all three columns of that unique key; drop review_id and it runs on a
// NON-UNIQUE prefix. Giving each run its own coordinates would look like tidying and would
// SILENTLY DELETE this coverage.
//
// MUTATIONS MUST BE BODY-SCOPED, and this is not a formality: the two joins are BYTE-
// IDENTICAL between this body and ListJudgeRecommendationRowsForUser above, so a text-based
// edit silently hits FOUR lines where two were intended and a "body 2" fold reddens body-1
// assertions you then misattribute. Address the line by NUMBER and assert the changed-line
// count, not merely that the text changed.
//
// TWO FOLDS THAT DO NOT WORK HERE, recorded so nobody re-derives them: `d.status` to a
// string constant, and `f.filed_at` to `now()`. Both make sqlc type the column NOT NULL, the
// generated struct field loses its pgtype wrapper, and the package stops COMPILING. That is
// loud, but a build error is not a red assertion — the test never runs. Use a nullable
// cross-wire off the other LEFT JOIN instead; it preserves the type and looks like data.
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
		// NOT improve_uzi, and that is load-bearing rather than arbitrary. improve_uzi is the
		// ONE category with a second consumer: ListOpenImproveUziRecommendations
		// (selfimprove.sql) selects `WHERE rr.category = 'improve_uzi'` across the WHOLE
		// TABLE — no user scope, no review scope — and TestFiledIssueCloseAutoDonesOnceLiveDB
		// asserts on its result filtering only by TARGET. Seeding an open improve_uzi row on
		// target 'rg' here therefore fails an M6 test in a different package, from a fixture
		// that has nothing to do with it. Measured: it did, on the first baseline run of this
		// fixture. Any inert category serves this test, so use one.
		catB = "adjust_template"
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
	// ONE coordinate half.
	//
	// Shared BY ACCIDENT, and NOT currently observable — flagged so the next author does not
	// inherit an inert fixture the way this branch already has:
	//   - ONE OWNER, ONE REPO across every run. This entry was originally filed under
	//     "on purpose", worded as "which is what keeps the read owner-scoped". THAT WAS
	//     BACKWARDS: a single-owner fixture does not keep the read owner-scoped, it makes
	//     `WHERE rv.user_id = @user_id` UNOBSERVABLE — there is no other user's row for a
	//     broken predicate to return. Unlike the rest of this list it is not
	//     hypothetical-on-a-future-column: the predicate exists today and the query's own
	//     comment states a security claim about it. Closed below by the foreign-run assertion.
	//   - `AND rv.target_run_id = ANY(@run_ids)`: every run in the fixture is passed in, so
	//     dropping the predicate returns the identical set. Same closure.
	//   - every review carries verdict 'issues';
	//   - every recommendation carries confidence 'high';
	//   - every run carries status 'completed';
	//   - `d.set_via` is NULL on every disposition — worth naming above the rest, because it
	//     is the column PRD #98 B3 added, its fold is one of the checkpoint's canonical
	//     examples, and it is exactly the kind of column someone adds to this projection next;
	//   - and, as one catch-all: `d.set_by_user_id`, `f.filed_by_user_id`, `f.filed_repo_id`,
	//     `rr.addressed_by_run_id` (NULL throughout), `runs.issue_description` ('d'), and
	//     every timestamp (all `now()`).
	// None of those is projected by THIS query, so nothing here is weakened today. But the
	// moment a column is added to the SELECT, a fold of it would be invisible on this
	// fixture. Vary the value before you pin a new column, not after.
	//
	// 🔴 ONE EXCEPTION, AND IT CUTS THE OTHER WAY: `d.rationale_hash` is uniform ('h' on both
	// disposed runs) and that uniformity is LOAD-BEARING — it is what makes the fifth fold
	// (`d.status -> d.rationale_hash`) collapse the pair and fire assertion 6. If you follow
	// the instruction above and vary it, you SILENTLY DISARM that fold. Re-derive the fold
	// rather than assuming it still discriminates. (The two instructions are not in conflict:
	// the sqlc rule constrains the fold's TYPE, this list constrains the fixture's VALUES.)
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

	// A fifth run for the DERIVED BOOLEAN'S SOURCE, the same hole closed in the big test
	// above. `filed_settled` is `(f.filed_at IS NOT NULL)::bool`; the natural wrong
	// implementation is "did the LEFT JOIN match?". Every filed row in the four runs above is
	// fully settled, so row-exists and filed-at-set agree everywhere and that fold is
	// invisible. A CLAIMED coordinate — PRD #68's claim-first INSERT stamps filing_since with
	// filed_* still NULL — is the state where they disagree, and it is reachable, not
	// contrived. Effect on THIS query: a coordinate mid-filing would leave the todo rung and
	// the badge would under-count while the forge issue does not yet exist.
	runCL, revCL := newRun()
	rec(revCL, catA, tgt1)
	mustExec(ctx, t, pool,
		`INSERT INTO recommendation_filed_issues (id, review_id, category, target, filed_by_user_id, filing_since)
		 VALUES ($1, $2, $3, $4, $5, now())`,
		uuid.New(), revCL, catA, tgt1, owner)

	// A SIXTH run belonging to a DIFFERENT USER, passed in @run_ids as if spoofed. This closes
	// the two live predicates the inventory above names as unobservable: `WHERE rv.user_id =
	// @user_id` and `AND rv.target_run_id = ANY(@run_ids)`. Until now every run in the fixture
	// belonged to the caller AND was passed in, so dropping either predicate returned the
	// identical set and every assertion passed.
	//
	// The query's own comment makes this an explicit security claim: "Owner-scoped by
	// rv.user_id, so the caller's own page can never surface another user's recommendation
	// counts even if a run id were somehow spoofed into the list." That is the exact sentence
	// this asserts. TestJudgeBacklogRunAnchorLiveDB already does the same for body 1; the
	// pattern was in this file and was not carried across.
	//
	// A separate user needs a separate connection and repo: repos.connection_id ->
	// forge_connections.user_id, so a user cannot borrow another's repo.
	stranger, strangerConn, strangerRepo := uuid.New(), uuid.New(), uuid.New()
	strangerRun, strangerRev := uuid.New(), uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		stranger, fmt.Sprintf("judgebadge-stranger-%s@e2e", stranger))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, strangerConn, stranger, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 2, 'g/stranger', 'https://forge.e2e/g/stranger', 'main', true)`,
		strangerRepo, strangerConn)
	mustExec(ctx, t, pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status)
		 VALUES ($1, $2, $3, 1, 'stranger run', 'd', 'completed')`, strangerRun, stranger, strangerRepo)
	mustExec(ctx, t, pool,
		`INSERT INTO run_reviews (id, target_run_id, user_id, verdict) VALUES ($1, $2, $3, 'issues')`,
		strangerRev, strangerRun, stranger)
	mustExec(ctx, t, pool,
		`INSERT INTO review_recommendations (review_id, category, target, rationale_md, confidence)
		 VALUES ($1, $2, $3, 'stranger rationale', 'high')`, strangerRev, catA, tgt1)

	// A SEVENTH run: the CALLER'S OWN, deliberately NOT passed in @run_ids. The stranger above
	// pins the OWNER predicate only — measured: dropping `AND rv.target_run_id = ANY(@run_ids)`
	// alone left the whole suite GREEN, because every one of the caller's runs was already
	// being requested, so an unfiltered read returned the identical set. The two predicates
	// need separate rows to be separately observable.
	//
	// Not cosmetic: this query is @lim-bounded, so a read that ignores the page returns every
	// run the caller has ever had and the LIMIT then cuts arbitrary rows — including rows for
	// runs that ARE on the requested page, which understates their badges. A wrong number, and
	// the query's own ORDER BY comment makes exactly this argument about truncation.
	unrequestedRun, unrequestedRev := newRun()
	rec(unrequestedRev, catA, tgt1)

	rows, err := q.ListJudgeTriageRowsForRuns(ctx, store.ListJudgeTriageRowsForRunsParams{
		UserID: owner,
		RunIds: []uuid.UUID{runFT, runFC, runDT, runDC, runCL, strangerRun},
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

	// EACH COUNT IS CHECKED IN BOTH DIRECTIONS, WITH A DIFFERENT MESSAGE FOR EACH, because
	// the two directions have different causes and a single "want exactly 1" message names
	// only one of them. Measured, and this is why the split exists: the `d.status ->
	// d.dismiss_reason` fold drives runDT's count to ZERO, and the original single message
	// blamed "the disposition join's TARGET half is gone" — which was not what broke. An
	// assertion whose message names a cause it did not observe is the same defect this file
	// already corrected twice for `hand`.
	//   too MANY  = a neighbour's row was inherited -> the named join half is gone.
	//   too FEW   = the row is not being read at all -> the projection, not the join.
	tooMany := "%s: %d of 2 coordinates %s, want exactly 1 — %s"
	tooFew := "%s: %d of 2 coordinates %s, want exactly 1 — the column is not reading the joined " +
		"row for a coordinate that HAS one, so this is the projection rather than the join half"

	// 1. THE FILED JOIN'S TARGET HALF. runFT holds (catA,tgt1) filed and (catA,tgt2) unfiled:
	// same category, different target. Dropping `AND f.target = rr.target` makes tgt2 inherit
	// tgt1's filed issue, and both rows read settled.
	if n := at("FT", runFT).filed; n > 1 {
		t.Errorf(tooMany, "runFT", n, "read filed_settled", "the filed join's TARGET half is gone, "+
			"so every coordinate sharing a category with a filed one reads as filed and drops off "+
			"the todo rung")
	} else if n < 1 {
		t.Errorf(tooFew, "runFT", n, "read filed_settled")
	}

	// 2. THE FILED JOIN'S CATEGORY HALF. runFC holds (catA,tgt1) filed and (catB,tgt1) unfiled:
	// same target, different category. This is the half the big test above could not see until
	// its own fourth coordinate was added.
	if n := at("FC", runFC).filed; n > 1 {
		t.Errorf(tooMany, "runFC", n, "read filed_settled", "the filed join's CATEGORY half is gone, "+
			"so filing under one category marks the SAME target filed under every other category")
	} else if n < 1 {
		t.Errorf(tooFew, "runFC", n, "read filed_settled")
	}

	// 3. THE DISPOSITION JOIN'S TARGET HALF. runDT holds (catA,tgt1) marked done and
	// (catA,tgt2) undisposed.
	if n := at("DT", runDT).disposed; n > 1 {
		t.Errorf(tooMany, "runDT", n, "carry a disposition", "the disposition join's TARGET half is "+
			"gone, so resolving one coordinate silently resolves every other one in the review that "+
			"shares its category")
	} else if n < 1 {
		t.Errorf(tooFew, "runDT", n, "carry a disposition")
	}

	// 4. THE DISPOSITION JOIN'S CATEGORY HALF. runDC holds (catA,tgt1) dismissed and
	// (catB,tgt1) undisposed.
	if n := at("DC", runDC).disposed; n > 1 {
		t.Errorf(tooMany, "runDC", n, "carry a disposition", "the disposition join's CATEGORY half is "+
			"gone, so dismissing one category's coordinate silently dismisses the same target under "+
			"every other category")
	} else if n < 1 {
		t.Errorf(tooFew, "runDC", n, "carry a disposition")
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

	// 7. filed_settled must follow filed_at, not the mere existence of the joined row. runCL's
	// single coordinate is CLAIMED and not settled, which is the only place in this fixture
	// where those two differ.
	cl := at("CL", runCL)
	if cl.rows != 1 {
		t.Errorf("runCL returned %d rows, want 1", cl.rows)
	}
	if cl.filed != 0 {
		t.Errorf("runCL: a coordinate that is only CLAIMED (filing_since set, filed_at still NULL) "+
			"reports filed_settled — the column is testing whether the filed row EXISTS rather than "+
			"whether it is settled, so a coordinate mid-filing leaves the todo rung and the badge "+
			"under-counts while no forge issue exists yet (%d of 1 settled)", cl.filed)
	}
	if cl.disposed != 0 {
		t.Errorf("runCL reports %d dispositions, want 0 — nothing disposed this coordinate", cl.disposed)
	}

	// 8. THE OWNER PREDICATE. The stranger's run was passed in @run_ids exactly as a spoofed
	// id would be, and it holds a recommendation on the same coordinate as everything else.
	// It must come back with NO rows.
	//
	// ASSERTED DIRECTLY, NEVER THROUGH at(). That helper Fatalfs when a run returns no rows —
	// which is precisely the CORRECT behaviour here — so routing this through it would fail
	// on correct code, and the natural "fix" (make at() tolerate nil) would disarm the guard
	// for all five legitimate runs. That guard is the only thing between a broken fixture and
	// five vacuously-passing tallies. For the same reason the stranger is deliberately absent
	// from assertion 0's loop.
	if tl := got[strangerRun]; tl != nil {
		t.Errorf("another user's run came back with %d rows (filed=%d disposed=%d) despite being "+
			"only a spoofed id in @run_ids — `WHERE rv.user_id = @user_id` is gone, so this caller's "+
			"badge counts another tenant's recommendations. The query's own comment claims exactly "+
			"this cannot happen", tl.rows, tl.filed, tl.disposed)
	}

	// 9. THE PAGE PREDICATE, which the stranger cannot observe: this run is the caller's OWN,
	// so the owner predicate admits it, and only `AND rv.target_run_id = ANY(@run_ids)` keeps
	// it out. Also asserted directly rather than through at(), for the same reason.
	if tl := got[unrequestedRun]; tl != nil {
		t.Errorf("a run the caller owns but did NOT request came back with %d rows — "+
			"`AND rv.target_run_id = ANY(@run_ids)` is gone, so this @lim-bounded read returns every "+
			"run the caller has ever had and the LIMIT then cuts arbitrary rows, understating the "+
			"badges of runs that ARE on the page", tl.rows)
	}
}

// ---- the tenant boundary, with two real tenants -------------------------------------

// TestJudgeBacklogIsTenantScopedLiveDB is the cross-TENANT half of the review_id pin, and it
// is deliberately a separate test from the cross-REVIEW coordinate in
// TestJudgeBacklogProjectsEveryColumnLiveDB. The two are NOT redundant and should not be
// merged later on the argument that they are:
//
//   - the cross-review coordinate (one user, two reviews) pins "the join is at least
//     REVIEW-scoped". Drop either review_id half and handRev's occurrence inherits autoRev's
//     link. Sufficient as a guard against the predicate being REMOVED.
//   - this test pins "and not merely USER-scoped, or looser". A one-user fixture cannot tell
//     those apart, and per-user is a plausible future request precisely BECAUSE of this PRD's
//     premise: "the same recommendation recurs across runs, triage it once" invites someone
//     to propose that one filed issue should cover the coordinate across all of a user's
//     reviews. Whoever implements that will delete the cross-review assertion as part of the
//     change — it is red by design under it — rewrite the join as per-user, and at that
//     moment the tenant boundary would rest on nothing with no test red anywhere.
//
// PROVENANCE: the auditor wrote, RAN and then DELETED this test in a throwaway worktree, so
// the strongest evidence for this session's most serious finding existed in no tree. It
// handed over the exact fixture, assertions and measured output; this is that test, landed,
// so the durable citation is a pin in the repo rather than "an agent demonstrated it once".
//
// 🟢 THE PRODUCTION CODE IS CORRECT — all three predicates are present and right. This closes
// a gap in what the SUITE can observe. Nothing shipped leaking.
//
// EITHER HALF ALONE LEAKS ITS OWN HALF (measured by the auditor): with only `f.review_id`
// gone the caller inherits the victim's filed link and the disposition stays clean; with only
// `d.review_id` gone, the reverse. So this is TWO pins with TWO separately-worded assertions
// — one combined check would name the wrong join under a single-half break.
//
// DO NOT "HARDEN" THIS WITH `AND d.set_by_user_id = @user_id`. It is the natural-looking fix
// and it would SILENTLY DROP EVERY AUTO-DONE: judge_issue_close.sql writes M6's issue-close
// sync with set_via='issue_close' and set_by_user_id hardcoded NULL, deliberately, so a
// system inference is never attributed to a person (Decision 6 / PF-4). `filed_by_user_id`
// and `set_by_user_id` are nullable ATTRIBUTION pointers that are NULL by design for a whole
// class of rows. `review_id -> run_reviews.user_id` is not merely the best available
// ownership path — it is the ONLY one, and that is a property of the design.
func TestJudgeBacklogIsTenantScopedLiveDB(t *testing.T) {
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

	// THE SAME on both tenants. This is the load-bearing line: if the two users' coordinates
	// differ by so much as the target string, nothing can leak and this test passes forever
	// while proving nothing — the memberRow lesson, which is that a fixture unable to
	// construct the failing input silently bounds every test built on it.
	const cat, target = "install_worker_tool", "tenant-shared-rg"

	// Each tenant is fully independent: repos.connection_id -> forge_connections.user_id, so
	// one user cannot borrow the other's repo.
	seed := func(label string, iid int64) (userID, reviewID, repoID uuid.UUID) {
		userID, reviewID, repoID = uuid.New(), uuid.New(), uuid.New()
		connID, runID := uuid.New(), uuid.New()
		mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
			userID, fmt.Sprintf("tenant-%s-%s@e2e", label, userID))
		mustExec(ctx, t, pool,
			`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
			 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
		mustExec(ctx, t, pool,
			`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
			 VALUES ($1, $2, $3, $4, 'https://forge.e2e/g/r', 'main', true)`,
			repoID, connID, iid, fmt.Sprintf("g/%s", label))
		mustExec(ctx, t, pool,
			`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status)
			 VALUES ($1, $2, $3, $4, $5, 'd', 'completed')`,
			runID, userID, repoID, iid, fmt.Sprintf("%s run", label))
		mustExec(ctx, t, pool,
			`INSERT INTO run_reviews (id, target_run_id, user_id, verdict) VALUES ($1, $2, $3, 'issues')`,
			reviewID, runID, userID)
		mustExec(ctx, t, pool,
			`INSERT INTO review_recommendations (review_id, category, target, rationale_md, confidence)
			 VALUES ($1, $2, $3, $4, 'high')`, reviewID, cat, target,
			fmt.Sprintf("%s's own rationale for %s/%s", label, cat, target))
		return userID, reviewID, repoID
	}

	victim, victimRev, victimRepo := seed("victim", 8801)
	caller, _, _ := seed("caller", 8802)

	// Only the VICTIM's coordinate is filed and disposed. Both live on one coordinate, so a
	// single fixture covers both join halves.
	mustExec(ctx, t, pool,
		`INSERT INTO recommendation_filed_issues
		   (id, review_id, category, target, filed_repo_id, filed_issue_iid, filed_issue_url, filed_by_user_id, filed_at)
		 VALUES ($1, $2, $3, $4, $5, 7777, 'https://forge.e2e/g/r/-/issues/7777', $6, now())`,
		uuid.New(), victimRev, cat, target, victimRepo, victim)
	mustExec(ctx, t, pool,
		`INSERT INTO recommendation_dispositions
		   (review_id, category, target, status, dismiss_reason, rationale_hash, set_by_user_id)
		 VALUES ($1, $2, $3, 'dismissed', 'wont_do', 'h', $4)`, victimRev, cat, target, victim)

	// Read as the CALLER and assert on the CALLER's own occurrence. Reading as the victim
	// shows the link correctly present and proves nothing.
	rows, err := q.ListJudgeRecommendationRowsForUser(ctx, store.ListJudgeRecommendationRowsForUserParams{
		UserID: caller,
		Lim:    100,
	})
	if err != nil {
		t.Fatalf("list backlog rows: %v", err)
	}
	var got *store.ListJudgeRecommendationRowsForUserRow
	for i := range rows {
		if rows[i].Category == cat && rows[i].Target == target {
			got = &rows[i]
			break
		}
	}
	// LOAD-BEARING, not defensive noise: without it, a change that drops the caller's row from
	// the result entirely makes both assertions below vacuously true and this test goes green
	// on a worse bug.
	if got == nil {
		t.Fatalf("fixture broken: the caller's own occurrence of %s/%s is missing", cat, target)
	}

	if got.FiledSettled || got.FiledIssueIid.Valid || got.FiledIssueUrl.Valid || got.FiledAt.Valid {
		t.Errorf("CROSS-TENANT LEAK: another user's filed issue reached this caller's backlog "+
			"(settled=%v iid=%d urlValid=%v url=%q) — recommendation_filed_issues has no user column, so "+
			"`f.review_id = rv.id` is the only thing scoping it",
			got.FiledSettled, got.FiledIssueIid.Int64, got.FiledIssueUrl.Valid, got.FiledIssueUrl.String)
	}
	if got.DispositionStatus.Valid {
		t.Errorf("CROSS-TENANT LEAK: another user's disposition (%q) reached this caller's backlog "+
			"— recommendation_dispositions has no user column, so `d.review_id = rv.id` is the only "+
			"thing scoping it. This caller's own open recommendation would also drop out of todo "+
			"because someone else settled theirs", got.DispositionStatus.String)
	}
}

// TestJudgeBacklogCategoryFilterPreCapLiveDB pins the load-bearing property of the
// ?category= filter (PRD #235 M1): it is enforced in SQL BEFORE the row-cap LIMIT, in the
// same owner-scoped WHERE the run_anchor semi-join already lives in — NOT in Go after the
// grouper. Two guarantees live only in that predicate and nowhere in Go:
//
//  1. PRE-CAP ORDERING. The cap (JudgeBacklogMaxRows) is applied BEFORE grouping, so a
//     post-cap Go filter would let a whole label truncate off-page: if the only rows of a
//     category sort past the LIMIT, a Go filter renders NOTHING while the true answer is
//     "several, past the cap". The SQL predicate narrows the rows FIRST, so the cap then
//     bounds only the selected label and its rows come back even when they would otherwise
//     be off-page. This test seeds a category whose only rows sort PAST a small Lim behind a
//     larger block of a different category, filters to it, and asserts they still return.
//  2. NULL = all labels; ANY(...) = the OR-union. A nil Categories slice maps to SQL NULL
//     and is a no-op (every label); a multi-value slice returns the union of those labels.
//
// WHY THIS MUST BE A LIVE-DB TEST. The handler's fake backlogStore returns its seeded rows
// VERBATIM and ignores every param (Categories included), so it cannot demonstrate a SQL
// filter at all — worse, a fake-store test would apply the filter in Go AFTER the cap and
// therefore PASS on the very off-page bug this predicate exists to prevent. Only real
// Postgres, ordering and cutting the rows itself, can show the filter running before the
// LIMIT. Modelled on TestJudgeBacklogRunAnchorLiveDB; SKIPs without UZI_TEST_DATABASE_URL.
func TestJudgeBacklogCategoryFilterPreCapLiveDB(t *testing.T) {
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
		owner, fmt.Sprintf("catfilter-%s@e2e", owner))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, owner, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)

	// One recommendation per run/review, with an EXPLICIT updated_at so the ORDER BY
	// (rv.updated_at DESC, …) is exact rather than dependent on insert speed — the same
	// technique TestJudgeBacklogPreviewRecencyLiveDB uses. Targets are namespaced so they
	// cannot collide with another live-DB test's target-scoped fixture on the shared database.
	iid := int64(0)
	seed := func(category, target, updatedAt string) {
		iid++
		runID, reviewID := uuid.New(), uuid.New()
		mustExec(ctx, t, pool,
			`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status)
			 VALUES ($1, $2, $3, $4, $5, 'd', 'completed')`, runID, owner, repoID, iid,
			fmt.Sprintf("run %d", iid))
		mustExec(ctx, t, pool,
			`INSERT INTO run_reviews (id, target_run_id, user_id, verdict, created_at, updated_at)
			 VALUES ($1, $2, $3, 'issues', $4::timestamptz, $4::timestamptz)`,
			reviewID, runID, owner, updatedAt)
		mustExec(ctx, t, pool,
			`INSERT INTO review_recommendations (review_id, category, target, rationale_md)
			 VALUES ($1, $2, $3, 'because')`, reviewID, category, target)
	}

	// A BLOCK of improve_agent that sorts AHEAD (newest updated_at), and the ONLY rows of two
	// other labels sitting BEHIND it (older). With a small Lim, the improve_agent block alone
	// fills the page and the other labels' rows are off-page — which is exactly the truncation
	// the pre-cap filter must survive.
	const aheadBlock = 5
	for i := 0; i < aheadBlock; i++ {
		seed("improve_agent", fmt.Sprintf("catfilter-agent-%d", i),
			fmt.Sprintf("2026-07-1%d 09:00:00+00", i)) // 2026-07-10 .. 2026-07-14, all newest
	}
	seed("improve_uzi", "catfilter-uzi-a", "2026-07-01 09:00:00+00")
	seed("improve_uzi", "catfilter-uzi-b", "2026-07-02 09:00:00+00")
	seed("enable_tool", "catfilter-tool-a", "2026-07-03 09:00:00+00")

	fetch := func(categories []string, lim int32) []store.ListJudgeRecommendationRowsForUserRow {
		t.Helper()
		rows, err := q.ListJudgeRecommendationRowsForUser(ctx, store.ListJudgeRecommendationRowsForUserParams{
			UserID: owner, Categories: categories, Lim: lim,
		})
		if err != nil {
			t.Fatalf("ListJudgeRecommendationRowsForUser(categories=%v, lim=%d): %v", categories, lim, err)
		}
		return rows
	}
	countByCategory := func(rows []store.ListJudgeRecommendationRowsForUserRow) map[string]int {
		out := map[string]int{}
		for _, r := range rows {
			out[r.Category]++
		}
		return out
	}

	// ---- 1. nil Categories is the no-op: SQL NULL → all labels --------------------------
	all := fetch(nil, 100)
	if got := countByCategory(all); len(all) != 8 ||
		got["improve_agent"] != aheadBlock || got["improve_uzi"] != 2 || got["enable_tool"] != 1 {
		t.Fatalf("nil filter returned %d rows %v, want all 8 across 3 labels (a NULL predicate must not filter)",
			len(all), got)
	}

	// ---- 2. CONTROL: unfiltered, the small cap drops the other labels off-page ----------
	// This is what makes the pre-cap proof below unambiguous: with Lim smaller than the
	// improve_agent block, an UNFILTERED read returns only improve_agent — the improve_uzi
	// rows genuinely sit past the LIMIT, so a Go post-cap filter would see none of them.
	const smallLim = 3 // < aheadBlock
	control := fetch(nil, smallLim)
	if got := countByCategory(control); len(control) != smallLim || got["improve_agent"] != smallLim {
		t.Fatalf("unfiltered Lim=%d returned %v, want %d rows ALL improve_agent — the other labels must "+
			"be off-page for the pre-cap proof to mean anything", smallLim, got, smallLim)
	}

	// ---- 3. THE DECISIVE ONE: the filter runs BEFORE the LIMIT --------------------------
	// Filter to improve_uzi with the same small Lim. Its rows sort PAST that Lim (proven by
	// the control above), so if the predicate ran in Go AFTER the cap it would filter the
	// top-3 (all improve_agent) down to ZERO. Getting both improve_uzi rows back proves the
	// predicate is in SQL, ahead of the LIMIT.
	uzi := fetch([]string{"improve_uzi"}, smallLim)
	if got := countByCategory(uzi); len(uzi) != 2 || got["improve_uzi"] != 2 {
		t.Fatalf("category=improve_uzi with Lim=%d returned %v, want BOTH improve_uzi rows even though "+
			"they sort past the cap — the filter must run in SQL before the LIMIT, not in Go after it",
			smallLim, got)
	}

	// ---- 4. multi-value is the OR-union -------------------------------------------------
	union := fetch([]string{"improve_uzi", "enable_tool"}, 100)
	if got := countByCategory(union); len(union) != 3 ||
		got["improve_uzi"] != 2 || got["enable_tool"] != 1 || got["improve_agent"] != 0 {
		t.Fatalf("category=improve_uzi,enable_tool returned %v, want the OR-union (2 improve_uzi + "+
			"1 enable_tool, no improve_agent)", got)
	}
}
