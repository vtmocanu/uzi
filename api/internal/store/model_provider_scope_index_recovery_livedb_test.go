package store_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4710: the scope-index guard in 00527, 00528 and users_codex_provider_scope names a recovery
// that works while shared selections or keys exist. These tests follow the runbook in the "Scope-index
// recovery" section of adr/4324-admin-shared-providers.md against real Postgres: the helper runs the
// precondition and count queries as written and reads the index and foreign key catalog rows the other
// inspection queries show to pick the state-table row; the documented repair is then run and the
// migration retried. Every test stands its own isolated database (sharedDB). Skipped
// unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; run via ./e2e/run-store-it.sh.
//
// Several cases corrupt pg_index directly. That is a catalog stand-in for an interrupted concurrent
// build or a state normal operations cannot reach, used in throwaway databases only.

const (
	refusal00527 = "refusing to apply 00527: model_providers_scope_owner_id_key is missing, invalid or not unique. If it is missing, recreate it as 00526 does and retry; if it is invalid with the exact 00526 definition, rebuild it with REINDEX per the runbook and retry; otherwise stop and investigate. See Scope-index recovery in ADR 4324 (adr/4324-admin-shared-providers.md)."
	refusal00528 = "refusing to apply 00528: model_providers_scope_owner_id_key is missing, invalid or not unique. If it is missing, stop and inspect the prerequisite foreign key user_secrets_provider_fkey, since recreating the index alone does not restore it; if it is invalid with the exact 00526 definition, first confirm that foreign key still references it, then rebuild it with REINDEX per the runbook and retry; otherwise stop and investigate. See Scope-index recovery in ADR 4324 (adr/4324-admin-shared-providers.md)."
	refusalCodex = "refusing to apply users_codex_provider_scope: model_providers_scope_owner_id_key is missing, invalid or not unique. If it is missing, stop and inspect the prerequisite foreign keys user_secrets_provider_fkey and users_claude_provider_fkey, since recreating the index alone does not restore them; if it is invalid with the exact 00526 definition, first confirm those foreign keys still reference it, then rebuild it with REINDEX per the runbook and retry; otherwise stop and investigate. See Scope-index recovery in ADR 4324 (adr/4324-admin-shared-providers.md)."

	scopeIndex = "model_providers_scope_owner_id_key"
)

type runbookStage int

const (
	stage00527 runbookStage = iota
	stage00528
	stageCodex
)

// wantRefusal requires the migration error to be the guard's own P0001 with exactly the message, and
// the applied version to be unchanged (the refused file leaves no version row).
func wantRefusal(ctx context.Context, t *testing.T, pool *pgxpool.Pool, err error, want string, wantVersion int64) {
	t.Helper()
	var pgerr *pgconn.PgError
	if !errors.As(err, &pgerr) || pgerr.Code != "P0001" || pgerr.Message != want {
		t.Fatalf("want guard refusal P0001 %q, got %v", want, err)
	}
	if got := appliedVersion(ctx, t, pool); got != wantVersion {
		t.Fatalf("version after the refusal = %d, want %d", got, wantVersion)
	}
}

// runbookFK is one foreign key row the runbook reads from pg_constraint.
type runbookFK struct {
	found, onIndex, validated bool
	def                       string
}

func readRunbookFK(ctx context.Context, t *testing.T, pool *pgxpool.Pool, table, name string, idx uint32) runbookFK {
	t.Helper()
	var fk runbookFK
	err := pool.QueryRow(ctx, `SELECT pg_get_constraintdef(oid), convalidated, conindid = $3::oid FROM pg_constraint
        WHERE contype = 'f' AND conrelid = to_regclass($1) AND conname = $2`, table, name, idx).Scan(&fk.def, &fk.validated, &fk.onIndex)
	if errors.Is(err, pgx.ErrNoRows) {
		return fk
	}
	if err != nil {
		t.Fatalf("read foreign key %s.%s: %v", table, name, err)
	}
	fk.found = true
	return fk
}

// runbook follows the ADR's runbook for the refusing stage and returns the row of its state table that
// applies. The precondition and count queries run as written; the other rows are read from the index
// and foreign key catalogs the remaining inspection queries show.
func runbook(ctx context.Context, t *testing.T, pool *pgxpool.Pool, stage runbookStage) string {
	t.Helper()
	// The documented goose head query must sit just below the refusing file the caller claims.
	wantHead := map[runbookStage]int64{stage00527: 526, stage00528: 527}[stage]
	if stage == stageCodex {
		wantHead = sharedCodexPredecessor(t, resolveSharedCodexVersions(t).userScope)
	}
	if head := scalar[int64](ctx, t, pool, `SELECT max(version_id) FROM goose_db_version WHERE is_applied`); head != wantHead {
		t.Fatalf("migration head = %d, want %d for stage %d (the pending file is the next version above the head)", head, wantHead, stage)
	}
	// Preconditions: no index build in progress on model_providers (any row means wait).
	if n := scalar[int](ctx, t, pool, `SELECT count(*) FROM pg_stat_progress_create_index WHERE relid = to_regclass('model_providers') OR relid IS NULL`); n != 0 {
		return "stop-build-in-progress"
	}
	schema := scalar[string](ctx, t, pool, `SELECT n.nspname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE c.oid = to_regclass('model_providers')`)

	var relkind string
	err := pool.QueryRow(ctx, `SELECT relkind::text FROM pg_class WHERE oid = to_regclass('model_providers_scope_owner_id_key')`).Scan(&relkind)
	if errors.Is(err, pgx.ErrNoRows) {
		if stage == stage00527 {
			return "recreate"
		}
		return "stop-missing-after-00527"
	}
	if err != nil {
		t.Fatalf("read relkind: %v", err)
	}
	if relkind != "i" {
		return "stop-not-index"
	}

	var onParent, valid, ready bool
	var def string
	var idx uint32
	if err := pool.QueryRow(ctx, `SELECT indrelid = to_regclass($1 || '.model_providers'), indisvalid, indisready,
            pg_get_indexdef(indexrelid), indexrelid::oid
        FROM pg_index WHERE indexrelid = to_regclass('model_providers_scope_owner_id_key')`, schema).Scan(&onParent, &valid, &ready, &def, &idx); err != nil {
		t.Fatalf("read index: %v", err)
	}
	exact := "CREATE UNIQUE INDEX model_providers_scope_owner_id_key ON " + schema + ".model_providers USING btree (scope_owner_id, id)"
	nonUnique := "CREATE INDEX model_providers_scope_owner_id_key ON " + schema + ".model_providers USING btree (scope_owner_id, id)"
	switch {
	case !onParent:
		return "stop-wrong-definition"
	case def == exact:
	case def == nonUnique:
		return "stop-not-unique"
	default:
		return "stop-wrong-definition"
	}
	// Exact and unique: the stage's foreign keys must be as expected before either retry or REINDEX.
	ref := "model_providers"
	if scalar[bool](ctx, t, pool, `SELECT NOT pg_table_is_visible(to_regclass('model_providers'))`) {
		ref = schema + ".model_providers"
	}
	secrets := readRunbookFK(ctx, t, pool, "user_secrets", "user_secrets_provider_fkey", idx)
	claude := readRunbookFK(ctx, t, pool, "users", "users_claude_provider_fkey", idx)
	secretsDef := "FOREIGN KEY (provider_scope_id, provider_id) REFERENCES " + ref + "(scope_owner_id, id) ON DELETE CASCADE"
	claudeDef := "FOREIGN KEY (claude_provider_scope_id, claude_provider_id) REFERENCES " + ref + "(scope_owner_id, id) ON DELETE RESTRICT"
	// The 00477/00479-era form the Claude foreign key keeps until users_codex_provider_scope.
	claudeOwnerDef := "FOREIGN KEY (id, claude_provider_id) REFERENCES " + ref + "(owner_user_id, id) ON DELETE RESTRICT"
	switch stage {
	case stage00527:
		// No foreign key depends on the index yet: the documented count query must return 0.
		if n := scalar[int](ctx, t, pool, `SELECT count(*) FROM pg_constraint WHERE contype = 'f' AND conindid = to_regclass('model_providers_scope_owner_id_key')`); n != 0 {
			return "stop-fks"
		}
	case stage00528:
		// The documented count query must return 1 (only user_secrets_provider_fkey is on the index) and
		// the Claude foreign key must still show its owner-key form.
		if n := scalar[int](ctx, t, pool, `SELECT count(*) FROM pg_constraint WHERE contype = 'f' AND conindid = to_regclass('model_providers_scope_owner_id_key')`); n != 1 {
			return "stop-fks"
		}
		if !secrets.found || !secrets.onIndex || secrets.validated || secrets.def != secretsDef+" NOT VALID" ||
			!claude.found || claude.onIndex || claude.def != claudeOwnerDef {
			return "stop-fks"
		}
	case stageCodex:
		if !secrets.found || !secrets.onIndex || !secrets.validated || secrets.def != secretsDef ||
			!claude.found || !claude.onIndex || !claude.validated || claude.def != claudeDef {
			return "stop-fks"
		}
	}
	if valid && ready {
		return "retry"
	}
	return "reindex"
}

func wantRunbook(ctx context.Context, t *testing.T, pool *pgxpool.Pool, stage runbookStage, want string) {
	t.Helper()
	if got := runbook(ctx, t, pool, stage); got != want {
		t.Fatalf("runbook state = %q, want %q", got, want)
	}
}

func schemaOfModelProviders(ctx context.Context, t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	return scalar[string](ctx, t, pool, `SELECT n.nspname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE c.oid = to_regclass('model_providers')`)
}

// recreateScopeIndex is the documented recreate, run outside any transaction.
func recreateScopeIndex(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	mustExec(ctx, t, pool, "CREATE UNIQUE INDEX CONCURRENTLY "+scopeIndex+" ON "+schemaOfModelProviders(ctx, t, pool)+".model_providers (scope_owner_id, id)")
}

// reindexScopeIndex is the documented rebuild.
func reindexScopeIndex(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	mustExec(ctx, t, pool, "REINDEX INDEX "+schemaOfModelProviders(ctx, t, pool)+"."+scopeIndex)
}

func invalidateScopeIndex(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	// Catalog stand-in for an interrupted concurrent build; throwaway database only.
	mustExec(ctx, t, pool, `UPDATE pg_index SET indisvalid = false WHERE indexrelid = 'model_providers_scope_owner_id_key'::regclass`)
	if scalar[bool](ctx, t, pool, `SELECT indisvalid FROM pg_index WHERE indexrelid = 'model_providers_scope_owner_id_key'::regclass`) {
		t.Fatal("the index is still valid after the injection")
	}
}

func wantScopeIndexHealthy(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if !scalar[bool](ctx, t, pool, `SELECT indisvalid AND indisready AND indisunique AND indrelid = to_regclass($1 || '.model_providers')
        FROM pg_index WHERE indexrelid = to_regclass('model_providers_scope_owner_id_key')`, schemaOfModelProviders(ctx, t, pool)) {
		t.Fatal("the scope index is not valid, ready, unique and on model_providers")
	}
}

func TestScopeIndexRecoveryMissingAt00527LiveDB(t *testing.T) {
	t.Run("recreate", func(t *testing.T) {
		ctx, dsn, pool := sharedDB(t, "scope_idx_missing_", 526)
		mustExec(ctx, t, pool, `DROP INDEX `+scopeIndex)
		wantRefusal(ctx, t, pool, store.Migrate(ctx, dsn), refusal00527, 526)
		wantRunbook(ctx, t, pool, stage00527, "recreate")
		recreateScopeIndex(ctx, t, pool)
		if err := store.Migrate(ctx, dsn); err != nil {
			t.Fatalf("Migrate after the documented recreate: %v", err)
		}
		wantScopeIndexHealthy(ctx, t, pool)
	})
	t.Run("a table of that name is not an index", func(t *testing.T) {
		ctx, dsn, pool := sharedDB(t, "scope_idx_table_", 526)
		mustExec(ctx, t, pool, `DROP INDEX `+scopeIndex)
		mustExec(ctx, t, pool, `CREATE TABLE `+scopeIndex+` (x int)`)
		wantRefusal(ctx, t, pool, store.Migrate(ctx, dsn), refusal00527, 526)
		wantRunbook(ctx, t, pool, stage00527, "stop-not-index")
	})
}

func TestScopeIndexRecoveryCascadeAt00528LiveDB(t *testing.T) {
	t.Run("dropped with cascade", func(t *testing.T) {
		ctx, dsn, pool := sharedDB(t, "scope_idx_cascade_a_", 527)
		mustExec(ctx, t, pool, `DROP INDEX `+scopeIndex+` CASCADE`)
		wantRefusal(ctx, t, pool, store.Migrate(ctx, dsn), refusal00528, 527)
		wantRunbook(ctx, t, pool, stage00528, "stop-missing-after-00527")
	})
	t.Run("a decoy foreign key is not accepted", func(t *testing.T) {
		ctx, dsn, pool := sharedDB(t, "scope_idx_cascade_b_", 527)
		mustExec(ctx, t, pool, `DROP INDEX `+scopeIndex+` CASCADE`)
		recreateScopeIndex(ctx, t, pool)
		// Matches the 00528-stage expectation in everything except the table it is on.
		// NOT VALID only takes effect through ALTER TABLE; CREATE TABLE validates the empty table.
		mustExec(ctx, t, pool, `CREATE TABLE scope_decoy (provider_scope_id uuid, provider_id uuid)`)
		mustExec(ctx, t, pool, `ALTER TABLE scope_decoy ADD CONSTRAINT user_secrets_provider_fkey FOREIGN KEY (provider_scope_id, provider_id) REFERENCES model_providers (scope_owner_id, id) ON DELETE CASCADE NOT VALID`)
		invalidateScopeIndex(ctx, t, pool)
		wantRefusal(ctx, t, pool, store.Migrate(ctx, dsn), refusal00528, 527)
		wantRunbook(ctx, t, pool, stage00528, "stop-fks")
	})
}

// scopeSeed is what TestScopeIndexRecoveryInvalidAtCodexScopeLiveDB writes at the schema just before
// users_codex_provider_scope: a shared Claude definition with a key and a selection, and a personal
// definition with a key and a selection for another user.
type scopeSeed struct {
	sharedUser, personalUser uuid.UUID
	shared, personal         uuid.UUID
}

func seedScopeSelections(ctx context.Context, t *testing.T, pool *pgxpool.Pool) scopeSeed {
	t.Helper()
	s := scopeSeed{}
	s.sharedUser, _ = newProductUser(ctx, t, pool)
	s.personalUser, _ = newProductUser(ctx, t, pool)
	s.shared = scalar[uuid.UUID](ctx, t, pool, `INSERT INTO model_providers (owner_user_id, name, engine, current_revision, credential_mode, scope_owner_id)
        VALUES (NULL, 'shared-claude', 'claude', 1, 'per_user_key', $1) RETURNING id`, nilScope)
	mustExec(ctx, t, pool, `INSERT INTO model_provider_revisions (provider_id, revision, protocol, base_url, auth_style, model_map)
        VALUES ($1, 1, 'anthropic_messages', 'https://gw.example.com/v1', 'bearer', '{"haiku":{"model":"h"}}')`, s.shared)
	mustExec(ctx, t, pool, `INSERT INTO user_secrets (user_id, kind, label, is_default, auto_eligible, ciphertext, sealed_with, provider_id, provider_scope_id)
        VALUES ($1, 'anthropic_compatible_key', 'k', false, false, 'ct-shared', 'master', $2, $3)`, s.sharedUser, s.shared, nilScope)
	mustExec(ctx, t, pool, `UPDATE users SET claude_provider_id = $2, claude_provider_scope_id = $3 WHERE id = $1`, s.sharedUser, s.shared, nilScope)

	s.personal = scalar[uuid.UUID](ctx, t, pool, `INSERT INTO model_providers (owner_user_id, name, engine, current_revision, scope_owner_id)
        VALUES ($1, 'personal-claude', 'claude', 1, $1) RETURNING id`, s.personalUser)
	mustExec(ctx, t, pool, `INSERT INTO model_provider_revisions (provider_id, revision, protocol, base_url, auth_style, model_map)
        VALUES ($1, 1, 'anthropic_messages', 'https://gw.example.com/v1', 'bearer', '{"haiku":{"model":"h"}}')`, s.personal)
	mustExec(ctx, t, pool, `INSERT INTO user_secrets (user_id, kind, label, is_default, auto_eligible, ciphertext, sealed_with, provider_id, provider_scope_id)
        VALUES ($1, 'anthropic_compatible_key', 'k', false, false, 'ct-personal', 'master', $2, $1)`, s.personalUser, s.personal)
	mustExec(ctx, t, pool, `UPDATE users SET claude_provider_id = $2, claude_provider_scope_id = $1 WHERE id = $1`, s.personalUser, s.personal)
	return s
}

// scopeSnapshot renders the two dependent foreign keys, the index oid and the seeded rows as one string
// so two states can be compared for identity.
func scopeSnapshot(ctx context.Context, t *testing.T, pool *pgxpool.Pool, s scopeSeed) string {
	t.Helper()
	fks := scalar[string](ctx, t, pool, `SELECT COALESCE(string_agg(concat_ws('|', oid::text, conname, conrelid::regclass::text, pg_get_constraintdef(oid), convalidated::text, conindid::text), E'\n' ORDER BY conname), '')
        FROM pg_constraint WHERE contype = 'f' AND conname IN ('user_secrets_provider_fkey', 'users_claude_provider_fkey')`)
	oid := scalar[string](ctx, t, pool, `SELECT to_regclass('model_providers_scope_owner_id_key')::oid::text`)
	qualified := scalar[string](ctx, t, pool, `SELECT to_regclass($1 || '.model_providers_scope_owner_id_key')::oid::text`, schemaOfModelProviders(ctx, t, pool))
	// Whole rows (to_jsonb): no migration from the predecessor to head adds, drops or retypes a column of
	// model_providers, model_provider_revisions or user_secrets (00538 alters users; 00539, 00540 and 00541 only
	// validate constraints).
	providers := scalar[string](ctx, t, pool, `SELECT COALESCE(string_agg(to_jsonb(p)::text, E'\n' ORDER BY p.id), '') FROM model_providers p WHERE p.id IN ($1, $2)`, s.shared, s.personal)
	revisions := scalar[string](ctx, t, pool, `SELECT COALESCE(string_agg(to_jsonb(r)::text, E'\n' ORDER BY r.provider_id, r.revision), '') FROM model_provider_revisions r WHERE r.provider_id IN ($1, $2)`, s.shared, s.personal)
	secrets := scalar[string](ctx, t, pool, `SELECT COALESCE(string_agg(to_jsonb(u)::text, E'\n' ORDER BY u.id), '') FROM user_secrets u WHERE u.user_id IN ($1, $2)`, s.sharedUser, s.personalUser)
	users := scalar[string](ctx, t, pool, `SELECT COALESCE(string_agg(concat_ws('|', id::text, claude_provider_id::text, claude_provider_scope_id::text), E'\n' ORDER BY id), '')
        FROM users WHERE id IN ($1, $2)`, s.sharedUser, s.personalUser)
	return fmt.Sprintf("fks:\n%s\nindex oid: %s (qualified %s)\nproviders:\n%s\nrevisions:\n%s\nsecrets:\n%s\nusers (claude fields):\n%s", fks, oid, qualified, providers, revisions, secrets, users)
}

func TestScopeIndexRecoveryInvalidAtCodexScopeLiveDB(t *testing.T) {
	vs := resolveSharedCodexVersions(t)
	pred := sharedCodexPredecessor(t, vs.userScope)
	ctx, dsn, pool := sharedDB(t, "scope_idx_inv_codex_", pred)
	seed := seedScopeSelections(ctx, t, pool)

	// Fail closed: with a valid index the runbook does not say reindex.
	wantRunbook(ctx, t, pool, stageCodex, "retry")
	before := scopeSnapshot(ctx, t, pool, seed)
	if n := scalar[int](ctx, t, pool, `SELECT count(*) FROM pg_constraint WHERE contype = 'f' AND conname IN ('user_secrets_provider_fkey', 'users_claude_provider_fkey')
        AND conindid = to_regclass('model_providers_scope_owner_id_key') AND convalidated`); n != 2 {
		t.Fatalf("validated foreign keys on the scope index = %d, want 2", n)
	}
	idxOID := scalar[string](ctx, t, pool, `SELECT to_regclass('model_providers_scope_owner_id_key')::oid::text`)

	// Normal operations cannot reach this state after 00527: a catalog stand-in.
	invalidateScopeIndex(ctx, t, pool)
	wantRefusal(ctx, t, pool, store.Migrate(ctx, dsn), refusalCodex, pred)
	wantRunbook(ctx, t, pool, stageCodex, "reindex")
	reindexScopeIndex(ctx, t, pool)
	wantScopeIndexHealthy(ctx, t, pool)
	if got := scalar[string](ctx, t, pool, `SELECT to_regclass('model_providers_scope_owner_id_key')::oid::text`); got != idxOID {
		t.Errorf("index oid after REINDEX = %s, want %s", got, idxOID)
	}
	if after := scopeSnapshot(ctx, t, pool, seed); after != before {
		t.Errorf("state changed across the refusal and REINDEX:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("Migrate to head after REINDEX: %v", err)
	}
	if after := scopeSnapshot(ctx, t, pool, seed); after != before {
		t.Errorf("state changed after migrating to head:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestScopeIndexRecoveryInvalidAt00528LiveDB(t *testing.T) {
	ctx, dsn, pool := sharedDB(t, "scope_idx_inv_528_", 527)
	idxOID := scalar[string](ctx, t, pool, `SELECT to_regclass('model_providers_scope_owner_id_key')::oid::text`)
	invalidateScopeIndex(ctx, t, pool)
	wantRefusal(ctx, t, pool, store.Migrate(ctx, dsn), refusal00528, 527)
	wantRunbook(ctx, t, pool, stage00528, "reindex")
	reindexScopeIndex(ctx, t, pool)
	wantScopeIndexHealthy(ctx, t, pool)
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("Migrate to head after REINDEX: %v", err)
	}
	if got := scalar[string](ctx, t, pool, `SELECT to_regclass('model_providers_scope_owner_id_key')::oid::text`); got != idxOID {
		t.Errorf("index oid after recovery = %s, want %s", got, idxOID)
	}
}

func TestScopeIndexRecoveryNotUniqueLiveDB(t *testing.T) {
	t.Run("00527", func(t *testing.T) {
		ctx, dsn, pool := sharedDB(t, "scope_idx_nonuniq_a_", 526)
		mustExec(ctx, t, pool, `DROP INDEX `+scopeIndex)
		mustExec(ctx, t, pool, `CREATE INDEX `+scopeIndex+` ON model_providers (scope_owner_id, id)`)
		wantRefusal(ctx, t, pool, store.Migrate(ctx, dsn), refusal00527, 526)
		wantRunbook(ctx, t, pool, stage00527, "stop-not-unique")
	})
	t.Run("users_codex_provider_scope", func(t *testing.T) {
		pred := sharedCodexPredecessor(t, resolveSharedCodexVersions(t).userScope)
		ctx, dsn, pool := sharedDB(t, "scope_idx_nonuniq_b_", pred)
		// Catalog stand-in; normal operations cannot reach a non-unique index here.
		mustExec(ctx, t, pool, `UPDATE pg_index SET indisunique = false WHERE indexrelid = 'model_providers_scope_owner_id_key'::regclass`)
		wantRefusal(ctx, t, pool, store.Migrate(ctx, dsn), refusalCodex, pred)
		wantRunbook(ctx, t, pool, stageCodex, "stop-not-unique")
	})
}

func TestScopeIndexRecoveryStopRowsLiveDB(t *testing.T) {
	t.Run("a unique index with another column order is a wrong definition", func(t *testing.T) {
		ctx, dsn, pool := sharedDB(t, "scope_idx_wrongdef_", 526)
		mustExec(ctx, t, pool, `DROP INDEX `+scopeIndex)
		mustExec(ctx, t, pool, `CREATE UNIQUE INDEX `+scopeIndex+` ON model_providers (id, scope_owner_id)`)
		// Valid and unique would pass the guard; invalidate (catalog stand-in) so it refuses.
		invalidateScopeIndex(ctx, t, pool)
		wantRefusal(ctx, t, pool, store.Migrate(ctx, dsn), refusal00527, 526)
		wantRunbook(ctx, t, pool, stage00527, "stop-wrong-definition")
	})
	t.Run("00527 with a foreign key already on the index", func(t *testing.T) {
		ctx, dsn, pool := sharedDB(t, "scope_idx_fk527_", 526)
		// Any foreign key on the index, whatever its name or table, makes the 00527 count nonzero.
		mustExec(ctx, t, pool, `CREATE TABLE scope_decoy (s uuid, p uuid)`)
		mustExec(ctx, t, pool, `ALTER TABLE scope_decoy ADD CONSTRAINT scope_decoy_fkey FOREIGN KEY (s, p) REFERENCES model_providers (scope_owner_id, id) NOT VALID`)
		invalidateScopeIndex(ctx, t, pool)
		wantRefusal(ctx, t, pool, store.Migrate(ctx, dsn), refusal00527, 526)
		wantRunbook(ctx, t, pool, stage00527, "stop-fks")
	})
	t.Run("00528 with a second foreign key on the index", func(t *testing.T) {
		ctx, dsn, pool := sharedDB(t, "scope_idx_fk528_two_", 527)
		// The real user_secrets foreign key stays; a second one from another table makes the count 2.
		mustExec(ctx, t, pool, `CREATE TABLE scope_decoy (s uuid, p uuid)`)
		mustExec(ctx, t, pool, `ALTER TABLE scope_decoy ADD CONSTRAINT scope_decoy_fkey FOREIGN KEY (s, p) REFERENCES model_providers (scope_owner_id, id) NOT VALID`)
		invalidateScopeIndex(ctx, t, pool)
		wantRefusal(ctx, t, pool, store.Migrate(ctx, dsn), refusal00528, 527)
		wantRunbook(ctx, t, pool, stage00528, "stop-fks")
	})
	t.Run("00528 with the Claude foreign key missing", func(t *testing.T) {
		ctx, dsn, pool := sharedDB(t, "scope_idx_fk528_claude_", 527)
		mustExec(ctx, t, pool, `ALTER TABLE users DROP CONSTRAINT users_claude_provider_fkey`)
		invalidateScopeIndex(ctx, t, pool)
		wantRefusal(ctx, t, pool, store.Migrate(ctx, dsn), refusal00528, 527)
		wantRunbook(ctx, t, pool, stage00528, "stop-fks")
	})
	t.Run("users_codex_provider_scope with a missing Claude foreign key", func(t *testing.T) {
		pred := sharedCodexPredecessor(t, resolveSharedCodexVersions(t).userScope)
		ctx, dsn, pool := sharedDB(t, "scope_idx_codex_fks_", pred)
		mustExec(ctx, t, pool, `ALTER TABLE users DROP CONSTRAINT users_claude_provider_fkey`)
		invalidateScopeIndex(ctx, t, pool)
		wantRefusal(ctx, t, pool, store.Migrate(ctx, dsn), refusalCodex, pred)
		wantRunbook(ctx, t, pool, stageCodex, "stop-fks")
	})
	t.Run("users_codex_provider_scope with the index dropped with CASCADE and recreated valid", func(t *testing.T) {
		pred := sharedCodexPredecessor(t, resolveSharedCodexVersions(t).userScope)
		ctx, _, pool := sharedDB(t, "scope_idx_codex_recreated_", pred)
		mustExec(ctx, t, pool, `DROP INDEX `+scopeIndex+` CASCADE`)
		recreateScopeIndex(ctx, t, pool)
		// No Migrate here: the unchanged guard still passes on this valid index (ADR Residual gap). If an
		// operator reaches the runbook in this state (e.g. after an interrupted repair), the valid row
		// must still stop on the missing foreign keys.
		wantRunbook(ctx, t, pool, stageCodex, "stop-fks")
	})
}

func TestScopeIndexRecoveryValidAfterLostAckLiveDB(t *testing.T) {
	ctx, dsn, pool := sharedDB(t, "scope_idx_lostack_", 526)
	mustExec(ctx, t, pool, `DROP INDEX `+scopeIndex)
	recreateScopeIndex(ctx, t, pool)
	wantRunbook(ctx, t, pool, stage00527, "retry")
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("Migrate over a valid recreated index: %v", err)
	}
}
