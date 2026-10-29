package store_test

import (
	"testing"

	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestRecoveryAvailableGuardMigrationRoundTripLiveDB(t *testing.T) {
	// Migration 00374 replaces only the function; 00373 is its preceding base.
	const migrationVersion int64 = 374
	ctx, dsn := standIsolatedDB(t, "available_roundtrip_")
	if err := store.MigrateTo(ctx, dsn, migrationVersion-1); err != nil {
		t.Fatalf("prepare preceding migration: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	guardDefinition := func() string {
		t.Helper()
		var def string
		if err := pool.QueryRow(ctx, "SELECT pg_get_functiondef('recovery_inventory_capture_guard'::regproc)").Scan(&def); err != nil {
			t.Fatal(err)
		}
		return def
	}
	preceding := guardDefinition()
	t.Run("preceding", func(t *testing.T) {
		exerciseAvailableTransitions(t, ctx, pool, false)
		exercisePreviousCaptureGuard(t, ctx, pool)
	})
	if err := store.MigrateTo(ctx, dsn, migrationVersion); err != nil {
		t.Fatalf("Up: %v", err)
	}
	t.Run("up", func(t *testing.T) {
		exerciseAvailableTransitions(t, ctx, pool, true)
		exercisePreviousCaptureGuard(t, ctx, pool)
	})
	if err := store.MigrateDownTo(ctx, dsn, migrationVersion-1); err != nil {
		t.Fatalf("Down: %v", err)
	}
	if restored := guardDefinition(); restored != preceding {
		t.Fatal("Down did not restore the exact preceding pg_get_functiondef")
	}
	t.Run("down", func(t *testing.T) {
		exerciseAvailableTransitions(t, ctx, pool, false)
		exercisePreviousCaptureGuard(t, ctx, pool)
	})
	if err := store.MigrateTo(ctx, dsn, migrationVersion); err != nil {
		t.Fatalf("Up again: %v", err)
	}
	t.Run("up_again", func(t *testing.T) {
		exerciseAvailableTransitions(t, ctx, pool, true)
		exercisePreviousCaptureGuard(t, ctx, pool)
	})
}
