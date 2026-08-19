package store_test

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestReviseCountBackfillLiveDB is the permanent regression gate for the ONE
// statement in 00094_run_revise_count.sql that no other test in the repo can see:
// the UPDATE that backfills runs.revise_count from count(*) of the run's
// revise_plan rows.
//
// # THE BLIND SPOT THIS TEST EXISTS FOR
//
// A data migration's backfill runs exactly once, on the row corpus that happens to
// exist when the migration is applied. Every OTHER live-DB test in this package
// (revise_cap_*_test.go, mr_watch_integration_test.go) reaches the schema through
// store.Migrate, which runs migrations to HEAD — so by the time those tests insert
// anything, 00094 has already run over an EMPTY runs/run_user_inputs pair and the
// backfill matched nothing. A miswrite of the backfill (wrong table, a stray
// consumed_at filter, a missing kind filter, a wrong join) is therefore
// STRUCTURALLY INVISIBLE to them: it can only be wrong about pre-existing rows, and
// pre-existing rows are exactly what an always-at-head fixture never has.
//
// This test uses the store.MigrateTo seam (issue #187) to close that gap directly:
// it stands a throwaway database at version 93 (the migration BEFORE 00094), seeds a
// corpus chosen to DISCRIMINATE a correct backfill from the plausible miswrites,
// then applies 94 and asserts the counter the backfill computed.
//
// # WHY THESE PARTICULAR RUNS ARE THE DISCRIMINATING FIXTURE
//
// The backfill is `count(*) ... WHERE kind='revise_plan' GROUP BY run_id`. The four
// runs are picked so that each of the likely wrong versions of that statement scores
// a DIFFERENT number on at least one run than the correct statement does:
//
//   - bf5c has 5 revise_plan rows, 3 of them already carrying consumed_at. The cap is
//     a LIFETIME budget (PRD #41 Decision 6: consumed rows still count), so the
//     correct backfill scores 5. A backfill that copied CountRunReviseInputs' shape
//     but wrongly added `AND consumed_at IS NULL` would score 2 here — this is the
//     ONLY run that separates those two statements, which is why it is the headline
//     case.
//   - bfnoise has 2 revise_plan rows PLUS one each of follow_up, approve_plan and
//     cancel on the SAME run. A backfill missing the `kind='revise_plan'` filter (or
//     with the wrong kind literal) would score 5 instead of 2. All four kinds are
//     valid at v93.
//   - bf1 (1 row) and bf0 (0 rows) pin the ordinary and the zero cases; bf0 in
//     particular proves the UPDATE's join does not touch a run that has no rows and
//     leaves it at the column DEFAULT 0 rather than NULL or something else.
//
// # WHY IT MUST STAND ITS OWN DATABASE
//
// The shared store-IT database is always migrated to head, so the pre-migration
// corpus cannot be seeded there — 00094 has already run. So the test CREATEs an
// isolated database on the same server, migrates IT to 93, seeds, and applies 94.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; the store-IT
// runner (e2e/run-store-it.sh) provides one. `go test ./...` without it SKIPs, which
// is what lets this file compile and pass in a DB-less `go test` while doing its real
// work only under the live-DB runner.
func TestReviseCountBackfillLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()

	// --- Stand an isolated database on the same server. CREATE DATABASE cannot run
	// inside a transaction; a bare pool Exec is autocommit, which is what we want. The
	// name is all-lowercase hex + underscore, so it is a safe identifier; we still
	// Sanitize it everywhere it is interpolated.
	name := "revise_bf_" + strings.ReplaceAll(uuid.NewString(), "-", "")

	adminPool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open admin pool: %v", err)
	}
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		adminPool.Close()
		t.Fatalf("create database %s: %v", name, err)
	}
	adminPool.Close()

	// Register teardown the instant the database exists, so nothing between here and
	// the migration can leak it. The work pool is opened below; the cleanup owns
	// closing it (a single owner, so no double Close). Cleanup then opens a FRESH admin
	// pool on the ORIGINAL dsn to drop the isolated database. WITH (FORCE) (pg17)
	// terminates any leftover connection to it so the DROP cannot hang. A cleanup
	// failure is logged, never fatal — it must not mask the real result of the test.
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

	// --- Migrate the isolated database to 93 — the version BEFORE 00094. If the seam
	// over-migrated, the pre-migration guard below would catch it.
	if err := store.MigrateTo(ctx, newDSN, 93); err != nil {
		t.Fatalf("MigrateTo(93): %v", err)
	}

	pool, err = store.OpenPool(ctx, newDSN)
	if err != nil {
		t.Fatalf("open work pool: %v", err)
	}
	// NOTE: no `defer pool.Close()` — the cleanup above owns closing pool.

	// --- Prove we are genuinely pre-migration. If revise_count already exists, the
	// seam over-migrated and every assertion below would be vacuous (the backfill
	// would have already run over the corpus we are about to seed). Guard against it.
	var haveCol int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_schema = 'public' AND table_name = 'runs' AND column_name = 'revise_count'`).Scan(&haveCol); err != nil {
		t.Fatalf("probe for runs.revise_count at v93: %v", err)
	}
	if haveCol != 0 {
		t.Fatalf("runs.revise_count exists at v93 (%d) — MigrateTo(93) did not stop before 00094; test would be vacuous", haveCol)
	}

	// --- Seed the discriminating corpus. run_user_inputs rows are inserted with RAW
	// SQL, never a generated store.Queries method: the generated code reflects the
	// HEAD schema, and we are deliberately at v93. repro106SeedRun (same package,
	// revise_cap_repro_test.go) builds each user/connection/repo/run chain.
	insertRevise := func(runID uuid.UUID, body string) {
		mustExec(ctx, t, pool,
			`INSERT INTO run_user_inputs (run_id, kind, body) VALUES ($1, $2, $3)`,
			runID, "revise_plan", body)
	}
	insertReviseConsumed := func(runID uuid.UUID, body string) {
		mustExec(ctx, t, pool,
			`INSERT INTO run_user_inputs (run_id, kind, body, consumed_at) VALUES ($1, 'revise_plan', $2, now())`,
			runID, body)
	}
	insertKind := func(runID uuid.UUID, kind, body string) {
		mustExec(ctx, t, pool,
			`INSERT INTO run_user_inputs (run_id, kind, body) VALUES ($1, $2, $3)`,
			runID, kind, body)
	}

	// bf0 — the zero case: no run_user_inputs at all. Expected revise_count 0.
	r0 := repro106SeedRun(ctx, t, pool, "bf0")

	// bf1 — one unconsumed revise_plan row. Expected 1.
	r1 := repro106SeedRun(ctx, t, pool, "bf1")
	insertRevise(r1, "bf1 revise 0")

	// bf5c — 5 revise_plan rows, 3 consumed and 2 not. Expected 5 (consumed rows still
	// count). THE DISCRIMINATING FIXTURE: a consumed_at filter would score 2, not 5.
	r5 := repro106SeedRun(ctx, t, pool, "bf5c")
	insertReviseConsumed(r5, "bf5c consumed 0")
	insertReviseConsumed(r5, "bf5c consumed 1")
	insertReviseConsumed(r5, "bf5c consumed 2")
	insertRevise(r5, "bf5c unconsumed 0")
	insertRevise(r5, "bf5c unconsumed 1")

	// bfnoise — 2 revise_plan rows PLUS one each of three other kinds on the SAME run.
	// Expected 2 (a missing/wrong kind filter would score 5).
	rn := repro106SeedRun(ctx, t, pool, "bfnoise")
	insertRevise(rn, "bfnoise revise 0")
	insertRevise(rn, "bfnoise revise 1")
	insertKind(rn, "follow_up", "bfnoise follow_up")
	insertKind(rn, "approve_plan", "bfnoise approve_plan")
	insertKind(rn, "cancel", "bfnoise cancel")

	// --- Pre-migration sanity: prove the fixture actually landed the corpus it names,
	// so a later green cannot be the vacuous "nothing to backfill" result. Assert the
	// revise_plan row counts, and that bf5c's 3 consumed rows really carry consumed_at.
	reviseRows := func(runID uuid.UUID) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM run_user_inputs WHERE run_id = $1 AND kind = 'revise_plan'`,
			runID).Scan(&n); err != nil {
			t.Fatalf("count revise_plan rows: %v", err)
		}
		return n
	}
	for _, tc := range []struct {
		runID uuid.UUID
		want  int
		label string
	}{
		{r0, 0, "bf0"}, {r1, 1, "bf1"}, {r5, 5, "bf5c"}, {rn, 2, "bfnoise"},
	} {
		if got := reviseRows(tc.runID); got != tc.want {
			t.Fatalf("pre-migration %s: revise_plan rows = %d, want %d (fixture did not land)", tc.label, got, tc.want)
		}
	}
	var consumed int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM run_user_inputs
		 WHERE run_id = $1 AND kind = 'revise_plan' AND consumed_at IS NOT NULL`,
		r5).Scan(&consumed); err != nil {
		t.Fatalf("count bf5c consumed rows: %v", err)
	}
	if consumed != 3 {
		t.Fatalf("pre-migration bf5c: consumed revise_plan rows = %d, want 3 (the discriminating rows did not land)", consumed)
	}

	// --- Apply the migration under test.
	if err := store.MigrateTo(ctx, newDSN, 94); err != nil {
		t.Fatalf("MigrateTo(94): %v", err)
	}

	// --- Assert the backfill. For each run, the backfilled runs.revise_count and the
	// live count(*) of revise_plan rows must BOTH equal the expected value. Errorf (not
	// Fatalf) so all four report on a miswrite instead of stopping at the first.
	for _, tc := range []struct {
		runID uuid.UUID
		want  int
		label string
	}{
		{r0, 0, "bf0 (zero case)"},
		{r1, 1, "bf1 (one revise)"},
		{r5, 5, "bf5c (5 rows, 3 consumed — consumed rows still count)"},
		{rn, 2, "bfnoise (2 revises + other kinds — kind filter)"},
	} {
		var counter, rows int
		if err := pool.QueryRow(ctx, `SELECT revise_count FROM runs WHERE id = $1`, tc.runID).Scan(&counter); err != nil {
			t.Fatalf("%s: read runs.revise_count: %v", tc.label, err)
		}
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM run_user_inputs WHERE run_id = $1 AND kind = 'revise_plan'`,
			tc.runID).Scan(&rows); err != nil {
			t.Fatalf("%s: count revise_plan rows: %v", tc.label, err)
		}
		if counter != tc.want || rows != tc.want {
			t.Errorf("%s: revise_count=%d, revise_plan rows=%d, want both=%d", tc.label, counter, rows, tc.want)
		}
	}
}
