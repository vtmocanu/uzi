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

// TestSetRunRunningAwaitingApprovalGuardLiveDB pins the PRD #44 F2 guard against a
// REAL Postgres — the SQL WHERE clause the fake-store unit tests cannot exercise. A
// retry-delayed pre-gate `running` report must NOT regress an awaiting_approval run
// back to running (that would silently hide the plan gate, with no self-heal). The
// EXISTS clause lets the transition through only once a consumed approve_plan input
// exists — the legitimate post-approval resume — and leaves claimed→running and
// running→running heartbeats untouched.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (the store IT
// runner provides one); mirrors the other *_integration_test.go here.
func TestSetRunRunningAwaitingApprovalGuardLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via the store integration runner for live-DB coverage")
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

	userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
	gateRun, claimedRun := uuid.New(), uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("f2guard-%s@e2e", userID))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)

	// A unique token_hash: the store IT runner shares one DB across every LiveDB
	// test, and workers.token_hash is UNIQUE — a fixed literal would collide.
	wkr, err := q.CreateWorker(ctx, store.CreateWorkerParams{
		UserID: userID, Name: "laptop", TokenHash: append([]byte("f2guard-"), userID[:]...),
		// 00088 made anthropic_bind_mode NOT NULL with a CHECK, and CreateWorker now
		// names it (PRD #111 M3-BLOCK), so a direct-store call must say which mode it
		// means — the zero value "" is a row the database cannot hold. Deliberately NOT
		// defaulted in SQL: an empty mode silently becoming 'default' is exactly how the
		// mint-time binding regression hid.
		AnthropicBindMode: "default",
	})
	if err != nil {
		t.Fatalf("CreateWorker: %v", err)
	}
	workerID := pgtype.UUID{Bytes: wkr.ID, Valid: true}

	mustExec(ctx, t, pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, worker_id)
		 VALUES ($1, $2, $3, 1, 't', 'd', 'awaiting_approval', $4)`, gateRun, userID, repoID, wkr.ID)
	mustExec(ctx, t, pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, worker_id)
		 VALUES ($1, $2, $3, 2, 't', 'd', 'claimed', $4)`, claimedRun, userID, repoID, wkr.ID)

	statusOf := func(id uuid.UUID) string {
		t.Helper()
		var s string
		if err := pool.QueryRow(ctx, `SELECT status FROM runs WHERE id = $1`, id).Scan(&s); err != nil {
			t.Fatalf("read status of %s: %v", id, err)
		}
		return s
	}
	report := func(id uuid.UUID) int64 {
		t.Helper()
		rows, err := q.SetRunRunning(ctx, store.SetRunRunningParams{ID: id, WorkerID: workerID})
		if err != nil {
			t.Fatalf("SetRunRunning(%s): %v", id, err)
		}
		return rows
	}

	// ── claimed → running is unaffected by the guard (the EXISTS clause only narrows
	//    the awaiting_approval source status). ──
	if rows := report(claimedRun); rows != 1 {
		t.Fatalf("claimed→running: rows = %d, want 1", rows)
	}
	if s := statusOf(claimedRun); s != "running" {
		t.Fatalf("claimed→running: status = %q, want running", s)
	}
	// ── running → running heartbeat still passes. ──
	if rows := report(claimedRun); rows != 1 {
		t.Fatalf("running→running heartbeat: rows = %d, want 1", rows)
	}

	// ── A stale pre-gate report against awaiting_approval, with NO approve_plan input,
	//    is a no-op: the gate is left intact. ──
	if rows := report(gateRun); rows != 0 {
		t.Fatalf("stale pre-gate report (no input): rows = %d, want 0", rows)
	}
	if s := statusOf(gateRun); s != "awaiting_approval" {
		t.Fatalf("stale pre-gate report (no input): status = %q, want awaiting_approval", s)
	}

	// ── A consumed approve_plan on a DIFFERENT run does not unblock this one: the
	//    EXISTS clause is correlated by run_id. ──
	mustExec(ctx, t, pool,
		`INSERT INTO run_user_inputs (run_id, kind, consumed_at) VALUES ($1, 'approve_plan', now())`, claimedRun)
	if rows := report(gateRun); rows != 0 {
		t.Fatalf("stale report with a foreign run's consumed input: rows = %d, want 0", rows)
	}
	if s := statusOf(gateRun); s != "awaiting_approval" {
		t.Fatalf("foreign consumed input: status = %q, want awaiting_approval", s)
	}

	// ── An UNconsumed approve_plan on this run still blocks: only consumed_at IS NOT
	//    NULL counts (a queued verdict the worker has not yet acted on). ──
	mustExec(ctx, t, pool,
		`INSERT INTO run_user_inputs (run_id, kind) VALUES ($1, 'approve_plan')`, gateRun)
	if rows := report(gateRun); rows != 0 {
		t.Fatalf("stale report with an unconsumed approve_plan: rows = %d, want 0", rows)
	}
	if s := statusOf(gateRun); s != "awaiting_approval" {
		t.Fatalf("unconsumed approve_plan: status = %q, want awaiting_approval", s)
	}

	// ── Once the approve_plan is consumed, the legitimate post-approval resume report
	//    transitions awaiting_approval → running. ──
	mustExec(ctx, t, pool,
		`UPDATE run_user_inputs SET consumed_at = now() WHERE run_id = $1 AND kind = 'approve_plan'`, gateRun)
	if rows := report(gateRun); rows != 1 {
		t.Fatalf("post-approval resume report: rows = %d, want 1", rows)
	}
	if s := statusOf(gateRun); s != "running" {
		t.Fatalf("post-approval resume report: status = %q, want running", s)
	}
}
