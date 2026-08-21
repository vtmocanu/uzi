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

// TestWaitOnLimitDefaultOnBackfillLiveDB is the regression gate for the data
// backfill in 00144_wait_on_limit_default_on.sql (issue #520): the
// `UPDATE users SET wait_on_limit = true WHERE wait_on_limit = false` that flips
// EXISTING users to the new default-on behaviour.
//
// # THE BLIND SPOT THIS TEST EXISTS FOR
//
// A data-migration backfill runs exactly once, over whatever rows exist when the
// migration is applied. Every OTHER live-DB test in this package reaches the
// schema through store.Migrate (migrations to HEAD), so by the time they insert
// anything 00144 has already run over an EMPTY users table and the backfill
// matched nothing. A miswrite of the backfill is therefore STRUCTURALLY INVISIBLE
// to them: it can only be wrong about pre-existing rows, and an always-at-head
// fixture never has any.
//
// This test uses the store.MigrateTo seam to stand a throwaway database at
// version 143 (the version BEFORE 00144), seed a user carrying the OLD default
// (wait_on_limit = false), prove it is genuinely false pre-migration, then apply
// 00144 and assert the flip actually happened over that seeded row.
//
// It also asserts the two COLUMN-default flips: a user created after the
// migration via the generated CreateUser query (which does not stamp the column)
// inherits true, and a run_schedules row inserted without the column inherits
// true.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; the
// store-IT runner provides one. `go test ./...` without it SKIPs.
func TestWaitOnLimitDefaultOnBackfillLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via the store-IT runner for live-DB coverage")
	}
	ctx := context.Background()

	// --- Stand an isolated database on the same server. CREATE DATABASE cannot
	// run inside a transaction; a bare pool Exec is autocommit. The name is
	// all-lowercase hex + underscore, a safe identifier; still Sanitize it.
	name := "wait_dflt_" + strings.ReplaceAll(uuid.NewString(), "-", "")

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

	// --- Migrate the isolated database to 143 — the version BEFORE 00144.
	if err := store.MigrateTo(ctx, newDSN, 143); err != nil {
		t.Fatalf("MigrateTo(143): %v", err)
	}

	pool, err = store.OpenPool(ctx, newDSN)
	if err != nil {
		t.Fatalf("open work pool: %v", err)
	}
	// NOTE: no `defer pool.Close()` — the cleanup above owns closing pool.

	// --- Prove we are genuinely pre-migration: the users column default must
	// still be false, else the seam over-migrated and the backfill assertion
	// would be vacuous (the seeded row would already read true).
	var colDefault string
	if err := pool.QueryRow(ctx,
		`SELECT column_default FROM information_schema.columns
		 WHERE table_schema = 'public' AND table_name = 'users' AND column_name = 'wait_on_limit'`).Scan(&colDefault); err != nil {
		t.Fatalf("probe users.wait_on_limit default at v143: %v", err)
	}
	if !strings.Contains(colDefault, "false") {
		t.Fatalf("users.wait_on_limit default at v143 = %q, want it to contain \"false\" — MigrateTo(143) did not stop before 00144; test would be vacuous", colDefault)
	}

	// --- Seed a user carrying the OLD default. Insert wait_on_limit explicitly
	// false with RAW SQL (the generated CreateUser reflects the HEAD schema; we
	// are deliberately at v143).
	seededID := uuid.New()
	mustExec(ctx, t, pool,
		`INSERT INTO users (id, email, password_hash, display_name, wait_on_limit)
		 VALUES ($1, $2, 'x', 'Seeded User', false)`,
		seededID, fmt.Sprintf("wait-dflt-seed-%s@e2e", seededID))

	// --- Non-vacuity control (assertion 3): the seeded row is genuinely false
	// BEFORE the migration, so a later green proves a real flip, not a no-op.
	var before bool
	if err := pool.QueryRow(ctx, `SELECT wait_on_limit FROM users WHERE id = $1`, seededID).Scan(&before); err != nil {
		t.Fatalf("read seeded user wait_on_limit at v143: %v", err)
	}
	if before {
		t.Fatalf("pre-migration: seeded user wait_on_limit = true, want false (fixture did not land the old default)")
	}

	// --- Apply the migration under test.
	if err := store.MigrateTo(ctx, newDSN, 144); err != nil {
		t.Fatalf("MigrateTo(144): %v", err)
	}

	// --- Assertion 1: the backfill flipped the seeded pre-existing row to true.
	var after bool
	if err := pool.QueryRow(ctx, `SELECT wait_on_limit FROM users WHERE id = $1`, seededID).Scan(&after); err != nil {
		t.Fatalf("read seeded user wait_on_limit after 00144: %v", err)
	}
	if !after {
		t.Errorf("backfill: seeded user wait_on_limit = false after 00144, want true (00144 UPDATE did not flip existing users)")
	}

	// --- Assertion 2: a NEW user created via the generated CreateUser query
	// (which does not stamp wait_on_limit) inherits the flipped column default.
	q := store.New(pool)
	newUserID := uuid.New()
	newUser, err := q.CreateUser(ctx, store.CreateUserParams{
		Email:        fmt.Sprintf("wait-dflt-new-%s@e2e", newUserID),
		PasswordHash: pgtype.Text{String: "x", Valid: true},
		DisplayName:  pgtype.Text{String: "New User", Valid: true},
		IsAdmin:      false,
	})
	if err != nil {
		t.Fatalf("CreateUser after 00144: %v", err)
	}
	if !newUser.WaitOnLimit {
		t.Errorf("column default: CreateUser produced wait_on_limit = false after 00144, want true (users column default not flipped)")
	}

	// --- Assertion 4: a run_schedules row inserted WITHOUT wait_on_limit
	// inherits the flipped column default. Needs a user + forge_connection + repo
	// to satisfy the FKs; insert with RAW SQL and read the column back.
	connID, repoID := uuid.New(), uuid.New()
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, newUser.ID, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)
	var schedWait bool
	if err := pool.QueryRow(ctx,
		`INSERT INTO run_schedules (user_id, repo_id, target, prompt, timing, run_at)
		 VALUES ($1, $2, 'prompt', 'hi', 'once', now())
		 RETURNING wait_on_limit`, newUser.ID, repoID).Scan(&schedWait); err != nil {
		t.Fatalf("insert run_schedules without wait_on_limit after 00144: %v", err)
	}
	if !schedWait {
		t.Errorf("column default: run_schedules row inserted without wait_on_limit read false after 00144, want true (run_schedules column default not flipped)")
	}
}
