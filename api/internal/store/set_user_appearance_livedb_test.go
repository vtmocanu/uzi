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

// TestSetUserAppearanceLiveDB exercises the store.SetUserAppearance WRITE seam
// (PRD #1167 M2): the single conditional `UPDATE users SET appearance_mode =
// CASE WHEN $1 THEN $2 ELSE appearance_mode END, ... RETURNING ...` that backs
// PUT /api/me/settings. TestUserAppearanceBackfillLiveDB (M1) only proves the
// migration's one-shot backfill; it never calls SetUserAppearance itself. This
// test does, against a real Postgres, and checks the PATCH contract:
//
//  1. all four flags set to valid registry values -> RETURNING and a subsequent
//     GetUserSettings both reflect them;
//  2. a patch with only set_dark true (a new dark_theme, the other three flags
//     false) -> dark_theme changes while the other three KEEP their step-1 values,
//     proving an unset field is preserved (the whole point of the conditional
//     UPDATE: two concurrent saves of different fields cannot clobber each other);
//  3. a set flag with a NULL value (set_mode true, value NULL) -> that field
//     CLEARS to NULL (the resolver's "inherit" sentinel) while the still-unset
//     fields stay put;
//  4. all four flags set to NULL -> RETURNING and a re-read both show all NULL.
//
// Mirrors TestUserAppearanceBackfillLiveDB's DB-per-test setup (an isolated
// database, CREATE/DROP via a fresh admin pool, cleanup via t.Cleanup), but
// migrates straight to HEAD with store.Migrate since there is no pre-migration
// state to stand up here. Skipped unless UZI_TEST_DATABASE_URL points at a
// throwaway Postgres; the store-IT runner provides one.
func TestSetUserAppearanceLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via the store-IT runner for live-DB coverage")
	}
	ctx := context.Background()

	// --- Stand an isolated database on the same server (DB-per-test pattern
	// borrowed from TestUserAppearanceBackfillLiveDB). CREATE DATABASE cannot run
	// inside a transaction; a bare pool Exec is autocommit.
	name := "set_appearance_" + strings.ReplaceAll(uuid.NewString(), "-", "")

	adminPool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open admin pool: %v", err)
	}
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		adminPool.Close()
		t.Fatalf("create database %s: %v", name, err)
	}
	adminPool.Close()

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

	// --- Migrate the isolated database straight to HEAD: no pre-migration state
	// is needed for a write-seam test.
	if err := store.Migrate(ctx, newDSN); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	pool, err = store.OpenPool(ctx, newDSN)
	if err != nil {
		t.Fatalf("open work pool: %v", err)
	}
	// NOTE: no `defer pool.Close()` — the cleanup above owns closing pool.

	q := store.New(pool)

	// --- Seed one user via the generated CreateUser (HEAD schema, so no raw SQL
	// is needed here unlike the pre-migration backfill test).
	email := fmt.Sprintf("set-appearance-%s@e2e", uuid.NewString())
	user, err := q.CreateUser(ctx, store.CreateUserParams{
		Email:        email,
		PasswordHash: pgtype.Text{String: "x", Valid: true},
		DisplayName:  pgtype.Text{String: "Appearance Writer", Valid: true},
		IsAdmin:      false,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	// A freshly-created user carries no appearance override: prove the baseline
	// is NULL before writing anything, else the later NULL assertions would be
	// vacuous.
	base, err := q.GetUserSettings(ctx, user.ID)
	if err != nil {
		t.Fatalf("GetUserSettings(baseline): %v", err)
	}
	if base.AppearanceMode.Valid || base.LightTheme.Valid || base.DarkTheme.Valid || base.Typeface.Valid {
		t.Fatalf("baseline appearance columns not all NULL: %+v", base)
	}

	// --- (a) Set all four flags to valid registry values.
	set, err := q.SetUserAppearance(ctx, store.SetUserAppearanceParams{
		SetMode:        true,
		AppearanceMode: pgtype.Text{String: "light", Valid: true},
		SetLight:       true,
		LightTheme:     pgtype.Text{String: "dawn", Valid: true},
		SetDark:        true,
		DarkTheme:      pgtype.Text{String: "mission", Valid: true},
		SetTypeface:    true,
		Typeface:       pgtype.Text{String: "plex", Valid: true},
		ID:             user.ID,
	})
	if err != nil {
		t.Fatalf("SetUserAppearance(all-set): %v", err)
	}
	if set.AppearanceMode.String != "light" || set.LightTheme.String != "dawn" ||
		set.DarkTheme.String != "mission" || set.Typeface.String != "plex" {
		t.Errorf("SetUserAppearance(all-set) RETURNING = %+v, want {light dawn mission plex}", set)
	}
	afterSet, err := q.GetUserSettings(ctx, user.ID)
	if err != nil {
		t.Fatalf("GetUserSettings(after all-set): %v", err)
	}
	if afterSet.AppearanceMode.String != "light" || afterSet.LightTheme.String != "dawn" ||
		afterSet.DarkTheme.String != "mission" || afterSet.Typeface.String != "plex" {
		t.Errorf("GetUserSettings(after all-set) = %+v, want {light dawn mission plex}", afterSet)
	}

	// --- (b) Patch ONLY dark_theme (set_dark true, the other three flags false).
	// dark_theme must change to ember while mode/light/typeface KEEP their step-(a)
	// values. This is the contract that makes concurrent saves of different fields
	// non-clobbering: an unset field re-writes its own stored value.
	patched, err := q.SetUserAppearance(ctx, store.SetUserAppearanceParams{
		SetDark:   true,
		DarkTheme: pgtype.Text{String: "ember", Valid: true},
		ID:        user.ID,
	})
	if err != nil {
		t.Fatalf("SetUserAppearance(patch dark only): %v", err)
	}
	if !patched.DarkTheme.Valid || patched.DarkTheme.String != "ember" {
		t.Errorf("SetUserAppearance(patch) RETURNING dark_theme = %+v, want {ember true}", patched.DarkTheme)
	}
	if patched.AppearanceMode.String != "light" || patched.LightTheme.String != "dawn" || patched.Typeface.String != "plex" {
		t.Errorf("SetUserAppearance(patch) clobbered an unset field: %+v, want mode=light light=dawn typeface=plex", patched)
	}
	afterPatch, err := q.GetUserSettings(ctx, user.ID)
	if err != nil {
		t.Fatalf("GetUserSettings(after patch): %v", err)
	}
	if afterPatch.DarkTheme.String != "ember" || afterPatch.AppearanceMode.String != "light" ||
		afterPatch.LightTheme.String != "dawn" || afterPatch.Typeface.String != "plex" {
		t.Errorf("GetUserSettings(after patch) = %+v, want dark=ember mode=light light=dawn typeface=plex", afterPatch)
	}

	// --- (c) A set flag with a NULL value CLEARS that field (set_mode true, value
	// NULL) while the still-unset fields stay put.
	clearedMode, err := q.SetUserAppearance(ctx, store.SetUserAppearanceParams{
		SetMode:        true,
		AppearanceMode: pgtype.Text{Valid: false},
		ID:             user.ID,
	})
	if err != nil {
		t.Fatalf("SetUserAppearance(clear mode): %v", err)
	}
	if clearedMode.AppearanceMode.Valid {
		t.Errorf("SetUserAppearance(clear mode) RETURNING mode = %+v, want NULL", clearedMode.AppearanceMode)
	}
	if clearedMode.DarkTheme.String != "ember" || clearedMode.LightTheme.String != "dawn" || clearedMode.Typeface.String != "plex" {
		t.Errorf("SetUserAppearance(clear mode) clobbered an unset field: %+v", clearedMode)
	}

	// --- (d) Clear all four (every flag true, every value NULL) -> all NULL, on
	// both the RETURNING row and a fresh re-read.
	cleared, err := q.SetUserAppearance(ctx, store.SetUserAppearanceParams{
		SetMode:     true,
		SetLight:    true,
		SetDark:     true,
		SetTypeface: true,
		ID:          user.ID,
	})
	if err != nil {
		t.Fatalf("SetUserAppearance(clear-all): %v", err)
	}
	if cleared.AppearanceMode.Valid || cleared.LightTheme.Valid || cleared.DarkTheme.Valid || cleared.Typeface.Valid {
		t.Errorf("SetUserAppearance(clear-all) RETURNING = %+v, want all NULL", cleared)
	}
	afterClear, err := q.GetUserSettings(ctx, user.ID)
	if err != nil {
		t.Fatalf("GetUserSettings(after clear-all): %v", err)
	}
	if afterClear.AppearanceMode.Valid || afterClear.LightTheme.Valid || afterClear.DarkTheme.Valid || afterClear.Typeface.Valid {
		t.Errorf("GetUserSettings(after clear-all) = %+v, want all NULL", afterClear)
	}
}
