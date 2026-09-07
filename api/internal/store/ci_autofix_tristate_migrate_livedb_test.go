package store_test

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestCIAutofixTristateMigrateLiveDB is the regression gate for the one-time data
// fold in 00190_user_ci_autofix_tristate.sql (PRD #914): making
// users.ci_autofix_enabled a nullable tri-state (NULL = inherit the admin global
// default, which is ON), folding every existing `false` row to NULL and dropping
// the column DEFAULT so new rows are NULL too.
//
// # THE BLIND SPOT THIS TEST EXISTS FOR
//
// A once-over-existing-rows fold runs exactly once, over whatever rows exist when
// the migration is applied. Every OTHER live-DB test in this package reaches the
// schema through store.Migrate (migrations to HEAD), so by the time they insert
// anything 00190 has already run over an EMPTY users table and the fold matched
// nothing. A miswrite of the fold (wrong direction, wrong predicate) is therefore
// STRUCTURALLY INVISIBLE to them: it can only be wrong about pre-existing rows, and
// an always-at-head fixture never has any.
//
// This test uses the store.MigrateTo seam to stand a throwaway database at version
// 189 (the version BEFORE 00190), seed two users carrying the OLD schema
// (ci_autofix_enabled NOT NULL: one false = "opted out / never chose", one true =
// explicit opt-in), prove those values pre-migration, then apply 00190 and assert
// the fold happened: false -> NULL (Assertion A), true stays true (Assertion B).
// Assertion A/B MUST run at exactly v190 — a once-over-existing-rows fold is only
// observable against rows that predate it.
//
// It then migrates the rest of the way to HEAD and asserts the DROP DEFAULT
// (Assertion C): a user created via the generated CreateUser query (which does not
// stamp the column) reads NULL, proving a fresh row inherits (on), NOT false. This
// generated-query assertion runs at HEAD, not at v190: CreateUser's RETURNING *
// reflects the HEAD schema, so calling it against a v190 DB would fail with
// SQLSTATE 42703 on any column added after 00190.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; the
// store-IT runner provides one. `go test ./...` without it SKIPs.
func TestCIAutofixTristateMigrateLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via the store-IT runner for live-DB coverage")
	}
	ctx := context.Background()

	// --- Stand an isolated database on the same server. CREATE DATABASE cannot
	// run inside a transaction; a bare pool Exec is autocommit. The name is
	// all-lowercase hex + underscore, a safe identifier; still Sanitize it.
	name := "ci_af_tri_" + strings.ReplaceAll(uuid.NewString(), "-", "")

	adminPool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open admin pool: %v", err)
	}
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		adminPool.Close()
		t.Fatalf("create database %s: %v", name, err)
	}
	adminPool.Close()

	// Register teardown the instant the database exists. The work pool is opened
	// below; the cleanup owns closing it (single owner, no double Close). Cleanup
	// opens a FRESH admin pool on the ORIGINAL dsn to DROP ... WITH (FORCE) (pg17)
	// so the drop cannot hang on a leftover connection. A cleanup failure is
	// logged, never fatal — it must not mask the real result of the test.
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

	// Build the DSN for the new database by swapping only the path, leaving the
	// query (sslmode etc.) intact.
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	u.Path = "/" + name
	newDSN := u.String()

	// --- Migrate the isolated database to 189 — the version BEFORE 00190.
	if err := store.MigrateTo(ctx, newDSN, 189); err != nil {
		t.Fatalf("MigrateTo(189): %v", err)
	}

	pool, err = store.OpenPool(ctx, newDSN)
	if err != nil {
		t.Fatalf("open work pool: %v", err)
	}
	// NOTE: no `defer pool.Close()` — the cleanup above owns closing pool.

	// --- Prove we are genuinely pre-migration: at v189 the column must still be
	// NOT NULL with DEFAULT false (its 00115 shape). If the seam over-migrated
	// past 00190 the column would already be nullable with no default and the fold
	// assertions would be vacuous.
	var isNullable, colDefault string
	if err := pool.QueryRow(ctx,
		`SELECT is_nullable, COALESCE(column_default, '') FROM information_schema.columns
		 WHERE table_schema = 'public' AND table_name = 'users' AND column_name = 'ci_autofix_enabled'`).
		Scan(&isNullable, &colDefault); err != nil {
		t.Fatalf("probe users.ci_autofix_enabled at v189: %v", err)
	}
	if isNullable != "NO" {
		t.Fatalf("users.ci_autofix_enabled is_nullable at v189 = %q, want \"NO\" — MigrateTo(189) did not stop before 00190; test would be vacuous", isNullable)
	}
	if !strings.Contains(colDefault, "false") {
		t.Fatalf("users.ci_autofix_enabled default at v189 = %q, want it to contain \"false\" — MigrateTo(189) did not stop before 00190; test would be vacuous", colDefault)
	}

	// --- Seed two users carrying the OLD schema, with RAW SQL (the generated
	// CreateUser reflects the HEAD schema; we are deliberately at v189):
	//   optedOut: ci_autofix_enabled = false — the pre-00190 default, indistinguishable
	//             between "explicitly opted out" and "never chose".
	//   optedIn:  ci_autofix_enabled = true  — an explicit opt-in that must survive.
	optedOutID, optedInID := uuid.New(), uuid.New()
	mustExec(ctx, t, pool,
		`INSERT INTO users (id, email, password_hash, ci_autofix_enabled) VALUES ($1, $2, 'x', false)`,
		optedOutID, fmt.Sprintf("ci-af-out-%s@e2e", optedOutID))
	mustExec(ctx, t, pool,
		`INSERT INTO users (id, email, password_hash, ci_autofix_enabled) VALUES ($1, $2, 'x', true)`,
		optedInID, fmt.Sprintf("ci-af-in-%s@e2e", optedInID))

	// --- Non-vacuity control: confirm the pre-migration values are genuinely
	// false and true, so a later green proves a real fold, not a no-op. At v189 the
	// column is NOT NULL, so a plain bool scan is safe.
	var outBefore, inBefore bool
	if err := pool.QueryRow(ctx, `SELECT ci_autofix_enabled FROM users WHERE id = $1`, optedOutID).Scan(&outBefore); err != nil {
		t.Fatalf("read opted-out user ci_autofix_enabled at v189: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT ci_autofix_enabled FROM users WHERE id = $1`, optedInID).Scan(&inBefore); err != nil {
		t.Fatalf("read opted-in user ci_autofix_enabled at v189: %v", err)
	}
	if outBefore {
		t.Fatalf("pre-migration: opted-out user ci_autofix_enabled = true, want false (fixture did not land the old default)")
	}
	if !inBefore {
		t.Fatalf("pre-migration: opted-in user ci_autofix_enabled = false, want true (fixture did not land the explicit opt-in)")
	}

	// --- Apply the migration under test.
	if err := store.MigrateTo(ctx, newDSN, 190); err != nil {
		t.Fatalf("MigrateTo(190): %v", err)
	}

	// --- Assertion A (the fold): the previously-false row is now NULL (inherit =
	// on). Scan into a pgtype.Bool so a NULL is observable rather than an error.
	var outAfter pgtype.Bool
	if err := pool.QueryRow(ctx, `SELECT ci_autofix_enabled FROM users WHERE id = $1`, optedOutID).Scan(&outAfter); err != nil {
		t.Fatalf("read opted-out user ci_autofix_enabled after 00190: %v", err)
	}
	if outAfter.Valid {
		t.Errorf("fold: opted-out user ci_autofix_enabled = %v after 00190, want NULL (00190 UPDATE did not fold false -> NULL)", outAfter.Bool)
	}

	// --- Assertion B: the previously-true row is still true (explicit opt-in preserved).
	var inAfter pgtype.Bool
	if err := pool.QueryRow(ctx, `SELECT ci_autofix_enabled FROM users WHERE id = $1`, optedInID).Scan(&inAfter); err != nil {
		t.Fatalf("read opted-in user ci_autofix_enabled after 00190: %v", err)
	}
	if !inAfter.Valid || !inAfter.Bool {
		t.Errorf("fold: opted-in user ci_autofix_enabled = %+v after 00190, want true (the fold clobbered an explicit opt-in)", inAfter)
	}

	// --- Migrate the rest of the way to HEAD before the generated-query assertion.
	// Assertions A/B above had to run at EXACTLY v190 (the fold is once-over-existing-
	// rows; see the header). Assertion C uses the HEAD-generated CreateUser whose
	// RETURNING * reflects the HEAD schema, so calling it against a v190 DB would fail
	// with SQLSTATE 42703 on any column added after 00190. The dropped default set at
	// 00190 PERSISTS to HEAD, so Assertion C still validates exactly what it intends.
	if err := store.Migrate(ctx, newDSN); err != nil {
		t.Fatalf("Migrate to HEAD before generated-query assertion: %v", err)
	}

	// --- Assertion C (dropped default → new rows NULL = inherit = on): a NEW user
	// created via the generated CreateUser query (which does not stamp the column)
	// reads NULL, proving DROP DEFAULT means a fresh row inherits (on), not false.
	q := store.New(pool)
	newUserID := uuid.New()
	newUser, err := q.CreateUser(ctx, store.CreateUserParams{
		Email:        fmt.Sprintf("ci-af-new-%s@e2e", newUserID),
		PasswordHash: pgtype.Text{String: "x", Valid: true},
		DisplayName:  pgtype.Text{String: "New User", Valid: true},
		IsAdmin:      false,
	})
	if err != nil {
		t.Fatalf("CreateUser after 00190: %v", err)
	}
	if newUser.CiAutofixEnabled.Valid {
		t.Errorf("dropped default: CreateUser produced ci_autofix_enabled = %v after 00190, want NULL (users column default not dropped; a fresh row must inherit = on, not read false)", newUser.CiAutofixEnabled.Bool)
	}
}
