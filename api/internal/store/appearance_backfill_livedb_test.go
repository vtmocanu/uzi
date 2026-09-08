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
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestUserAppearanceBackfillLiveDB is the regression gate for the data backfill in
// 00203_user_appearance.sql (PRD #1167 M1): the
// `UPDATE users SET dark_theme = theme, appearance_mode = 'dark' WHERE theme IS NOT NULL`
// that pins EXISTING per-user theme overrides into the new dark slot at dark mode, at the
// same time the four appearance columns are added — so nobody's screen changes on upgrade.
//
// # THE BLIND SPOT THIS TEST EXISTS FOR
//
// A data-migration backfill runs exactly once, over whatever rows exist when the migration
// is applied. Every OTHER live-DB test in this package reaches the schema through
// store.Migrate (migrations to HEAD), so by the time they insert anything 00203 has already
// run over a users table with no theme'd rows and the backfill matched nothing. A miswrite
// of the backfill is therefore STRUCTURALLY INVISIBLE to them: it can only be wrong about
// pre-existing theme'd rows, and an always-at-head fixture never has any.
//
// This test uses the store.MigrateTo seam to stand a throwaway database at version 202 (the
// version BEFORE 00203), seed a user WITH a theme override and a user WITHOUT one via RAW
// SQL (the appearance columns do not exist yet), then apply 00203 and assert the backfill
// stamped exactly the theme'd row (dark_theme == its theme, appearance_mode == 'dark') and
// left the un-themed row's four appearance columns NULL. 00203 is HEAD, so the generated
// GetUserSettings reflects the same schema and reads the four columns back directly.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; the store-IT runner
// provides one. `go test ./...` without it SKIPs.
func TestUserAppearanceBackfillLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via the store-IT runner for live-DB coverage")
	}
	ctx := context.Background()

	// --- Stand an isolated database on the same server. CREATE DATABASE cannot run
	// inside a transaction; a bare pool Exec is autocommit. The name is all-lowercase
	// hex + underscore, a safe identifier; still Sanitize it.
	name := "appearance_bf_" + strings.ReplaceAll(uuid.NewString(), "-", "")

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

	// --- Migrate the isolated database to 202 — the version BEFORE 00203.
	if err := store.MigrateTo(ctx, newDSN, 202); err != nil {
		t.Fatalf("MigrateTo(202): %v", err)
	}

	pool, err = store.OpenPool(ctx, newDSN)
	if err != nil {
		t.Fatalf("open work pool: %v", err)
	}
	// NOTE: no `defer pool.Close()` — the cleanup above owns closing pool.

	// --- Prove we are genuinely pre-migration: the appearance columns must not exist
	// yet, else the seam over-migrated and the backfill assertion would be vacuous.
	var colCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_schema = 'public' AND table_name = 'users'
		   AND column_name IN ('appearance_mode', 'light_theme', 'dark_theme', 'typeface')`).Scan(&colCount); err != nil {
		t.Fatalf("probe users appearance columns at v202: %v", err)
	}
	if colCount != 0 {
		t.Fatalf("users appearance columns already exist at v202 (found %d) — MigrateTo(202) did not stop before 00203; test would be vacuous", colCount)
	}

	// --- Seed two users via RAW SQL (the generated CreateUser reflects the HEAD schema;
	// we are deliberately at v202). The THEMED user carries a per-user theme override; the
	// UN-THEMED user leaves theme NULL. Emails are derived from the fresh UUIDs to stay
	// unique against the shared server.
	themedID, plainID := uuid.New(), uuid.New()
	mustExec(ctx, t, pool,
		`INSERT INTO users (id, email, password_hash, theme) VALUES ($1, $2, 'x', 'mission')`,
		themedID, fmt.Sprintf("appearance-themed-%s@e2e", themedID))
	mustExec(ctx, t, pool,
		`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		plainID, fmt.Sprintf("appearance-plain-%s@e2e", plainID))

	// --- Apply the migration under test (00203 is HEAD).
	if err := store.MigrateTo(ctx, newDSN, 203); err != nil {
		t.Fatalf("MigrateTo(203): %v", err)
	}

	q := store.New(pool)

	// --- Assertion 1: the backfill pinned the pre-existing THEMED row into the dark slot
	// at dark mode, and touched neither the light slot nor the typeface. Read through the
	// generated GetUserSettings (HEAD schema == 203).
	themed, err := q.GetUserSettings(ctx, themedID)
	if err != nil {
		t.Fatalf("GetUserSettings(themed) after 00203: %v", err)
	}
	if !themed.DarkTheme.Valid || themed.DarkTheme.String != "mission" {
		t.Errorf("backfill: themed user dark_theme = %+v, want {mission true} (00203 did not copy theme into the dark slot)", themed.DarkTheme)
	}
	if !themed.AppearanceMode.Valid || themed.AppearanceMode.String != "dark" {
		t.Errorf("backfill: themed user appearance_mode = %+v, want {dark true} (00203 did not pin dark mode)", themed.AppearanceMode)
	}
	if themed.LightTheme.Valid {
		t.Errorf("backfill: themed user light_theme = %+v, want NULL (the backfill must not touch the light slot)", themed.LightTheme)
	}
	if themed.Typeface.Valid {
		t.Errorf("backfill: themed user typeface = %+v, want NULL (the backfill must not touch the typeface)", themed.Typeface)
	}

	// --- Assertion 2: the UN-THEMED row (theme NULL) took no backfill — all four
	// appearance columns remain NULL, so it inherits every instance default.
	plain, err := q.GetUserSettings(ctx, plainID)
	if err != nil {
		t.Fatalf("GetUserSettings(plain) after 00203: %v", err)
	}
	if plain.AppearanceMode.Valid || plain.LightTheme.Valid || plain.DarkTheme.Valid || plain.Typeface.Valid {
		t.Errorf("un-themed user appearance columns = {mode:%+v light:%+v dark:%+v typeface:%+v}, want all NULL (backfill matched a theme-NULL row)",
			plain.AppearanceMode, plain.LightTheme, plain.DarkTheme, plain.Typeface)
	}
}
