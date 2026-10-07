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

// TestSupersedeRunByWorkerLiveDB executes the generated query with a server-composed
// reason and pre-existing failure fields, and verifies ownership and terminal guards.
// The focused workersvc LiveDB test covers the generation fence for both allowed kinds.
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

	// Exercise the query for both branch-maintenance kinds with seeded stale failure fields.
	const stopReason = "superseded by a concurrent branch advance; further publication stopped. cause=remote_branch_advanced; superseding_tip=0123456789abcdef0123456789abcdef01234567"
	for _, kind := range []string{"mr_rework", "ci_fix"} {
		t.Run(kind, func(t *testing.T) {
			runID := uuid.New()
			exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_title, issue_description, pipeline_ref, mr_iid, target_run_id, status, worker_id, pipeline_id, failure_reason, fail_origin)
	      VALUES ($1, $2, $3, $7, 'maintenance', 'ctx', $4, 42, $5, 'running', $6, 123, 'old failure', 'agent_failure')`,
				runID, userID, repoID, branch, sourceRunID, wkr, kind)

			// ── Positive control / non-vacuity: a WRONG worker id must move 0 rows. ──
			rows, err := q.SupersedeRunByWorker(ctx, store.SupersedeRunByWorkerParams{ID: runID, WorkerID: pgUUID(uuid.New()), StopReason: stopReason})
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
			rows, err = q.SupersedeRunByWorker(ctx, store.SupersedeRunByWorkerParams{ID: runID, WorkerID: pgUUID(wkr), StopReason: stopReason})
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
			if !run.StopReason.Valid || run.StopReason.String != stopReason {
				t.Errorf("stop_reason = %+v, want %q", run.StopReason, stopReason)
			}
			if run.FailureReason.Valid {
				t.Errorf("failure_reason = %+v, want NULL", run.FailureReason)
			}
			if !run.FinishedAt.Valid {
				t.Error("finished_at is NULL after a supersede; terminal-run cleanup did not run")
			}

			// ── Terminal guard: a second supersede onto the now-terminal run is a 0-row no-op. ──
			rows, err = q.SupersedeRunByWorker(ctx, store.SupersedeRunByWorkerParams{ID: runID, WorkerID: pgUUID(wkr), StopReason: stopReason})
			if err != nil {
				t.Fatalf("SupersedeRunByWorker(second): %v", err)
			}
			if rows != 0 {
				t.Fatalf("a second supersede onto a terminal run moved %d rows, want 0 (NOT IN terminal guard)", rows)
			}
		})
	}
}
