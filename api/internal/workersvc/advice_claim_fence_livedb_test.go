package workersvc

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #1423: the judge review and task-review advice POSTs are fenced on the ADVICE run's
// claim (claim_generation + claim_released_at IS NULL), in the same statement as the upsert.
// These live-DB tests run the real CTEs: a superseded flight (the run was reclaimed at G+1
// while the post carries G) and a released claim write NOTHING and surface ErrStaleClaim; the
// current flight's post lands; a capability worker that omits the generation is refused before
// anything is written; a legacy worker's unstamped post still lands on a live claim.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.

const (
	adviceStaleGen int64 = 4
	adviceLiveGen  int64 = 5 // the run was reclaimed: G+1
)

func adviceWorkers(workerID, userID uuid.UUID) (legacy, capable store.Worker) {
	legacy = store.Worker{ID: workerID, UserID: userID}
	capable = store.Worker{ID: workerID, UserID: userID, ProtocolCapabilities: []string{capability.CredentialSwitchV1}}
	return legacy, capable
}

func (e codexTestEnv) adviceCount(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(e.ctx, sql, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", sql, err)
	}
	return n
}

func (e codexTestEnv) adviceText(t *testing.T, sql string, args ...any) string {
	t.Helper()
	var s string
	if err := e.pool.QueryRow(e.ctx, sql, args...).Scan(&s); err != nil {
		t.Fatalf("read %q: %v", sql, err)
	}
	return s
}

func TestPostReviewClaimGenerationFenceLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, workerID, repoID := env.seedCodexInfra(t)
	svc := New(env.q, env.box, testParams())
	legacy, capable := adviceWorkers(workerID, userID)

	targetID := uuid.New()
	env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status)
	          VALUES ($1, $2, $3, 'issue', 1423, 't', 'd', 'completed')`, targetID, userID, repoID)
	judgeID := uuid.New()
	env.exec(`INSERT INTO runs (id, user_id, kind, issue_title, issue_description, status, target_run_id)
	          VALUES ($1, $2, 'judge', 't', 'd', 'queued', $3)`, judgeID, userID, targetID)
	// A stale requeue + same-worker reclaim left the judge run live at G+1.
	env.exec(`UPDATE runs SET claim_generation = $1, status = 'running', worker_id = $2 WHERE id = $3`,
		adviceLiveGen, workerID, judgeID)

	// nonce keeps the global LIKE probes below scoped to this test's rows on a reused DB.
	nonce := uuid.NewString()
	sub := func(summary string) ReviewSubmission {
		return ReviewSubmission{
			Verdict: "issues", SummaryMd: summary, JudgeModel: "haiku", Status: "complete",
			Recommendations: []ReviewRecommendation{
				{Category: "improve_uzi", Target: "a-" + nonce + "-" + summary, RationaleMd: "r", Confidence: "high"},
				{Category: "improve_agent", Target: "b-" + nonce + "-" + summary, RationaleMd: "r", Confidence: "low"},
			},
		}
	}
	reviews := func() int {
		return env.adviceCount(t, `SELECT count(*) FROM run_reviews WHERE target_run_id = $1`, targetID)
	}
	recs := func() int {
		return env.adviceCount(t, `SELECT count(*) FROM review_recommendations WHERE produced_by_run_id = $1`, judgeID)
	}
	summary := func() string {
		return env.adviceText(t, `SELECT summary_md FROM run_reviews WHERE target_run_id = $1`, targetID)
	}

	// 1. The superseded flight (stamped G) writes nothing: no review, no recommendation rows.
	if _, err := svc.PostReview(env.ctx, capable, targetID, sub("stale-first"), i64(adviceStaleGen)); !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("stale-generation post err = %v, want ErrStaleClaim", err)
	}
	if n, r := reviews(), recs(); n != 0 || r != 0 {
		t.Fatalf("stale post persisted reviews=%d recommendations=%d, want 0/0", n, r)
	}

	// 2. A capability worker that omits the generation is refused, nothing written.
	if _, err := svc.PostReview(env.ctx, capable, targetID, sub("capable-nil"), nil); !errors.Is(err, ErrMissingClaimGeneration) {
		t.Fatalf("capability nil-gen post err = %v, want ErrMissingClaimGeneration", err)
	}
	if n, r := reviews(), recs(); n != 0 || r != 0 {
		t.Fatalf("capability nil-gen post persisted reviews=%d recommendations=%d, want 0/0", n, r)
	}

	// 3. The current flight (stamped G+1) lands.
	res, err := svc.PostReview(env.ctx, capable, targetID, sub("current"), i64(adviceLiveGen))
	if err != nil {
		t.Fatalf("live-generation post: %v", err)
	}
	if res.ReviewID == uuid.Nil || reviews() != 1 || recs() != 2 || summary() != "current" {
		t.Fatalf("live post: review=%v reviews=%d recs=%d summary=%q, want 1 review / 2 recs / current",
			res.ReviewID, reviews(), recs(), summary())
	}

	// 4. With a review present, a stale post leaves it (and its recommendations) unchanged.
	if _, err := svc.PostReview(env.ctx, capable, targetID, sub("stale-overwrite"), i64(adviceStaleGen)); !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("stale overwrite err = %v, want ErrStaleClaim", err)
	}
	if reviews() != 1 || recs() != 2 || summary() != "current" {
		t.Fatalf("stale overwrite changed the review: reviews=%d recs=%d summary=%q", reviews(), recs(), summary())
	}
	if n := env.adviceCount(t, `SELECT count(*) FROM review_recommendations WHERE target LIKE $1`, "%"+nonce+"-stale-overwrite"); n != 0 {
		t.Fatalf("stale overwrite inserted %d recommendation rows, want 0", n)
	}

	// 5. A capability worker nil-gen post with a review present: refused, unchanged.
	if _, err := svc.PostReview(env.ctx, capable, targetID, sub("capable-nil-2"), nil); !errors.Is(err, ErrMissingClaimGeneration) {
		t.Fatalf("capability nil-gen post err = %v, want ErrMissingClaimGeneration", err)
	}
	if summary() != "current" {
		t.Fatalf("capability nil-gen post changed the review summary to %q", summary())
	}

	// 6. A legacy worker's unstamped post on a live claim still lands (compatibility).
	if _, err := svc.PostReview(env.ctx, legacy, targetID, sub("legacy"), nil); err != nil {
		t.Fatalf("legacy nil-gen post on a live claim: %v", err)
	}
	if reviews() != 1 || recs() != 2 || summary() != "legacy" {
		t.Fatalf("legacy post: reviews=%d recs=%d summary=%q, want 1 / 2 / legacy", reviews(), recs(), summary())
	}

	// 7. A RELEASED claim fences out even the current generation, and a legacy nil-gen post.
	env.exec(`UPDATE runs SET claim_released_at = now() WHERE id = $1`, judgeID)
	if _, err := svc.PostReview(env.ctx, capable, targetID, sub("released"), i64(adviceLiveGen)); !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("released-claim post err = %v, want ErrStaleClaim", err)
	}
	if _, err := svc.PostReview(env.ctx, legacy, targetID, sub("released-legacy"), nil); !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("released-claim legacy post err = %v, want ErrStaleClaim", err)
	}
	if reviews() != 1 || recs() != 2 || summary() != "legacy" {
		t.Fatalf("released post changed the review: reviews=%d recs=%d summary=%q", reviews(), recs(), summary())
	}
}

func TestPostTaskReviewClaimGenerationFenceLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, workerID, repoID := env.seedCodexInfra(t)
	svc := New(env.q, env.box, testParams())
	legacy, capable := adviceWorkers(workerID, userID)

	// The reviewed task (completed) and its review run (a task carrying review_target_run_id).
	taskID := uuid.New()
	branch := "uzi/task/" + taskID.String()
	env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, branch, base_branch, review_requested, issue_title, issue_description, status, auto_approve)
	          VALUES ($1, $2, $3, 'task', $4, 'main', true, 't', 'd', 'completed', true)`, taskID, userID, repoID, branch)
	reviewID := uuid.New()
	env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, branch, base_branch, review_target_run_id, dispatched_at,
	                            auto_approve, open_mr, review_requested, issue_title, issue_description, status)
	          VALUES ($1, $2, $3, 'task', $4, 'main', $5, now(), true, false, false, 'review', '', 'queued')`,
		reviewID, userID, repoID, branch, taskID)
	env.exec(`UPDATE runs SET claim_generation = $1, status = 'running', worker_id = $2 WHERE id = $3`,
		adviceLiveGen, workerID, reviewID)

	// nonce keeps the global LIKE probes below scoped to this test's rows on a reused DB.
	nonce := uuid.NewString()
	sub := func(summary string) TaskReviewSubmission {
		return TaskReviewSubmission{Status: "complete", SummaryMd: summary, Findings: []TaskReviewFinding{
			{File: "api/" + nonce + "/" + summary + ".go", Line: 1, Severity: "error", SummaryMd: "x", RationaleMd: "y"},
			{File: "api/" + nonce + "/" + summary + "_2.go", Line: 2, Severity: "info", SummaryMd: "x", RationaleMd: "y"},
		}}
	}
	reviews := func() int {
		return env.adviceCount(t, `SELECT count(*) FROM task_reviews WHERE target_run_id = $1`, taskID)
	}
	findings := func() int {
		return env.adviceCount(t, `SELECT count(*) FROM task_review_findings f JOIN task_reviews r ON r.id = f.review_id
		                     WHERE r.target_run_id = $1`, taskID)
	}
	summary := func() string {
		return env.adviceText(t, `SELECT summary_md FROM task_reviews WHERE target_run_id = $1`, taskID)
	}

	// 1. The superseded flight (stamped G) writes nothing.
	if err := svc.PostTaskReview(env.ctx, capable, taskID, sub("stale-first"), i64(adviceStaleGen)); !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("stale-generation post err = %v, want ErrStaleClaim", err)
	}
	if n, f := reviews(), findings(); n != 0 || f != 0 {
		t.Fatalf("stale post persisted reviews=%d findings=%d, want 0/0", n, f)
	}
	if n := env.adviceCount(t, `SELECT count(*) FROM task_review_findings WHERE file LIKE $1`, "api/"+nonce+"/stale-first%"); n != 0 {
		t.Fatalf("stale post left %d finding rows, want 0", n)
	}

	// 2. A capability worker that omits the generation is refused, nothing written.
	if err := svc.PostTaskReview(env.ctx, capable, taskID, sub("capable-nil"), nil); !errors.Is(err, ErrMissingClaimGeneration) {
		t.Fatalf("capability nil-gen post err = %v, want ErrMissingClaimGeneration", err)
	}
	if n, f := reviews(), findings(); n != 0 || f != 0 {
		t.Fatalf("capability nil-gen post persisted reviews=%d findings=%d, want 0/0", n, f)
	}

	// 3. The current flight (stamped G+1) lands.
	if err := svc.PostTaskReview(env.ctx, capable, taskID, sub("current"), i64(adviceLiveGen)); err != nil {
		t.Fatalf("live-generation post: %v", err)
	}
	if reviews() != 1 || findings() != 2 || summary() != "current" {
		t.Fatalf("live post: reviews=%d findings=%d summary=%q, want 1 / 2 / current", reviews(), findings(), summary())
	}

	// 4. With a review present, a stale post leaves it unchanged.
	if err := svc.PostTaskReview(env.ctx, capable, taskID, sub("stale-overwrite"), i64(adviceStaleGen)); !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("stale overwrite err = %v, want ErrStaleClaim", err)
	}
	if reviews() != 1 || findings() != 2 || summary() != "current" {
		t.Fatalf("stale overwrite changed the review: reviews=%d findings=%d summary=%q", reviews(), findings(), summary())
	}
	if n := env.adviceCount(t, `SELECT count(*) FROM task_review_findings WHERE file LIKE $1`, "api/"+nonce+"/stale-overwrite%"); n != 0 {
		t.Fatalf("stale overwrite inserted %d finding rows, want 0", n)
	}

	// 5. A capability worker nil-gen post with a review present: refused, unchanged.
	if err := svc.PostTaskReview(env.ctx, capable, taskID, sub("capable-nil-2"), nil); !errors.Is(err, ErrMissingClaimGeneration) {
		t.Fatalf("capability nil-gen post err = %v, want ErrMissingClaimGeneration", err)
	}
	if summary() != "current" {
		t.Fatalf("capability nil-gen post changed the review summary to %q", summary())
	}

	// 6. A legacy worker's unstamped post on a live claim still lands.
	if err := svc.PostTaskReview(env.ctx, legacy, taskID, sub("legacy"), nil); err != nil {
		t.Fatalf("legacy nil-gen post on a live claim: %v", err)
	}
	if reviews() != 1 || findings() != 2 || summary() != "legacy" {
		t.Fatalf("legacy post: reviews=%d findings=%d summary=%q, want 1 / 2 / legacy", reviews(), findings(), summary())
	}

	// 7. A RELEASED claim fences out even the current generation, and a legacy nil-gen post.
	env.exec(`UPDATE runs SET claim_released_at = now() WHERE id = $1`, reviewID)
	if err := svc.PostTaskReview(env.ctx, capable, taskID, sub("released"), i64(adviceLiveGen)); !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("released-claim post err = %v, want ErrStaleClaim", err)
	}
	if err := svc.PostTaskReview(env.ctx, legacy, taskID, sub("released-legacy"), nil); !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("released-claim legacy post err = %v, want ErrStaleClaim", err)
	}
	if reviews() != 1 || findings() != 2 || summary() != "legacy" {
		t.Fatalf("released post changed the review: reviews=%d findings=%d summary=%q", reviews(), findings(), summary())
	}
}
