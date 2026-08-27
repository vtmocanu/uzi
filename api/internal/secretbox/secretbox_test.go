package secretbox

import (
	"bytes"
	"encoding/base64"
	"errors"
	"testing"
)

// key32 is a deterministic, non-uniform 32-byte key for the crypto tests.
func key32(fill byte) []byte {
	k := make([]byte, KeySize)
	for i := range k {
		k[i] = fill + byte(i)
	}
	return k
}

func newBox(t *testing.T, fill byte) *Box {
	t.Helper()
	b, err := New(key32(fill))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return b
}

func TestSealOpenRoundTrip(t *testing.T) {
	b := newBox(t, 1)
	msg := []byte("connect the forge, not the forgery")

	sealed, err := b.Seal(msg)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if bytes.Contains(sealed, msg) {
		t.Fatal("sealed output leaks the plaintext bytes")
	}

	got, err := b.Open(sealed)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bytes.Equal(got, msg) {
		t.Fatalf("round-trip = %q, want %q", got, msg)
	}
}

func TestSealIsNonDeterministic(t *testing.T) {
	b := newBox(t, 2)
	msg := []byte("same message twice")

	first, err := b.Seal(msg)
	if err != nil {
		t.Fatalf("first Seal: %v", err)
	}
	second, err := b.Seal(msg)
	if err != nil {
		t.Fatalf("second Seal: %v", err)
	}
	if bytes.Equal(first, second) {
		t.Fatal("two seals of the same plaintext are identical; nonce is not random")
	}
}

func TestOpenRejectsTamper(t *testing.T) {
	b := newBox(t, 3)
	sealed, err := b.Seal([]byte("do not modify"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	// Flip a bit in the last byte (inside the tag) — any single-byte change must fail.
	sealed[len(sealed)-1] ^= 0x01
	if _, err := b.Open(sealed); err == nil {
		t.Fatal("Open accepted a tampered ciphertext")
	}
}

func TestOpenRejectsTooShort(t *testing.T) {
	b := newBox(t, 4)
	for _, n := range []int{0, 1, 7, 12, 27} {
		if _, err := b.Open(make([]byte, n)); !errors.Is(err, ErrCiphertextTooShort) {
			t.Errorf("Open(len=%d) error = %v, want ErrCiphertextTooShort", n, err)
		}
	}
}

func TestOpenRejectsWrongKey(t *testing.T) {
	sealer := newBox(t, 5)
	opener := newBox(t, 9) // a different key

	sealed, err := sealer.Seal([]byte("keyed to sealer only"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if _, err := opener.Open(sealed); err == nil {
		t.Fatal("a ciphertext opened under the wrong key")
	}
}

func TestAADRoundTripAndMismatch(t *testing.T) {
	b := newBox(t, 6)
	msg := []byte("bound to context")
	aad := []byte("user:42")

	sealed, err := b.SealWithAAD(msg, aad)
	if err != nil {
		t.Fatalf("SealWithAAD: %v", err)
	}

	got, err := b.OpenWithAAD(sealed, aad)
	if err != nil {
		t.Fatalf("OpenWithAAD with correct aad: %v", err)
	}
	if !bytes.Equal(got, msg) {
		t.Fatalf("aad round-trip = %q, want %q", got, msg)
	}

	if _, err := b.OpenWithAAD(sealed, []byte("user:43")); err == nil {
		t.Fatal("OpenWithAAD accepted the wrong aad")
	}
}

func TestNilAADInterchangeableWithSeal(t *testing.T) {
	b := newBox(t, 7)
	msg := []byte("nil aad both directions")

	// Seal (nil aad) then OpenWithAAD(nil).
	viaSeal, err := b.Seal(msg)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	got, err := b.OpenWithAAD(viaSeal, nil)
	if err != nil {
		t.Fatalf("OpenWithAAD(nil) of a Seal output: %v", err)
	}
	if !bytes.Equal(got, msg) {
		t.Fatalf("got %q, want %q", got, msg)
	}

	// SealWithAAD(nil) then plain Open.
	viaSealAAD, err := b.SealWithAAD(msg, nil)
	if err != nil {
		t.Fatalf("SealWithAAD(nil): %v", err)
	}
	got, err = b.Open(viaSealAAD)
	if err != nil {
		t.Fatalf("Open of a SealWithAAD(nil) output: %v", err)
	}
	if !bytes.Equal(got, msg) {
		t.Fatalf("got %q, want %q", got, msg)
	}
}

func TestNewRejectsBadKeyLength(t *testing.T) {
	for _, n := range []int{0, 1, 16, 31, 33, 64} {
		if _, err := New(make([]byte, n)); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("New(len=%d) error = %v, want ErrInvalidKey", n, err)
		}
	}
}

func TestLoadKey(t *testing.T) {
	const env = "SECRETBOX_TEST_KEY"

	good := key32(1)
	goodB64 := base64.StdEncoding.EncodeToString(good)

	t.Run("missing", func(t *testing.T) {
		t.Setenv(env, "")
		if _, err := LoadKey(env); err == nil {
			t.Fatal("empty env accepted")
		}
	})

	t.Run("bad base64", func(t *testing.T) {
		t.Setenv(env, "not*valid*base64")
		if _, err := LoadKey(env); err == nil {
			t.Fatal("invalid base64 accepted")
		}
	})

	t.Run("wrong length", func(t *testing.T) {
		t.Setenv(env, base64.StdEncoding.EncodeToString(make([]byte, 16)))
		if _, err := LoadKey(env); err == nil {
			t.Fatal("a 16-byte key was accepted")
		}
	})

	t.Run("all zero is weak", func(t *testing.T) {
		t.Setenv(env, base64.StdEncoding.EncodeToString(make([]byte, KeySize)))
		if _, err := LoadKey(env); !errors.Is(err, ErrWeakKey) {
			t.Fatalf("all-zero key error = %v, want ErrWeakKey", err)
		}
	})

	t.Run("repeated byte is weak", func(t *testing.T) {
		repeated := bytes.Repeat([]byte{0x7a}, KeySize)
		t.Setenv(env, base64.StdEncoding.EncodeToString(repeated))
		if _, err := LoadKey(env); !errors.Is(err, ErrWeakKey) {
			t.Fatalf("repeated-byte key error = %v, want ErrWeakKey", err)
		}
	})

	t.Run("happy path", func(t *testing.T) {
		t.Setenv(env, goodB64)
		got, err := LoadKey(env)
		if err != nil {
			t.Fatalf("LoadKey: %v", err)
		}
		if !bytes.Equal(got, good) {
			t.Fatalf("LoadKey returned %x, want %x", got, good)
		}
	})
}
