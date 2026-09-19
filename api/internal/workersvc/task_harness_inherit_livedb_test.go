package workersvc

import (
	"testing"

	"github.com/google/uuid"
)

// task_harness_inherit_livedb_test.go is the PRD #1429 M3 regression suite for D4's
// derived-task-origin inheritance (task.go CreateTaskReviewRun/CreateThenFixRun): a
// task-review and a then-fix INHERIT their target/original run's harness as an EXPLICIT
// selection, routed through the M2 createRunResolved funnel (replacing the M1
// HarnessClaude stopgap). An inherited-but-unusable harness fails closed
// (ErrNoCredentialForHarness) with NO row created — a legible skip, not a silent
// disappearance. Reuses the codexTestEnv fixtures. Skipped unless
// UZI_TEST_DATABASE_URL points at a throwaway Postgres; run via ./e2e/run-store-it.sh.

// countTaskRunsForReviewTarget counts task runs reviewing targetRunID.
func countTaskRunsForReviewTarget(t *testing.T, env codexTestEnv, targetRunID uuid.UUID) int {
	t.Helper()
	var n int
	if err := env.pool.QueryRow(env.ctx, `SELECT count(*) FROM runs WHERE kind = 'task' AND review_target_run_id = $1`, targetRunID).Scan(&n); err != nil {
		t.Fatalf("count review runs for target: %v", err)
	}
	return n
}

// firstTaskRunForReviewTarget reads the (unique, active-index-enforced) review run for a
// target, failing the test if none exists.
func firstTaskRunForReviewTarget(t *testing.T, env codexTestEnv, targetRunID uuid.UUID) (id uuid.UUID, harness string) {
	t.Helper()
	if err := env.pool.QueryRow(env.ctx, `SELECT id, harness FROM runs WHERE kind = 'task' AND review_target_run_id = $1`, targetRunID).
		Scan(&id, &harness); err != nil {
		t.Fatalf("read review run for target %s: %v", targetRunID, err)
	}
	return id, harness
}

// countTaskRunsForThenFixOriginal counts task runs that are a then-fix of originalRunID.
func countTaskRunsForThenFixOriginal(t *testing.T, env codexTestEnv, originalRunID uuid.UUID) int {
	t.Helper()
	var n int
	if err := env.pool.QueryRow(env.ctx, `SELECT count(*) FROM runs WHERE kind = 'task' AND then_fix_of_run_id = $1`, originalRunID).Scan(&n); err != nil {
		t.Fatalf("count then-fix runs for original: %v", err)
	}
	return n
}

// firstTaskRunForThenFixOriginal reads the (unique, active-index-enforced) fix run for an
// original, failing the test if none exists.
func firstTaskRunForThenFixOriginal(t *testing.T, env codexTestEnv, originalRunID uuid.UUID) (id uuid.UUID, harness string) {
	t.Helper()
	if err := env.pool.QueryRow(env.ctx, `SELECT id, harness FROM runs WHERE kind = 'task' AND then_fix_of_run_id = $1`, originalRunID).
		Scan(&id, &harness); err != nil {
		t.Fatalf("read then-fix run for original %s: %v", originalRunID, err)
	}
	return id, harness
}

// seedTaskRun inserts a kind='task' run, returning its id.
func seedTaskRun(t *testing.T, env codexTestEnv, userID, repoID uuid.UUID, branch, harness, status string, reviewRequested, thenFixRequested bool) uuid.UUID {
	t.Helper()
	runID := uuid.New()
	env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_title, issue_description, status, branch, review_requested, then_fix_requested, harness, dispatched_at)
	          VALUES ($1, $2, $3, 'task', 't', '', $4, $5, $6, $7, $8, now())`,
		runID, userID, repoID, status, branch, reviewRequested, thenFixRequested, harness)
	return runID
}

// atomicSvcForUser is atomicSvc plus the judge-style settings/opt-in wiring maybeEnqueueJudge
// does not need here — task-review/then-fix creation goes straight through
// createRunResolved, so only the tx beginner matters.
func atomicSvcForUser(env codexTestEnv) *Service {
	return atomicSvc(env)
}

// TestMaybeEnqueueTaskReviewInheritsCodexHarnessLiveDB (D4): a completed --review task
// stamped harness='codex' auto-creates a review run FROZEN onto Codex, not the M1
// HarnessClaude stopgap.
func TestMaybeEnqueueTaskReviewInheritsCodexHarnessLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, repoID := seedCodexOnlyOriginUser(t, env)
	svc := atomicSvcForUser(env)

	originalID := seedTaskRun(t, env, userID, repoID, "uzi/task/"+uuid.NewString(), string(HarnessCodex), "completed", true, false)
	original := mustRun(t, env, originalID)

	svc.maybeEnqueueTaskReview(env.ctx, original)

	if n := countTaskRunsForReviewTarget(t, env, originalID); n != 1 {
		t.Fatalf("review runs for target = %d, want exactly 1", n)
	}
	reviewID, harness := firstTaskRunForReviewTarget(t, env, originalID)
	if harness != string(HarnessCodex) {
		t.Fatalf("review run harness = %q, want codex (inherited from the target)", harness)
	}
	reviewRun := mustRun(t, env, reviewID)
	if !reviewRun.CodexSecretID.Valid {
		t.Fatal("the inherited-Codex review run must have its own codex_secret_id frozen")
	}
}

// TestMaybeEnqueueTaskReviewUnusableHarnessSkipsLiveDB (D4): a completed --review task
// stamped harness='codex' for an owner with NO usable Codex credential fails closed —
// createRunResolved refuses (ErrNoCredentialForHarness) and NO review row is created. A
// legible skip, not a silent disappearance.
func TestMaybeEnqueueTaskReviewUnusableHarnessSkipsLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, _, repoID := env.seedCodexInfra(t) // no codex credential at all
	svc := atomicSvcForUser(env)

	originalID := seedTaskRun(t, env, userID, repoID, "uzi/task/"+uuid.NewString(), string(HarnessCodex), "completed", true, false)
	original := mustRun(t, env, originalID)

	svc.maybeEnqueueTaskReview(env.ctx, original)

	if n := countTaskRunsForReviewTarget(t, env, originalID); n != 0 {
		t.Fatalf("review runs for target = %d, want 0 (the inherited harness has no usable credential)", n)
	}
}

// seedCompleteTaskReview inserts a task_reviews header (status='complete') plus one
// finding, so maybeEnqueueThenFix's findings gate is satisfied.
func seedCompleteTaskReview(t *testing.T, env codexTestEnv, targetRunID, reviewRunID, userID uuid.UUID) {
	t.Helper()
	reviewHeaderID := uuid.New()
	env.exec(`INSERT INTO task_reviews (id, target_run_id, review_run_id, user_id, status, summary_md)
	          VALUES ($1, $2, $3, $4, 'complete', 'looks fine except one thing')`,
		reviewHeaderID, targetRunID, reviewRunID, userID)
	env.exec(`INSERT INTO task_review_findings (id, review_id, file, symbol, line, severity, summary_md, rationale_md)
	          VALUES ($1, $2, 'main.go', 'run', 10, 'warning', 'a real finding', 'fix it')`,
		uuid.New(), reviewHeaderID)
}

// TestMaybeEnqueueThenFixInheritsCodexHarnessLiveDB (D4): a completed review of a
// --then-fix original with findings auto-creates a fix run FROZEN onto the ORIGINAL's
// harness (codex), not the M1 HarnessClaude stopgap.
func TestMaybeEnqueueThenFixInheritsCodexHarnessLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, repoID := seedCodexOnlyOriginUser(t, env)
	svc := atomicSvcForUser(env)

	branch := "uzi/task/" + uuid.NewString()
	originalID := seedTaskRun(t, env, userID, repoID, branch, string(HarnessCodex), "completed", true, true)
	reviewID := uuid.New()
	env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_title, issue_description, status, branch, review_target_run_id, harness, dispatched_at)
	          VALUES ($1, $2, $3, 'task', 'Review', '', 'completed', $4, $5, $6, now())`,
		reviewID, userID, repoID, branch, originalID, string(HarnessCodex))
	seedCompleteTaskReview(t, env, originalID, reviewID, userID)
	reviewRun := mustRun(t, env, reviewID)

	svc.maybeEnqueueThenFix(env.ctx, reviewRun)

	if n := countTaskRunsForThenFixOriginal(t, env, originalID); n != 1 {
		t.Fatalf("then-fix runs for original = %d, want exactly 1", n)
	}
	fixID, harness := firstTaskRunForThenFixOriginal(t, env, originalID)
	if harness != string(HarnessCodex) {
		t.Fatalf("then-fix run harness = %q, want codex (inherited from the ORIGINAL task)", harness)
	}
	fixRun := mustRun(t, env, fixID)
	if !fixRun.CodexSecretID.Valid {
		t.Fatal("the inherited-Codex then-fix run must have its own codex_secret_id frozen")
	}
}

// TestMaybeEnqueueThenFixUnusableHarnessSkipsLiveDB (D4): the same shape as above, but the
// owner has NO usable Codex credential — createRunResolved fails closed and NO fix row is
// created.
func TestMaybeEnqueueThenFixUnusableHarnessSkipsLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, _, repoID := env.seedCodexInfra(t) // no codex credential at all
	svc := atomicSvcForUser(env)

	branch := "uzi/task/" + uuid.NewString()
	originalID := seedTaskRun(t, env, userID, repoID, branch, string(HarnessCodex), "completed", true, true)
	reviewID := uuid.New()
	env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_title, issue_description, status, branch, review_target_run_id, harness, dispatched_at)
	          VALUES ($1, $2, $3, 'task', 'Review', '', 'completed', $4, $5, $6, now())`,
		reviewID, userID, repoID, branch, originalID, string(HarnessCodex))
	seedCompleteTaskReview(t, env, originalID, reviewID, userID)
	reviewRun := mustRun(t, env, reviewID)

	svc.maybeEnqueueThenFix(env.ctx, reviewRun)

	if n := countTaskRunsForThenFixOriginal(t, env, originalID); n != 0 {
		t.Fatalf("then-fix runs for original = %d, want 0 (the inherited harness has no usable credential)", n)
	}
}
