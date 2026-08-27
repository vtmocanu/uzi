package store_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestRunScheduleDefaultOriginShapeLiveDB is the mandatory live-DB coverage for the
// PRD #589 M1 origin-gated shape relaxation (migration 00152). A green migration
// apply does not prove the reworked run_schedules_target_shape CHECK behaves, so
// this exercises both sides against real Postgres:
//   - a 'default'-origin 'prompt' row with a NULL prompt is ACCEPTED (its prompt
//     comes from the builtin schedtmpl catalog, resolved in Go at fire time);
//   - a 'user'-origin 'prompt' row with a NULL prompt is REJECTED by the CHECK.
//
// Inserts are raw SQL rather than sqlc calls because M1 adds the columns and the
// constraint only; the generated CreateRunSchedule does not yet carry origin.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres;
// e2e/run-store-it.sh provides one.
func TestRunScheduleDefaultOriginShapeLiveDB(t *testing.T) {
	ctx := context.Background()

	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)

	// Seed a user + forge connection + repo the schedules hang off.
	userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
	mustExec(ctx, t, pool,
		`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("origin-%s@e2e", userID))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', $3, $4, $5)`,
		connID, userID, "bot-origin", 7202, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, enabled)
		 VALUES ($1, $2, 7202, 'g/origin', 'https://forge.e2e/g/origin', true)`,
		repoID, connID)

	// (a) A 'default'-origin 'prompt' row with a NULL prompt is accepted.
	mustExec(ctx, t, pool,
		`INSERT INTO run_schedules (user_id, repo_id, target, prompt, timing, cron_expr, timezone, origin, catalog_slug)
		 VALUES ($1, $2, 'prompt', NULL, 'recurring', '0 8 * * 1', 'UTC', 'default', 'test-improvement')`,
		userID, repoID)

	// (b) A 'user'-origin 'prompt' row with a NULL prompt is rejected by the shape CHECK.
	_, err = pool.Exec(ctx,
		`INSERT INTO run_schedules (user_id, repo_id, target, prompt, timing, cron_expr, timezone, origin)
		 VALUES ($1, $2, 'prompt', NULL, 'recurring', '0 8 * * 1', 'UTC', 'user')`,
		userID, repoID)
	if err == nil {
		t.Fatal("a user-origin prompt row with a NULL prompt was accepted; the shape CHECK must reject it")
	}
	if !strings.Contains(err.Error(), "run_schedules_target_shape") {
		t.Fatalf("rejection error = %v, want a run_schedules_target_shape constraint violation", err)
	}
}
