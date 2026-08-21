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

// TestCancelRunByWorkerLiveDB is the live-DB gate for PRD #503 M1's new worker-scoped
// cancel transition (queries/runtime.sql, generated into runtime.sql.go). It exercises the
// part the fake-store unit tests structurally cannot: that the generated CancelRunByWorker
// statement actually runs against a real Postgres and moves a worker-owned, non-terminal run
// to status='cancelled' with fail_origin NULL — the row shape SetState's failed arm now
// routes an operator cancel to (instead of failed+agent_failure).
//
// It lives in the store package, not workersvc, DELIBERATELY: e2e/run-store-it.sh and the
// CI test-api-store-it job run `-run 'LiveDB$'` over ./internal/store/... and
// ./internal/handler/... ONLY, so a *LiveDB test placed in workersvc would never gate.
// CancelRunByWorker is a pure store query, so the store package is both its correct home and
// the one the live-DB harness reaches.
//
// Positive control (non-vacuity): a run owned by a DIFFERENT worker id must move 0 rows,
// proving the WHERE worker_id predicate is enforced — the real transition below is green
// because the row matched, not because the UPDATE is unconditional. A second cancel onto the
// now-terminal run must also move 0 rows, proving the NOT IN (terminal) guard.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; e2e/run-store-it.sh
// provides one. A package that prints `ok` with PASS=0 is INVALID, not green.
func TestCancelRunByWorkerLiveDB(t *testing.T) {
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
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, userID, fmt.Sprintf("cbw-%s@e2e", userID))
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'github', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, 1, 'g/cbw', 'https://forge.e2e/g/cbw', 'main', true)`, repoID, connID)

	wkr := uuid.New()
	// token_hash carries the worker UUID bytes so a re-run against a persistent DB never
	// collides on workers_token_hash_key (Migrate does not truncate).
	exec(`INSERT INTO workers (id, user_id, name, token_hash) VALUES ($1, $2, 'w', $3)`, wkr, userID, wkr[:])

	// A worker-owned, non-terminal run with stop_kind='cancelled' already stamped — exactly
	// what CreateStopVerdictInput leaves on the row before the live worker reports `failed`.
	// awaiting_approval is REC A's "cancel during planning" case.
	runID := uuid.New()
	exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, worker_id, stop_kind)
	      VALUES ($1, $2, $3, 'issue', 7, 'do x', 'ctx', 'awaiting_approval', $4, 'cancelled')`,
		runID, userID, repoID, wkr)

	// ── Positive control / non-vacuity: a WRONG worker id must move 0 rows. ──
	rows, err := q.CancelRunByWorker(ctx, store.CancelRunByWorkerParams{ID: runID, WorkerID: pgUUID(uuid.New())})
	if err != nil {
		t.Fatalf("CancelRunByWorker(wrong worker): %v", err)
	}
	if rows != 0 {
		t.Fatalf("CancelRunByWorker moved %d rows for a non-owning worker; the worker_id guard is not enforced (vacuous)", rows)
	}
	if pre, err := q.GetRunByID(ctx, runID); err != nil {
		t.Fatalf("GetRunByID: %v", err)
	} else if pre.Status != "awaiting_approval" {
		t.Fatalf("run status = %q after a non-owning cancel, want it untouched (awaiting_approval)", pre.Status)
	}

	// ── The real transition: the owning worker cancels. ──
	rows, err = q.CancelRunByWorker(ctx, store.CancelRunByWorkerParams{ID: runID, WorkerID: pgUUID(wkr)})
	if err != nil {
		t.Fatalf("CancelRunByWorker(owning worker): %v", err)
	}
	if rows != 1 {
		t.Fatalf("CancelRunByWorker moved %d rows, want 1", rows)
	}
	run, err := q.GetRunByID(ctx, runID)
	if err != nil {
		t.Fatalf("GetRunByID: %v", err)
	}
	if run.Status != "cancelled" {
		t.Errorf("status = %q, want cancelled", run.Status)
	}
	if run.FailOrigin.Valid {
		t.Errorf("fail_origin = {valid=true %q}, want NULL (a cancel is not a failure)", run.FailOrigin.String)
	}
	if !run.FinishedAt.Valid {
		t.Error("finished_at is NULL after a cancel; terminal-run cleanup did not run")
	}
	// stop_kind is left untouched by the query — it was already 'cancelled'.
	if !run.StopKind.Valid || run.StopKind.String != "cancelled" {
		t.Errorf("stop_kind = %+v, want it preserved as 'cancelled'", run.StopKind)
	}

	// ── Terminal guard: a second cancel onto the now-terminal run is a 0-row no-op. ──
	rows, err = q.CancelRunByWorker(ctx, store.CancelRunByWorkerParams{ID: runID, WorkerID: pgUUID(wkr)})
	if err != nil {
		t.Fatalf("CancelRunByWorker(second): %v", err)
	}
	if rows != 0 {
		t.Fatalf("a second cancel onto a terminal run moved %d rows, want 0 (NOT IN terminal guard)", rows)
	}
}
