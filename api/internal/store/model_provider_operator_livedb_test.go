package store_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/operatorkey"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #4540 M1 against real Postgres: the organization-funded mode (00549-00551), the custody and Test
// evidence tables, their queries, and the recipient statements that must never see any of it. Every
// test stands its own isolated database. Skipped unless UZI_TEST_DATABASE_URL points at a throwaway
// Postgres; run via ./e2e/run-store-it.sh.

// The three migrations are found by name, not by number: the numbers are renumbered at landing.
const (
	opModeMigration     = "model_providers_operator_mode"
	opValidateMigration = "validate_model_providers_operator_mode"
	opKeysMigration     = "model_provider_operator_keys"
)

type opMigration struct {
	version, predecessor int64
}

// operatorMigration returns the version of the named migration and the version just below it.
func operatorMigration(t *testing.T, name string) opMigration {
	t.Helper()
	entries, err := os.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	var versions []int64
	found := int64(-1)
	for _, e := range entries {
		prefix, rest, ok := strings.Cut(e.Name(), "_")
		if !ok || !strings.HasSuffix(rest, ".sql") {
			continue
		}
		var v int64
		if _, err := fmt.Sscanf(prefix, "%d", &v); err != nil {
			t.Fatalf("parse version of %s: %v", e.Name(), err)
		}
		versions = append(versions, v)
		if strings.TrimSuffix(rest, ".sql") == name {
			found = v
		}
	}
	if found < 0 {
		t.Fatalf("no migration named *_%s.sql", name)
	}
	sort.Slice(versions, func(i, j int) bool { return versions[i] < versions[j] })
	m := opMigration{version: found}
	for _, v := range versions {
		if v < found {
			m.predecessor = v
		}
	}
	return m
}

// opHeadDB stands an isolated database at head with a pool and a user to act as the admin.
func opHeadDB(t *testing.T, prefix string) (context.Context, string, *pgxpool.Pool, *store.Queries, uuid.UUID) {
	t.Helper()
	ctx, dsn, pool := sharedDB(t, prefix, 0)
	admin, _ := newProductUser(ctx, t, pool)
	return ctx, dsn, pool, store.New(pool), admin
}

// newOperatorProvider creates an organization-funded definition with revision 1, and returns it.
func newOperatorProvider(ctx context.Context, t *testing.T, q *store.Queries, name string) store.ModelProvider {
	t.Helper()
	p, err := q.InsertSharedOperatorModelProvider(ctx, name)
	if err != nil {
		t.Fatalf("insert organization-funded provider %q: %v", name, err)
	}
	addRevision(ctx, t, q, p.ID, 1, "https://gw.example.com/anthropic")
	return p
}

func opBox(t *testing.T) *secretbox.Box {
	t.Helper()
	key := make([]byte, secretbox.KeySize)
	for i := range key {
		key[i] = byte(i*5 + 11)
	}
	b, err := secretbox.New(key)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func setOperatorKey(ctx context.Context, t *testing.T, q *store.Queries, box *secretbox.Box, provider, by uuid.UUID, plaintext string) store.InsertOperatorProviderKeyRow {
	t.Helper()
	sealed, err := operatorkey.Seal(box, provider, []byte(plaintext))
	if err != nil {
		t.Fatal(err)
	}
	row, err := q.InsertOperatorProviderKey(ctx, store.InsertOperatorProviderKeyParams{ProviderID: provider, Ciphertext: sealed, SetBy: by})
	if err != nil {
		t.Fatalf("set the organization key: %v", err)
	}
	return row
}

// wantPgError fails unless err carries the exact PostgreSQL error code and message.
func wantPgError(t *testing.T, what string, err error, code, message string) {
	t.Helper()
	var pe *pgconn.PgError
	if !errors.As(err, &pe) {
		t.Errorf("%s: got %v, want a PostgreSQL error %s %q", what, err, code, message)
		return
	}
	if pe.Code != code || pe.Message != message {
		t.Errorf("%s: got %s %q, want %s %q", what, pe.Code, pe.Message, code, message)
	}
}

func opRowCounts(ctx context.Context, t *testing.T, pool *pgxpool.Pool, tables ...string) string {
	t.Helper()
	var parts []string
	for _, tbl := range tables {
		parts = append(parts, fmt.Sprintf("%s=%d", tbl, scalar[int](ctx, t, pool, "SELECT count(*) FROM "+tbl)))
	}
	return strings.Join(parts, ",")
}

// TestOperatorProviderRecipientQueriesLiveDB is the store half of "an organization-funded definition is
// unreachable to a recipient": every recipient statement returns no row for it (and the writes change
// nothing), a selection forced in SQL reads back as a source with no definition, and the runs binding
// CHECK refuses the operator mode, so nothing can bind a run to it in M1.
func TestOperatorProviderRecipientQueriesLiveDB(t *testing.T) {
	ctx, _, pool, q, admin := opHeadDB(t, "operator_recipient_")
	op := newOperatorProvider(ctx, t, q, "org-funded")
	custody := setOperatorKey(ctx, t, q, opBox(t), op.ID, admin, "org-gateway-key")
	if err := q.UpsertOperatorProviderKeyTestResult(ctx, store.UpsertOperatorProviderKeyTestResultParams{
		ProviderID: op.ID, Revision: 1, KeyID: custody.ID, KeyRev: custody.KeyRev, AuthResult: "ok", ProbedBy: admin}); err != nil {
		t.Fatal(err)
	}
	perUser := newSharedProvider(ctx, t, q, "per-user-shared") // the control: a recipient sees this one
	user, _ := newProductUser(ctx, t, pool)
	own := newProvider(ctx, t, q, user, "own-gw")

	list, err := q.ListVisibleProviders(ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[uuid.UUID]bool{}
	for _, r := range list {
		seen[r.ID] = true
	}
	if seen[op.ID] || !seen[perUser.ID] || !seen[own.ID] {
		t.Errorf("ListVisibleProviders: organization-funded=%t per-user-key shared=%t own=%t, want false true true", seen[op.ID], seen[perUser.ID], seen[own.ID])
	}
	notFound := func(what string, err error) {
		t.Helper()
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("%s = %v, want no row", what, err)
		}
	}
	_, err = q.GetVisibleProviderForUse(ctx, store.GetVisibleProviderForUseParams{ID: op.ID, UserID: user})
	notFound("GetVisibleProviderForUse", err)
	_, err = q.LockModelProviderForShare(ctx, store.LockModelProviderForShareParams{ID: op.ID, OwnerUserID: user})
	notFound("LockModelProviderForShare", err)
	_, err = q.GetModelProviderRevisionForOwner(ctx, store.GetModelProviderRevisionForOwnerParams{ProviderID: op.ID, Revision: 1, OwnerUserID: user})
	notFound("GetModelProviderRevisionForOwner", err)
	_, err = q.LockProviderBindingRevisionForKeyShareNowait(ctx, store.LockProviderBindingRevisionForKeyShareNowaitParams{ProviderID: op.ID, Revision: 1, OwnerUserID: user})
	notFound("LockProviderBindingRevisionForKeyShareNowait", err)
	_, err = q.LockVisibleProviderForKeyMutation(ctx, store.LockVisibleProviderForKeyMutationParams{ID: op.ID, UserID: user})
	notFound("LockVisibleProviderForKeyMutation", err)
	_, err = q.LockVisibleProviderForEvidence(ctx, store.LockVisibleProviderForEvidenceParams{ID: op.ID, UserID: user, Revision: 1})
	notFound("LockVisibleProviderForEvidence", err)
	_, err = insertProviderKey(ctx, q, user, op.ID)
	notFound("a recipient attaching a key (InsertProviderKeySecret)", err)
	_, err = q.SetUserProviderSource(ctx, store.SetUserProviderSourceParams{ID: user, Engine: "claude", ProviderID: pgUUID(op.ID)})
	notFound("SetUserProviderSource", err)
	if got := scalar[pgtype.UUID](ctx, t, pool, `SELECT claude_provider_id FROM users WHERE id = $1`, user); got.Valid {
		t.Errorf("a refused selection left claude_provider_id = %v", got)
	}
	if n := scalar[int](ctx, t, pool, `SELECT count(*) FROM user_secrets WHERE provider_id = $1`, op.ID); n != 0 {
		t.Errorf("recipient keys attached to the organization-funded definition = %d, want 0", n)
	}

	// A selection forced in SQL (what a bug or a hand edit could leave) reads as a selected source whose
	// definition does not resolve: the source reader returns a row with no provider columns.
	mustExec(ctx, t, pool, `UPDATE users SET claude_provider_id = $1, claude_provider_scope_id = $2 WHERE id = $3`, op.ID, nilScope, user)
	src, err := q.GetUserProviderSource(ctx, store.GetUserProviderSourceParams{Engine: "claude", ID: user})
	if err != nil {
		t.Fatal(err)
	}
	if !src.ProviderID.Valid || src.Engine.Valid || src.CurrentRevision.Valid || src.KeySecretID.Valid {
		t.Errorf("GetUserProviderSource on a forced selection = %+v, want the selected id and no resolved definition or key", src)
	}
	_, err = q.GetProviderKeyForUpdate(ctx, store.GetProviderKeyForUpdateParams{UserID: user, ProviderID: pgUUID(op.ID)})
	notFound("GetProviderKeyForUpdate", err)

	// The runs binding CHECK still admits only 'user_key': no run can be bound to the operator mode.
	conn, repo := uuid.New(), uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO forge_connections(id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext) VALUES($1,$2,'github','https://example.com','bot',1,'x')`, conn, user)
	mustExec(ctx, t, pool, `INSERT INTO repos(id,connection_id,forge_project_id,path_with_namespace,web_url,default_branch) VALUES($1,$2,1,'example/operator','https://example.com/operator','main')`, repo, conn)
	_, err = pool.Exec(ctx, `INSERT INTO runs(user_id,repo_id,kind,branch,issue_title,issue_description,harness,model_provider_id,model_provider_revision,model_provider_key_mode,model_provider_key_secret_id,model_provider_key_rev)
        VALUES($1,$2,'task','b','t','d','claude',$3,1,'operator_key',$4,1)`, user, repo, op.ID, custody.ID)
	wantViolation(t, "a run bound with the operator key mode", err, "23514", "runs_model_provider_key_mode_check")
	if n := scalar[int](ctx, t, pool, `SELECT count(*) FROM runs WHERE model_provider_id = $1`, op.ID); n != 0 {
		t.Errorf("runs bound to the organization-funded definition = %d, want 0", n)
	}
}

// TestOperatorKeyAADLiveDB: the sealed key is bound to its definition. A ciphertext swapped onto another
// definition's custody row, and a read for the wrong definition, both refuse; the sink returns the
// ciphertext only for the exact (id, provider, key_rev) while enabled.
func TestOperatorKeyAADLiveDB(t *testing.T) {
	ctx, _, pool, q, admin := opHeadDB(t, "operator_aad_")
	box := opBox(t)
	a := newOperatorProvider(ctx, t, q, "aad-a")
	b := newOperatorProvider(ctx, t, q, "aad-b")
	ka := setOperatorKey(ctx, t, q, box, a.ID, admin, "key-for-a")
	kb := setOperatorKey(ctx, t, q, box, b.ID, admin, "key-for-b")
	sink := func(provider uuid.UUID, k store.InsertOperatorProviderKeyRow) []byte {
		t.Helper()
		row, err := q.GetOperatorProviderKeyForTest(ctx, store.GetOperatorProviderKeyForTestParams{ID: k.ID, ProviderID: provider, KeyRev: k.KeyRev})
		if err != nil {
			t.Fatalf("the sink for %s: %v", provider, err)
		}
		return row.Ciphertext
	}
	ctA, ctB := sink(a.ID, ka), sink(b.ID, kb)
	if got, err := operatorkey.Open(box, a.ID, ctA); err != nil || string(got) != "key-for-a" {
		t.Fatalf("control: A's key opens for A = %q (%v)", got, err)
	}
	if got, err := operatorkey.Open(box, b.ID, ctA); err == nil {
		t.Errorf("A's ciphertext opened for B: %q", got)
	}
	// Swap the ciphertexts in the database: each row now holds the other definition's key.
	mustExec(ctx, t, pool, `UPDATE model_provider_operator_keys SET ciphertext = CASE provider_id WHEN $1 THEN $3::bytea ELSE $4::bytea END WHERE provider_id IN ($1, $2)`, a.ID, b.ID, ctB, ctA)
	swappedA, swappedB := sink(a.ID, ka), sink(b.ID, kb)
	if !bytes.Equal(swappedA, ctB) || !bytes.Equal(swappedB, ctA) {
		t.Fatal("the swap did not land")
	}
	if got, err := operatorkey.Open(box, a.ID, swappedA); err == nil {
		t.Errorf("after the swap, A's custody row opened for A: %q", got)
	}
	if got, err := operatorkey.Open(box, b.ID, swappedB); err == nil {
		t.Errorf("after the swap, B's custody row opened for B: %q", got)
	}
	// The sink matches only the exact tuple, and only while the key is enabled.
	for name, p := range map[string]store.GetOperatorProviderKeyForTestParams{
		"another definition": {ID: ka.ID, ProviderID: b.ID, KeyRev: ka.KeyRev},
		"a stale key_rev":    {ID: ka.ID, ProviderID: a.ID, KeyRev: ka.KeyRev - 1},
	} {
		if _, err := q.GetOperatorProviderKeyForTest(ctx, p); !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("the sink for %s = %v, want no row", name, err)
		}
	}
	if n, err := q.DisableOperatorProviderKey(ctx, a.ID); err != nil || n != 1 {
		t.Fatalf("disable = %d (%v)", n, err)
	}
	// Disabling draws a new key_rev, so the pre-disable ka.KeyRev would miss for that reason alone. Use
	// the disabled row's current key_rev: only the disabled_at predicate can then refuse it.
	const curRev = `SELECT key_rev FROM model_provider_operator_keys WHERE provider_id = $1`
	current := scalar[int64](ctx, t, pool, curRev, a.ID)
	if current == ka.KeyRev {
		t.Fatalf("disable did not draw a new key_rev (%d); the check below would not isolate the disabled predicate", current)
	}
	disabled := store.GetOperatorProviderKeyForTestParams{ID: ka.ID, ProviderID: a.ID, KeyRev: current}
	if _, err := q.GetOperatorProviderKeyForTest(ctx, disabled); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("the sink for a disabled key at its current key_rev = %v, want no row", err)
	}
	// Positive control: the same id, provider and current key_rev returns the row once the key is
	// enabled again (cleared in SQL so key_rev stays put).
	mustExec(ctx, t, pool, `UPDATE model_provider_operator_keys SET disabled_at = NULL WHERE provider_id = $1`, a.ID)
	if got := scalar[int64](ctx, t, pool, curRev, a.ID); got != current {
		t.Fatalf("control moved key_rev: %d -> %d", current, got)
	}
	if row, err := q.GetOperatorProviderKeyForTest(ctx, disabled); err != nil || row.KeyRev != current {
		t.Errorf("control: the sink for the same key_rev, enabled = %+v (%v), want the row", row, err)
	}
}

// TestOperatorShapeChecksLiveDB: the row shape the migrations enforce.
func TestOperatorShapeChecksLiveDB(t *testing.T) {
	ctx, _, pool, q, admin := opHeadDB(t, "operator_shape_")
	op := newOperatorProvider(ctx, t, q, "shape-op")
	if op.CredentialMode.String != "operator_key" || op.Engine != "claude" || op.OwnerUserID.Valid {
		t.Fatalf("operator definition = %+v, want a shared Claude operator_key row", op)
	}

	insert := `INSERT INTO model_providers (owner_user_id, name, engine, current_revision, credential_mode, scope_owner_id)
        VALUES (NULL, $1, $2, 1, $3, '00000000-0000-0000-0000-000000000000')`
	// The otherwise identical Codex row fails the engine check; the Claude one inserts.
	_, err := pool.Exec(ctx, insert, "shape-codex", "codex", "operator_key")
	wantViolation(t, "an operator-key Codex definition", err, "23514", "model_providers_operator_engine_check")
	if _, err := pool.Exec(ctx, insert, "shape-claude", "claude", "operator_key"); err != nil {
		t.Errorf("an operator-key Claude definition: %v", err)
	}
	// A per-user-key shared Codex definition is still allowed (the engine check binds only the operator mode).
	if _, err := pool.Exec(ctx, insert, "shape-peruser-codex", "codex", "per_user_key"); err != nil {
		t.Errorf("a per-user-key shared Codex definition: %v", err)
	}
	// The refusals the older migrations set keep firing.
	_, err = pool.Exec(ctx, insert, "shape-bogus", "claude", "bogus_mode")
	wantViolation(t, "an unknown credential mode", err, "23514", "model_providers_credential_mode_check")
	_, err = pool.Exec(ctx, `INSERT INTO model_providers (owner_user_id, name, engine, current_revision, scope_owner_id)
        VALUES (NULL, 'shape-nomode', 'claude', 1, '00000000-0000-0000-0000-000000000000')`)
	wantViolation(t, "a NULL owner with no mode", err, "23514", "model_providers_shared_mode_check")
	_, err = pool.Exec(ctx, `INSERT INTO model_providers (owner_user_id, name, engine, current_revision, credential_mode, scope_owner_id)
        VALUES ($1, 'shape-owned-operator', 'claude', 1, 'operator_key', $1)`, admin)
	wantViolation(t, "an owned definition in the operator mode", err, "23514", "model_providers_shared_mode_check")
	for _, c := range []string{"model_providers_credential_mode_check", "model_providers_operator_engine_check"} {
		if !scalar[bool](ctx, t, pool, `SELECT convalidated FROM pg_constraint WHERE conrelid = 'model_providers'::regclass AND conname = $1`, c) {
			t.Errorf("%s is not validated at head", c)
		}
	}

	// Custody: key_rev comes from the sequence; custody needs a definition; one key per definition.
	box := opBox(t)
	k1 := setOperatorKey(ctx, t, q, box, op.ID, admin, "one")
	if k1.KeyRev <= 0 {
		t.Errorf("key_rev from the sequence = %d, want positive", k1.KeyRev)
	}
	_, err = q.InsertOperatorProviderKey(ctx, store.InsertOperatorProviderKeyParams{ProviderID: op.ID, Ciphertext: []byte("x"), SetBy: admin})
	wantViolation(t, "a second key for one definition", err, "23505", "model_provider_operator_keys_provider_id_key")
	_, err = q.InsertOperatorProviderKey(ctx, store.InsertOperatorProviderKeyParams{ProviderID: uuid.New(), Ciphertext: []byte("x"), SetBy: admin})
	wantViolation(t, "a key for no definition", err, "23503", "model_provider_operator_keys_provider_id_fkey")
	other := newOperatorProvider(ctx, t, q, "shape-other")
	k2 := setOperatorKey(ctx, t, q, box, other.ID, admin, "two")
	if k2.KeyRev <= k1.KeyRev {
		t.Errorf("key_rev across definitions: %d then %d, want increasing", k1.KeyRev, k2.KeyRev)
	}

	// Evidence: the happy path, then each shape violation.
	good := store.UpsertOperatorProviderKeyTestResultParams{ProviderID: op.ID, Revision: 1, KeyID: k1.ID, KeyRev: k1.KeyRev, AuthResult: "ok", ProbedBy: admin}
	if err := q.UpsertOperatorProviderKeyTestResult(ctx, good); err != nil {
		t.Fatalf("valid evidence: %v", err)
	}
	fkNames := func(parent string) []string {
		rows, err := pool.Query(ctx, `SELECT conname FROM pg_constraint WHERE conrelid = 'model_provider_operator_key_test_results'::regclass AND contype = 'f' AND confrelid = $1::regclass`, parent)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var names []string
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				t.Fatal(err)
			}
			names = append(names, n)
		}
		return names
	}
	// Cross-provider evidence: B's evidence naming A's key id fails the (provider, key) foreign key.
	cross := good
	cross.ProviderID, cross.KeyID = other.ID, k1.ID
	cross.Revision, cross.KeyRev = 1, k2.KeyRev
	err = q.UpsertOperatorProviderKeyTestResult(ctx, cross)
	wantViolation(t, "evidence naming another definition's key", err, "23503", fkNames("model_provider_operator_keys")...)
	// Evidence for a revision the definition does not have.
	missing := good
	missing.Revision = 9
	err = q.UpsertOperatorProviderKeyTestResult(ctx, missing)
	wantViolation(t, "evidence for a missing revision", err, "23503", fkNames("model_provider_revisions")...)
	// Evidence with no custody row at all is impossible (the foreign key), the premise of the 00551 guard order.
	noCustody := newOperatorProvider(ctx, t, q, "shape-nocustody")
	err = q.UpsertOperatorProviderKeyTestResult(ctx, store.UpsertOperatorProviderKeyTestResultParams{ProviderID: noCustody.ID, Revision: 1, KeyID: uuid.New(), KeyRev: 1, AuthResult: "ok", ProbedBy: admin})
	wantViolation(t, "evidence with no custody row", err, "23503", fkNames("model_provider_operator_keys")...)
	// Each NOT NULL evidence column refuses NULL; tool_result may be NULL. Literal values keep every
	// column's type known to the server, so the failure is the NOT NULL check and nothing else.
	for _, col := range []string{"revision", "key_id", "key_rev", "auth_result", "probed_at", "probed_by"} {
		vals := map[string]string{
			"provider_id": "'" + other.ID.String() + "'", "revision": "1", "key_id": "'" + k2.ID.String() + "'",
			"key_rev": fmt.Sprint(k2.KeyRev), "auth_result": "'ok'", "probed_at": "now()", "probed_by": "'" + admin.String() + "'",
		}
		vals[col] = "NULL"
		var names, vs []string
		for _, c := range []string{"provider_id", "revision", "key_id", "key_rev", "auth_result", "probed_at", "probed_by"} {
			names, vs = append(names, c), append(vs, vals[c])
		}
		_, err := pool.Exec(ctx, `INSERT INTO model_provider_operator_key_test_results (`+strings.Join(names, ",")+`) VALUES (`+strings.Join(vs, ",")+`)`)
		if code, _ := pgCodeC(err); code != "23502" {
			t.Errorf("evidence with NULL %s: %v, want not_null_violation", col, err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO model_provider_operator_key_test_results (provider_id, revision, key_id, key_rev, auth_result, tool_result, probed_at, probed_by) VALUES ($1, 1, $2, $3, 'ok', NULL, now(), $4)`,
		other.ID, k2.ID, k2.KeyRev, admin); err != nil {
		t.Errorf("evidence with a NULL tool_result: %v", err)
	}
	// Deleting the definition removes custody and evidence with it (cascade); deleting custody alone
	// removes its evidence.
	if n, err := q.DeleteOperatorProviderKey(ctx, op.ID); err != nil || n != 1 {
		t.Fatalf("delete custody = %d (%v)", n, err)
	}
	if n := scalar[int](ctx, t, pool, `SELECT count(*) FROM model_provider_operator_key_test_results WHERE provider_id = $1`, op.ID); n != 0 {
		t.Errorf("evidence after its custody row was deleted = %d, want 0", n)
	}
	if n, err := q.DeleteSharedModelProvider(ctx, other.ID); err != nil || n != 1 {
		t.Fatalf("delete definition = %d (%v)", n, err)
	}
	if got := opRowCounts(ctx, t, pool, "model_provider_operator_keys", "model_provider_operator_key_test_results"); !strings.Contains(got, "model_provider_operator_keys=0") || !strings.HasSuffix(got, "results=0") {
		t.Errorf("after deleting the definitions: %s, want both tables empty", got)
	}
}

// TestOperatorKeyRevSQLMonotonicLiveDB: at the SQL level key_rev strictly increases across set, replace,
// disable and enable, and a repeated disable or enable updates no row and so leaves it alone. (PRD
// #4540 M2 adds the route-level proof, including delete-then-restore, in the handler.)
func TestOperatorKeyRevSQLMonotonicLiveDB(t *testing.T) {
	ctx, _, _, q, admin := opHeadDB(t, "operator_keyrev_")
	box := opBox(t)
	op := newOperatorProvider(ctx, t, q, "rev-op")
	rev := func() (int64, bool) {
		t.Helper()
		row, err := q.LockOperatorProviderKeyForWrite(ctx, op.ID)
		if err != nil {
			t.Fatalf("lock: %v", err)
		}
		return row.KeyRev, row.DisabledAt.Valid
	}
	k := setOperatorKey(ctx, t, q, box, op.ID, admin, "v1")
	prev := k.KeyRev
	step := func(what string, wantRows int64, n int64, err error) {
		t.Helper()
		if err != nil || n != wantRows {
			t.Fatalf("%s updated %d rows (%v), want %d", what, n, err, wantRows)
		}
	}
	grew := func(what string) {
		t.Helper()
		got, _ := rev()
		if got <= prev {
			t.Fatalf("%s: key_rev %d after %d, want strictly greater", what, got, prev)
		}
		prev = got
	}
	same := func(what string) {
		t.Helper()
		if got, _ := rev(); got != prev {
			t.Fatalf("%s: key_rev %d, want it unchanged at %d", what, got, prev)
		}
	}
	sealed, err := operatorkey.Seal(box, op.ID, []byte("v2"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.ReplaceOperatorProviderKey(ctx, store.ReplaceOperatorProviderKeyParams{ProviderID: op.ID, Ciphertext: sealed, SetBy: admin}); err != nil {
		t.Fatal(err)
	}
	grew("replace")
	n, err := q.DisableOperatorProviderKey(ctx, op.ID)
	step("disable", 1, n, err)
	grew("disable")
	n, err = q.DisableOperatorProviderKey(ctx, op.ID)
	step("a repeated disable", 0, n, err)
	same("a repeated disable")
	// A replace keeps the key disabled.
	if _, err := q.ReplaceOperatorProviderKey(ctx, store.ReplaceOperatorProviderKeyParams{ProviderID: op.ID, Ciphertext: sealed, SetBy: admin}); err != nil {
		t.Fatal(err)
	}
	grew("replace while disabled")
	if _, disabled := rev(); !disabled {
		t.Error("a replace re-enabled a disabled key")
	}
	n, err = q.EnableOperatorProviderKey(ctx, op.ID)
	step("enable", 1, n, err)
	grew("enable")
	n, err = q.EnableOperatorProviderKey(ctx, op.ID)
	step("a repeated enable", 0, n, err)
	same("a repeated enable")
}

// TestOperatorKeyDownGuardsLiveDB: each migration's Down is tested one step from exactly its own
// version, and refuses with its own sentinel while the state it guards exists. A refusal changes nothing:
// the data, the goose head and (for 00551) the tables are all still there.
func TestOperatorKeyDownGuardsLiveDB(t *testing.T) {
	const (
		s549      = "refusing to roll back 00549: operator-key provider definition(s) exist"
		s550      = "refusing to roll back 00550: operator-key provider definition(s) exist"
		sEvidence = "refusing to roll back 00551: operator key test evidence exists"
		sCustody  = "refusing to roll back 00551: organization key custody row(s) exist"
		sDef      = "refusing to roll back 00551: operator-key provider definition(s) exist"
	)
	insertDef := func(ctx context.Context, t *testing.T, pool *pgxpool.Pool, name string) uuid.UUID {
		t.Helper()
		id := uuid.New()
		mustExec(ctx, t, pool, `INSERT INTO model_providers (id, owner_user_id, name, engine, current_revision, credential_mode, scope_owner_id)
            VALUES ($1, NULL, $2, 'claude', 1, 'operator_key', '00000000-0000-0000-0000-000000000000')`, id, name)
		mustExec(ctx, t, pool, `INSERT INTO model_provider_revisions (provider_id, revision, protocol, base_url, auth_style, model_map)
            VALUES ($1, 1, 'anthropic_messages', 'https://gw.example.com/a', 'bearer', '{"haiku":{"model":"h"}}')`, id)
		return id
	}
	cases := []struct {
		name     string
		migrate  string
		fixture  func(ctx context.Context, t *testing.T, pool *pgxpool.Pool)
		sentinel string
		tables   bool
	}{
		{"00549 with a keyless definition", opModeMigration, func(ctx context.Context, t *testing.T, pool *pgxpool.Pool) { insertDef(ctx, t, pool, "d549") }, s549, false},
		{"00550 with a keyless definition", opValidateMigration, func(ctx context.Context, t *testing.T, pool *pgxpool.Pool) { insertDef(ctx, t, pool, "d550") }, s550, false},
		{"00551 with a keyless definition", opKeysMigration, func(ctx context.Context, t *testing.T, pool *pgxpool.Pool) { insertDef(ctx, t, pool, "d551") }, sDef, true},
		{"00551 with a custody row and no evidence", opKeysMigration, func(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
			id := insertDef(ctx, t, pool, "c551")
			mustExec(ctx, t, pool, `INSERT INTO model_provider_operator_keys (provider_id, ciphertext, set_by) VALUES ($1, 'x', $2)`, id, uuid.New())
		}, sCustody, true},
		{"00551 with evidence and its required custody row", opKeysMigration, func(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
			id := insertDef(ctx, t, pool, "e551")
			var key uuid.UUID
			var rev int64
			if err := pool.QueryRow(ctx, `INSERT INTO model_provider_operator_keys (provider_id, ciphertext, set_by) VALUES ($1, 'x', $2) RETURNING id, key_rev`, id, uuid.New()).Scan(&key, &rev); err != nil {
				t.Fatal(err)
			}
			mustExec(ctx, t, pool, `INSERT INTO model_provider_operator_key_test_results (provider_id, revision, key_id, key_rev, auth_result, probed_at, probed_by) VALUES ($1, 1, $2, $3, 'ok', now(), $4)`, id, key, rev, uuid.New())
		}, sEvidence, true},
	}
	counts := func(ctx context.Context, t *testing.T, pool *pgxpool.Pool, tables bool) string {
		t.Helper()
		out := opRowCounts(ctx, t, pool, "model_providers", "model_provider_revisions")
		if tables {
			out += "," + opRowCounts(ctx, t, pool, "model_provider_operator_keys", "model_provider_operator_key_test_results")
		}
		return out
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := operatorMigration(t, c.migrate)
			ctx, dsn, pool := sharedDB(t, "operator_down_", m.version)
			if head := appliedVersion(ctx, t, pool); head != m.version {
				t.Fatalf("database head = %d, want exactly %d", head, m.version)
			}
			c.fixture(ctx, t, pool)
			before := counts(ctx, t, pool, c.tables)
			err := store.MigrateDownTo(ctx, dsn, m.predecessor)
			wantPgError(t, "the guarded Down", err, "P0001", c.sentinel)
			if after := counts(ctx, t, pool, c.tables); after != before {
				t.Errorf("a refused Down changed the data: %s -> %s", before, after)
			}
			if head := appliedVersion(ctx, t, pool); head != m.version {
				t.Errorf("a refused Down moved the goose head to %d, want %d", head, m.version)
			}
			if c.tables {
				for _, tbl := range []string{"model_provider_operator_keys", "model_provider_operator_key_test_results"} {
					if !scalar[bool](ctx, t, pool, `SELECT to_regclass($1) IS NOT NULL`, tbl) {
						t.Errorf("%s is gone after a refused 00551 Down: the guard must run before the drop", tbl)
					}
				}
				if !scalar[bool](ctx, t, pool, `SELECT to_regclass('model_provider_operator_key_rev_seq') IS NOT NULL`) {
					t.Error("the key_rev sequence is gone after a refused 00551 Down")
				}
			}
		})
	}

	t.Run("evidence without custody is impossible", func(t *testing.T) {
		m := operatorMigration(t, opKeysMigration)
		ctx, _, pool := sharedDB(t, "operator_down_fk_", m.version)
		id := insertDef(ctx, t, pool, "fk551")
		_, err := pool.Exec(ctx, `INSERT INTO model_provider_operator_key_test_results (provider_id, revision, key_id, key_rev, auth_result, probed_at, probed_by) VALUES ($1, 1, $2, 1, 'ok', now(), $3)`, id, uuid.New(), uuid.New())
		if code, _ := pgCodeC(err); code != "23503" {
			t.Fatalf("evidence with no custody row: %v, want a foreign key violation", err)
		}
	})

	t.Run("the empty state rolls all the way down and back up", func(t *testing.T) {
		ops := []opMigration{operatorMigration(t, opModeMigration), operatorMigration(t, opValidateMigration), operatorMigration(t, opKeysMigration)}
		low, top := ops[0], ops[2]
		ctx, dsn, pool := sharedDB(t, "operator_down_empty_", top.version)
		if err := store.MigrateDownTo(ctx, dsn, low.predecessor); err != nil {
			t.Fatalf("Down of an empty state to %d: %v", low.predecessor, err)
		}
		if head := appliedVersion(ctx, t, pool); head != low.predecessor {
			t.Fatalf("head after the Down = %d, want %d", head, low.predecessor)
		}
		for _, obj := range []string{"model_provider_operator_keys", "model_provider_operator_key_test_results", "model_provider_operator_key_rev_seq"} {
			if scalar[bool](ctx, t, pool, `SELECT to_regclass($1) IS NOT NULL`, obj) {
				t.Errorf("%s survived the Down", obj)
			}
		}
		def := scalar[string](ctx, t, pool, `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conrelid = 'model_providers'::regclass AND conname = 'model_providers_credential_mode_check'`)
		if strings.Contains(def, "operator_key") || !scalar[bool](ctx, t, pool, `SELECT convalidated FROM pg_constraint WHERE conrelid = 'model_providers'::regclass AND conname = 'model_providers_credential_mode_check'`) {
			t.Errorf("credential_mode check after the Down = %s, want the validated single-mode check", def)
		}
		if scalar[bool](ctx, t, pool, `SELECT EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid = 'model_providers'::regclass AND conname = 'model_providers_operator_engine_check')`) {
			t.Error("the operator engine check survived the Down")
		}
		// Re-Up restores the default and the sequence ownership.
		if err := store.MigrateTo(ctx, dsn, top.version); err != nil {
			t.Fatalf("re-Up: %v", err)
		}
		if got := scalar[string](ctx, t, pool, `SELECT pg_get_serial_sequence('model_provider_operator_keys', 'key_rev')`); got != "public.model_provider_operator_key_rev_seq" {
			t.Errorf("owned sequence after the re-Up = %q", got)
		}
		if def := scalar[string](ctx, t, pool, `SELECT pg_get_expr(d.adbin, d.adrelid) FROM pg_attrdef d JOIN pg_attribute a ON a.attrelid = d.adrelid AND a.attnum = d.adnum WHERE d.adrelid = 'model_provider_operator_keys'::regclass AND a.attname = 'key_rev'`); !strings.Contains(def, "nextval('model_provider_operator_key_rev_seq'") {
			t.Errorf("key_rev default after the re-Up = %q", def)
		}
	})
}

// TestOperatorKeyMigrationFailureRollbackLiveDB: an Up that fails after its DDL leaves nothing behind
// and no version row, and the retry applies it. A trigger on the version insert first checks the
// file's DDL is present in the same transaction, then raises, so the rollback is of real DDL.
func TestOperatorKeyMigrationFailureRollbackLiveDB(t *testing.T) {
	type state struct{ modeCheck, engineCheck, tables string }
	read := func(ctx context.Context, t *testing.T, pool *pgxpool.Pool) state {
		t.Helper()
		var s state
		for _, c := range []struct {
			dst  *string
			name string
		}{{&s.modeCheck, "model_providers_credential_mode_check"}, {&s.engineCheck, "model_providers_operator_engine_check"}} {
			var def string
			var valid bool
			err := pool.QueryRow(ctx, `SELECT pg_get_constraintdef(oid), convalidated FROM pg_constraint WHERE conrelid = 'model_providers'::regclass AND conname = $1`, c.name).Scan(&def, &valid)
			switch {
			case errors.Is(err, pgx.ErrNoRows):
				*c.dst = "absent"
			case err != nil:
				t.Fatal(err)
			default:
				*c.dst = fmt.Sprintf("%t:%s", valid, def)
			}
		}
		s.tables = scalar[string](ctx, t, pool, `SELECT concat_ws(',', to_regclass('model_provider_operator_keys')::text, to_regclass('model_provider_operator_key_test_results')::text, to_regclass('model_provider_operator_key_rev_seq')::text)`)
		return s
	}
	cases := []struct {
		name      string
		migration string
		// ddlPresent is a boolean SQL expression, true once the file's DDL is visible in the transaction.
		ddlPresent string
		prior      func(t *testing.T, s state)
		post       func(t *testing.T, s state)
	}{
		{"mode", opModeMigration,
			`EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid = 'model_providers'::regclass AND conname = 'model_providers_credential_mode_check' AND pg_get_constraintdef(oid) LIKE '%operator_key%')
             AND EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid = 'model_providers'::regclass AND conname = 'model_providers_operator_engine_check')`,
			func(t *testing.T, s state) {
				if !strings.HasPrefix(s.modeCheck, "true:") || strings.Contains(s.modeCheck, "operator_key") || s.engineCheck != "absent" {
					t.Errorf("state after the failed 00549 = %+v, want the validated single-mode check and no engine check", s)
				}
			},
			func(t *testing.T, s state) {
				if !strings.HasPrefix(s.modeCheck, "false:") || !strings.Contains(s.modeCheck, "operator_key") || !strings.HasPrefix(s.engineCheck, "false:") {
					t.Errorf("state after the retried 00549 = %+v, want both checks present and NOT VALID", s)
				}
			}},
		{"validate", opValidateMigration,
			`(SELECT count(*) = 2 AND bool_and(convalidated) FROM pg_constraint WHERE conrelid = 'model_providers'::regclass AND conname IN ('model_providers_credential_mode_check', 'model_providers_operator_engine_check'))`,
			func(t *testing.T, s state) {
				if !strings.HasPrefix(s.modeCheck, "false:") || !strings.HasPrefix(s.engineCheck, "false:") {
					t.Errorf("state after the failed 00550 = %+v, want both checks still NOT VALID", s)
				}
			},
			func(t *testing.T, s state) {
				if !strings.HasPrefix(s.modeCheck, "true:") || !strings.HasPrefix(s.engineCheck, "true:") {
					t.Errorf("state after the retried 00550 = %+v, want both checks validated", s)
				}
			}},
		{"keys", opKeysMigration,
			`to_regclass('model_provider_operator_keys') IS NOT NULL AND to_regclass('model_provider_operator_key_test_results') IS NOT NULL AND to_regclass('model_provider_operator_key_rev_seq') IS NOT NULL`,
			func(t *testing.T, s state) {
				if s.tables != "" {
					t.Errorf("state after the failed 00551 = %+v, want no tables and no sequence", s)
				}
			},
			func(t *testing.T, s state) {
				if s.tables == "" || strings.Count(s.tables, ",") != 2 {
					t.Errorf("state after the retried 00551 = %+v, want both tables and the sequence", s)
				}
			}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := operatorMigration(t, c.migration)
			ctx, dsn, pool := sharedDB(t, "operator_rollback_", m.predecessor)
			if head := appliedVersion(ctx, t, pool); head != m.predecessor {
				t.Fatalf("head = %d, want %d", head, m.predecessor)
			}
			mustExec(ctx, t, pool, fmt.Sprintf(`CREATE FUNCTION public.operator_fail_after_ddl() RETURNS trigger LANGUAGE plpgsql AS $body$
 BEGIN
 IF NEW.version_id = %d AND NEW.is_applied THEN
 IF NOT (%s) THEN
 RAISE EXCEPTION USING ERRCODE = 'P0002', MESSAGE = 'the migration DDL is not visible';
 END IF;
 RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'operator-after-ddl';
 END IF;
 RETURN NEW;
 END $body$;
 CREATE TRIGGER operator_fail_after_ddl BEFORE INSERT ON public.goose_db_version
 FOR EACH ROW EXECUTE FUNCTION public.operator_fail_after_ddl();`, m.version, c.ddlPresent))
			err := store.MigrateTo(ctx, dsn, m.version)
			wantPgError(t, "the failed Up", err, "P0001", "operator-after-ddl")
			c.prior(t, read(ctx, t, pool))
			if head := appliedVersion(ctx, t, pool); head != m.predecessor {
				t.Errorf("head after the failed Up = %d, want %d", head, m.predecessor)
			}
			mustExec(ctx, t, pool, `DROP TRIGGER operator_fail_after_ddl ON public.goose_db_version; DROP FUNCTION public.operator_fail_after_ddl()`)
			if err := store.MigrateTo(ctx, dsn, m.version); err != nil {
				t.Fatalf("retry Up: %v", err)
			}
			c.post(t, read(ctx, t, pool))
			if head := appliedVersion(ctx, t, pool); head != m.version {
				t.Errorf("head after the retry = %d, want %d", head, m.version)
			}
		})
	}
}

// TestOperatorKeyMigrationsAreTransactional: none of the three migrations opts out of goose's per-file
// transaction, the premise of the recovery claim (each file and its version row commit together).
// opDownLocks is, per migration, the tables its Down locks in ACCESS EXCLUSIVE MODE, in order.
var opDownLocks = map[string][]string{
	opModeMigration:     {"model_providers"},
	opValidateMigration: {"model_providers"},
	opKeysMigration:     {"model_providers", "model_provider_revisions", "model_provider_operator_keys", "model_provider_operator_key_test_results"},
}

func TestOperatorKeyMigrationsAreTransactional(t *testing.T) {
	for _, name := range []string{opModeMigration, opValidateMigration, opKeysMigration} {
		m := operatorMigration(t, name)
		path := fmt.Sprintf("migrations/%05d_%s.sql", m.version, name)
		raw, err := os.ReadFile(path) //nolint:gosec // G304: a path built from this package's own embedded migrations dir and a constant name, read-only.
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
					t.Errorf("%s locks a table in a Down in a mode other than ACCESS EXCLUSIVE: %s", path, line)
				} else {
					locked = append(locked, strings.TrimSuffix(strings.TrimPrefix(line, "LOCK TABLE "), " IN ACCESS EXCLUSIVE MODE;"))
					lastLock = offset
				}
			}
			offset += len(line) + 1
		}
		// The Down locks every table its guard reads before the guard runs, in the plan's order.
		if want := opDownLocks[name]; !slices.Equal(locked, want) {
			t.Errorf("%s Down locks %v in ACCESS EXCLUSIVE MODE, want %v", path, locked, want)
		}
		if guard := strings.Index(down, "DO $$"); guard < 0 || lastLock > guard {
			t.Errorf("%s Down has a LOCK TABLE after its first DO $$ guard (or no guard)", path)
		}
	}
}
