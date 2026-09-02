package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// TestPostReviewAutoDismissesDeniedCLILiveDB exercises the issue #167 deterministic net
// against a REAL Postgres: at the PostReview hook, a judge recommendation whose target names
// a denylisted credential-bearing CLI (glab/gh/aws/az/…) is auto-dismissed with the distinct
// self-measuring provenance set_via='denied_cli', while clean recs, out-of-scope categories,
// and coordinates already carrying a human verdict are left alone.
//
// It also VERIFIES THE 00128 MIGRATION CONSTRAINT NAME by construction: store.Migrate applies
// 00128, whose Up does `DROP CONSTRAINT recommendation_dispositions_set_via_check` by the
// Postgres-auto-generated name. A wrong name fails migrate (Fatalf below); and the subsequent
// insert of a set_via='denied_cli' row would violate the OLD domain if the recreate had not
// widened it — so a green run proves both the name and the widened CHECK.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; ./e2e/run-store-it.sh
// and the test:api-store-it CI job provide one and sweep this package for the LiveDB suffix.
func TestPostReviewAutoDismissesDeniedCLILiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate (00128 DROP CONSTRAINT by name would fail here if the auto-name is wrong): %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	q := store.New(pool)
	box := newHandlerTestBox(t)
	svc := workersvc.New(q, box, workersvc.Params{})

	owner := uuid.New()
	connID, repoID := uuid.New(), uuid.New()
	targetID, judgeID, workerID := uuid.New(), uuid.New(), uuid.New()

	mustExecT(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		owner, fmt.Sprintf("denycli-%s@e2e", owner))
	// Ensure the whole fixture (runs, workers, reviews, recs, dispositions) is gone
	// afterwards — the improve_uzi rec below is instance-wide backlog and must not leak.
	t.Cleanup(func() { mustExecT(ctx, t, pool, `DELETE FROM users WHERE id = $1`, owner) })
	mustExecT(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, owner, []byte{0x1})
	mustExecT(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)
	// The reviewed (target) run, and a NON-TERMINAL judge run owned by our worker that
	// reviews it — exactly what authorizeJudgeTrace requires for PostReview to authorize.
	mustExecT(ctx, t, pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind)
		 VALUES ($1, $2, $3, 42, 'Do X', 'd', 'completed', 'issue')`, targetID, owner, repoID)
	// token_hash is UNIQUE and shared across every package the live-DB sweep runs
	// (nothing truncates workers between package binaries), so derive it from a fresh
	// uuid rather than a fixed literal — see hosted_provision_livedb_test.go.
	workerTokenHash := uuid.New()
	mustExecT(ctx, t, pool,
		`INSERT INTO workers (id, user_id, name, token_hash, status) VALUES ($1, $2, 'judge-worker', $3, 'online')`,
		workerID, owner, workerTokenHash[:])
	mustExecT(ctx, t, pool,
		`INSERT INTO runs (id, user_id, kind, target_run_id, worker_id, issue_title, issue_description, status)
		 VALUES ($1, $2, 'judge', $3, $4, '', '', 'running')`, judgeID, owner, targetID, workerID)

	// The review submission the worker posts. Two denied in-scope recs (a bare glab, and a
	// mixed "file, glab"), one clean in-scope rec, one denied token in an OUT-OF-SCOPE
	// category, and a denied 'gh' rec that already carries a HUMAN verdict.
	const (
		catInstall  = "install_worker_tool"
		catEnable   = "enable_tool"
		catImprove  = "improve_uzi"
		glabRat     = "install glab for MR ops"
		mixedRat    = "the file uses glab"
		ghRat       = "install gh"
		ripgrepRat  = "install ripgrep"
		improveRat  = "improve aws integration coverage"
		mixedTarget = "file, glab"
	)
	sub := workersvc.ReviewSubmission{
		Verdict: "issues", SummaryMd: "s", JudgeModel: "haiku", Status: "complete",
		Recommendations: []workersvc.ReviewRecommendation{
			{Category: catInstall, Target: "glab", RationaleMd: glabRat, Confidence: "high"},
			{Category: catEnable, Target: mixedTarget, RationaleMd: mixedRat, Confidence: "medium"},
			{Category: catInstall, Target: "ripgrep", RationaleMd: ripgrepRat, Confidence: "high"},
			{Category: catImprove, Target: "improve aws integration", RationaleMd: improveRat, Confidence: "low"},
			{Category: catInstall, Target: "gh", RationaleMd: ghRat, Confidence: "high"},
		},
	}

	// Pre-create the review (server-side upsert, same call PostReview makes) so we can plant a
	// HUMAN disposition on the 'gh' coordinate BEFORE the auto-dismiss runs. PostReview reuses
	// this review row (UNIQUE target_run_id), so the coordinate — and the human row on it —
	// survive into the hook.
	recsJSON, _ := json.Marshal(sub.Recommendations)
	reviewID, err := q.UpsertRunReviewWithRecommendations(ctx, store.UpsertRunReviewWithRecommendationsParams{
		TargetRunID: targetID, UserID: owner, Verdict: sub.Verdict, SummaryMd: sub.SummaryMd,
		JudgeModel: sub.JudgeModel, Status: sub.Status, Recommendations: recsJSON,
	})
	if err != nil {
		t.Fatalf("seed review: %v", err)
	}
	// Human verdict on gh: 'done', a real user as setter. The net must NOT clobber it.
	mustExecT(ctx, t, pool,
		`INSERT INTO recommendation_dispositions (review_id, category, target, status, rationale_hash, set_by_user_id)
		 VALUES ($1, $2, 'gh', 'done', 'human-hash', $3)`, reviewID, catInstall, owner)

	// ── the hook under test ──
	res, err := svc.PostReview(ctx, store.Worker{ID: workerID, UserID: owner}, targetID, sub)
	if err != nil {
		t.Fatalf("PostReview: %v", err)
	}
	if res.ReviewID != reviewID {
		t.Fatalf("PostReview reused review = %v, want %v", res.ReviewID, reviewID)
	}

	// glab: an auto-dismissal exists, server-side literals, hash = sha256(rationale).
	d := getDisposition(ctx, t, pool, reviewID, catInstall, "glab")
	if d.Status != "dismissed" || d.DismissReason.String != "wont_do" || d.SetVia.String != "denied_cli" {
		t.Fatalf("glab disposition wrong: status=%q reason=%q set_via=%q", d.Status, d.DismissReason.String, d.SetVia.String)
	}
	if d.SetByUserID.Valid {
		t.Fatalf("glab auto-dismissal must have a NULL setter, got %v", d.SetByUserID)
	}
	if d.RationaleHash != workersvc.RationaleHash(glabRat) {
		t.Fatalf("glab rationale_hash = %q, want %q", d.RationaleHash, workersvc.RationaleHash(glabRat))
	}

	// "file, glab" (mixed, enable_tool): a partial token match dismisses.
	if m := getDisposition(ctx, t, pool, reviewID, catEnable, mixedTarget); m.Status != "dismissed" || m.SetVia.String != "denied_cli" {
		t.Fatalf("mixed target disposition wrong: status=%q set_via=%q", m.Status, m.SetVia.String)
	}

	// ripgrep: clean, no disposition.
	if hasDisposition(ctx, t, pool, reviewID, catInstall, "ripgrep") {
		t.Fatal("a clean rec (ripgrep) must not be auto-dismissed")
	}
	// improve_uzi: out of scope, even though 'aws' appears in the target.
	if hasDisposition(ctx, t, pool, reviewID, catImprove, "improve aws integration") {
		t.Fatal("a denied token in an out-of-scope category (improve_uzi) must not be auto-dismissed")
	}

	// gh: the human verdict is untouched (ON CONFLICT DO NOTHING).
	gh := getDisposition(ctx, t, pool, reviewID, catInstall, "gh")
	if gh.Status != "done" || gh.SetVia.Valid || !gh.SetByUserID.Valid || uuid.UUID(gh.SetByUserID.Bytes) != owner {
		t.Fatalf("human 'gh' disposition must be untouched: status=%q set_via=%q setter=%v",
			gh.Status, gh.SetVia.String, gh.SetByUserID)
	}

	// Triage tally: the auto-dismissals count as Dismissed but NOT FalsePositives (wont_do,
	// not not_an_issue). Rows: glab+mixed dismissed, gh done, ripgrep+improve todo.
	rows, err := q.ListJudgeTriageRowsForUser(ctx, owner)
	if err != nil {
		t.Fatalf("ListJudgeTriageRowsForUser: %v", err)
	}
	tr := make([]workersvc.TriageRow, 0, len(rows))
	for _, r := range rows {
		tr = append(tr, workersvc.TriageRow{Status: r.DispositionStatus.String, Reason: r.DismissReason.String, FiledSettled: r.FiledSettled})
	}
	dto := workersvc.BucketTriage(tr)
	if dto.Dismissed != 2 {
		t.Fatalf("triage Dismissed = %d, want 2 (glab + file,glab)", dto.Dismissed)
	}
	if dto.FalsePositives != 0 {
		t.Fatalf("triage FalsePositives = %d, want 0 — a denied-CLI dismissal is wont_do, not not_an_issue", dto.FalsePositives)
	}
	if dto.Done != 1 {
		t.Fatalf("triage Done = %d, want 1 (the human gh verdict)", dto.Done)
	}
}

// getDisposition reads the single disposition row on a coordinate; it fails the test if the
// row is absent, so callers use it only where a disposition is expected.
func getDisposition(ctx context.Context, t *testing.T, pool *pgxpool.Pool, reviewID uuid.UUID, category, target string) store.RecommendationDisposition {
	t.Helper()
	var d store.RecommendationDisposition
	err := pool.QueryRow(ctx,
		`SELECT status, dismiss_reason, rationale_hash, set_via, set_by_user_id
		   FROM recommendation_dispositions WHERE review_id = $1 AND category = $2 AND target = $3`,
		reviewID, category, target,
	).Scan(&d.Status, &d.DismissReason, &d.RationaleHash, &d.SetVia, &d.SetByUserID)
	if err != nil {
		t.Fatalf("read disposition (%s/%s): %v", category, target, err)
	}
	return d
}

// hasDisposition reports whether any disposition row exists on a coordinate.
func hasDisposition(ctx context.Context, t *testing.T, pool *pgxpool.Pool, reviewID uuid.UUID, category, target string) bool {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM recommendation_dispositions WHERE review_id = $1 AND category = $2 AND target = $3`,
		reviewID, category, target,
	).Scan(&n); err != nil {
		t.Fatalf("count disposition (%s/%s): %v", category, target, err)
	}
	return n > 0
}
