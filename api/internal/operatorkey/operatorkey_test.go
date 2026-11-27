package operatorkey

import (
	"bytes"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/secretbox"
)

func testBox(t *testing.T) *secretbox.Box {
	t.Helper()
	key := make([]byte, secretbox.KeySize)
	for i := range key {
		key[i] = byte(i*7 + 3)
	}
	b, err := secretbox.New(key)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestAADPinsTheVersionedBinding(t *testing.T) {
	id := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	if got, want := string(AAD(id)), "uzi/provider-operator-key/v1|11111111-2222-3333-4444-555555555555"; got != want {
		t.Fatalf("AAD = %q, want %q", got, want)
	}
}

func TestSealOpenRoundTrip(t *testing.T) {
	box, id := testBox(t), uuid.New()
	plain := []byte("gateway-key-" + uuid.NewString())
	sealed, err := Seal(box, id, plain)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, plain) {
		t.Fatal("the sealed value contains the plaintext")
	}
	got, err := Open(box, id, sealed)
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("Open = %q, %v; want the plaintext", got, err)
	}
}

func TestOpenRefusesAnotherProvider(t *testing.T) {
	box := testBox(t)
	a, b := uuid.New(), uuid.New()
	sealedA, err := Seal(box, a, []byte("key-a"))
	if err != nil {
		t.Fatal(err)
	}
	sealedB, err := Seal(box, b, []byte("key-b"))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := Open(box, b, sealedA); err == nil {
		t.Fatalf("a ciphertext sealed for provider A opened for provider B: %q", got)
	}
	// Another provider's ciphertext (a swap between custody rows) refuses too.
	if got, err := Open(box, a, sealedB); err == nil {
		t.Fatalf("provider B's ciphertext opened for provider A: %q", got)
	}
	if got, err := Open(box, a, sealedA); err != nil || string(got) != "key-a" {
		t.Fatalf("control: Open for the sealing provider = %q, %v", got, err)
	}
}

func TestOpenRefusesTruncatedAndTamperedInput(t *testing.T) {
	box, id := testBox(t), uuid.New()
	sealed, err := Seal(box, id, []byte("key"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(box, id, sealed[:5]); !errors.Is(err, secretbox.ErrCiphertextTooShort) {
		t.Fatalf("truncated below a nonce and tag: %v, want ErrCiphertextTooShort", err)
	}
	if _, err := Open(box, id, sealed[:len(sealed)-1]); err == nil {
		t.Fatal("a ciphertext missing its last byte opened")
	}
	flipped := append([]byte(nil), sealed...)
	flipped[len(flipped)-1] ^= 0x01
	if _, err := Open(box, id, flipped); err == nil {
		t.Fatal("a tampered ciphertext opened")
	}
	if _, err := Open(box, id, nil); err == nil {
		t.Fatal("an empty value opened")
	}
}

func TestSealIsNotDeterministic(t *testing.T) {
	box, id := testBox(t), uuid.New()
	a, err := Seal(box, id, []byte("key"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := Seal(box, id, []byte("key"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(a, b) {
		t.Fatal("two seals of one value are identical")
	}
}
