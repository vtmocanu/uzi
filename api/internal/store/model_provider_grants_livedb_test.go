package store_test

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #4540 M2 m1 against real Postgres: the grants table and the all-users column (their migrations,
// guarded Downs and constraints) and the grant statements. Every test stands its own isolated database.
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; run via ./e2e/run-store-it.sh.

const (
	grantsMigration         = "model_provider_grants"
	grantsValidateMigration = "validate_model_provider_grants_all_users"
)

func grantsInsertOperatorDef(ctx context.Context, t *testing.T, pool *pgxpool.Pool, name string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO model_providers (id, owner_user_id, name, engine, current_revision, credential_mode, scope_owner_id)
		VALUES ($1, NULL, $2, 'claude', 1, 'operator_key', '00000000-0000-0000-0000-000000000000')`, id, name)
	return id
}

// TestModelProviderGrantsDownGuardsLiveDB: each Down is tested one step from exactly its own version. It
// refuses with its own sentinel while a grant exists or the all-users switch is on, changing nothing, and
// succeeds when neither does.
func TestModelProviderGrantsDownGuardsLiveDB(t *testing.T) {
	const (
		sGrant    = "refusing to roll back 00560: recipient grant(s) exist"
		sAllUsers = "refusing to roll back 00560: all-users organization definition(s) exist"
		sValidate = "refusing to roll back 00561: all-users organization definition(s) exist"
	)
	cases := []struct {
		name, migrate string
		fixture       func(ctx context.Context, t *testing.T, pool *pgxpool.Pool)
		sentinel      string
	}{
		{"00560 with a grant", grantsMigration, func(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
			def := grantsInsertOperatorDef(ctx, t, pool, "g560")
			user, _ := newProductUser(ctx, t, pool)
			mustExec(ctx, t, pool, `INSERT INTO model_provider_grants (provider_id, user_id) VALUES ($1, $2)`, def, user)
		}, sGrant},
		{"00560 with all-users on", grantsMigration, func(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
			def := grantsInsertOperatorDef(ctx, t, pool, "a560")
			mustExec(ctx, t, pool, `UPDATE model_providers SET operator_all_users = true WHERE id = $1`, def)
		}, sAllUsers},
		{"00561 with all-users on", grantsValidateMigration, func(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
			def := grantsInsertOperatorDef(ctx, t, pool, "a561")
			mustExec(ctx, t, pool, `UPDATE model_providers SET operator_all_users = true WHERE id = $1`, def)
		}, sValidate},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := operatorMigration(t, c.migrate)
			ctx, dsn, pool := sharedDB(t, "grants_down_", m.version)
			if head := appliedVersion(ctx, t, pool); head != m.version {
				t.Fatalf("database head = %d, want exactly %d", head, m.version)
			}
			c.fixture(ctx, t, pool)
			before := opRowCounts(ctx, t, pool, "model_providers", "model_provider_grants")
			wantPgError(t, "the guarded Down", store.MigrateDownTo(ctx, dsn, m.predecessor), "P0001", c.sentinel)
			if after := opRowCounts(ctx, t, pool, "model_providers", "model_provider_grants"); after != before {
				t.Errorf("a refused Down changed the data: %s -> %s", before, after)
			}
			if head := appliedVersion(ctx, t, pool); head != m.version {
				t.Errorf("a refused Down moved the goose head to %d, want %d", head, m.version)
			}
			for _, obj := range []string{"model_provider_grants", "model_provider_grants_user_id_idx"} {
				if !scalar[bool](ctx, t, pool, `SELECT to_regclass($1) IS NOT NULL`, obj) {
					t.Errorf("%s is gone after a refused Down: the guard must run before the drop", obj)
				}
			}
			if !scalar[bool](ctx, t, pool, `SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'model_providers' AND column_name = 'operator_all_users')`) {
				t.Error("operator_all_users is gone after a refused Down")
			}
		})
	}

	t.Run("the empty state rolls down and back up", func(t *testing.T) {
		low, top := operatorMigration(t, grantsMigration), operatorMigration(t, grantsValidateMigration)
		ctx, dsn, pool := sharedDB(t, "grants_down_empty_", top.version)
		// Definitions with all-users off, and a grant removed, do not block the Down.
		def := grantsInsertOperatorDef(ctx, t, pool, "empty")
		user, _ := newProductUser(ctx, t, pool)
		mustExec(ctx, t, pool, `INSERT INTO model_provider_grants (provider_id, user_id) VALUES ($1, $2)`, def, user)
		mustExec(ctx, t, pool, `DELETE FROM model_provider_grants WHERE provider_id = $1`, def)
		if err := store.MigrateDownTo(ctx, dsn, low.predecessor); err != nil {
			t.Fatalf("Down of an empty state to %d: %v", low.predecessor, err)
		}
		if head := appliedVersion(ctx, t, pool); head != low.predecessor {
			t.Fatalf("head after the Down = %d, want %d", head, low.predecessor)
		}
		if scalar[bool](ctx, t, pool, `SELECT to_regclass('model_provider_grants') IS NOT NULL`) {
			t.Error("the grants table survived the Down")
		}
		if scalar[bool](ctx, t, pool, `SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'model_providers' AND column_name = 'operator_all_users')`) ||
			scalar[bool](ctx, t, pool, `SELECT EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid = 'model_providers'::regclass AND conname = 'model_providers_operator_all_users_check')`) {
			t.Error("the all-users column or its check survived the Down")
		}
		if err := store.MigrateTo(ctx, dsn, top.version); err != nil {
			t.Fatalf("re-Up: %v", err)
		}
		if !scalar[bool](ctx, t, pool, `SELECT convalidated FROM pg_constraint WHERE conrelid = 'model_providers'::regclass AND conname = 'model_providers_operator_all_users_check'`) {
			t.Error("the all-users check is not validated after the re-Up")
		}
	})
}

// TestModelProviderGrantsMigrationShapeLiveDB: the all-users check is added NOT VALID by 00560 and
// validated by 00561, the Downs lock every table the guard reads (parents first) before it runs, and
// neither file opts out of goose's transaction.
func TestModelProviderGrantsMigrationShapeLiveDB(t *testing.T) {
	add, validate := operatorMigration(t, grantsMigration), operatorMigration(t, grantsValidateMigration)
	if validate.predecessor != add.version {
		t.Errorf("the validation migration %d does not directly follow %d", validate.version, add.version)
	}
	ctx, _, pool := sharedDB(t, "grants_shape_", add.version)
	const q = `SELECT convalidated FROM pg_constraint WHERE conrelid = 'model_providers'::regclass AND conname = 'model_providers_operator_all_users_check'`
	if scalar[bool](ctx, t, pool, q) {
		t.Error("00560 added the all-users check already validated; it must be NOT VALID")
	}
	_, _, head := sharedDB(t, "grants_shape_head_", 0)
	if !scalar[bool](ctx, t, head, q) {
		t.Error("the all-users check is not validated at head")
	}
	for _, c := range []struct {
		name  string
		locks []string
	}{
		{grantsMigration, []string{"model_providers", "users", "model_provider_grants"}},
		{grantsValidateMigration, []string{"model_providers"}},
	} {
		m := operatorMigration(t, c.name)
		path := fmt.Sprintf("migrations/%05d_%s.sql", m.version, c.name)
		raw, err := os.ReadFile(path) //nolint:gosec // G304: a path built from this package's own migrations dir and a constant name, read-only.
		if err != nil {
			t.Fatal(err)
		}
		text := string(raw)
		if strings.Contains(text, "NO TRANSACTION") {
			t.Errorf("%s opts out of the per-file transaction", path)
		}
		for _, want := range []string{"-- +goose Up\nSET LOCAL lock_timeout = '5s';", "-- +goose Down\nSET LOCAL lock_timeout = '5s';"} {
			if !strings.Contains(text, want) {
				t.Errorf("%s lacks %q", path, want)
			}
		}
		down := text[strings.Index(text, "-- +goose Down"):]
		var locked []string
		lastLock := -1
		offset := 0
		for _, line := range strings.Split(down, "\n") {
			if strings.HasPrefix(line, "LOCK TABLE") {
				if !strings.HasSuffix(line, "IN ACCESS EXCLUSIVE MODE;") {
					t.Errorf("%s locks a table in a mode other than ACCESS EXCLUSIVE: %s", path, line)
				}
				locked = append(locked, strings.TrimSuffix(strings.TrimPrefix(line, "LOCK TABLE "), " IN ACCESS EXCLUSIVE MODE;"))
				lastLock = offset
			}
			offset += len(line) + 1
		}
		if !slices.Equal(locked, c.locks) {
			t.Errorf("%s Down locks %v, want %v", path, locked, c.locks)
		}
		if guard := strings.Index(down, "DO $$"); guard < 0 || lastLock > guard {
			t.Errorf("%s Down has a LOCK TABLE after its first DO $$ guard (or no guard)", path)
		}
	}
}

// TestModelProviderGrantStatementsLiveDB: the grant statements against the real schema: an idempotent
// insert, the keyset page, the cascades, the user lock, the all-users CHECK and the switch's own scope.
func TestModelProviderGrantStatementsLiveDB(t *testing.T) {
	ctx, _, pool, q, _ := opHeadDB(t, "grants_stmt_")
	def := newOperatorProvider(ctx, t, q, "grants-def")
	perUser, err := q.InsertSharedModelProvider(ctx, store.InsertSharedModelProviderParams{Name: "grants-peruser", Engine: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	var users []uuid.UUID
	for range 4 {
		u, _ := newProductUser(ctx, t, pool)
		users = append(users, u)
	}

	// Insert is idempotent: the second is 0 rows, and the count and the existence read follow.
	for i, want := range []int64{1, 0} {
		n, err := q.InsertModelProviderGrant(ctx, store.InsertModelProviderGrantParams{ProviderID: def.ID, UserID: users[0]})
		if err != nil || n != want {
			t.Fatalf("insert %d = %d, %v, want %d", i, n, err, want)
		}
	}
	for _, u := range users[1:] {
		if _, err := q.InsertModelProviderGrant(ctx, store.InsertModelProviderGrantParams{ProviderID: def.ID, UserID: u}); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := q.CountModelProviderGrants(ctx, def.ID); err != nil || n != 4 {
		t.Fatalf("count = %d, %v, want 4", n, err)
	}
	if ok, err := q.HasModelProviderGrant(ctx, store.HasModelProviderGrantParams{ProviderID: def.ID, UserID: users[2]}); err != nil || !ok {
		t.Errorf("has(granted) = %v, %v", ok, err)
	}
	if ok, err := q.HasModelProviderGrant(ctx, store.HasModelProviderGrantParams{ProviderID: def.ID, UserID: uuid.New()}); err != nil || ok {
		t.Errorf("has(absent) = %v, %v", ok, err)
	}

	// The keyset page: ordered by user_id, strictly after the cursor, limited.
	sorted := slices.Clone(users)
	slices.SortFunc(sorted, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	page, err := q.ListModelProviderGrantsPage(ctx, store.ListModelProviderGrantsPageParams{ProviderID: def.ID, AfterUserID: uuid.Nil, PageLimit: 3})
	if err != nil || !slices.Equal(page, sorted[:3]) {
		t.Fatalf("first page = %v, %v, want %v", page, err, sorted[:3])
	}
	page, err = q.ListModelProviderGrantsPage(ctx, store.ListModelProviderGrantsPageParams{ProviderID: def.ID, AfterUserID: sorted[2], PageLimit: 3})
	if err != nil || !slices.Equal(page, sorted[3:]) {
		t.Fatalf("second page = %v, %v, want %v", page, err, sorted[3:])
	}

	// The user lock: a present user returns a row, an absent one none.
	if _, err := q.LockUserForGrant(ctx, users[0]); err != nil {
		t.Errorf("lock of a present user: %v", err)
	}
	if _, err := q.LockUserForGrant(ctx, uuid.New()); err == nil {
		t.Error("lock of an absent user returned a row")
	}

	// The switch sets only an organization-funded definition; a per-user-key row is 0 rows.
	if n, err := q.SetOperatorAllUsers(ctx, store.SetOperatorAllUsersParams{ID: perUser.ID, Enabled: true}); err != nil || n != 0 {
		t.Errorf("all-users on a per-user-key definition = %d, %v, want 0 rows", n, err)
	}
	if n, err := q.SetOperatorAllUsers(ctx, store.SetOperatorAllUsersParams{ID: def.ID, Enabled: true}); err != nil || n != 1 {
		t.Errorf("all-users on an operator definition = %d, %v, want 1 row", n, err)
	}
	locked, err := q.LockSharedModelProviderForEdit(ctx, def.ID)
	if err != nil || !locked.OperatorAllUsers {
		t.Errorf("the edit lock reads all-users = %v, %v, want true", locked.OperatorAllUsers, err)
	}
	// The admin read carries the switch and the count, never a user id.
	row, err := q.GetSharedModelProviderAdmin(ctx, def.ID)
	if err != nil || !row.OperatorAllUsers || row.NamedGrantCount != 4 {
		t.Errorf("admin get = all-users %v, count %d, %v, want true and 4", row.OperatorAllUsers, row.NamedGrantCount, err)
	}

	// Delete one, then all.
	if n, err := q.DeleteModelProviderGrant(ctx, store.DeleteModelProviderGrantParams{ProviderID: def.ID, UserID: users[0]}); err != nil || n != 1 {
		t.Errorf("delete = %d, %v", n, err)
	}
	if n, err := q.DeleteModelProviderGrant(ctx, store.DeleteModelProviderGrantParams{ProviderID: def.ID, UserID: users[0]}); err != nil || n != 0 {
		t.Errorf("repeated delete = %d, %v, want 0", n, err)
	}
	if n, err := q.DeleteAllModelProviderGrants(ctx, def.ID); err != nil || n != 3 {
		t.Errorf("delete all = %d, %v, want 3", n, err)
	}

	// The foreign keys: a grant for an unknown user or definition is refused, and a deleted user or
	// definition takes its grants with it.
	_, err = q.InsertModelProviderGrant(ctx, store.InsertModelProviderGrantParams{ProviderID: def.ID, UserID: uuid.New()})
	if code, _ := pgCodeC(err); code != "23503" {
		t.Errorf("grant for an unknown user = %v, want a foreign key violation", err)
	}
	_, err = q.InsertModelProviderGrant(ctx, store.InsertModelProviderGrantParams{ProviderID: uuid.New(), UserID: users[1]})
	if code, _ := pgCodeC(err); code != "23503" {
		t.Errorf("grant for an unknown definition = %v, want a foreign key violation", err)
	}
	if _, err := q.InsertModelProviderGrant(ctx, store.InsertModelProviderGrantParams{ProviderID: def.ID, UserID: users[1]}); err != nil {
		t.Fatal(err)
	}
	mustExec(ctx, t, pool, `DELETE FROM users WHERE id = $1`, users[1])
	if n := scalar[int](ctx, t, pool, `SELECT count(*) FROM model_provider_grants WHERE provider_id = $1`, def.ID); n != 0 {
		t.Errorf("grants after the user was deleted = %d, want 0", n)
	}
	if _, err := q.InsertModelProviderGrant(ctx, store.InsertModelProviderGrantParams{ProviderID: def.ID, UserID: users[2]}); err != nil {
		t.Fatal(err)
	}
	mustExec(ctx, t, pool, `UPDATE model_providers SET operator_all_users = false WHERE id = $1`, def.ID)
	mustExec(ctx, t, pool, `DELETE FROM model_providers WHERE id = $1`, def.ID)
	if n := scalar[int](ctx, t, pool, `SELECT count(*) FROM model_provider_grants WHERE user_id = $1`, users[2]); n != 0 {
		t.Errorf("grants after the definition was deleted = %d, want 0", n)
	}
}
