package secretopen

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"gitlab.example.com/vtmocanu/uzi/api/internal/secretbox"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
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
