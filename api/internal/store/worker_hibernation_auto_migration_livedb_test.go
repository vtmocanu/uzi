package store_test

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4207 migrations (worker_hibernation_auto, its validate step and the runs indexes): the
// grace anchor backfill, the staged CHECKs, the indexes, and a clean Down/Up.

func TestWorkerHibernationAutoMigrationLiveDB(t *testing.T) {
	ctx, dsn, pool := hibernationMigrationDB(t)
	autoV, validateV, idxV, belowV := autoMigrationVersions(t)
	if err := store.MigrateTo(ctx, dsn, belowV); err != nil {
		t.Fatal(err)
	}

	// Three pre-existing workers: awake after a wake, never woken, and mid-wake.
	owner, awoke := hibWorker(ctx, t, pool)
	mustExec(ctx, t, pool, `UPDATE workers SET wake_requested_at = '2026-01-02T03:04:05Z' WHERE id=$1`, awoke)
	never := uuid.New()
	mustExec(ctx, t, pool, "INSERT INTO workers(id,user_id,name,token_hash,kind,hosted_size,template_declared) VALUES($1,$2,'n',$3,'hosted','m','base')", never, owner, never[:])
	waking := uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO workers(id,user_id,name,token_hash,kind,hosted_size,template_declared,hibernation_state,hibernation_op_id,wake_requested_at)
		VALUES($1,$2,'w',$3,'hosted','m','base','waking',gen_random_uuid(),'2026-01-02T03:04:05Z')`, waking, owner, waking[:])

	if err := store.MigrateTo(ctx, dsn, autoV); err != nil {
		t.Fatal(err)
	}
	// Backfill: only a settled (none) worker that has a wake time keeps it as its grace anchor.
	if n := scalarInt(ctx, t, pool, `SELECT count(*) FROM workers WHERE id=$1 AND last_wake_settled_at = wake_requested_at`, awoke); n != 1 {
		t.Fatalf("awake worker was not backfilled: %d", n)
	}
	for name, id := range map[string]uuid.UUID{"never woken": never, "waking": waking} {
		if n := scalarInt(ctx, t, pool, `SELECT count(*) FROM workers WHERE id=$1 AND last_wake_settled_at IS NULL`, id); n != 1 {
			t.Fatalf("%s worker got a grace anchor: %d", name, n)
		}
	}
	// The staged constraints exist, NOT VALID, and no cleanup-named constraint was added.
	if n := scalarInt(ctx, t, pool, `SELECT count(*) FROM pg_constraint WHERE conrelid='workers'::regclass AND conname LIKE 'workers_hibernation_blocked_%' AND NOT convalidated`); n != 2 {
		t.Fatalf("%d staged constraints NOT VALID, want 2", n)
	}
	if n := scalarInt(ctx, t, pool, `SELECT count(*) FROM pg_constraint WHERE conrelid='workers'::regclass AND conname LIKE 'workers_%cleanup%'`); n != 10 {
		t.Fatalf("%d cleanup constraints, want the 10 of the cleanup migration", n)
	}

	if err := store.MigrateTo(ctx, dsn, validateV); err != nil {
		t.Fatal(err)
	}
	if n := scalarInt(ctx, t, pool, `SELECT count(*) FROM pg_constraint WHERE conrelid='workers'::regclass AND conname LIKE 'workers_hibernation_blocked_%' AND convalidated`); n != 2 {
		t.Fatalf("%d constraints validated, want 2", n)
	}
	// Enforcement: the three blocked columns are all set or all NULL, and the reason is 1..64 chars.
	for name, sql := range map[string]string{
		"reason alone":         `UPDATE workers SET hibernation_blocked_reason='custody_hold' WHERE id=$1`,
		"timestamps no reason": `UPDATE workers SET hibernation_blocked_at=now(), hibernation_blocked_since=now() WHERE id=$1`,
		"empty reason":         `UPDATE workers SET hibernation_blocked_reason='', hibernation_blocked_at=now(), hibernation_blocked_since=now() WHERE id=$1`,
		"long reason":          `UPDATE workers SET hibernation_blocked_reason=repeat('x',65), hibernation_blocked_at=now(), hibernation_blocked_since=now() WHERE id=$1`,
	} {
		if _, err := pool.Exec(ctx, sql, never); err == nil || !strings.Contains(err.Error(), "violates check constraint") {
			t.Fatalf("%s: %v, want a check violation", name, err)
		}
	}
	mustExec(ctx, t, pool, `UPDATE workers SET hibernation_blocked_reason=repeat('x',64), hibernation_blocked_at=now(), hibernation_blocked_since=now() WHERE id=$1`, never)

	if err := store.MigrateTo(ctx, dsn, idxV); err != nil {
		t.Fatal(err)
	}
	if n := scalarInt(ctx, t, pool, `SELECT count(*) FROM pg_indexes WHERE tablename='runs' AND indexname IN ('runs_worker_nonterminal_idx','runs_worker_finished_idx')`); n != 2 {
		t.Fatalf("%d runs indexes, want 2", n)
	}
	if n := scalarInt(ctx, t, pool, `SELECT count(*) FROM pg_index i JOIN pg_class c ON c.oid=i.indexrelid WHERE c.relname LIKE 'runs_worker_%_idx' AND NOT i.indisvalid`); n != 0 {
		t.Fatalf("%d invalid runs indexes", n)
	}

	// Down drops everything it added; Up restores it.
	if err := store.MigrateDownTo(ctx, dsn, belowV); err != nil {
		t.Fatalf("Down: %v", err)
	}
	if n := scalarInt(ctx, t, pool, `SELECT count(*) FROM information_schema.columns WHERE table_name='workers' AND (column_name='last_wake_settled_at' OR column_name LIKE 'hibernation_blocked_%')`); n != 0 {
		t.Fatalf("Down left %d columns", n)
	}
	if n := scalarInt(ctx, t, pool, `SELECT count(*) FROM pg_indexes WHERE tablename='runs' AND indexname IN ('runs_worker_nonterminal_idx','runs_worker_finished_idx')`); n != 0 {
		t.Fatalf("Down left %d indexes", n)
	}
	if err := store.MigrateTo(ctx, dsn, idxV); err != nil {
		t.Fatalf("re-Up: %v", err)
	}
}

// autoMigrationVersions derives, from the embedded migration files, the versions of the
// automatic hibernation migration, its validation and the runs-index migration, and the highest
// version below the first of them, so the numbers survive a renumber at landing.
func autoMigrationVersions(t *testing.T) (auto, validate, idx, below int64) {
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
		case strings.HasSuffix(f, "_validate_worker_hibernation_auto.sql"):
			validate = version(f)
		case strings.HasSuffix(f, "_worker_hibernation_auto.sql"):
			auto = version(f)
		case strings.HasSuffix(f, "_runs_worker_idle_idx.sql"):
			idx = version(f)
		}
	}
	if auto == 0 || validate == 0 || idx == 0 {
		t.Fatalf("migrations not found: auto=%d validate=%d idx=%d", auto, validate, idx)
	}
	for _, f := range files {
		if v := version(f); v < auto && v > below {
			below = v
		}
	}
	return auto, validate, idx, below
}
