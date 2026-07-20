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

// TestCountRunReviseInputsCountsConsumedRows nails down the load-bearing invariant of
// the PRD #41 plan-revision cap: CountRunReviseInputs — the count(*) the server checks
// against PLAN_MAX_REVISIONS — has NO consumed_at filter, so a revise that has already
// been delivered to the worker (consumed_at stamped) STILL counts toward the cap.
//
// A fakeStore + SQL inspection can't prove this: only the real query, run against a real
// Postgres after a real ConsumeRunInputs, shows that consumption/requeue does not
// decrement the count and thus cannot be used to defeat the cap (PRD #41 Decision 6,
// Success Criterion 5: "including after requeue (consumed rows still count)").
//
// It also checks the kind filter: follow_up / approve_plan rows never inflate the count.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; the store-IT
// runner (e2e/run-store-it.sh) provides one.
func TestCountRunReviseInputsCountsConsumedRows(t *testing.T) {
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

	userID, connID, repoID, runID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("revisecap-%s@e2e", userID))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)
	mustExec(ctx, t, pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status)
		 VALUES ($1, $2, $3, 1, 't', 'd', 'running')`, runID, userID, repoID)

	const n = 3

	// Enqueue N revise_plan inputs via the same insert path the server uses.
	for i := 0; i < n; i++ {
		if _, err := q.CreateRunInput(ctx, store.CreateRunInputParams{
			RunID: runID,
			Kind:  "revise_plan",
			Body:  pgtype.Text{String: fmt.Sprintf("revision %d", i), Valid: true},
		}); err != nil {
			t.Fatalf("CreateRunInput(revise_plan %d): %v", i, err)
		}
	}

	// Non-revise steering inputs must NOT inflate the revise count (kind filter).
	if _, err := q.CreateRunInput(ctx, store.CreateRunInputParams{
		RunID: runID, Kind: "follow_up", Body: pgtype.Text{String: "steer", Valid: true},
	}); err != nil {
		t.Fatalf("CreateRunInput(follow_up): %v", err)
	}
	if _, err := q.CreateRunInput(ctx, store.CreateRunInputParams{
		RunID: runID, Kind: "approve_plan", Body: pgtype.Text{},
	}); err != nil {
		t.Fatalf("CreateRunInput(approve_plan): %v", err)
	}

	// Before consumption: exactly N revises count (follow_up/approve_plan ignored).
	before, err := q.CountRunReviseInputs(ctx, runID)
	if err != nil {
		t.Fatalf("CountRunReviseInputs (before consume): %v", err)
	}
	if before != n {
		t.Fatalf("CountRunReviseInputs before consume = %d, want %d (follow_up/approve_plan must not count)", before, n)
	}

	// Deliver every pending input to the worker — this stamps consumed_at on all of them,
	// exactly as a poll/requeue would.
	consumed, err := q.ConsumeRunInputs(ctx, runID)
	if err != nil {
		t.Fatalf("ConsumeRunInputs: %v", err)
	}
	reviseConsumed := 0
	for _, c := range consumed {
		if c.Kind == "revise_plan" {
			reviseConsumed++
		}
	}
	if reviseConsumed != n {
		t.Fatalf("ConsumeRunInputs stamped %d revise_plan rows, want %d (all revises must be marked consumed)", reviseConsumed, n)
	}

	// After consumption: the count is UNCHANGED. A consumed revise still counts, so the
	// cap is the lifetime number of revisions and cannot be reset by consuming/requeuing.
	after, err := q.CountRunReviseInputs(ctx, runID)
	if err != nil {
		t.Fatalf("CountRunReviseInputs (after consume): %v", err)
	}
	if after != n {
		t.Fatalf("CountRunReviseInputs after consume = %d, want %d — consumption must NOT decrement the cap count", after, n)
	}
}
