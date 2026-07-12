package oidc

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/oauth2"
)

// fakeIDP is a minimal OIDC provider: discovery + JWKS + a token endpoint that
// mints an RS256-signed ID token from the currently-configured claims. It lets the
// Exchange path be exercised end-to-end (real signature + JWKS verification)
// offline, with no external IdP.
type fakeIDP struct {
	srv        *httptest.Server
	key        *rsa.PrivateKey
	kid        string
	issuer     string
	clientID   string
	tokenCalls int

	// Claims minted into the next id_token.
	sub           string
	email         string
	name          string
	nonce         string
	emailVerified any // nil => omit the claim entirely
}

func newFakeIDP(t *testing.T, clientID string) *fakeIDP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa key: %v", err)
	}
	f := &fakeIDP{
		key:           key,
		kid:           "test-key-1",
		clientID:      clientID,
		sub:           "sub-123",
		email:         "user@example.com",
		name:          "Test User",
		emailVerified: true,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                f.issuer,
			"authorization_endpoint":                f.issuer + "/authorize",
			"token_endpoint":                        f.issuer + "/token",
			"jwks_uri":                              f.issuer + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		pub := f.key.PublicKey
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]any{{
				"kty": "RSA",
				"use": "sig",
				"alg": "RS256",
				"kid": f.kid,
				"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
				"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
			}},
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		f.tokenCalls++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "fake-access-token",
			"token_type":   "Bearer",
			"expires_in":   3600,
			"id_token":     f.signIDToken(t),
		})
	})
	f.srv = httptest.NewServer(mux)
	f.issuer = f.srv.URL
	return f
}

func (f *fakeIDP) signIDToken(t *testing.T) string {
	t.Helper()
	claims := jwt.MapClaims{
		"iss":   f.issuer,
		"sub":   f.sub,
		"aud":   f.clientID,
		"exp":   time.Now().Add(time.Hour).Unix(),
		"iat":   time.Now().Unix(),
		"nonce": f.nonce,
		"email": f.email,
		"name":  f.name,
	}
	if f.emailVerified != nil {
		claims["email_verified"] = f.emailVerified
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = f.kid
	signed, err := tok.SignedString(f.key)
	if err != nil {
		t.Fatalf("sign id_token: %v", err)
	}
	return signed
}

func (f *fakeIDP) provider() *Provider {
	return New(Config{
		IssuerURL:    f.issuer,
		ClientID:     f.clientID,
		ClientSecret: "test-secret",
		RedirectURL:  f.issuer + "/cb",
		Scopes:       []string{"openid", "profile", "email"},
		HTTPTimeout:  5 * time.Second,
	})
}

// TestExchangeVerifiesAndMapsIdentity: a well-formed token round-trips through the
// real JWKS verification and maps to the expected Identity. sub/iss come from the
// verified token; the wrapper does NOT canonicalize email (the handler does).
func TestExchangeVerifiesAndMapsIdentity(t *testing.T) {
	f := newFakeIDP(t, "uzi-client")
	defer f.srv.Close()
	f.nonce = "expected-nonce"
	f.sub = "sub-abc"
	f.email = "Alice@Example.com"
	f.name = "Alice"
	f.emailVerified = true

	id, err := f.provider().Exchange(context.Background(), "auth-code", oauth2.GenerateVerifier(), "expected-nonce")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if id.Subject != "sub-abc" {
		t.Errorf("Subject = %q, want sub-abc", id.Subject)
	}
	if id.Issuer != f.issuer {
		t.Errorf("Issuer = %q, want %q", id.Issuer, f.issuer)
	}
	if id.Email != "Alice@Example.com" {
		t.Errorf("Email = %q, want the raw claim (uncanonicalized)", id.Email)
	}
	if !id.EmailVerified {
		t.Error("EmailVerified = false, want true")
	}
	if id.Name != "Alice" {
		t.Errorf("Name = %q, want Alice", id.Name)
	}
	if f.tokenCalls != 1 {
		t.Errorf("token endpoint called %d times, want 1", f.tokenCalls)
	}
}

// TestExchangeNonceMismatch: the ID token verifies but its nonce differs from the
// one sealed at login → ErrNonceMismatch (replay/CSRF guard).
func TestExchangeNonceMismatch(t *testing.T) {
	f := newFakeIDP(t, "uzi-client")
	defer f.srv.Close()
	f.nonce = "token-nonce"

	_, err := f.provider().Exchange(context.Background(), "auth-code", oauth2.GenerateVerifier(), "different-nonce")
	if !errors.Is(err, ErrNonceMismatch) {
		t.Fatalf("Exchange error = %v, want ErrNonceMismatch", err)
	}
}

// TestExchangeEmailVerifiedThroughToken: end-to-end proof that email_verified is
// honored only as a JSON boolean true, even coming off a real signed token
// (audit L2).
func TestExchangeEmailVerifiedThroughToken(t *testing.T) {
	cases := []struct {
		name string
		val  any
		want bool
	}{
		{"bool true", true, true},
		{"bool false", false, false},
		{"string true", "true", false},
		{"number one", 1, false},
		{"absent", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeIDP(t, "uzi-client")
			defer f.srv.Close()
			f.nonce = "n"
			f.emailVerified = tc.val

			id, err := f.provider().Exchange(context.Background(), "code", oauth2.GenerateVerifier(), "n")
			if err != nil {
				t.Fatalf("Exchange: %v", err)
			}
			if id.EmailVerified != tc.want {
				t.Errorf("EmailVerified = %v, want %v", id.EmailVerified, tc.want)
			}
		})
	}
}

// TestExchangeWrongAudienceRejected: go-oidc must reject a token minted for a
// different client id (the verifier is bound to our client id).
func TestExchangeWrongAudienceRejected(t *testing.T) {
	f := newFakeIDP(t, "someone-else")
	defer f.srv.Close()
	f.nonce = "n"

	// Our RP is configured for "uzi-client" but the IdP mints aud="someone-else".
	p := New(Config{
		IssuerURL:    f.issuer,
		ClientID:     "uzi-client",
		ClientSecret: "test-secret",
		RedirectURL:  f.issuer + "/cb",
		Scopes:       []string{"openid"},
		HTTPTimeout:  5 * time.Second,
	})
	if _, err := p.Exchange(context.Background(), "code", oauth2.GenerateVerifier(), "n"); err == nil {
		t.Fatal("Exchange accepted a token with the wrong audience")
	}
}
