package recovery

import (
	"bytes"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/secretbox"
)

func testBox(t *testing.T) *secretbox.Box {
	t.Helper()
	key := make([]byte, secretbox.KeySize)
	for i := range key {
		key[i] = byte(i + 7)
	}
	box, err := secretbox.New(key)
	if err != nil {
		t.Fatalf("new box: %v", err)
	}
	return box
}

// TestChunkAADRoundTrip proves a chunk sealed with its per-chunk AAD opens back to the
// exact plaintext under the SAME AAD.
func TestChunkAADRoundTrip(t *testing.T) {
	box := testBox(t)
	cap := uuid.New()
	plain := []byte("committed history bytes")
	sealed, err := box.SealWithAAD(plain, chunkAAD(cap, 0, len(plain)))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	got, err := box.OpenWithAAD(sealed, chunkAAD(cap, 0, len(plain)))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("round-trip mismatch: got %q want %q", got, plain)
	}
}

// TestChunkAADDefeatsReorderSubstitutionSplice proves the AAD binding defeats the three
// attacks D4 names: a chunk cannot be opened under a DIFFERENT index (reorder), a DIFFERENT
// capture id (cross-capture substitution/splice), or a DIFFERENT declared length. Each is an
// authentication failure, never a silently-served corrupt byte.
func TestChunkAADDefeatsReorderSubstitutionSplice(t *testing.T) {
	box := testBox(t)
	capA := uuid.New()
	capB := uuid.New()
	plain := bytes.Repeat([]byte("x"), 4096)
	sealed, err := box.SealWithAAD(plain, chunkAAD(capA, 3, len(plain)))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	// Reorder: same capture + length, WRONG index.
	if _, err := box.OpenWithAAD(sealed, chunkAAD(capA, 4, len(plain))); err == nil {
		t.Error("open under a wrong chunk index succeeded; reorder is not defeated")
	}
	// Substitution/splice across captures: WRONG capture id.
	if _, err := box.OpenWithAAD(sealed, chunkAAD(capB, 3, len(plain))); err == nil {
		t.Error("open under a foreign capture id succeeded; cross-capture splice is not defeated")
	}
	// Length forgery: WRONG declared length.
	if _, err := box.OpenWithAAD(sealed, chunkAAD(capA, 3, len(plain)-1)); err == nil {
		t.Error("open under a wrong length succeeded; length is not bound")
	}
	// A single flipped ciphertext byte fails the tag.
	tampered := append([]byte(nil), sealed...)
	tampered[len(tampered)-1] ^= 0x01
	if _, err := box.OpenWithAAD(tampered, chunkAAD(capA, 3, len(plain))); err == nil {
		t.Error("open of a tampered ciphertext succeeded; the AEAD tag is not enforced")
	}
}
