package store_test

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #4324 M1 against real Postgres: the ownership/reference migrations 00524-00531, the shared
// catalogue queries and the per-key Test evidence fence. Every test stands its own isolated database
// (standIsolatedDB), so shared definitions never leak into the sweep's shared database. Skipped
// unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; run via ./e2e/run-store-it.sh.

const nilScope = "00000000-0000-0000-0000-000000000000"

// sharedDB stands an isolated database migrated to version (0 means all migrations) and returns a
// pool with room for the multi-session tests.
func sharedDB(t *testing.T, prefix string, version int64) (context.Context, string, *pgxpool.Pool) {
	t.Helper()
	ctx, dsn := standIsolatedDB(t, prefix)
	migrateTo(ctx, t, dsn, version)
	return ctx, dsn, openSharedPool(ctx, t, dsn)
}

func migrateTo(ctx context.Context, t *testing.T, dsn string, version int64) {
	t.Helper()
	var err error
	if version == 0 {
		err = store.Migrate(ctx, dsn)
	} else {
		err = store.MigrateTo(ctx, dsn, version)
	}
	if err != nil {
		t.Fatalf("migrate to %d: %v", version, err)
	}
}

func openSharedPool(ctx context.Context, t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	qv := u.Query()
	qv.Set("pool_max_conns", "16")
	u.RawQuery = qv.Encode()
	pool, err := store.OpenPool(ctx, u.String())
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// wantViolation fails unless err is a Postgres error with code and one of the constraint names.
func wantViolation(t *testing.T, what string, err error, code string, names ...string) {
	t.Helper()
	gotCode, gotName := pgCodeC(err)
	if err == nil || gotCode != code {
		t.Errorf("%s: got %v (%s %s), want SQLSTATE %s on %v", what, err, gotCode, gotName, code, names)
		return
	}
	for _, n := range names {
		if gotName == n {
			return
		}
	}
	t.Errorf("%s: violated %q, want one of %v (%v)", what, gotName, names, err)
}

func newSharedProvider(ctx context.Context, t *testing.T, q *store.Queries, name string) store.ModelProvider {
	t.Helper()
	p, err := q.InsertSharedModelProvider(ctx, name)
	if err != nil {
		t.Fatalf("insert shared provider %q: %v", name, err)
	}
	addRevision(ctx, t, q, p.ID, 1, "https://gw.example.com/anthropic")
	return p
}

func scalar[T any](ctx context.Context, t *testing.T, pool *pgxpool.Pool, sql string, args ...any) T {
	t.Helper()
	var v T
	if err := pool.QueryRow(ctx, sql, args...).Scan(&v); err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
	return v
}

func appliedVersion(ctx context.Context, t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	// goose records each Down as an is_applied=false row, so the head is the newest row's state.
	return scalar[int64](ctx, t, pool, `SELECT COALESCE(max(version_id), 0) FROM goose_db_version g
        WHERE g.is_applied AND NOT EXISTS (SELECT 1 FROM goose_db_version d WHERE d.version_id = g.version_id AND NOT d.is_applied AND d.id > g.id)`)
}

// seedLegacy writes personal data with the 00523 schema (no scope columns): a Claude provider with two
// revisions, its key and a selection, a Codex provider with its key and selection for another user, and
// a legacy probe row. It returns the ids and the key ciphertext it wrote.
type legacySeed struct {
	userA, userB       uuid.UUID
	claudeP, codexP    uuid.UUID
	claudeKey, codexKy uuid.UUID
	claudeCT, codexCT  []byte
}

func seedLegacy(ctx context.Context, t *testing.T, pool *pgxpool.Pool) legacySeed {
	t.Helper()
	s := legacySeed{userA: uuid.New(), userB: uuid.New(), claudeP: uuid.New(), codexP: uuid.New(), claudeKey: uuid.New(), codexKy: uuid.New(),
		claudeCT: []byte{0x01, 0x02, 0xfe, 0xff, 0x00, 0x7f}, codexCT: []byte("codex-ciphertext-\x00-bytes")}
	for _, u := range []uuid.UUID{s.userA, s.userB} {
		mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, u, u.String()+"@example.com")
	}
	mustExec(ctx, t, pool, `INSERT INTO model_providers (id, owner_user_id, name, engine, current_revision) VALUES ($1, $2, 'claude-gw', 'claude', 2)`, s.claudeP, s.userA)
	mustExec(ctx, t, pool, `INSERT INTO model_provider_revisions (provider_id, revision, protocol, base_url, auth_style, model_map) VALUES
        ($1, 1, 'anthropic_messages', 'https://gw.example.com/v1', 'bearer', '{"haiku":{"model":"h1"}}'),
        ($1, 2, 'anthropic_messages', 'https://gw.example.com/v1', 'bearer', '{"haiku":{"model":"h2"}}')`, s.claudeP)
	mustExec(ctx, t, pool, `INSERT INTO user_secrets (id, user_id, kind, label, is_default, auto_eligible, ciphertext, sealed_with, provider_id)
        VALUES ($1, $2, 'anthropic_compatible_key', 'k', false, false, $3, 'master', $4)`, s.claudeKey, s.userA, s.claudeCT, s.claudeP)
	mustExec(ctx, t, pool, `UPDATE users SET claude_provider_id = $2 WHERE id = $1`, s.userA, s.claudeP)
	mustExec(ctx, t, pool, `INSERT INTO model_provider_probe_results (provider_id, revision, auth_result) VALUES ($1, 2, 'ok')`, s.claudeP)

	mustExec(ctx, t, pool, `INSERT INTO model_providers (id, owner_user_id, name, engine, current_revision) VALUES ($1, $2, 'codex-gw', 'codex', 1)`, s.codexP, s.userB)
	mustExec(ctx, t, pool, `INSERT INTO model_provider_revisions (provider_id, revision, protocol, base_url, auth_style, model_map, engine)
        VALUES ($1, 1, 'openai_responses', 'https://resp.example.com/v1', 'bearer', '{"default":{"model":"m"}}', 'codex')`, s.codexP)
	mustExec(ctx, t, pool, `INSERT INTO user_secrets (id, user_id, kind, label, is_default, auto_eligible, ciphertext, sealed_with, provider_id)
        VALUES ($1, $2, 'openai_compatible_key', 'k', false, false, $3, 'master', $4)`, s.codexKy, s.userB, s.codexCT, s.codexP)
	mustExec(ctx, t, pool, `UPDATE users SET codex_provider_id = $2 WHERE id = $1`, s.userB, s.codexP)
	return s
}

// TestSharedScopeUpgradePreservesPersonalDataLiveDB: upgrading a 00523 database keeps every personal
// provider, key (byte-equal ciphertext), selection and revision, backfills the scope columns to the
// owner, discards the legacy provider-wide probe cache, and leaves every new constraint validated.
func TestSharedScopeUpgradePreservesPersonalDataLiveDB(t *testing.T) {
	ctx, dsn := standIsolatedDB(t, "shared_scope_upgrade_")
	migrateTo(ctx, t, dsn, 523)
	pool := openSharedPool(ctx, t, dsn)
	s := seedLegacy(ctx, t, pool)
	revisions := func() string {
		return scalar[string](ctx, t, pool, `SELECT jsonb_agg(jsonb_build_array(provider_id, revision, engine, protocol,
				base_url, auth_style, model_map, created_at) ORDER BY provider_id, revision)::text
			FROM model_provider_revisions WHERE provider_id IN ($1, $2)`, s.claudeP, s.codexP)
	}
	beforeRevisions := revisions()
	migrateTo(ctx, t, dsn, 0)

	if got := scalar[int](ctx, t, pool, `SELECT count(*) FROM model_providers`); got != 2 {
		t.Fatalf("providers after upgrade = %d, want 2", got)
	}
	for _, c := range []struct {
		id    uuid.UUID
		owner uuid.UUID
		rev   int
	}{{s.claudeP, s.userA, 2}, {s.codexP, s.userB, 1}} {
		var owner, scope uuid.UUID
		var mode pgtype.Text
		var rev int
		if err := pool.QueryRow(ctx, `SELECT owner_user_id, scope_owner_id, credential_mode, current_revision FROM model_providers WHERE id = $1`, c.id).Scan(&owner, &scope, &mode, &rev); err != nil {
			t.Fatal(err)
		}
		if owner != c.owner || scope != c.owner || mode.Valid || rev != c.rev {
			t.Errorf("provider %s: owner=%s scope=%s mode=%v rev=%d, want owner and scope %s, no mode, rev %d", c.id, owner, scope, mode, rev, c.owner, c.rev)
		}
	}
	if got := revisions(); got != beforeRevisions {
		t.Errorf("provider revisions changed on upgrade:\nbefore: %s\nafter: %s", beforeRevisions, got)
	}
	for _, k := range []struct {
		id    uuid.UUID
		owner uuid.UUID
		ct    []byte
	}{{s.claudeKey, s.userA, s.claudeCT}, {s.codexKy, s.userB, s.codexCT}} {
		var ct []byte
		var scope uuid.UUID
		if err := pool.QueryRow(ctx, `SELECT ciphertext, provider_scope_id FROM user_secrets WHERE id = $1`, k.id).Scan(&ct, &scope); err != nil {
			t.Fatal(err)
		}
		if string(ct) != string(k.ct) || scope != k.owner {
			t.Errorf("key %s: ciphertext equal=%t scope=%s, want byte-equal and scope %s", k.id, string(ct) == string(k.ct), scope, k.owner)
		}
	}
	var claudeSel, claudeScope, codexSel pgtype.UUID
	if err := pool.QueryRow(ctx, `SELECT claude_provider_id, claude_provider_scope_id FROM users WHERE id = $1`, s.userA).Scan(&claudeSel, &claudeScope); err != nil {
		t.Fatal(err)
	}
	if uuid.UUID(claudeSel.Bytes) != s.claudeP || uuid.UUID(claudeScope.Bytes) != s.userA {
		t.Errorf("user A source = %v scope %v, want %s scope %s", claudeSel, claudeScope, s.claudeP, s.userA)
	}
	if err := pool.QueryRow(ctx, `SELECT codex_provider_id FROM users WHERE id = $1`, s.userB).Scan(&codexSel); err != nil || uuid.UUID(codexSel.Bytes) != s.codexP {
		t.Errorf("user B codex source = %v (%v), want %s", codexSel, err, s.codexP)
	}
	if got := scalar[int](ctx, t, pool, `SELECT count(*) FROM users WHERE id = $1 AND claude_provider_scope_id IS NULL`, s.userB); got != 1 {
		t.Errorf("a user with no Claude source must keep a NULL scope")
	}
	if got := scalar[int](ctx, t, pool, `SELECT count(*) FROM model_provider_probe_results`); got != 0 {
		t.Errorf("legacy probe rows after 00531 = %d, want 0 (provider-wide cache discarded)", got)
	}
	var unvalidated []string
	rows, err := pool.Query(ctx, `SELECT conrelid::regclass::text || '.' || conname FROM pg_constraint
        WHERE NOT convalidated AND conrelid IN ('model_providers'::regclass, 'user_secrets'::regclass, 'users'::regclass)`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		unvalidated = append(unvalidated, n)
	}
	rows.Close()
	if len(unvalidated) != 0 {
		t.Errorf("constraints left NOT VALID after the upgrade: %v", unvalidated)
	}
	if got := scalar[string](ctx, t, pool, `SELECT is_nullable FROM information_schema.columns WHERE table_name = 'model_providers' AND column_name = 'scope_owner_id'`); got != "NO" {
		t.Errorf("scope_owner_id nullable = %s, want NO", got)
	}
	for _, idx := range []string{"model_providers_scope_owner_id_key", "model_providers_shared_name_lower_key", "users_claude_provider_id_idx", "model_providers_owner_id_key", "model_providers_owner_name_lower_key"} {
		if !scalar[bool](ctx, t, pool, `SELECT COALESCE((SELECT indisvalid FROM pg_index WHERE indexrelid = to_regclass($1)), false)`, idx) {
			t.Errorf("index %s missing or invalid after the upgrade", idx)
		}
	}
}

// TestSharedScopeConstraintNegativesLiveDB: the CHECKs and composite foreign keys reject every
// cross-scope plant, partial tuple and shared-Codex shape. Each case violates the named constraints.
func TestSharedScopeConstraintNegativesLiveDB(t *testing.T) {
	ctx, _, pool := sharedDB(t, "shared_scope_neg_", 0)
	q := store.New(pool)
	a, _ := newProductUser(ctx, t, pool)
	b, _ := newProductUser(ctx, t, pool)
	pa := newProvider(ctx, t, q, a, "a-private")
	cx, err := q.InsertModelProvider(ctx, store.InsertModelProviderParams{OwnerUserID: a, Name: "a-codex", Engine: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	shared := newSharedProvider(ctx, t, q, "shared-gw")

	insProv := `INSERT INTO model_providers (owner_user_id, name, engine, current_revision, credential_mode, scope_owner_id) VALUES ($1, $2, $3, 1, $4, $5)`
	owner := pgtype.UUID{Bytes: a, Valid: true}
	// model_providers shapes
	_, err = pool.Exec(ctx, insProv, owner, "nil-scope", "claude", nil, nilScope)
	wantViolation(t, "private provider with the nil scope", err, "23514", "model_providers_scope_owner_check")
	_, err = pool.Exec(ctx, insProv, pgtype.UUID{}, "user-scope", "claude", "per_user_key", a)
	wantViolation(t, "shared provider with a user scope", err, "23514", "model_providers_scope_owner_check")
	_, err = pool.Exec(ctx, insProv, pgtype.UUID{}, "no-mode", "claude", nil, nilScope)
	wantViolation(t, "NULL owner without a credential mode", err, "23514", "model_providers_shared_mode_check")
	_, err = pool.Exec(ctx, insProv, owner, "owned-mode", "claude", "per_user_key", a)
	wantViolation(t, "owned provider with a credential mode", err, "23514", "model_providers_shared_mode_check")
	_, err = pool.Exec(ctx, insProv, pgtype.UUID{}, "bad-mode", "claude", "operator_key", nilScope)
	wantViolation(t, "unknown credential mode", err, "23514", "model_providers_credential_mode_check")
	_, err = pool.Exec(ctx, insProv, pgtype.UUID{}, "shared-codex", "codex", "per_user_key", nilScope)
	wantViolation(t, "NULL-owner Codex insert", err, "23514", "model_providers_shared_engine_check")
	_, err = pool.Exec(ctx, insProv, pgtype.UUID{Bytes: uuid.Nil, Valid: true}, "nil-owner", "claude", nil, nilScope)
	wantViolation(t, "owner carrying the nil UUID", err, "23514", "model_providers_owner_not_nil_check")
	_, err = pool.Exec(ctx, insProv, owner, "no-scope", "claude", nil, nil)
	wantViolation(t, "provider without a scope", err, "23502", "")
	_, err = pool.Exec(ctx, `UPDATE model_providers SET owner_user_id = NULL WHERE id = $1`, pa.ID)
	wantViolation(t, "moving a private provider into the shared catalogue", err, "23514", "model_providers_scope_owner_check", "model_providers_shared_mode_check")

	// user_secrets: B plants a key on A's private Claude provider, every scope B can name
	insKey := `INSERT INTO user_secrets (user_id, kind, label, ciphertext, sealed_with, provider_id, provider_scope_id) VALUES ($1, $2, $3, 'ct', 'master', $4, $5)`
	_, err = pool.Exec(ctx, insKey, b, "anthropic_compatible_key", "p1", pa.ID, a)
	wantViolation(t, "B's key carrying A's scope", err, "23514", "user_secrets_provider_scope_check")
	_, err = pool.Exec(ctx, insKey, b, "anthropic_compatible_key", "p2", pa.ID, b)
	wantViolation(t, "B's key on A's provider with B's scope", err, "23503", "user_secrets_provider_fkey")
	_, err = pool.Exec(ctx, insKey, b, "anthropic_compatible_key", "p3", pa.ID, nilScope)
	wantViolation(t, "B's Claude key on A's private provider with the shared scope", err, "23503", "user_secrets_provider_fkey")
	for i, c := range []struct{ scope, code, name string }{
		{a.String(), "23514", "user_secrets_provider_scope_check"}, // A's scope on B's key
		{b.String(), "23503", "user_secrets_provider_fkey"},        // B's own scope: no such (scope, provider) pair
		{nilScope, "23514", "user_secrets_provider_scope_check"},   // the shared scope is for Claude keys only
	} {
		_, err = pool.Exec(ctx, insKey, b, "openai_compatible_key", fmt.Sprintf("c%d", i), cx.ID, c.scope)
		wantViolation(t, "B's Codex key planted on A's Codex provider with scope "+c.scope, err, c.code, c.name)
	}
	_, err = pool.Exec(ctx, insKey, b, "openai_compatible_key", "c-shared", shared.ID, nilScope)
	wantViolation(t, "a Codex key with the shared scope", err, "23514", "user_secrets_provider_scope_check")
	_, err = pool.Exec(ctx, insKey, b, "openai_compatible_key", "c-shared-b", shared.ID, b)
	wantViolation(t, "a Codex key on a shared provider with B's own scope", err, "23503", "user_secrets_provider_fkey")
	_, err = pool.Exec(ctx, `INSERT INTO user_secrets (user_id, kind, label, ciphertext, sealed_with, provider_id) VALUES ($1, 'anthropic_compatible_key', 'np', 'ct', 'master', $2)`, b, shared.ID)
	wantViolation(t, "provider key with a provider and no scope", err, "23514", "user_secrets_provider_scope_check")
	_, err = pool.Exec(ctx, `INSERT INTO user_secrets (user_id, kind, label, ciphertext, sealed_with, provider_scope_id) VALUES ($1, 'anthropic_token', 'ns', 'ct', 'master', $2)`, b, b)
	wantViolation(t, "a scope with no provider", err, "23514", "user_secrets_provider_scope_check")

	// users: B's sources
	setSrc := `UPDATE users SET claude_provider_id = $2, claude_provider_scope_id = $3 WHERE id = $1`
	_, err = pool.Exec(ctx, setSrc, b, pa.ID, a)
	wantViolation(t, "B's source carrying A's scope", err, "23514", "users_claude_provider_scope_check")
	_, err = pool.Exec(ctx, setSrc, b, pa.ID, b)
	wantViolation(t, "B's source on A's provider with B's scope", err, "23503", "users_claude_provider_fkey")
	_, err = pool.Exec(ctx, setSrc, b, pa.ID, nilScope)
	wantViolation(t, "B's source on A's provider with the shared scope", err, "23503", "users_claude_provider_fkey")
	_, err = pool.Exec(ctx, `UPDATE users SET claude_provider_id = $2 WHERE id = $1`, b, shared.ID)
	wantViolation(t, "a source with no scope", err, "23514", "users_claude_provider_scope_check")
	_, err = pool.Exec(ctx, `UPDATE users SET claude_provider_scope_id = $2 WHERE id = $1`, b, b)
	wantViolation(t, "a scope with no source", err, "23514", "users_claude_provider_scope_check")
	_, err = pool.Exec(ctx, `UPDATE users SET codex_provider_id = $2 WHERE id = $1`, b, shared.ID)
	wantViolation(t, "a Codex source on a shared provider", err, "23503", "users_codex_provider_fkey")
	_, err = pool.Exec(ctx, `UPDATE users SET codex_provider_id = $2 WHERE id = $1`, b, cx.ID)
	wantViolation(t, "B's Codex source on A's Codex provider", err, "23503", "users_codex_provider_fkey")

	// The legitimate shapes still work.
	if _, err := pool.Exec(ctx, insKey, b, "anthropic_compatible_key", "ok", shared.ID, nilScope); err != nil {
		t.Errorf("B's own key on the shared provider: %v", err)
	}
	if _, err := pool.Exec(ctx, setSrc, b, shared.ID, nilScope); err != nil {
		t.Errorf("B's source on the shared provider: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM model_providers WHERE id = $1`, shared.ID); err == nil {
		t.Errorf("deleting a selected shared provider succeeded; users_claude_provider_fkey is ON DELETE RESTRICT")
	} else {
		wantViolation(t, "deleting a selected shared provider", err, "23503", "users_claude_provider_fkey")
	}
}

// TestSharedScopeRecipientQueriesLiveDB: through the generated queries, a recipient sees and uses a
// shared definition with their own key and nothing of another user's, and never reaches another
// user's private definition.
func TestSharedScopeRecipientQueriesLiveDB(t *testing.T) {
	ctx, _, pool := sharedDB(t, "shared_scope_recipient_", 0)
	q := store.New(pool)
	a, _ := newProductUser(ctx, t, pool)
	b, _ := newProductUser(ctx, t, pool)
	c, _ := newProductUser(ctx, t, pool)
	pa := newProvider(ctx, t, q, a, "a-private")
	shared := newSharedProvider(ctx, t, q, "Shared-GW")

	pb := newProvider(ctx, t, q, b, "b-private")
	// Read the actual result set so an extra owner-only WHERE clause cannot
	// make the shared row disappear while the VIS text pin still passes.
	list, err := q.ListVisibleProviders(ctx, b)
	if err != nil || len(list) != 2 {
		t.Fatalf("B's visible list = %+v (%v), want B's own and the shared definition", list, err)
	}
	byID := make(map[uuid.UUID]store.ListVisibleProvidersRow, len(list))
	for _, row := range list {
		byID[row.ID] = row
	}
	if own, ok := byID[pb.ID]; !ok || own.OwnerUserID != pgUUID(b) || own.CredentialMode.Valid {
		t.Errorf("B's private definition = %+v, present=%v", own, ok)
	}
	if row, ok := byID[shared.ID]; !ok || row.OwnerUserID.Valid || row.CredentialMode.String != "per_user_key" || row.KeySecretID.Valid {
		t.Errorf("B's shared definition = %+v, present=%v, want shared and no key", row, ok)
	}
	if _, ok := byID[pa.ID]; ok {
		t.Error("B can see A's private definition")
	}
	if _, err := q.GetVisibleProviderForUse(ctx, store.GetVisibleProviderForUseParams{ID: pa.ID, UserID: b}); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("B reading A's private definition = %v, want no rows", err)
	}
	if _, err := q.LockModelProviderForShare(ctx, store.LockModelProviderForShareParams{ID: pa.ID, OwnerUserID: b}); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("B freezing A's private definition = %v, want no rows", err)
	}
	if _, err := q.GetModelProviderRevisionForOwner(ctx, store.GetModelProviderRevisionForOwnerParams{ProviderID: pa.ID, Revision: 1, OwnerUserID: b}); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("B reading A's private revision = %v, want no rows", err)
	}
	if _, err := insertProviderKey(ctx, q, b, pa.ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("B's key on A's private definition = %v, want no rows", err)
	}
	if _, err := q.SetUserProviderSource(ctx, store.SetUserProviderSourceParams{ID: b, Engine: "claude", ProviderID: pgUUID(pa.ID)}); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("B selecting A's private definition = %v, want no rows", err)
	}
	if _, err := q.LockVisibleProviderForKeyMutation(ctx, store.LockVisibleProviderForKeyMutationParams{ID: pa.ID, UserID: b}); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("B key-mutation lock on A's private definition = %v, want no rows", err)
	}
	if _, err := q.LockVisibleProviderForEvidence(ctx, store.LockVisibleProviderForEvidenceParams{ID: pa.ID, UserID: b, Revision: 1}); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("B evidence lock on A's private definition = %v, want no rows", err)
	}

	// B attaches their own key and selects the shared definition: scope is written from the definition.
	kb, err := insertProviderKey(ctx, q, b, shared.ID)
	if err != nil {
		t.Fatalf("B's key on the shared definition: %v", err)
	}
	if kb.Kind != store.KindAnthropicCompatibleKey || kb.IsDefault || kb.AutoEligible {
		t.Errorf("shared key flags = %+v", kb)
	}
	if got := scalar[string](ctx, t, pool, `SELECT user_id::text || ' ' || provider_scope_id::text FROM user_secrets WHERE id = $1`, kb.ID); got != b.String()+" "+nilScope {
		t.Errorf("stored key owner/scope = %q, want B and the shared scope", got)
	}
	// C has no key: B's key never resolves for C.
	cv, err := q.GetVisibleProviderForUse(ctx, store.GetVisibleProviderForUseParams{ID: shared.ID, UserID: c})
	if err != nil || cv.KeySecretID.Valid {
		t.Errorf("C's view of the shared definition = %+v (%v), want no key", cv, err)
	}
	if _, err := q.GetProviderKeyForUpdate(ctx, store.GetProviderKeyForUpdateParams{UserID: c, ProviderID: pgUUID(shared.ID)}); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("C locking B's key = %v, want no rows", err)
	}
	row, err := q.SetUserProviderSource(ctx, store.SetUserProviderSourceParams{ID: b, Engine: "claude", ProviderID: pgUUID(shared.ID)})
	if err != nil || !row.ClaudeProviderID.Valid || uuid.UUID(row.ClaudeProviderID.Bytes) != shared.ID {
		t.Fatalf("B selecting the shared definition = %+v (%v)", row, err)
	}
	if got := scalar[string](ctx, t, pool, `SELECT claude_provider_scope_id::text FROM users WHERE id = $1`, b); got != nilScope {
		t.Errorf("claude_provider_scope_id after selecting a shared definition = %s, want the nil scope", got)
	}
	src, err := q.GetUserProviderSource(ctx, store.GetUserProviderSourceParams{ID: b, Engine: "claude"})
	if err != nil || !src.KeySecretID.Valid || uuid.UUID(src.KeySecretID.Bytes) != kb.ID {
		t.Errorf("B's resolved source = %+v (%v), want B's own key", src, err)
	}
	if src, err := q.GetUserProviderSource(ctx, store.GetUserProviderSourceParams{ID: b, Engine: "codex"}); err != nil || src.ProviderID.Valid {
		t.Errorf("B's Codex source = %+v (%v), want none", src, err)
	}
	// A Codex source never accepts a shared definition, and selecting Codex leaves the Claude scope alone.
	if _, err := q.SetUserProviderSource(ctx, store.SetUserProviderSourceParams{ID: b, Engine: "codex", ProviderID: pgUUID(shared.ID)}); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("B's Codex source on a shared definition = %v, want no rows", err)
	}
	bc, err := q.InsertModelProvider(ctx, store.InsertModelProviderParams{OwnerUserID: b, Name: "b-codex", Engine: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.SetUserProviderSource(ctx, store.SetUserProviderSourceParams{ID: b, Engine: "codex", ProviderID: pgUUID(bc.ID)}); err != nil {
		t.Fatalf("B's own Codex source: %v", err)
	}
	if got := scalar[string](ctx, t, pool, `SELECT claude_provider_scope_id::text FROM users WHERE id = $1`, b); got != nilScope {
		t.Errorf("selecting a Codex source changed the Claude scope to %s", got)
	}
	// With no runs there are no run names to resolve (the shared-binding and inaccessible-binding cases
	// are covered by TestRunProviderNamesForSharedAndInaccessibleBindingsLiveDB in internal/workersvc).
	names, err := q.ListRunProviderNames(ctx, nil)
	if err != nil || len(names) != 0 {
		t.Errorf("ListRunProviderNames(no runs) = %v (%v)", names, err)
	}
	// Clearing writes a NULL scope in the same statement.
	if _, err := q.SetUserProviderSource(ctx, store.SetUserProviderSourceParams{ID: b, Engine: "claude"}); err != nil {
		t.Fatalf("clearing B's Claude source: %v", err)
	}
	if got := scalar[bool](ctx, t, pool, `SELECT claude_provider_id IS NULL AND claude_provider_scope_id IS NULL FROM users WHERE id = $1`, b); !got {
		t.Errorf("clearing the Claude source left a scope behind")
	}
	// Owner-only queries never see the shared definition.
	if _, err := q.GetModelProviderForOwner(ctx, store.GetModelProviderForOwnerParams{ID: shared.ID, OwnerUserID: b}); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("owner-only read of a shared definition = %v, want no rows", err)
	}
	if n, err := q.RenameModelProvider(ctx, store.RenameModelProviderParams{ID: shared.ID, OwnerUserID: a, Name: "x"}); err != nil || n != 0 {
		t.Errorf("owner rename of a shared definition = %d (%v), want 0 rows", n, err)
	}
	// Admin reads: counts only.
	adm, err := q.GetSharedModelProviderAdmin(ctx, shared.ID)
	if err != nil || adm.AttachedKeyCount != 1 || adm.SelectedUserCount != 0 {
		t.Errorf("admin get = %+v (%v), want one attached key and no selection", adm, err)
	}
	if _, err := q.GetSharedModelProviderAdmin(ctx, pa.ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("admin get of a private definition = %v, want no rows", err)
	}
	all, err := q.ListSharedModelProvidersAdmin(ctx)
	if err != nil || len(all) != 1 || all[0].ID != shared.ID {
		t.Errorf("admin list = %+v (%v), want only the shared definition", all, err)
	}
}

// schemaFingerprint lists the constraints, indexes and columns of the three tables the migrations
// change plus the model_provider tables, as comparable text.
func schemaFingerprint(ctx context.Context, t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	var out []string
	for _, sql := range []string{
		`SELECT 'constraint ' || conrelid::regclass::text || '.' || conname || ' valid=' || convalidated || ' ' || pg_get_constraintdef(oid)
           FROM pg_constraint WHERE conrelid IN ('model_providers'::regclass, 'user_secrets'::regclass, 'users'::regclass) ORDER BY 1`,
		`SELECT 'index ' || tablename || '.' || indexname || ' ' || indexdef FROM pg_indexes
           WHERE tablename IN ('model_providers', 'user_secrets', 'users') ORDER BY 1`,
		`SELECT 'column ' || table_name || '.' || column_name || ' ' || is_nullable || ' ' || data_type FROM information_schema.columns
           WHERE table_name IN ('model_providers', 'user_secrets', 'users', 'model_provider_revisions', 'model_provider_probe_results') ORDER BY 1`,
		`SELECT 'table ' || tablename FROM pg_tables WHERE tablename LIKE 'model_provider%' ORDER BY 1`,
	} {
		rows, err := pool.Query(ctx, sql)
		if err != nil {
			t.Fatalf("fingerprint %q: %v", sql, err)
		}
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			out = append(out, s)
		}
		rows.Close()
	}
	return out
}

func diffFingerprints(want, got []string) string {
	w, g := map[string]bool{}, map[string]bool{}
	for _, s := range want {
		w[s] = true
	}
	for _, s := range got {
		g[s] = true
	}
	var d []string
	for _, s := range want {
		if !g[s] {
			d = append(d, "missing: "+s)
		}
	}
	for _, s := range got {
		if !w[s] {
			d = append(d, "extra: "+s)
		}
	}
	if len(d) > 5 {
		d = append(d[:5], fmt.Sprintf("... %d differences in all", len(d)))
	}
	return strings.Join(d, "\n")
}

// TestSharedScopeDownGuardsAndFullDownLiveDB: each Down refuses while a live shared reference exists
// (the selection, the shared key, the shared definition), and once they are gone the full Down restores
// the 00477 and 00479 foreign keys and leaves no orphan index, column or table. A re-upgrade then works.
func TestSharedScopeDownGuardsAndFullDownLiveDB(t *testing.T) {
	refCtx, refDSN := standIsolatedDB(t, "shared_scope_ref_")
	migrateTo(refCtx, t, refDSN, 523)
	want := schemaFingerprint(refCtx, t, openSharedPool(refCtx, t, refDSN))

	ctx, dsn, pool := sharedDB(t, "shared_scope_down_", 0)
	q := store.New(pool)
	b, _ := newProductUser(ctx, t, pool)
	shared := newSharedProvider(ctx, t, q, "down-gw")
	kb, err := insertProviderKey(ctx, q, b, shared.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.SetUserProviderSource(ctx, store.SetUserProviderSourceParams{ID: b, Engine: "claude", ProviderID: pgUUID(shared.ID)}); err != nil {
		t.Fatal(err)
	}
	mustExec(ctx, t, pool, `INSERT INTO model_provider_key_test_results (provider_id, user_id, revision, key_secret_id, key_enablement_rev, auth_result) VALUES ($1, $2, 1, $3, 0, 'ok')`, shared.ID, b, kb.ID)

	refuses := func(to int64, snippet string) {
		t.Helper()
		err := store.MigrateDownTo(ctx, dsn, to)
		if err == nil || !strings.Contains(err.Error(), snippet) {
			t.Fatalf("Down to %d = %v, want a refusal containing %q", to, err, snippet)
		}
	}
	// 00531-00529 roll back freely; 00528 refuses while B's source is the shared definition.
	refuses(527, "refusing to roll back 00528")
	if v := appliedVersion(ctx, t, pool); v != 528 {
		t.Fatalf("version after the refused 00528 Down = %d, want 528", v)
	}
	if _, err := q.SetUserProviderSource(ctx, store.SetUserProviderSourceParams{ID: b, Engine: "claude"}); err != nil {
		t.Fatal(err)
	}
	refuses(526, "refusing to roll back 00527")
	if _, err := q.DeleteSharedProviderKeys(ctx, shared.ID); err != nil {
		t.Fatal(err)
	}
	refuses(523, "refusing to roll back 00524")
	if v := appliedVersion(ctx, t, pool); v != 524 {
		t.Fatalf("version after the refused 00524 Down = %d, want 524", v)
	}
	if n, err := q.DeleteSharedModelProvider(ctx, shared.ID); err != nil || n != 1 {
		t.Fatalf("delete shared provider = %d (%v)", n, err)
	}
	if err := store.MigrateDownTo(ctx, dsn, 523); err != nil {
		t.Fatalf("full Down: %v", err)
	}
	if got := schemaFingerprint(ctx, t, pool); diffFingerprints(want, got) != "" {
		t.Errorf("schema after the full Down differs from a 00523 database:\n%s", diffFingerprints(want, got))
	}
	for _, f := range []string{"model_providers_scope_owner_id_key", "model_providers_shared_name_lower_key", "users_claude_provider_id_idx"} {
		if scalar[bool](ctx, t, pool, `SELECT to_regclass($1) IS NOT NULL`, f) {
			t.Errorf("orphan index %s survived the full Down", f)
		}
	}
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("re-upgrade after the full Down: %v", err)
	}
}

// TestSharedScopeInvalidIndexRefusesThenRecoversLiveDB: the scope key index is the parent of the 00527
// and 00528 foreign keys. An invalid one (an interrupted concurrent build) makes 00527 refuse with no
// partial change, and re-running the index migration rebuilds it VALID so the rest completes.
func TestSharedScopeInvalidIndexRefusesThenRecoversLiveDB(t *testing.T) {
	invalidate := func(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
		t.Helper()
		// A deterministic stand-in for a failed CREATE INDEX CONCURRENTLY, in a throwaway database only.
		mustExec(ctx, t, pool, `UPDATE pg_index SET indisvalid = false WHERE indexrelid = 'model_providers_scope_owner_id_key'::regclass`)
		if scalar[bool](ctx, t, pool, `SELECT indisvalid FROM pg_index WHERE indexrelid = 'model_providers_scope_owner_id_key'::regclass`) {
			t.Fatal("the index is still valid after the injection")
		}
	}
	t.Run("00527 refuses an invalid parent index", func(t *testing.T) {
		ctx, dsn, pool := sharedDB(t, "shared_scope_inv_a_", 526)
		invalidate(ctx, t, pool)
		err := store.Migrate(ctx, dsn)
		if err == nil || !strings.Contains(err.Error(), "refusing to apply 00527") {
			t.Fatalf("Migrate over an invalid index = %v, want the 00527 refusal", err)
		}
		if v := appliedVersion(ctx, t, pool); v != 526 {
			t.Errorf("version after the refusal = %d, want 526", v)
		}
		if scalar[bool](ctx, t, pool, `SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'user_secrets' AND column_name = 'provider_scope_id')`) {
			t.Errorf("00527 left provider_scope_id behind after refusing")
		}
		if scalar[string](ctx, t, pool, `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = 'user_secrets_provider_fkey'`) != "FOREIGN KEY (user_id, provider_id) REFERENCES model_providers(owner_user_id, id) ON DELETE CASCADE" {
			t.Errorf("00527 left the provider foreign key changed after refusing")
		}
		// Recovery: roll the index migration back and apply it again, which drops and rebuilds the index.
		if err := store.MigrateDownTo(ctx, dsn, 525); err != nil {
			t.Fatal(err)
		}
		if err := store.Migrate(ctx, dsn); err != nil {
			t.Fatalf("Migrate after rolling 00526 back: %v", err)
		}
		if !scalar[bool](ctx, t, pool, `SELECT indisvalid AND indisunique FROM pg_index WHERE indexrelid = 'model_providers_scope_owner_id_key'::regclass`) {
			t.Errorf("the index is not valid after recovery")
		}
		if v := appliedVersion(ctx, t, pool); v < 531 {
			t.Errorf("version after recovery = %d, want at least 531", v)
		}
	})
	t.Run("00526 replaces an invalid index it finds", func(t *testing.T) {
		ctx, dsn, pool := sharedDB(t, "shared_scope_inv_b_", 525)
		mustExec(ctx, t, pool, `CREATE UNIQUE INDEX model_providers_scope_owner_id_key ON model_providers (scope_owner_id, id)`)
		invalidate(ctx, t, pool)
		if err := store.Migrate(ctx, dsn); err != nil {
			t.Fatalf("Migrate over a leftover invalid index: %v", err)
		}
		if !scalar[bool](ctx, t, pool, `SELECT indisvalid AND indisunique FROM pg_index WHERE indexrelid = 'model_providers_scope_owner_id_key'::regclass`) {
			t.Errorf("the index was not rebuilt valid")
		}
	})
}

// TestSharedScopeNilUserRollsBack00524LiveDB: the nil UUID is the shared scope, so a user row carrying it
// makes 00524 refuse and roll back completely, with its version unrecorded; removing the row lets it apply.
func TestSharedScopeNilUserRollsBack00524LiveDB(t *testing.T) {
	ctx, dsn, pool := sharedDB(t, "shared_scope_nil_", 523)
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, 'nil@example.com', 'x')`, nilScope)
	err := store.Migrate(ctx, dsn)
	if err == nil || !strings.Contains(err.Error(), "refusing to apply 00524") {
		t.Fatalf("Migrate with a nil-UUID user = %v, want the 00524 refusal", err)
	}
	if v := appliedVersion(ctx, t, pool); v != 523 {
		t.Errorf("version after the refusal = %d, want 523 (00524 unrecorded)", v)
	}
	if got := scalar[string](ctx, t, pool, `SELECT is_nullable FROM information_schema.columns WHERE table_name = 'model_providers' AND column_name = 'owner_user_id'`); got != "NO" {
		t.Errorf("owner_user_id is nullable after a refused 00524; the migration did not roll back")
	}
	if scalar[bool](ctx, t, pool, `SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'model_providers' AND column_name IN ('credential_mode', 'scope_owner_id'))`) {
		t.Errorf("a refused 00524 left its columns behind")
	}
	mustExec(ctx, t, pool, `DELETE FROM users WHERE id = $1`, nilScope)
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("Migrate after removing the nil-UUID user: %v", err)
	}
	if v := appliedVersion(ctx, t, pool); v < 531 {
		t.Errorf("version after the retry = %d, want at least 531", v)
	}
}

// The statements the PREVIOUS API image (base 15d9a49cd, before this change) runs, copied verbatim from
// its generated queries. An image-only rollback keeps the new schema, so these are what an older API
// executes against it.
const (
	baseInsertModelProvider = `INSERT INTO model_providers (owner_user_id, name, engine, current_revision)
VALUES ($1, $2, $3, 1)
RETURNING id, owner_user_id, name, engine, current_revision, retired_at, created_at`
	baseInsertProviderKeySecret = `INSERT INTO user_secrets (user_id, kind, label, is_default, auto_eligible, ciphertext, sealed_with, provider_id)
SELECT $1, CASE p.engine WHEN 'claude' THEN 'anthropic_compatible_key' WHEN 'codex' THEN 'openai_compatible_key' END,
       $2, false, false, $3, $4, $5
FROM model_providers p
WHERE p.id = $5 AND p.owner_user_id = $1
RETURNING id, kind, label, is_default, auto_eligible, created_at, updated_at`
	baseSetUserProviderSource = `UPDATE users u
SET claude_provider_id = CASE WHEN $1::text = 'claude' THEN $2::uuid ELSE claude_provider_id END,
    codex_provider_id = CASE WHEN $1::text = 'codex' THEN $2::uuid ELSE codex_provider_id END
WHERE u.id = $3 AND $1::text IN ('claude', 'codex')
  AND ($2::uuid IS NULL OR EXISTS (
      SELECT 1 FROM model_providers p WHERE p.id = $2::uuid
        AND p.owner_user_id = u.id AND p.engine = $1::text))
RETURNING claude_provider_id, codex_provider_id`
	baseUpsertModelProviderProbeResult = `INSERT INTO model_provider_probe_results (provider_id, revision, auth_result, tool_result, probed_at)
SELECT p.id, p.current_revision, $1, $2, now()
FROM model_providers p
WHERE p.id = $3 AND p.owner_user_id = $4 AND p.current_revision = $5
ON CONFLICT (provider_id, revision) DO UPDATE
SET auth_result = EXCLUDED.auth_result, tool_result = EXCLUDED.tool_result, probed_at = EXCLUDED.probed_at`
)

// TestSharedScopeImageOnlyRollbackLiveDB: with the new schema and the previous API image, the writes that
// cannot fill the scope columns fail (provider insert, key insert, Claude source set and clear), while the
// stop levers keep working: Codex set and clear, key disable and delete, retire, and the old probe upsert
// against the retained legacy table.
func TestSharedScopeImageOnlyRollbackLiveDB(t *testing.T) {
	ctx, _, pool := sharedDB(t, "shared_scope_image_", 0)
	q := store.New(pool)
	a, _ := newProductUser(ctx, t, pool)
	pc := newProvider(ctx, t, q, a, "claude-gw")
	pk := newProvider(ctx, t, q, a, "second-gw")
	cx, err := q.InsertModelProvider(ctx, store.InsertModelProviderParams{OwnerUserID: a, Name: "codex-gw", Engine: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.InsertModelProviderRevision(ctx, store.InsertModelProviderRevisionParams{ProviderID: cx.ID, Revision: 1, Protocol: "openai_responses", BaseUrl: "https://resp.example.com/v1", AuthStyle: "bearer", ModelMap: []byte(`{"default":{"model":"m"}}`)}); err != nil {
		t.Fatal(err)
	}
	key, err := insertProviderKey(ctx, q, a, pc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.SetUserProviderSource(ctx, store.SetUserProviderSourceParams{ID: a, Engine: "claude", ProviderID: pgUUID(pc.ID)}); err != nil {
		t.Fatal(err)
	}

	_, err = pool.Exec(ctx, baseInsertModelProvider, a, "old-image", "claude")
	wantViolation(t, "the old image creating a provider", err, "23502", "")
	_, err = pool.Exec(ctx, baseInsertProviderKeySecret, a, "lbl", []byte("ct"), "master", pk.ID)
	wantViolation(t, "the old image attaching a key", err, "23514", "user_secrets_provider_scope_check")
	// A user with no selection has no scope to carry the new source, so the old set is refused.
	fresh, _ := newProductUser(ctx, t, pool)
	fp := newProvider(ctx, t, q, fresh, "fresh-gw")
	_, err = pool.Exec(ctx, baseSetUserProviderSource, "claude", pgUUID(fp.ID), fresh)
	wantViolation(t, "the old image selecting a Claude source", err, "23514", "users_claude_provider_scope_check")
	_, err = pool.Exec(ctx, baseSetUserProviderSource, "claude", pgtype.UUID{}, a)
	wantViolation(t, "the old image clearing a selected Claude source", err, "23514", "users_claude_provider_scope_check")

	// With a personal Claude source already selected, the old image switching to another of the same
	// user's personal providers keeps the scope and succeeds; switching away from a SHARED selection
	// would leave the shared scope behind and is refused.
	if tag, err := pool.Exec(ctx, baseSetUserProviderSource, "claude", pgUUID(pk.ID), a); err != nil || tag.RowsAffected() != 1 {
		t.Errorf("the old image switching between personal Claude providers: rows=%d err=%v, want one row", tag.RowsAffected(), err)
	}
	sh, err := q.InsertSharedModelProvider(ctx, "rollback-shared")
	if err != nil {
		t.Fatal(err)
	}
	sel, _ := newProductUser(ctx, t, pool)
	sp := newProvider(ctx, t, q, sel, "sel-gw")
	if _, err := q.SetUserProviderSource(ctx, store.SetUserProviderSourceParams{ID: sel, Engine: "claude", ProviderID: pgUUID(sh.ID)}); err != nil {
		t.Fatalf("selecting the shared definition: %v", err)
	}
	_, err = pool.Exec(ctx, baseSetUserProviderSource, "claude", pgUUID(sp.ID), sel)
	wantViolation(t, "the old image switching away from a shared selection", err, "23503", "users_claude_provider_fkey")

	checkSources := func(wantCodex pgtype.UUID) {
		t.Helper()
		var codex, claude, scope pgtype.UUID
		if err := pool.QueryRow(ctx, `SELECT codex_provider_id, claude_provider_id, claude_provider_scope_id FROM users WHERE id = $1`, a).Scan(&codex, &claude, &scope); err != nil {
			t.Fatal(err)
		}
		if codex != wantCodex || claude != pgUUID(pk.ID) || scope != pgUUID(a) {
			t.Errorf("after old-image Codex write: codex=%v claude=%v scope=%v; want codex=%v, claude=%s, scope=%s",
				codex, claude, scope, wantCodex, pk.ID, a)
		}
	}
	if _, err := pool.Exec(ctx, baseSetUserProviderSource, "codex", pgUUID(cx.ID), a); err != nil {
		t.Errorf("the old image selecting a Codex source: %v", err)
	}
	checkSources(pgUUID(cx.ID))
	if _, err := pool.Exec(ctx, baseSetUserProviderSource, "codex", pgtype.UUID{}, a); err != nil {
		t.Errorf("the old image clearing a Codex source: %v", err)
	}
	checkSources(pgtype.UUID{})
	// Key disable, then delete, then retire: the application's own statements for those.
	if _, err := pool.Exec(ctx, `UPDATE user_secrets SET disabled_at = now(), enablement_rev = enablement_rev + 1 WHERE id = $1`, key.ID); err != nil {
		t.Errorf("disabling the key: %v", err)
	}
	if n, err := q.RetireModelProvider(ctx, store.RetireModelProviderParams{ID: pc.ID, OwnerUserID: a}); err != nil || n != 1 {
		t.Errorf("retiring the provider = %d (%v)", n, err)
	}
	if n, err := q.DeleteProviderKeySecret(ctx, store.DeleteProviderKeySecretParams{UserID: a, ProviderID: pgUUID(pc.ID)}); err != nil || n != 1 {
		t.Errorf("deleting the key = %d (%v)", n, err)
	}
	tag, err := pool.Exec(ctx, baseUpsertModelProviderProbeResult, "ok", pgtype.Text{String: "ok", Valid: true}, pk.ID, a, int32(1))
	if err != nil || tag.RowsAffected() != 1 {
		t.Errorf("the old image's probe upsert = %v (%v), want one row in the legacy table", tag, err)
	}
	if got := scalar[int](ctx, t, pool, `SELECT count(*) FROM model_provider_probe_results WHERE provider_id = $1`, pk.ID); got != 1 {
		t.Errorf("legacy probe rows = %d, want 1", got)
	}
}

// TestSharedCatalogueRacesLiveDB: two concurrent shared creates race on the case-insensitive name (one
// loses with a unique violation) and on the cap (the catalogue lock makes the count-then-insert exact,
// and retired definitions keep their slot).
func TestSharedCatalogueRacesLiveDB(t *testing.T) {
	ctx, _, pool := sharedDB(t, "shared_scope_races_", 0)
	create := func(name string, cap int64) (created bool, err error) {
		tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		if err != nil {
			return false, err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		q := store.New(tx)
		if err := q.LockSharedModelProviderCatalogue(ctx); err != nil {
			return false, err
		}
		n, err := q.CountSharedModelProviders(ctx)
		if err != nil {
			return false, err
		}
		if n >= cap {
			return false, nil
		}
		p, err := q.InsertSharedModelProvider(ctx, name)
		if err != nil {
			return false, err
		}
		if err := q.InsertModelProviderRevision(ctx, store.InsertModelProviderRevisionParams{ProviderID: p.ID, Revision: 1, Protocol: "anthropic_messages", BaseUrl: "https://gw.example.com/a", AuthStyle: "bearer", ModelMap: []byte(`{"haiku":{"model":"h"}}`)}); err != nil {
			return false, err
		}
		return true, tx.Commit(ctx)
	}

	var wg sync.WaitGroup
	results := make([]error, 2)
	won := make([]bool, 2)
	for i, name := range []string{"Corp-GW", "corp-gw"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			won[i], results[i] = create(name, 100)
		}()
	}
	wg.Wait()
	winners, losers := 0, 0
	for i := range results {
		switch {
		case results[i] == nil && won[i]:
			winners++
		default:
			code, c := pgCodeC(results[i])
			if code != "23505" || c != "model_providers_shared_name_lower_key" {
				t.Errorf("loser %d: %v (%s %s), want the shared name unique violation", i, results[i], code, c)
			}
			losers++
		}
	}
	if winners != 1 || losers != 1 {
		t.Fatalf("name race: %d winners, %d losers, want 1 and 1", winners, losers)
	}

	// Cap race: 12 creators, cap 4, one slot already used by the name race.
	const cap = 4
	var created int32
	var mu sync.Mutex
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := create(fmt.Sprintf("cap-%d", i), cap)
			if err != nil {
				t.Errorf("cap creator %d: %v", i, err)
				return
			}
			if ok {
				mu.Lock()
				created++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if total := scalar[int](ctx, t, pool, `SELECT count(*) FROM model_providers WHERE owner_user_id IS NULL`); total != cap || created != cap-1 {
		t.Fatalf("after the cap race: %d shared definitions, %d created by the race, want %d and %d (no overshoot)", total, created, cap, cap-1)
	}
	// Retiring does not free a slot.
	q := store.New(pool)
	rows, err := pool.Query(ctx, `SELECT id FROM model_providers WHERE owner_user_id IS NULL`)
	if err != nil {
		t.Fatal(err)
	}
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		if n, err := q.RetireSharedModelProvider(ctx, id); err != nil || n != 1 {
			t.Fatalf("retire %s = %d (%v)", id, n, err)
		}
		if n, err := q.RetireSharedModelProvider(ctx, id); err != nil || n != 1 {
			t.Fatalf("retire is idempotent: second call = %d (%v)", n, err)
		}
	}
	if ok, err := create("after-retire", cap); err != nil || ok {
		t.Errorf("create with every slot retired = %t (%v), want refused by the cap", ok, err)
	}
}

func waitBlocked(ctx context.Context, t *testing.T, pool *pgxpool.Pool, pid int32, what string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var wait pgtype.Text
		if err := pool.QueryRow(ctx, `SELECT wait_event_type FROM pg_stat_activity WHERE pid = $1`, pid).Scan(&wait); err != nil {
			t.Fatal(err)
		}
		if wait.String == "Lock" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s never blocked on a lock", what)
}

// TestProviderKeyTestEvidenceFenceLiveDB: the per-key Test evidence is upserted only under the two locks,
// shows only for the exact current tuple, is isolated between users, and a rotation or revision bump,
// committed or still in flight, leaves the stale result unwritable and unreadable.
func TestProviderKeyTestEvidenceFenceLiveDB(t *testing.T) {
	ctx, _, pool := sharedDB(t, "shared_scope_evidence_", 0)
	q := store.New(pool)
	b, _ := newProductUser(ctx, t, pool)
	c, _ := newProductUser(ctx, t, pool)
	shared := newSharedProvider(ctx, t, q, "evidence-gw")
	kb, err := insertProviderKey(ctx, q, b, shared.ID)
	if err != nil {
		t.Fatal(err)
	}
	kc, err := insertProviderKey(ctx, q, c, shared.ID)
	if err != nil {
		t.Fatal(err)
	}

	// record runs the post-probe fence in one transaction and reports whether a result was written.
	record := func(user, key uuid.UUID, revision int32, keyRev int64, auth string) (bool, error) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			return false, err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		tq := store.New(tx)
		if _, err := tq.LockVisibleProviderForEvidence(ctx, store.LockVisibleProviderForEvidenceParams{ID: shared.ID, UserID: user, Revision: revision}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return false, nil
			}
			return false, err
		}
		if _, err := tq.LockProviderKeyForEvidence(ctx, store.LockProviderKeyForEvidenceParams{KeyID: key, UserID: user, ProviderID: pgUUID(shared.ID), KeyRev: keyRev}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return false, nil
			}
			return false, err
		}
		if err := tq.UpsertProviderKeyTestResult(ctx, store.UpsertProviderKeyTestResultParams{ProviderID: shared.ID, UserID: user, Revision: revision, KeySecretID: key, KeyEnablementRev: keyRev, AuthResult: auth}); err != nil {
			return false, err
		}
		return true, tx.Commit(ctx)
	}
	probe := func(user uuid.UUID) string {
		t.Helper()
		v, err := q.GetVisibleProviderForUse(ctx, store.GetVisibleProviderForUseParams{ID: shared.ID, UserID: user})
		if err != nil {
			t.Fatal(err)
		}
		if !v.ProbeAuthResult.Valid {
			return ""
		}
		return v.ProbeAuthResult.String
	}

	// B succeeds, C fails: each sees only their own result.
	if ok, err := record(b, kb.ID, 1, 0, "ok"); err != nil || !ok {
		t.Fatalf("B's evidence = %t (%v), want written", ok, err)
	}
	if ok, err := record(c, kc.ID, 1, 0, "unauthorized"); err != nil || !ok {
		t.Fatalf("C's evidence = %t (%v), want written", ok, err)
	}
	if got := probe(b); got != "ok" {
		t.Errorf("B reads %q, want ok", got)
	}
	if got := probe(c); got != "unauthorized" {
		t.Errorf("C reads %q, want unauthorized", got)
	}
	if list, err := q.ListVisibleProviders(ctx, b); err != nil || len(list) != 1 || list[0].ProbeAuthResult.String != "ok" {
		t.Errorf("B's list evidence = %+v (%v)", list, err)
	}
	// Another user's key cannot be fenced as mine.
	if ok, err := record(c, kb.ID, 1, 0, "forged"); err != nil || ok {
		t.Errorf("C recording evidence against B's key = %t (%v), want refused", ok, err)
	}

	// A rotation that committed first: the old tuple cannot be written and the old row is unreadable.
	mustExec(ctx, t, pool, `UPDATE user_secrets SET enablement_rev = enablement_rev + 1 WHERE id = $1`, kb.ID)
	if got := probe(b); got != "" {
		t.Errorf("after a rotation B still reads %q, want not tested", got)
	}
	if ok, err := record(b, kb.ID, 1, 0, "late-old-key"); err != nil || ok {
		t.Errorf("a delayed old-key result = %t (%v), want refused", ok, err)
	}
	if got := probe(b); got != "" {
		t.Errorf("after the refused late write B reads %q, want not tested", got)
	}
	if ok, err := record(b, kb.ID, 1, 1, "ok-new"); err != nil || !ok {
		t.Fatalf("evidence for the rotated key = %t (%v), want written", ok, err)
	}
	if got := probe(b); got != "ok-new" {
		t.Errorf("B reads %q, want ok-new", got)
	}
	// A disabled key never records evidence.
	mustExec(ctx, t, pool, `UPDATE user_secrets SET disabled_at = now() WHERE id = $1`, kc.ID)
	if ok, err := record(c, kc.ID, 1, 0, "disabled"); err != nil || ok {
		t.Errorf("evidence for a disabled key = %t (%v), want refused", ok, err)
	}
	mustExec(ctx, t, pool, `UPDATE user_secrets SET disabled_at = NULL WHERE id = $1`, kc.ID)

	// Three-session interleaving. S1 holds the key row (a rotation in flight, provider lock taken first as
	// the key routes do); S2 takes the provider lock and blocks on the key; S3's revision bump blocks on
	// S2's provider lock. When S1 commits the rotation S2 finds no row and rolls back, then S3 proceeds.
	tx1, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx1.Rollback(ctx) }()
	q1 := store.New(tx1)
	if _, err := q1.LockVisibleProviderForKeyMutation(ctx, store.LockVisibleProviderForKeyMutationParams{ID: shared.ID, UserID: b}); err != nil {
		t.Fatal(err)
	}
	cur, err := q1.GetProviderKeyForUpdate(ctx, store.GetProviderKeyForUpdateParams{UserID: b, ProviderID: pgUUID(shared.ID)})
	if err != nil {
		t.Fatal(err)
	}

	tx2, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx2.Rollback(ctx) }()
	q2 := store.New(tx2)
	var pid2 int32
	if err := tx2.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid2); err != nil {
		t.Fatal(err)
	}
	if _, err := q2.LockVisibleProviderForEvidence(ctx, store.LockVisibleProviderForEvidenceParams{ID: shared.ID, UserID: b, Revision: 1}); err != nil {
		t.Fatalf("S2 provider lock: %v", err)
	}
	s2 := make(chan error, 1)
	s2Ctx, cancelS2 := context.WithCancel(ctx)
	s2Done := make(chan struct{})
	defer func() { cancelS2(); <-s2Done }()
	go func() {
		defer close(s2Done)
		_, err := q2.LockProviderKeyForEvidence(s2Ctx, store.LockProviderKeyForEvidenceParams{KeyID: kb.ID, UserID: b, ProviderID: pgUUID(shared.ID), KeyRev: cur.EnablementRev})
		s2 <- err
	}()
	waitBlocked(ctx, t, pool, pid2, "S2 (key lock)")

	tx3, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx3.Rollback(ctx) }()
	var pid3 int32
	if err := tx3.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid3); err != nil {
		t.Fatal(err)
	}
	s3 := make(chan error, 1)
	s3Ctx, cancelS3 := context.WithCancel(ctx)
	s3Done := make(chan struct{})
	defer func() { cancelS3(); <-s3Done }()
	var bumped int32
	go func() {
		defer close(s3Done)
		n, err := store.New(tx3).BumpSharedModelProviderRevision(s3Ctx, shared.ID)
		bumped = n
		s3 <- err
	}()
	waitBlocked(ctx, t, pool, pid3, "S3 (revision bump)")

	if _, err := tx1.Exec(ctx, `UPDATE user_secrets SET enablement_rev = enablement_rev + 1 WHERE id = $1`, kb.ID); err != nil {
		t.Fatal(err)
	}
	if err := tx1.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-s2:
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("S2 key lock after the rotation = %v, want no rows", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("S2 never returned after the rotation committed")
	}
	if err := tx2.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-s3:
		if err != nil || bumped != 2 {
			t.Fatalf("S3 bump = %d (%v), want revision 2", bumped, err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("S3 never returned after S2 released the provider")
	}
	addRevision(ctx, t, store.New(tx3), shared.ID, 2, "https://gw.example.com/anthropic")
	if err := tx3.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// A revision bump committed: evidence for revision 1 is unwritable and the new revision reads not tested.
	if ok, err := record(b, kb.ID, 1, cur.EnablementRev+1, "late-old-revision"); err != nil || ok {
		t.Errorf("evidence for a superseded revision = %t (%v), want refused", ok, err)
	}
	if got := probe(b); got != "" {
		t.Errorf("after a revision bump B reads %q, want not tested", got)
	}
	if got := probe(c); got != "" {
		t.Errorf("after a revision bump C reads %q, want not tested", got)
	}
	if ok, err := record(b, kb.ID, 2, cur.EnablementRev+1, "ok-r2"); err != nil || !ok {
		t.Fatalf("evidence for the current revision = %t (%v), want written", ok, err)
	}
	if got := probe(b); got != "ok-r2" {
		t.Errorf("B reads %q, want ok-r2", got)
	}

	// Retired: key mutation still locks the definition, evidence and edits do not.
	if n, err := q.RetireSharedModelProvider(ctx, shared.ID); err != nil || n != 1 {
		t.Fatalf("retire = %d (%v)", n, err)
	}
	if _, err := q.LockVisibleProviderForKeyMutation(ctx, store.LockVisibleProviderForKeyMutationParams{ID: shared.ID, UserID: b}); err != nil {
		t.Errorf("key-mutation lock on a retired shared definition = %v, want the row", err)
	}
	if ok, err := record(b, kb.ID, 2, cur.EnablementRev+1, "retired"); err != nil || ok {
		t.Errorf("evidence on a retired definition = %t (%v), want refused", ok, err)
	}
	if _, err := q.BumpSharedModelProviderRevision(ctx, shared.ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("bumping a retired shared definition = %v, want no rows", err)
	}
}

// TestSharedProviderDeleteCascadesLiveDB: a selected shared definition cannot be deleted; an unselected
// one is deleted after its keys, DeleteSharedProviderKeys returns the exact attached count, and the
// cascade removes the revisions and the per-key evidence while leaving other users' personal data alone.
func TestSharedProviderDeleteCascadesLiveDB(t *testing.T) {
	ctx, _, pool := sharedDB(t, "shared_scope_delete_", 0)
	q := store.New(pool)
	a, _ := newProductUser(ctx, t, pool)
	b, _ := newProductUser(ctx, t, pool)
	c, _ := newProductUser(ctx, t, pool)
	pa := newProvider(ctx, t, q, a, "a-private")
	ka, err := insertProviderKey(ctx, q, a, pa.ID)
	if err != nil {
		t.Fatal(err)
	}
	shared := newSharedProvider(ctx, t, q, "delete-gw")
	var keys []uuid.UUID
	for _, u := range []uuid.UUID{b, c} {
		k, err := insertProviderKey(ctx, q, u, shared.ID)
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, k.ID)
		mustExec(ctx, t, pool, `INSERT INTO model_provider_key_test_results (provider_id, user_id, revision, key_secret_id, key_enablement_rev, auth_result) VALUES ($1, $2, 1, $3, 0, 'ok')`, shared.ID, u, k.ID)
	}
	if _, err := q.SetUserProviderSource(ctx, store.SetUserProviderSourceParams{ID: b, Engine: "claude", ProviderID: pgUUID(shared.ID)}); err != nil {
		t.Fatal(err)
	}

	del := func(selected int64) (deleted []uuid.UUID, err error) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			return nil, err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		tq := store.New(tx)
		if _, err := tq.LockSharedModelProviderForDelete(ctx, shared.ID); err != nil {
			return nil, err
		}
		n, err := tq.CountSharedProviderSelections(ctx, shared.ID)
		if err != nil {
			return nil, err
		}
		if n != selected {
			return nil, fmt.Errorf("selection count = %d, want %d", n, selected)
		}
		if n > 0 {
			// The structural backstop: the delete itself is refused by the RESTRICT foreign key.
			if _, err := tq.DeleteSharedModelProvider(ctx, shared.ID); err == nil {
				return nil, errors.New("deleting a selected shared definition succeeded")
			}
			return nil, nil
		}
		ids, err := tq.DeleteSharedProviderKeys(ctx, shared.ID)
		if err != nil {
			return nil, err
		}
		if rows, err := tq.DeleteSharedModelProvider(ctx, shared.ID); err != nil || rows != 1 {
			return nil, fmt.Errorf("delete = %d (%v)", rows, err)
		}
		return ids, tx.Commit(ctx)
	}
	if _, err := del(1); err != nil {
		t.Fatalf("selected delete: %v", err)
	}
	if got := scalar[int](ctx, t, pool, `SELECT count(*) FROM model_providers WHERE id = $1`, shared.ID); got != 1 {
		t.Fatalf("a refused delete removed the definition")
	}
	if _, err := q.SetUserProviderSource(ctx, store.SetUserProviderSourceParams{ID: b, Engine: "claude"}); err != nil {
		t.Fatal(err)
	}
	ids, err := del(0)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("DeleteSharedProviderKeys returned %d ids, want exactly the 2 attached keys", len(ids))
	}
	got := map[uuid.UUID]bool{ids[0]: true, ids[1]: true}
	for _, k := range keys {
		if !got[k] {
			t.Errorf("key %s missing from the deleted ids %v", k, ids)
		}
	}
	for table, want := range map[string]int{"model_providers": 1, "model_provider_revisions": 1, "model_provider_key_test_results": 0} {
		if n := scalar[int](ctx, t, pool, `SELECT count(*) FROM `+table); n != want {
			t.Errorf("%s rows after the delete = %d, want %d", table, n, want)
		}
	}
	if n := scalar[int](ctx, t, pool, `SELECT count(*) FROM user_secrets WHERE id = ANY($1)`, keys); n != 0 {
		t.Errorf("shared keys left behind: %d", n)
	}
	if n := scalar[int](ctx, t, pool, `SELECT count(*) FROM user_secrets WHERE id = $1`, ka.ID); n != 1 {
		t.Errorf("A's personal key was removed by the shared delete")
	}
}
