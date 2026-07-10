package vault

import (
	"bytes"
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"gitlab.example.com/vtmocanu/uzi/api/internal/secretbox"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// fakeVaultStore is an in-memory Store so the vault's DB-touching paths (unlock,
// create-on-first-unlock) are exercised without a live database.
type fakeVaultStore struct {
	mu     sync.Mutex
	vaults map[uuid.UUID]store.UserVault
}

func newFakeVaultStore() *fakeVaultStore {
	return &fakeVaultStore{vaults: make(map[uuid.UUID]store.UserVault)}
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

func (f *fakeVaultStore) UpsertUserVault(_ context.Context, arg store.UpsertUserVaultParams) (store.UserVault, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	uv := store.UserVault{UserID: arg.UserID, KekSalt: arg.KekSalt, WrappedDek: arg.WrappedDek}
	f.vaults[arg.UserID] = uv
	return uv, nil
}

func newTestVault(t *testing.T) (*Vault, *secretbox.Box) {
	t.Helper()
	key := make([]byte, secretbox.KeySize)
	for i := range key {
		key[i] = byte(i) + 1 // non-placeholder; New only checks length
	}
	master, err := secretbox.New(key)
	if err != nil {
		t.Fatalf("secretbox.New master: %v", err)
	}
	return New(master, newFakeVaultStore()), master
}

const testKind = "anthropic_token"

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
	if _, err := v.Open(uid, testKind, SealedWithDEK, sealed); err != ErrLocked {
		t.Fatalf("Open dek while locked: got %v, want ErrLocked", err)
	}

	if err := v.Unlock(ctx, uid, pw); err != nil {
		t.Fatalf("re-Unlock: %v", err)
	}
	opened, err := v.Open(uid, testKind, SealedWithDEK, sealed)
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

	if _, err := v.Open(uid, "other_kind", SealedWithDEK, sealed); err == nil {
		t.Fatal("Open succeeded with a mismatched kind AAD")
	}

	// A different owner: unlock a second user, then try to open user 1's ciphertext
	// as if it were user 2's (a row-swap by a DB-write operator).
	other := uuid.New()
	if err := v.Unlock(ctx, other, "second-user-pw"); err != nil {
		t.Fatalf("Unlock other: %v", err)
	}
	if _, err := v.Open(other, testKind, SealedWithDEK, sealed); err == nil {
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
	opened, err := v.Open(uid, testKind, SealedWithMaster, sealed)
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
