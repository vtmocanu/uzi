package producttoken

import (
	"regexp"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/clitoken"
)

// minted is the exact shape Generate must produce: the class prefix and the
// RawURLEncoding of 32 random bytes (43 chars of [A-Za-z0-9_-]). The body alphabet
// matters beyond this package: every scrub pattern keys on
// `uz[capw]_[A-Za-z0-9_-]{16,}`, so a body character outside that class would let
// the tail of a live token survive a scrub.
var minted = regexp.MustCompile(`^uzp_[A-Za-z0-9_-]{43}$`)

func TestGenerateShapeAndDisplay(t *testing.T) {
	token, hash, prefix, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !minted.MatchString(token) {
		t.Errorf("token %q does not match %s", token, minted)
	}
	if !HasPrefix(token) {
		t.Errorf("HasPrefix(%q) = false for a minted token", token)
	}
	// token_prefix is the 4-char class prefix + 4 body chars and a strict prefix of
	// the token.
	if len(prefix) != len(Prefix)+displayBodyChars || !strings.HasPrefix(token, prefix) {
		t.Errorf("display prefix %q: want %d chars and a prefix of %q", prefix, len(Prefix)+displayBodyChars, token)
	}
	if len(hash) != 32 {
		t.Errorf("hash len = %d, want 32 (sha256)", len(hash))
	}
	if !Equal(hash, Hash(token)) {
		t.Error("hash != Hash(token)")
	}
	if Equal(hash, Hash(token+"x")) {
		t.Error("Equal matched the hash of a different token")
	}
}

// The class prefix is distinct from every CLI class, so a uzp_ value can never be
// dispatched to the cli_tokens path by prefix and vice versa.
func TestPrefixDistinctFromCLIClasses(t *testing.T) {
	for _, p := range clitoken.Prefixes {
		if p == Prefix || strings.HasPrefix(p, Prefix) || strings.HasPrefix(Prefix, p) {
			t.Errorf("product prefix %q overlaps CLI prefix %q", Prefix, p)
		}
	}
}

func TestGenerateUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		token, _, _, err := Generate()
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
	// Assembled at runtime: never one token-shaped literal in source.
	body := strings.Repeat("a", 32)
	product := "uz" + "p_" + body
	cli := "uz" + "c_" + body
	cases := []struct {
		name   string
		header string
		want   string
		ok     bool
	}{
		{"product bearer", "Bearer " + product, product, true},
		{"case-insensitive scheme", "bearer " + product, product, true},
		{"trims space", "Bearer   " + product + "  ", product, true},
		{"cli bearer is not a product token", "Bearer " + cli, "", false},
		{"unknown bearer", "Bearer " + body, "", false},
		{"prefix is case-sensitive", "Bearer UZP_" + body, "", false},
		{"empty", "", "", false},
		{"bearer empty credential", "Bearer ", "", false},
		{"basic", "Basic " + product, "", false},
		{"no scheme", product, "", false},
	}
	for _, tc := range cases {
		got, ok := FromAuthorizationHeader(tc.header)
		if ok != tc.ok || got != tc.want {
			t.Errorf("%s: FromAuthorizationHeader(%q) = (%q, %v), want (%q, %v)", tc.name, tc.header, got, ok, tc.want, tc.ok)
		}
	}
}

func TestScopes(t *testing.T) {
	// The Go vocabulary must equal the migration's CHECK list, in this order.
	if strings.Join(Scopes, ",") != "jobs:run,jobs:read" {
		t.Fatalf("Scopes = %v, want [jobs:run jobs:read] (mirror of the product_tokens CHECK)", Scopes)
	}
	for _, s := range Scopes {
		if !ValidScope(s) {
			t.Errorf("ValidScope(%q) = false", s)
		}
	}
	for _, s := range []string{"", "jobs", "jobs:write", "JOBS:RUN", "jobs:run "} {
		if ValidScope(s) {
			t.Errorf("ValidScope(%q) = true, want false", s)
		}
	}
	good := [][]string{{ScopeJobsRun}, {ScopeJobsRead}, {ScopeJobsRun, ScopeJobsRead}, {ScopeJobsRead, ScopeJobsRun}}
	for _, ss := range good {
		if !ValidScopes(ss) {
			t.Errorf("ValidScopes(%v) = false", ss)
		}
	}
	bad := [][]string{nil, {}, {"jobs:write"}, {ScopeJobsRun, "x"}, {ScopeJobsRun, ScopeJobsRun}}
	for _, ss := range bad {
		if ValidScopes(ss) {
			t.Errorf("ValidScopes(%v) = true, want false", ss)
		}
	}
}
