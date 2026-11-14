package store_test

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #4206 migrations (hibernation_cache_cleanup and its validate step): the cleanup columns and their staged CHECKs. Down refuses while any
// attempt is unsettled, and the CHECK fences reject a write that would reopen claims.

func TestWorkerHibernationCleanupMigrationLiveDB(t *testing.T) {
	ctx, dsn, pool := hibernationMigrationDB(t)
	_, validateV, belowV := cleanupMigrationVersions(t)
	if err := store.MigrateTo(ctx, dsn, validateV); err != nil {
		t.Fatal(err)
	}
	_, worker := hibWorker(ctx, t, pool)

	// Defaults: a pre-existing row is phase none with no outcome.
	if n := scalarInt(ctx, t, pool, `SELECT count(*) FROM workers WHERE id=$1 AND cleanup_phase='none' AND NOT cleanup_cancel_requested AND cleanup_reconcile_seq=0 AND hibernation_cleanup IS NULL`, worker); n != 1 {
		t.Fatalf("defaults wrong: %d", n)
	}
	// The validate migration validated every staged constraint.
	if n := scalarInt(ctx, t, pool, `SELECT count(*) FROM pg_constraint WHERE conrelid='workers'::regclass AND conname LIKE 'workers_%cleanup%' AND NOT convalidated`); n != 0 {
		t.Fatalf("%d cleanup constraints left NOT VALID", n)
	}
	if n := scalarInt(ctx, t, pool, `SELECT count(*) FROM pg_constraint WHERE conrelid='workers'::regclass AND conname LIKE 'workers_%cleanup%'`); n != 10 {
		t.Fatalf("%d cleanup constraints, want 10", n)
	}

	// An unsettled attempt: coherent columns under a preparing worker.
	mustExec(ctx, t, pool, `UPDATE workers SET hibernation_state='preparing', hibernation_op_id=gen_random_uuid(), hibernation_requested_at=now(),
		cleanup_phase='requested', cleanup_op_id=gen_random_uuid(), cleanup_register_nonce='n', cleanup_requested_at=now() WHERE id=$1`, worker)

	// The fences reject what an image that does not know the attempt would write.
	for name, sql := range map[string]string{
		"restore none":     `UPDATE workers SET hibernation_state='none' WHERE id=$1`,
		"scaling down":     `UPDATE workers SET hibernation_scaling_down_at=now() WHERE id=$1`,
		"hibernated":       `UPDATE workers SET hibernation_state='hibernated', hibernated_at=now() WHERE id=$1`,
		"started no pod":   `UPDATE workers SET cleanup_phase='started', cleanup_started_at=now() WHERE id=$1`,
		"settled no value": `UPDATE workers SET cleanup_phase='settled', cleanup_settled_at=now() WHERE id=$1`,
	} {
		if _, err := pool.Exec(ctx, sql, worker); err == nil || !strings.Contains(err.Error(), "violates check constraint") {
			t.Fatalf("%s: %v, want a check violation", name, err)
		}
	}

	// Down refuses while the attempt is unsettled, and changes nothing.
	err := store.MigrateDownTo(ctx, dsn, belowV)
	if err == nil || !strings.Contains(err.Error(), "rollback refused") {
		t.Fatalf("Down with an unsettled attempt: %v", err)
	}
	if n := scalarInt(ctx, t, pool, `SELECT count(*) FROM workers WHERE cleanup_phase='requested'`); n != 1 {
		t.Fatalf("a refused Down changed the worker: %d", n)
	}

	// Once settled, Down drops the columns, and Up restores them.
	mustExec(ctx, t, pool, `UPDATE workers SET cleanup_phase='settled', hibernation_cleanup='skipped', cleanup_settled_at=now() WHERE id=$1`, worker)
	if err := store.MigrateDownTo(ctx, dsn, belowV); err != nil {
		t.Fatalf("Down with only settled attempts: %v", err)
	}
	if n := scalarInt(ctx, t, pool, `SELECT count(*) FROM information_schema.columns WHERE table_name='workers' AND (column_name LIKE 'cleanup_%' OR column_name LIKE 'hibernation_cleanup%')`); n != 0 {
		t.Fatalf("Down left %d cleanup columns", n)
	}
	if err := store.MigrateTo(ctx, dsn, validateV); err != nil {
		t.Fatalf("re-Up: %v", err)
	}
}

// cleanupMigrationVersions derives, from the embedded migration files, the versions of the cleanup
// migration and its validation, and the highest version below the cleanup migration, so the
// numbers survive a renumber at landing.
func cleanupMigrationVersions(t *testing.T) (cleanup, validate, below int64) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("migrations", "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("list migrations: %v (%d files)", err, len(files))
	}
	version := func(f string) int64 {
		name := filepath.Base(f)
		n, err := strconv.ParseInt(name[:strings.IndexByte(name, '_')], 10, 64)
		if err != nil {
			t.Fatalf("migration %s: %v", name, err)
		}
		return n
	}
	for _, f := range files {
		switch {
		case strings.HasSuffix(f, "_validate_hibernation_cache_cleanup.sql"):
			validate = version(f)
		case strings.HasSuffix(f, "_hibernation_cache_cleanup.sql"):
			cleanup = version(f)
		}
	}
	if cleanup == 0 || validate == 0 {
		t.Fatalf("cleanup migrations not found: cleanup=%d validate=%d", cleanup, validate)
	}
	for _, f := range files {
		if v := version(f); v < cleanup && v > below {
			below = v
		}
	}
	return cleanup, validate, below
}
