package vault

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/runenv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #4416: run_env_vars rows are vault material. These tests drive the crypto
// fixture (no PostgreSQL); the SQL itself runs in the handler and store LiveDB suites.

type envFixtureRow struct {
	id     uuid.UUID
	repo   *uuid.UUID
	name   string
	secret bool
	plain  string
}

func (r envFixtureRow) scope() string {
	if r.repo == nil {
		return runenv.ScopeUser
	}
	return runenv.ScopeRepo
}

func (r envFixtureRow) kind() string { return runenv.AADKind(r.scope(), r.repo, r.secret, r.name) }

// seedEnv seals each row with the real seam and stages it in the fake.
func seedEnv(t *testing.T, v *Vault, st *fakeVaultStore, uid uuid.UUID, rows []envFixtureRow) []envFixtureRow {
	t.Helper()
	for i := range rows {
		m, err := v.Seal(context.Background(), uid, rows[i].kind(), []byte(rows[i].plain), SealForUse)
		if err != nil {
			t.Fatal(err)
		}
		rows[i].id = uuid.New()
		var repo pgtype.UUID
		if rows[i].repo != nil {
			repo = pgtype.UUID{Bytes: *rows[i].repo, Valid: true}
		}
		st.mu.Lock()
		st.envs = append(st.envs, &fakeEnv{id: rows[i].id, userID: uid, repoID: repo, name: rows[i].name,
			secret: rows[i].secret, ciphertext: m.Ciphertext, sealedWith: m.SealedWith})
		st.mu.Unlock()
	}
	return rows
}

func envRowByID(st *fakeVaultStore, id uuid.UUID) *fakeEnv {
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, e := range st.envs {
		if e.id == id {
			return e
		}
	}
	return nil
}

func fixtureEnvRows() []envFixtureRow {
	repoA, repoB := uuid.New(), uuid.New()
	return []envFixtureRow{
		{repo: &repoA, name: "PAYMENTS_SANDBOX_KEY", secret: true, plain: "sk_test_dummy_value_1"},
		{repo: &repoA, name: "PAYMENTS_SANDBOX_URL", secret: false, plain: "https://sandbox.example.test"},
		{repo: &repoB, name: "PAYMENTS_SANDBOX_KEY", secret: true, plain: "sk_test_dummy_value_2"},
		{repo: nil, name: "SHARED_TOKEN", secret: true, plain: "user-scope-secret-1"},
		{repo: &repoB, name: "EMPTY_PLAIN", secret: false, plain: ""},
	}
}

func TestEnvMaterialEnableDisableRoundTrip(t *testing.T) {
	v, _, st := newTestVaultWithStore(t)
	uid := uuid.New()
	ctx := context.Background()
	st.states[uid] = store.LockVaultAuthStateRow{VaultMode: store.VaultModeUnprotected, VaultEpoch: 2}
	rows := seedEnv(t, v, st, uid, fixtureEnvRows())

	if has, err := st.HasMasterVaultMaterial(ctx, uid); err != nil || !has {
		t.Fatalf("HasMasterVaultMaterial with only env rows = %v, %v; want true", has, err)
	}
	totals, err := st.GetVaultMaterialTotals(ctx, uid)
	if err != nil || totals.MaterialRows != int64(len(rows)) {
		t.Fatalf("totals = %+v, %v; want %d material rows from env rows alone", totals, err, len(rows))
	}

	if err := v.Enable(ctx, uid, "current-login-password"); err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		e := envRowByID(st, r.id)
		if e.sealedWith != store.SealedWithDEK {
			t.Fatalf("%s not converted to the DEK on enable (sealed_with=%s)", r.name, e.sealedWith)
		}
		got, err := v.OpenAtEpoch(ctx, uid, r.kind(), e.sealedWith, e.ciphertext, 3)
		if err != nil || string(got) != r.plain {
			t.Fatalf("%s after enable: %q, %v", r.name, got, err)
		}
	}
	if err := v.Disable(ctx, uid); err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		e := envRowByID(st, r.id)
		if e.sealedWith != store.SealedWithMasterAAD {
			t.Fatalf("%s not converted back on disable (sealed_with=%s)", r.name, e.sealedWith)
		}
		got, err := v.OpenAtEpoch(ctx, uid, r.kind(), e.sealedWith, e.ciphertext, 4)
		if err != nil || string(got) != r.plain {
			t.Fatalf("%s after disable: %q, %v", r.name, got, err)
		}
	}
}

func TestEnvMaterialCountsTowardConversionLimit(t *testing.T) {
	v, _, st := newTestVaultWithStore(t)
	uid := uuid.New()
	st.states[uid] = store.LockVaultAuthStateRow{VaultMode: store.VaultModeUnprotected, VaultEpoch: 2}
	repo := pgtype.UUID{Bytes: uuid.New(), Valid: true}
	for i := int64(0); i <= maxMaterialRows; i++ {
		st.envs = append(st.envs, &fakeEnv{id: uuid.New(), userID: uid, repoID: repo, name: "X", ciphertext: []byte{1}, sealedWith: store.SealedWithMasterAAD})
	}
	err := v.Enable(context.Background(), uid, "current-login-password")
	var limit *ConversionLimitError
	if !errors.As(err, &limit) || limit.Rows != maxMaterialRows+1 {
		t.Fatalf("Enable with %d env rows = %v; want a ConversionLimitError naming them", maxMaterialRows+1, err)
	}
	if got := st.state(uid); got.VaultMode != store.VaultModeUnprotected || got.VaultEpoch != 2 {
		t.Fatalf("a refused conversion changed state: %+v", got)
	}
}

func TestCheckMaterialHeadroomBoundary(t *testing.T) {
	at := store.GetVaultMaterialTotalsRow{MaterialRows: maxMaterialRows - 1, CiphertextBytes: maxMaterialBytes - 100}
	if err := CheckMaterialHeadroom(at, 1, 100); err != nil {
		t.Fatalf("exactly at the limit must pass: %v", err)
	}
	var limit *ConversionLimitError
	if err := CheckMaterialHeadroom(at, 2, 100); !errors.As(err, &limit) {
		t.Fatalf("one row past the limit = %v", err)
	}
	if err := CheckMaterialHeadroom(at, 1, 101); !errors.As(err, &limit) {
		t.Fatalf("one byte past the limit = %v", err)
	}
}

// A ciphertext sealed for one (scope, repo, secret flag, name) must not open under
// any other: the AAD binds all four.
func TestEnvCiphertextCannotMoveBetweenContexts(t *testing.T) {
	v, _, st := newTestVaultWithStore(t)
	uid := uuid.New()
	ctx := context.Background()
	st.states[uid] = store.LockVaultAuthStateRow{VaultMode: store.VaultModeUnprotected, VaultEpoch: 1}
	repoA, repoB := uuid.New(), uuid.New()
	base := envFixtureRow{repo: &repoA, name: "TOKEN_A", secret: true, plain: "a-secret-value"}
	m, err := v.Seal(ctx, uid, base.kind(), []byte(base.plain), SealForUse)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := v.Open(uid, base.kind(), m.SealedWith, m.Ciphertext); err != nil || string(got) != base.plain {
		t.Fatalf("control open failed: %q, %v", got, err)
	}
	cases := map[string]string{
		"another name":         runenv.AADKind(runenv.ScopeRepo, &repoA, true, "TOKEN_B"),
		"another repo":         runenv.AADKind(runenv.ScopeRepo, &repoB, true, "TOKEN_A"),
		"secret relabelled":    runenv.AADKind(runenv.ScopeRepo, &repoA, false, "TOKEN_A"),
		"user scope":           runenv.AADKind(runenv.ScopeUser, nil, true, "TOKEN_A"),
		"plain secret kind":    store.KindAnthropicToken,
		"name length smuggled": runenv.AADKind(runenv.ScopeRepo, &repoA, true, "TOKEN_A\x00"),
	}
	for name, kind := range cases {
		if got, err := v.Open(uid, kind, m.SealedWith, m.Ciphertext); err == nil {
			t.Errorf("%s: ciphertext opened under a foreign context: %q", name, got)
		}
	}
	other := uuid.New()
	if _, err := v.Open(other, base.kind(), m.SealedWith, m.Ciphertext); err == nil {
		t.Error("ciphertext opened for another owner")
	}
}

func TestResetDeletesEnvRows(t *testing.T) {
	v, st, uid, ctx := resetFixture(t)
	seedEnv(t, v, st, uid, fixtureEnvRows())
	other := uuid.New()
	st.envs = append(st.envs, &fakeEnv{id: uuid.New(), userID: other, name: "KEEP_ME", ciphertext: []byte{1}, sealedWith: store.SealedWithMasterAAD})
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := v.Reset(ctx, uid, noopReset); err != nil {
		t.Fatal(err)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, e := range st.envs {
		if e.userID == uid {
			t.Fatalf("env row %s survived reset", e.name)
		}
	}
	if len(st.envs) != 1 || st.envs[0].userID != other {
		t.Fatalf("reset touched another owner's env rows: %d left", len(st.envs))
	}
}
