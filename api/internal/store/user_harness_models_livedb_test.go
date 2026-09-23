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

	"github.com/vtmocanu/uzi/api/internal/store"
)

// user_harness_models_livedb_test.go proves the PRD #1551 M1 store seam against a real
// Postgres: the atomic grouped SetUserHarnessModels write, its all-or-nothing atomicity
// under an injected write failure, and the additive migration 00246's Up backfill
// classification. Each test stands its OWN isolated database (the appearance_backfill /
// set_user_appearance DB-per-test pattern) so a data-migration test never migrates the
// shared store-IT database, and the atomicity trigger cannot touch a parallel test.
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; run via
// ./e2e/run-store-it.sh. Every test name ends LiveDB so the sweep selects it.

// standIsolatedDB creates a fresh database on the same server as UZI_TEST_DATABASE_URL and
// returns its DSN, registering a best-effort DROP on cleanup. It does NOT migrate — the
// caller migrates to whatever version it needs (HEAD, or a specific version for a migration
// test). Returns "" only via t.Skip when UZI_TEST_DATABASE_URL is unset.
func standIsolatedDB(t *testing.T, prefix string) (context.Context, string) {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via the store-IT runner for live-DB coverage")
	}
	ctx := context.Background()
	name := prefix + strings.ReplaceAll(uuid.NewString(), "-", "")

	adminPool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open admin pool: %v", err)
	}
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		adminPool.Close()
		t.Fatalf("create database %s: %v", name, err)
	}
	adminPool.Close()

	t.Cleanup(func() {
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

	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	u.Path = "/" + name
	return ctx, u.String()
}

// TestSetUserHarnessModelsLiveDB proves the grouped PATCH write returns and stores all four
// fields, and that an unset field keeps its stored value (the PATCH contract that makes the
// atomic grouped save non-clobbering).
func TestSetUserHarnessModelsLiveDB(t *testing.T) {
	ctx, dsn := standIsolatedDB(t, "harness_models_")
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open work pool: %v", err)
	}
	defer pool.Close()
	q := store.New(pool)

	user, err := q.CreateUser(ctx, store.CreateUserParams{
		Email:        fmt.Sprintf("harness-models-%s@e2e", uuid.NewString()),
		PasswordHash: pgtype.Text{String: "x", Valid: true},
		DisplayName:  pgtype.Text{String: "Harness Models", Valid: true},
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	// Baseline: everything NULL, else the later assertions would be vacuous.
	base, err := q.GetUserSettings(ctx, user.ID)
	if err != nil {
		t.Fatalf("GetUserSettings(baseline): %v", err)
	}
	if base.DefaultHarness.Valid || base.DefaultClaudeModel.Valid || base.DefaultCodexModel.Valid || base.DefaultModel.Valid {
		t.Fatalf("baseline not all NULL: %+v", base)
	}

	txt := func(s string) pgtype.Text { return pgtype.Text{String: s, Valid: true} }

	// (a) Set all four together; RETURNING and a re-read reflect them.
	got, err := q.SetUserHarnessModels(ctx, store.SetUserHarnessModelsParams{
		SetHarness:         true,
		DefaultHarness:     txt("codex"),
		SetClaude:          true,
		DefaultClaudeModel: txt("opus"),
		SetCodex:           true,
		DefaultCodexModel:  txt("gpt-6-sol"),
		SetDefaultModel:    true,
		DefaultModel:       txt("gpt-6-sol"),
		ID:                 user.ID,
	})
	if err != nil {
		t.Fatalf("SetUserHarnessModels(all): %v", err)
	}
	if got.DefaultHarness.String != "codex" || got.DefaultClaudeModel.String != "opus" ||
		got.DefaultCodexModel.String != "gpt-6-sol" || got.DefaultModel.String != "gpt-6-sol" {
		t.Errorf("RETURNING = %+v, want {codex opus gpt-6-sol gpt-6-sol}", got)
	}
	after, err := q.GetUserSettings(ctx, user.ID)
	if err != nil {
		t.Fatalf("GetUserSettings(after all): %v", err)
	}
	if after.DefaultHarness.String != "codex" || after.DefaultClaudeModel.String != "opus" ||
		after.DefaultCodexModel.String != "gpt-6-sol" || after.DefaultModel.String != "gpt-6-sol" {
		t.Errorf("GetUserSettings(after all) = %+v, want {codex opus gpt-6-sol gpt-6-sol}", after)
	}
	// The dedicated per-harness read (M4 claim assembly reads through this) reflects both lanes.
	lanes, err := q.GetUserHarnessModelDefaults(ctx, user.ID)
	if err != nil {
		t.Fatalf("GetUserHarnessModelDefaults: %v", err)
	}
	if lanes.DefaultClaudeModel.String != "opus" || lanes.DefaultCodexModel.String != "gpt-6-sol" {
		t.Errorf("GetUserHarnessModelDefaults = %+v, want {opus gpt-6-sol}", lanes)
	}

	// (b) PATCH only the Claude lane; the Codex lane and harness KEEP their step-(a) values.
	if _, err := q.SetUserHarnessModels(ctx, store.SetUserHarnessModelsParams{
		SetClaude:          true,
		DefaultClaudeModel: txt("sonnet"),
		ID:                 user.ID,
	}); err != nil {
		t.Fatalf("SetUserHarnessModels(patch claude): %v", err)
	}
	patched, err := q.GetUserSettings(ctx, user.ID)
	if err != nil {
		t.Fatalf("GetUserSettings(after patch): %v", err)
	}
	if patched.DefaultClaudeModel.String != "sonnet" {
		t.Errorf("claude lane = %+v, want sonnet", patched.DefaultClaudeModel)
	}
	if patched.DefaultCodexModel.String != "gpt-6-sol" || patched.DefaultHarness.String != "codex" || patched.DefaultModel.String != "gpt-6-sol" {
		t.Errorf("patch clobbered an unset field: %+v, want codex/gpt-6-sol retained", patched)
	}
}

// TestSetUserHarnessModelsAtomicityLiveDB proves the grouped write is all-or-nothing: a
// test-scoped BEFORE UPDATE trigger that RAISEs when the Codex lane changes makes a combined
// harness+claude+codex write fail, and NONE of the four columns move — a database error
// cannot leave the harness changed while a lane stays stale (D1). The trigger is scoped by a
// WHEN on this test user's id and dropped on cleanup so it cannot affect any other row.
func TestSetUserHarnessModelsAtomicityLiveDB(t *testing.T) {
	ctx, dsn := standIsolatedDB(t, "harness_atomic_")
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open work pool: %v", err)
	}
	defer pool.Close()
	q := store.New(pool)

	txt := func(s string) pgtype.Text { return pgtype.Text{String: s, Valid: true} }

	user, err := q.CreateUser(ctx, store.CreateUserParams{
		Email:        fmt.Sprintf("harness-atomic-%s@e2e", uuid.NewString()),
		PasswordHash: pgtype.Text{String: "x", Valid: true},
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	// Seed a known starting state for all four columns.
	if _, err := q.SetUserHarnessModels(ctx, store.SetUserHarnessModelsParams{
		SetHarness: true, DefaultHarness: txt("claude"),
		SetClaude: true, DefaultClaudeModel: txt("opus"),
		SetCodex: true, DefaultCodexModel: txt("gpt-6-astra"),
		SetDefaultModel: true, DefaultModel: txt("opus"),
		ID: user.ID,
	}); err != nil {
		t.Fatalf("seed state: %v", err)
	}

	// A BEFORE UPDATE trigger that raises when THIS user's Codex lane changes. Names are
	// unique per test run; the WHEN scopes it to the test user so a parallel test's row is
	// untouched even on a shared server.
	fn := "test_block_codex_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	trg := "trg_" + fn
	mustExec(ctx, t, pool, fmt.Sprintf(
		`CREATE FUNCTION %s() RETURNS trigger AS $$ BEGIN RAISE EXCEPTION 'codex lane change blocked by test trigger'; END; $$ LANGUAGE plpgsql`,
		pgx.Identifier{fn}.Sanitize()))
	mustExec(ctx, t, pool, fmt.Sprintf(
		`CREATE TRIGGER %s BEFORE UPDATE ON users FOR EACH ROW
		 WHEN (NEW.id = '%s' AND NEW.default_codex_model IS DISTINCT FROM OLD.default_codex_model)
		 EXECUTE FUNCTION %s()`,
		pgx.Identifier{trg}.Sanitize(), user.ID.String(), pgx.Identifier{fn}.Sanitize()))
	t.Cleanup(func() {
		if _, err := pool.Exec(ctx, "DROP TRIGGER IF EXISTS "+pgx.Identifier{trg}.Sanitize()+" ON users"); err != nil {
			t.Logf("cleanup drop trigger: %v", err)
		}
		if _, err := pool.Exec(ctx, "DROP FUNCTION IF EXISTS "+pgx.Identifier{fn}.Sanitize()+"()"); err != nil {
			t.Logf("cleanup drop function: %v", err)
		}
	})

	// A grouped write that changes harness + claude + codex together. The codex change trips
	// the trigger, aborting the single UPDATE statement in full.
	_, err = q.SetUserHarnessModels(ctx, store.SetUserHarnessModelsParams{
		SetHarness: true, DefaultHarness: txt("codex"),
		SetClaude: true, DefaultClaudeModel: txt("sonnet"),
		SetCodex: true, DefaultCodexModel: txt("gpt-6-sol"),
		SetDefaultModel: true, DefaultModel: txt("gpt-6-sol"),
		ID: user.ID,
	})
	if err == nil {
		t.Fatal("SetUserHarnessModels must fail while the codex-blocking trigger is armed")
	}

	// All four columns must be exactly the seeded values — the failed statement moved nothing.
	after, err := q.GetUserSettings(ctx, user.ID)
	if err != nil {
		t.Fatalf("GetUserSettings(after failed write): %v", err)
	}
	if after.DefaultHarness.String != "claude" || after.DefaultClaudeModel.String != "opus" ||
		after.DefaultCodexModel.String != "gpt-6-astra" || after.DefaultModel.String != "opus" {
		t.Errorf("a failed grouped write moved a column: %+v, want {claude opus gpt-6-astra opus} unchanged", after)
	}
}

// TestUserPerHarnessBackfillLiveDB proves migration 00246's Up backfill classification in an
// isolated database standing at 245 (the version BEFORE 00246): the three curated Codex ids
// copy into the Codex lane, every other non-null legacy value copies into the Claude lane, a
// NULL legacy value backfills neither lane, and default_model itself is never touched.
func TestUserPerHarnessBackfillLiveDB(t *testing.T) {
	ctx, dsn := standIsolatedDB(t, "harness_backfill_")

	if err := store.MigrateTo(ctx, dsn, 245); err != nil {
		t.Fatalf("MigrateTo(245): %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open work pool: %v", err)
	}
	defer pool.Close()

	// Prove the new lanes do not exist yet, else the assertions below would be vacuous.
	var cols int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_schema='public' AND table_name='users'
		   AND column_name IN ('default_claude_model','default_codex_model')`).Scan(&cols); err != nil {
		t.Fatalf("probe lane columns at v245: %v", err)
	}
	if cols != 0 {
		t.Fatalf("lane columns already exist at v245 (found %d); MigrateTo(245) over-migrated", cols)
	}

	// Seed users with a spread of legacy default_model values via RAW SQL (the generated
	// CreateUser reflects HEAD, which has the lanes we are deliberately without here).
	type seed struct {
		id    uuid.UUID
		model any // string or nil
	}
	seeds := []seed{
		{uuid.New(), "gpt-6-astra"},     // curated Codex -> codex lane
		{uuid.New(), "gpt-5.6-sol"},     // curated Codex -> codex lane
		{uuid.New(), "gpt-6-sol"},       // curated Codex -> codex lane
		{uuid.New(), "opus"},            // Claude alias -> claude lane
		{uuid.New(), "claude-custom-x"}, // custom Claude id -> claude lane
		{uuid.New(), nil},               // NULL -> neither lane
	}
	for _, s := range seeds {
		if s.model == nil {
			mustExec(ctx, t, pool,
				`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
				s.id, fmt.Sprintf("bf-%s@e2e", s.id))
		} else {
			mustExec(ctx, t, pool,
				`INSERT INTO users (id, email, password_hash, default_model) VALUES ($1, $2, 'x', $3)`,
				s.id, fmt.Sprintf("bf-%s@e2e", s.id), s.model)
		}
	}

	// Apply 00246.
	if err := store.MigrateTo(ctx, dsn, 246); err != nil {
		t.Fatalf("MigrateTo(246): %v", err)
	}

	read := func(id uuid.UUID) (model, claude, codex pgtype.Text) {
		if err := pool.QueryRow(ctx,
			`SELECT default_model, default_claude_model, default_codex_model FROM users WHERE id=$1`, id).
			Scan(&model, &claude, &codex); err != nil {
			t.Fatalf("read lanes for %s: %v", id, err)
		}
		return
	}

	want := []struct {
		s      seed
		claude string // "" ⇒ NULL
		codex  string // "" ⇒ NULL
	}{
		{seeds[0], "", "gpt-6-astra"},
		{seeds[1], "", "gpt-5.6-sol"},
		{seeds[2], "", "gpt-6-sol"},
		{seeds[3], "opus", ""},
		{seeds[4], "claude-custom-x", ""},
		{seeds[5], "", ""},
	}
	for _, w := range want {
		model, claude, codex := read(w.s.id)
		// default_model is never touched by Up.
		if w.s.model == nil {
			if model.Valid {
				t.Errorf("%v: default_model = %+v, want NULL (Up must not touch it)", w.s.model, model)
			}
		} else if !model.Valid || model.String != w.s.model.(string) {
			t.Errorf("default_model = %+v, want the untouched %v", model, w.s.model)
		}
		if w.claude == "" {
			if claude.Valid {
				t.Errorf("legacy %v: claude lane = %+v, want NULL", w.s.model, claude)
			}
		} else if !claude.Valid || claude.String != w.claude {
			t.Errorf("legacy %v: claude lane = %+v, want %q", w.s.model, claude, w.claude)
		}
		if w.codex == "" {
			if codex.Valid {
				t.Errorf("legacy %v: codex lane = %+v, want NULL", w.s.model, codex)
			}
		} else if !codex.Valid || codex.String != w.codex {
			t.Errorf("legacy %v: codex lane = %+v, want %q", w.s.model, codex, w.codex)
		}
	}
}
