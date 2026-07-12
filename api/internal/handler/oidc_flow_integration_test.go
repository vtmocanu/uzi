package handler

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"gitlab.example.com/vtmocanu/uzi/api/internal/config"
	"gitlab.example.com/vtmocanu/uzi/api/internal/oidc"
	"gitlab.example.com/vtmocanu/uzi/api/internal/secretbox"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// These live-DB tests drive the REAL OIDCLogin -> OIDCCallback flow against a real
// Postgres and a signing httptest fake IdP: the Decision 5 resolve paths the
// offline handler tests cannot reach (they need GetUserByEmail / LinkUserOIDC /
// CreateUserOIDC + the JIT pg_advisory_xact_lock transaction). Skipped unless
// UZI_TEST_DATABASE_URL points at a throwaway Postgres (`go test ./...` without it
// SKIPs). The full callback matrix (first-admin, concurrent-JIT 23505, ...) is M5;
// these prove the core flow.

// signingIDP is a full fake OpenID provider: discovery + JWKS + a token endpoint
// that mints an RS256-signed ID token from the currently-configured claims.
type signingIDP struct {
	srv    *httptest.Server
	key    *rsa.PrivateKey
	kid    string
	issuer string
	client string

	sub           string
	email         string
	name          string
	nonce         string
	emailVerified any
}

func newSigningIDP(t *testing.T) *signingIDP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa key: %v", err)
	}
	f := &signingIDP{key: key, kid: "k1", client: "uzi-client", emailVerified: true, name: "Test User"}
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
				"kty": "RSA", "use": "sig", "alg": "RS256", "kid": f.kid,
				"n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
				"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
			}},
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		claims := jwt.MapClaims{
			"iss": f.issuer, "sub": f.sub, "aud": f.client,
			"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
			"nonce": f.nonce, "email": f.email, "name": f.name,
		}
		if f.emailVerified != nil {
			claims["email_verified"] = f.emailVerified
		}
		tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
		tok.Header["kid"] = f.kid
		signed, err := tok.SignedString(f.key)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "fake", "token_type": "Bearer", "expires_in": 3600, "id_token": signed,
		})
	})
	f.srv = httptest.NewServer(mux)
	f.issuer = f.srv.URL
	return f
}

func (f *signingIDP) provider() *oidc.Provider {
	return oidc.New(oidc.Config{
		IssuerURL:    f.issuer,
		ClientID:     f.client,
		ClientSecret: "test-secret",
		RedirectURL:  "https://uzi.example.com/api/auth/oidc/callback",
		Scopes:       []string{"openid", "profile", "email"},
		HTTPTimeout:  5 * time.Second,
	})
}

// oidcLivePool applies migrations against the throwaway DB and returns a pool, or
// skips when no test DB is configured.
func oidcLivePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run against a throwaway Postgres for live OIDC flow coverage")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func oidcLiveHandler(t *testing.T, pool *pgxpool.Pool, f *signingIDP) *Handler {
	t.Helper()
	key := make([]byte, secretbox.KeySize)
	for i := range key {
		key[i] = byte(i + 3)
	}
	box, err := secretbox.New(key)
	if err != nil {
		t.Fatalf("secretbox.New: %v", err)
	}
	cfg := config.Config{
		CookieSecure:         false,
		RegistrationEnabled:  true,
		PasswordLoginEnabled: true,
		JWTSecret:            []byte("integration-test-jwt-secret-not-a-real-key"),
		AuthTokenTTL:         time.Hour,
	}
	return &Handler{pool: pool, q: store.New(pool), cfg: cfg, box: box, oidc: f.provider()}
}

// callbackFor runs OIDCLogin to obtain a sealed state cookie, points the IdP's
// minted nonce at the sealed nonce (unless matchNonce=false, exercising the
// mismatch path), and runs OIDCCallback. It returns the callback recorder.
func callbackFor(t *testing.T, h *Handler, f *signingIDP, matchNonce bool) *httptest.ResponseRecorder {
	t.Helper()
	recLogin := httptest.NewRecorder()
	h.OIDCLogin(recLogin, httptest.NewRequest(http.MethodGet, "/api/auth/oidc/login", nil))
	if recLogin.Code != http.StatusFound {
		t.Fatalf("login status = %d, want 302", recLogin.Code)
	}
	var stateCookie *http.Cookie
	for _, c := range recLogin.Result().Cookies() {
		if c.Name == oidcStateCookieName {
			stateCookie = c
		}
	}
	if stateCookie == nil {
		t.Fatal("login set no state cookie")
	}
	rr := httptest.NewRequest(http.MethodGet, "/", nil)
	rr.AddCookie(stateCookie)
	data, ok := h.readOIDCStateCookie(rr)
	if !ok {
		t.Fatal("state cookie unreadable")
	}
	if matchNonce {
		f.nonce = data.Nonce
	} else {
		f.nonce = "deliberately-wrong-nonce"
	}

	req := httptest.NewRequest(http.MethodGet, "/api/auth/oidc/callback?code=auth-code&state="+url.QueryEscape(data.State), nil)
	req.AddCookie(stateCookie)
	rec := httptest.NewRecorder()
	h.OIDCCallback(rec, req)
	return rec
}

func TestOIDCCallbackJITCreatesPasswordlessUserLiveDB(t *testing.T) {
	pool := oidcLivePool(t)
	f := newSigningIDP(t)
	defer f.srv.Close()
	h := oidcLiveHandler(t, pool, f)
	ctx := context.Background()

	// A pre-existing user guarantees this JIT user is not the first (so is_admin is
	// deterministically false); the first-admin branch is covered in M5.
	seedEmail := "seed-" + oidcUniq(t) + "@example.com"
	if _, err := store.New(pool).CreateUser(ctx, store.CreateUserParams{Email: seedEmail, PasswordHash: pgtype.Text{String: "x", Valid: true}}); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	f.sub = "sub-jit-" + oidcUniq(t)
	f.email = "JIT-" + oidcUniq(t) + "@Example.com" // mixed case → must be canonicalized
	f.emailVerified = true

	rec := callbackFor(t, h, f, true)
	assertLoggedInRedirect(t, rec)

	got, err := store.New(pool).GetUserByEmail(ctx, strings.ToLower(f.email))
	if err != nil {
		t.Fatalf("JIT user not found by canonicalized email: %v", err)
	}
	if got.PasswordHash.Valid {
		t.Error("JIT user has a non-NULL password_hash; must be passwordless")
	}
	if !got.OidcSubject.Valid || got.OidcSubject.String != f.sub {
		t.Errorf("oidc_subject = %+v, want %q", got.OidcSubject, f.sub)
	}
	if !got.OidcIssuer.Valid || got.OidcIssuer.String != f.issuer {
		t.Errorf("oidc_issuer = %+v, want %q", got.OidcIssuer, f.issuer)
	}
	if got.IsAdmin {
		t.Error("JIT user is admin though a prior user exists")
	}
	// Decision 6 / audit M4: the callback must NOT create a vault row.
	var vaultRows int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM user_vaults WHERE user_id=$1", got.ID).Scan(&vaultRows); err != nil {
		t.Fatalf("count vault rows: %v", err)
	}
	if vaultRows != 0 {
		t.Errorf("JIT login created %d vault rows; must be 0 (Decision 6)", vaultRows)
	}
}

func TestOIDCCallbackLinksExistingUserLiveDB(t *testing.T) {
	pool := oidcLivePool(t)
	f := newSigningIDP(t)
	defer f.srv.Close()
	h := oidcLiveHandler(t, pool, f)
	ctx := context.Background()

	email := "link-" + oidcUniq(t) + "@example.com"
	existing, err := store.New(pool).CreateUser(ctx, store.CreateUserParams{Email: email, PasswordHash: pgtype.Text{String: "argon2-hash-placeholder", Valid: true}})
	if err != nil {
		t.Fatalf("seed password user: %v", err)
	}

	f.sub = "sub-link-" + oidcUniq(t)
	f.email = email
	f.emailVerified = true

	rec := callbackFor(t, h, f, true)
	assertLoggedInRedirect(t, rec)

	got, err := store.New(pool).GetUserByID(ctx, existing.ID)
	if err != nil {
		t.Fatalf("reload user: %v", err)
	}
	if !got.OidcSubject.Valid || got.OidcSubject.String != f.sub {
		t.Errorf("existing account was not linked: oidc_subject=%+v", got.OidcSubject)
	}
	if !got.PasswordHash.Valid {
		t.Error("linking wiped the password_hash; a linked user must keep their password")
	}
}

func TestOIDCCallbackUnverifiedEmailForbiddenLiveDB(t *testing.T) {
	pool := oidcLivePool(t)
	f := newSigningIDP(t)
	defer f.srv.Close()
	h := oidcLiveHandler(t, pool, f)
	ctx := context.Background()

	f.sub = "sub-unverified-" + oidcUniq(t)
	f.email = "unverified-" + oidcUniq(t) + "@example.com"
	f.emailVerified = false // must block link AND JIT

	rec := callbackFor(t, h, f, true)
	assertErrorRedirect(t, rec, "oidc_forbidden")
	assertNoAuthCookie(t, rec)

	if _, err := store.New(pool).GetUserByEmail(ctx, f.email); err == nil {
		t.Error("an unverified-email login JIT-created a user; it must not")
	} else if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("unexpected lookup error: %v", err)
	}
}

func TestOIDCCallbackBadNonceLiveDB(t *testing.T) {
	pool := oidcLivePool(t)
	f := newSigningIDP(t)
	defer f.srv.Close()
	h := oidcLiveHandler(t, pool, f)

	f.sub = "sub-badnonce-" + oidcUniq(t)
	f.email = "badnonce-" + oidcUniq(t) + "@example.com"
	f.emailVerified = true

	rec := callbackFor(t, h, f, false) // token nonce != sealed nonce
	assertErrorRedirect(t, rec, "oidc_exchange")
	assertNoAuthCookie(t, rec)
}

// --- assertions ------------------------------------------------------------

func oidcUniq(t *testing.T) string {
	t.Helper()
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%x", b)
}

func assertLoggedInRedirect(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302; body=%s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/" {
		t.Fatalf("Location = %q, want / (successful login)", loc)
	}
	if !hasLiveAuthCookie(rec) {
		t.Error("no auth cookie set on a successful OIDC login")
	}
}

func assertErrorRedirect(t *testing.T, rec *httptest.ResponseRecorder, code string) {
	t.Helper()
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	if want := "/login?error=" + code; rec.Header().Get("Location") != want {
		t.Fatalf("Location = %q, want %q", rec.Header().Get("Location"), want)
	}
}

func assertNoAuthCookie(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if hasLiveAuthCookie(rec) {
		t.Error("an auth cookie was set on a failed OIDC login")
	}
}

func hasLiveAuthCookie(rec *httptest.ResponseRecorder) bool {
	for _, c := range rec.Result().Cookies() {
		if c.Name == "uzi_auth" && c.Value != "" && c.MaxAge >= 0 {
			return true
		}
	}
	return false
}
