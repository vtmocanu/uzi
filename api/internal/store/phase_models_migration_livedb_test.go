package store

import (
	"context"
	"testing"
	"time"
)

// Capture the actual predecessor, including function-body comments, rather than keeping a
// second static whole-body oracle. Roll back the function before dropping its planning columns.
func TestCodexPhasePlacementMigrationRoundTripLiveDB(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	version := migrationVersionByName(t, "placement_codex_phase_models")
	pool, dsn := codexJudgeMigrationDB(t, version-1)
	definition := func() string {
		t.Helper()
		var value string
		if err := pool.QueryRow(ctx, "SELECT pg_get_functiondef('fn_placement_admitting_workers'::regproc)").Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	columnsVersion := migrationVersionByName(t, "user_planning_codex")
	if columnsVersion >= version {
		t.Fatal("planning columns must precede placement migration")
	}
	assertColumns := func(want bool) {
		t.Helper()
		for _, column := range []string{"planning_codex_model", "planning_codex_effort"} {
			var exists bool
			if err := pool.QueryRow(ctx, `SELECT EXISTS (
				SELECT 1 FROM information_schema.columns
				WHERE table_schema='public' AND table_name='users' AND column_name=$1)`, column).Scan(&exists); err != nil {
				t.Fatal(err)
			}
			if exists != want {
				t.Fatalf("column %s present=%v, want %v", column, exists, want)
			}
		}
	}
	assertColumns(true)
	before := definition()
	if err := MigrateTo(ctx, dsn, version); err != nil {
		t.Fatal(err)
	}
	assertColumns(true)
	after := definition()
	if before == after {
		t.Fatal("Up did not change the placement function")
	}
	if err := MigrateDownTo(ctx, dsn, version-1); err != nil {
		t.Fatal(err)
	}
	if definition() != before {
		t.Fatal("Down did not restore the exact predecessor")
	}
	// The predecessor no longer references the Codex columns, so their Down can run.
	if err := MigrateDownTo(ctx, dsn, columnsVersion-1); err != nil {
		t.Fatal(err)
	}
	assertColumns(false)
	if definition() != before {
		t.Fatal("columns Down changed the restored predecessor function")
	}
	if err := MigrateTo(ctx, dsn, version); err != nil {
		t.Fatal(err)
	}
	assertColumns(true)
	if definition() != after {
		t.Fatal("second Up did not restore the same Codex placement function")
	}
}
