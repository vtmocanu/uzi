package store_test

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5/pgxpool"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4633 M2 against real Postgres: the shared Codex source writer, the cross-engine guard, the
// selection counts and the key-kind filter of the source reader. Every test stands its own isolated
// database. Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; run via
// ./e2e/run-store-it.sh.

func codexSelection(ctx context.Context, t *testing.T, pool *pgxpool.Pool, user uuid.UUID) (provider, scope *uuid.UUID) {
	t.Helper()
	var p, s *uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT codex_provider_id, codex_provider_scope_id FROM users WHERE id = $1`, user).Scan(&p, &s); err != nil {
		t.Fatalf("read codex selection: %v", err)
	}
	return p, s
}

func claudeSelection(ctx context.Context, t *testing.T, pool *pgxpool.Pool, user uuid.UUID) (provider, scope *uuid.UUID) {
	t.Helper()
	var p, s *uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT claude_provider_id, claude_provider_scope_id FROM users WHERE id = $1`, user).Scan(&p, &s); err != nil {
		t.Fatalf("read claude selection: %v", err)
	}
	return p, s
}

// TestSharedCodexSourceWriterLiveDB: SetUserProviderSource on the Codex engine accepts a shared Codex
// definition from a CLEARED selection (both columns NULL), writes the nil shared scope with it, leaves
// the Claude lane alone, and a clear writes both columns NULL again.
func TestSharedCodexSourceWriterLiveDB(t *testing.T) {
	ctx, _, pool := sharedDB(t, "shared_codex_writer_", 0)
	q := store.New(pool)
	b, _ := newProductUser(ctx, t, pool)
	def := insertSharedCodexRow(ctx, t, pool, "shared-codex-writer")

	if p, s := codexSelection(ctx, t, pool, b); p != nil || s != nil {
		t.Fatalf("fresh user codex selection = (%v, %v), want both NULL", p, s)
	}
	row, err := q.SetUserProviderSource(ctx, store.SetUserProviderSourceParams{ID: b, Engine: "codex", ProviderID: pgUUID(def)})
	if err != nil {
		t.Fatalf("selecting the shared Codex definition: %v", err)
	}
	if !row.CodexProviderID.Valid || uuid.UUID(row.CodexProviderID.Bytes) != def || row.ClaudeProviderID.Valid {
		t.Errorf("returned lanes = %+v, want codex = %s and no claude", row, def)
	}
	lanes, err := q.GetUserProviderLaneSources(ctx, b)
	if err != nil || !lanes.CodexProviderID.Valid || uuid.UUID(lanes.CodexProviderID.Bytes) != def || lanes.ClaudeProviderID.Valid {
		t.Errorf("lane sources = %+v (%v), want codex = %s only", lanes, err, def)
	}
	p, s := codexSelection(ctx, t, pool, b)
	if p == nil || *p != def || s == nil || s.String() != nilScope {
		t.Errorf("stored codex selection = (%v, %v), want (%s, %s)", p, s, def, nilScope)
	}
	if cp, cs := claudeSelection(ctx, t, pool, b); cp != nil || cs != nil {
		t.Errorf("selecting a Codex source touched the Claude lane: (%v, %v)", cp, cs)
	}

	if _, err := q.SetUserProviderSource(ctx, store.SetUserProviderSourceParams{ID: b, Engine: "codex"}); err != nil {
		t.Fatalf("clearing the Codex source: %v", err)
	}
	if p, s := codexSelection(ctx, t, pool, b); p != nil || s != nil {
		t.Errorf("after a clear the codex selection = (%v, %v), want both NULL", p, s)
	}
}

// TestSharedCodexSourceWriterRefusesCrossEngineLiveDB: the engine is enforced by the queries, not the
// database (the constraint test shows the database accepts the crossed shapes). The writer refuses a
// shared Codex definition on the Claude lane and a shared Claude definition on the Codex lane, leaving
// both columns unchanged, and the reader never resolves a crossed row.
func TestSharedCodexSourceWriterRefusesCrossEngineLiveDB(t *testing.T) {
	ctx, _, pool := sharedDB(t, "shared_codex_cross_", 0)
	q := store.New(pool)
	b, _ := newProductUser(ctx, t, pool)
	codexDef := insertSharedCodexRow(ctx, t, pool, "shared-codex-cross")
	claudeDef := newSharedProvider(ctx, t, q, "shared-claude-cross").ID

	if _, err := q.SetUserProviderSource(ctx, store.SetUserProviderSourceParams{ID: b, Engine: "codex", ProviderID: pgUUID(claudeDef)}); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("shared Claude definition on the Codex lane = %v, want no rows", err)
	}
	if _, err := q.SetUserProviderSource(ctx, store.SetUserProviderSourceParams{ID: b, Engine: "claude", ProviderID: pgUUID(codexDef)}); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("shared Codex definition on the Claude lane = %v, want no rows", err)
	}
	if p, s := codexSelection(ctx, t, pool, b); p != nil || s != nil {
		t.Errorf("codex selection = (%v, %v), want unchanged NULLs", p, s)
	}
	if p, s := claudeSelection(ctx, t, pool, b); p != nil || s != nil {
		t.Errorf("claude selection = (%v, %v), want unchanged NULLs", p, s)
	}

	// The reader's engine guard: force the crossed rows past the writer and read each lane.
	mustExec(ctx, t, pool, `UPDATE users SET codex_provider_id = $2, codex_provider_scope_id = $3 WHERE id = $1`, b, claudeDef, nilScope)
	mustExec(ctx, t, pool, `UPDATE users SET claude_provider_id = $2, claude_provider_scope_id = $3 WHERE id = $1`, b, codexDef, nilScope)
	for _, engine := range []string{"claude", "codex"} {
		src, err := q.GetUserProviderSource(ctx, store.GetUserProviderSourceParams{ID: b, Engine: engine})
		if err != nil || src.Engine.Valid || src.CurrentRevision.Valid {
			t.Errorf("%s lane over a crossed definition = %+v (%v), want an unresolved provider", engine, src, err)
		}
	}
}

// TestSharedCodexSelectionCountsLiveDB: CountSharedProviderSelections and the admin list/get count a
// shared definition's nil-scope selections through each engine's own lane, exactly, and never count a
// personal-scope selection.
func TestSharedCodexSelectionCountsLiveDB(t *testing.T) {
	ctx, _, pool := sharedDB(t, "shared_codex_counts_", 0)
	q := store.New(pool)
	u1, _ := newProductUser(ctx, t, pool)
	u2, _ := newProductUser(ctx, t, pool)
	u3, _ := newProductUser(ctx, t, pool)
	u4, _ := newProductUser(ctx, t, pool)

	claudeDef := newSharedProvider(ctx, t, q, "count-claude").ID
	codexDef := insertSharedCodexRow(ctx, t, pool, "count-codex")
	idleCodex := insertSharedCodexRow(ctx, t, pool, "count-idle-codex")
	personal := newProvider(ctx, t, q, u3, "u3-claude")

	selectClaude := func(u, def uuid.UUID, scope string) {
		mustExec(ctx, t, pool, `UPDATE users SET claude_provider_id = $2, claude_provider_scope_id = $3 WHERE id = $1`, u, def, scope)
	}
	selectCodex := func(u, def uuid.UUID, scope string) {
		mustExec(ctx, t, pool, `UPDATE users SET codex_provider_id = $2, codex_provider_scope_id = $3 WHERE id = $1`, u, def, scope)
	}
	// Claude-only definition: u1 and u2 select it; u3's personal selection carries its own scope.
	selectClaude(u1, claudeDef, nilScope)
	selectClaude(u2, claudeDef, nilScope)
	selectClaude(u3, personal.ID, u3.String())
	// Codex-only definition: u2 (who also selects the Claude one: a mixed user) and u4.
	selectCodex(u2, codexDef, nilScope)
	selectCodex(u4, codexDef, nilScope)

	want := map[uuid.UUID]int64{claudeDef: 2, codexDef: 2, idleCodex: 0}
	for id, n := range want {
		got, err := q.CountSharedProviderSelections(ctx, id)
		if err != nil || got != n {
			t.Errorf("CountSharedProviderSelections(%s) = %d (%v), want %d", id, got, err, n)
		}
		row, err := q.GetSharedModelProviderAdmin(ctx, id)
		if err != nil || row.SelectedUserCount != n {
			t.Errorf("GetSharedModelProviderAdmin(%s) selected = %d (%v), want %d", id, row.SelectedUserCount, err, n)
		}
	}
	rows, err := q.ListSharedModelProvidersAdmin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, r := range rows {
		if n, ok := want[r.ID]; ok {
			seen++
			if r.SelectedUserCount != n {
				t.Errorf("ListSharedModelProvidersAdmin(%s) selected = %d, want %d", r.ID, r.SelectedUserCount, n)
			}
		}
	}
	if seen != len(want) {
		t.Errorf("list returned %d of the %d seeded definitions", seen, len(want))
	}
}

// TestSharedCodexWrongKindKeySQLLiveDB: the Codex source reader joins only an openai_compatible_key.
// An anthropic_compatible_key attached to a selected shared Codex definition (the database allows it)
// resolves no key, while an openai_compatible_key for another user resolves.
func TestSharedCodexWrongKindKeySQLLiveDB(t *testing.T) {
	ctx, _, pool := sharedDB(t, "shared_codex_kind_", 0)
	q := store.New(pool)
	bad, _ := newProductUser(ctx, t, pool)
	good, _ := newProductUser(ctx, t, pool)
	def := insertSharedCodexRow(ctx, t, pool, "kind-codex")
	for _, u := range []uuid.UUID{bad, good} {
		if _, err := q.SetUserProviderSource(ctx, store.SetUserProviderSourceParams{ID: u, Engine: "codex", ProviderID: pgUUID(def)}); err != nil {
			t.Fatalf("select: %v", err)
		}
	}
	mustExec(ctx, t, pool, `INSERT INTO user_secrets (user_id, kind, label, is_default, auto_eligible, ciphertext, sealed_with, provider_id, provider_scope_id)
        VALUES ($1, 'anthropic_compatible_key', 'k', false, false, 'ct', 'master', $2, $3)`, bad, def, nilScope)
	insertSharedCodexKey(ctx, t, pool, good, def)

	src, err := q.GetUserProviderSource(ctx, store.GetUserProviderSourceParams{ID: bad, Engine: "codex"})
	if err != nil || !src.ProviderID.Valid || src.KeySecretID.Valid {
		t.Errorf("wrong-kind key: source = %+v (%v), want the provider with a NULL key", src, err)
	}
	src, err = q.GetUserProviderSource(ctx, store.GetUserProviderSourceParams{ID: good, Engine: "codex"})
	if err != nil || !src.KeySecretID.Valid || src.KeyKind.String != store.KindOpenAICompatibleKey {
		t.Errorf("control: source = %+v (%v), want the openai_compatible_key", src, err)
	}
}
