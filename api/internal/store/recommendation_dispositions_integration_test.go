package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
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
//	    (Decision 2/8);
//	(d) a disposed improve_uzi LEAVES the self-improve backlog and Undo (delete) re-includes
//	    it (Decision 9).
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
