package workersvc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestMRReworkBranchGuardForcedInterleaveLiveDB is the acceptance test for the create-time
// cross-kind branch guard's CONCURRENT-WINDOW arm against a REAL Postgres. It is a
// deterministic forced interleave (no sleep, no wall-clock assertion):
//
//  1. an uncommitted tx (tx1) inserts an active ci_fix on pipeline_ref=agent/issue-N,
//     leaving an uncommitted uq_runs_one_active_branch_ref index entry;
//  2. a goroutine calls svc.CreateAutoMRReworkRun for the SAME ref. Its atomic
//     INSERT … WHERE NOT EXISTS runs under READ COMMITTED, so its subquery cannot see
//     tx1's uncommitted ci_fix — WHERE NOT EXISTS is true and it proceeds to insert an
//     mr_rework row on the same (repo_id, pipeline_ref), which BLOCKS on tx1's
//     uncommitted spanning-index entry;
//  3. tx1 commits — the ci_fix becomes the one active branch run and the blocked insert
//     is arbitrated by the durable uq_runs_one_active_branch_ref index, raising 23505;
//  4. the service maps that constraint-named 23505 to ErrBranchInUse.
//
// This is the acceptance criterion: against the pre-fix read-then-insert code, the losing
// insert's generic 23505 was mapped to ErrActiveMRReworkExists, so this test FAILS before
// the fix (uniqueViolationOn + the WHERE NOT EXISTS insert) and PASSES after it.
//
// The mr_iid on the mr_rework (7600) does not collide with any active mr_rework — there are
// none — so the ONLY conflict is the branch-ref index, making the 23505 unambiguously on
// uq_runs_one_active_branch_ref.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (per the store
// live-DB harness). If the goroutine never blocks within the bound the run is INVALID
// (t.Fatalf), not a red.
func TestMRReworkBranchGuardForcedInterleaveLiveDB(t *testing.T) {
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

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}

	// The repo-ownership check (GetRepoForUser) needs users + forge_connections + repos.
	userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, userID, fmt.Sprintf("bg-%s@e2e", userID))
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, 1, $3, 'https://forge.e2e/g/bg', 'main', true)`, repoID, connID, fmt.Sprintf("g/bg-%s", repoID))

	// A completed source issue run with an opened MR on the branch, serving as target_run_id
	// (runs_kind_shape requires an mr_rework to carry a non-null target_run_id).
	const branch = "agent/issue-760"
	sourceRunID := uuid.New()
	exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, branch, mr_iid, mr_state, status)
	      VALUES ($1, $2, $3, 'issue', 760, 't', 'd', $4, 7600, 'opened', 'completed')`,
		sourceRunID, userID, repoID, branch)

	// tx1: insert an active ci_fix on the SAME pipeline_ref, leaving an UNCOMMITTED
	// uq_runs_one_active_branch_ref entry. ci_fix's shape needs repo_id + pipeline_id +
	// pipeline_ref.
	tx1, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx1: %v", err)
	}
	defer tx1.Rollback(ctx) //nolint:errcheck // no-op after Commit
	if _, err := tx1.Exec(ctx,
		`INSERT INTO runs (id, user_id, repo_id, kind, issue_title, issue_description, pipeline_id, pipeline_ref, status)
		 VALUES ($1, $2, $3, 'ci_fix', 't', 'd', 4242, $4, 'running')`,
		uuid.New(), userID, repoID, branch); err != nil {
		t.Fatalf("tx1 insert ci_fix: %v", err)
	}

	// The goroutine's create blocks on tx1's uncommitted spanning-index entry.
	type result struct {
		run store.Run
		err error
	}
	ch := make(chan result, 1)
	go func() {
		run, err := svc.CreateAutoMRReworkRun(ctx, userID, repoID, branch, 7600, sourceRunID, "Rework MR review", "desc", nil)
		ch <- result{run, err}
	}()

	if !waitBlockedOnCreateMRRework(ctx, t, pool) {
		t.Fatalf("the create goroutine never blocked on a lock within 15s — interleave NOT established, this run is INVALID (not a red)")
	}

	// Commit tx1: the ci_fix is now THE one active branch run, and the blocked mr_rework
	// insert loses to the durable index.
	if err := tx1.Commit(ctx); err != nil {
		t.Fatalf("commit tx1: %v", err)
	}

	r := <-ch
	// Post-fix: the concurrent-window loser is typed ErrBranchInUse (pre-fix: this was the
	// generic mapping's ErrActiveMRReworkExists — the bug this test pins).
	if !errors.Is(r.err, ErrBranchInUse) {
		t.Fatalf("concurrent-window create err = %v, want ErrBranchInUse", r.err)
	}

	// Exactly one create succeeded: the ci_fix committed via tx1. The mr_rework was
	// refused, so no mr_rework row exists on the branch and the ci_fix is the sole active
	// branch run.
	var mrReworkRows int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM runs WHERE repo_id = $1 AND kind = 'mr_rework' AND pipeline_ref = $2`,
		repoID, branch).Scan(&mrReworkRows); err != nil {
		t.Fatalf("count mr_rework rows: %v", err)
	}
	if mrReworkRows != 0 {
		t.Fatalf("mr_rework row count = %d, want 0 (the branch-in-use loser must not have inserted)", mrReworkRows)
	}
	var activeBranchRuns int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM runs
		 WHERE repo_id = $1 AND pipeline_ref = $2
		   AND kind IN ('ci_fix', 'mr_rework')
		   AND status NOT IN ('completed', 'failed', 'cancelled')`,
		repoID, branch).Scan(&activeBranchRuns); err != nil {
		t.Fatalf("count active branch runs: %v", err)
	}
	if activeBranchRuns != 1 {
		t.Fatalf("active branch-run count = %d, want exactly 1 (the committed ci_fix)", activeBranchRuns)
	}
}

// TestCreateAutoMRReworkRunSameMRDuplicateIsActiveExistsLiveDB pins the restored contract
// after the create-query predicate was narrowed to the CROSS-KIND case only: a repeated
// create for the SAME merge request (same pipeline_ref = agent/issue-N, same mr_iid) must
// fall through the WHERE NOT EXISTS guard and be rejected by the uq_runs_one_active_mr_rework
// (repo_id, mr_iid) unique index → ErrActiveMRReworkExists, NOT the branch guard's
// ErrBranchInUse.
//
// Before the fix the predicate matched kind IN ('ci_fix','mr_rework'), so the first
// (committed) mr_rework made the second create see a same-kind sibling → zero rows →
// pgx.ErrNoRows → ErrBranchInUse, wrongly shadowing the same-MR duplicate. This is the
// sequential, committed-path counterpart to the concurrent-window test above; both calls
// commit through the pool, so no goroutines or sleeps are needed.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (per the store
// live-DB harness).
func TestCreateAutoMRReworkRunSameMRDuplicateIsActiveExistsLiveDB(t *testing.T) {
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

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}

	// The repo-ownership check (GetRepoForUser) needs users + forge_connections + repos.
	userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, userID, fmt.Sprintf("dup-%s@e2e", userID))
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, 1, $3, 'https://forge.e2e/g/dup', 'main', true)`, repoID, connID, fmt.Sprintf("g/dup-%s", repoID))

	// A completed source issue run with an opened MR on the branch, serving as target_run_id
	// (runs_kind_shape requires an mr_rework to carry a non-null target_run_id).
	const branch = "agent/issue-760"
	const mrIID int64 = 7600
	sourceRunID := uuid.New()
	exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, branch, mr_iid, mr_state, status)
	      VALUES ($1, $2, $3, 'issue', 760, 't', 'd', $4, $5, 'opened', 'completed')`,
		sourceRunID, userID, repoID, branch, mrIID)

	// First create SUCCEEDS: no active branch run, no active rework → the WHERE NOT EXISTS
	// guard passes and the row inserts.
	run, err := svc.CreateAutoMRReworkRun(ctx, userID, repoID, branch, mrIID, sourceRunID, "Rework MR review", "desc", nil)
	if err != nil {
		t.Fatalf("first CreateAutoMRReworkRun err = %v, want success", err)
	}
	if run.Kind != runkind.MRRework {
		t.Fatalf("first run.Kind = %q, want %q", run.Kind, runkind.MRRework)
	}

	// Second create for the SAME branch + SAME mr_iid: the narrowed predicate no longer
	// matches the committed mr_rework, so this proceeds past WHERE NOT EXISTS and is rejected
	// by uq_runs_one_active_mr_rework (repo_id, mr_iid) → ErrActiveMRReworkExists (NOT
	// ErrBranchInUse, which was the pre-fix behavior).
	_, err = svc.CreateAutoMRReworkRun(ctx, userID, repoID, branch, mrIID, sourceRunID, "Rework MR review", "desc", nil)
	if !errors.Is(err, ErrActiveMRReworkExists) {
		t.Fatalf("same-MR duplicate create err = %v, want ErrActiveMRReworkExists", err)
	}
}

// waitBlockedOnCreateMRRework proves, from a THIRD connection, that the racing
// CreateAutoMRReworkRun is parked on a lock rather than merely slow. Returns false if it
// cannot establish that within the bound; the caller must then invalidate the run rather
// than report a red. Mirrors store.repro106WaitBlockedOnLock.
func waitBlockedOnCreateMRRework(ctx context.Context, t *testing.T, pool *pgxpool.Pool) bool {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		// The generated const carries its own `-- name: CreateAutoMRReworkRun :one` header,
		// so pg_stat_activity.query identifies the waiter unambiguously.
		if err := pool.QueryRow(ctx, `
			SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database()
			  AND state = 'active'
			  AND wait_event_type = 'Lock'
			  AND query LIKE '%CreateAutoMRReworkRun%'`).Scan(&n); err != nil {
			t.Fatalf("pg_stat_activity: %v", err)
		}
		if n > 0 {
			t.Logf("confirmed: %d caller(s) blocked with wait_event_type=Lock on CreateAutoMRReworkRun", n)
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}
