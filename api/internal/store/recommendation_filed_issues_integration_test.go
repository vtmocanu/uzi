package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestRecommendationFiledIssuesLiveDB exercises the PRD #68 M1 schema + queries against
// a REAL Postgres — the claim-first coordinate table and its interplay with a re-judge
// and with the Decision 12 backlog exclusion, none of which the fake-store unit tests
// can cover. It proves the three load-bearing M1 properties from the PRD:
//
//	(a) the filed link SURVIVES a re-judge — UpsertRunReviewWithRecommendations
//	    delete-reinserts the recommendation rows but leaves recommendation_filed_issues
//	    untouched (Decision 6, the whole reason the link lives in its own table keyed on
//	    the stable review coordinate);
//	(b) a claim on an already-claimed coordinate is REJECTED (pgx.ErrNoRows), the
//	    claim-first ON CONFLICT DO NOTHING (Decision 7);
//	(c) duplicate (category, target) coordinates COLLAPSE to a single claim — the
//	    accepted target='' collapse (Decision 6): one row, not a fan-out.
//
// Plus the settle / revert / sweep paths and the Decision 12 claimed-or-filed exclusion
// (a claim with filed_at still NULL already drops the coordinate from the backlog).
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (e2e/run-store-it.sh).
func TestRecommendationFiledIssuesLiveDB(t *testing.T) {
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
		userID, fmt.Sprintf("filed-%s@e2e", userID))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)
	mustExec(ctx, t, pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind)
		 VALUES ($1, $2, $3, 42, 'Do X', 'desc', 'completed', 'issue')`, targetID, userID, repoID)

	// A review with two coordinates: an improve_uzi with target='' (the backlog coordinate
	// Decision 12 must exclude once filed/claimed) and an install_worker_tool.
	recs, _ := json.Marshal([]map[string]string{
		{"category": "improve_uzi", "target": "", "rationale_md": "tidy", "confidence": "low"},
		{"category": "install_worker_tool", "target": "shellcheck", "rationale_md": "missing", "confidence": "high"},
	})
	reviewID, err := q.UpsertRunReviewWithRecommendations(ctx, store.UpsertRunReviewWithRecommendationsParams{
		TargetRunID: targetID, UserID: userID,
		Verdict: "issues", SummaryMd: "s", JudgeModel: "haiku", Status: "complete",
		Recommendations: recs,
	})
	if err != nil {
		t.Fatalf("UpsertRunReviewWithRecommendations: %v", err)
	}

	// ── Decision 12 baseline: the improve_uzi target='' coordinate is in the backlog ──
	if got := openImproveUziTargets(ctx, t, q, userID); !contains(got, "") {
		t.Fatalf("before any claim, the improve_uzi target='' rec must be in the backlog; got %v", got)
	}

	// ── (b)+(c) claim the improve_uzi target='' coordinate; a second claim is rejected ──
	claimID, err := q.ClaimRecommendationFiledIssue(ctx, store.ClaimRecommendationFiledIssueParams{
		ReviewID: reviewID, Category: "improve_uzi", Target: "",
		FiledByUserID: pgtype.UUID{Bytes: userID, Valid: true},
	})
	if err != nil {
		t.Fatalf("first claim must win: %v", err)
	}
	if _, err := q.ClaimRecommendationFiledIssue(ctx, store.ClaimRecommendationFiledIssueParams{
		ReviewID: reviewID, Category: "improve_uzi", Target: "",
		FiledByUserID: pgtype.UUID{Bytes: userID, Valid: true},
	}); err != pgx.ErrNoRows {
		t.Fatalf("a second claim on an already-claimed coordinate must return pgx.ErrNoRows, got %v", err)
	}
	// Two distinct improve_uzi target='' recommendations collapse to ONE claim row: the
	// coordinate, not the row, is the grain (Decision 6). Both concurrent siblings would
	// hit the same ON CONFLICT — exactly one row exists.
	if n := countFiledForCoord(ctx, t, pool, reviewID, "improve_uzi", ""); n != 1 {
		t.Fatalf("duplicate (category,target) coordinates must collapse to one claim; got %d rows", n)
	}

	// ── Decision 12: a CLAIMED (filed_at still NULL) coordinate already drops out of the
	//    backlog — the exclusion keys on the row EXISTING, not filed_at IS NOT NULL ──
	if got := openImproveUziTargets(ctx, t, q, userID); contains(got, "") {
		t.Fatalf("a mid-filing (claimed, unsettled) improve_uzi coordinate must be excluded from the backlog; got %v", got)
	}

	// ── settle the winning claim; a re-settle is a no-op (0 rows, filed_at guard) ──
	n, err := q.SettleRecommendationFiledIssue(ctx, store.SettleRecommendationFiledIssueParams{
		FiledRepoID:   pgtype.UUID{Bytes: repoID, Valid: true},
		FiledIssueIid: pgtype.Int8{Int64: 101, Valid: true},
		FiledIssueUrl: "https://forge.e2e/g/r/-/issues/101",
		ID:            claimID,
	})
	if err != nil || n != 1 {
		t.Fatalf("settle should affect exactly 1 row, got n=%d err=%v", n, err)
	}
	if n, err := q.SettleRecommendationFiledIssue(ctx, store.SettleRecommendationFiledIssueParams{
		FiledIssueUrl: "x", ID: claimID,
	}); err != nil || n != 0 {
		t.Fatalf("re-settling an already-settled claim must affect 0 rows (created-with-warning path), got n=%d err=%v", n, err)
	}

	// read side (M2/M4): the settled coordinate reads back with the issue link ──
	filed := listFiled(ctx, t, q, reviewID)
	if len(filed) != 1 {
		t.Fatalf("ListFiledIssuesForReview: want 1 row, got %d", len(filed))
	}
	if !filed[0].FiledAt.Valid || filed[0].FilingSince.Valid || filed[0].FiledIssueUrl != "https://forge.e2e/g/r/-/issues/101" {
		t.Fatalf("settled row shape wrong: %+v", filed[0])
	}

	// ── (a) re-judge: recommendation rows are delete-reinserted, the filed link is NOT ──
	recsRejudge, _ := json.Marshal([]map[string]string{
		{"category": "improve_uzi", "target": "", "rationale_md": "tidy v2", "confidence": "medium"},
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
	filedAfter := listFiled(ctx, t, q, reviewID)
	if len(filedAfter) != 1 || filedAfter[0].ID != claimID || !filedAfter[0].FiledAt.Valid {
		t.Fatalf("the filed link must survive a re-judge untouched (same id, still settled); got %+v", filedAfter)
	}
	// And the re-emitted improve_uzi target='' coordinate stays excluded from the backlog
	// because the surviving link still covers it — no duplicate re-arm (a success criterion).
	if got := openImproveUziTargets(ctx, t, q, userID); contains(got, "") {
		t.Fatalf("after re-judge, the still-filed improve_uzi coordinate must stay out of the backlog; got %v", got)
	}

	// ── revert: a forge failure deletes the claim; the coordinate is fileable again ──
	revClaim, err := q.ClaimRecommendationFiledIssue(ctx, store.ClaimRecommendationFiledIssueParams{
		ReviewID: reviewID, Category: "improve_agent", Target: "coder",
		FiledByUserID: pgtype.UUID{Bytes: userID, Valid: true},
	})
	if err != nil {
		t.Fatalf("claim improve_agent/coder: %v", err)
	}
	if err := q.RevertRecommendationFiledIssue(ctx, revClaim); err != nil {
		t.Fatalf("revert: %v", err)
	}
	if n := countFiledForCoord(ctx, t, pool, reviewID, "improve_agent", "coder"); n != 0 {
		t.Fatalf("revert must delete the claim row; got %d rows", n)
	}
	if _, err := q.ClaimRecommendationFiledIssue(ctx, store.ClaimRecommendationFiledIssueParams{
		ReviewID: reviewID, Category: "improve_agent", Target: "coder",
		FiledByUserID: pgtype.UUID{Bytes: userID, Valid: true},
	}); err != nil {
		t.Fatalf("a reverted coordinate must be claimable again: %v", err)
	}

	// ── sweep: a claim stranded past the cutoff is deleted; a settled row is NOT ──
	sweepClaim, err := q.ClaimRecommendationFiledIssue(ctx, store.ClaimRecommendationFiledIssueParams{
		ReviewID: reviewID, Category: "install_worker_tool", Target: "shellcheck",
		FiledByUserID: pgtype.UUID{Bytes: userID, Valid: true},
	})
	if err != nil {
		t.Fatalf("claim to strand: %v", err)
	}
	mustExec(ctx, t, pool, `UPDATE recommendation_filed_issues SET filing_since = now() - interval '1 hour' WHERE id = $1`, sweepClaim)
	swept, err := q.SweepStrandedRecommendationClaims(ctx, pgtype.Timestamptz{Time: time.Now().Add(-time.Minute), Valid: true})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}

	// 🔴 THIS USED TO ASSERT `swept != 1` — A TABLE-WIDE EQUALITY ON A QUERY WITH NO SCOPE.
	// SweepStrandedRecommendationClaims deletes by cutoff across the WHOLE table, and the
	// live-DB packages share one database, so that number counted every stranded claim any
	// other fixture had left behind. It is the standing "scope live-DB assertions to the
	// fixture" rule, broken by the assertion written to enforce the sweep.
	//
	// Not hypothetical: measured 2026-07-21 at `swept=23` against `want 1`, on a database that
	// had accumulated claims across repeated runs of a NEIGHBOURING test. It fails on what other
	// tests left behind, which is the definition of a flake, and it would have gone off in CI
	// the first time two fixtures overlapped. Worse than flaky, it was ALSO weak: an equality on
	// a global count says nothing about WHICH row went.
	//
	// Replaced by three fixture-scoped facts, which are strictly stronger. Each names the
	// predicate it pins, because a sweep has three and the old assertion reached none of them:
	//
	//   1. the STRANDED claim is gone            -> `filing_since < @cutoff` fires when it should
	//   2. the FRESH claim SURVIVES              -> `filing_since < @cutoff` does NOT over-fire
	//   3. the SETTLED row survives              -> `filed_at IS NULL` protects a filed link
	//
	// BE PRECISE ABOUT WHAT (2) ADDS, because the tempting claim is bigger than the measurement.
	// It is NOT that over-firing was previously undetectable: on a clean database the old global
	// count would have read 2 and tripped `!= 1`. What (2) adds is that the detection no longer
	// rides on THE SAME NUMBER that made the assertion flaky — a count that other fixtures move
	// in both directions can mask an over-fire as easily as it can invent one — and that the
	// failure now names the row and the consequence instead of printing an integer.
	//
	// It is still the assertion that matters most, because it pins the cutoff in the direction
	// that costs money. The query's own comment explains why: the sweep is a DELETE, so reaping a
	// slow-but-alive CreateIssue lets a retry re-INSERT and file a SECOND forge issue, the exact
	// duplicate the claim-first design exists to prevent. The fixture already held a live,
	// unstranded claim (improve_agent/coder, re-claimed just above) and nothing had ever asserted
	// it must survive.
	//
	// MEASURED, both directions, one fold per run against a fresh database, each compiled first:
	//
	//	`filing_since < @cutoff` -> `filing_since > @cutoff`            RED at (1), that one only
	//	`AND filing_since < @cutoff` -> `AND (filing_since < @cutoff
	//	    OR filing_since IS NOT NULL)`                               RED at (2), that one only
	//
	// The obvious over-fire fold — DELETING the `AND filing_since < @cutoff` clause — does NOT
	// work here, and the reason is the sqlc trap in CLAUDE.md wearing a new costume: dropping the
	// clause drops the only use of @cutoff, so the generated function loses its parameter and the
	// TEST FILE stops compiling ("too many arguments in call"). A build error, not a red
	// assertion. The always-true disjunction keeps the parameter referenced and its type intact.
	if n := countFiledForCoord(ctx, t, pool, reviewID, "install_worker_tool", "shellcheck"); n != 0 {
		t.Fatalf("the stranded claim (filing_since backdated an hour, cutoff a minute) must be "+
			"reaped; %d row(s) survive — the cutoff comparison is not firing", n)
	}
	if n := countFiledForCoord(ctx, t, pool, reviewID, "improve_agent", "coder"); n != 1 {
		t.Fatalf("a claim stamped seconds ago must SURVIVE a cutoff of now()-1m; got %d row(s). "+
			"An over-eager sweep DELETEs a claim whose CreateIssue is still in flight, and the retry "+
			"then files a SECOND forge issue — the duplicate claim-first exists to prevent", n)
	}
	// The settled improve_uzi row (filing_since NULL) must be untouched by any sweep.
	if n := countFiledForCoord(ctx, t, pool, reviewID, "improve_uzi", ""); n != 1 {
		t.Fatalf("the sweep must never touch a settled row (filing_since NULL); got %d", n)
	}
	// Kept only as a sanity floor, deliberately NOT an equality: this test's own stranded row
	// makes >=1 true regardless of what else shares the database, while `== 1` was a statement
	// about every other fixture in the suite. It catches a sweep that deleted nothing at all.
	if swept < 1 {
		t.Fatalf("sweep reported %d rows deleted; it must have reaped at least this fixture's "+
			"stranded claim", swept)
	}
}

// TestRecommendationFiledIssueSetNullLiveDB proves the FK delete rules (PRD #68 Decision
// 6): disconnecting the filed repo or removing the filing user must NOT destroy another
// run's filed link — filed_repo_id / filed_by_user_id go NULL and filed_issue_url stays as
// the durable pointer. It uses a DISTINCT filer (an admin, userB) filing against a DISTINCT
// repo (repoB) than the review owner (userA)'s run repo, so deleting them exercises SET
// NULL without cascading the review away (the review's own user_id is CASCADE, the run's
// repo is CASCADE — those would take the link with them, which is the correct, different
// behavior asserted by the re-judge/run-deletion paths).
func TestRecommendationFiledIssueSetNullLiveDB(t *testing.T) {
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

	userA, connA, repoA, targetID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	userB, connB, repoB := uuid.New(), uuid.New(), uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, userA, fmt.Sprintf("owner-%s@e2e", userA))
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash, is_admin) VALUES ($1, $2, 'x', true)`, userB, fmt.Sprintf("admin-%s@e2e", userB))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bota', 1, $3)`, connA, userA, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'botb', 2, $3)`, connB, userB, []byte{0x2})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/ra', 'https://forge.e2e/g/ra', 'main', true)`, repoA, connA)
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 2, 'g/rb', 'https://forge.e2e/g/rb', 'main', true)`, repoB, connB)
	mustExec(ctx, t, pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind)
		 VALUES ($1, $2, $3, 7, 'Do X', 'd', 'completed', 'issue')`, targetID, userA, repoA)

	recs, _ := json.Marshal([]map[string]string{{"category": "improve_uzi", "target": "", "rationale_md": "r", "confidence": "low"}})
	reviewID, err := q.UpsertRunReviewWithRecommendations(ctx, store.UpsertRunReviewWithRecommendationsParams{
		TargetRunID: targetID, UserID: userA, Verdict: "issues", SummaryMd: "s", JudgeModel: "haiku", Status: "complete", Recommendations: recs,
	})
	if err != nil {
		t.Fatalf("upsert review: %v", err)
	}

	// userB (admin) files against repoB (their own repo), NOT userA's run repo.
	claimID, err := q.ClaimRecommendationFiledIssue(ctx, store.ClaimRecommendationFiledIssueParams{
		ReviewID: reviewID, Category: "improve_uzi", Target: "", FiledByUserID: pgtype.UUID{Bytes: userB, Valid: true},
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	const filedURL = "https://forge.e2e/g/rb/-/issues/101"
	if n, err := q.SettleRecommendationFiledIssue(ctx, store.SettleRecommendationFiledIssueParams{
		FiledRepoID: pgtype.UUID{Bytes: repoB, Valid: true}, FiledIssueIid: pgtype.Int8{Int64: 101, Valid: true},
		FiledIssueUrl: filedURL, ID: claimID,
	}); err != nil || n != 1 {
		t.Fatalf("settle: n=%d err=%v", n, err)
	}

	// Disconnect the filed repo, then remove the filing user. Neither owns the review, so
	// the link must SURVIVE with the two FKs nulled and the durable URL intact.
	mustExec(ctx, t, pool, `DELETE FROM repos WHERE id = $1`, repoB)
	mustExec(ctx, t, pool, `DELETE FROM users WHERE id = $1`, userB)

	var repoNull, userNull bool
	var url string
	var filedAtValid bool
	if err := pool.QueryRow(ctx,
		`SELECT filed_repo_id IS NULL, filed_by_user_id IS NULL, filed_issue_url, filed_at IS NOT NULL
		   FROM recommendation_filed_issues WHERE id = $1`, claimID).Scan(&repoNull, &userNull, &url, &filedAtValid); err != nil {
		t.Fatalf("the filed link must survive the repo+user deletion (SET NULL, not CASCADE): %v", err)
	}
	if !repoNull {
		t.Error("filed_repo_id must be NULL after the filed repo was deleted (ON DELETE SET NULL)")
	}
	if !userNull {
		t.Error("filed_by_user_id must be NULL after the filing user was deleted (ON DELETE SET NULL)")
	}
	if url != filedURL {
		t.Errorf("filed_issue_url = %q, want the durable pointer %q (must outlive the repo/user)", url, filedURL)
	}
	if !filedAtValid {
		t.Error("filed_at must remain set — only the disconnected FKs go NULL")
	}
}

// openImproveUziTargets returns the targets of the given owner's open improve_uzi
// backlog. PRD #590 M2 deleted the instance-wide ListOpenImproveUziRecommendations, so
// this now uses the owner-scoped sibling (ListOpenImproveUziRecommendationsForUser) with
// the SAME two coordinate exclusions (filed-issues, dispositions). Scoping by userID also
// makes the shared-DB assertions robust against another test's leftover improve_uzi rows.
func openImproveUziTargets(ctx context.Context, t *testing.T, q *store.Queries, userID uuid.UUID) []string {
	t.Helper()
	rows, err := q.ListOpenImproveUziRecommendationsForUser(ctx, store.ListOpenImproveUziRecommendationsForUserParams{
		UserID: userID,
		Lim:    100,
	})
	if err != nil {
		t.Fatalf("ListOpenImproveUziRecommendationsForUser: %v", err)
	}
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Target
	}
	return out
}

func listFiled(ctx context.Context, t *testing.T, q *store.Queries, reviewID uuid.UUID) []store.RecommendationFiledIssue {
	t.Helper()
	rows, err := q.ListFiledIssuesForReview(ctx, reviewID)
	if err != nil {
		t.Fatalf("ListFiledIssuesForReview: %v", err)
	}
	return rows
}

func countFiledForCoord(ctx context.Context, t *testing.T, pool *pgxpool.Pool, reviewID uuid.UUID, category, target string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM recommendation_filed_issues WHERE review_id = $1 AND category = $2 AND target = $3`,
		reviewID, category, target).Scan(&n); err != nil {
		t.Fatalf("count filed for coord: %v", err)
	}
	return n
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
