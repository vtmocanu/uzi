package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/schedsvc"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Live-DB coverage for the PRD #590 M2 boot-migration MigrateLegacySelfImprove
// (api/internal/schedsvc/self_improve_migrate.go). The subject reads the legacy,
// engine-configured selfimprove_* app_settings rows and, when the install was enabled
// against a valid owned repo, materializes exactly one self_improve default-schedule row
// and DELETES the five legacy keys — all in one transaction, idempotent, ON CONFLICT DO
// NOTHING. None of that is provable off a live Postgres: the transaction, the ON CONFLICT
// guard, the GetRepoForUser owner-scoping JOIN, and the DeleteAppSettings retire are all
// SQL. These tests live in package store_test (external), which may import schedsvc without
// an import cycle: schedsvc -> store, and store_test -> schedsvc.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres;
// e2e/run-store-it.sh provides one. The fixture helpers (selfImproveFixture,
// seedSelfImproveRepo, mustExec) live in self_improve_dedup_livedb_test.go.
//
// Isolation note: the legacy keys are GLOBAL app_settings rows (not user-scoped), so each
// test seeds all five of its own keys at the start (UpsertAppSetting overwrites, making the
// starting state deterministic regardless of what a sibling leaked), and the NON-deleting
// tests (disabled / disconnected / unconfigured) delete the keys in a t.Cleanup so no
// selfimprove_% row leaks into a sibling test. The success-path tests need no cleanup — the
// migration itself deletes the keys — but register one defensively anyway. run_schedules
// rows stay isolated by the fixture's random per-test user id.

// legacySIKeys is the exact five-key set the migration reads and retires.
var legacySIKeys = []string{
	"selfimprove_enabled",
	"selfimprove_interval",
	"selfimprove_repo",
	"selfimprove_user_id",
	"selfimprove_last_run_at",
}

// migrateFixedNow is a deterministic "now" so NextFire never depends on wall-clock; the
// migration derives next_fire_at from the cron, not from this value's relation to any fire.
var migrateFixedNow = time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)

// seedLegacySI upserts one legacy app_settings key.
func seedLegacySI(ctx context.Context, t *testing.T, q *store.Queries, key, value string) {
	t.Helper()
	if _, err := q.UpsertAppSetting(ctx, store.UpsertAppSettingParams{
		Key: key, Value: value, UpdatedBy: pgtype.UUID{},
	}); err != nil {
		t.Fatalf("seed legacy key %q: %v", key, err)
	}
}

// currentLegacyKeys returns the set of selfimprove_* keys currently present in app_settings.
func currentLegacyKeys(ctx context.Context, t *testing.T, q *store.Queries) map[string]string {
	t.Helper()
	rows, err := q.ListAppSettings(ctx)
	if err != nil {
		t.Fatalf("ListAppSettings: %v", err)
	}
	present := map[string]string{}
	for _, r := range rows {
		for _, k := range legacySIKeys {
			if r.Key == k {
				present[r.Key] = r.Value
			}
		}
	}
	return present
}

// selfImproveRowsForUser returns the user's run_schedules rows whose target is self_improve.
func selfImproveRowsForUser(ctx context.Context, t *testing.T, q *store.Queries, userID uuid.UUID) []store.RunSchedule {
	t.Helper()
	all, err := q.ListRunSchedulesForUser(ctx, userID)
	if err != nil {
		t.Fatalf("ListRunSchedulesForUser: %v", err)
	}
	var out []store.RunSchedule
	for _, s := range all {
		if s.Target == "self_improve" {
			out = append(out, s)
		}
	}
	return out
}

// TestMigrateLegacySelfImproveEnabledLiveDB — DoD #1: an enabled legacy install against a
// valid owned repo materializes EXACTLY ONE self_improve default schedule (48h -> the
// every-2-days cron) and RETIRES all five legacy keys, in one transaction.
func TestMigrateLegacySelfImproveEnabledLiveDB(t *testing.T) {
	ctx := context.Background()
	q, pool, userID, connID := selfImproveFixture(ctx, t)
	repoID := seedSelfImproveRepo(ctx, t, pool, connID, 8501, "migrate-enabled")

	seedLegacySI(ctx, t, q, "selfimprove_enabled", "true")
	seedLegacySI(ctx, t, q, "selfimprove_interval", "48h")
	seedLegacySI(ctx, t, q, "selfimprove_repo", repoID.String())
	seedLegacySI(ctx, t, q, "selfimprove_user_id", userID.String())
	seedLegacySI(ctx, t, q, "selfimprove_last_run_at", "2026-08-01T04:00:00Z")
	// Defensive: the success path deletes the keys, but if an assertion fails before that
	// commit we must not leak them into a sibling test.
	t.Cleanup(func() {
		mustExec(ctx, t, pool, `DELETE FROM app_settings WHERE key LIKE 'selfimprove_%'`)
	})

	if err := schedsvc.MigrateLegacySelfImprove(ctx, pool, migrateFixedNow, nil); err != nil {
		t.Fatalf("MigrateLegacySelfImprove: %v", err)
	}

	rows := selfImproveRowsForUser(ctx, t, q, userID)
	if len(rows) != 1 {
		t.Fatalf("self_improve schedules for user = %d, want exactly 1: %+v", len(rows), rows)
	}
	s := rows[0]
	if s.RepoID != repoID {
		t.Fatalf("materialized schedule repo = %s, want %s", s.RepoID, repoID)
	}
	if s.Origin != "default" {
		t.Fatalf("origin = %q, want default", s.Origin)
	}
	if s.CatalogSlug.String != "self-improve" {
		t.Fatalf("catalog_slug = %q, want self-improve", s.CatalogSlug.String)
	}
	if !s.Enabled {
		t.Fatalf("enabled = false, want true")
	}
	if s.CronExpr.String != "0 4 */2 * *" {
		t.Fatalf("cron_expr = %q, want %q (48h -> every 2 days at 04:00)", s.CronExpr.String, "0 4 */2 * *")
	}

	// All five legacy keys must be gone (retired in the same transaction as the insert).
	if present := currentLegacyKeys(ctx, t, q); len(present) != 0 {
		t.Fatalf("legacy selfimprove_* keys still present after migration: %v", present)
	}
}

// TestMigrateLegacySelfImproveIdempotentLiveDB — DoD #2: running the migration twice yields
// STILL exactly one self_improve row and no error. After the first run the keys are gone, so
// the second run is a no-op via the disabled guard — it must not duplicate the row nor error.
func TestMigrateLegacySelfImproveIdempotentLiveDB(t *testing.T) {
	ctx := context.Background()
	q, pool, userID, connID := selfImproveFixture(ctx, t)
	repoID := seedSelfImproveRepo(ctx, t, pool, connID, 8502, "migrate-idem")

	seedLegacySI(ctx, t, q, "selfimprove_enabled", "true")
	seedLegacySI(ctx, t, q, "selfimprove_interval", "72h")
	seedLegacySI(ctx, t, q, "selfimprove_repo", repoID.String())
	seedLegacySI(ctx, t, q, "selfimprove_user_id", userID.String())
	seedLegacySI(ctx, t, q, "selfimprove_last_run_at", "2026-08-02T04:00:00Z")
	t.Cleanup(func() {
		mustExec(ctx, t, pool, `DELETE FROM app_settings WHERE key LIKE 'selfimprove_%'`)
	})

	if err := schedsvc.MigrateLegacySelfImprove(ctx, pool, migrateFixedNow, nil); err != nil {
		t.Fatalf("first MigrateLegacySelfImprove: %v", err)
	}
	if err := schedsvc.MigrateLegacySelfImprove(ctx, pool, migrateFixedNow, nil); err != nil {
		t.Fatalf("second MigrateLegacySelfImprove (idempotent): %v", err)
	}

	rows := selfImproveRowsForUser(ctx, t, q, userID)
	if len(rows) != 1 {
		t.Fatalf("self_improve schedules after two migrations = %d, want exactly 1 (idempotent): %+v", len(rows), rows)
	}
	// 72h -> every 3 days at 04:00, confirming the interval mapping and that the single
	// surviving row is the one the first run created.
	if got := rows[0].CronExpr.String; got != "0 4 */3 * *" {
		t.Fatalf("cron_expr = %q, want %q (72h -> every 3 days)", got, "0 4 */3 * *")
	}
}

// TestMigrateLegacySelfImproveDisabledLiveDB — DoD #3: a disabled legacy install seeds
// NOTHING and leaves the keys intact.
func TestMigrateLegacySelfImproveDisabledLiveDB(t *testing.T) {
	ctx := context.Background()
	q, pool, userID, connID := selfImproveFixture(ctx, t)
	repoID := seedSelfImproveRepo(ctx, t, pool, connID, 8503, "migrate-disabled")

	seedLegacySI(ctx, t, q, "selfimprove_enabled", "false")
	seedLegacySI(ctx, t, q, "selfimprove_interval", "48h")
	seedLegacySI(ctx, t, q, "selfimprove_repo", repoID.String())
	seedLegacySI(ctx, t, q, "selfimprove_user_id", userID.String())
	seedLegacySI(ctx, t, q, "selfimprove_last_run_at", "2026-08-03T04:00:00Z")
	t.Cleanup(func() {
		mustExec(ctx, t, pool, `DELETE FROM app_settings WHERE key LIKE 'selfimprove_%'`)
	})

	if err := schedsvc.MigrateLegacySelfImprove(ctx, pool, migrateFixedNow, nil); err != nil {
		t.Fatalf("MigrateLegacySelfImprove (disabled must be a nil no-op): %v", err)
	}

	if rows := selfImproveRowsForUser(ctx, t, q, userID); len(rows) != 0 {
		t.Fatalf("disabled install seeded %d self_improve rows, want 0: %+v", len(rows), rows)
	}
	if present := currentLegacyKeys(ctx, t, q); len(present) != 5 {
		t.Fatalf("disabled install must leave all 5 legacy keys, present = %v", present)
	}
}

// TestMigrateLegacySelfImproveDisconnectedRepoLiveDB — DoD #4: enabled, but the recorded repo
// is not connected/owned (here a random uuid that is no repo at all). The migration must
// return nil (boot never fails on a stale legacy install), seed nothing, and keep the keys.
func TestMigrateLegacySelfImproveDisconnectedRepoLiveDB(t *testing.T) {
	ctx := context.Background()
	q, pool, userID, _ := selfImproveFixture(ctx, t)
	orphanRepo := uuid.New() // a valid uuid that is NOT a repos row

	seedLegacySI(ctx, t, q, "selfimprove_enabled", "true")
	seedLegacySI(ctx, t, q, "selfimprove_interval", "48h")
	seedLegacySI(ctx, t, q, "selfimprove_repo", orphanRepo.String())
	seedLegacySI(ctx, t, q, "selfimprove_user_id", userID.String())
	seedLegacySI(ctx, t, q, "selfimprove_last_run_at", "2026-08-04T04:00:00Z")
	t.Cleanup(func() {
		mustExec(ctx, t, pool, `DELETE FROM app_settings WHERE key LIKE 'selfimprove_%'`)
	})

	if err := schedsvc.MigrateLegacySelfImprove(ctx, pool, migrateFixedNow, nil); err != nil {
		t.Fatalf("MigrateLegacySelfImprove (disconnected repo must not fail boot): %v", err)
	}

	if rows := selfImproveRowsForUser(ctx, t, q, userID); len(rows) != 0 {
		t.Fatalf("disconnected repo seeded %d self_improve rows, want 0: %+v", len(rows), rows)
	}
	if present := currentLegacyKeys(ctx, t, q); len(present) != 5 {
		t.Fatalf("disconnected repo must leave all 5 legacy keys, present = %v", present)
	}
}

// TestMigrateLegacySelfImproveUnconfiguredLiveDB — DoD #5: enabled but never fully configured
// (an unparseable repo id). The migration returns nil, seeds nothing, and keeps the keys.
func TestMigrateLegacySelfImproveUnconfiguredLiveDB(t *testing.T) {
	ctx := context.Background()
	q, pool, userID, _ := selfImproveFixture(ctx, t)

	seedLegacySI(ctx, t, q, "selfimprove_enabled", "true")
	seedLegacySI(ctx, t, q, "selfimprove_interval", "48h")
	seedLegacySI(ctx, t, q, "selfimprove_repo", "not-a-uuid")
	seedLegacySI(ctx, t, q, "selfimprove_user_id", userID.String())
	seedLegacySI(ctx, t, q, "selfimprove_last_run_at", "2026-08-05T04:00:00Z")
	t.Cleanup(func() {
		mustExec(ctx, t, pool, `DELETE FROM app_settings WHERE key LIKE 'selfimprove_%'`)
	})

	if err := schedsvc.MigrateLegacySelfImprove(ctx, pool, migrateFixedNow, nil); err != nil {
		t.Fatalf("MigrateLegacySelfImprove (unconfigured must not fail boot): %v", err)
	}

	if rows := selfImproveRowsForUser(ctx, t, q, userID); len(rows) != 0 {
		t.Fatalf("unconfigured install seeded %d self_improve rows, want 0: %+v", len(rows), rows)
	}
	if present := currentLegacyKeys(ctx, t, q); len(present) != 5 {
		t.Fatalf("unconfigured install must leave all 5 legacy keys, present = %v", present)
	}
}
