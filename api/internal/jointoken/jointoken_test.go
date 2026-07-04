package jointoken

import (
	"bytes"
	"crypto/sha256"
	"strings"
	"testing"
)

func TestGenerateProducesPrefixedTokenAndMatchingHash(t *testing.T) {
	token, hash, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !strings.HasPrefix(token, Prefix) {
		t.Fatalf("token %q missing prefix %q", token, Prefix)
	}
	want := sha256.Sum256([]byte(token))
	if !bytes.Equal(hash, want[:]) {
		t.Fatal("returned hash is not sha256(token)")
	}
	if !Equal(hash, Hash(token)) {
		t.Fatal("Hash(token) does not match the hash returned by Generate")
	}
}

func TestGenerateIsUnique(t *testing.T) {
	seen := map[string]struct{}{}
	for i := 0; i < 100; i++ {
		token, _, err := Generate()
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		if _, dup := seen[token]; dup {
			t.Fatal("Generate produced a duplicate token")
		}
		seen[token] = struct{}{}
	}
}

func TestHashDoesNotEqualPlaintext(t *testing.T) {
	token, hash, _ := Generate()
	if strings.Contains(string(hash), token) {
		t.Fatal("stored hash contains the plaintext token")
	}
}

func TestFromAuthorizationHeader(t *testing.T) {
	tests := []struct {
		in      string
		wantTok string
		wantOK  bool
	}{
		{"Bearer uzw_abc", "uzw_abc", true},
		{"bearer uzw_abc", "uzw_abc", true}, // scheme is case-insensitive
		{"Bearer   uzw_spaces  ", "uzw_spaces", true},
		{"", "", false},
		{"uzw_no_scheme", "", false},
		{"Basic dXNlcjpwYXNz", "", false},
		{"Bearer ", "", false},
		{"Bearer    ", "", false},
	}
	for _, tc := range tests {
		gotTok, gotOK := FromAuthorizationHeader(tc.in)
		if gotTok != tc.wantTok || gotOK != tc.wantOK {
			t.Errorf("FromAuthorizationHeader(%q) = (%q, %v), want (%q, %v)", tc.in, gotTok, gotOK, tc.wantTok, tc.wantOK)
		}
	}
}
