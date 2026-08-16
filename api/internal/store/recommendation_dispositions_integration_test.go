package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestRecommendationDispositionsLiveDB exercises the PRD #94 M1 schema + queries against a
// REAL Postgres — the coordinate-keyed disposition table, its status/reason CHECK, its
// survival across a re-judge, the global flat join feeding the Go bucketer, and the
// self-improve exclusion — none of which the fake-store unit tests can cover. It proves the
// load-bearing M1 properties from the PRD:
//
//	(a) a disposition SURVIVES a re-judge — UpsertRunReviewWithRecommendations delete-
//	    reinserts the recommendation rows but leaves recommendation_dispositions (and its
//	    rationale_hash) untouched on the stable review coordinate (Decision 1/3);
//	(b) the status/reason CHECK: dismissed REQUIRES a reason and done FORBIDS one;
//	(c) ListJudgeTriageRowsForUser returns the right (disposition_status, filed_settled)
//	    per recommendation — an UNSETTLED filed claim (filed_at NULL) is NOT "filed"
//	    (Decision 2/8). 🔴 IT DOES NOT PIN THAT QUERY'S COORDINATE JOINS — see below;
//	(d) a disposed improve_uzi LEAVES the self-improve backlog and Undo (delete) re-includes
//	    it (Decision 9).
//
// 🔴 WHAT THIS TEST DOES NOT COVER, stated HERE because a reader who finds it first would
// otherwise reasonably conclude it does. Its (c) leg exercises ListJudgeTriageRowsForUser but
// CANNOT observe either LEFT JOIN's coordinate predicates: the three coordinates below differ
// in BOTH halves at once (improve_uzi with an empty target, install_worker_tool/shellcheck,
// improve_agent/coder), so no two share a category or a target and every half has nothing to
// discriminate. MEASURED 2026-07-21, four fresh-database runs with a positive control:
// dropping `d.target`, `d.category`, `f.target` or `f.category` in isolation left the ENTIRE
// live-DB suite green each time. Only dropping BOTH halves of a join went red — the weaker
// mutation passing while the stronger one fails, which is what makes this worth stating
// rather than assuming.
//
// The load-bearing half of that is "the ENTIRE suite": the fold was invisible to every test,
// not merely to the one under discussion. The suite tally was 126 pass / 0 fail, and it is
// BOUND TO `8c6be2b8` rather than stated as a current figure — it was 128 by `c1fcdfce` and
// 129 by `31080a40`, so a reader who counts today would conclude this comment is wrong when
// it is only unlabelled. Every one of those figures is bound to a SHA, including the newest:
// writing "129 now" would reintroduce the defect inside the sentence describing it. A tally
// drifts exactly like a line number; the mechanism is the claim.
//
// TestJudgeTriageRowsForUserAreCoordinateScopedLiveDB (below) covers those four halves. Do not
// delete it as redundant with this one: the properties do not overlap, and this fixture cannot
// be extended to cover them without rewriting the assertions it was reviewed for.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (e2e/run-store-it.sh).
func TestRecommendationDispositionsLiveDB(t *testing.T) {
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

	userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
	targetID := uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("disp-%s@e2e", userID))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)
	mustExec(ctx, t, pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind)
		 VALUES ($1, $2, $3, 42, 'Do X', 'desc', 'completed', 'issue')`, targetID, userID, repoID)

	// Three coordinates: improve_uzi/'' (the backlog coordinate), install_worker_tool/
	// 'shellcheck' (we settle a filed link on it), improve_agent/'coder' (we leave an
	// UNSETTLED claim on it — must read filed_settled=false).
	recs, _ := json.Marshal([]map[string]string{
		{"category": "improve_uzi", "target": "", "rationale_md": "tidy", "confidence": "low"},
		{"category": "install_worker_tool", "target": "shellcheck", "rationale_md": "missing", "confidence": "high"},
		{"category": "improve_agent", "target": "coder", "rationale_md": "tweak", "confidence": ""},
	})
	reviewID, err := q.UpsertRunReviewWithRecommendations(ctx, store.UpsertRunReviewWithRecommendationsParams{
		TargetRunID: targetID, UserID: userID,
		Verdict: "issues", SummaryMd: "s", JudgeModel: "haiku", Status: "complete",
		Recommendations: recs,
	})
	if err != nil {
		t.Fatalf("UpsertRunReviewWithRecommendations: %v", err)
	}

	// Isolation: the self-improve backlog query (ListOpenImproveUziRecommendations) is
	// INSTANCE-WIDE, and this test deliberately ends with an OPEN improve_uzi/'' rec (the
	// tail Undo re-includes it). In the shared-DB LiveDB run that row would otherwise leak
	// into a later test's global backlog assertion — it broke TestRecommendationFiledIssuesLiveDB
	// (which asserts its own claimed improve_uzi/'' is excluded, but saw this one lingering).
	// Deleting the review cascades its recommendations, dispositions, and filed links. Runs
	// before the deferred pool.Close (LIFO), and on failure too (Fatalf runs defers).
	defer func() {
		mustExec(ctx, t, pool, `DELETE FROM run_reviews WHERE id = $1`, reviewID)
	}()

	// ── (b) the status/reason CHECK, on a throwaway coordinate ──
	// dismissed with NULL reason must fail.
	if _, err := q.UpsertRecommendationDisposition(ctx, store.UpsertRecommendationDispositionParams{
		ReviewID: reviewID, Category: "enable_tool", Target: "chk", Status: "dismissed",
		DismissReason: pgtype.Text{Valid: false}, RationaleHash: "h",
		SetByUserID: pgtype.UUID{Bytes: userID, Valid: true},
	}); err == nil {
		t.Fatal("dismissed with a NULL reason must violate the status/reason CHECK")
	}
	// done with a non-NULL reason must fail.
	if _, err := q.UpsertRecommendationDisposition(ctx, store.UpsertRecommendationDispositionParams{
		ReviewID: reviewID, Category: "enable_tool", Target: "chk", Status: "done",
		DismissReason: pgtype.Text{String: "wont_do", Valid: true}, RationaleHash: "h",
		SetByUserID: pgtype.UUID{Bytes: userID, Valid: true},
	}); err == nil {
		t.Fatal("done with a non-NULL reason must violate the status/reason CHECK")
	}
	// done with a NULL reason succeeds.
	if _, err := q.UpsertRecommendationDisposition(ctx, store.UpsertRecommendationDispositionParams{
		ReviewID: reviewID, Category: "enable_tool", Target: "chk", Status: "done",
		DismissReason: pgtype.Text{Valid: false}, RationaleHash: "h",
		SetByUserID: pgtype.UUID{Bytes: userID, Valid: true},
	}); err != nil {
		t.Fatalf("done + NULL reason must be accepted: %v", err)
	}
	// dismissed with a reason succeeds (upsert on the same throwaway coordinate).
	if _, err := q.UpsertRecommendationDisposition(ctx, store.UpsertRecommendationDispositionParams{
		ReviewID: reviewID, Category: "enable_tool", Target: "chk", Status: "dismissed",
		DismissReason: pgtype.Text{String: "not_an_issue", Valid: true}, RationaleHash: "h",
		SetByUserID: pgtype.UUID{Bytes: userID, Valid: true},
	}); err != nil {
		t.Fatalf("dismissed + reason must be accepted: %v", err)
	}

	// ── dispose the improve_uzi/'' coordinate (dismissed, false-positive), hash h1 ──
	const hash1 = "sha256:tidy-v1"
	disp, err := q.UpsertRecommendationDisposition(ctx, store.UpsertRecommendationDispositionParams{
		ReviewID: reviewID, Category: "improve_uzi", Target: "", Status: "dismissed",
		DismissReason: pgtype.Text{String: "not_an_issue", Valid: true}, RationaleHash: hash1,
		SetByUserID: pgtype.UUID{Bytes: userID, Valid: true},
	})
	if err != nil {
		t.Fatalf("dispose improve_uzi: %v", err)
	}
	if disp.Status != "dismissed" || disp.DismissReason.String != "not_an_issue" || disp.RationaleHash != hash1 {
		t.Fatalf("disposition round-trip wrong: %+v", disp)
	}

	// ── (d) baseline: a disposed improve_uzi/'' leaves the self-improve backlog ──
	if got := openImproveUziTargets(ctx, t, q); contains(got, "") {
		t.Fatalf("a disposed improve_uzi/'' must be excluded from the backlog; got %v", got)
	}

	// ── set up the filed axis for the triage join ──
	// shellcheck: claim + settle → a SETTLED filed link (filed_settled=true).
	settleID, err := q.ClaimRecommendationFiledIssue(ctx, store.ClaimRecommendationFiledIssueParams{
		ReviewID: reviewID, Category: "install_worker_tool", Target: "shellcheck",
		FiledByUserID: pgtype.UUID{Bytes: userID, Valid: true},
	})
	if err != nil {
		t.Fatalf("claim shellcheck: %v", err)
	}
	if n, err := q.SettleRecommendationFiledIssue(ctx, store.SettleRecommendationFiledIssueParams{
		FiledRepoID: pgtype.UUID{Bytes: repoID, Valid: true}, FiledIssueIid: pgtype.Int8{Int64: 7, Valid: true},
		FiledIssueUrl: "https://forge.e2e/g/r/-/issues/7", ID: settleID,
	}); err != nil || n != 1 {
		t.Fatalf("settle shellcheck: n=%d err=%v", n, err)
	}
	// coder: claim only → an UNSETTLED claim (filed_at NULL, filed_settled MUST be false).
	if _, err := q.ClaimRecommendationFiledIssue(ctx, store.ClaimRecommendationFiledIssueParams{
		ReviewID: reviewID, Category: "improve_agent", Target: "coder",
		FiledByUserID: pgtype.UUID{Bytes: userID, Valid: true},
	}); err != nil {
		t.Fatalf("claim coder: %v", err)
	}

	// ── (c) the global flat join: one row per rec with (disposition_status, filed_settled) ──
	rows, err := q.ListJudgeTriageRowsForUser(ctx, userID)
	if err != nil {
		t.Fatalf("ListJudgeTriageRowsForUser: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("want 3 flat rows (one per rec), got %d: %+v", len(rows), rows)
	}
	var gotDismissed, gotFiledSettled, gotUnsettledOpen int
	for _, r := range rows {
		switch {
		case r.DispositionStatus.Valid && r.DispositionStatus.String == "dismissed" && !r.FiledSettled:
			gotDismissed++ // improve_uzi/'' — dismissed, never filed
		case !r.DispositionStatus.Valid && r.FiledSettled:
			gotFiledSettled++ // shellcheck — settled filed link, no disposition
		case !r.DispositionStatus.Valid && !r.FiledSettled:
			gotUnsettledOpen++ // coder — an UNSETTLED claim counts as NOT filed
		default:
			t.Fatalf("unexpected triage row: %+v", r)
		}
	}
	if gotDismissed != 1 || gotFiledSettled != 1 || gotUnsettledOpen != 1 {
		t.Fatalf("triage buckets wrong: dismissed=%d filedSettled=%d unsettledOpen=%d",
			gotDismissed, gotFiledSettled, gotUnsettledOpen)
	}

	// ── (a) re-judge: recommendation rows delete-reinserted, the disposition + hash SURVIVE ──
	recsRejudge, _ := json.Marshal([]map[string]string{
		{"category": "improve_uzi", "target": "", "rationale_md": "tidy v2", "confidence": "medium"},
		{"category": "install_worker_tool", "target": "shellcheck", "rationale_md": "missing", "confidence": "high"},
		{"category": "improve_agent", "target": "coder", "rationale_md": "tweak", "confidence": ""},
	})
	reviewID2, err := q.UpsertRunReviewWithRecommendations(ctx, store.UpsertRunReviewWithRecommendationsParams{
		TargetRunID: targetID, UserID: userID,
		Verdict: "ok", SummaryMd: "s2", JudgeModel: "haiku", Status: "complete",
		Recommendations: recsRejudge,
	})
	if err != nil {
		t.Fatalf("re-judge upsert: %v", err)
	}
	if reviewID2 != reviewID {
		t.Fatalf("re-judge must reuse the same review row (UNIQUE target_run_id): %v vs %v", reviewID2, reviewID)
	}
	survivors := listDispositions(ctx, t, q, reviewID)
	var improveDisp *store.RecommendationDisposition
	for i := range survivors {
		if survivors[i].Category == "improve_uzi" && survivors[i].Target == "" {
			improveDisp = &survivors[i]
		}
	}
	if improveDisp == nil {
		t.Fatalf("the improve_uzi disposition must survive a re-judge untouched; rows: %+v", survivors)
	}
	if improveDisp.Status != "dismissed" || improveDisp.RationaleHash != hash1 {
		t.Fatalf("disposition must survive with status + hash unchanged (hash compare is API-layer); got %+v", *improveDisp)
	}

	// ── (d) Undo: deleting the disposition re-includes improve_uzi/'' in the backlog ──
	if got := openImproveUziTargets(ctx, t, q); contains(got, "") {
		t.Fatalf("after re-judge the still-disposed improve_uzi/'' must stay out of the backlog; got %v", got)
	}
	n, err := q.DeleteRecommendationDisposition(ctx, store.DeleteRecommendationDispositionParams{
		ReviewID: reviewID, Category: "improve_uzi", Target: "",
	})
	if err != nil || n != 1 {
		t.Fatalf("delete disposition (undo) should affect 1 row, got n=%d err=%v", n, err)
	}
	if got := openImproveUziTargets(ctx, t, q); !contains(got, "") {
		t.Fatalf("after Undo, the improve_uzi/'' rec must re-enter the backlog; got %v", got)
	}
	// Undo is idempotent — a second delete affects 0 rows.
	if n, err := q.DeleteRecommendationDisposition(ctx, store.DeleteRecommendationDispositionParams{
		ReviewID: reviewID, Category: "improve_uzi", Target: "",
	}); err != nil || n != 0 {
		t.Fatalf("a second delete must affect 0 rows (idempotent undo), got n=%d err=%v", n, err)
	}
}

func listDispositions(ctx context.Context, t *testing.T, q *store.Queries, reviewID uuid.UUID) []store.RecommendationDisposition {
	t.Helper()
	rows, err := q.ListDispositionsForReview(ctx, reviewID)
	if err != nil {
		t.Fatalf("ListDispositionsForReview: %v", err)
	}
	return rows
}

// TestJudgeTriageRowsForUserAreCoordinateScopedLiveDB pins the four coordinate halves of the
// two LEFT JOINs in ListJudgeTriageRowsForUser (dispositions.sql) — PRD #94's query, which
// PRD #98 made load-bearing.
//
// WHY IT EXISTS, measured (auditor, 2026-07-21, fresh database and positive control per run):
// each of the four halves was individually INERT. Dropping `d.target`, `d.category`,
// `f.target` or `f.category` in isolation left the ENTIRE live-DB suite green, every time —
// 126 pass / 0 fail AT `8c6be2b8`, where it was measured (128 by `c1fcdfce`, 129 by
// `31080a40`; the tally is the receipt, "the ENTIRE suite" is the claim). Only dropping BOTH
// halves of a join
// went red. The cause is the shape of
// TestRecommendationDispositionsLiveDB's fixture above: its three coordinates differ in BOTH
// halves at once (improve_uzi with an empty target, install_worker_tool/shellcheck,
// improve_agent/coder), so no
// two of them share a category or a target and every half has nothing to discriminate.
//
// WHY THIS QUERY MATTERS MOST OF THE FOUR SITES. It produces `triage.todo` — the ONE canonical
// number the nav badge, the Judge page tab and M5's notification all single-source (Decision
// 1, deliberately the same query rather than three equal-by-construction counts). THE
// CONSISTENCY THE DESIGN BUYS IS EXACTLY WHAT HIDES A FAULT HERE: with one source, a broken
// coordinate half makes all three consumers read the same wrong number and agree perfectly,
// so the cross-check the PRD relies on cannot fire. Direction: a recommendation sharing a
// category with a disposed sibling in the same review inherits that disposition, so `todo`
// UNDER-counts and work silently vanishes from the backlog this PRD exists to surface.
//
// SHAPE. The row projects only (disposition_status, dismiss_reason, filed_settled) — no
// category, no target, no run id — so within one caller you cannot tell WHICH coordinate
// inherited, and the target-half and category-half folds would fire the same assertion. The
// query is keyed by user, so each half gets its OWN USER holding exactly two coordinates that
// share one half and differ in the other. Each fold then moves exactly one user's tally.
//
// ✅ MEASURED after this test existed: each of the four folds above now reddens exactly ONE
// assertion — userFT, userFC, userDT, userDC respectively. Four runs, fresh database each,
// mutation asserted present in both the .sql and the regenerated .sql.go and gone from both
// after, and the positive control asserted (this test seen as RUN and PASS/FAIL, zero SKIP).
//
// improve_uzi IS DELIBERATELY ABSENT. It is the one category with an instance-wide second
// consumer (ListOpenImproveUziRecommendations selects it across the whole table), which is
// why the fixture above needs its `defer DELETE FROM run_reviews` and why an unrelated M4
// fixture failed an M6 test earlier the same day. Every coordinate here is inert.
func TestJudgeTriageRowsForUserAreCoordinateScopedLiveDB(t *testing.T) {
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

	const (
		catA = "install_worker_tool"
		catB = "improve_agent"
		tgt1 = "shellcheck"
		tgt2 = "rg"
	)

	// seedPair creates an independent user holding ONE review with exactly two coordinates,
	// then puts a filed+settled link or a disposition on the FIRST of them.
	type coord struct{ category, target string }
	seedPair := func(label string, iid int64, mark string, a, b coord) uuid.UUID {
		userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
		runID, reviewID := uuid.New(), uuid.New()
		mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
			userID, fmt.Sprintf("triagescope-%s-%s@e2e", label, userID))
		mustExec(ctx, t, pool,
			`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
			 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
		mustExec(ctx, t, pool,
			`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
			 VALUES ($1, $2, $3, $4, 'https://forge.e2e/g/r', 'main', true)`,
			repoID, connID, iid, fmt.Sprintf("g/%s", label))
		mustExec(ctx, t, pool,
			`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status)
			 VALUES ($1, $2, $3, $4, $5, 'd', 'completed')`, runID, userID, repoID, iid,
			fmt.Sprintf("%s run", label))
		mustExec(ctx, t, pool,
			`INSERT INTO run_reviews (id, target_run_id, user_id, verdict) VALUES ($1, $2, $3, 'issues')`,
			reviewID, runID, userID)
		for _, c := range []coord{a, b} {
			mustExec(ctx, t, pool,
				`INSERT INTO review_recommendations (review_id, category, target, rationale_md, confidence)
				 VALUES ($1, $2, $3, $4, 'high')`, reviewID, c.category, c.target,
				fmt.Sprintf("%s rationale for %s/%s", label, c.category, c.target))
		}
		switch mark {
		case "filed":
			mustExec(ctx, t, pool,
				`INSERT INTO recommendation_filed_issues
				   (id, review_id, category, target, filed_repo_id, filed_issue_iid, filed_issue_url, filed_by_user_id, filed_at)
				 VALUES ($1, $2, $3, $4, $5, $6, 'https://forge.e2e/g/r/-/issues/1', $7, now())`,
				uuid.New(), reviewID, a.category, a.target, repoID, iid, userID)
		case "disposed":
			mustExec(ctx, t, pool,
				`INSERT INTO recommendation_dispositions
				   (review_id, category, target, status, rationale_hash, set_by_user_id)
				 VALUES ($1, $2, $3, 'done', 'h', $4)`, reviewID, a.category, a.target, userID)
		default:
			t.Fatalf("seedPair: unknown mark %q", mark)
		}
		return userID
	}

	// Each user's pair shares exactly ONE half with its marked sibling.
	userFT := seedPair("ft", 9101, "filed", coord{catA, tgt1}, coord{catA, tgt2})    // same category
	userFC := seedPair("fc", 9102, "filed", coord{catA, tgt1}, coord{catB, tgt1})    // same target
	userDT := seedPair("dt", 9103, "disposed", coord{catA, tgt2}, coord{catA, tgt1}) // same category
	userDC := seedPair("dc", 9104, "disposed", coord{catA, tgt2}, coord{catB, tgt2}) // same target

	tally := func(label string, userID uuid.UUID) (rows, settled, disposed int) {
		t.Helper()
		got, err := q.ListJudgeTriageRowsForUser(ctx, userID)
		if err != nil {
			t.Fatalf("%s: ListJudgeTriageRowsForUser: %v", label, err)
		}
		for _, r := range got {
			rows++
			if r.FiledSettled {
				settled++
			}
			if r.DispositionStatus.Valid {
				disposed++
			}
		}
		if rows != 2 {
			t.Fatalf("%s: got %d rows, want exactly 2 — the fixture or the owner predicate is "+
				"wrong, so nothing below proves anything", label, rows)
		}
		return rows, settled, disposed
	}

	// 1. THE FILED JOIN'S TARGET HALF.
	if _, settled, _ := tally("userFT", userFT); settled != 1 {
		t.Errorf("userFT: %d of 2 coordinates read filed_settled, want exactly 1 — the filed join's "+
			"TARGET half is gone, so a coordinate merely sharing a category with a filed one reads "+
			"as filed and drops off the todo rung", settled)
	}
	// 2. THE FILED JOIN'S CATEGORY HALF.
	if _, settled, _ := tally("userFC", userFC); settled != 1 {
		t.Errorf("userFC: %d of 2 coordinates read filed_settled, want exactly 1 — the filed join's "+
			"CATEGORY half is gone, so filing under one category marks the SAME target filed under "+
			"every other category", settled)
	}
	// 3. THE DISPOSITION JOIN'S TARGET HALF — the one that makes triage.todo UNDER-count.
	if _, _, disposed := tally("userDT", userDT); disposed != 1 {
		t.Errorf("userDT: %d of 2 coordinates carry a disposition, want exactly 1 — the disposition "+
			"join's TARGET half is gone, so resolving one coordinate silently resolves every other "+
			"one in the review sharing its category, and triage.todo under-counts", disposed)
	}
	// 4. THE DISPOSITION JOIN'S CATEGORY HALF.
	if _, _, disposed := tally("userDC", userDC); disposed != 1 {
		t.Errorf("userDC: %d of 2 coordinates carry a disposition, want exactly 1 — the disposition "+
			"join's CATEGORY half is gone, so resolving one category's coordinate silently resolves "+
			"the same target under every other category", disposed)
	}
}
