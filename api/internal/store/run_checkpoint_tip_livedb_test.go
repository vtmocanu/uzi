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

// TestRunCheckpointTipLiveDB is the live-DB gate for the new nullable runs.checkpoint_tip
// column (migration 00184) and its single writer SetRunCheckpointTip, plus the read-back
// path the terminal delete goroutine relies on (GetRunClaimContext now projects
// r.checkpoint_tip). It exercises what a fake store structurally cannot: that the generated
// UPDATE actually runs against a real Postgres, lands the tip on the row, and ADVANCES it on
// a second write (a checkpoint publish happens on every push, not just the first).
//
// It lives in the store package DELIBERATELY: e2e/run-store-it.sh and the CI
// test-api-store-it job run `-run 'LiveDB$'` over ./internal/store/... and
// ./internal/handler/... ONLY, so a *LiveDB test placed elsewhere would never gate.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; e2e/run-store-it.sh
// provides one. A package that prints `ok` with PASS=0 is INVALID, not green.
func TestRunCheckpointTipLiveDB(t *testing.T) {
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
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, userID, fmt.Sprintf("cpt-%s@e2e", userID))
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'github', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, 1, 'g/cpt', 'https://forge.e2e/g/cpt', 'main', true)`, repoID, connID)

	runID := uuid.New()
	exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status)
	      VALUES ($1, $2, $3, 'issue', 1, 'do x', 'ctx', 'running')`, runID, userID, repoID)

	// A freshly-created run has no checkpoint tip yet.
	if run, err := q.GetRunByID(ctx, runID); err != nil {
		t.Fatalf("GetRunByID(initial): %v", err)
	} else if run.CheckpointTip.Valid {
		t.Fatalf("checkpoint_tip = %+v on a fresh run, want NULL", run.CheckpointTip)
	}

	// ── First publish: SetRunCheckpointTip stamps the tip. ──
	const tip1 = "1111111111111111111111111111111111111111"
	rows, err := q.SetRunCheckpointTip(ctx, store.SetRunCheckpointTipParams{
		ID: runID, CheckpointTip: pgtype.Text{String: tip1, Valid: true},
	})
	if err != nil {
		t.Fatalf("SetRunCheckpointTip(tip1): %v", err)
	}
	if rows != 1 {
		t.Fatalf("SetRunCheckpointTip(tip1) moved %d rows, want 1", rows)
	}
	if run, err := q.GetRunByID(ctx, runID); err != nil {
		t.Fatalf("GetRunByID(after tip1): %v", err)
	} else if !run.CheckpointTip.Valid || run.CheckpointTip.String != tip1 {
		t.Fatalf("checkpoint_tip = %+v after first publish, want %q", run.CheckpointTip, tip1)
	}

	// The delete goroutine's read-back path (GetRunClaimContext) sees the same tip.
	if rc, err := q.GetRunClaimContext(ctx, runID); err != nil {
		t.Fatalf("GetRunClaimContext(after tip1): %v", err)
	} else if !rc.CheckpointTip.Valid || rc.CheckpointTip.String != tip1 {
		t.Fatalf("GetRunClaimContext.CheckpointTip = %+v, want %q", rc.CheckpointTip, tip1)
	}

	// ── Second publish OVERWRITES: proves the tip advances, not first-only. ──
	const tip2 = "2222222222222222222222222222222222222222"
	rows, err = q.SetRunCheckpointTip(ctx, store.SetRunCheckpointTipParams{
		ID: runID, CheckpointTip: pgtype.Text{String: tip2, Valid: true},
	})
	if err != nil {
		t.Fatalf("SetRunCheckpointTip(tip2): %v", err)
	}
	if rows != 1 {
		t.Fatalf("SetRunCheckpointTip(tip2) moved %d rows, want 1", rows)
	}
	if run, err := q.GetRunByID(ctx, runID); err != nil {
		t.Fatalf("GetRunByID(after tip2): %v", err)
	} else if !run.CheckpointTip.Valid || run.CheckpointTip.String != tip2 {
		t.Fatalf("checkpoint_tip = %+v after second publish, want it advanced to %q", run.CheckpointTip, tip2)
	}

	// ── Non-vacuity: a WRONG run id moves 0 rows and touches nothing. ──
	if rows, err := q.SetRunCheckpointTip(ctx, store.SetRunCheckpointTipParams{
		ID: uuid.New(), CheckpointTip: pgtype.Text{String: "deadbeef", Valid: true},
	}); err != nil {
		t.Fatalf("SetRunCheckpointTip(wrong id): %v", err)
	} else if rows != 0 {
		t.Fatalf("SetRunCheckpointTip(wrong id) moved %d rows, want 0 (the id predicate is not enforced)", rows)
	}
	if run, err := q.GetRunByID(ctx, runID); err != nil {
		t.Fatalf("GetRunByID(after wrong-id write): %v", err)
	} else if !run.CheckpointTip.Valid || run.CheckpointTip.String != tip2 {
		t.Fatalf("checkpoint_tip = %+v after a wrong-id write, want it untouched at %q", run.CheckpointTip, tip2)
	}
}
