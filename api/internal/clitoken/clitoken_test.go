package clitoken

import (
	"strings"
	"testing"
)

func TestGeneratePrefixesAndDisplay(t *testing.T) {
	cases := []struct {
		scope      string
		wantClass  string
		otherClass string
	}{
		{ScopeUser, PrefixUser, PrefixAdmin},
		{ScopeAdminRO, PrefixAdmin, PrefixUser},
		{"", PrefixUser, PrefixAdmin}, // empty scope defaults to a user token
	}
	for _, tc := range cases {
		token, hash, prefix, err := Generate(tc.scope)
		if err != nil {
			t.Fatalf("Generate(%q): %v", tc.scope, err)
		}
		if !strings.HasPrefix(token, tc.wantClass) {
			t.Errorf("Generate(%q) token %q lacks class prefix %q", tc.scope, token, tc.wantClass)
		}
		if strings.HasPrefix(token, tc.otherClass) {
			t.Errorf("Generate(%q) token %q must not carry the other class prefix %q", tc.scope, token, tc.otherClass)
		}
		// token_prefix is the 4-char class prefix + 4 body chars = 8 chars, and is a
		// strict prefix of the full token.
		if len(prefix) != len(tc.wantClass)+displayBodyChars {
			t.Errorf("Generate(%q) display prefix %q len = %d, want %d", tc.scope, prefix, len(prefix), len(tc.wantClass)+displayBodyChars)
		}
		if !strings.HasPrefix(token, prefix) {
			t.Errorf("Generate(%q) display prefix %q is not a prefix of token %q", tc.scope, prefix, token)
		}
		if len(hash) != 32 {
			t.Errorf("Generate(%q) hash len = %d, want 32 (sha256)", tc.scope, len(hash))
		}
		if !Equal(hash, Hash(token)) {
			t.Errorf("Generate(%q) hash != Hash(token)", tc.scope)
		}
	}
}

func TestGenerateUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		token, _, _, err := Generate(ScopeUser)
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		if seen[token] {
			t.Fatalf("duplicate token generated: %q", token)
		}
		seen[token] = true
	}
}

func TestFromAuthorizationHeader(t *testing.T) {
	cases := []struct {
		name   string
		header string
		want   string
		ok     bool
	}{
		{"bearer token", "Bearer uzc_abc", "uzc_abc", true},
		{"bearer case-insensitive scheme", "bearer uzc_abc", "uzc_abc", true},
		{"bearer trims space", "Bearer   uzc_abc  ", "uzc_abc", true},
		{"empty", "", "", false},
		{"bearer empty credential", "Bearer ", "", false},
		{"bearer only spaces", "Bearer    ", "", false},
		{"basic falls through", "Basic dXNlcjpwYXNz", "", false},
		{"no scheme", "uzc_abc", "", false},
	}
	for _, tc := range cases {
		got, ok := FromAuthorizationHeader(tc.header)
		if ok != tc.ok || got != tc.want {
			t.Errorf("%s: FromAuthorizationHeader(%q) = (%q, %v), want (%q, %v)", tc.name, tc.header, got, ok, tc.want, tc.ok)
		}
	}
}
