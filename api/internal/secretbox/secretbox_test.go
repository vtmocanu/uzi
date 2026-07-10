package secretbox

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"testing"
)

func mustNewBox(t *testing.T) *Box {
	t.Helper()
	key := make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand: %v", err)
	}
	box, err := New(key)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return box
}

func TestRoundTrip(t *testing.T) {
	box := mustNewBox(t)
	plaintext := []byte("glpat-deadbeefdeadbeefdead")
	sealed, err := box.Seal(plaintext)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	// The ciphertext must not contain the plaintext bytes: a DB dump alone
	// cannot recover the secret.
	if bytes.Contains(sealed, plaintext) {
		t.Fatal("sealed output contains the plaintext")
	}
	opened, err := box.Open(sealed)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bytes.Equal(opened, plaintext) {
		t.Fatalf("round trip mismatch: got %q want %q", opened, plaintext)
	}
}

func TestSealIsNonDeterministic(t *testing.T) {
	// Same plaintext + same box → different ciphertext each Seal (random
	// nonce). Prevents confirming two connections share the same PAT.
	box := mustNewBox(t)
	plaintext := []byte("repeat")
	a, _ := box.Seal(plaintext)
	b, _ := box.Seal(plaintext)
	if bytes.Equal(a, b) {
		t.Fatal("expected non-deterministic Seal, got identical ciphertexts")
	}
}

func TestOpenRejectsTampered(t *testing.T) {
	box := mustNewBox(t)
	sealed, _ := box.Seal([]byte("important"))
	tampered := append([]byte(nil), sealed...)
	tampered[len(tampered)-1] ^= 0x01
	if _, err := box.Open(tampered); err == nil {
		t.Fatal("expected auth failure on tampered ciphertext")
	}
}

func TestOpenRejectsShort(t *testing.T) {
	box := mustNewBox(t)
	if _, err := box.Open([]byte("short")); err != ErrCiphertextTooShort {
		t.Fatalf("expected ErrCiphertextTooShort, got %v", err)
	}
}

func TestOpenRejectsWrongKey(t *testing.T) {
	// A ciphertext sealed under one key must not decrypt under another.
	sealed, _ := mustNewBox(t).Seal([]byte("secret"))
	if _, err := mustNewBox(t).Open(sealed); err == nil {
		t.Fatal("expected auth failure opening under a different key")
	}
}

func TestAADRoundTrip(t *testing.T) {
	box := mustNewBox(t)
	aad := []byte("user-id||kind")
	sealed, err := box.SealWithAAD([]byte("bound"), aad)
	if err != nil {
		t.Fatalf("SealWithAAD: %v", err)
	}
	opened, err := box.OpenWithAAD(sealed, aad)
	if err != nil {
		t.Fatalf("OpenWithAAD: %v", err)
	}
	if !bytes.Equal(opened, []byte("bound")) {
		t.Fatalf("aad round trip mismatch: got %q", opened)
	}
}

func TestOpenWithAADRejectsWrongAAD(t *testing.T) {
	box := mustNewBox(t)
	sealed, _ := box.SealWithAAD([]byte("bound"), []byte("owner-a"))
	if _, err := box.OpenWithAAD(sealed, []byte("owner-b")); err == nil {
		t.Fatal("expected auth failure opening under different AAD")
	}
}

func TestNilAADEqualsPlainSeal(t *testing.T) {
	// A ciphertext produced by plain Seal must open via OpenWithAAD(nil) and vice
	// versa — the guarantee that pre-existing (nil-AAD) at-rest rows stay openable.
	box := mustNewBox(t)
	sealed, _ := box.Seal([]byte("legacy"))
	if _, err := box.OpenWithAAD(sealed, nil); err != nil {
		t.Fatalf("OpenWithAAD(nil) on plain Seal output: %v", err)
	}
	sealed2, _ := box.SealWithAAD([]byte("legacy"), nil)
	if _, err := box.Open(sealed2); err != nil {
		t.Fatalf("Open on SealWithAAD(nil) output: %v", err)
	}
}

func TestNewRejectsBadKey(t *testing.T) {
	if _, err := New(make([]byte, 16)); err != ErrInvalidKey {
		t.Fatalf("expected ErrInvalidKey for 16-byte key, got %v", err)
	}
}

func TestLoadKey(t *testing.T) {
	const envVar = "TEST_SECRETBOX_KEY"
	t.Run("missing", func(t *testing.T) {
		t.Setenv(envVar, "")
		if _, err := LoadKey(envVar); err == nil {
			t.Fatal("expected error on missing env var")
		}
	})
	t.Run("bad base64", func(t *testing.T) {
		t.Setenv(envVar, "not!base64!")
		if _, err := LoadKey(envVar); err == nil {
			t.Fatal("expected error on invalid base64")
		}
	})
	t.Run("wrong length", func(t *testing.T) {
		t.Setenv(envVar, base64.StdEncoding.EncodeToString([]byte("too short")))
		if _, err := LoadKey(envVar); err == nil {
			t.Fatal("expected error on short key")
		}
	})
	t.Run("all-zero key rejected", func(t *testing.T) {
		t.Setenv(envVar, base64.StdEncoding.EncodeToString(make([]byte, KeySize)))
		if _, err := LoadKey(envVar); err != ErrWeakKey {
			t.Fatalf("expected ErrWeakKey for all-zero key, got %v", err)
		}
	})
	t.Run("single-repeated-byte placeholder rejected", func(t *testing.T) {
		k := make([]byte, KeySize)
		for i := range k {
			k[i] = 'A'
		}
		t.Setenv(envVar, base64.StdEncoding.EncodeToString(k))
		if _, err := LoadKey(envVar); err != ErrWeakKey {
			t.Fatalf("expected ErrWeakKey for repeated-byte key, got %v", err)
		}
	})
	t.Run("happy path", func(t *testing.T) {
		key := make([]byte, KeySize)
		_, _ = rand.Read(key)
		t.Setenv(envVar, base64.StdEncoding.EncodeToString(key))
		got, err := LoadKey(envVar)
		if err != nil {
			t.Fatalf("LoadKey: %v", err)
		}
		if !bytes.Equal(got, key) {
			t.Fatal("LoadKey returned wrong bytes")
		}
	})
}
