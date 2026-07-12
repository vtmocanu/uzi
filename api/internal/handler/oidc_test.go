package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"gitlab.example.com/vtmocanu/uzi/api/internal/config"
	"gitlab.example.com/vtmocanu/uzi/api/internal/oidc"
	"gitlab.example.com/vtmocanu/uzi/api/internal/secretbox"
)

// oidcTestBox builds a real secretbox from a fixed non-placeholder key so the
// state-cookie seal/open is exercised for real.
func oidcTestBox(t *testing.T) *secretbox.Box {
	t.Helper()
	key := make([]byte, secretbox.KeySize)
	for i := range key {
		key[i] = byte(i + 7)
	}
	box, err := secretbox.New(key)
	if err != nil {
		t.Fatalf("secretbox.New: %v", err)
	}
	return box
}

func oidcTestHandler(t *testing.T, p *oidc.Provider) *Handler {
	t.Helper()
	return &Handler{cfg: config.Config{CookieSecure: false}, box: oidcTestBox(t), oidc: p}
}

// handlerFakeIDP is a discovery-only fake whose token endpoint just counts calls.
// It is enough to (a) let OIDCLogin discover an authorize endpoint and (b) prove
// the callback never calls /token on a pre-exchange reject (audit M1).
type handlerFakeIDP struct {
	srv        *httptest.Server
	issuer     string
	tokenCalls int
}

func newHandlerFakeIDP(t *testing.T) *handlerFakeIDP {
	t.Helper()
	f := &handlerFakeIDP{}
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
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		f.tokenCalls++
		http.Error(w, "unexpected token call", http.StatusInternalServerError)
	})
	f.srv = httptest.NewServer(mux)
	f.issuer = f.srv.URL
	return f
}

func (f *handlerFakeIDP) provider() *oidc.Provider {
	return oidc.New(oidc.Config{
		IssuerURL:    f.issuer,
		ClientID:     "uzi-client",
		ClientSecret: "test-secret",
		RedirectURL:  "https://uzi.example.com/api/auth/oidc/callback",
		Scopes:       []string{"openid", "profile", "email"},
		HTTPTimeout:  5 * time.Second,
	})
}

// TestOIDCStateCookieRoundTrip: a sealed state cookie opens back to the same data,
// and a wrong/absent/tampered/incomplete cookie is rejected.
func TestOIDCStateCookieRoundTrip(t *testing.T) {
	h := oidcTestHandler(t, nil)
	want := oidcStateData{State: "state-val", Nonce: "nonce-val", Verifier: "verifier-val", IssuedAt: time.Now().Unix()}

	rec := httptest.NewRecorder()
	if err := h.setOIDCStateCookie(rec, want); err != nil {
		t.Fatalf("setOIDCStateCookie: %v", err)
	}
	cookies := rec.Result().Cookies()
	var state *http.Cookie
	for _, c := range cookies {
		if c.Name == oidcStateCookieName {
			state = c
		}
	}
	if state == nil {
		t.Fatal("state cookie not set")
	}
	if !state.HttpOnly || state.SameSite != http.SameSiteLaxMode {
		t.Errorf("cookie flags = HttpOnly:%v SameSite:%v, want HttpOnly + Lax", state.HttpOnly, state.SameSite)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/auth/oidc/callback", nil)
	req.AddCookie(state)
	got, ok := h.readOIDCStateCookie(req)
	if !ok {
		t.Fatal("readOIDCStateCookie: ok=false for a valid cookie")
	}
	if got != want {
		t.Errorf("round-trip = %+v, want %+v", got, want)
	}

	// Rejections.
	t.Run("absent", func(t *testing.T) {
		if _, ok := h.readOIDCStateCookie(httptest.NewRequest(http.MethodGet, "/", nil)); ok {
			t.Error("ok=true with no cookie")
		}
	})
	t.Run("garbage value", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.AddCookie(&http.Cookie{Name: oidcStateCookieName, Value: "!!!not-base64!!!"})
		if _, ok := h.readOIDCStateCookie(r); ok {
			t.Error("ok=true for non-base64 value")
		}
	})
	t.Run("tampered ciphertext", func(t *testing.T) {
		tampered := []byte(state.Value)
		tampered[len(tampered)-1] ^= 0x01 // flip a bit; GCM auth must fail
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.AddCookie(&http.Cookie{Name: oidcStateCookieName, Value: string(tampered)})
		if _, ok := h.readOIDCStateCookie(r); ok {
			t.Error("ok=true for a tampered cookie")
		}
	})
	t.Run("missing field", func(t *testing.T) {
		rec2 := httptest.NewRecorder()
		if err := h.setOIDCStateCookie(rec2, oidcStateData{State: "s", Nonce: "n", IssuedAt: time.Now().Unix()}); err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		for _, c := range rec2.Result().Cookies() {
			r.AddCookie(c)
		}
		if _, ok := h.readOIDCStateCookie(r); ok {
			t.Error("ok=true for a cookie missing the verifier")
		}
	})
	t.Run("expired (back-dated issue time)", func(t *testing.T) {
		rec2 := httptest.NewRecorder()
		stale := oidcStateData{State: "s", Nonce: "n", Verifier: "v", IssuedAt: time.Now().Add(-oidcStateTTL - time.Minute).Unix()}
		if err := h.setOIDCStateCookie(rec2, stale); err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		for _, c := range rec2.Result().Cookies() {
			r.AddCookie(c)
		}
		if _, ok := h.readOIDCStateCookie(r); ok {
			t.Error("ok=true for a cookie older than the server-side TTL (audit Low1)")
		}
	})
}

// TestOIDCLoginRedirectsAndSealsState: OIDCLogin redirects to the IdP authorize
// endpoint with state + nonce + PKCE S256, and the sealed cookie carries the same
// state/nonce that appear in the URL.
func TestOIDCLoginRedirectsAndSealsState(t *testing.T) {
	f := newHandlerFakeIDP(t)
	defer f.srv.Close()
	h := oidcTestHandler(t, f.provider())

	rec := httptest.NewRecorder()
	h.OIDCLogin(rec, httptest.NewRequest(http.MethodGet, "/api/auth/oidc/login", nil))

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	if !strings.HasPrefix(loc.String(), f.issuer+"/authorize") {
		t.Errorf("redirect = %s, want IdP authorize endpoint", loc.String())
	}
	qp := loc.Query()
	if qp.Get("code_challenge") == "" || qp.Get("code_challenge_method") != "S256" {
		t.Errorf("missing PKCE S256 params: %v", qp)
	}
	if qp.Get("state") == "" || qp.Get("nonce") == "" {
		t.Errorf("missing state/nonce: %v", qp)
	}
	if qp.Get("response_type") != "code" {
		t.Errorf("response_type = %q, want code", qp.Get("response_type"))
	}

	// The sealed cookie must carry the same state + nonce that went to the IdP.
	req := httptest.NewRequest(http.MethodGet, "/api/auth/oidc/callback", nil)
	for _, c := range rec.Result().Cookies() {
		req.AddCookie(c)
	}
	data, ok := h.readOIDCStateCookie(req)
	if !ok {
		t.Fatal("login did not seal a readable state cookie")
	}
	if data.State != qp.Get("state") || data.Nonce != qp.Get("nonce") {
		t.Errorf("cookie state/nonce (%q/%q) != URL state/nonce (%q/%q)", data.State, data.Nonce, qp.Get("state"), qp.Get("nonce"))
	}
	if f.tokenCalls != 0 {
		t.Errorf("login hit the token endpoint %d times, want 0", f.tokenCalls)
	}
}

// TestOIDCCallbackNoCookieRejectsBeforeExchange is the audit-M1 property: a
// callback with a code + state but NO state cookie is a hard reject that never
// touches the token endpoint.
func TestOIDCCallbackNoCookieRejectsBeforeExchange(t *testing.T) {
	f := newHandlerFakeIDP(t)
	defer f.srv.Close()
	h := oidcTestHandler(t, f.provider())

	rec := httptest.NewRecorder()
	h.OIDCCallback(rec, httptest.NewRequest(http.MethodGet, "/api/auth/oidc/callback?code=abc&state=xyz", nil))

	assertOIDCErrorRedirect(t, rec, "oidc_state")
	if f.tokenCalls != 0 {
		t.Fatalf("audit M1 violation: token endpoint called %d times with no state cookie", f.tokenCalls)
	}
}

// TestOIDCCallbackStateMismatchRejectsBeforeExchange: a present cookie whose state
// differs from the query state is rejected before any exchange.
func TestOIDCCallbackStateMismatchRejectsBeforeExchange(t *testing.T) {
	f := newHandlerFakeIDP(t)
	defer f.srv.Close()
	h := oidcTestHandler(t, f.provider())

	// Seal a valid (unexpired) cookie with state "correct".
	setter := httptest.NewRecorder()
	if err := h.setOIDCStateCookie(setter, oidcStateData{State: "correct", Nonce: "n", Verifier: "v", IssuedAt: time.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/auth/oidc/callback?code=abc&state=wrong", nil)
	for _, c := range setter.Result().Cookies() {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.OIDCCallback(rec, req)

	assertOIDCErrorRedirect(t, rec, "oidc_state")
	if f.tokenCalls != 0 {
		t.Fatalf("audit M1 violation: token endpoint called %d times on state mismatch", f.tokenCalls)
	}
}

// TestOIDCCallbackEmptyCodeIsExchangeError: a valid cookie + matching state but no
// code is a protocol failure (Nit A) → oidc_exchange, and no token call happens
// (there is nothing to exchange).
func TestOIDCCallbackEmptyCodeIsExchangeError(t *testing.T) {
	f := newHandlerFakeIDP(t)
	defer f.srv.Close()
	h := oidcTestHandler(t, f.provider())

	setter := httptest.NewRecorder()
	if err := h.setOIDCStateCookie(setter, oidcStateData{State: "st", Nonce: "n", Verifier: "v", IssuedAt: time.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/auth/oidc/callback?state=st", nil) // no code
	for _, c := range setter.Result().Cookies() {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.OIDCCallback(rec, req)

	assertOIDCErrorRedirect(t, rec, "oidc_exchange")
	if f.tokenCalls != 0 {
		t.Fatalf("token endpoint called %d times for an empty code", f.tokenCalls)
	}
}

// TestOIDCCallbackClearsStateCookie: every callback outcome deletes the one-shot
// state cookie (here, the reject path).
func TestOIDCCallbackClearsStateCookie(t *testing.T) {
	f := newHandlerFakeIDP(t)
	defer f.srv.Close()
	h := oidcTestHandler(t, f.provider())

	rec := httptest.NewRecorder()
	h.OIDCCallback(rec, httptest.NewRequest(http.MethodGet, "/api/auth/oidc/callback?code=abc&state=xyz", nil))

	var cleared bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == oidcStateCookieName && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Error("callback did not clear the state cookie")
	}
}

// TestRegisterDisabledWhenPasswordLoginOff: with password login off, registration
// is refused even when RegistrationEnabled is true (audit M3).
func TestRegisterDisabledWhenPasswordLoginOff(t *testing.T) {
	rec := postRegister(config.Config{RegistrationEnabled: true, PasswordLoginEnabled: false},
		`{"email":"alice@example.com","password":"a-long-enough-password"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "password login is disabled") {
		t.Errorf("body = %q, want the password-login-disabled reason", rec.Body.String())
	}
}

func assertOIDCErrorRedirect(t *testing.T, rec *httptest.ResponseRecorder, code string) {
	t.Helper()
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	if want := "/login?error=" + code; rec.Header().Get("Location") != want {
		t.Fatalf("Location = %q, want %q", rec.Header().Get("Location"), want)
	}
}
