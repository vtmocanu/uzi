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
// (PRD #1167 M2): the single 4-column `UPDATE users SET appearance_mode = $1,
// light_theme = $2, dark_theme = $3, typeface = $4 ... RETURNING ...` that backs
// PUT /api/me/settings. TestUserAppearanceBackfillLiveDB (M1) only proves the
// migration's one-shot backfill; it never calls SetUserAppearance itself. This
// test does, against a real Postgres, and checks three shapes of the write:
//
//  1. all four set to valid registry values -> RETURNING and a subsequent
//     GetUserSettings both reflect them;
//  2. all four cleared to SQL NULL (pgtype.Text{Valid:false}) -> RETURNING and a
//     re-read both show NULL again (the resolver's "inherit" sentinel);
//  3. a partial write (only dark_theme set, the other three NULL) -> RETURNING
//     and a re-read agree, proving the statement does not silently preserve a
//     stale value in an unset column (a 4-column UPDATE with no COALESCE would).
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

	// --- (a) Set all four to valid registry values.
	set, err := q.SetUserAppearance(ctx, store.SetUserAppearanceParams{
		AppearanceMode: pgtype.Text{String: "light", Valid: true},
		LightTheme:     pgtype.Text{String: "dawn", Valid: true},
		DarkTheme:      pgtype.Text{String: "mission", Valid: true},
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

	// --- (b) Clear all four back to NULL (the "inherit" sentinel). A 4-column
	// UPDATE that forgot a column, or one that COALESCEd instead of overwriting,
	// would leave a stale value here.
	cleared, err := q.SetUserAppearance(ctx, store.SetUserAppearanceParams{
		AppearanceMode: pgtype.Text{Valid: false},
		LightTheme:     pgtype.Text{Valid: false},
		DarkTheme:      pgtype.Text{Valid: false},
		Typeface:       pgtype.Text{Valid: false},
		ID:             user.ID,
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

	// --- (c) A partial write: only dark_theme set, the other three NULL. Proves
	// the RETURNING row and a fresh re-read agree field-by-field, including the
	// three that must come back NULL rather than silently keeping whatever the
	// previous write left there.
	partial, err := q.SetUserAppearance(ctx, store.SetUserAppearanceParams{
		AppearanceMode: pgtype.Text{Valid: false},
		LightTheme:     pgtype.Text{Valid: false},
		DarkTheme:      pgtype.Text{String: "ember", Valid: true},
		Typeface:       pgtype.Text{Valid: false},
		ID:             user.ID,
	})
	if err != nil {
		t.Fatalf("SetUserAppearance(partial): %v", err)
	}
	if partial.AppearanceMode.Valid || partial.LightTheme.Valid || partial.Typeface.Valid {
		t.Errorf("SetUserAppearance(partial) RETURNING left a non-dark field non-NULL: %+v", partial)
	}
	if !partial.DarkTheme.Valid || partial.DarkTheme.String != "ember" {
		t.Errorf("SetUserAppearance(partial) RETURNING dark_theme = %+v, want {ember true}", partial.DarkTheme)
	}
	afterPartial, err := q.GetUserSettings(ctx, user.ID)
	if err != nil {
		t.Fatalf("GetUserSettings(after partial): %v", err)
	}
	if afterPartial.AppearanceMode.Valid || afterPartial.LightTheme.Valid || afterPartial.Typeface.Valid {
		t.Errorf("GetUserSettings(after partial) left a non-dark field non-NULL: %+v", afterPartial)
	}
	if !afterPartial.DarkTheme.Valid || afterPartial.DarkTheme.String != "ember" {
		t.Errorf("GetUserSettings(after partial) dark_theme = %+v, want {ember true}", afterPartial.DarkTheme)
	}
	// RETURNING and the fresh re-read must agree exactly (proves the RETURNING
	// clause isn't reporting the parameters back verbatim while the actual row
	// diverges).
	if partial.AppearanceMode != afterPartial.AppearanceMode || partial.LightTheme != afterPartial.LightTheme ||
		partial.DarkTheme != afterPartial.DarkTheme || partial.Typeface != afterPartial.Typeface {
		t.Errorf("RETURNING %+v does not match re-read %+v", partial, afterPartial)
	}
}
