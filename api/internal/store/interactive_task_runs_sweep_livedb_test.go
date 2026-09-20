package store_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestInteractiveRunWallClockExemptionLiveDB pins PRD #517 Decision 6 against a REAL
// Postgres: an interactive task run is user-paced (it parks at awaiting_followup between
// follow-ups and can be alive far longer than RUN_TIMEOUT), so it is exempt from the
// wall-clock sweep — bounded instead by the M5 worker idle timeout. `started_at` is
// stamped once and never reset, so on the RESUME back to 'running' its ORIGINAL
// started_at is already past the wall budget; without the exemption the first sweep tick
// would fail a legitimately-resumed long-lived run.
//
// The test is DISCRIMINATING by contrast, not by a single positive: it seeds two runs
// with the SAME stale started_at, differing only in `interactive`. The interactive run
// must survive; the non-interactive run must be PARKED at the wall (PRD #1497 — the wall no
// longer fails; here the shared worker is stale, so ParkRunsAtWall parks the non-interactive
// run server-side). Removing `AND interactive = false` from the wall passes would park the
// interactive run too, which is what makes this non-vacuous.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; mirrors the other
// *_livedb / *_integration_test.go in this package.
func TestInteractiveRunWallClockExemptionLiveDB(t *testing.T) {
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
		userID, fmt.Sprintf("sweep-interactive-%s@e2e", userID))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)

	// A shared STALE worker (heartbeat well before the cutoff) so ParkRunsAtWall parks the
	// non-interactive run server-side (the interactive one is exempt regardless).
	staleWorker := uuid.New()
	mustExec(ctx, t, pool,
		`INSERT INTO workers (id, user_id, name, token_hash, status, last_heartbeat_at)
		 VALUES ($1, $2, 'stale', $3, 'offline', now() - interval '10 years')`, staleWorker, userID, staleWorker[:])

	// Two running task runs with the SAME ancient started_at (well past the 60s wall
	// budget below); the ONLY difference is `interactive`. kind='task' requires issue_iid
	// NULL + branch set (runs_kind_shape), which is the interactive-task shape.
	interactiveID, plainID := uuid.New(), uuid.New()
	mustExec(ctx, t, pool,
		`INSERT INTO runs (id, user_id, repo_id, kind, interactive, branch, issue_title, issue_description, status, worker_id, started_at)
		 VALUES ($1, $2, $3, 'task', true, $4, 't', 'd', 'running', $5, now() - interval '10 years')`,
		interactiveID, userID, repoID, "uzi/task/"+interactiveID.String(), staleWorker)
	mustExec(ctx, t, pool,
		`INSERT INTO runs (id, user_id, repo_id, kind, interactive, branch, issue_title, issue_description, status, worker_id, started_at)
		 VALUES ($1, $2, $3, 'task', false, $4, 't', 'd', 'running', $5, now() - interval '10 years')`,
		plainID, userID, repoID, "uzi/task/"+plainID.String(), staleWorker)

	now := pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true}
	staleCutoff := pgtype.Timestamptz{Time: time.Now().UTC().Add(-5 * time.Minute), Valid: true}
	if _, err := q.RequestWallParks(ctx, store.RequestWallParksParams{
		Now: now, GlobalTimeoutSeconds: 60, WorkerStaleCutoff: staleCutoff,
	}); err != nil {
		t.Fatalf("RequestWallParks: %v", err)
	}
	parked, err := q.ParkRunsAtWall(ctx, store.ParkRunsAtWallParams{
		Now: now, GlobalTimeoutSeconds: 60, WorkerStaleCutoff: staleCutoff, GraceSeconds: 600,
	})
	if err != nil {
		t.Fatalf("ParkRunsAtWall: %v", err)
	}
	parkedIDs := map[uuid.UUID]bool{}
	for _, s := range parked {
		parkedIDs[s.ID] = true
	}

	readStatus := func(id uuid.UUID) string {
		t.Helper()
		var status string
		if err := pool.QueryRow(ctx, `SELECT status FROM runs WHERE id = $1`, id).Scan(&status); err != nil {
			t.Fatalf("read run %s: %v", id, err)
		}
		return status
	}

	// The interactive run must NOT be parked: not in the returned set, and still running.
	// This is the assertion that breaks if `AND interactive = false` is removed.
	if parkedIDs[interactiveID] {
		t.Fatalf("the interactive task run was parked by the wall sweep; PRD #517 Decision 6 exempts it "+
			"(user-paced, bounded by the M5 idle timeout). Its resumed started_at is past the wall budget, "+
			"which is exactly the legitimately-resumed long-lived run the exemption protects: %+v", parked)
	}
	if got := readStatus(interactiveID); got != "running" {
		t.Fatalf("interactive run status = %q after the wall passes, want running (untouched)", got)
	}

	// The non-interactive control MUST be parked — same stale started_at, so this proves
	// the interactive run survived because of `interactive`, not because the passes did
	// nothing at all. PRD #1497 D2: parked (paused), never failed.
	if !parkedIDs[plainID] {
		t.Fatalf("the NON-interactive task run was NOT parked; with a started_at 10 years past a 60s wall and a "+
			"stale worker it must be parked at the wall — the contrast is what makes the interactive exemption "+
			"non-vacuous: %+v", parked)
	}
	if got := readStatus(plainID); got != "paused" {
		t.Fatalf("non-interactive run status = %q after the wall passes, want paused (never failed)", got)
	}
}
