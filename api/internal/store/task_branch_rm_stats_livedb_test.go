package store_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestTaskBranchRmStatsLiveDB pins the issue #403 F1/F6 branch-scoped safety query against a
// REAL Postgres. TaskBranchRmStats answers "is this uzi/task/<orig> branch safe to rm?" over
// EVERY kind='task' run sharing the branch (original + auto-review + --then-fix fix), so it
// cannot be modeled by the in-memory fakes — it exercises the FILTER aggregates and the
// user/kind/branch scoping directly:
//
//	(a) a branch whose runs are all terminal and none opened an MR → active_count 0, mr_count 0;
//	(b) a branch with a still-running review child → active_count ≥ 1;
//	(c) a branch whose original set mr_web_url → mr_count ≥ 1;
//	(d) rows on a DIFFERENT branch or a DIFFERENT user are excluded.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (named OUTSIDE the uzi-
// namespace, per the store live-DB harness). A package that prints `ok` with PASS=0 is INVALID.
func TestTaskBranchRmStatsLiveDB(t *testing.T) {
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

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	// seedUser creates a user + forge connection + repo and returns the user and repo ids.
	seedUser := func() (uuid.UUID, uuid.UUID) {
		userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
		exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, userID, fmt.Sprintf("rm-%s@e2e", userID))
		exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		      VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
		exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		      VALUES ($1, $2, 1, $3, $4, 'main', true)`, repoID, connID, fmt.Sprintf("g/%s", repoID), fmt.Sprintf("https://forge.e2e/g/%s", repoID))
		return userID, repoID
	}
	// insertRun inserts a kind='task' run on the given branch with the given status; mrWebURL
	// non-empty sets mr_web_url (only an open_mr original ever does).
	insertRun := func(userID, repoID uuid.UUID, branch, status, mrWebURL string) {
		t.Helper()
		var mr any
		if mrWebURL != "" {
			mr = mrWebURL
		}
		exec(`INSERT INTO runs (id, user_id, repo_id, kind, branch, issue_title, issue_description, status, auto_approve, mr_web_url)
		      VALUES ($1, $2, $3, 'task', $4, 'do the thing', 'ctx', $5, true, $6)`,
			uuid.New(), userID, repoID, branch, status, mr)
	}

	userA, repoA := seedUser()
	userB, repoB := seedUser()

	// ── (a) a branch: original + review + fix all terminal, no MR → 0 active, 0 mr. ──
	branchClean := "uzi/task/" + uuid.New().String()
	insertRun(userA, repoA, branchClean, "completed", "") // original
	insertRun(userA, repoA, branchClean, "completed", "") // auto-review child
	insertRun(userA, repoA, branchClean, "cancelled", "") // fix child
	// Noise that MUST be excluded: another branch (userA) and the same-shaped branch on userB.
	insertRun(userA, repoA, "uzi/task/"+uuid.New().String(), "running", "https://forge/mr/9")
	insertRun(userB, repoB, branchClean, "running", "https://forge/mr/8")

	stats, err := q.TaskBranchRmStats(ctx, store.TaskBranchRmStatsParams{UserID: userA, Branch: pgtype.Text{String: branchClean, Valid: true}})
	if err != nil {
		t.Fatalf("clean-branch stats: %v", err)
	}
	if stats.ActiveCount != 0 {
		t.Errorf("clean branch active_count = %d, want 0 (all runs terminal; foreign-branch/foreign-user rows must be excluded)", stats.ActiveCount)
	}
	if stats.MrCount != 0 {
		t.Errorf("clean branch mr_count = %d, want 0 (no run on this branch opened an MR)", stats.MrCount)
	}

	// ── (b) a still-running review child makes the branch active. ──
	branchActive := "uzi/task/" + uuid.New().String()
	insertRun(userA, repoA, branchActive, "completed", "") // original finished
	insertRun(userA, repoA, branchActive, "running", "")   // review child still live
	stats, err = q.TaskBranchRmStats(ctx, store.TaskBranchRmStatsParams{UserID: userA, Branch: pgtype.Text{String: branchActive, Valid: true}})
	if err != nil {
		t.Fatalf("active-branch stats: %v", err)
	}
	if stats.ActiveCount < 1 {
		t.Errorf("active branch active_count = %d, want >= 1 (a running review child is live)", stats.ActiveCount)
	}
	if stats.MrCount != 0 {
		t.Errorf("active branch mr_count = %d, want 0", stats.MrCount)
	}

	// ── (c) an original that opened an MR makes the branch MR-exempt. ──
	branchMR := "uzi/task/" + uuid.New().String()
	insertRun(userA, repoA, branchMR, "completed", "https://forge/mr/1") // original opened an MR
	insertRun(userA, repoA, branchMR, "completed", "")                   // review child (no MR of its own)
	stats, err = q.TaskBranchRmStats(ctx, store.TaskBranchRmStatsParams{UserID: userA, Branch: pgtype.Text{String: branchMR, Valid: true}})
	if err != nil {
		t.Fatalf("mr-branch stats: %v", err)
	}
	if stats.MrCount < 1 {
		t.Errorf("mr branch mr_count = %d, want >= 1 (the original opened an MR)", stats.MrCount)
	}
	if stats.ActiveCount != 0 {
		t.Errorf("mr branch active_count = %d, want 0 (both runs terminal)", stats.ActiveCount)
	}
}
