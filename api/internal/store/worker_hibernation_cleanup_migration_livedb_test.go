package store_test

import (
	"errors"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

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
	if n := scalarInt(ctx, t, pool, `SELECT count(*) FROM pg_constraint WHERE conrelid='workers'::regclass AND contype='c' AND conname LIKE 'workers_%cleanup%' AND NOT convalidated`); n != 0 {
		t.Fatalf("%d cleanup constraints left NOT VALID", n)
	}
	// The exact staged set, by name: a rename or a swap that keeps the count still fails.
	rows, err := pool.Query(ctx, `SELECT conname FROM pg_constraint WHERE conrelid='workers'::regclass AND contype='c' AND conname LIKE 'workers_%cleanup%' ORDER BY conname`)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		names = append(names, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	wantNames := []string{
		"workers_cleanup_coherent",
		"workers_cleanup_fence_none",
		"workers_cleanup_fence_unsettled",
		"workers_cleanup_phase_check",
		"workers_cleanup_reconcile_seq_check",
		"workers_cleanup_settled_coherent",
		"workers_cleanup_started_coherent",
		"workers_cleanup_unknown_coherent",
		"workers_hibernation_cleanup_check",
		"workers_hibernation_cleanup_reason_bounds",
	}
	if !reflect.DeepEqual(names, wantNames) {
		t.Fatalf("cleanup constraints = %v, want %v", names, wantNames)
	}

	// An unsettled attempt: coherent columns under a preparing worker.
	mustExec(ctx, t, pool, `UPDATE workers SET hibernation_state='preparing', hibernation_op_id=gen_random_uuid(), hibernation_requested_at=now(),
		cleanup_phase='requested', cleanup_op_id=gen_random_uuid(), cleanup_register_nonce='n', cleanup_requested_at=now() WHERE id=$1`, worker)

	// The fences reject what an image that does not know the attempt would write, each by
	// the constraint that owns that rule.
	for name, c := range map[string]struct{ sql, constraint string }{
		// Also violates workers_cleanup_fence_unsettled; PostgreSQL tests CHECKs in name order,
		// so fence_none is the one reported.
		"restore none":     {`UPDATE workers SET hibernation_state='none' WHERE id=$1`, "workers_cleanup_fence_none"},
		"scaling down":     {`UPDATE workers SET hibernation_scaling_down_at=now() WHERE id=$1`, "workers_cleanup_fence_unsettled"},
		"hibernated":       {`UPDATE workers SET hibernation_state='hibernated', hibernated_at=now() WHERE id=$1`, "workers_cleanup_fence_unsettled"},
		"started no pod":   {`UPDATE workers SET cleanup_phase='started', cleanup_started_at=now() WHERE id=$1`, "workers_cleanup_started_coherent"},
		"settled no value": {`UPDATE workers SET cleanup_phase='settled', cleanup_settled_at=now() WHERE id=$1`, "workers_cleanup_settled_coherent"},
	} {
		_, err := pool.Exec(ctx, c.sql, worker)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != c.constraint {
			t.Fatalf("%s: %v, want a check violation of %s", name, err, c.constraint)
		}
	}

	// Down refuses while the attempt is unsettled, and changes nothing.
	err = store.MigrateDownTo(ctx, dsn, belowV)
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
