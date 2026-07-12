package oidc

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

// TestEmailVerifiedStrictBoolean locks the audit-L2 rule: email_verified counts as
// verified ONLY when it is the JSON boolean true. The string "true", a missing
// value, the number 1, and boolean false all read as unverified.
func TestEmailVerifiedStrictBoolean(t *testing.T) {
	cases := map[string]struct {
		raw  string // the raw JSON value of email_verified, or "" for absent
		want bool
	}{
		"boolean true":  {"true", true},
		"boolean false": {"false", false},
		"string true":   {`"true"`, false},
		"string false":  {`"false"`, false},
		"number one":    {"1", false},
		"null":          {"null", false},
		"absent":        {"", false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var c rawClaims
			if tc.raw != "" {
				c.EmailVerified = json.RawMessage(tc.raw)
			}
			if got := c.emailVerifiedTrue(); got != tc.want {
				t.Errorf("emailVerifiedTrue(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

// discoveryDoc is the minimal OpenID discovery document go-oidc requires.
func discoveryDoc(issuer string) map[string]any {
	return map[string]any{
		"issuer":                                issuer,
		"authorization_endpoint":                issuer + "/authorize",
		"token_endpoint":                        issuer + "/token",
		"jwks_uri":                              issuer + "/jwks",
		"id_token_signing_alg_values_supported": []string{"RS256"},
	}
}

// TestDiscoverLazyAndDegraded proves the Decision 8 posture: a Provider whose IdP
// is down at first contact returns an error and stays uncached (degraded), then a
// later call retries and succeeds once the IdP is up — never crash-looping, never
// caching the failure. After discovery, AuthCodeURL carries the PKCE S256 params,
// state, and nonce.
func TestDiscoverLazyAndDegraded(t *testing.T) {
	var up atomic.Bool
	var issuer string
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		if !up.Load() {
			http.Error(w, "idp down", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(discoveryDoc(issuer))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	issuer = srv.URL

	p := New(Config{
		IssuerURL:    issuer,
		ClientID:     "uzi",
		ClientSecret: "shhh",
		RedirectURL:  issuer + "/api/auth/oidc/callback",
		Scopes:       []string{"openid", "profile", "email"},
		HTTPTimeout:  5 * time.Second,
	})

	// IdP down: discovery fails and nothing is cached.
	if err := p.Discover(context.Background()); err == nil {
		t.Fatal("expected discovery error while IdP is down")
	}
	// A build attempt while degraded also errors (retry, not a stale cache).
	if _, err := p.AuthCodeURL(context.Background(), "s", "n", oauth2.GenerateVerifier()); err == nil {
		t.Fatal("expected AuthCodeURL error while IdP is down")
	}

	// IdP comes up: the retry succeeds.
	up.Store(true)
	if err := p.Discover(context.Background()); err != nil {
		t.Fatalf("discovery retry after IdP up: %v", err)
	}

	u, err := p.AuthCodeURL(context.Background(), "state-xyz", "nonce-xyz", oauth2.GenerateVerifier())
	if err != nil {
		t.Fatalf("AuthCodeURL: %v", err)
	}
	for _, want := range []string{
		"code_challenge=",
		"code_challenge_method=S256",
		"state=state-xyz",
		"nonce=nonce-xyz",
		"response_type=code",
	} {
		if !strings.Contains(u, want) {
			t.Errorf("auth URL missing %q: %s", want, u)
		}
	}
}
