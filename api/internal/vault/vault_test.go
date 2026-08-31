package vault

import (
	"bytes"
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// fakeVaultStore is an in-memory Store so the vault's DB-touching paths (unlock,
// race-safe create, lazy rewrap) are exercised without a live database. It models
// the real semantics that matter: CreateUserVaultIfAbsent's DO NOTHING (returns
// pgx.ErrNoRows on conflict) and RewrapUserSecret's guard on sealed_with='master'.
//
// Secrets are a flat ROW LIST, not a kind-keyed map: since PRD #104 a user may
// hold several secrets of one kind, and a map keyed by kind would silently make
// that impossible to stage — hiding exactly the rewrap-clobber bug D10 fixes.
type fakeSecret struct {
	id         uuid.UUID
	userID     uuid.UUID
	kind       string
	ciphertext []byte
	sealedWith string
}

type fakeVaultStore struct {
	mu      sync.Mutex
	vaults  map[uuid.UUID]store.UserVault
	secrets []*fakeSecret

	// clearNoticeCalls records every ClearVaultLockNotice(userID) the unlock hook
	// fires (PRD #890 M1), so the unlock-hook test can assert it ran. clearNoticeErr,
	// when set, forces the clear to fail — proving the unlock still succeeds anyway.
	clearNoticeCalls []uuid.UUID
	clearNoticeErr   error
}

func newFakeVaultStore() *fakeVaultStore {
	return &fakeVaultStore{vaults: make(map[uuid.UUID]store.UserVault)}
}

// putSecret seeds a stored secret (used to stage a legacy master-sealed row) and
// returns its id, so a test can assert on one specific row of a kind.
func (f *fakeVaultStore) putSecret(userID uuid.UUID, kind string, ciphertext []byte, sealedWith string) uuid.UUID {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := &fakeSecret{id: uuid.New(), userID: userID, kind: kind, ciphertext: ciphertext, sealedWith: sealedWith}
	f.secrets = append(f.secrets, s)
	return s.id
}

// getSecret returns the user's single secret of a kind. It is only valid where the
// test staged exactly one; multi-row cases use getSecretByID.
func (f *fakeVaultStore) getSecret(userID uuid.UUID, kind string) (fakeSecret, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.secrets {
		if s.userID == userID && s.kind == kind {
			return *s, true
		}
	}
	return fakeSecret{}, false
}

func (f *fakeVaultStore) getSecretByID(id uuid.UUID) (fakeSecret, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.secrets {
		if s.id == id {
			return *s, true
		}
	}
	return fakeSecret{}, false
}

func (f *fakeVaultStore) GetUserVault(_ context.Context, userID uuid.UUID) (store.UserVault, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	uv, ok := f.vaults[userID]
	if !ok {
		return store.UserVault{}, pgx.ErrNoRows
	}
	return uv, nil
}

func (f *fakeVaultStore) CreateUserVaultIfAbsent(_ context.Context, arg store.CreateUserVaultIfAbsentParams) (store.UserVault, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.vaults[arg.UserID]; ok {
		return store.UserVault{}, pgx.ErrNoRows // ON CONFLICT DO NOTHING → no row
	}
	uv := store.UserVault{UserID: arg.UserID, KekSalt: arg.KekSalt, WrappedDek: arg.WrappedDek}
	f.vaults[arg.UserID] = uv
	return uv, nil
}

func (f *fakeVaultStore) ListMasterSealedSecrets(_ context.Context, userID uuid.UUID) ([]store.ListMasterSealedSecretsRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []store.ListMasterSealedSecretsRow{}
	for _, s := range f.secrets {
		if s.userID == userID && s.sealedWith == store.SealedWithMaster {
			out = append(out, store.ListMasterSealedSecretsRow{ID: s.id, Kind: s.kind, Ciphertext: s.ciphertext})
		}
	}
	return out, nil
}

// RewrapUserSecret models the real statement's predicate exactly —
// WHERE id = @id AND user_id = @user_id AND sealed_with = 'master' — including
// that it is a set-based UPDATE, so a predicate matching several rows would write
// all of them (which is how the pre-D10 by-kind form destroyed siblings).
func (f *fakeVaultStore) RewrapUserSecret(_ context.Context, arg store.RewrapUserSecretParams) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int64
	for _, s := range f.secrets {
		if s.id != arg.ID || s.userID != arg.UserID || s.sealedWith != store.SealedWithMaster {
			continue
		}
		s.ciphertext = arg.Ciphertext
		s.sealedWith = store.SealedWithDEK
		n++
	}
	return n, nil
}

func (f *fakeVaultStore) ClearVaultLockNotice(_ context.Context, userID uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clearNoticeCalls = append(f.clearNoticeCalls, userID)
	return f.clearNoticeErr
}

func newTestVault(t *testing.T) (*Vault, *secretbox.Box) {
	t.Helper()
	v, master, _ := newTestVaultWithStore(t)
	return v, master
}

func newTestVaultWithStore(t *testing.T) (*Vault, *secretbox.Box, *fakeVaultStore) {
	t.Helper()
	key := make([]byte, secretbox.KeySize)
	for i := range key {
		key[i] = byte(i) + 1 // non-placeholder; New only checks length
	}
	master, err := secretbox.New(key)
	if err != nil {
		t.Fatalf("secretbox.New master: %v", err)
	}
	st := newFakeVaultStore()
	return New(master, st), master, st
}

const testKind = store.KindAnthropicToken

// TestUnlockSealOpenRoundTrip: create the vault on first unlock, seal a secret,
// lock, unlock again with the same password, and recover the exact plaintext.
func TestUnlockSealOpenRoundTrip(t *testing.T) {
	v, _ := newTestVault(t)
	ctx := context.Background()
	uid := uuid.New()
	const pw = "correct horse battery"

	if err := v.Unlock(ctx, uid, pw); err != nil {
		t.Fatalf("first Unlock (create): %v", err)
	}
	if !v.Unlocked(uid) {
		t.Fatal("vault should be unlocked after Unlock")
	}

	secret := []byte("sk-ant-oat-deadbeef")
	sealed, err := v.Seal(uid, testKind, secret)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if bytes.Contains(sealed, secret) {
		t.Fatal("sealed output contains the plaintext")
	}

	v.Lock(uid)
	if v.Unlocked(uid) {
		t.Fatal("vault should be locked after Lock")
	}
	if _, err := v.Seal(uid, testKind, secret); err != ErrLocked {
		t.Fatalf("Seal while locked: got %v, want ErrLocked", err)
	}
	if _, err := v.Open(uid, testKind, store.SealedWithDEK, sealed); err != ErrLocked {
		t.Fatalf("Open dek while locked: got %v, want ErrLocked", err)
	}

	if err := v.Unlock(ctx, uid, pw); err != nil {
		t.Fatalf("re-Unlock: %v", err)
	}
	opened, err := v.Open(uid, testKind, store.SealedWithDEK, sealed)
	if err != nil {
		t.Fatalf("Open after re-unlock: %v", err)
	}
	if !bytes.Equal(opened, secret) {
		t.Fatalf("round trip mismatch: got %q want %q", opened, secret)
	}
}

// TestWrongPasswordFailsGCMAuth: a wrong password must fail to unwrap the DEK
// (GCM auth failure ⇒ ErrWrongPassword) and leave the vault locked.
func TestWrongPasswordFailsGCMAuth(t *testing.T) {
	v, _ := newTestVault(t)
	ctx := context.Background()
	uid := uuid.New()

	if err := v.Unlock(ctx, uid, "the-real-password"); err != nil {
		t.Fatalf("create Unlock: %v", err)
	}
	v.Lock(uid)

	if err := v.Unlock(ctx, uid, "not-the-password"); err != ErrWrongPassword {
		t.Fatalf("wrong password: got %v, want ErrWrongPassword", err)
	}
	if v.Unlocked(uid) {
		t.Fatal("wrong password must not unlock the vault")
	}
}

// TestMasterKeyCannotOpenDEKSealed: the whole point of the vault — a secret
// sealed under the DEK must not decrypt under the process master key, even though
// both use the same AES-GCM construction.
func TestMasterKeyCannotOpenDEKSealed(t *testing.T) {
	v, master := newTestVault(t)
	ctx := context.Background()
	uid := uuid.New()

	if err := v.Unlock(ctx, uid, "password-for-dek"); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	sealed, err := v.Seal(uid, testKind, []byte("dek-only secret"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if _, err := master.Open(sealed); err == nil {
		t.Fatal("master key opened a DEK-sealed ciphertext — vault provides no isolation")
	}
}

// TestAADMismatchFails: a DEK-sealed secret is bound to user_id||kind; opening it
// under a different kind or a different owner must fail GCM authentication.
func TestAADMismatchFails(t *testing.T) {
	v, _ := newTestVault(t)
	ctx := context.Background()
	uid := uuid.New()

	if err := v.Unlock(ctx, uid, "aad-binding-pw"); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	sealed, err := v.Seal(uid, testKind, []byte("bound secret"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	if _, err := v.Open(uid, "other_kind", store.SealedWithDEK, sealed); err == nil {
		t.Fatal("Open succeeded with a mismatched kind AAD")
	}

	// A different owner: unlock a second user, then try to open user 1's ciphertext
	// as if it were user 2's (a row-swap by a DB-write operator).
	other := uuid.New()
	if err := v.Unlock(ctx, other, "second-user-pw"); err != nil {
		t.Fatalf("Unlock other: %v", err)
	}
	if _, err := v.Open(other, testKind, store.SealedWithDEK, sealed); err == nil {
		t.Fatal("Open succeeded under a different owner's DEK/AAD")
	}
}

// TestOpenMasterRow: a legacy master-sealed secret opens via Open(...,'master')
// using the process key, with no unlock required.
func TestOpenMasterRow(t *testing.T) {
	v, master := newTestVault(t)
	uid := uuid.New()

	plaintext := []byte("legacy master-sealed token")
	sealed, err := master.Seal(plaintext) // nil AAD, exactly how legacy rows were written
	if err != nil {
		t.Fatalf("master.Seal: %v", err)
	}
	if v.Unlocked(uid) {
		t.Fatal("precondition: user should be locked")
	}
	opened, err := v.Open(uid, testKind, store.SealedWithMaster, sealed)
	if err != nil {
		t.Fatalf("Open master row while locked: %v", err)
	}
	if !bytes.Equal(opened, plaintext) {
		t.Fatalf("master row mismatch: got %q want %q", opened, plaintext)
	}
}

// TestUnknownSealedWith guards the enum: an unrecognized sealed_with is an error,
// never a silent master-open.
func TestUnknownSealedWith(t *testing.T) {
	v, _ := newTestVault(t)
	if _, err := v.Open(uuid.New(), testKind, "bogus", []byte("x")); err == nil {
		t.Fatal("expected error for unknown sealed_with")
	}
}

// TestUnlockExistingNeverCreates: the interactive unlock path returns ErrNoVault
// for a user with no vault row and does NOT mint one — otherwise any password
// would create a vault and lock the user out of their real secrets.
func TestUnlockExistingNeverCreates(t *testing.T) {
	v, _, st := newTestVaultWithStore(t)
	uid := uuid.New()

	if err := v.UnlockExisting(context.Background(), uid, "whatever"); err != ErrNoVault {
		t.Fatalf("UnlockExisting with no vault: got %v, want ErrNoVault", err)
	}
	if v.Unlocked(uid) {
		t.Fatal("UnlockExisting must not unlock when there is no vault")
	}
	if _, ok := st.vaults[uid]; ok {
		t.Fatal("UnlockExisting created a vault row — it must never create")
	}
}

// TestLazyRewrapOnUnlock: a legacy master-sealed secret is resealed under the DEK
// and flipped to 'dek' on unlock, then opens via the DEK path.
func TestLazyRewrapOnUnlock(t *testing.T) {
	v, master, st := newTestVaultWithStore(t)
	ctx := context.Background()
	uid := uuid.New()

	// Stage a legacy row: master-box-sealed (nil AAD), sealed_with='master'.
	token := []byte("legacy-token-to-migrate")
	legacy, err := master.Seal(token)
	if err != nil {
		t.Fatalf("master.Seal: %v", err)
	}
	st.putSecret(uid, testKind, legacy, store.SealedWithMaster)

	if err := v.Unlock(ctx, uid, "unlock-pw"); err != nil {
		t.Fatalf("Unlock: %v", err)
	}

	got, ok := st.getSecret(uid, testKind)
	if !ok {
		t.Fatal("secret vanished during rewrap")
	}
	if got.sealedWith != store.SealedWithDEK {
		t.Fatalf("sealed_with after rewrap = %q, want %q", got.sealedWith, store.SealedWithDEK)
	}
	// The rewritten ciphertext must open under the DEK (with AAD) and no longer
	// under the master key.
	opened, err := v.Open(uid, testKind, store.SealedWithDEK, got.ciphertext)
	if err != nil {
		t.Fatalf("Open rewrapped secret: %v", err)
	}
	if !bytes.Equal(opened, token) {
		t.Fatalf("rewrapped plaintext mismatch: got %q want %q", opened, token)
	}
	if _, err := master.Open(got.ciphertext); err == nil {
		t.Fatal("rewrapped ciphertext still opens under the master key")
	}
	// A second unlock finds nothing left to rewrap (idempotent).
	if rows, _ := st.ListMasterSealedSecrets(ctx, uid); len(rows) != 0 {
		t.Fatalf("still %d master-sealed rows after rewrap", len(rows))
	}
}

// TestRewrapPreservesSiblingSecretsOfSameKind is the PRD #104 D10 regression gate.
//
// Before the fix, RewrapUserSecret was keyed on (user_id, kind, sealed_with='master')
// and ListMasterSealedSecrets returned no id, so the rewrap loop's FIRST iteration
// resealed token 1 and wrote it over EVERY master-sealed row of that kind. The
// remaining iterations then matched nothing (the siblings were already 'dek') and
// the loop finished without a single error: tokens 2..N were silently replaced by a
// copy of token 1. It is reachable in a supported configuration — a vault-nil
// deployment seals everything 'master', and PRD #32's documented upgrade is to
// enable the vault later, at which point the owner's first unlock runs this loop.
//
// The gate: two master-sealed rows of ONE kind with DIFFERENT plaintexts must both
// survive an unlock with their own original plaintext. Asserting on the plaintexts
// (not just on distinct ciphertexts) is what makes the clobber unmissable — the
// buggy path leaves two rows that both open, but to the SAME secret.
//
// If you are bisecting and landed on the commit that introduced this test: yes, it
// stages a shape the live schema still forbids here. `UNIQUE (user_id, kind)` does
// not drop until migration 00077 in the NEXT commit, so at this commit no database
// would accept these two rows. That is deliberate and harmless — a fake store is a
// model, and this one models the post-00077 world so the correctness fix can land
// and be proven before the schema change that makes it reachable. The test that
// runs against real SQL is TestUserSecretsRewrapLiveDB, which arrives with the
// migration.
//
// This test is also NOT independently falsifiable: reverting the production fix
// changes the generated types, so a pre-fix tree fails to COMPILE here rather than
// going red. Its job is to prove vault.go threads the id through the real
// rewrapMasterSecrets loop; the evidentiary weight for the bug itself belongs to
// the live-DB test.
func TestRewrapPreservesSiblingSecretsOfSameKind(t *testing.T) {
	v, master, st := newTestVaultWithStore(t)
	ctx := context.Background()
	uid := uuid.New()

	// Two legacy rows of the same kind, distinct plaintexts (a Max subscription and
	// a console key, in the motivating scenario).
	first := []byte("token-one-subscription")
	second := []byte("token-two-console-key")
	sealFirst, err := master.Seal(first)
	if err != nil {
		t.Fatalf("master.Seal first: %v", err)
	}
	sealSecond, err := master.Seal(second)
	if err != nil {
		t.Fatalf("master.Seal second: %v", err)
	}
	idFirst := st.putSecret(uid, testKind, sealFirst, store.SealedWithMaster)
	idSecond := st.putSecret(uid, testKind, sealSecond, store.SealedWithMaster)

	if err := v.Unlock(ctx, uid, "rewrap-two-rows-pw"); err != nil {
		t.Fatalf("Unlock: %v", err)
	}

	for _, tc := range []struct {
		name string
		id   uuid.UUID
		want []byte
	}{
		{"first", idFirst, first},
		{"second", idSecond, second},
	} {
		row, ok := st.getSecretByID(tc.id)
		if !ok {
			t.Fatalf("%s row vanished during rewrap", tc.name)
		}
		if row.sealedWith != store.SealedWithDEK {
			t.Fatalf("%s row sealed_with = %q, want %q (it was never rewrapped)", tc.name, row.sealedWith, store.SealedWithDEK)
		}
		opened, oerr := v.Open(uid, testKind, store.SealedWithDEK, row.ciphertext)
		if oerr != nil {
			t.Fatalf("Open %s row after rewrap: %v", tc.name, oerr)
		}
		if !bytes.Equal(opened, tc.want) {
			t.Fatalf("%s row now holds %q, want %q — a sibling secret was overwritten by the rewrap", tc.name, opened, tc.want)
		}
	}
}

// TestConcurrentFirstUnlockConsistent: N concurrent first-unlocks for the same
// user (same password) must converge on ONE vault row and ONE DEK — the cached
// DEK must equal the persisted one. Verified by sealing after the race, then
// re-unlocking from the persisted row and opening: a cache/DB DEK divergence
// (the check-then-act bug) would fail the open.
func TestConcurrentFirstUnlockConsistent(t *testing.T) {
	v, _, st := newTestVaultWithStore(t)
	ctx := context.Background()
	uid := uuid.New()
	const pw = "shared-first-unlock-pw"

	const n = 8
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			if err := v.Unlock(ctx, uid, pw); err != nil {
				t.Errorf("concurrent Unlock: %v", err)
			}
		}()
	}
	wg.Wait()

	if len(st.vaults) != 1 {
		t.Fatalf("expected exactly one vault row, got %d", len(st.vaults))
	}
	if !v.Unlocked(uid) {
		t.Fatal("vault should be unlocked after the race")
	}

	// Seal with the cached DEK, then Lock + UnlockExisting (which unwraps the
	// PERSISTED row) and open: proves cache DEK == persisted DEK.
	secret := []byte("post-race-secret")
	sealed, err := v.Seal(uid, testKind, secret)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	v.Lock(uid)
	if err := v.UnlockExisting(ctx, uid, pw); err != nil {
		t.Fatalf("UnlockExisting from persisted row: %v", err)
	}
	opened, err := v.Open(uid, testKind, store.SealedWithDEK, sealed)
	if err != nil {
		t.Fatalf("Open after re-unlock (cache/DB DEK diverged?): %v", err)
	}
	if !bytes.Equal(opened, secret) {
		t.Fatalf("mismatch after race: got %q want %q", opened, secret)
	}
}
