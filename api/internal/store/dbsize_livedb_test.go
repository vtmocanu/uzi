package store_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/store"
)

func dbSizeTestPool(t *testing.T) (context.Context, string, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return ctx, dsn, pool
}

func dbSizeTestSchema(ctx context.Context, t *testing.T, pool *pgxpool.Pool, name string) {
	t.Helper()
	if _, err := pool.Exec(ctx, "DROP SCHEMA IF EXISTS "+name+" CASCADE"); err != nil {
		t.Fatalf("drop stale schema: %v", err)
	}
	if _, err := pool.Exec(ctx, "CREATE SCHEMA "+name); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+name+" CASCADE")
	})
}

// TestDatabaseSizeStatusNamesLargestRelationLiveDB seeds a table in its own schema sized to
// exceed every other user relation in the (possibly populated, shared) database and expects it
// first, schema-qualified, while a still larger TEMP table on a pinned connection is excluded.
func TestDatabaseSizeStatusNamesLargestRelationLiveDB(t *testing.T) {
	const (
		// Each row is a 500-byte pad plus tuple overhead, so the heap grows by at least
		// ~500 bytes per row; sizing rows from bytes/500 therefore over-provisions.
		bytesPerRow = 500
		maxRows     = 400000 // ~200 MB of pad; beyond this the seed is no longer fast
	)
	ctx, _, pool := dbSizeTestPool(t)
	dbSizeTestSchema(ctx, t, pool, "dbsize_it_big")

	// Same relation filters as store.DatabaseSizeStatus, read after the test schema is
	// dropped so earlier tests' leftovers in the shared database are accounted for.
	var otherMax int64
	if err := pool.QueryRow(ctx, `SELECT COALESCE(max(pg_total_relation_size(c.oid)), 0)
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r','m')
  AND n.nspname NOT IN ('pg_catalog','information_schema')
  AND n.nspname NOT LIKE 'pg_toast%'
  AND n.nspname NOT LIKE 'pg_temp%'`).Scan(&otherMax); err != nil {
		t.Fatalf("read current largest relation: %v", err)
	}
	// Twice the current largest plus 1 MB of slack; the temp table is twice that again.
	bigRows := (2*otherMax + 1<<20) / bytesPerRow
	if bigRows < 2000 {
		bigRows = 2000
	}
	if bigRows*2 > maxRows {
		t.Skipf("largest existing relation is %d bytes; seeding a larger table would exceed %d rows", otherMax, maxRows)
	}
	if _, err := pool.Exec(ctx, `CREATE TABLE dbsize_it_big.big AS SELECT g, repeat('x',500) AS pad FROM generate_series(1,$1::int) g`, bigRows); err != nil {
		t.Fatalf("seed: %v", err)
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `CREATE TEMP TABLE dbsize_it_temp AS SELECT g, repeat('y',500) AS pad FROM generate_series(1,$1::int) g`, bigRows*2); err != nil {
		t.Fatalf("temp seed: %v", err)
	}

	got, err := store.DatabaseSizeStatus(ctx, pool)
	if err != nil {
		t.Fatalf("DatabaseSizeStatus: %v", err)
	}
	if got.RelationsUnavailable || len(got.Largest) == 0 {
		t.Fatalf("relations missing: %+v", got)
	}
	if got.Largest[0].Name != "dbsize_it_big.big" {
		t.Errorf("Largest[0] = %q, want dbsize_it_big.big (all: %+v)", got.Largest[0].Name, got.Largest)
	}
	for _, r := range got.Largest {
		if r.Name == "dbsize_it_temp" || r.Name == "pg_temp.dbsize_it_temp" {
			t.Errorf("temp table listed: %+v", r)
		}
	}
}

// TestDatabaseSizeStatusSurvivesAccessExclusiveLockLiveDB holds ACCESS EXCLUSIVE on a table
// from a second connection: the size must still come back quickly, with the relations
// reported unavailable instead of failing the probe.
func TestDatabaseSizeStatusSurvivesAccessExclusiveLockLiveDB(t *testing.T) {
	ctx, dsn, pool := dbSizeTestPool(t)
	dbSizeTestSchema(ctx, t, pool, "dbsize_it_lock")
	if _, err := pool.Exec(ctx, `CREATE TABLE dbsize_it_lock.locked AS SELECT g FROM generate_series(1,10) g`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	holder, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect holder: %v", err)
	}
	t.Cleanup(func() { _ = holder.Close(context.Background()) })
	tx, err := holder.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `LOCK TABLE dbsize_it_lock.locked IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatalf("lock: %v", err)
	}

	start := time.Now()
	got, err := store.DatabaseSizeStatus(ctx, pool)
	if err != nil {
		t.Fatalf("DatabaseSizeStatus under lock: %v", err)
	}
	// The 500ms lock_timeout must settle this well before the 2s relations-context
	// backstop; a budget at or above the backstop could not tell the two apart.
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Errorf("took %v, want <= 1.5s (lock_timeout, not the context backstop)", elapsed)
	}
	if got.SizeBytes <= 0 {
		t.Errorf("SizeBytes = %d, want > 0", got.SizeBytes)
	}
	if !got.RelationsUnavailable || len(got.Largest) != 0 {
		t.Errorf("want relations unavailable and empty, got %+v", got)
	}

	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("release lock: %v", err)
	}
	got, err = store.DatabaseSizeStatus(ctx, pool)
	if err != nil || got.RelationsUnavailable || len(got.Largest) == 0 {
		t.Errorf("after release: %+v, %v", got, err)
	}
}

// TestDatabaseSizeStatusDegradesOnStatementTimeoutLiveDB proves a relations failure other
// than a lock timeout (SQLSTATE 57014) still returns the size with relations unavailable.
// The pool's session statement_timeout (300ms) fires before the 500ms lock_timeout while a
// second connection holds ACCESS EXCLUSIVE; the size read takes no relation locks.
func TestDatabaseSizeStatusDegradesOnStatementTimeoutLiveDB(t *testing.T) {
	ctx, dsn, pool := dbSizeTestPool(t)
	dbSizeTestSchema(ctx, t, pool, "dbsize_it_stmt")
	if _, err := pool.Exec(ctx, `CREATE TABLE dbsize_it_stmt.locked AS SELECT g FROM generate_series(1,10) g`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["statement_timeout"] = "300"
	stmtPool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("open statement-timeout pool: %v", err)
	}
	t.Cleanup(stmtPool.Close)

	holder, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect holder: %v", err)
	}
	t.Cleanup(func() { _ = holder.Close(context.Background()) })
	tx, err := holder.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `LOCK TABLE dbsize_it_stmt.locked IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatalf("lock: %v", err)
	}

	got, err := store.DatabaseSizeStatus(ctx, stmtPool)
	if err != nil {
		t.Fatalf("DatabaseSizeStatus with statement timeout: %v", err)
	}
	if got.SizeBytes <= 0 || !got.RelationsUnavailable || len(got.Largest) != 0 {
		t.Errorf("want size with relations unavailable, got %+v", got)
	}
}

// TestDatabaseSizeOnlyLiveDB: the size-only read returns a size and never reads relations.
func TestDatabaseSizeOnlyLiveDB(t *testing.T) {
	ctx, _, pool := dbSizeTestPool(t)
	got, err := store.DatabaseSizeOnly(ctx, pool)
	if err != nil || got.SizeBytes <= 0 || got.Largest != nil || got.RelationsUnavailable {
		t.Errorf("DatabaseSizeOnly = %+v, %v", got, err)
	}
}

// TestDatabaseSizeStatusLiveDB proves the db.size probe SQL against a real Postgres: a
// positive size and at most three non-empty relation names, largest first.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (./e2e/run-store-it.sh).
func TestDatabaseSizeStatusLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
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

	got, err := store.DatabaseSizeStatus(ctx, pool)
	if err != nil {
		t.Fatalf("DatabaseSizeStatus: %v", err)
	}
	if got.SizeBytes <= 0 {
		t.Errorf("SizeBytes = %d, want > 0", got.SizeBytes)
	}
	if n := len(got.Largest); n == 0 || n > 3 {
		t.Fatalf("len(Largest) = %d, want 1..3", n)
	}
	for i, r := range got.Largest {
		if r.Name == "" {
			t.Errorf("Largest[%d] has an empty name", i)
		}
		if i > 0 && r.SizeBytes > got.Largest[i-1].SizeBytes {
			t.Errorf("Largest not descending: [%d]=%d > [%d]=%d", i, r.SizeBytes, i-1, got.Largest[i-1].SizeBytes)
		}
	}
}
