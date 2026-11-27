package vault

import (
	"context"
	"errors"
	"fmt"
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
	user   uuid.UUID
	varID  uuid.UUID
	rev    int32
	v2     bool // sealed under the version-2 context; false is version 1
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

func (r envFixtureRow) aadVersion() int16 {
	if r.v2 {
		return runenv.AADVersion2
	}
	return runenv.AADVersion1
}

func (r envFixtureRow) kind() string {
	k, err := runenv.AADFor(r.aadVersion(), r.user, r.scope(), r.repo, r.secret, r.name, r.varID, r.rev)
	if err != nil {
		panic(err)
	}
	return k
}

// seedEnv seals each row with the real seam and stages it in the fake.
func seedEnv(t *testing.T, v *Vault, st *fakeVaultStore, uid uuid.UUID, rows []envFixtureRow) []envFixtureRow {
	t.Helper()
	for i := range rows {
		rows[i].user, rows[i].varID, rows[i].rev = uid, uuid.New(), int32(i+1)
		rows[i].v2 = i%2 == 1 // a mix of both contexts
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
			secret: rows[i].secret, ciphertext: m.Ciphertext, sealedWith: m.SealedWith,
			varID: rows[i].varID, rev: rows[i].rev, aadVersion: rows[i].aadVersion()})
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

// The lock-order fence must come first in convertMaterial: the connection fence, then the
// repo fence, both before the first measurement of the owner's material and before any
// material row lock (the real ordering is exercised against PostgreSQL in the store and
// handler LiveDB suites; this pins that convertMaterial asks for it).
func TestConvertMaterialTakesEnvFenceBeforeMeasuringOrLocking(t *testing.T) {
	v, _, st := newTestVaultWithStore(t)
	uid := uuid.New()
	st.states[uid] = store.LockVaultAuthStateRow{VaultMode: store.VaultModeUnprotected, VaultEpoch: 2}
	seedEnv(t, v, st, uid, fixtureEnvRows())
	st.resetCalls()
	if err := v.Enable(context.Background(), uid, "current-login-password"); err != nil {
		t.Fatal(err)
	}
	calls := st.callLog()
	first := func(names ...string) int {
		for i, c := range calls {
			for _, n := range names {
				if c == n {
					return i
				}
			}
		}
		t.Fatalf("none of %v was called: %v", names, calls)
		return -1
	}
	conns := first("LockVaultEnvConnectionsKeyShare")
	repos := first("LockVaultEnvReposKeyShare")
	totals := first("GetVaultMaterialTotals")
	batch := first("LockVaultAccountMaterialBatch", "LockVaultSecretMaterialBatch", "LockVaultEnvMaterialBatch")
	if conns >= repos || repos >= totals || totals >= batch {
		t.Fatalf("want connections < repos < totals < first material batch lock, got %d, %d, %d, %d in %v", conns, repos, totals, batch, calls)
	}
}

func TestEnvMaterialCountsTowardConversionLimit(t *testing.T) {
	v, _, st := newTestVaultWithStore(t)
	uid := uuid.New()
	st.states[uid] = store.LockVaultAuthStateRow{VaultMode: store.VaultModeUnprotected, VaultEpoch: 2}
	repo := pgtype.UUID{Bytes: uuid.New(), Valid: true}
	for i := int64(0); i <= maxMaterialRows; i++ {
		st.envs = append(st.envs, &fakeEnv{id: uuid.New(), userID: uid, repoID: repo, name: "X", ciphertext: []byte{1}, sealedWith: store.SealedWithMasterAAD, aadVersion: 1})
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

// An env row marked sealed_with=master (nil AAD) never exists (the table CHECK); a
// conversion that meets one refuses instead of opening it without its binding.
func TestEnvMaterialConversionRefusesMasterMarkedRow(t *testing.T) {
	v, _, st := newTestVaultWithStore(t)
	uid := uuid.New()
	st.states[uid] = store.LockVaultAuthStateRow{VaultMode: store.VaultModeUnprotected, VaultEpoch: 2}
	repo := pgtype.UUID{Bytes: uuid.New(), Valid: true}
	st.envs = append(st.envs, &fakeEnv{id: uuid.New(), userID: uid, repoID: repo, name: "LEGACY", ciphertext: []byte{1}, sealedWith: store.SealedWithMaster, aadVersion: 1})
	err := v.Enable(context.Background(), uid, "current-login-password")
	if !errors.Is(err, ErrInvalidState) {
		t.Fatalf("Enable with a master-marked env row = %v; want ErrInvalidState", err)
	}
	if got := st.state(uid); got.VaultMode != store.VaultModeUnprotected || got.VaultEpoch != 2 {
		t.Fatalf("a refused conversion changed state: %+v", got)
	}
}

func TestCheckMaterialHeadroomBoundary(t *testing.T) {
	at := store.GetVaultMaterialTotalsRow{MaterialRows: maxMaterialRows - 1, CiphertextBytes: maxMaterialBytes - 100}
	if err := CheckMaterialHeadroom(at, 1, 100, 100); err != nil {
		t.Fatalf("exactly at the limit must pass: %v", err)
	}
	var limit *ConversionLimitError
	if err := CheckMaterialHeadroom(at, 2, 100, 100); !errors.As(err, &limit) {
		t.Fatalf("one row past the limit = %v", err)
	}
	if err := CheckMaterialHeadroom(at, 1, 101, 101); !errors.As(err, &limit) {
		t.Fatalf("one byte past the limit = %v", err)
	}
}

// The per-row batch limit applies to the size of the row written, not to the change in
// total bytes: a replace that barely grows a row already over the limit is refused, and
// a shrinking replace of a row inside the limit passes.
func TestCheckMaterialHeadroomPerRowLimitUsesRowSize(t *testing.T) {
	totals := store.GetVaultMaterialTotalsRow{MaterialRows: 5, CiphertextBytes: 3 * maxBatchBytes, MaxRowBytes: 10}
	var limit *ConversionLimitError
	if err := CheckMaterialHeadroom(totals, 0, 1, maxBatchBytes+1); !errors.As(err, &limit) {
		t.Fatalf("a row above the batch limit with a +1 byte delta = %v; want a ConversionLimitError", err)
	}
	if err := CheckMaterialHeadroom(totals, 0, -50, 1000); err != nil {
		t.Fatalf("a shrinking replace of an in-limit row = %v; want nil", err)
	}
}

// A repo delete cascades its env rows without the secret-mutation lock, so a row counted
// by the first measurement can be gone before its batch is locked. The conversion must
// settle on the fresh total instead of failing.
func TestEnvMaterialConversionToleratesConcurrentEnvDelete(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(*Vault, context.Context, uuid.UUID) error
	}{
		{"enable", func(v *Vault, ctx context.Context, uid uuid.UUID) error {
			return v.Enable(ctx, uid, "current-login-password")
		}},
		{"disable", func(v *Vault, ctx context.Context, uid uuid.UUID) error {
			return v.Disable(ctx, uid)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, _, st := newTestVaultWithStore(t)
			uid := uuid.New()
			ctx := context.Background()
			st.states[uid] = store.LockVaultAuthStateRow{VaultMode: store.VaultModeUnprotected, VaultEpoch: 2}
			rows := seedEnv(t, v, st, uid, fixtureEnvRows())
			if tc.name == "disable" {
				if err := v.Enable(ctx, uid, "current-login-password"); err != nil {
					t.Fatal(err)
				}
			}
			gone := rows[len(rows)-1].id // the delete lands before any env batch is locked
			hs := &totalsHookStore{fakeVaultStore: st}
			hs.after = func() {
				st.mu.Lock()
				defer st.mu.Unlock()
				kept := st.envs[:0:0]
				for _, e := range st.envs {
					if e.id != gone {
						kept = append(kept, e)
					}
				}
				st.envs = kept
			}
			v.transact = st.transaction(hs)
			if err := tc.run(v, ctx, uid); err != nil {
				t.Fatalf("%s with an env row deleted mid-conversion = %v; want success", tc.name, err)
			}
			if envRowByID(st, gone) != nil {
				t.Fatal("the deleted env row came back")
			}
			for _, r := range rows[:len(rows)-1] {
				e := envRowByID(st, r.id)
				if e == nil {
					t.Fatalf("%s vanished", r.name)
				}
				wantMarker := store.SealedWithDEK
				if tc.name == "disable" {
					wantMarker = store.SealedWithMasterAAD
				}
				if e.sealedWith != wantMarker {
					t.Fatalf("%s sealed_with = %s; want %s", r.name, e.sealedWith, wantMarker)
				}
			}
		})
	}
}

// A repo delete can cascade a whole candidate page between the unlocked listing and the
// row lock, leaving nothing locked for that page while later env rows still exist. The
// scan must advance past the vanished candidates, not stop at the first empty lock.
func TestEnvMaterialConversionSurvivesWholePageVanishing(t *testing.T) {
	v, _, st := newTestVaultWithStore(t)
	uid := uuid.New()
	ctx := context.Background()
	st.states[uid] = store.LockVaultAuthStateRow{VaultMode: store.VaultModeUnprotected, VaultEpoch: 2}
	var fixture []envFixtureRow
	for i := 0; i < int(maxBatchRows)+6; i++ { // more than one page
		fixture = append(fixture, envFixtureRow{name: fmt.Sprintf("BULK_VAR_%03d", i), secret: true, plain: fmt.Sprintf("value-%03d", i)})
	}
	rows := seedEnv(t, v, st, uid, fixture)

	vanished := map[uuid.UUID]bool{}
	var lastVanished uuid.UUID
	hs := &candidateHookStore{fakeVaultStore: st}
	hs.after = func(page []uuid.UUID) { // the cascade lands after the first page was listed
		lastVanished = page[len(page)-1]
		st.mu.Lock()
		defer st.mu.Unlock()
		for _, id := range page {
			vanished[id] = true
		}
		kept := st.envs[:0:0]
		for _, e := range st.envs {
			if !vanished[e.id] {
				kept = append(kept, e)
			}
		}
		st.envs = kept
	}
	v.transact = st.transaction(hs)
	if err := v.Enable(ctx, uid, "current-login-password"); err != nil {
		t.Fatalf("Enable with a whole env page deleted before its lock = %v; want success", err)
	}
	if len(vanished) != int(maxBatchRows) {
		t.Fatalf("hook removed %d rows; want one full page of %d", len(vanished), maxBatchRows)
	}
	// The listing right after the vanished page must resume from that page's last listed
	// id; nothing was locked for the page, so a cursor that followed locked rows would not.
	if len(hs.afterIDs) < 2 {
		t.Fatalf("listing calls = %d; want at least 2", len(hs.afterIDs))
	}
	if got := hs.afterIDs[1]; !got.Valid || got.Bytes != lastVanished {
		t.Fatalf("second listing AfterID = %+v; want the vanished page's last id %s", got, lastVanished)
	}
	resealed := 0
	for _, r := range rows {
		e := envRowByID(st, r.id)
		if vanished[r.id] {
			if e != nil {
				t.Fatalf("%s came back after its delete", r.name)
			}
			continue
		}
		resealed++
		if e == nil || e.sealedWith != store.SealedWithDEK {
			t.Fatalf("%s after the vanished page was not resealed to the DEK: %+v", r.name, e)
		}
	}
	if resealed != 6 {
		t.Fatalf("resealed %d rows after the vanished page; want 6", resealed)
	}
}

// A total that still differs from what the conversion visited after the fresh read is
// not a repo delete: the conversion fails closed.
func TestEnvMaterialConversionFailsWhenFreshTotalsStillDiffer(t *testing.T) {
	v, _, st := newTestVaultWithStore(t)
	uid := uuid.New()
	st.states[uid] = store.LockVaultAuthStateRow{VaultMode: store.VaultModeUnprotected, VaultEpoch: 2}
	seedEnv(t, v, st, uid, fixtureEnvRows())
	hs := &totalsHookStore{fakeVaultStore: st, skewFresh: 1}
	hs.after = func() {
		st.mu.Lock()
		defer st.mu.Unlock()
		st.envs = st.envs[:len(st.envs)-1] // a delete makes progress != first totals
	}
	v.transact = st.transaction(hs)
	if err := v.Enable(context.Background(), uid, "current-login-password"); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("Enable with fresh totals above the visited rows = %v; want ErrInvalidState", err)
	}
	if got := st.state(uid); got.VaultMode != store.VaultModeUnprotected || got.VaultEpoch != 2 {
		t.Fatalf("a refused conversion changed state: %+v", got)
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
	st.envs = append(st.envs, &fakeEnv{id: uuid.New(), userID: other, name: "KEEP_ME", ciphertext: []byte{1}, sealedWith: store.SealedWithMasterAAD, aadVersion: 1})
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
