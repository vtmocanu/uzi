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
	before := definition()
	if err := MigrateTo(ctx, dsn, version); err != nil {
		t.Fatal(err)
	}
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
	if err := MigrateTo(ctx, dsn, version); err != nil {
		t.Fatal(err)
	}
	if definition() != after {
		t.Fatal("second Up did not restore the same Codex placement function")
	}
}
