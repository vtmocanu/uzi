package store_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// TestWorkerConcurrencyLiveDB exercises the PRD #42 M3a schema + queries against a
// REAL Postgres — the surface the fake-store unit tests cannot cover because they
// never run the SQL: RegisterWorker persisting (and overwriting) the advertised
// max_concurrent_runs cap, and the ListWorkersByUser / ListAllWorkers active_runs
// count (the EXISTS→count(*) change, Decision 10). The count includes exactly the
// claimed/running/awaiting_approval statuses — queued and the terminal statuses are
// excluded — and busy stays derivable as active_runs > 0.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; the store
// e2e runner (e2e/run-store-it.sh) provides one.
func TestWorkerConcurrencyLiveDB(t *testing.T) {
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

	userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("wconc-%s@e2e", userID))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)

	// workers.token_hash is globally UNIQUE (00020), so a constant literal collides on
	// workers_token_hash_key the SECOND time this runs against a DB that outlives the
	// invocation -- a KEEP_STACK=1 re-run, not CI, which spins a throwaway Postgres per
	// invocation. The failure reads as a real one (issue #107). tokenHash() derives 32
	// unique bytes from two uuid.New()s, which is what every other LiveDB test here does.
	wkr, err := q.CreateWorker(ctx, store.CreateWorkerParams{
		UserID: userID, Name: "laptop", TokenHash: tokenHash(),
		AnthropicBindMode: "default",
	})
	if err != nil {
		t.Fatalf("CreateWorker: %v", err)
	}
	// A fresh worker has advertised no cap yet.
	if wkr.MaxConcurrentRuns.Valid {
		t.Fatalf("fresh worker max_concurrent_runs must be NULL, got %+v", wkr.MaxConcurrentRuns)
	}

	// ── Register advertising cap 2 persists it. ──
	if _, err := q.RegisterWorker(ctx, store.RegisterWorkerParams{
		ID: wkr.ID, Version: pgtype.Text{String: "1.2.3", Valid: true},
		MaxConcurrentRuns: pgtype.Int4{Int32: 2, Valid: true},
	}); err != nil {
		t.Fatalf("RegisterWorker(cap=2): %v", err)
	}
	got, err := q.GetWorkerByID(ctx, wkr.ID)
	if err != nil {
		t.Fatalf("GetWorkerByID: %v", err)
	}
	if !got.MaxConcurrentRuns.Valid || got.MaxConcurrentRuns.Int32 != 2 {
		t.Fatalf("advertised cap not persisted: %+v", got.MaxConcurrentRuns)
	}

	// ── A later register advertising nothing overwrites the cap back to NULL (the
	//    fresh-start "declare current state" semantics, mirroring template_reported). ──
	if _, err := q.RegisterWorker(ctx, store.RegisterWorkerParams{
		ID: wkr.ID, Version: pgtype.Text{String: "1.2.4", Valid: true},
	}); err != nil {
		t.Fatalf("RegisterWorker(cap=NULL): %v", err)
	}
	if got, _ = q.GetWorkerByID(ctx, wkr.ID); got.MaxConcurrentRuns.Valid {
		t.Fatalf("re-register without a cap must clear it to NULL, got %+v", got.MaxConcurrentRuns)
	}
	// Re-advertise cap 3 so the count assertions run against a worker with a cap.
	if _, err := q.RegisterWorker(ctx, store.RegisterWorkerParams{
		ID: wkr.ID, Version: pgtype.Text{String: "1.2.5", Valid: true},
		MaxConcurrentRuns: pgtype.Int4{Int32: 3, Valid: true},
	}); err != nil {
		t.Fatalf("RegisterWorker(cap=3): %v", err)
	}

	// ── Seed runs: three ACTIVE (claimed/running/awaiting_approval) held by the
	//    worker, plus a queued and a completed one that must NOT be counted. ──
	seedRun := func(iid int64, status string, holder *uuid.UUID) {
		id := uuid.New()
		mustExec(ctx, t, pool,
			`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, worker_id)
			 VALUES ($1, $2, $3, $4, 't', 'd', $5, $6)`, id, userID, repoID, iid, status, holder)
	}
	for i, st := range []string{"claimed", "running", "awaiting_approval"} {
		seedRun(int64(10+i), st, &wkr.ID)
	}
	seedRun(20, "queued", &wkr.ID)    // active_runs excludes queued
	seedRun(21, "completed", &wkr.ID) // and excludes terminal
	seedRun(22, "running", nil)       // a run held by no worker must not inflate the count

	// seedChatRun inserts an ACTIVE chat run (kind='chat' ⇒ repo_id/issue_iid/branch
	// all NULL per runs_kind_shape) held by holder. A chat is active for busy but is
	// EXCLUDED from active_runs (it does not consume a run-lane slot; PRD #42).
	seedChatRun := func(status string, holder uuid.UUID) {
		id := uuid.New()
		mustExec(ctx, t, pool,
			`INSERT INTO runs (id, user_id, kind, issue_title, issue_description, status, worker_id)
			 VALUES ($1, $2, 'chat', 't', 'd', $3, $4)`, id, userID, status, holder)
	}
	// wkr also holds a live chat: it must NOT bump active_runs past the 3 run-lane runs.
	seedChatRun("running", wkr.ID)

	// A SECOND worker holds ONLY an active chat: busy=true, active_runs=0 (the exact
	// case the naive "busy = active_runs > 0" derivation would get wrong).
	wkr2, err := q.CreateWorker(ctx, store.CreateWorkerParams{
		UserID: userID, Name: "chat-only", TokenHash: tokenHash(),
		AnthropicBindMode: "default",
	})
	if err != nil {
		t.Fatalf("CreateWorker(wkr2): %v", err)
	}
	seedChatRun("running", wkr2.ID)

	const wantActive = 3

	// ── ListWorkersByUser: wkr = active_runs 3 / busy true / cap 3; the chat-only
	//    worker = active_runs 0 but busy true. ──
	byUser, err := q.ListWorkersByUser(ctx, userID)
	if err != nil {
		t.Fatalf("ListWorkersByUser: %v", err)
	}
	var sawWkr, sawChatOnly bool
	for _, row := range byUser {
		switch row.ID {
		case wkr.ID:
			sawWkr = true
			if row.ActiveRuns != wantActive {
				t.Fatalf("ListWorkersByUser active_runs = %d, want %d (chat must not count)", row.ActiveRuns, wantActive)
			}
			if !row.Busy {
				t.Fatalf("ListWorkersByUser busy = false, want true")
			}
			if !row.MaxConcurrentRuns.Valid || row.MaxConcurrentRuns.Int32 != 3 {
				t.Fatalf("ListWorkersByUser cap = %+v, want 3", row.MaxConcurrentRuns)
			}
		case wkr2.ID:
			sawChatOnly = true
			if row.ActiveRuns != 0 {
				t.Fatalf("chat-only worker active_runs = %d, want 0 (chat excluded)", row.ActiveRuns)
			}
			if !row.Busy {
				t.Fatalf("chat-only worker busy = false, want true (a live chat is still busy)")
			}
		}
	}
	if !sawWkr || !sawChatOnly {
		t.Fatalf("ListWorkersByUser missing a worker: sawWkr=%v sawChatOnly=%v", sawWkr, sawChatOnly)
	}

	// ── ListAllWorkers (admin): same busy/active_runs split on the embedded rows. ──
	all, err := q.ListAllWorkers(ctx)
	if err != nil {
		t.Fatalf("ListAllWorkers: %v", err)
	}
	sawWkr, sawChatOnly = false, false
	for _, row := range all {
		switch row.Worker.ID {
		case wkr.ID:
			sawWkr = true
			if row.ActiveRuns != wantActive {
				t.Fatalf("ListAllWorkers active_runs = %d, want %d", row.ActiveRuns, wantActive)
			}
			if !row.Busy {
				t.Fatalf("ListAllWorkers busy = false, want true")
			}
			if !row.Worker.MaxConcurrentRuns.Valid || row.Worker.MaxConcurrentRuns.Int32 != 3 {
				t.Fatalf("ListAllWorkers cap = %+v, want 3", row.Worker.MaxConcurrentRuns)
			}
		case wkr2.ID:
			sawChatOnly = true
			if row.ActiveRuns != 0 || !row.Busy {
				t.Fatalf("ListAllWorkers chat-only worker = active_runs %d busy %v, want 0/true", row.ActiveRuns, row.Busy)
			}
		}
	}
	if !sawWkr || !sawChatOnly {
		t.Fatalf("ListAllWorkers missing a worker: sawWkr=%v sawChatOnly=%v", sawWkr, sawChatOnly)
	}
}
