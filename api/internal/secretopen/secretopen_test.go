package secretopen

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"gitlab.example.com/vtmocanu/uzi/api/internal/secretbox"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
	"gitlab.example.com/vtmocanu/uzi/api/internal/vault"
)

type fakeStore struct {
	row store.GetUserSecretCiphertextRow
	err error
}

func (f fakeStore) GetUserSecretCiphertext(context.Context, store.GetUserSecretCiphertextParams) (store.GetUserSecretCiphertextRow, error) {
	return f.row, f.err
}

// key is a valid 32-byte AES key for a real master box (nil-vault path).
var key = []byte("0123456789abcdef0123456789abcdef")

func TestOpenNoSecret(t *testing.T) {
	box, err := secretbox.New(key)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Open(context.Background(), fakeStore{err: pgx.ErrNoRows}, nil, box, uuid.New(), "anthropic_token")
	if !errors.Is(err, ErrNoSecret) {
		t.Fatalf("want ErrNoSecret, got %v", err)
	}
}

func TestOpenLookupError(t *testing.T) {
	box, _ := secretbox.New(key)
	sentinel := errors.New("db down")
	_, err := Open(context.Background(), fakeStore{err: sentinel}, nil, box, uuid.New(), "anthropic_token")
	if errors.Is(err, ErrNoSecret) || errors.Is(err, ErrUndecryptable) {
		t.Fatalf("a non-ErrNoRows lookup error must not collapse to a credential sentinel: %v", err)
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("lookup error should wrap the underlying error, got %v", err)
	}
}

func TestOpenUndecryptable(t *testing.T) {
	box, _ := secretbox.New(key)
	// Ciphertext that the master box cannot open.
	row := store.GetUserSecretCiphertextRow{Ciphertext: []byte("not a valid sealed blob"), SealedWith: store.SealedWithMaster}
	_, err := Open(context.Background(), fakeStore{row: row}, nil, box, uuid.New(), "anthropic_token")
	if !errors.Is(err, ErrUndecryptable) {
		t.Fatalf("want ErrUndecryptable, got %v", err)
	}
}

func TestOpenSealed(t *testing.T) {
	box, _ := secretbox.New(key)
	sealed, err := box.Seal([]byte("s3cr3t-token"))
	if err != nil {
		t.Fatal(err)
	}
	// Round-trips a master-sealed row with no vault (the tick path).
	plain, err := OpenSealed(nil, box, uuid.New(), "anthropic_token", store.SealedWithMaster, sealed)
	if err != nil {
		t.Fatal(err)
	}
	if string(plain) != "s3cr3t-token" {
		t.Fatalf("plaintext = %q, want s3cr3t-token", plain)
	}

	// A bad ciphertext returns ErrUndecryptable, and the ciphertext bytes must not
	// leak into the error string (auditor: ciphertext out of every error).
	bad := []byte("BADCIPHERTEXT-sentinel-bytes")
	_, err = OpenSealed(nil, box, uuid.New(), "anthropic_token", store.SealedWithMaster, bad)
	if !errors.Is(err, ErrUndecryptable) {
		t.Fatalf("want ErrUndecryptable, got %v", err)
	}
	if strings.Contains(err.Error(), string(bad)) {
		t.Fatalf("error leaked the ciphertext: %q", err.Error())
	}
}

// TestOpenSealedRealVaultLocked exercises the D3 dispatch through a REAL
// *vault.Vault whose cache is empty (i.e. the user is locked), not the nil-vault
// shortcut: a master-sealed secret opens regardless of the lock, while a
// dek-sealed one surfaces ErrVaultLocked. vault.Open never touches the store for
// either path, so a nil store is sufficient.
func TestOpenSealedRealVaultLocked(t *testing.T) {
	box, _ := secretbox.New(key)
	vlt := vault.New(box, nil) // empty DEK cache ⇒ every user is locked
	userID := uuid.New()
	if vlt.Unlocked(userID) {
		t.Fatal("a fresh vault must report the user as locked")
	}

	// Master-sealed: opens under the master box even though the vault is locked.
	sealed, err := box.Seal([]byte("legacy-token"))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := OpenSealed(vlt, box, userID, "anthropic_token", store.SealedWithMaster, sealed)
	if err != nil {
		t.Fatalf("master-sealed must open while locked, got %v", err)
	}
	if string(plain) != "legacy-token" {
		t.Fatalf("plaintext = %q, want legacy-token", plain)
	}

	// Dek-sealed: the locked vault refuses before any decryption, so the ciphertext
	// value is irrelevant — the lock is what gates it.
	_, err = OpenSealed(vlt, box, userID, "anthropic_token", store.SealedWithDEK, []byte("irrelevant"))
	if !errors.Is(err, ErrVaultLocked) {
		t.Fatalf("dek-sealed while locked must return ErrVaultLocked, got %v", err)
	}
}

func TestOpenRoundTripNilVault(t *testing.T) {
	box, _ := secretbox.New(key)
	sealed, err := box.Seal([]byte("s3cr3t-token"))
	if err != nil {
		t.Fatal(err)
	}
	row := store.GetUserSecretCiphertextRow{Ciphertext: sealed, SealedWith: store.SealedWithMaster}
	plain, err := Open(context.Background(), fakeStore{row: row}, nil, box, uuid.New(), "anthropic_token")
	if err != nil {
		t.Fatal(err)
	}
	if string(plain) != "s3cr3t-token" {
		t.Fatalf("plaintext = %q, want s3cr3t-token", plain)
	}
}
