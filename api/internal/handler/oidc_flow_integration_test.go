package handler

import (
	"bytes"
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
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/config"
	"github.com/vtmocanu/uzi/api/internal/oidc"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/store"
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
	emailVerified any

	// Group-claim controls (PRD #55). groupsClaimName is the claim key emitted AND
	// threaded into the provider as GroupsClaim (default "groups"). groupsSet=false
	// omits the claim entirely (absent case); when true, groups is emitted verbatim
	// ([]any{...} => array; "s" => string/malformed; []any{"a",""} => empty element).
	groupsClaimName string
	groupsSet       bool
	groups          any
}

func newSigningIDP(t *testing.T) *signingIDP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa key: %v", err)
	}
	f := &signingIDP{key: key, kid: "k1", client: "uzi-client", emailVerified: true, name: "Test User", groupsClaimName: "groups"}
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
		// Mint the id_token nonce from the received authorization code, so each
		// callback gets a token matching its OWN sealed nonce with no shared field to
		// race (concurrency-safe). Tests pass code == the sealed nonce for a match, or
		// a deliberately-wrong value for the mismatch path.
		_ = r.ParseForm()
		claims := jwt.MapClaims{
			"iss": f.issuer, "sub": f.sub, "aud": f.client,
			"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
			"nonce": r.FormValue("code"), "email": f.email, "name": f.name,
		}
		if f.emailVerified != nil {
			claims["email_verified"] = f.emailVerified
		}
		if f.groupsSet {
			claims[f.groupsClaimName] = f.groups
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
		GroupsClaim:  f.groupsClaimName,
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

func oidcLiveBox(t *testing.T) *secretbox.Box {
	t.Helper()
	key := make([]byte, secretbox.KeySize)
	for i := range key {
		key[i] = byte(i + 3)
	}
	box, err := secretbox.New(key)
	if err != nil {
		t.Fatalf("secretbox.New: %v", err)
	}
	return box
}

// oidcLiveCfg is the default handler config for the flow tests: registration on,
// password login on, no domain allowlist. Individual tests copy + tweak it.
func oidcLiveCfg() config.Config {
	return config.Config{
		CookieSecure:         false,
		RegistrationEnabled:  true,
		PasswordLoginEnabled: true,
		JWTSecret:            []byte("integration-test-jwt-secret-not-a-real-key"),
		AuthTokenTTL:         time.Hour,
	}
}

func oidcLiveHandlerWith(t *testing.T, pool *pgxpool.Pool, f *signingIDP, cfg config.Config) *Handler {
	t.Helper()
	return &Handler{pool: pool, q: store.New(pool), cfg: cfg, box: oidcLiveBox(t), oidc: f.provider()}
}

func oidcLiveHandler(t *testing.T, pool *pgxpool.Pool, f *signingIDP) *Handler {
	return oidcLiveHandlerWith(t, pool, f, oidcLiveCfg())
}

// resetUsers truncates the users table for tests that depend on the first-user
// state (JIT-first-admin, concurrent JIT). Safe because the handler-package
// live-DB tests run sequentially against a dedicated throwaway Postgres.
func resetUsers(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), "TRUNCATE users CASCADE"); err != nil {
		t.Fatalf("reset users: %v", err)
	}
}

func oidcTxt(s string) pgtype.Text { return pgtype.Text{String: s, Valid: true} }

// callbackFor runs OIDCLogin to obtain a sealed state cookie, then runs
// OIDCCallback with code == the sealed nonce (matchNonce) or a wrong value — the
// fake IdP mints the id_token nonce from the code, so a matching code yields a
// nonce that passes verification. Returns the callback recorder. Safe to call
// concurrently (each call has its own login/cookie/nonce).
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
	code := data.Nonce
	if !matchNonce {
		code = "deliberately-wrong-nonce"
	}

	req := httptest.NewRequest(http.MethodGet, "/api/auth/oidc/callback?code="+url.QueryEscape(code)+"&state="+url.QueryEscape(data.State), nil)
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

// --- Decision 5 resolve matrix (live DB) -----------------------------------

// TestOIDCCallbackFirstUserBecomesAdminLiveDB: on an empty instance the first
// JIT-provisioned OIDC user becomes admin (passwordless generalization of the
// advisory-locked first-admin path; review B2).
func TestOIDCCallbackFirstUserBecomesAdminLiveDB(t *testing.T) {
	pool := oidcLivePool(t)
	resetUsers(t, pool)
	f := newSigningIDP(t)
	defer f.srv.Close()
	h := oidcLiveHandler(t, pool, f)

	f.sub = "sub-first-" + oidcUniq(t)
	f.email = "first-" + oidcUniq(t) + "@example.com"
	f.emailVerified = true

	rec := callbackFor(t, h, f, true)
	assertLoggedInRedirect(t, rec)

	got, err := store.New(pool).GetUserByEmail(context.Background(), f.email)
	if err != nil {
		t.Fatalf("first user not found: %v", err)
	}
	if !got.IsAdmin {
		t.Error("the first-ever OIDC user must be admin")
	}
	if got.PasswordHash.Valid {
		t.Error("first admin created via OIDC must be passwordless")
	}
}

// TestOIDCCallbackDeactivatedLiveDB: a subject-match login for a deactivated
// account is rejected with oidc_deactivated, never a session (review N5).
func TestOIDCCallbackDeactivatedLiveDB(t *testing.T) {
	pool := oidcLivePool(t)
	ctx := context.Background()
	f := newSigningIDP(t)
	defer f.srv.Close()
	q := store.New(pool)

	sub := "sub-deact-" + oidcUniq(t)
	email := "deact-" + oidcUniq(t) + "@example.com"
	u, err := q.CreateUserOIDC(ctx, store.CreateUserOIDCParams{Email: email, OidcIssuer: oidcTxt(f.issuer), OidcSubject: oidcTxt(sub)})
	if err != nil {
		t.Fatalf("seed oidc user: %v", err)
	}
	if _, err := q.SetUserActive(ctx, store.SetUserActiveParams{ID: u.ID, IsActive: false}); err != nil {
		t.Fatalf("deactivate: %v", err)
	}

	h := oidcLiveHandler(t, pool, f)
	f.sub = sub
	f.email = email
	f.emailVerified = true

	rec := callbackFor(t, h, f, true)
	assertErrorRedirect(t, rec, "oidc_deactivated")
	assertNoAuthCookie(t, rec)
}

// TestOIDCCallbackDomainRejectedLiveDB: JIT is refused (oidc_forbidden) when the
// verified email's domain is not in the allowlist, and no user is created.
func TestOIDCCallbackDomainRejectedLiveDB(t *testing.T) {
	pool := oidcLivePool(t)
	ctx := context.Background()
	f := newSigningIDP(t)
	defer f.srv.Close()
	cfg := oidcLiveCfg()
	cfg.AllowedEmailDomains = []string{"example.com"}
	h := oidcLiveHandlerWith(t, pool, f, cfg)

	f.sub = "sub-domain-" + oidcUniq(t)
	f.email = "outsider-" + oidcUniq(t) + "@example.com" // not example.com
	f.emailVerified = true

	rec := callbackFor(t, h, f, true)
	assertErrorRedirect(t, rec, "oidc_forbidden")
	assertNoAuthCookie(t, rec)
	if _, err := store.New(pool).GetUserByEmail(ctx, f.email); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("a domain-rejected login must not create a user (err=%v)", err)
	}
}

// TestOIDCCallbackRegistrationDisabledLiveDB: with registration off, JIT is refused
// (oidc_forbidden) and no user is created (linking an existing account still works,
// covered separately).
func TestOIDCCallbackRegistrationDisabledLiveDB(t *testing.T) {
	pool := oidcLivePool(t)
	ctx := context.Background()
	f := newSigningIDP(t)
	defer f.srv.Close()
	cfg := oidcLiveCfg()
	cfg.RegistrationEnabled = false
	h := oidcLiveHandlerWith(t, pool, f, cfg)

	f.sub = "sub-regoff-" + oidcUniq(t)
	f.email = "regoff-" + oidcUniq(t) + "@example.com"
	f.emailVerified = true

	rec := callbackFor(t, h, f, true)
	assertErrorRedirect(t, rec, "oidc_forbidden")
	assertNoAuthCookie(t, rec)
	if _, err := store.New(pool).GetUserByEmail(ctx, f.email); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("JIT must not create a user when registration is disabled (err=%v)", err)
	}
}

// TestOIDCCallbackEmailBoundToOtherSubjectLiveDB: an email that already belongs to a
// row bound to a DIFFERENT subject is rejected (oidc_forbidden), never re-bound —
// blind backfill would let a recycled IdP email take over an account (audit H1).
func TestOIDCCallbackEmailBoundToOtherSubjectLiveDB(t *testing.T) {
	pool := oidcLivePool(t)
	ctx := context.Background()
	f := newSigningIDP(t)
	defer f.srv.Close()
	q := store.New(pool)

	email := "bound-" + oidcUniq(t) + "@example.com"
	subjectA := "sub-A-" + oidcUniq(t)
	existing, err := q.CreateUserOIDC(ctx, store.CreateUserOIDCParams{Email: email, OidcIssuer: oidcTxt(f.issuer), OidcSubject: oidcTxt(subjectA)})
	if err != nil {
		t.Fatalf("seed subject-A user: %v", err)
	}

	h := oidcLiveHandler(t, pool, f)
	f.sub = "sub-B-" + oidcUniq(t) // a DIFFERENT subject, same email
	f.email = email
	f.emailVerified = true

	rec := callbackFor(t, h, f, true)
	assertErrorRedirect(t, rec, "oidc_forbidden")
	assertNoAuthCookie(t, rec)

	reloaded, err := q.GetUserByID(ctx, existing.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.OidcSubject.String != subjectA {
		t.Errorf("the row's subject was overwritten to %q; must stay %q", reloaded.OidcSubject.String, subjectA)
	}
}

// TestOIDCCallbackNonBooleanEmailVerifiedLiveDB: email_verified as the string
// "true" is NOT verified (audit L2) — JIT is refused and no user is created.
func TestOIDCCallbackNonBooleanEmailVerifiedLiveDB(t *testing.T) {
	pool := oidcLivePool(t)
	ctx := context.Background()
	f := newSigningIDP(t)
	defer f.srv.Close()
	h := oidcLiveHandler(t, pool, f)

	f.sub = "sub-strv-" + oidcUniq(t)
	f.email = "strv-" + oidcUniq(t) + "@example.com"
	f.emailVerified = "true" // string, not boolean

	rec := callbackFor(t, h, f, true)
	assertErrorRedirect(t, rec, "oidc_forbidden")
	assertNoAuthCookie(t, rec)
	if _, err := store.New(pool).GetUserByEmail(ctx, f.email); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("a string email_verified must not verify a JIT user (err=%v)", err)
	}
}

// TestOIDCCallbackConcurrentJITLiveDB: N callbacks racing to JIT the SAME identity
// must all end logged-in and produce exactly ONE user row — the advisory lock +
// 23505 re-fetch path (audit L6) makes concurrent first-logins converge.
func TestOIDCCallbackConcurrentJITLiveDB(t *testing.T) {
	pool := oidcLivePool(t)
	resetUsers(t, pool)
	f := newSigningIDP(t)
	defer f.srv.Close()
	h := oidcLiveHandler(t, pool, f)

	f.sub = "sub-conc-" + oidcUniq(t)
	f.email = "conc-" + oidcUniq(t) + "@example.com"
	f.emailVerified = true

	const n = 6
	codes := make([]int, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			codes[i] = callbackFor(t, h, f, true).Code
		}(i)
	}
	close(start)
	wg.Wait()

	for i, c := range codes {
		if c != http.StatusFound {
			t.Errorf("goroutine %d: callback code = %d, want 302", i, c)
		}
	}
	var count int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM users WHERE email=$1", f.email).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("concurrent JIT created %d rows for one identity, want exactly 1", count)
	}
}

// TestLoginNullHashReturns401LiveDB is the audit-H2 oracle test: logging in to an
// OIDC-only account (NULL password_hash) with any password returns a 401, never a
// 500 — so the response does not distinguish an OIDC-only account from a wrong
// password. Uses a real DB because it needs a persisted NULL-hash user.
func TestLoginNullHashReturns401LiveDB(t *testing.T) {
	pool := oidcLivePool(t)
	ctx := context.Background()
	q := store.New(pool)
	email := "oidc-only-" + oidcUniq(t) + "@example.com"
	if _, err := q.CreateUserOIDC(ctx, store.CreateUserOIDCParams{Email: email, OidcIssuer: oidcTxt("https://idp.example.com"), OidcSubject: oidcTxt("sub-" + oidcUniq(t))}); err != nil {
		t.Fatalf("create oidc-only user: %v", err)
	}

	h := &Handler{q: q, cfg: config.Config{PasswordLoginEnabled: true, JWTSecret: []byte("integration-test-jwt-secret-not-a-real-key"), AuthTokenTTL: time.Hour}}
	body, _ := json.Marshal(map[string]string{"email": email, "password": "any-password-at-all"})
	rec := httptest.NewRecorder()
	h.Login(rec, httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewReader(body)))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("login of an OIDC-only account: code = %d, want 401 (never 500 — no oracle); body=%s", rec.Code, rec.Body.String())
	}
}

// TestCreateUserOIDCWritesNullPasswordHashLiveDB: a JIT/OIDC user's password_hash
// is stored as SQL NULL, never an empty string, so the NULL-hash Login branch can
// never be bypassed by an empty-hash VerifyPassword (Phase-1 wave (a)).
func TestCreateUserOIDCWritesNullPasswordHashLiveDB(t *testing.T) {
	pool := oidcLivePool(t)
	ctx := context.Background()
	u, err := store.New(pool).CreateUserOIDC(ctx, store.CreateUserOIDCParams{
		Email: "nullhash-" + oidcUniq(t) + "@example.com", OidcIssuer: oidcTxt("https://idp.example.com"), OidcSubject: oidcTxt("sub-" + oidcUniq(t)),
	})
	if err != nil {
		t.Fatalf("create oidc user: %v", err)
	}
	var ph pgtype.Text
	if err := pool.QueryRow(ctx, "SELECT password_hash FROM users WHERE id=$1", u.ID).Scan(&ph); err != nil {
		t.Fatalf("select password_hash: %v", err)
	}
	if ph.Valid {
		t.Errorf("password_hash = %q (non-NULL); an OIDC user must have SQL NULL, never an empty string", ph.String)
	}
}
