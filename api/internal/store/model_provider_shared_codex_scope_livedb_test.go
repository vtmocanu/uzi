package store_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4633 (admin-shared Codex provider definitions) M1 against real Postgres: migrations
// model_providers_shared_codex_engine, user_secrets_shared_codex_scope, users_codex_provider_scope
// and their validate siblings. Versions are resolved by name, so a renumber at merge needs no edit
// here. Every test stands its own isolated database. Skipped unless UZI_TEST_DATABASE_URL points at a
// throwaway Postgres; run via ./e2e/run-store-it.sh.

// sharedCodexVersion returns the goose version of the migration file named "<version>_<name>.sql".
// The match is exact, so "model_providers_shared_codex_engine" never resolves to its validate sibling.
func sharedCodexVersion(t *testing.T, name string) int64 {
	t.Helper()
	entries, err := os.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		prefix, rest, ok := strings.Cut(e.Name(), "_")
		if !ok || rest != name+".sql" {
			continue
		}
		v, err := strconv.ParseInt(prefix, 10, 64)
		if err != nil {
			t.Fatalf("parse version of %s: %v", e.Name(), err)
		}
		return v
	}
	t.Fatalf("no migration named %s", name)
	return 0
}

// sharedCodexPredecessor is the highest migration version below v.
func sharedCodexPredecessor(t *testing.T, v int64) int64 {
	t.Helper()
	entries, err := os.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	var pred int64
	for _, e := range entries {
		prefix, _, ok := strings.Cut(filepath.Base(e.Name()), "_")
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(prefix, 10, 64)
		if err != nil {
			continue
		}
		if n < v && n > pred {
			pred = n
		}
	}
	if pred == 0 {
		t.Fatalf("no predecessor of %d", v)
	}
	return pred
}

// sharedCodexVersions are the six versions in order: engine, key scope, users scope, then the three
// validations.
type sharedCodexVersions struct{ engine, keyScope, userScope, vEngine, vKeyScope, vUserScope int64 }

func resolveSharedCodexVersions(t *testing.T) sharedCodexVersions {
	t.Helper()
	return sharedCodexVersions{
		engine:     sharedCodexVersion(t, "model_providers_shared_codex_engine"),
		keyScope:   sharedCodexVersion(t, "user_secrets_shared_codex_scope"),
		userScope:  sharedCodexVersion(t, "users_codex_provider_scope"),
		vEngine:    sharedCodexVersion(t, "validate_model_providers_shared_codex_engine"),
		vKeyScope:  sharedCodexVersion(t, "validate_user_secrets_shared_codex_scope"),
		vUserScope: sharedCodexVersion(t, "validate_users_codex_provider_scope"),
	}
}

func (v sharedCodexVersions) all() []int64 {
	return []int64{v.engine, v.keyScope, v.userScope, v.vEngine, v.vKeyScope, v.vUserScope}
}

// insertSharedCodexRow writes a shared Codex definition (and its revision) with raw SQL, valid from
// the engine migration on.
func insertSharedCodexRow(ctx context.Context, t *testing.T, pool *pgxpool.Pool, name string) uuid.UUID {
	t.Helper()
	id := scalar[uuid.UUID](ctx, t, pool, `INSERT INTO model_providers (owner_user_id, name, engine, current_revision, credential_mode, scope_owner_id)
        VALUES (NULL, $1, 'codex', 1, 'per_user_key', $2) RETURNING id`, name, nilScope)
	mustExec(ctx, t, pool, `INSERT INTO model_provider_revisions (provider_id, revision, protocol, base_url, auth_style, model_map, engine)
        VALUES ($1, 1, 'openai_responses', 'https://resp.example.com/v1', 'bearer', '{"default":{"model":"m"}}', 'codex')`, id)
	return id
}

func insertSharedCodexKey(ctx context.Context, t *testing.T, pool *pgxpool.Pool, user, provider uuid.UUID) uuid.UUID {
	t.Helper()
	return scalar[uuid.UUID](ctx, t, pool, `INSERT INTO user_secrets (user_id, kind, label, is_default, auto_eligible, ciphertext, sealed_with, provider_id, provider_scope_id)
        VALUES ($1, 'openai_compatible_key', 'k', false, false, 'ct', 'master', $2, $3) RETURNING id`, user, provider, nilScope)
}

// TestSharedCodexScopeConstraintNegativesLiveDB: a shared Codex definition, a source on it and a key
// for it are accepted, while every wrong shape is refused by the named constraint.
func TestSharedCodexScopeConstraintNegativesLiveDB(t *testing.T) {
	ctx, _, pool := sharedDB(t, "shared_codex_neg_", 0)
	q := store.New(pool)
	a, _ := newProductUser(ctx, t, pool)
	b, _ := newProductUser(ctx, t, pool)
	cx, err := q.InsertModelProvider(ctx, store.InsertModelProviderParams{OwnerUserID: a, Name: "a-codex", Engine: "codex"})
	if err != nil {
		t.Fatal(err)
	}

	// model_providers
	sc := insertSharedCodexRow(ctx, t, pool, "shared-codex")
	insProv := `INSERT INTO model_providers (owner_user_id, name, engine, current_revision, credential_mode, scope_owner_id) VALUES (NULL, $1, $2, 1, 'per_user_key', $3)`
	_, err = pool.Exec(ctx, insProv, "third-engine", "gemini", nilScope)
	wantViolation(t, "a shared definition with a third engine", err, "23514", "model_providers_engine_check", "model_providers_shared_engine_check")
	_, err = pool.Exec(ctx, `INSERT INTO model_provider_revisions (provider_id, revision, protocol, base_url, auth_style, model_map, engine)
        VALUES ($1, 2, 'anthropic_messages', 'https://gw.example.com/v1', 'bearer', '{}', 'claude')`, sc)
	wantViolation(t, "a Claude revision on the shared Codex definition", err, "23503", "model_provider_revisions_engine_fkey")

	// users: B's Codex sources
	setSrc := `UPDATE users SET codex_provider_id = $2, codex_provider_scope_id = $3 WHERE id = $1`
	_, err = pool.Exec(ctx, `UPDATE users SET codex_provider_id = $2 WHERE id = $1`, b, sc)
	wantViolation(t, "a Codex source with no scope", err, "23514", "users_codex_provider_scope_check")
	_, err = pool.Exec(ctx, `UPDATE users SET codex_provider_scope_id = $2 WHERE id = $1`, b, b)
	wantViolation(t, "a Codex scope with no source", err, "23514", "users_codex_provider_scope_check")
	_, err = pool.Exec(ctx, setSrc, b, sc, a)
	wantViolation(t, "B's Codex source carrying A's scope", err, "23514", "users_codex_provider_scope_check")
	_, err = pool.Exec(ctx, setSrc, b, cx.ID, b)
	wantViolation(t, "B's source on A's personal Codex definition with B's own scope", err, "23503", "users_codex_provider_fkey")
	_, err = pool.Exec(ctx, setSrc, b, cx.ID, nilScope)
	wantViolation(t, "B's source on A's personal Codex definition with the shared scope", err, "23503", "users_codex_provider_fkey")

	// The legitimate shapes work.
	if _, err := pool.Exec(ctx, setSrc, b, sc, nilScope); err != nil {
		t.Errorf("B's source on the shared Codex definition: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM model_providers WHERE id = $1`, sc); err == nil {
		t.Errorf("deleting a selected shared Codex definition succeeded; users_codex_provider_fkey is ON DELETE RESTRICT")
	} else {
		wantViolation(t, "deleting a selected shared Codex definition", err, "23503", "users_codex_provider_fkey")
	}
	// The database does NOT enforce the engine of a shared source: it accepts a Codex source on a shared
	// Claude definition and a Claude source on a shared Codex definition (nil scope both ways). The
	// engine is enforced by the queries; TestSharedCodexSourceWriterRefusesCrossEngineLiveDB proves the
	// writer refuses both and the reader resolves neither.
	d, _ := newProductUser(ctx, t, pool)
	sharedClaude := newSharedProvider(ctx, t, q, "shared-claude-crossed")
	if _, err := pool.Exec(ctx, setSrc, d, sharedClaude.ID, nilScope); err != nil {
		t.Errorf("the database refused a Codex source on a shared Claude definition: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET claude_provider_id = $2, claude_provider_scope_id = $3 WHERE id = $1`, d, sc, nilScope); err != nil {
		t.Errorf("the database refused a Claude source on a shared Codex definition: %v", err)
	}
	insKey := `INSERT INTO user_secrets (user_id, kind, label, ciphertext, sealed_with, provider_id, provider_scope_id) VALUES ($1, 'openai_compatible_key', 'k', 'ct', 'master', $2, $3)`
	if _, err := pool.Exec(ctx, insKey, b, sc, nilScope); err != nil {
		t.Errorf("B's Codex key on the shared Codex definition with the shared scope: %v", err)
	}
	// A's key may not carry B's scope, and the shared scope still needs a real shared definition.
	_, err = pool.Exec(ctx, insKey, a, sc, b)
	wantViolation(t, "A's key carrying B's scope", err, "23514", "user_secrets_provider_scope_check")
	_, err = pool.Exec(ctx, insKey, a, cx.ID, nilScope)
	wantViolation(t, "a shared-scope key on A's personal Codex definition", err, "23503", "user_secrets_provider_fkey")
}

// sharedCodexLevel is how much shared Codex state a staged Down case holds; each level maps to the
// refusal its Down must raise.
type sharedCodexLevel int

const (
	sharedCodexRowOnly sharedCodexLevel = iota + 1
	sharedCodexKeyAndRow
	sharedCodexSelectionKeyRow
)

func (l sharedCodexLevel) refusal() string {
	switch l {
	case sharedCodexRowOnly:
		return "shared Codex provider definition(s) still exist"
	case sharedCodexKeyAndRow:
		return "shared Codex provider key(s) still exist"
	default:
		return "a user still has a shared Codex provider source selected"
	}
}

// assertSharedCodexSchemaAt checks the objects the migrations up to and including version v leave, and
// whether their validations have run.
func assertSharedCodexSchemaAt(ctx context.Context, t *testing.T, pool *pgxpool.Pool, vs sharedCodexVersions, v int64) {
	t.Helper()
	conDef := func(table, name string) (def string, valid bool, found bool) {
		err := pool.QueryRow(ctx, `SELECT pg_get_constraintdef(oid), convalidated FROM pg_constraint WHERE conrelid = $1::regclass AND conname = $2`, table, name).Scan(&def, &valid)
		return def, valid, err == nil
	}
	if v >= vs.engine {
		def, valid, ok := conDef("model_providers", "model_providers_shared_engine_check")
		if !ok || !strings.Contains(def, "'codex'") || valid != (v >= vs.vEngine) {
			t.Errorf("at %d model_providers_shared_engine_check = %q valid=%t found=%t, want a codex-admitting check validated=%t", v, def, valid, ok, v >= vs.vEngine)
		}
	}
	if v >= vs.keyScope {
		def, valid, ok := conDef("user_secrets", "user_secrets_provider_scope_check")
		if !ok || !strings.Contains(def, "openai_compatible_key") || valid != (v >= vs.vKeyScope) {
			t.Errorf("at %d user_secrets_provider_scope_check = %q valid=%t found=%t, want an openai-key-admitting check validated=%t", v, def, valid, ok, v >= vs.vKeyScope)
		}
	}
	if v >= vs.userScope {
		if !scalar[bool](ctx, t, pool, `SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'users' AND column_name = 'codex_provider_scope_id')`) {
			t.Errorf("at %d users.codex_provider_scope_id is missing", v)
		}
		if !scalar[bool](ctx, t, pool, `SELECT COALESCE((SELECT indisvalid FROM pg_index WHERE indexrelid = to_regclass('users_codex_provider_id_idx')), false)`) {
			t.Errorf("at %d users_codex_provider_id_idx is missing or invalid", v)
		}
		_, valid, ok := conDef("users", "users_codex_provider_scope_check")
		if !ok || valid != (v >= vs.vUserScope) {
			t.Errorf("at %d users_codex_provider_scope_check found=%t valid=%t, want validated=%t", v, ok, valid, v >= vs.vUserScope)
		}
		def, valid, ok := conDef("users", "users_codex_provider_fkey")
		if !ok || !strings.Contains(def, "FOREIGN KEY (codex_provider_scope_id, codex_provider_id)") || valid != (v >= vs.vUserScope) {
			t.Errorf("at %d users_codex_provider_fkey = %q valid=%t found=%t, want the scope-bound key validated=%t", v, def, valid, ok, v >= vs.vUserScope)
		}
	}
}

// TestSharedCodexDownGuardsLiveDB: with shared Codex state present, every one of the six Downs
// refuses with the message for the first kind of state it finds (selection, then key, then
// definition), keeps its version applied and its objects in place, and loses no row. Each case
// stands a fresh database staged at the version under test.
func TestSharedCodexDownGuardsLiveDB(t *testing.T) {
	vs := resolveSharedCodexVersions(t)
	type testCase struct {
		version int64
		level   sharedCodexLevel
	}
	var cases []testCase
	for _, v := range vs.all() {
		cases = append(cases, testCase{v, sharedCodexRowOnly})
		if v >= vs.keyScope {
			cases = append(cases, testCase{v, sharedCodexKeyAndRow})
		}
		if v >= vs.userScope {
			cases = append(cases, testCase{v, sharedCodexSelectionKeyRow})
		}
	}
	if len(cases) != 15 {
		t.Fatalf("case table has %d cases, want 15 (six row-only, five key+row, four selection+key+row)", len(cases))
	}
	migrationNames := map[int64]string{
		vs.engine:     "model_providers_shared_codex_engine",
		vs.keyScope:   "user_secrets_shared_codex_scope",
		vs.userScope:  "users_codex_provider_scope",
		vs.vEngine:    "validate_model_providers_shared_codex_engine",
		vs.vKeyScope:  "validate_user_secrets_shared_codex_scope",
		vs.vUserScope: "validate_users_codex_provider_scope",
	}
	names := map[sharedCodexLevel]string{sharedCodexRowOnly: "row", sharedCodexKeyAndRow: "key+row", sharedCodexSelectionKeyRow: "selection+key+row"}
	for _, c := range cases {
		t.Run(fmt.Sprintf("%05d/%s", c.version, names[c.level]), func(t *testing.T) {
			ctx, dsn, pool := sharedDB(t, "shared_codex_down_", c.version)
			user, _ := newProductUser(ctx, t, pool)
			provider := insertSharedCodexRow(ctx, t, pool, "shared-codex")
			var key uuid.UUID
			if c.level >= sharedCodexKeyAndRow {
				key = insertSharedCodexKey(ctx, t, pool, user, provider)
			}
			if c.level >= sharedCodexSelectionKeyRow {
				mustExec(ctx, t, pool, `UPDATE users SET codex_provider_id = $2, codex_provider_scope_id = $3 WHERE id = $1`, user, provider, nilScope)
			}

			err := store.MigrateDownTo(ctx, dsn, sharedCodexPredecessor(t, c.version))
			// The RAISE messages name the migration, not its draft number, so a renumber changes nothing here.
			want := fmt.Sprintf("refusing to roll back %s: %s", migrationNames[c.version], c.level.refusal())
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("Down of %d = %v, want a refusal containing %q", c.version, err, want)
			}
			if got := appliedVersion(ctx, t, pool); got != c.version {
				t.Errorf("version after the refused Down = %d, want %d", got, c.version)
			}
			assertSharedCodexSchemaAt(ctx, t, pool, vs, c.version)
			if n := scalar[int](ctx, t, pool, `SELECT count(*) FROM model_providers WHERE id = $1 AND owner_user_id IS NULL AND engine = 'codex'`, provider); n != 1 {
				t.Errorf("shared Codex definition rows = %d, want 1 preserved", n)
			}
			wantKeys := 0
			if c.level >= sharedCodexKeyAndRow {
				wantKeys = 1
			}
			if n := scalar[int](ctx, t, pool, `SELECT count(*) FROM user_secrets WHERE id = $1`, key); n != wantKeys {
				t.Errorf("shared Codex key rows = %d, want %d preserved", n, wantKeys)
			}
			if c.level >= sharedCodexSelectionKeyRow {
				if n := scalar[int](ctx, t, pool, `SELECT count(*) FROM users WHERE id = $1 AND codex_provider_id = $2 AND codex_provider_scope_id = $3`, user, provider, nilScope); n != 1 {
					t.Errorf("selected shared Codex source rows = %d, want 1 preserved", n)
				}
			}
		})
	}
}

// TestSharedCodexFullDownLiveDB: with no shared Codex state, rolling the six migrations back lands on
// exactly the schema of a database that never had them, and a re-upgrade works.
func TestSharedCodexFullDownLiveDB(t *testing.T) {
	vs := resolveSharedCodexVersions(t)
	pred := sharedCodexPredecessor(t, vs.engine)
	refCtx, refDSN := standIsolatedDB(t, "shared_codex_ref_")
	migrateTo(refCtx, t, refDSN, pred)
	want := schemaFingerprint(refCtx, t, openSharedPool(refCtx, t, refDSN))

	ctx, dsn, pool := sharedDB(t, "shared_codex_fulldown_", 0)
	if err := store.MigrateDownTo(ctx, dsn, pred); err != nil {
		t.Fatalf("full Down to %d: %v", pred, err)
	}
	if got := appliedVersion(ctx, t, pool); got != pred {
		t.Errorf("version after the full Down = %d, want %d", got, pred)
	}
	if got := schemaFingerprint(ctx, t, pool); diffFingerprints(want, got) != "" {
		t.Errorf("schema after the full Down differs from a database at %d:\n%s", pred, diffFingerprints(want, got))
	}
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("re-upgrade after the full Down: %v", err)
	}
}

// TestSharedCodexMigrationFailureRollbackLiveDB: a failure after the last statement of the users scope
// migration, in either direction, rolls the whole migration back (column, index, check and foreign key
// together with the goose row), and a retry then succeeds.
func TestSharedCodexMigrationFailureRollbackLiveDB(t *testing.T) {
	vs := resolveSharedCodexVersions(t)
	v := vs.userScope
	pred := sharedCodexPredecessor(t, v)
	const fkey00517 = "FOREIGN KEY (id, codex_provider_id) REFERENCES model_providers(owner_user_id, id) ON DELETE RESTRICT"
	columnPresent := `SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'users' AND column_name = 'codex_provider_scope_id')`
	indexPresent := `SELECT to_regclass('users_codex_provider_id_idx') IS NOT NULL`
	checkPresent := `SELECT EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid = 'users'::regclass AND conname = 'users_codex_provider_scope_check')`
	fkeyDef := `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conrelid = 'users'::regclass AND conname = 'users_codex_provider_fkey'`

	wantSentinel := func(t *testing.T, err error, message string) {
		t.Helper()
		var pgerr *pgconn.PgError
		if !errors.As(err, &pgerr) || pgerr.Code != "P0001" || pgerr.Message != message {
			t.Fatalf("want sentinel P0001 %q, got %v", message, err)
		}
	}
	// seedPersonal selects a personal Codex definition for a new user with the columns the staged
	// schema has.
	seedPersonal := func(ctx context.Context, t *testing.T, pool *pgxpool.Pool, withScope bool) (user, provider uuid.UUID) {
		t.Helper()
		user, _ = newProductUser(ctx, t, pool)
		p, err := store.New(pool).InsertModelProvider(ctx, store.InsertModelProviderParams{OwnerUserID: user, Name: "personal-codex", Engine: "codex"})
		if err != nil {
			t.Fatal(err)
		}
		if withScope {
			mustExec(ctx, t, pool, `UPDATE users SET codex_provider_id = $2, codex_provider_scope_id = $1 WHERE id = $1`, user, p.ID)
		} else {
			mustExec(ctx, t, pool, `UPDATE users SET codex_provider_id = $2 WHERE id = $1`, user, p.ID)
		}
		return user, p.ID
	}
	dropTrigger := func(ctx context.Context, t *testing.T, pool *pgxpool.Pool, name string) {
		t.Helper()
		mustExec(ctx, t, pool, "DROP TRIGGER "+name+" ON public.goose_db_version")
		mustExec(ctx, t, pool, "DROP FUNCTION public."+name+"()")
	}

	t.Run("up", func(t *testing.T) {
		ctx, dsn, pool := sharedDB(t, "shared_codex_upfail_", pred)
		user, provider := seedPersonal(ctx, t, pool, false)
		// The trigger runs after the migration's last statement but before goose can record and
		// commit the version, and first proves the changes are visible to the migrating transaction.
		mustExec(ctx, t, pool, fmt.Sprintf(`CREATE FUNCTION public.shared_codex_up_fail() RETURNS trigger LANGUAGE plpgsql AS $body$
 BEGIN
 IF NEW.version_id = %d AND NEW.is_applied THEN
 IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'users' AND column_name = 'codex_provider_scope_id')
    OR to_regclass('users_codex_provider_id_idx') IS NULL
    OR NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'users_codex_provider_fkey' AND pg_get_constraintdef(oid) LIKE '%%codex_provider_scope_id%%') THEN
 RAISE EXCEPTION USING ERRCODE='P0002', MESSAGE='not visible';
 END IF;
 RAISE EXCEPTION USING ERRCODE='P0001', MESSAGE='shared-codex-up-after-change';
 END IF;
 RETURN NEW;
 END $body$;
 CREATE TRIGGER shared_codex_up_fail BEFORE INSERT ON public.goose_db_version
 FOR EACH ROW EXECUTE FUNCTION public.shared_codex_up_fail();`, v))
		wantSentinel(t, store.MigrateTo(ctx, dsn, v), "shared-codex-up-after-change")
		if got := appliedVersion(ctx, t, pool); got != pred {
			t.Errorf("version after the failed Up = %d, want %d", got, pred)
		}
		if scalar[bool](ctx, t, pool, columnPresent) || scalar[bool](ctx, t, pool, indexPresent) || scalar[bool](ctx, t, pool, checkPresent) {
			t.Errorf("the failed Up left its column, index or check behind")
		}
		if got := scalar[string](ctx, t, pool, fkeyDef); got != fkey00517 {
			t.Errorf("users_codex_provider_fkey after the failed Up = %q, want %q", got, fkey00517)
		}
		if got := scalar[pgtype.UUID](ctx, t, pool, `SELECT codex_provider_id FROM users WHERE id = $1`, user); uuid.UUID(got.Bytes) != provider {
			t.Errorf("selection after the failed Up = %v, want %s", got, provider)
		}
		dropTrigger(ctx, t, pool, "shared_codex_up_fail")
		if err := store.MigrateTo(ctx, dsn, v); err != nil {
			t.Fatalf("retry Up: %v", err)
		}
		if got := scalar[uuid.UUID](ctx, t, pool, `SELECT codex_provider_scope_id FROM users WHERE id = $1`, user); got != user {
			t.Errorf("backfilled scope = %s, want %s", got, user)
		}
	})

	t.Run("down", func(t *testing.T) {
		ctx, dsn, pool := sharedDB(t, "shared_codex_downfail_", v)
		user, provider := seedPersonal(ctx, t, pool, true)
		mustExec(ctx, t, pool, fmt.Sprintf(`CREATE FUNCTION public.shared_codex_down_fail() RETURNS trigger LANGUAGE plpgsql AS $body$
 BEGIN
 IF OLD.version_id = %d THEN
 IF EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'users' AND column_name = 'codex_provider_scope_id')
    OR to_regclass('users_codex_provider_id_idx') IS NOT NULL
    OR EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'users_codex_provider_scope_check')
    OR NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'users_codex_provider_fkey' AND pg_get_constraintdef(oid) = '%s') THEN
 RAISE EXCEPTION USING ERRCODE='P0002', MESSAGE='not visible';
 END IF;
 RAISE EXCEPTION USING ERRCODE='P0001', MESSAGE='shared-codex-down-after-change';
 END IF;
 RETURN OLD;
 END $body$;
 CREATE TRIGGER shared_codex_down_fail BEFORE DELETE ON public.goose_db_version
 FOR EACH ROW EXECUTE FUNCTION public.shared_codex_down_fail();`, v, fkey00517))
		wantSentinel(t, store.MigrateDownTo(ctx, dsn, pred), "shared-codex-down-after-change")
		if got := appliedVersion(ctx, t, pool); got != v {
			t.Errorf("version after the failed Down = %d, want %d", got, v)
		}
		if !scalar[bool](ctx, t, pool, columnPresent) || !scalar[bool](ctx, t, pool, indexPresent) || !scalar[bool](ctx, t, pool, checkPresent) {
			t.Errorf("the failed Down dropped the column, index or check")
		}
		if got := scalar[string](ctx, t, pool, fkeyDef); !strings.Contains(got, "FOREIGN KEY (codex_provider_scope_id, codex_provider_id)") {
			t.Errorf("users_codex_provider_fkey after the failed Down = %q, want the scope-bound key", got)
		}
		var sel, scope pgtype.UUID
		if err := pool.QueryRow(ctx, `SELECT codex_provider_id, codex_provider_scope_id FROM users WHERE id = $1`, user).Scan(&sel, &scope); err != nil {
			t.Fatal(err)
		}
		if uuid.UUID(sel.Bytes) != provider || uuid.UUID(scope.Bytes) != user {
			t.Errorf("selection after the failed Down = %v scope %v, want %s scope %s", sel, scope, provider, user)
		}
		dropTrigger(ctx, t, pool, "shared_codex_down_fail")
		if err := store.MigrateDownTo(ctx, dsn, pred); err != nil {
			t.Fatalf("retry Down: %v", err)
		}
		if got := scalar[string](ctx, t, pool, fkeyDef); got != fkey00517 {
			t.Errorf("users_codex_provider_fkey after the retried Down = %q, want %q", got, fkey00517)
		}
		if got := scalar[pgtype.UUID](ctx, t, pool, `SELECT codex_provider_id FROM users WHERE id = $1`, user); uuid.UUID(got.Bytes) != provider {
			t.Errorf("selection after the retried Down = %v, want %s", got, provider)
		}
	})
}
