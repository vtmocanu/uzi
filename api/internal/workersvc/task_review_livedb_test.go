package workersvc

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestTaskReviewLiveDB exercises PRD #400 M4a end to end against a REAL Postgres — the
// parts only a live DB can answer:
//
//	(a) the terminal-transition hook: maybeEnqueueTaskReview on a COMPLETED, review_requested
//	    task creates a review run carrying review_target_run_id + a non-null dispatched_at,
//	    and the uq_one_active_task_review_per_target partial unique index prevents a SECOND
//	    active review for the same target;
//	(b) the write→read round-trip: PostTaskReview (worker-authorized) → GetTaskReviewPanel
//	    persists and returns a header + findings NON-VACUOUSLY (file/line/severity survive).
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (named OUTSIDE the
// uzi- namespace, per the store live-DB harness). A package that prints `ok` with PASS=0 is
// INVALID, not green.
func TestTaskReviewLiveDB(t *testing.T) {
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
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, userID, fmt.Sprintf("m4a-%s@e2e", userID))
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, 1, 'g/m4a', 'https://forge.e2e/g/m4a', 'main', true)`, repoID, connID)

	// A COMPLETED, review_requested task run — the exact shape the terminal transition hands
	// maybeEnqueueTaskReview. Its branch is the server-named uzi/task/<id> the review clones.
	taskID := uuid.New()
	branch := "uzi/task/" + taskID.String()
	exec(`INSERT INTO runs (id, user_id, repo_id, kind, branch, base_branch, review_requested, issue_title, issue_description, status, auto_approve)
	      VALUES ($1, $2, $3, 'task', $4, 'develop', true, 'do the thing', 'ctx', 'completed', true)`,
		taskID, userID, repoID, branch)

	// ── (a) the hook creates one review run; a second call is a no-op (23505 swallowed). ──
	run, err := q.GetRunByID(ctx, taskID)
	if err != nil {
		t.Fatalf("load task run: %v", err)
	}
	svc.maybeEnqueueTaskReview(ctx, run)
	svc.maybeEnqueueTaskReview(ctx, run) // second time: the partial unique index must dedupe

	var activeReviews int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM runs WHERE review_target_run_id = $1 AND status NOT IN ('completed','failed','cancelled')`,
		taskID).Scan(&activeReviews); err != nil {
		t.Fatalf("count active reviews: %v", err)
	}
	if activeReviews != 1 {
		t.Fatalf("active review runs for target = %d, want exactly 1 (partial unique index must prevent a second)", activeReviews)
	}

	// The review run itself: kind='task', review_target_run_id set, dispatched_at non-null
	// (immediately claimable — a review needs no CLI seed push), branch == the target's.
	var reviewID uuid.UUID
	var revKind, revBranch string
	var dispatchedNotNull bool
	if err := pool.QueryRow(ctx,
		`SELECT id, kind, branch, dispatched_at IS NOT NULL FROM runs WHERE review_target_run_id = $1`,
		taskID).Scan(&reviewID, &revKind, &revBranch, &dispatchedNotNull); err != nil {
		t.Fatalf("load review run: %v", err)
	}
	if revKind != "task" {
		t.Errorf("review run kind = %q, want task", revKind)
	}
	if !dispatchedNotNull {
		t.Error("review run dispatched_at is NULL; a review must be immediately claimable")
	}
	if revBranch != branch {
		t.Errorf("review run branch = %q, want the target's %q", revBranch, branch)
	}

	// ── (b) PostTaskReview (worker-authorized) → GetTaskReviewPanel round-trips findings. ──
	// The worker must OWN the active review run (worker_id set, non-terminal status) for
	// authorizeTaskReviewTarget to admit the POST.
	// token_hash is UNIQUE and the live-DB sweep shares one database across packages
	// (nothing truncates workers between package binaries), so a fixed literal here
	// collides with whichever earlier package's test inserted the same one — measured
	// against handler's denied_cli_dismiss fixture. Derive it from a fresh uuid.
	wkr := store.Worker{ID: uuid.New(), UserID: userID}
	tokenHash := uuid.New()
	exec(`INSERT INTO workers (id, user_id, name, token_hash) VALUES ($1, $2, 'w', $3)`, wkr.ID, userID, tokenHash[:])
	exec(`UPDATE runs SET worker_id = $1, status = 'running' WHERE id = $2`, wkr.ID, reviewID)

	sub := TaskReviewSubmission{
		Status:    "complete",
		SummaryMd: "one real bug, one nit",
		Findings: []TaskReviewFinding{
			{File: "api/main.go", Symbol: "run", Line: 42, Severity: "error", SummaryMd: "nil deref", RationaleMd: "x may be nil"},
			{File: "api/util.go", Symbol: "", Line: 0, Severity: "info", SummaryMd: "rename", RationaleMd: "clarity"},
		},
	}
	if err := svc.PostTaskReview(ctx, wkr, taskID, sub); err != nil {
		t.Fatalf("PostTaskReview: %v", err)
	}

	got, err := svc.GetTaskReviewPanel(ctx, userID, false, taskID)
	if err != nil {
		t.Fatalf("GetTaskReviewPanel: %v", err)
	}
	if got == nil {
		t.Fatal("GetTaskReviewPanel returned nil after a POST; expected the header + findings")
	}
	if got.Review.Status != "complete" || got.Review.SummaryMd != "one real bug, one nit" {
		t.Errorf("review header = {%q, %q}, want {complete, \"one real bug, one nit\"}", got.Review.Status, got.Review.SummaryMd)
	}
	if got.Review.ReviewRunID.Valid && uuid.UUID(got.Review.ReviewRunID.Bytes) != reviewID {
		t.Errorf("review_run_id = %v, want the review run %v", uuid.UUID(got.Review.ReviewRunID.Bytes), reviewID)
	}
	if len(got.Findings) != 2 {
		t.Fatalf("findings count = %d, want 2", len(got.Findings))
	}
	// NON-VACUOUS: the error finding sorts first (severity rank), and its file/line/severity
	// must survive the jsonb round-trip verbatim.
	first := got.Findings[0]
	if first.Severity != "error" || first.File != "api/main.go" || first.Line != 42 {
		t.Errorf("first finding = {sev=%q file=%q line=%d}, want {error api/main.go 42}", first.Severity, first.File, first.Line)
	}

	// A re-review REPLACES (UPSERT semantics), not 23505: one fewer finding lands.
	sub2 := TaskReviewSubmission{Status: "complete", SummaryMd: "re-reviewed", Findings: []TaskReviewFinding{
		{File: "api/main.go", Line: 7, Severity: "warning", SummaryMd: "still off", RationaleMd: "y"},
	}}
	if err := svc.PostTaskReview(ctx, wkr, taskID, sub2); err != nil {
		t.Fatalf("re-PostTaskReview: %v", err)
	}
	got2, err := svc.GetTaskReviewPanel(ctx, userID, false, taskID)
	if err != nil {
		t.Fatalf("GetTaskReviewPanel after re-review: %v", err)
	}
	if got2 == nil || len(got2.Findings) != 1 || got2.Review.SummaryMd != "re-reviewed" {
		t.Fatalf("re-review did not REPLACE: header/findings = %+v", got2)
	}
}
