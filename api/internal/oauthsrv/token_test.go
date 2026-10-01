package oauthsrv

import (
	"crypto/sha256"
	"encoding/base64"
	"net/url"
	"strings"
	"testing"
)

func s256(v string) string {
	sum := sha256.Sum256([]byte(v))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func TestValidVerifier(t *testing.T) {
	good43 := strings.Repeat("a", 43)
	cases := []struct {
		name string
		v    string
		want bool
	}{
		{"min length", good43, true},
		{"max length", strings.Repeat("Z", 128), true},
		{"whole unreserved set", strings.Repeat("aZ09-._~", 6), true},
		{"empty", "", false},
		{"one short", good43[:42], false},
		{"one long", strings.Repeat("a", 129), false},
		{"space", good43[:42] + " ", false},
		{"plus", good43[:42] + "+", false},
		{"slash", good43[:42] + "/", false},
		{"padding", good43[:42] + "=", false},
		{"non-ascii", good43[:41] + "é", false},
		{"control", good43[:42] + "\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ValidVerifier(tc.v); got != tc.want {
				t.Fatalf("ValidVerifier(%q) = %v, want %v", tc.v, got, tc.want)
			}
		})
	}
}

func TestVerifierMatchesS256(t *testing.T) {
	// RFC 7636 appendix B's worked example.
	const verifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	const challenge = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
	if !VerifierMatchesS256(verifier, challenge) {
		t.Fatal("the RFC 7636 appendix B pair must match")
	}
	other := strings.Repeat("b", 43)
	if VerifierMatchesS256(other, challenge) {
		t.Fatal("a different verifier matched")
	}
	if VerifierMatchesS256(verifier, s256("x")) {
		t.Fatal("a different challenge matched")
	}
	short := strings.Repeat("c", 42)
	if VerifierMatchesS256(short, s256(short)) {
		t.Fatal("a malformed verifier matched its own hash: the format check must come first")
	}
	if VerifierMatchesS256(verifier, "") {
		t.Fatal("an empty challenge matched")
	}
}

func TestParseTokenForm(t *testing.T) {
	f := ParseTokenForm(url.Values{
		"grant_type": {"authorization_code"}, "code": {"c"}, "redirect_uri": {"https://x/cb"},
		"code_verifier": {"v"}, "client_id": {"id"},
	})
	if f.GrantType != "authorization_code" || f.Code != "c" || f.RedirectURI != "https://x/cb" ||
		f.CodeVerifier != "v" || f.ClientID != "id" || f.HasClientSecret || f.Repeated {
		t.Fatalf("parsed = %+v", f)
	}
	if f := ParseTokenForm(url.Values{"client_secret": {""}}); !f.HasClientSecret {
		t.Fatal("an empty client_secret must still count as present")
	}
	if f := ParseTokenForm(url.Values{"code": {"a", "b"}}); !f.Repeated {
		t.Fatal("a repeated known parameter must be flagged")
	}
	if f := ParseTokenForm(url.Values{"whatever": {"a", "b"}}); !f.Repeated {
		t.Fatal("a repeated unknown parameter must be flagged too")
	}
}

// PRD #1910 M4: the refresh_token grant's and the RFC 7009 revoke request's parameters.
func TestParseTokenFormRefreshAndRevokeParameters(t *testing.T) {
	f := ParseTokenForm(url.Values{
		"grant_type": {"refresh_token"}, "refresh_token": {"r"}, "scope": {"jobs:read"},
		"token": {"t"}, "token_type_hint": {"access_token"},
	})
	if f.RefreshToken != "r" || f.Scope != "jobs:read" || !f.HasScope || f.Token != "t" || f.TokenTypeHint != "access_token" {
		t.Fatalf("parsed = %+v", f)
	}
	if f := ParseTokenForm(url.Values{"scope": {""}}); !f.HasScope || f.Scope != "" {
		t.Fatalf("an empty scope must count as present (malformed), got %+v", f)
	}
	if f := ParseTokenForm(url.Values{}); f.HasScope {
		t.Fatal("an absent scope must not count as present")
	}
}

func TestSecretMatches(t *testing.T) {
	secret, hash, _, err := GenerateSecret()
	if err != nil {
		t.Fatal(err)
	}
	if !SecretMatches(secret, hash) {
		t.Fatal("the generated secret must match its hash")
	}
	if SecretMatches(secret+"x", hash) || SecretMatches("", hash) {
		t.Fatal("a wrong secret matched")
	}
	if SecretMatches(secret, nil) || SecretMatches("", nil) {
		t.Fatal("no stored hash must never match")
	}
}
