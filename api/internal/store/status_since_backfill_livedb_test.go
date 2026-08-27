package store_test

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestStatusSinceBackfillLiveDB is the permanent regression gate for the ONE statement in
// 00163_run_status_since.sql that no always-at-head fixture can see: the no-WHERE
// `UPDATE runs SET status_since = updated_at` that backfills EVERY existing row.
//
// # THE BLIND SPOT THIS TEST EXISTS FOR
//
// The migration adds status_since nullable, backfills every row from updated_at, then
// installs NOT NULL and DEFAULT now(). Every other live-DB test reaches the schema through
// store.Migrate (to HEAD), so by the time they insert a run, 00163 has already run over an
// empty corpus — the backfill matched nothing, and the DEFAULT now() then stamps their
// freshly-inserted rows. A miswrite of the backfill (dropping the UPDATE and leaning on
// DEFAULT now(), a stray WHERE that skips old rows, copying the wrong source column) is
// therefore STRUCTURALLY INVISIBLE to them: it can only be wrong about rows that PRE-EXIST
// the migration, and an at-head fixture never has any.
//
// This test uses the store.MigrateTo seam (issue #187) to close that gap: it stands a
// throwaway database at version 162 (the version BEFORE 00163), seeds a corpus chosen to
// DISCRIMINATE the correct backfill from the plausible miswrites, applies 163, and asserts
// the value status_since was backfilled to.
//
// # WHY THESE PARTICULAR RUNS ARE THE DISCRIMINATING FIXTURE
//
//   - rOld's updated_at is forced to a FIXED timestamp far in the past (2020). This is the
//     headline discriminator: the correct backfill copies updated_at, so status_since must
//     read 2020. A migration that dropped the backfill and relied on DEFAULT now() would
//     leave status_since at ~migration time (2026), NOT 2020 — the assertion that
//     status_since equals 2020 exactly, and is nowhere near now(), separates those two.
//   - rRecent keeps its natural (recent) updated_at. It proves the no-WHERE backfill
//     touches EVERY row, not just the aged one: status_since must equal its updated_at too.
//
// # WHY IT MUST STAND ITS OWN DATABASE
//
// The shared store-IT database is always migrated to head, so a pre-migration corpus can
// never be seeded there — 00163 has already run. So the test CREATEs an isolated database
// on the same server, migrates IT to 162, seeds, and applies 163.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (e2e/run-store-it.sh
// provides one). A DB-less `go test ./...` SKIPs, which is what lets this file compile and
// pass there while doing its real work only under the live-DB runner.
func TestStatusSinceBackfillLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()

	// --- Stand an isolated database on the same server. CREATE DATABASE cannot run inside
	// a transaction; a bare pool Exec is autocommit. The name is all-lowercase hex +
	// underscore, a safe identifier; we Sanitize it everywhere it is interpolated anyway.
	name := "status_since_bf_" + strings.ReplaceAll(uuid.NewString(), "-", "")

	adminPool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open admin pool: %v", err)
	}
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		adminPool.Close()
		t.Fatalf("create database %s: %v", name, err)
	}
	adminPool.Close()

	// Register teardown the instant the database exists. The work pool is opened below; the
	// cleanup owns closing it (single owner, no double Close), then opens a FRESH admin pool
	// on the ORIGINAL dsn to drop the isolated database. WITH (FORCE) (pg17) terminates any
	// leftover connection so the DROP cannot hang. A cleanup failure is logged, never fatal.
	var pool *pgxpool.Pool
	t.Cleanup(func() {
		if pool != nil {
			pool.Close()
		}
		cleanupAdmin, err := store.OpenPool(ctx, dsn)
		if err != nil {
			t.Logf("cleanup: open admin pool to drop %s: %v", name, err)
			return
		}
		defer cleanupAdmin.Close()
		if _, err := cleanupAdmin.Exec(ctx,
			"DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Logf("cleanup: drop database %s: %v", name, err)
		}
	})

	// Build the DSN for the new database by swapping only the path, leaving the query
	// (sslmode etc.) intact.
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	u.Path = "/" + name
	newDSN := u.String()

	// --- Migrate the isolated database to 162 — the version BEFORE 00163. If the seam
	// over-migrated, the pre-migration guard below catches it.
	if err := store.MigrateTo(ctx, newDSN, 162); err != nil {
		t.Fatalf("MigrateTo(162): %v", err)
	}

	pool, err = store.OpenPool(ctx, newDSN)
	if err != nil {
		t.Fatalf("open work pool: %v", err)
	}
	// NOTE: no `defer pool.Close()` — the cleanup above owns closing pool.

	// --- Prove we are genuinely pre-migration. If status_since already exists, the seam
	// over-migrated and every assertion below would be vacuous (the backfill would have
	// already run over the corpus we are about to seed). Guard against it.
	var haveCol int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_schema = 'public' AND table_name = 'runs' AND column_name = 'status_since'`).Scan(&haveCol); err != nil {
		t.Fatalf("probe for runs.status_since at v162: %v", err)
	}
	if haveCol != 0 {
		t.Fatalf("runs.status_since exists at v162 (%d) — MigrateTo(162) did not stop before 00163; test would be vacuous", haveCol)
	}

	// --- Seed the discriminating corpus. repro106SeedRun (same package,
	// revise_cap_repro_test.go) builds each user/connection/repo/run chain with raw SQL;
	// the generated store methods can't be used here because they reflect the HEAD schema
	// that has status_since, and we are deliberately at v162.
	oldTS := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)

	// rOld — updated_at forced far into the past. THE DISCRIMINATING FIXTURE: a
	// DEFAULT now() without the backfill would leave status_since ~2026, not 2020.
	rOld := repro106SeedRun(ctx, t, pool, "status-since-old")
	mustExec(ctx, t, pool, `UPDATE runs SET updated_at = $1 WHERE id = $2`, oldTS, rOld)

	// rRecent — natural (recent) updated_at, untouched. Proves the no-WHERE backfill
	// touches every row, not just the aged one.
	rRecent := repro106SeedRun(ctx, t, pool, "status-since-recent")

	// --- Pre-migration sanity: prove rOld actually carries the fixed old updated_at, so a
	// later green cannot be the vacuous "everything already recent" result.
	var preUpdated pgtype.Timestamptz
	if err := pool.QueryRow(ctx, `SELECT updated_at FROM runs WHERE id = $1`, rOld).Scan(&preUpdated); err != nil {
		t.Fatalf("pre-migration read rOld.updated_at: %v", err)
	}
	if !preUpdated.Valid || !preUpdated.Time.Equal(oldTS) {
		t.Fatalf("pre-migration rOld.updated_at = %v, want %v (fixture did not land)", preUpdated.Time, oldTS)
	}

	// Capture rRecent's updated_at now, to compare its backfilled status_since against.
	var recentUpdated pgtype.Timestamptz
	if err := pool.QueryRow(ctx, `SELECT updated_at FROM runs WHERE id = $1`, rRecent).Scan(&recentUpdated); err != nil {
		t.Fatalf("pre-migration read rRecent.updated_at: %v", err)
	}
	if !recentUpdated.Valid {
		t.Fatalf("pre-migration rRecent.updated_at is NULL — fixture did not land")
	}

	// --- Apply the migration under test.
	if err := store.MigrateTo(ctx, newDSN, 163); err != nil {
		t.Fatalf("MigrateTo(163): %v", err)
	}

	// --- Assert the backfill on rOld: status_since must equal the FIXED old timestamp
	// exactly, copied from updated_at — not ~migration time.
	var oldSince pgtype.Timestamptz
	if err := pool.QueryRow(ctx, `SELECT status_since FROM runs WHERE id = $1`, rOld).Scan(&oldSince); err != nil {
		t.Fatalf("read rOld.status_since: %v", err)
	}
	if !oldSince.Valid {
		t.Fatalf("rOld.status_since is NULL after backfill — the NOT NULL install should have failed the migration first")
	}
	if !oldSince.Time.Equal(oldTS) {
		t.Errorf("rOld.status_since = %v, want %v (the backfill must copy updated_at, not stamp now())", oldSince.Time, oldTS)
	}
	// Belt-and-braces that the DEFAULT now() path did not win: 2020 is nowhere near now().
	if now := time.Now(); now.Sub(oldSince.Time) < 5*time.Minute {
		t.Errorf("rOld.status_since = %v is within 5m of now (%v) — a DEFAULT now() backfill, not the updated_at copy", oldSince.Time, now)
	}

	// --- Assert the backfill touched rRecent too: status_since must equal its updated_at.
	var recentSince pgtype.Timestamptz
	if err := pool.QueryRow(ctx, `SELECT status_since FROM runs WHERE id = $1`, rRecent).Scan(&recentSince); err != nil {
		t.Fatalf("read rRecent.status_since: %v", err)
	}
	if !recentSince.Valid || !recentSince.Time.Equal(recentUpdated.Time) {
		t.Errorf("rRecent.status_since = %v, want it to equal updated_at %v (the no-WHERE backfill must touch every row)",
			recentSince.Time, recentUpdated.Time)
	}

	// --- Positive control (the migration's own operational invariant): immediately after
	// the backfill, EVERY row's status_since equals its updated_at. IS DISTINCT FROM counts
	// NULL mismatches too, so a single skipped or NULL row trips this.
	var mismatched int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM runs WHERE status_since IS DISTINCT FROM updated_at`).Scan(&mismatched); err != nil {
		t.Fatalf("positive control count: %v", err)
	}
	if mismatched != 0 {
		t.Errorf("%d run(s) have status_since != updated_at immediately after backfill, want 0 (no-WHERE backfill must cover every row)", mismatched)
	}
}
