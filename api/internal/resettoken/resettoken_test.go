package resettoken

import (
	"bytes"
	"strings"
	"testing"
)

func TestGenerateShapeAndRoundTrip(t *testing.T) {
	tok, hash, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tok, "uzr_") {
		t.Fatalf("token %q lacks uzr_ prefix", tok)
	}
	if len(tok) != Length || Length != 47 {
		t.Fatalf("token length %d, Length %d, want 47", len(tok), Length)
	}
	if len(hash) != 32 {
		t.Fatalf("hash length %d, want 32", len(hash))
	}
	got, ok := Hash(tok)
	if !ok || !bytes.Equal(got, hash) {
		t.Fatalf("Hash(token) = %x, %v; want the stored hash", got, ok)
	}
}

func TestGenerateIsRandom(t *testing.T) {
	a, _, _ := Generate()
	b, _, _ := Generate()
	if a == b {
		t.Fatal("two tokens collided")
	}
}

func TestHashRejectsWrongShape(t *testing.T) {
	tok, _, _ := Generate()
	for name, in := range map[string]string{
		"empty":       "",
		"short":       tok[:len(tok)-1],
		"long":        tok + "A",
		"huge":        strings.Repeat("A", 1<<20),
		"wrongprefix": "uzc_" + tok[len("uzr_"):],
		"noprefix":    strings.Repeat("A", Length),
	} {
		if h, ok := Hash(in); ok || h != nil {
			t.Errorf("%s: Hash accepted %d bytes", name, len(in))
		}
	}
}
