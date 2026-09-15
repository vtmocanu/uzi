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

// TestSupersedeRunByWorkerLiveDB is the live-DB gate for issue #1117's new worker-scoped
// supersession transition (queries/runtime.sql, generated into runtime.sql.go). It exercises
// the part the fake-store unit tests structurally cannot: that the generated
// SupersedeRunByWorker statement actually runs against a real Postgres and moves a
// worker-owned, non-terminal mr_rework run to status='cancelled' with stop_kind='branch_moved',
// fail_origin NULL, and a static stop_reason — the row shape SetState's failed arm now routes
// an mr_rework branch_moved report to (instead of failed+agent_failure). Because it stamps
// stop_kind='branch_moved', it also proves migration 00230 widened runs_stop_kind_check to
// accept that value: a missing CHECK value would raise PG error 23514 right here.
//
// It lives in the store package, not workersvc, DELIBERATELY: e2e/run-store-it.sh and the
// CI test-api-store-it job run `-run 'LiveDB$'` over ./internal/store/... and
// ./internal/handler/... ONLY, so a *LiveDB test placed in workersvc would never gate.
// SupersedeRunByWorker is a pure store query, so the store package is both its correct home
// and the one the live-DB harness reaches.
//
// Positive control (non-vacuity): a run owned by a DIFFERENT worker id must move 0 rows,
// proving the WHERE worker_id predicate is enforced. A second supersession onto the
// now-terminal run must also move 0 rows, proving the NOT IN (terminal) guard.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; e2e/run-store-it.sh
// provides one. A package that prints `ok` with PASS=0 is INVALID, not green.
func TestSupersedeRunByWorkerLiveDB(t *testing.T) {
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

	pgUUID := func(id uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: id, Valid: true} }

	userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, userID, fmt.Sprintf("sbw-%s@e2e", userID))
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'github', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, 1, 'g/sbw', 'https://forge.e2e/g/sbw', 'main', true)`, repoID, connID)

	wkr := uuid.New()
	// token_hash carries the worker UUID bytes so a re-run against a persistent DB never
	// collides on workers_token_hash_key (Migrate does not truncate).
	exec(`INSERT INTO workers (id, user_id, name, token_hash) VALUES ($1, $2, 'w', $3)`, wkr, userID, wkr[:])

	// runs_kind_shape (migration 00167) requires an mr_rework row to carry a non-null
	// pipeline_ref (its branch), mr_iid, and target_run_id — the last a FK to the source
	// issue run the rework folds onto. Insert that completed source run first so the FK and
	// the shape CHECK are satisfied; issue_iid stays NULL on the mr_rework row.
	const branch = "agent/issue-42"
	sourceRunID := uuid.New()
	exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, branch, mr_iid, mr_state, status)
	      VALUES ($1, $2, $3, 'issue', 42, 't', 'd', $4, 42, 'opened', 'completed')`,
		sourceRunID, userID, repoID, branch)

	// A worker-owned, non-terminal mr_rework run — exactly what a live rework worker owns when
	// its finalize push is rejected non-fast-forward by a concurrent same-branch writer.
	runID := uuid.New()
	exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_title, issue_description, pipeline_ref, mr_iid, target_run_id, status, worker_id)
	      VALUES ($1, $2, $3, 'mr_rework', 'rework x', 'ctx', $4, 42, $5, 'running', $6)`,
		runID, userID, repoID, branch, sourceRunID, wkr)

	// ── Positive control / non-vacuity: a WRONG worker id must move 0 rows. ──
	rows, err := q.SupersedeRunByWorker(ctx, store.SupersedeRunByWorkerParams{ID: runID, WorkerID: pgUUID(uuid.New())})
	if err != nil {
		t.Fatalf("SupersedeRunByWorker(wrong worker): %v", err)
	}
	if rows != 0 {
		t.Fatalf("SupersedeRunByWorker moved %d rows for a non-owning worker; the worker_id guard is not enforced (vacuous)", rows)
	}
	if pre, err := q.GetRunByID(ctx, runID); err != nil {
		t.Fatalf("GetRunByID: %v", err)
	} else if pre.Status != "running" {
		t.Fatalf("run status = %q after a non-owning supersede, want it untouched (running)", pre.Status)
	}

	// ── The real transition: the owning worker supersedes. ──
	rows, err = q.SupersedeRunByWorker(ctx, store.SupersedeRunByWorkerParams{ID: runID, WorkerID: pgUUID(wkr)})
	if err != nil {
		t.Fatalf("SupersedeRunByWorker(owning worker): %v", err)
	}
	if rows != 1 {
		t.Fatalf("SupersedeRunByWorker moved %d rows, want 1", rows)
	}
	run, err := q.GetRunByID(ctx, runID)
	if err != nil {
		t.Fatalf("GetRunByID: %v", err)
	}
	if run.Status != "cancelled" {
		t.Errorf("status = %q, want cancelled", run.Status)
	}
	if !run.StopKind.Valid || run.StopKind.String != "branch_moved" {
		t.Errorf("stop_kind = %+v, want 'branch_moved' (migration 00230 must accept it)", run.StopKind)
	}
	if run.FailOrigin.Valid {
		t.Errorf("fail_origin = {valid=true %q}, want NULL (a supersession is not a failure)", run.FailOrigin.String)
	}
	if !run.StopReason.Valid || run.StopReason.String == "" {
		t.Errorf("stop_reason = %+v, want the static supersession reason", run.StopReason)
	}
	if !run.FinishedAt.Valid {
		t.Error("finished_at is NULL after a supersede; terminal-run cleanup did not run")
	}

	// ── Terminal guard: a second supersede onto the now-terminal run is a 0-row no-op. ──
	rows, err = q.SupersedeRunByWorker(ctx, store.SupersedeRunByWorkerParams{ID: runID, WorkerID: pgUUID(wkr)})
	if err != nil {
		t.Fatalf("SupersedeRunByWorker(second): %v", err)
	}
	if rows != 0 {
		t.Fatalf("a second supersede onto a terminal run moved %d rows, want 0 (NOT IN terminal guard)", rows)
	}
}
