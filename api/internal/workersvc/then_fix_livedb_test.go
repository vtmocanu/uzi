package workersvc

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestThenFixLiveDB exercises PRD #400 M5 end to end against a REAL Postgres — the parts
// only a live DB can answer:
//
//	(a) the terminal-transition hook: maybeEnqueueThenFix on a COMPLETED review run whose
//	    ORIGINAL task has then_fix_requested=true AND a task-review with ≥1 finding creates a
//	    NORMAL fix task run — then_fix_of_run_id = the original, review_target_run_id NULL,
//	    review_requested false, dispatched_at non-null, branch == the original's, and
//	    issue_description carries the composed findings — and the
//	    uq_one_active_then_fix_per_target partial unique index prevents a SECOND active fix;
//	(b) a review with ZERO findings, and a review whose status is 'failed', each create NO
//	    fix run (a clean or failed review needs no fix).
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (named OUTSIDE the
// uzi- namespace, per the store live-DB harness). A package that prints `ok` with PASS=0 is
// INVALID, not green.
func TestThenFixLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh for live-DB coverage")
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
	svc := New(q, nil, Params{})

	userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, userID, fmt.Sprintf("m5-%s@e2e", userID))
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, 1, 'g/m5', 'https://forge.e2e/g/m5', 'main', true)`, repoID, connID)

	// seedChain creates an original --then-fix task (completed) on its uzi/task/<id> branch,
	// a completed review run targeting it, and a task_reviews header of the given status with
	// the given findings. Returns the original id, its branch, and the review run loaded as a
	// store.Run (the exact value the terminal transition hands maybeEnqueueThenFix).
	seedChain := func(thenFix bool, reviewStatus string, findings []store.TaskReviewFinding) (uuid.UUID, string, store.Run) {
		t.Helper()
		origID := uuid.New()
		branch := "uzi/task/" + origID.String()
		exec(`INSERT INTO runs (id, user_id, repo_id, kind, branch, base_branch, review_requested, then_fix_requested, issue_title, issue_description, status, auto_approve)
		      VALUES ($1, $2, $3, 'task', $4, 'develop', true, $5, 'do the thing', 'ctx', 'completed', true)`,
			origID, userID, repoID, branch, thenFix)

		reviewID := uuid.New()
		exec(`INSERT INTO runs (id, user_id, repo_id, kind, branch, base_branch, review_target_run_id, dispatched_at, issue_title, issue_description, status, auto_approve)
		      VALUES ($1, $2, $3, 'task', $4, 'develop', $5, now(), 'review', '', 'completed', true)`,
			reviewID, userID, repoID, branch, origID)

		trID := uuid.New()
		exec(`INSERT INTO task_reviews (id, target_run_id, review_run_id, user_id, status, summary_md)
		      VALUES ($1, $2, $3, $4, $5, 'reviewed')`, trID, origID, reviewID, userID, reviewStatus)
		for _, f := range findings {
			exec(`INSERT INTO task_review_findings (review_id, file, symbol, line, severity, summary_md, rationale_md)
			      VALUES ($1, $2, $3, $4, $5, $6, $7)`, trID, f.File, f.Symbol, f.Line, f.Severity, f.SummaryMd, f.RationaleMd)
		}

		reviewRun, err := q.GetRunByID(ctx, reviewID)
		if err != nil {
			t.Fatalf("load review run: %v", err)
		}
		return origID, branch, reviewRun
	}

	countActiveFixes := func(origID uuid.UUID) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM runs WHERE then_fix_of_run_id = $1 AND status NOT IN ('completed','failed','cancelled')`,
			origID).Scan(&n); err != nil {
			t.Fatalf("count active fixes: %v", err)
		}
		return n
	}

	// ── (a) findings + then_fix_requested → exactly one fix run; a second call is a no-op. ──
	findings := []store.TaskReviewFinding{
		{File: "api/svc.go", Symbol: "run", Line: 42, Severity: "error", SummaryMd: "nil deref", RationaleMd: "x may be nil"},
		{File: "api/util.go", Symbol: "", Line: 0, Severity: "info", SummaryMd: "rename", RationaleMd: "clarity"},
	}
	origID, branch, reviewRun := seedChain(true, "complete", findings)

	svc.maybeEnqueueThenFix(ctx, reviewRun)
	svc.maybeEnqueueThenFix(ctx, reviewRun) // second time: the partial unique index must dedupe

	if got := countActiveFixes(origID); got != 1 {
		t.Fatalf("active fix runs for original = %d, want exactly 1 (partial unique index must prevent a second)", got)
	}

	var fixID uuid.UUID
	var fixKind, fixBranch, fixDesc string
	var reviewTargetNull, dispatchedNotNull, reviewReq, thenFixReq bool
	if err := pool.QueryRow(ctx,
		`SELECT id, kind, branch, issue_description,
		        review_target_run_id IS NULL, dispatched_at IS NOT NULL, review_requested, then_fix_requested
		 FROM runs WHERE then_fix_of_run_id = $1`, origID).
		Scan(&fixID, &fixKind, &fixBranch, &fixDesc, &reviewTargetNull, &dispatchedNotNull, &reviewReq, &thenFixReq); err != nil {
		t.Fatalf("load fix run: %v", err)
	}
	if fixKind != "task" {
		t.Errorf("fix run kind = %q, want task (a fix is a NORMAL task)", fixKind)
	}
	if !reviewTargetNull {
		t.Error("fix run review_target_run_id is NOT NULL; a fix is a plain task the worker implements-and-pushes, not a review")
	}
	if !dispatchedNotNull {
		t.Error("fix run dispatched_at is NULL; a fix must be immediately claimable (its branch already exists)")
	}
	if reviewReq || thenFixReq {
		t.Errorf("fix run review_requested=%v then_fix_requested=%v, want both false (no recursion)", reviewReq, thenFixReq)
	}
	if fixBranch != branch {
		t.Errorf("fix run branch = %q, want the original's %q (the fix pushes where the user pulls)", fixBranch, branch)
	}
	// NON-VACUOUS: the composed description must carry the untrusted framing AND the findings.
	if !strings.Contains(fixDesc, "untrusted") {
		t.Errorf("fix run issue_description missing the untrusted framing:\n%s", fixDesc)
	}
	if !strings.Contains(fixDesc, "api/svc.go:42") || !strings.Contains(fixDesc, "nil deref") {
		t.Errorf("fix run issue_description does not carry the findings:\n%s", fixDesc)
	}

	// ── (b1) a review with ZERO findings creates NO fix run. ──
	emptyOrig, _, emptyReview := seedChain(true, "complete", nil)
	svc.maybeEnqueueThenFix(ctx, emptyReview)
	if got := countActiveFixes(emptyOrig); got != 0 {
		t.Fatalf("a review with zero findings created %d fix runs, want 0 (a clean review needs no fix)", got)
	}

	// ── (b2) a review whose status is 'failed' creates NO fix run, even with findings. ──
	failedOrig, _, failedReview := seedChain(true, "failed", findings)
	svc.maybeEnqueueThenFix(ctx, failedReview)
	if got := countActiveFixes(failedOrig); got != 0 {
		t.Fatalf("a failed review created %d fix runs, want 0 (a failed review has no trustworthy findings)", got)
	}

	// ── (b3) then_fix NOT requested → NO fix run even with a clean review + findings. ──
	noFixOrig, _, noFixReview := seedChain(false, "complete", findings)
	svc.maybeEnqueueThenFix(ctx, noFixReview)
	if got := countActiveFixes(noFixOrig); got != 0 {
		t.Fatalf("a review without then_fix_requested created %d fix runs, want 0", got)
	}
}
