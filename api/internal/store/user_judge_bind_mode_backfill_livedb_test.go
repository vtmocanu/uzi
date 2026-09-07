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

// TestUserJudgeBindModeBackfillLiveDB is the regression gate for the data backfill in
// 00197_user_judge_anthropic_bind_mode.sql (PRD #1140 M2): the
// `UPDATE users SET judge_anthropic_bind_mode = 'pinned' WHERE judge_anthropic_secret_id
// IS NOT NULL` that stamps EXISTING judge-bound users at the same time the column is
// added, so a bound row never carries the DEFAULT 'auto' while holding a pointer.
//
// # THE BLIND SPOT THIS TEST EXISTS FOR
//
// A data-migration backfill runs exactly once, over whatever rows exist when the
// migration is applied. Every OTHER live-DB test in this package reaches the schema
// through store.Migrate (migrations to HEAD), so by the time they insert anything 00197
// has already run over a users table with no judge-bound rows and the backfill matched
// nothing. A miswrite of the backfill is therefore STRUCTURALLY INVISIBLE to them: it
// can only be wrong about pre-existing bound rows, and an always-at-head fixture never
// has any.
//
// This test uses the store.MigrateTo seam to stand a throwaway database at version 196
// (the version BEFORE 00197), seed a judge-BOUND user and an UNBOUND user via RAW SQL
// (the judge_anthropic_bind_mode column does not exist yet), then apply 00197 and assert
// the backfill stamped exactly the bound row 'pinned' and left the unbound row on the
// column default 'auto'. Those backfill/default assertions MUST run at exactly v197 — a
// once-over-existing-rows migration is only observable against rows that predate it.
//
// It then migrates the rest of the way to HEAD and asserts the COLUMN default reaches a
// NEW user created via the generated CreateUser query (which does not stamp the column),
// which must read 'auto'. That generated-query assertion runs at HEAD, NOT at v197:
// CreateUser's RETURNING * reflects the HEAD schema, so calling it against a v197 DB
// would fail on any post-197 users column. The 'auto' column default set at 00197
// persists to HEAD, so the assertion still validates exactly the flipped default.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; the store-IT
// runner provides one. `go test ./...` without it SKIPs.
func TestUserJudgeBindModeBackfillLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via the store-IT runner for live-DB coverage")
	}
	ctx := context.Background()

	// --- Stand an isolated database on the same server. CREATE DATABASE cannot run
	// inside a transaction; a bare pool Exec is autocommit. The name is all-lowercase
	// hex + underscore, a safe identifier; still Sanitize it.
	name := "judge_bm_" + strings.ReplaceAll(uuid.NewString(), "-", "")

	adminPool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open admin pool: %v", err)
	}
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		adminPool.Close()
		t.Fatalf("create database %s: %v", name, err)
	}
	adminPool.Close()

	// Register teardown the instant the database exists. The work pool is opened below;
	// the cleanup owns closing it (single owner, no double Close). Cleanup opens a FRESH
	// admin pool on the ORIGINAL dsn to DROP ... WITH (FORCE) (pg17) so the drop cannot
	// hang on a leftover connection. A cleanup failure is logged, never fatal.
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

	// --- Migrate the isolated database to 196 — the version BEFORE 00197.
	if err := store.MigrateTo(ctx, newDSN, 196); err != nil {
		t.Fatalf("MigrateTo(196): %v", err)
	}

	pool, err = store.OpenPool(ctx, newDSN)
	if err != nil {
		t.Fatalf("open work pool: %v", err)
	}
	// NOTE: no `defer pool.Close()` — the cleanup above owns closing pool.

	// --- Prove we are genuinely pre-migration: the judge_anthropic_bind_mode column
	// must not exist yet, else the seam over-migrated and the backfill assertion would be
	// vacuous (both rows would already carry a mode).
	var colCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_schema = 'public' AND table_name = 'users' AND column_name = 'judge_anthropic_bind_mode'`).Scan(&colCount); err != nil {
		t.Fatalf("probe users.judge_anthropic_bind_mode at v196: %v", err)
	}
	if colCount != 0 {
		t.Fatalf("users.judge_anthropic_bind_mode already exists at v196 — MigrateTo(196) did not stop before 00197; test would be vacuous")
	}

	// --- Seed two users via RAW SQL (the generated CreateUser reflects the HEAD schema;
	// we are deliberately at v196). The BOUND user gets a judge pointer, the UNBOUND user
	// leaves it NULL.
	boundID, unboundID := uuid.New(), uuid.New()
	mustExec(ctx, t, pool,
		`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		boundID, fmt.Sprintf("judge-bound-%s@e2e", boundID))
	mustExec(ctx, t, pool,
		`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		unboundID, fmt.Sprintf("judge-unbound-%s@e2e", unboundID))

	// The bound user's judge token. 00079's composite FK is (users.id,
	// judge_anthropic_secret_id) → user_secrets (user_id, id), so the secret must belong
	// to the bound user; insert it with user_id = boundID, then point the user at it.
	secretID := uuid.New()
	mustExec(ctx, t, pool,
		`INSERT INTO user_secrets (id, user_id, kind, label, ciphertext, sealed_with)
		 VALUES ($1, $2, 'anthropic_token', $3, $4, 'master')`,
		secretID, boundID, "judge-token", []byte("ct-judge"))
	mustExec(ctx, t, pool,
		`UPDATE users SET judge_anthropic_secret_id = $1 WHERE id = $2`, secretID, boundID)

	// --- Apply the migration under test.
	if err := store.MigrateTo(ctx, newDSN, 197); err != nil {
		t.Fatalf("MigrateTo(197): %v", err)
	}

	// --- Assertion 1: the backfill stamped the pre-existing BOUND row 'pinned'.
	var boundMode string
	if err := pool.QueryRow(ctx, `SELECT judge_anthropic_bind_mode FROM users WHERE id = $1`, boundID).Scan(&boundMode); err != nil {
		t.Fatalf("read bound user judge_anthropic_bind_mode after 00197: %v", err)
	}
	if boundMode != "pinned" {
		t.Errorf("backfill: bound user judge_anthropic_bind_mode = %q after 00197, want \"pinned\" (00197 UPDATE did not stamp existing judge-bound users)", boundMode)
	}

	// --- Assertion 2: the UNBOUND row took the column default 'auto' (its pointer is
	// NULL, so the backfill's WHERE clause did not match it).
	var unboundMode string
	if err := pool.QueryRow(ctx, `SELECT judge_anthropic_bind_mode FROM users WHERE id = $1`, unboundID).Scan(&unboundMode); err != nil {
		t.Fatalf("read unbound user judge_anthropic_bind_mode after 00197: %v", err)
	}
	if unboundMode != "auto" {
		t.Errorf("column default: unbound user judge_anthropic_bind_mode = %q after 00197, want \"auto\" (the default did not reach an existing NULL-pointer row)", unboundMode)
	}

	// --- Migrate the rest of the way to HEAD before the generated-query assertion.
	// Assertions 1/2 had to run at EXACTLY v197 (the backfill is a once-over-existing-rows
	// migration). But the CreateUser assertion below uses a HEAD-generated query whose
	// RETURNING * reflects the HEAD schema, so calling it against a v197 DB would fail on
	// any post-197 users column. The 'auto' column default set at 00197 PERSISTS to HEAD.
	if err := store.Migrate(ctx, newDSN); err != nil {
		t.Fatalf("Migrate to HEAD before the generated-query assertion: %v", err)
	}

	// --- Assertion 3: a NEW user created via the generated CreateUser query (which does
	// not stamp judge_anthropic_bind_mode) inherits the column default 'auto' (D5).
	q := store.New(pool)
	newUser, err := q.CreateUser(ctx, store.CreateUserParams{
		Email:        fmt.Sprintf("judge-new-%s@e2e", uuid.New()),
		PasswordHash: pgtype.Text{String: "x", Valid: true},
		DisplayName:  pgtype.Text{String: "New User", Valid: true},
		IsAdmin:      false,
	})
	if err != nil {
		t.Fatalf("CreateUser after 00197: %v", err)
	}
	var newMode string
	if err := pool.QueryRow(ctx, `SELECT judge_anthropic_bind_mode FROM users WHERE id = $1`, newUser.ID).Scan(&newMode); err != nil {
		t.Fatalf("read new user judge_anthropic_bind_mode: %v", err)
	}
	if newMode != "auto" {
		t.Errorf("column default: CreateUser produced judge_anthropic_bind_mode = %q, want \"auto\" (users column default not set to auto)", newMode)
	}
}
