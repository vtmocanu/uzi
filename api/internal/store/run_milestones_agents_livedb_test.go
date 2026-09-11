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

// TestSetRunRunningMilestonesAgentsCoupledWrite is the permanent guard for PRD #1224 M2's
// SetRunRunning surface: the per-milestone agent attribution (runs.milestones_agents) is
// written COUPLED to the validated milestones_in_progress write (Decision 6), not on its own
// presence. EXECUTION is the point — a green `sqlc generate` cannot prove the CASE keys off
// the milestones_in_progress narg, nor that a nil attribution param on a present in_progress
// resolves to '[]' rather than NULL; only a live UPDATE against Postgres does.
//
// It pins two invariants on one run, in order:
//
//	(a) coupled write + nil-gate untouched — a report that sets in_progress persists both
//	    columns; a later report OMITTING milestones (both params nil) leaves BOTH untouched.
//	(b) D6 no-stale — a report that ADVANCES in_progress but carries no attribution
//	    (MilestonesAgents nil) overwrites milestones_agents to '[]', so a departed lane's
//	    stale entry cannot survive.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.
func TestSetRunRunningMilestonesAgentsCoupledWrite(t *testing.T) {
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
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("magents-%s@e2e", userID))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)

	wkr, err := q.CreateWorker(ctx, store.CreateWorkerParams{
		UserID: userID, Name: "laptop", TokenHash: append([]byte("magents-"), userID[:]...),
		AnthropicBindMode: "auto",
	})
	if err != nil {
		t.Fatalf("CreateWorker: %v", err)
	}
	workerID := pgtype.UUID{Bytes: wkr.ID, Valid: true}

	// A running issue run under this worker — SetRunRunning applies as a running→running
	// heartbeat, which is where a milestone report lands its in_progress + attribution.
	runID := uuid.New()
	mustExec(ctx, t, pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description,
			status, kind, worker_id, started_at, status_since)
		 VALUES ($1,$2,$3,1,'t','d','running','issue',$4, now(), now())`, runID, userID, repoID, wkr.ID)

	// The non-null args SetRunRunning requires (copied from an existing caller): a report that
	// omits milestones passes exactly this, leaving MilestonesInProgress/MilestonesAgents nil.
	baseParams := store.SetRunRunningParams{
		IterationCount: 1, ID: runID, WorkerID: workerID,
		RunMaxIterations: 30, MilestoneBudgetCap: 7, RunTimeoutSeconds: 7200, BudgetWallCeilingSeconds: 28800,
	}

	// jsonbEq asserts a jsonb column equals wantJSON by SEMANTIC jsonb equality (key order /
	// whitespace independent). A NULL column yields NULL (never equal), so it doubles as a
	// not-null check for the positive assertions here.
	jsonbEq := func(t *testing.T, col, wantJSON string) bool {
		t.Helper()
		var eq pgtype.Bool
		if err := pool.QueryRow(ctx, fmt.Sprintf(`SELECT %s = $2::jsonb FROM runs WHERE id=$1`, col), runID, wantJSON).Scan(&eq); err != nil {
			t.Fatalf("compare %s: %v", col, err)
		}
		return eq.Valid && eq.Bool
	}

	t.Run("coupled write persists both columns, and a later omit leaves both untouched", func(t *testing.T) {
		// A report that validly updates in_progress carries the attribution WITH it.
		coupled := baseParams
		coupled.MilestonesInProgress = []byte(`["m1","m2"]`)
		coupled.MilestonesAgents = []byte(`[{"id":"m1","agent":"coder"}]`)
		if rows, err := q.SetRunRunning(ctx, coupled); err != nil || rows != 1 {
			t.Fatalf("SetRunRunning (coupled) = (%d,%v), want (1,nil)", rows, err)
		}
		if !jsonbEq(t, "milestones_in_progress", `["m1","m2"]`) {
			t.Fatal("milestones_in_progress did not persist on the coupled write")
		}
		if !jsonbEq(t, "milestones_agents", `[{"id":"m1","agent":"coder"}]`) {
			t.Fatal("milestones_agents did not persist on the coupled write")
		}

		// A subsequent report OMITTING milestones (both params nil) — the common heartbeat —
		// leaves BOTH columns untouched: the nil in_progress narg gates both CASEs.
		if rows, err := q.SetRunRunning(ctx, baseParams); err != nil || rows != 1 {
			t.Fatalf("SetRunRunning (omit) = (%d,%v), want (1,nil)", rows, err)
		}
		if !jsonbEq(t, "milestones_in_progress", `["m1","m2"]`) {
			t.Fatal("milestones_in_progress must be UNCHANGED when the narg is nil")
		}
		if !jsonbEq(t, "milestones_agents", `[{"id":"m1","agent":"coder"}]`) {
			t.Fatal("milestones_agents must be UNCHANGED when the in_progress narg is nil")
		}
	})

	t.Run("advancing in_progress with no attribution overwrites milestones_agents to []", func(t *testing.T) {
		// D6 no-stale: in_progress advances to {m2} while THIS report declares no attribution
		// (MilestonesAgents nil). Because the write is coupled to the in_progress narg, the
		// departed id m1's stale attribution entry is overwritten to '[]', not preserved.
		advance := baseParams
		advance.MilestonesInProgress = []byte(`["m2"]`)
		advance.MilestonesAgents = nil
		if rows, err := q.SetRunRunning(ctx, advance); err != nil || rows != 1 {
			t.Fatalf("SetRunRunning (advance) = (%d,%v), want (1,nil)", rows, err)
		}
		if !jsonbEq(t, "milestones_in_progress", `["m2"]`) {
			t.Fatal("milestones_in_progress did not advance to [\"m2\"]")
		}
		if !jsonbEq(t, "milestones_agents", `[]`) {
			t.Fatal("D6: advancing in_progress with no attribution must overwrite milestones_agents to '[]' (no stale entry)")
		}
	})
}
