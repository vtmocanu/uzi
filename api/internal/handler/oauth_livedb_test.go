package handler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/auth"
	"github.com/vtmocanu/uzi/api/internal/oauthsrv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #1910 M2: the consent half of the OAuth server through the PRODUCTION router (h.Routes via
// v1Routers). Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.

const (
	oauthTestIssuer   = "https://uzi.example.test"
	oauthTestRedirect = "https://p.example.test/cb"
	oauthTestState    = "state with spaces & symbols/=?"
)

// oauthTestVerifier is the RFC 7636 appendix B code verifier, assembled from fragments so no
// secret-shaped literal sits in tracked source (the secret scan flags the joined form).
var oauthTestVerifier = strings.Join([]string{"dBjftJeZ4C", "VP-mB92K27", "uhbUJU1p1r", "_wW1gFWFOE", "jXk"}, "")

// oauthTestChallenge is the S256 challenge of oauthTestVerifier, derived at runtime (RFC 7636
// appendix B pins the same value for this verifier).
var oauthTestChallenge = func() string {
	sum := sha256.Sum256([]byte(oauthTestVerifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}()

type oauthEnv struct {
	h         *Handler
	pool      *pgxpool.Pool
	routes    http.Handler
	product   uuid.UUID
	user      uuid.UUID
	jwt       string
	clientURI string
}

// oauthSetup builds the router (with a https FrontendOrigin so the binding cookie is Secure), an
// OAuth-client product allowed both scopes, and a signed-in user.
func oauthSetup(t *testing.T) *oauthEnv {
	t.Helper()
	h, pool := v1LiveDB(t)
	h.cfg.FrontendOrigin = oauthTestIssuer + "/"
	h.cfg.CookieSecure = true
	routes, _ := v1Routers(h)
	e := &oauthEnv{h: h, pool: pool, routes: routes, clientURI: oauthTestRedirect}
	admin := cliSeedUser(t, pool, true)
	e.product = oauthSeedClient(t, h, admin, []string{oauthTestRedirect, "http://127.0.0.1:9090/cb?tenant=a"}, []string{"jobs:run", "jobs:read"})
	e.user = cliSeedUser(t, pool, false)
	e.jwt = cliMintJWT(t, pool, e.user)
	return e
}

func oauthSeedClient(t *testing.T, h *Handler, owner uuid.UUID, uris, scopes []string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	p, err := h.q.CreateProduct(ctx, store.CreateProductParams{
		Name: "OAuth M2 " + uuid.NewString(), Description: "plain <b>text</b> description",
		CreatedBy: pgtype.UUID{Bytes: owner, Valid: true},
	})
	if err != nil {
		t.Fatalf("CreateProduct: %v", err)
	}
	if _, err := h.q.SetProductOAuthClient(ctx, store.SetProductOAuthClientParams{ID: p.ID, RedirectUris: uris, OauthScopes: scopes}); err != nil {
		t.Fatalf("SetProductOAuthClient: %v", err)
	}
	_, hash, prefix, err := oauthsrv.GenerateSecret()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.q.RotateProductClientSecret(ctx, store.RotateProductClientSecretParams{ID: p.ID, ClientSecretHash: hash, ClientSecretPrefix: prefix}); err != nil {
		t.Fatalf("RotateProductClientSecret: %v", err)
	}
	return p.ID
}

func (e *oauthEnv) authorizeQuery(mut func(url.Values)) url.Values {
	v := url.Values{
		"client_id":             {e.product.String()},
		"redirect_uri":          {e.clientURI},
		"response_type":         {"code"},
		"code_challenge":        {oauthTestChallenge},
		"code_challenge_method": {"S256"},
		"state":                 {oauthTestState},
	}
	if mut != nil {
		mut(v)
	}
	return v
}

// authorize drives GET /api/oauth/authorize as a browser holding binding (nil = none).
func (e *oauthEnv) authorize(t *testing.T, q url.Values, binding *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	return e.authorizeFrom(t, q, binding, "")
}

// authorizeFrom is authorize from a given peer address (RemoteAddr, "" = httptest's default). The
// test Handler has no trusted proxies, so the peer address is the client address.
func (e *oauthEnv) authorizeFrom(t *testing.T, q url.Values, binding *http.Cookie, remoteAddr string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/oauth/authorize?"+q.Encode(), nil)
	if remoteAddr != "" {
		req.RemoteAddr = remoteAddr
	}
	if binding != nil {
		req.AddCookie(binding)
	}
	rec := httptest.NewRecorder()
	e.routes.ServeHTTP(rec, req)
	return rec
}

func oauthBindingCookie(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == oauthBindCookieName {
			return c
		}
	}
	t.Fatalf("no %s cookie on the response", oauthBindCookieName)
	return nil
}

// start runs a successful authorize and returns the pending request id and the binding cookie.
func (e *oauthEnv) start(t *testing.T, binding *http.Cookie, mut func(url.Values)) (string, *http.Cookie) {
	t.Helper()
	rec := e.authorize(t, e.authorizeQuery(mut), binding)
	if rec.Code != http.StatusFound {
		t.Fatalf("authorize = %d, want 302\n%s", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	id, ok := strings.CutPrefix(loc, "/connect?request=")
	if !ok {
		t.Fatalf("Location = %q, want /connect?request=<id>", loc)
	}
	if _, err := uuid.Parse(id); err != nil {
		t.Fatalf("request id %q: %v", id, err)
	}
	return id, oauthBindingCookie(t, rec)
}

// consent calls one of the /requests routes as the signed-in user, with the given binding cookie.
func (e *oauthEnv) consent(t *testing.T, method, id, action, jwt string, binding *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	path := "/api/oauth/requests/" + id + action
	req := httptest.NewRequest(method, path, nil)
	if jwt != "" {
		req.AddCookie(&http.Cookie{Name: auth.AuthCookieName, Value: jwt}) //nolint:gosec // G124: test-only client cookie on an httptest request.
		req.Header.Set(auth.CSRFHeaderName, cliCSRFHeader(t, jwt))
	}
	if binding != nil {
		req.AddCookie(&http.Cookie{Name: binding.Name, Value: binding.Value}) //nolint:gosec // G124: test-only client cookie on an httptest request.
	}
	rec := httptest.NewRecorder()
	e.routes.ServeHTTP(rec, req)
	return rec
}

func decodeRedirect(t *testing.T, rec *httptest.ResponseRecorder) *url.URL {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	var resp apitypes.OAuthRedirectResponse
	dec := json.NewDecoder(bytes.NewReader(rec.Body.Bytes()))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&resp); err != nil {
		t.Fatalf("decode redirect response: %v", err)
	}
	u, err := url.Parse(resp.RedirectURL)
	if err != nil {
		t.Fatalf("redirect_url %q: %v", resp.RedirectURL, err)
	}
	return u
}

func (e *oauthEnv) requestRow(t *testing.T, id string) store.OauthAuthorizeRequest {
	t.Helper()
	rid, _ := uuid.Parse(id)
	row, err := e.h.q.GetOAuthAuthorizeRequest(context.Background(), rid)
	if err != nil {
		t.Fatalf("read request row: %v", err)
	}
	return row
}

func (e *oauthEnv) countRequests(t *testing.T) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM oauth_authorize_requests WHERE product_id = $1`, e.product).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (e *oauthEnv) liveGrant(t *testing.T) store.OauthGrant {
	t.Helper()
	var g store.OauthGrant
	err := e.pool.QueryRow(context.Background(),
		`SELECT id, user_id, product_id, scopes, refresh_token_hash, refresh_token_prefix, refresh_issued_at, refresh_last_used_at, created_at, consented_at, revoked_at
		   FROM oauth_grants WHERE user_id = $1 AND product_id = $2 AND revoked_at IS NULL`, e.user, e.product).
		Scan(&g.ID, &g.UserID, &g.ProductID, &g.Scopes, &g.RefreshTokenHash, &g.RefreshTokenPrefix, &g.RefreshIssuedAt, &g.RefreshLastUsedAt, &g.CreatedAt, &g.ConsentedAt, &g.RevokedAt)
	if err != nil {
		t.Fatalf("read live grant: %v", err)
	}
	return g
}

// seedGrantToken inserts an access-token row bound to the grant, as the token endpoint does.
func (e *oauthEnv) seedGrantToken(t *testing.T, grant uuid.UUID, scopes []string) uuid.UUID {
	t.Helper()
	sum := sha256.Sum256([]byte(uuid.NewString()))
	var id uuid.UUID
	err := e.pool.QueryRow(context.Background(),
		`INSERT INTO product_tokens (user_id, product_id, name, token_hash, token_prefix, scopes, expires_at, grant_id)
		 VALUES ($1, $2, 'oauth access', $3, 'uzp_test', $4, now() + interval '1 hour', $5) RETURNING id`,
		e.user, e.product, sum[:], scopes, grant).Scan(&id)
	if err != nil {
		t.Fatalf("seed grant token: %v", err)
	}
	return id
}

func (e *oauthEnv) tokenRevoked(t *testing.T, id uuid.UUID) bool {
	t.Helper()
	var revoked bool
	if err := e.pool.QueryRow(context.Background(), `SELECT revoked FROM product_tokens WHERE id = $1`, id).Scan(&revoked); err != nil {
		t.Fatal(err)
	}
	return revoked
}

func TestOAuthAuthorizeStaticErrorLiveDB(t *testing.T) {
	e := oauthSetup(t)
	ctx := context.Background()
	admin := cliSeedUser(t, e.pool, true)
	noSecret := oauthSeedClient(t, e.h, admin, []string{oauthTestRedirect}, []string{"jobs:run"})
	cliMustExec(t, e.pool, `UPDATE products SET client_secret_hash = NULL, client_secret_prefix = NULL WHERE id = $1`, noSecret)
	disabled := oauthSeedClient(t, e.h, admin, []string{oauthTestRedirect}, []string{"jobs:run"})
	cliMustExec(t, e.pool, `UPDATE products SET enabled = false WHERE id = $1`, disabled)
	deleted := oauthSeedClient(t, e.h, admin, []string{oauthTestRedirect}, []string{"jobs:run"})
	if _, err := e.h.q.SoftDeleteProduct(ctx, deleted); err != nil {
		t.Fatal(err)
	}
	plain, err := e.h.q.CreateProduct(ctx, store.CreateProductParams{Name: "Plain " + uuid.NewString(), CreatedBy: pgtype.UUID{Bytes: admin, Valid: true}})
	if err != nil {
		t.Fatal(err)
	}

	const uriSentinel = "https://evil.example/REDIRECT-SENTINEL"
	const stateSentinel = "STATE-SENTINEL-77"
	tests := []struct {
		name string
		mut  func(url.Values)
	}{
		{"unknown client", func(v url.Values) { v.Set("client_id", uuid.NewString()) }},
		{"disabled product", func(v url.Values) { v.Set("client_id", disabled.String()) }},
		{"deleted product", func(v url.Values) { v.Set("client_id", deleted.String()) }},
		{"product without a secret", func(v url.Values) { v.Set("client_id", noSecret.String()) }},
		{"product that is not a client", func(v url.Values) { v.Set("client_id", plain.ID.String()) }},
		{"unregistered redirect_uri", func(v url.Values) { v.Set("redirect_uri", uriSentinel) }},
		{"redirect_uri differs by trailing slash", func(v url.Values) { v.Set("redirect_uri", oauthTestRedirect+"/") }},
		{"redirect_uri missing", func(v url.Values) { v.Del("redirect_uri") }},
		{"redirect_uri repeated", func(v url.Values) { v["redirect_uri"] = []string{oauthTestRedirect, oauthTestRedirect} }},
		{"unknown client with a bad scope and PKCE too", func(v url.Values) {
			v.Set("client_id", uuid.NewString())
			v.Set("scope", "bogus")
			v.Del("code_challenge")
		}},
	}
	before := e.countRequests(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q := e.authorizeQuery(func(v url.Values) {
				v.Set("state", stateSentinel)
				tt.mut(v)
			})
			if q.Get("redirect_uri") == "" {
				// keep a sentinel on the wire even when the case removed redirect_uri
				q.Set("evil", uriSentinel)
			}
			rec := e.authorize(t, q, nil)
			assertOAuthStaticError(t, rec, stateSentinel, uriSentinel, e.product.String())
		})
	}
	if after := e.countRequests(t); after != before {
		t.Errorf("a rejected client stored %d request rows", after-before)
	}
}

func TestOAuthAuthorizeRejectsRedirectToRegisteredURILiveDB(t *testing.T) {
	e := oauthSetup(t)
	tests := []struct {
		name      string
		mut       func(url.Values)
		wantErr   string
		wantState string
	}{
		{"response_type token", func(v url.Values) { v.Set("response_type", "token") }, "unsupported_response_type", oauthTestState},
		{"pkce plain", func(v url.Values) { v.Set("code_challenge_method", "plain") }, "invalid_request", oauthTestState},
		{"pkce missing", func(v url.Values) { v.Del("code_challenge"); v.Del("code_challenge_method") }, "invalid_request", oauthTestState},
		{"state missing", func(v url.Values) { v.Del("state") }, "invalid_request", ""},
		{"state too long", func(v url.Values) { v.Set("state", strings.Repeat("s", 513)) }, "invalid_request", ""},
		{"scope unknown", func(v url.Values) { v.Set("scope", "jobs:admin") }, "invalid_scope", oauthTestState},
		{"repeated parameter", func(v url.Values) { v["scope"] = []string{"jobs:run", "jobs:read"} }, "invalid_request", oauthTestState},
	}
	before := e.countRequests(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := e.authorize(t, e.authorizeQuery(tt.mut), nil)
			if rec.Code != http.StatusFound {
				t.Fatalf("status = %d, want 302", rec.Code)
			}
			if sc := rec.Header()["Set-Cookie"]; len(sc) != 0 {
				t.Errorf("an error redirect must not set the binding cookie: %v", sc)
			}
			loc, err := url.Parse(rec.Header().Get("Location"))
			if err != nil {
				t.Fatal(err)
			}
			if loc.Scheme+"://"+loc.Host+loc.Path != oauthTestRedirect || loc.Fragment != "" {
				t.Fatalf("Location = %s, want the registered URI", loc)
			}
			q := loc.Query()
			if q.Get("error") != tt.wantErr || q.Get("iss") != oauthTestIssuer || q.Get("state") != tt.wantState {
				t.Errorf("redirect query = %v, want error=%s iss=%s state=%q", q, tt.wantErr, oauthTestIssuer, tt.wantState)
			}
			if q.Has("code") {
				t.Error("an error redirect carries no code")
			}
		})
	}
	if after := e.countRequests(t); after != before {
		t.Errorf("a rejected request stored %d rows", after-before)
	}
}

func TestOAuthAuthorizeSuccessStoresHashAndSetsCookieLiveDB(t *testing.T) {
	e := oauthSetup(t)
	rec := e.authorize(t, e.authorizeQuery(func(v url.Values) { v.Set("scope", "jobs:read") }), nil)
	if rec.Code != http.StatusFound {
		t.Fatalf("authorize = %d", rec.Code)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q", cc)
	}
	id, _ := strings.CutPrefix(rec.Header().Get("Location"), "/connect?request=")
	c := oauthBindingCookie(t, rec)
	if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode || c.Path != "/api/oauth" || c.MaxAge <= 0 {
		t.Errorf("binding cookie attributes = %+v, want HttpOnly Secure SameSite=Lax Path=/api/oauth with a Max-Age", c)
	}
	if len(c.Value) != 43 {
		t.Errorf("binding nonce is %d chars, want 43 (256 bits, base64url)", len(c.Value))
	}

	row := e.requestRow(t, id)
	wantHash := sha256.Sum256([]byte(c.Value))
	if !bytes.Equal(row.BindingHash, wantHash[:]) {
		t.Error("binding_hash is not sha256 of the cookie nonce")
	}
	if bytes.Contains(row.BindingHash, []byte(c.Value)) {
		t.Error("the plaintext nonce is stored")
	}
	var anyPlain bool
	if err := e.pool.QueryRow(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM oauth_authorize_requests WHERE id = $1 AND (redirect_uri = $2 OR state = $2 OR code_challenge = $2))`, id, c.Value).Scan(&anyPlain); err != nil || anyPlain {
		t.Errorf("the plaintext nonce appears in a text column (err %v)", err)
	}
	if row.Status != "pending" || row.ProductID != e.product || row.RedirectUri != oauthTestRedirect || row.State != oauthTestState ||
		row.CodeChallenge != oauthTestChallenge || strings.Join(row.Scopes, " ") != "jobs:read" || row.UserID.Valid || row.CodeHash != nil {
		t.Errorf("stored row = %+v", row)
	}
	if d := time.Until(row.ExpiresAt.Time); d < 4*time.Minute || d > 5*time.Minute+5*time.Second {
		t.Errorf("expires in %s, want about 5 minutes", d)
	}

	// A second authorize from the same browser reuses the cookie nonce, so both tabs work.
	id2, c2 := e.start(t, c, nil)
	if c2.Value != c.Value {
		t.Error("a well-formed binding cookie must be reused, not replaced")
	}
	if g := e.consent(t, http.MethodGet, id, "", e.jwt, c); g.Code != http.StatusOK {
		t.Errorf("first request with the shared cookie = %d", g.Code)
	}
	if g := e.consent(t, http.MethodGet, id2, "", e.jwt, c); g.Code != http.StatusOK {
		t.Errorf("second request with the shared cookie = %d", g.Code)
	}
	// A malformed cookie is replaced.
	short := &http.Cookie{Name: oauthBindCookieName, Value: "short"} //nolint:gosec // G124: test-only client cookie on an httptest request.
	_, c3 := e.start(t, short, nil)
	if c3.Value == "short" || len(c3.Value) != 43 {
		t.Errorf("a malformed cookie must be replaced, got %q", c3.Value)
	}
}

func TestOAuthRequestMetadataLiveDB(t *testing.T) {
	e := oauthSetup(t)
	id, c := e.start(t, nil, func(v url.Values) { v.Set("scope", "jobs:run") })
	rec := e.consent(t, http.MethodGet, id, "", e.jwt, c)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d %s", rec.Code, rec.Body.String())
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q", cc)
	}
	var dto apitypes.OAuthAuthorizeRequestDTO
	dec := json.NewDecoder(bytes.NewReader(rec.Body.Bytes()))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&dto); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(dto.ProductName, "OAuth M2 ") || dto.ProductDescription != "plain <b>text</b> description" ||
		dto.RedirectHost != "p.example.test" || strings.Join(dto.Scopes, " ") != "jobs:run" || dto.Status != "pending" || dto.ExpiresAt.IsZero() {
		t.Errorf("metadata = %+v", dto)
	}
	body := rec.Body.String()
	for _, secret := range []string{oauthTestChallenge, oauthTestRedirect, oauthTestState, c.Value} {
		if strings.Contains(body, secret) {
			t.Errorf("metadata leaks %q", secret)
		}
	}
	// Signed out: the cookie session is required.
	if rec := e.consent(t, http.MethodGet, id, "", "", c); rec.Code != http.StatusUnauthorized {
		t.Errorf("no session = %d, want 401", rec.Code)
	}
}

// TestOAuthConsentFixationLiveDB: a request created with browser A's binding cookie cannot be
// read, approved or denied by browser B (another cookie, or none), and every refusal is the same
// 404 (PRD #1910 D3).
func TestOAuthConsentFixationLiveDB(t *testing.T) {
	e := oauthSetup(t)
	id, a := e.start(t, nil, nil)
	b := &http.Cookie{Name: oauthBindCookieName, Value: strings.Repeat("B", 43)} //nolint:gosec // G124: test-only client cookie on an httptest request.
	_, other := e.start(t, nil, nil)                                             // a real cookie of a different browser, bound to a different request
	if other.Value == a.Value {
		t.Fatal("two fresh browsers must get different nonces")
	}
	malformed := &http.Cookie{Name: oauthBindCookieName, Value: "x"} //nolint:gosec // G124: test-only client cookie on an httptest request.
	routes := []struct{ method, action string }{{http.MethodGet, ""}, {http.MethodPost, "/approve"}, {http.MethodPost, "/deny"}}
	var firstBody string
	for _, r := range routes {
		for name, cookie := range map[string]*http.Cookie{"none": nil, "forged": b, "another browser": other, "malformed": malformed} {
			rec := e.consent(t, r.method, id, r.action, e.jwt, cookie)
			if rec.Code != http.StatusNotFound {
				t.Errorf("%s %s with %s binding = %d, want 404", r.method, r.action, name, rec.Code)
			}
			if firstBody == "" {
				firstBody = rec.Body.String()
			} else if rec.Body.String() != firstBody {
				t.Errorf("%s %s with %s binding answers a distinguishable body %q", r.method, r.action, name, rec.Body.String())
			}
		}
	}
	if st := e.requestRow(t, id).Status; st != "pending" {
		t.Fatalf("a fixation attempt changed the request to %q", st)
	}
	// Unknown ids and non-uuid ids are the same 404.
	for _, bad := range []string{uuid.NewString(), "not-a-uuid"} {
		if rec := e.consent(t, http.MethodGet, bad, "", e.jwt, a); rec.Code != http.StatusNotFound || rec.Body.String() != firstBody {
			t.Errorf("id %q = %d %q", bad, rec.Code, rec.Body.String())
		}
	}
	// The real browser still works.
	if rec := e.consent(t, http.MethodPost, id, "/approve", e.jwt, a); rec.Code != http.StatusOK {
		t.Fatalf("approve with the right binding = %d %s", rec.Code, rec.Body.String())
	}
}

func TestOAuthApproveRedirectCarriesCodeStateIssLiveDB(t *testing.T) {
	e := oauthSetup(t)
	id, c := e.start(t, nil, nil)
	rec := e.consent(t, http.MethodPost, id, "/approve", e.jwt, c)
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("approve Cache-Control = %q, want no-store", cc)
	}
	u := decodeRedirect(t, rec)
	if u.Scheme+"://"+u.Host+u.Path != oauthTestRedirect || u.Fragment != "" {
		t.Fatalf("redirect_url = %s, want the registered URI", u)
	}
	q := u.Query()
	code := q.Get("code")
	if len(code) != 43 || q.Get("state") != oauthTestState || q.Get("iss") != oauthTestIssuer || len(q) != 3 {
		t.Fatalf("redirect query = %v", q)
	}
	row := e.requestRow(t, id)
	want := sha256.Sum256([]byte(code))
	if row.Status != "approved" || !bytes.Equal(row.CodeHash, want[:]) || !row.GrantID.Valid || !row.UserID.Valid || uuid.UUID(row.UserID.Bytes) != e.user {
		t.Errorf("row after approve = %+v", row)
	}
	if d := time.Until(row.CodeExpiresAt.Time); d < 50*time.Second || d > 61*time.Second {
		t.Errorf("code expires in %s, want about 60s", d)
	}
	var plain bool
	if err := e.pool.QueryRow(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM oauth_authorize_requests WHERE redirect_uri = $1 OR state = $1 OR code_challenge = $1)`, code).Scan(&plain); err != nil || plain {
		t.Errorf("the plaintext code is stored (err %v)", err)
	}
	g := e.liveGrant(t)
	if strings.Join(g.Scopes, " ") != "jobs:run jobs:read" || uuid.UUID(row.GrantID.Bytes) != g.ID || g.RefreshTokenHash != nil || time.Since(g.ConsentedAt.Time) > time.Minute {
		t.Errorf("grant = %+v", g)
	}
	// The metadata read now reports the new status.
	var dto apitypes.OAuthAuthorizeRequestDTO
	if err := json.Unmarshal(e.consent(t, http.MethodGet, id, "", e.jwt, c).Body.Bytes(), &dto); err != nil || dto.Status != "approved" {
		t.Errorf("status after approve = %q (%v)", dto.Status, err)
	}
}

func TestOAuthApproveRedirectKeepsRegisteredQueryLiveDB(t *testing.T) {
	e := oauthSetup(t)
	e.clientURI = "http://127.0.0.1:9090/cb?tenant=a"
	id, c := e.start(t, nil, nil)
	u := decodeRedirect(t, e.consent(t, http.MethodPost, id, "/approve", e.jwt, c))
	q := u.Query()
	if u.Host != "127.0.0.1:9090" || q.Get("tenant") != "a" || q.Get("code") == "" || q.Get("iss") != oauthTestIssuer {
		t.Errorf("redirect_url = %s", u)
	}
}

func TestOAuthDoubleApproveAndDenyLiveDB(t *testing.T) {
	e := oauthSetup(t)

	t.Run("second approve is 409", func(t *testing.T) {
		id, c := e.start(t, nil, nil)
		if rec := e.consent(t, http.MethodPost, id, "/approve", e.jwt, c); rec.Code != http.StatusOK {
			t.Fatalf("approve = %d", rec.Code)
		}
		if rec := e.consent(t, http.MethodPost, id, "/approve", e.jwt, c); rec.Code != http.StatusConflict {
			t.Errorf("second approve = %d, want 409", rec.Code)
		}
		if rec := e.consent(t, http.MethodPost, id, "/deny", e.jwt, c); rec.Code != http.StatusConflict {
			t.Errorf("deny after approve = %d, want 409", rec.Code)
		}
		if st := e.requestRow(t, id).Status; st != "approved" {
			t.Errorf("status = %q, want approved", st)
		}
	})

	t.Run("deny then approve or deny is 409 and the redirect is access_denied", func(t *testing.T) {
		id, c := e.start(t, nil, nil)
		rec := e.consent(t, http.MethodPost, id, "/deny", e.jwt, c)
		if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
			t.Errorf("deny Cache-Control = %q, want no-store", cc)
		}
		u := decodeRedirect(t, rec)
		q := u.Query()
		if u.Scheme+"://"+u.Host+u.Path != oauthTestRedirect || q.Get("error") != "access_denied" || q.Get("state") != oauthTestState || q.Get("iss") != oauthTestIssuer || q.Has("code") {
			t.Errorf("deny redirect_url = %s", u)
		}
		if rec := e.consent(t, http.MethodPost, id, "/approve", e.jwt, c); rec.Code != http.StatusConflict {
			t.Errorf("approve after deny = %d, want 409", rec.Code)
		}
		if rec := e.consent(t, http.MethodPost, id, "/deny", e.jwt, c); rec.Code != http.StatusConflict {
			t.Errorf("second deny = %d, want 409", rec.Code)
		}
		row := e.requestRow(t, id)
		if row.Status != "denied" || row.CodeHash != nil {
			t.Errorf("row = %+v", row)
		}
	})

	t.Run("an expired request is the same 404 and cannot be approved", func(t *testing.T) {
		id, c := e.start(t, nil, nil)
		cliMustExec(t, e.pool, `UPDATE oauth_authorize_requests SET expires_at = now() - interval '1 second' WHERE id = $1`, id)
		for _, r := range []struct{ method, action string }{{http.MethodGet, ""}, {http.MethodPost, "/approve"}, {http.MethodPost, "/deny"}} {
			if rec := e.consent(t, r.method, id, r.action, e.jwt, c); rec.Code != http.StatusNotFound {
				t.Errorf("%s %s on an expired request = %d, want 404", r.method, r.action, rec.Code)
			}
		}
		if st := e.requestRow(t, id).Status; st != "pending" {
			t.Errorf("status = %q", st)
		}
	})
}

// TestOAuthConcurrentApproveLiveDB: of many simultaneous approves of one request exactly one
// wins; the rest are 409, and exactly one grant results. A loser may supersede the grant's
// earlier codes (including the winner's) before its claim of the request fails; the rollback
// that restores the winner is asserted deterministically by
// TestOAuthLosingApproveRollsBackSupersedeLiveDB.
func TestOAuthConcurrentApproveLiveDB(t *testing.T) {
	e := oauthSetup(t)
	id, c := e.start(t, nil, nil)
	const n = 8
	codes := make([]int, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			codes[i] = e.consent(t, http.MethodPost, id, "/approve", e.jwt, c).Code
		}()
	}
	close(start)
	wg.Wait()
	wins, conflicts := 0, 0
	for _, code := range codes {
		switch code {
		case http.StatusOK:
			wins++
		case http.StatusConflict:
			conflicts++
		}
	}
	if wins != 1 || conflicts != n-1 {
		t.Fatalf("concurrent approves = %v, want exactly one 200 and %d 409s", codes, n-1)
	}
	var grants int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM oauth_grants WHERE user_id = $1 AND product_id = $2`, e.user, e.product).Scan(&grants); err != nil || grants != 1 {
		t.Errorf("grants = %d (%v), want 1", grants, err)
	}
	if row := e.requestRow(t, id); row.Status != "approved" || row.CodeHash == nil {
		t.Errorf("winning request = status %q, code present %t, want approved with a code", row.Status, row.CodeHash != nil)
	}
}

// TestOAuthLosingApproveRollsBackSupersedeLiveDB: an approve that reaches its claim AFTER another
// decision committed answers 409 and leaves that decision intact. Deterministic: with a live grant
// present, a test transaction holds the grant's row lock while the handler's approve of request R
// loads R as pending and blocks on that lock; the test then approves R itself (claim, code) and
// commits. The handler resumes, supersedes the grant's approved codes (R's, now) in its own
// transaction, loses the claim (409), and only its rollback keeps R approved with its code.
func TestOAuthLosingApproveRollsBackSupersedeLiveDB(t *testing.T) {
	e := oauthSetup(t)
	ctx := context.Background()
	id0, c := e.start(t, nil, nil)
	if rec := e.consent(t, http.MethodPost, id0, "/approve", e.jwt, c); rec.Code != http.StatusOK {
		t.Fatalf("first approve = %d", rec.Code)
	}
	grant := e.liveGrant(t)
	id, c := e.start(t, c, nil)
	rid, _ := uuid.Parse(id)

	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit
	if _, err := tx.Exec(ctx, `SELECT 1 FROM oauth_grants WHERE id = $1 FOR UPDATE`, grant.ID); err != nil {
		t.Fatal(err)
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- e.consent(t, http.MethodPost, id, "/approve", e.jwt, c) }()
	// Wait until the handler is queued behind the grant lock.
	var waiting int
	for i := 0; i < 200 && waiting == 0; i++ {
		if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM pg_locks WHERE NOT granted`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting == 0 {
			time.Sleep(25 * time.Millisecond)
		}
	}
	if waiting == 0 {
		t.Fatal("the handler's approve never blocked on the grant lock")
	}
	qtx := e.h.q.WithTx(tx)
	userID := pgtype.UUID{Bytes: e.user, Valid: true}
	if _, err := qtx.ClaimOAuthAuthorizeRequest(ctx, store.ClaimOAuthAuthorizeRequestParams{ID: rid, UserID: userID}); err != nil {
		t.Fatal(err)
	}
	winnerHash := sha256.Sum256([]byte("winner code " + id))
	if _, err := qtx.IssueOAuthAuthorizationCode(ctx, store.IssueOAuthAuthorizationCodeParams{
		ID: rid, UserID: userID, GrantID: pgtype.UUID{Bytes: grant.ID, Valid: true},
		CodeHash: winnerHash[:], CodeExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Minute), Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case rec := <-done:
		if rec.Code != http.StatusConflict {
			t.Fatalf("losing approve = %d %s, want 409", rec.Code, rec.Body.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the losing approve never returned")
	}
	row := e.requestRow(t, id)
	if row.Status != "approved" || !bytes.Equal(row.CodeHash, winnerHash[:]) {
		t.Errorf("after the losing approve: status %q, code hash kept %t, want approved with the winner's hash", row.Status, bytes.Equal(row.CodeHash, winnerHash[:]))
	}
}

// TestOAuthConcurrentFirstConsentsShareOneGrantLiveDB: simultaneous approves of different
// requests by one user for one product converge on a single live grant (the insert-or-lock path).
func TestOAuthConcurrentFirstConsentsShareOneGrantLiveDB(t *testing.T) {
	e := oauthSetup(t)
	const n = 6
	type req struct {
		id string
		c  *http.Cookie
	}
	reqs := make([]req, n)
	for i := range reqs {
		id, c := e.start(t, nil, nil)
		reqs[i] = req{id, c}
	}
	codes := make([]int, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range reqs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			codes[i] = e.consent(t, http.MethodPost, reqs[i].id, "/approve", e.jwt, reqs[i].c).Code
		}()
	}
	close(start)
	wg.Wait()
	for i, code := range codes {
		if code != http.StatusOK {
			t.Fatalf("approve %d = %d, want 200 (all %v)", i, code, codes)
		}
	}
	var live, approved, superseded int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM oauth_grants WHERE user_id = $1 AND product_id = $2 AND revoked_at IS NULL`, e.user, e.product).Scan(&live); err != nil || live != 1 {
		t.Errorf("live grants = %d (%v), want 1", live, err)
	}
	if err := e.pool.QueryRow(context.Background(),
		`SELECT count(*) FILTER (WHERE status = 'approved'), count(*) FILTER (WHERE status = 'superseded') FROM oauth_authorize_requests WHERE user_id = $1 AND product_id = $2`, e.user, e.product).Scan(&approved, &superseded); err != nil {
		t.Fatal(err)
	}
	if approved+superseded != n {
		t.Errorf("approved %d + superseded %d != %d", approved, superseded, n)
	}
	if approved > 1 {
		// Each approve supersedes the grant's earlier unredeemed codes under the grant lock, so
		// however they interleave only the last one can remain live.
		t.Errorf("approved (live) codes = %d, want at most 1", approved)
	}
}

func TestOAuthReconsentLiveDB(t *testing.T) {
	approveWith := func(t *testing.T, e *oauthEnv, scope string) (id string, redirect *url.URL) {
		t.Helper()
		id, c := e.start(t, nil, func(v url.Values) {
			if scope == "" {
				v.Del("scope")
			} else {
				v.Set("scope", scope)
			}
		})
		return id, decodeRedirect(t, e.consent(t, http.MethodPost, id, "/approve", e.jwt, c))
	}
	setRefresh := func(t *testing.T, e *oauthEnv, grant uuid.UUID) {
		t.Helper()
		cliMustExec(t, e.pool,
			`UPDATE oauth_grants SET refresh_token_hash = $2, refresh_token_prefix = 'uzr_test', refresh_issued_at = now(), refresh_last_used_at = now(), consented_at = now() - interval '1 day' WHERE id = $1`,
			grant, sha256Of(uuid.NewString()))
	}

	tests := []struct {
		name        string
		firstScope  string // initial consent
		tokenScopes []string
		expired     bool   // the seeded access token is past its expiry (never revoked)
		reScope     string // re-consent
		wantScopes  string
		wantRevoked bool
	}{
		{"same scopes keep access tokens", "jobs:run", []string{"jobs:run"}, false, "jobs:run", "jobs:run", false},
		{"wider scopes keep access tokens", "jobs:run", []string{"jobs:run"}, false, "jobs:run jobs:read", "jobs:run jobs:read", false},
		{"missing scope (all allowed) is wider and keeps tokens", "jobs:read", []string{"jobs:read"}, false, "", "jobs:run jobs:read", false},
		{"narrowing past a held scope revokes access tokens", "jobs:run jobs:read", []string{"jobs:run", "jobs:read"}, false, "jobs:read", "jobs:read", true},
		{"narrowing away a scope no unrevoked token holds keeps them", "jobs:run jobs:read", []string{"jobs:read"}, false, "jobs:read", "jobs:read", false},
		// D6: an expired jobs:run token may have created a job that is still running, and the
		// job-revoke sweep cancels by revoked, not by expiry, so expiry must not hide the token.
		{"narrowing past a scope an EXPIRED token holds revokes it", "jobs:run jobs:read", []string{"jobs:run"}, true, "jobs:read", "jobs:read", true},
		{"an expired token holding only kept scopes is not revoked", "jobs:run jobs:read", []string{"jobs:read"}, true, "jobs:read", "jobs:read", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := oauthSetup(t)
			firstID, firstRedirect := approveWith(t, e, tt.firstScope)
			g1 := e.liveGrant(t)
			running := e.seedGrantToken(t, g1.ID, tt.tokenScopes)
			if tt.expired {
				cliMustExec(t, e.pool, `UPDATE product_tokens SET expires_at = now() - interval '1 minute' WHERE id = $1`, running)
			}
			setRefresh(t, e, g1.ID)
			before := e.liveGrant(t)

			_, secondRedirect := approveWith(t, e, tt.reScope)
			g2 := e.liveGrant(t)
			if g2.ID != g1.ID {
				t.Fatalf("re-consent must reuse the live grant: %s != %s", g2.ID, g1.ID)
			}
			var grants int
			if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM oauth_grants WHERE user_id = $1 AND product_id = $2`, e.user, e.product).Scan(&grants); err != nil || grants != 1 {
				t.Errorf("grants = %d (%v), want 1", grants, err)
			}
			if strings.Join(g2.Scopes, " ") != tt.wantScopes {
				t.Errorf("scopes = %v, want %q", g2.Scopes, tt.wantScopes)
			}
			if !g2.ConsentedAt.Time.After(before.ConsentedAt.Time) || time.Since(g2.ConsentedAt.Time) > time.Minute {
				t.Errorf("consented_at did not advance to now: %s", g2.ConsentedAt.Time)
			}
			if g2.RefreshTokenHash != nil || g2.RefreshTokenPrefix.Valid || g2.RefreshIssuedAt.Valid || g2.RefreshLastUsedAt.Valid {
				t.Errorf("the refresh token must be cleared by a re-consent: %+v", g2)
			}
			if got := e.tokenRevoked(t, running); got != tt.wantRevoked {
				t.Errorf("running token revoked = %v, want %v", got, tt.wantRevoked)
			}
			// The earlier unredeemed code is superseded; the new one is live.
			if st := e.requestRow(t, firstID).Status; st != "superseded" {
				t.Errorf("first code status = %q, want superseded", st)
			}
			if firstRedirect.Query().Get("code") == secondRedirect.Query().Get("code") {
				t.Error("a re-consent must issue a new code")
			}
			var live int
			if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM oauth_authorize_requests WHERE grant_id = $1 AND status = 'approved'`, g2.ID).Scan(&live); err != nil || live != 1 {
				t.Errorf("live codes = %d (%v), want 1", live, err)
			}
		})
	}
}

func sha256Of(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

// A revoked grant is not re-consented: approve creates a fresh live grant beside the old row.
func TestOAuthApproveAfterRevokedGrantCreatesNewGrantLiveDB(t *testing.T) {
	e := oauthSetup(t)
	id, c := e.start(t, nil, nil)
	decodeRedirect(t, e.consent(t, http.MethodPost, id, "/approve", e.jwt, c))
	old := e.liveGrant(t)
	cliMustExec(t, e.pool, `UPDATE oauth_grants SET revoked_at = now() WHERE id = $1`, old.ID)
	id2, c2 := e.start(t, nil, nil)
	decodeRedirect(t, e.consent(t, http.MethodPost, id2, "/approve", e.jwt, c2))
	fresh := e.liveGrant(t)
	if fresh.ID == old.ID {
		t.Fatal("a revoked grant must not be revived")
	}
}

// If the product stops being a client between authorize and approve, approve is refused and the
// request stays pending (nothing was claimed, no grant created).
func TestOAuthApproveProductChangedLiveDB(t *testing.T) {
	e := oauthSetup(t)
	id, c := e.start(t, nil, nil)
	cliMustExec(t, e.pool, `UPDATE products SET redirect_uris = '{}', oauth_scopes = '{}' WHERE id = $1`, e.product)
	if rec := e.consent(t, http.MethodPost, id, "/approve", e.jwt, c); rec.Code != http.StatusConflict {
		t.Fatalf("approve = %d, want 409", rec.Code)
	}
	if st := e.requestRow(t, id).Status; st != "pending" {
		t.Errorf("status = %q, want pending", st)
	}
	var grants int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM oauth_grants WHERE product_id = $1`, e.product).Scan(&grants); err != nil || grants != 0 {
		t.Errorf("grants = %d (%v)", grants, err)
	}
}

func TestOAuthSweepKeepsRecentCodesLiveDB(t *testing.T) {
	e := oauthSetup(t)
	ids := map[string]string{}
	for _, name := range []string{"pending_expired", "pending_live", "approved_recent", "approved_old", "denied_expired", "redeemed_old", "redeemed_recent"} {
		id, _ := e.start(t, nil, nil)
		ids[name] = id
	}
	cliMustExec(t, e.pool, `UPDATE oauth_authorize_requests SET expires_at = now() - interval '1 minute' WHERE id = ANY($1)`,
		[]string{ids["pending_expired"], ids["denied_expired"], ids["approved_recent"], ids["approved_old"], ids["redeemed_old"], ids["redeemed_recent"]})
	set := func(name, status, codeAge string) {
		cliMustExec(t, e.pool, `UPDATE oauth_authorize_requests SET status = $2, code_expires_at = CASE WHEN $3 = '' THEN NULL ELSE now() - $3::interval END WHERE id = $1`, ids[name], status, codeAge)
	}
	set("denied_expired", "denied", "")
	set("approved_recent", "approved", "2 minutes")
	set("approved_old", "approved", "11 minutes")
	set("redeemed_old", "redeemed", "11 minutes")
	set("redeemed_recent", "redeemed", "1 minute")

	if _, err := e.h.q.DeleteExpiredOAuthAuthorizeRequests(context.Background()); err != nil {
		t.Fatal(err)
	}
	wantKept := map[string]bool{"pending_expired": false, "pending_live": true, "approved_recent": true, "approved_old": false, "denied_expired": false, "redeemed_old": false, "redeemed_recent": true}
	for name, want := range wantKept {
		var exists bool
		if err := e.pool.QueryRow(context.Background(), `SELECT EXISTS (SELECT 1 FROM oauth_authorize_requests WHERE id = $1)`, ids[name]).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if exists != want {
			t.Errorf("%s kept = %v, want %v", name, exists, want)
		}
	}
}

// Deny re-checks validity like approve: a product that stopped being an enabled client, or no
// longer registers the request's redirect URI, must not get a redirect to that URI. The request
// is marked denied (it can never be approved) and the answer is the same 409 as approve's.
func TestOAuthDenyStaleRequestIsRefusedLiveDB(t *testing.T) {
	for _, tc := range []struct{ name, sql string }{
		{"product no longer a client", `UPDATE products SET redirect_uris = '{}', oauth_scopes = '{}' WHERE id = $1`},
		{"redirect URI no longer registered", `UPDATE products SET redirect_uris = ARRAY['https://other.example.test/cb'] WHERE id = $1`},
		{"product disabled", `UPDATE products SET enabled = false WHERE id = $1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := oauthSetup(t)
			id, c := e.start(t, nil, nil)
			cliMustExec(t, e.pool, tc.sql, e.product)
			rec := e.consent(t, http.MethodPost, id, "/deny", e.jwt, c)
			if rec.Code != http.StatusConflict {
				t.Fatalf("deny = %d, want 409\n%s", rec.Code, rec.Body.String())
			}
			if rec.Header().Get("Location") != "" || strings.Contains(rec.Body.String(), oauthTestRedirect) || strings.Contains(rec.Body.String(), "redirect_url") {
				t.Errorf("a stale request must not yield a redirect: %q %q", rec.Header().Get("Location"), rec.Body.String())
			}
			if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", cc)
			}
			if st := e.requestRow(t, id).Status; st != "denied" {
				t.Errorf("status = %q, want denied", st)
			}
			if rec := e.consent(t, http.MethodPost, id, "/approve", e.jwt, c); rec.Code != http.StatusConflict {
				t.Errorf("approve after a stale deny = %d, want 409", rec.Code)
			}
		})
	}
}

// oauthLockRaceWait runs consent in a goroutine and blocks until a backend waits on a lock inside
// a statement containing fragment. Ordering is observed (pg_stat_activity), never slept. The wait
// is bounded: if the request finishes without ever blocking (the unlocked behaviour) or the lock
// wait never shows, it fails cleanly instead of hanging. It returns a func yielding the response.
func oauthLockRaceWait(t *testing.T, e *tokenEnv, fragment string, consent func() *httptest.ResponseRecorder) (wait func() *httptest.ResponseRecorder) {
	t.Helper()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- consent() }()
	deadline := time.Now().Add(10 * time.Second)
	for pgWaitingCount(t, e, fragment) < 1 {
		select {
		case rec := <-done:
			done <- rec
			t.Fatalf("request finished (%d %q) without waiting on a lock in %q", rec.Code, rec.Body.String(), fragment)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("request never waited on a lock in %q", fragment)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return func() *httptest.ResponseRecorder {
		select {
		case rec := <-done:
			return rec
		case <-time.After(10 * time.Second):
			t.Fatal("request did not finish after the lock was released")
			return nil
		}
	}
}

// oauthRegistrationRemovals are admin writes that each make a request for oauthTestRedirect with
// scope jobs:run invalid.
var oauthRegistrationRemovals = []struct{ name, sql string }{
	{"redirect URI removed", `UPDATE products SET redirect_uris = ARRAY['https://other.example.test/cb'] WHERE id = $1`},
	{"client disabled", `UPDATE products SET enabled = false WHERE id = $1`},
	{"scopes narrowed past the request", `UPDATE products SET oauth_scopes = ARRAY['jobs:read'] WHERE id = $1`},
}

// beginUncommittedAdminWrite opens a pool tx, runs sql (an uncommitted admin product write) and
// registers a rollback cleanup; commit it with the returned func.
func beginUncommittedAdminWrite(t *testing.T, e *oauthEnv, sql string) (commit func()) {
	t.Helper()
	ctx := context.Background()
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	finished := false
	t.Cleanup(func() {
		if !finished {
			_ = tx.Rollback(ctx)
		}
	})
	if _, err := tx.Exec(ctx, sql, e.product); err != nil {
		t.Fatalf("admin write: %v", err)
	}
	return func() {
		finished = true
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit admin write: %v", err)
		}
	}
}

func scopeJobsRun(v url.Values) { v.Set("scope", "jobs:run") }

// An admin registration change that is uncommitted when approve starts is serialized by the
// product row FOR SHARE: approve waits, and after the commit it re-reads the new registration and
// refuses, instead of redirecting a code to the removed URI.
func TestOAuthApproveRegistrationRemovedBeforeLockedRecheckLiveDB(t *testing.T) {
	for _, tc := range oauthRegistrationRemovals {
		for _, reconsent := range []bool{false, true} {
			name := tc.name
			if reconsent {
				name += " (re-consent)"
			}
			t.Run(name, func(t *testing.T) {
				e := tokenSetup(t)
				var before store.OauthGrant
				if reconsent {
					id0, c0 := e.start(t, nil, scopeJobsRun)
					decodeRedirect(t, e.consent(t, http.MethodPost, id0, "/approve", e.jwt, c0))
					before = e.liveGrant(t)
				}
				id, c := e.start(t, nil, scopeJobsRun)
				commit := beginUncommittedAdminWrite(t, e.oauthEnv, tc.sql)
				wait := oauthLockRaceWait(t, e, "name: GetProductForShare", func() *httptest.ResponseRecorder {
					return e.consent(t, http.MethodPost, id, "/approve", e.jwt, c)
				})
				commit()
				rec := wait()
				if rec.Code != http.StatusConflict {
					t.Fatalf("approve = %d, want 409\n%s", rec.Code, rec.Body.String())
				}
				if strings.Contains(rec.Body.String(), "redirect_url") || strings.Contains(rec.Body.String(), "code=") {
					t.Errorf("a refused approve must not yield a redirect: %q", rec.Body.String())
				}
				row := e.requestRow(t, id)
				if row.Status != "pending" || row.CodeHash != nil {
					t.Errorf("request = %q code_hash=%v, want pending with no code", row.Status, row.CodeHash)
				}
				if reconsent {
					after := e.liveGrant(t)
					if after.ID != before.ID || strings.Join(after.Scopes, " ") != strings.Join(before.Scopes, " ") || !after.ConsentedAt.Time.Equal(before.ConsentedAt.Time) {
						t.Errorf("existing grant changed by a refused approve: before %+v after %+v", before, after)
					}
				} else {
					var grants int
					if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM oauth_grants WHERE product_id = $1`, e.product).Scan(&grants); err != nil || grants != 0 {
						t.Errorf("grants = %d (%v), want 0", grants, err)
					}
				}
			})
		}
	}
}

// The deny twin: the same uncommitted admin write is awaited, then the stale request is marked
// denied and answers 409 with no redirect to the removed URI.
func TestOAuthDenyRegistrationRemovedBeforeLockedRecheckLiveDB(t *testing.T) {
	for _, tc := range oauthRegistrationRemovals {
		t.Run(tc.name, func(t *testing.T) {
			e := tokenSetup(t)
			id, c := e.start(t, nil, scopeJobsRun)
			commit := beginUncommittedAdminWrite(t, e.oauthEnv, tc.sql)
			wait := oauthLockRaceWait(t, e, "name: GetProductForShare", func() *httptest.ResponseRecorder {
				return e.consent(t, http.MethodPost, id, "/deny", e.jwt, c)
			})
			commit()
			rec := wait()
			if rec.Code != http.StatusConflict {
				t.Fatalf("deny = %d, want 409\n%s", rec.Code, rec.Body.String())
			}
			if rec.Header().Get("Location") != "" || strings.Contains(rec.Body.String(), oauthTestRedirect) || strings.Contains(rec.Body.String(), "redirect_url") {
				t.Errorf("a stale deny must not yield a redirect: %q %q", rec.Header().Get("Location"), rec.Body.String())
			}
			if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", cc)
			}
			if st := e.requestRow(t, id).Status; st != "denied" {
				t.Errorf("status = %q, want denied", st)
			}
		})
	}
}

// The product share lock is held through commit: an admin registration change that starts while
// an approve is mid-transaction (blocked on the grant) waits for it, the approve still succeeds,
// and the code it issued is unredeemable once the change lands (redeem re-checks the registration).
func TestOAuthAdminRegistrationChangeWaitsForApproveLiveDB(t *testing.T) {
	e := tokenSetup(t)
	ctx := context.Background()
	id0, c0 := e.start(t, nil, nil)
	decodeRedirect(t, e.consent(t, http.MethodPost, id0, "/approve", e.jwt, c0))
	grant := e.liveGrant(t)
	id, c := e.start(t, nil, nil)

	// A test tx holds the existing grant, so approve (per-user lock, product FOR SHARE) blocks on it.
	gtx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	released := false
	t.Cleanup(func() {
		if !released {
			_ = gtx.Rollback(ctx)
		}
	})
	var one int
	if err := gtx.QueryRow(ctx, `SELECT 1 FROM oauth_grants WHERE id = $1 FOR UPDATE`, grant.ID).Scan(&one); err != nil {
		t.Fatal(err)
	}
	waitApprove := oauthLockRaceWait(t, e, "name: LockLiveOAuthGrant", func() *httptest.ResponseRecorder {
		return e.consent(t, http.MethodPost, id, "/approve", e.jwt, c)
	})

	adminDone := make(chan error, 1)
	go func() {
		_, err := e.h.q.SetProductOAuthClient(ctx, store.SetProductOAuthClientParams{
			ID: e.product, RedirectUris: []string{"https://other.example.test/cb"}, OauthScopes: []string{"jobs:run", "jobs:read"},
		})
		adminDone <- err
	}()
	deadline := time.Now().Add(10 * time.Second)
	for pgWaitingCount(t, e, "name: SetProductOAuthClient") < 1 {
		select {
		case err := <-adminDone:
			t.Fatalf("admin change finished (%v) without waiting for the in-flight approve", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("admin change never waited on a lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	var blockedByApprove bool
	if err := e.pool.QueryRow(ctx, `
		SELECT EXISTS (
		  SELECT 1 FROM pg_stat_activity adm, pg_stat_activity app
		   WHERE adm.wait_event_type = 'Lock' AND adm.query LIKE '%name: SetProductOAuthClient%'
		     AND app.wait_event_type = 'Lock' AND app.query LIKE '%name: LockLiveOAuthGrant%'
		     AND app.pid = ANY(pg_blocking_pids(adm.pid)))`).Scan(&blockedByApprove); err != nil {
		t.Fatal(err)
	}
	if !blockedByApprove {
		t.Fatal("the admin change is not blocked by the approve backend")
	}

	released = true
	if err := gtx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	rec := waitApprove()
	redirect := decodeRedirect(t, rec)
	if got := redirect.Scheme + "://" + redirect.Host + redirect.Path; got != oauthTestRedirect {
		t.Errorf("redirect = %q, want %q", got, oauthTestRedirect)
	}
	code := redirect.Query().Get("code")
	if code == "" {
		t.Fatalf("approve redirect carries no code: %s", rec.Body.String())
	}
	select {
	case err := <-adminDone:
		if err != nil {
			t.Fatalf("admin change: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("admin change did not complete after the approve committed")
	}
	var uris []string
	if err := e.pool.QueryRow(ctx, `SELECT redirect_uris FROM products WHERE id = $1`, e.product).Scan(&uris); err != nil {
		t.Fatal(err)
	}
	if len(uris) != 1 || uris[0] != "https://other.example.test/cb" {
		t.Fatalf("product redirect_uris = %v, want the admin's replacement", uris)
	}
	exch := e.exchange(t, code)
	if exch.Code != http.StatusBadRequest || !strings.Contains(exch.Body.String(), "invalid_grant") {
		t.Errorf("exchange after the registration change = %d %q, want 400 invalid_grant", exch.Code, exch.Body.String())
	}
}

// withOAuthPendingCaps lowers the authorize pending-request caps for one test.
func withOAuthPendingCaps(t *testing.T, perSource, perProduct, global int) {
	t.Helper()
	oldS, oldP, oldG := oauthPendingPerSourceCap, oauthPendingPerProductCap, oauthPendingGlobalCap
	oauthPendingPerSourceCap, oauthPendingPerProductCap, oauthPendingGlobalCap = perSource, perProduct, global
	t.Cleanup(func() { oauthPendingPerSourceCap, oauthPendingPerProductCap, oauthPendingGlobalCap = oldS, oldP, oldG })
}

// withOAuthSourceTierCaps lowers the /56 and /48 (IPv6) and /24 (IPv4) tier caps for one test.
func withOAuthSourceTierCaps(t *testing.T, v6Mid, v6Wide, v4Mid int) {
	t.Helper()
	oldM, oldW, old4 := oauthPendingV6MidCap, oauthPendingV6WideCap, oauthPendingV4MidCap
	oauthPendingV6MidCap, oauthPendingV6WideCap, oauthPendingV4MidCap = v6Mid, v6Wide, v4Mid
	t.Cleanup(func() { oauthPendingV6MidCap, oauthPendingV6WideCap, oauthPendingV4MidCap = oldM, oldW, old4 })
}

// The authorize endpoint is unauthenticated and its per-IP limiter does not bound a /64, so the
// table is bounded by a per-source cap (the fairness bound) and per-product and global storage
// backstops, checked BEFORE the insert, atomically with it. Over a cap the
// answer is an error redirect (temporarily_unavailable, state, iss) to the verified registered
// URI, and nothing is stored (and no binding cookie set).
func TestOAuthAuthorizePendingCapLiveDB(t *testing.T) {
	assertRefused := func(t *testing.T, e *oauthEnv, rec *httptest.ResponseRecorder, wantRows int) {
		t.Helper()
		if rec.Code != http.StatusFound {
			t.Fatalf("over the cap = %d, want 302\n%s", rec.Code, rec.Body.String())
		}
		u, err := url.Parse(rec.Header().Get("Location"))
		if err != nil {
			t.Fatal(err)
		}
		q := u.Query()
		if u.Scheme+"://"+u.Host+u.Path != oauthTestRedirect || q.Get("error") != oauthsrv.ErrTemporarilyUnavailable ||
			q.Get("state") != oauthTestState || q.Get("iss") != oauthTestIssuer || q.Has("code") {
			t.Errorf("Location = %s", u)
		}
		if len(rec.Result().Cookies()) != 0 {
			t.Errorf("a refused authorize must not set the binding cookie: %v", rec.Result().Cookies())
		}
		if rec.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("Cache-Control = %q", rec.Header().Get("Cache-Control"))
		}
		if got := e.countRequests(t); got != wantRows {
			t.Errorf("rows for the product = %d, want %d (nothing may be inserted over the cap)", got, wantRows)
		}
	}

	t.Run("per product", func(t *testing.T) {
		e := oauthSetup(t)
		withOAuthPendingCaps(t, 1_000_000, 2, 1_000_000)
		first, c := e.start(t, nil, nil)
		e.start(t, c, nil)
		assertRefused(t, e, e.authorize(t, e.authorizeQuery(nil), c), 2)

		// Another product is unaffected by this one's count.
		other := oauthSeedClient(t, e.h, cliSeedUser(t, e.pool, true), []string{oauthTestRedirect}, []string{"jobs:run"})
		rec := e.authorize(t, e.authorizeQuery(func(v url.Values) { v.Set("client_id", other.String()) }), nil)
		if rec.Code != http.StatusFound || !strings.HasPrefix(rec.Header().Get("Location"), "/connect?request=") {
			t.Errorf("another product = %d %q, want the consent redirect", rec.Code, rec.Header().Get("Location"))
		}

		// A decided request frees its slot.
		if rec := e.consent(t, http.MethodPost, first, "/deny", e.jwt, c); rec.Code != http.StatusOK {
			t.Fatalf("deny = %d", rec.Code)
		}
		if rec := e.authorize(t, e.authorizeQuery(nil), c); rec.Code != http.StatusFound || !strings.HasPrefix(rec.Header().Get("Location"), "/connect?request=") {
			t.Errorf("after a deny freed a slot = %d %q", rec.Code, rec.Header().Get("Location"))
		}
		assertRefused(t, e, e.authorize(t, e.authorizeQuery(nil), c), 3)

		// An expired request frees its slot too (and the sweep removes it).
		cliMustExec(t, e.pool, `UPDATE oauth_authorize_requests SET expires_at = now() - interval '1 second' WHERE product_id = $1 AND status = 'pending'`, e.product)
		if rec := e.authorize(t, e.authorizeQuery(nil), c); rec.Code != http.StatusFound || !strings.HasPrefix(rec.Header().Get("Location"), "/connect?request=") {
			t.Errorf("after the pending rows expired = %d %q", rec.Code, rec.Header().Get("Location"))
		}
	})

	t.Run("global", func(t *testing.T) {
		e := oauthSetup(t)
		var live int
		if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM oauth_authorize_requests WHERE status = 'pending' AND expires_at > now()`).Scan(&live); err != nil {
			t.Fatal(err)
		}
		withOAuthPendingCaps(t, 1_000_000, 1_000_000, live+2)
		_, c := e.start(t, nil, nil)
		e.start(t, c, nil)
		assertRefused(t, e, e.authorize(t, e.authorizeQuery(nil), c), 2)
	})

	t.Run("per source is the fairness bound", func(t *testing.T) {
		e := oauthSetup(t)
		withOAuthPendingCaps(t, 2, 1_000_000, 1_000_000)
		const a, b = "198.51.100.7:4000", "198.51.100.8:4000"
		for i := 0; i < 2; i++ {
			if rec := e.authorizeFrom(t, e.authorizeQuery(nil), nil, a); !strings.HasPrefix(rec.Header().Get("Location"), "/connect?request=") {
				t.Fatalf("source A request %d = %d %q", i, rec.Code, rec.Header().Get("Location"))
			}
		}
		assertRefused(t, e, e.authorizeFrom(t, e.authorizeQuery(nil), nil, a), 2)
		// Another source of the same product is unaffected: one source cannot lock real users out.
		if rec := e.authorizeFrom(t, e.authorizeQuery(nil), nil, b); !strings.HasPrefix(rec.Header().Get("Location"), "/connect?request=") {
			t.Errorf("source B while A is capped = %d %q, want the consent redirect", rec.Code, rec.Header().Get("Location"))
		}
		// The stored bucket is the derived prefix, not the raw address with its port.
		var n int
		if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM oauth_authorize_requests WHERE product_id = $1 AND source_prefix = '198.51.100.7'`, e.product).Scan(&n); err != nil || n != 2 {
			t.Errorf("rows with source_prefix 198.51.100.7 = %d (%v), want 2", n, err)
		}
	})

	t.Run("an IPv6 /64 is one source", func(t *testing.T) {
		e := oauthSetup(t)
		withOAuthPendingCaps(t, 2, 1_000_000, 1_000_000)
		for _, addr := range []string{"[2001:db8:1:2::1]:1", "[2001:db8:1:2:ffff::9]:1"} {
			if rec := e.authorizeFrom(t, e.authorizeQuery(nil), nil, addr); !strings.HasPrefix(rec.Header().Get("Location"), "/connect?request=") {
				t.Fatalf("%s = %d %q", addr, rec.Code, rec.Header().Get("Location"))
			}
		}
		assertRefused(t, e, e.authorizeFrom(t, e.authorizeQuery(nil), nil, "[2001:db8:1:2:abcd::5]:1"), 2)
		if rec := e.authorizeFrom(t, e.authorizeQuery(nil), nil, "[2001:db8:1:3::1]:1"); !strings.HasPrefix(rec.Header().Get("Location"), "/connect?request=") {
			t.Errorf("a different /64 = %d %q, want the consent redirect", rec.Code, rec.Header().Get("Location"))
		}
	})

	t.Run("a decided request frees its source slot", func(t *testing.T) {
		e := oauthSetup(t)
		withOAuthPendingCaps(t, 1, 1_000_000, 1_000_000)
		id, c := e.start(t, nil, nil)
		assertRefused(t, e, e.authorize(t, e.authorizeQuery(nil), c), 1)
		if rec := e.consent(t, http.MethodPost, id, "/deny", e.jwt, c); rec.Code != http.StatusOK {
			t.Fatalf("deny = %d", rec.Code)
		}
		if rec := e.authorize(t, e.authorizeQuery(nil), c); !strings.HasPrefix(rec.Header().Get("Location"), "/connect?request=") {
			t.Errorf("after a deny = %d %q", rec.Code, rec.Header().Get("Location"))
		}
	})

	accepted := func(rec *httptest.ResponseRecorder) bool {
		return strings.HasPrefix(rec.Header().Get("Location"), "/connect?request=")
	}

	t.Run("a flood from many /64s of one /56 is refused at the /56 tier", func(t *testing.T) {
		e := oauthSetup(t)
		withOAuthPendingCaps(t, 20, 1_000_000, 1_000_000)
		withOAuthSourceTierCaps(t, 3, 1_000, 1_000)
		// Three requests from three different /64s of 2001:db8:5::/56 fill its tier.
		for i, addr := range []string{"[2001:db8:5:1::1]:1", "[2001:db8:5:2::1]:1", "[2001:db8:5:3::1]:1"} {
			if rec := e.authorizeFrom(t, e.authorizeQuery(nil), nil, addr); !accepted(rec) {
				t.Fatalf("request %d from %s = %d %q", i, addr, rec.Code, rec.Header().Get("Location"))
			}
		}
		// A fourth /64 of the same /56 is refused although its own /64 is empty.
		assertRefused(t, e, e.authorizeFrom(t, e.authorizeQuery(nil), nil, "[2001:db8:5:ee::1]:1"), 3)
		// A user in another network still succeeds, so the flood did not lock the product.
		if rec := e.authorizeFrom(t, e.authorizeQuery(nil), nil, "[2001:db8:6:1::1]:1"); !accepted(rec) {
			t.Errorf("a user of another /56 = %d %q, want the consent redirect", rec.Code, rec.Header().Get("Location"))
		}
		if rec := e.authorizeFrom(t, e.authorizeQuery(nil), nil, "198.51.100.9:1"); !accepted(rec) {
			t.Errorf("an IPv4 user = %d %q, want the consent redirect", rec.Code, rec.Header().Get("Location"))
		}
	})

	t.Run("many /56s of one /48 are refused at the /48 tier", func(t *testing.T) {
		e := oauthSetup(t)
		withOAuthPendingCaps(t, 20, 1_000_000, 1_000_000)
		withOAuthSourceTierCaps(t, 1_000, 3, 1_000)
		for i, addr := range []string{"[2001:db8:7:100::1]:1", "[2001:db8:7:200::1]:1", "[2001:db8:7:300::1]:1"} {
			if rec := e.authorizeFrom(t, e.authorizeQuery(nil), nil, addr); !accepted(rec) {
				t.Fatalf("request %d from %s = %d %q", i, addr, rec.Code, rec.Header().Get("Location"))
			}
		}
		assertRefused(t, e, e.authorizeFrom(t, e.authorizeQuery(nil), nil, "[2001:db8:7:900::1]:1"), 3)
		if rec := e.authorizeFrom(t, e.authorizeQuery(nil), nil, "[2001:db8:8::1]:1"); !accepted(rec) {
			t.Errorf("a user of another /48 = %d %q, want the consent redirect", rec.Code, rec.Header().Get("Location"))
		}
	})

	t.Run("an IPv4 /24 is bounded and 6to4 counts as its embedded IPv4", func(t *testing.T) {
		e := oauthSetup(t)
		withOAuthPendingCaps(t, 20, 1_000_000, 1_000_000)
		withOAuthSourceTierCaps(t, 1_000, 1_000, 3)
		// 203.0.113.9 directly, then two 6to4 addresses embedding 203.0.113.x in different /64s,
		// fill the /24: 2002:cb00:7109:: is 203.0.113.9, 2002:cb00:710a:: is 203.0.113.10.
		for i, addr := range []string{"203.0.113.9:1", "[2002:cb00:710a:1::5]:1", "[2002:cb00:710b:ffff::5]:1"} {
			if rec := e.authorizeFrom(t, e.authorizeQuery(nil), nil, addr); !accepted(rec) {
				t.Fatalf("request %d from %s = %d %q", i, addr, rec.Code, rec.Header().Get("Location"))
			}
		}
		assertRefused(t, e, e.authorizeFrom(t, e.authorizeQuery(nil), nil, "203.0.113.200:1"), 3)
		if rec := e.authorizeFrom(t, e.authorizeQuery(nil), nil, "203.0.114.1:1"); !accepted(rec) {
			t.Errorf("another /24 = %d %q, want the consent redirect", rec.Code, rec.Header().Get("Location"))
		}
		var n int
		if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM oauth_authorize_requests WHERE product_id = $1 AND source_mid = '203.0.113.0/24'`, e.product).Scan(&n); err != nil || n != 3 {
			t.Errorf("rows bucketed in 203.0.113.0/24 = %d (%v), want 3", n, err)
		}
	})

	t.Run("the finest tier of 6to4 is the embedded IPv4 address, not the /64", func(t *testing.T) {
		e := oauthSetup(t)
		withOAuthPendingCaps(t, 1, 1_000_000, 1_000_000)
		if rec := e.authorizeFrom(t, e.authorizeQuery(nil), nil, "[2002:cb00:7109:1::1]:1"); !accepted(rec) {
			t.Fatalf("first 6to4 request = %d", rec.Code)
		}
		// Another /64 of the same 6to4 prefix, and the plain IPv4 it embeds, are the same source.
		assertRefused(t, e, e.authorizeFrom(t, e.authorizeQuery(nil), nil, "[2002:cb00:7109:2::1]:1"), 1)
		assertRefused(t, e, e.authorizeFrom(t, e.authorizeQuery(nil), nil, "203.0.113.9:1"), 1)
	})

	t.Run("the source bucket honours TRUSTED_PROXIES", func(t *testing.T) {
		e := oauthSetup(t)
		withOAuthPendingCaps(t, 1, 1_000_000, 1_000_000)
		_, trusted, _ := net.ParseCIDR("10.0.0.0/8")
		e.h.cfg.TrustedProxies = []*net.IPNet{trusted}
		t.Cleanup(func() { e.h.cfg.TrustedProxies = nil })
		via := func(remote, xff string) *httptest.ResponseRecorder {
			req := httptest.NewRequest(http.MethodGet, "/api/oauth/authorize?"+e.authorizeQuery(nil).Encode(), nil)
			req.RemoteAddr = remote
			req.Header.Set("X-Forwarded-For", xff)
			rec := httptest.NewRecorder()
			e.routes.ServeHTTP(rec, req)
			return rec
		}
		// Behind a trusted proxy the client is the forwarded address: one capped client is
		// refused while a different client behind the same proxy is accepted.
		if rec := via("10.1.1.1:5000", "198.51.100.1"); !accepted(rec) {
			t.Fatalf("client A = %d %q", rec.Code, rec.Header().Get("Location"))
		}
		assertRefused(t, e, via("10.1.1.1:5000", "198.51.100.1"), 1)
		if rec := via("10.1.1.1:5000", "198.51.100.2"); !accepted(rec) {
			t.Errorf("client B behind the same proxy = %d %q, want the consent redirect", rec.Code, rec.Header().Get("Location"))
		}
		// An untrusted peer's X-Forwarded-For is forged input: the peer address is the bucket.
		if rec := via("192.0.2.50:5000", "198.51.100.77"); !accepted(rec) {
			t.Fatalf("untrusted peer = %d %q", rec.Code, rec.Header().Get("Location"))
		}
		assertRefused(t, e, via("192.0.2.50:5000", "198.51.100.78"), 3)
		var n int
		if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM oauth_authorize_requests WHERE product_id = $1 AND source_prefix = '192.0.2.50'`, e.product).Scan(&n); err != nil || n != 1 {
			t.Errorf("rows bucketed by the untrusted peer = %d (%v), want 1", n, err)
		}
	})

	t.Run("a held product lock refuses within the bound, an over-cap request never waits", func(t *testing.T) {
		e := oauthSetup(t)
		oldTO := oauthAuthorizeLockTimeout
		oauthAuthorizeLockTimeout = 400 * time.Millisecond
		t.Cleanup(func() { oauthAuthorizeLockTimeout = oldTO })
		withOAuthPendingCaps(t, 1, 1_000_000, 1_000_000)

		ctx := context.Background()
		holder, err := e.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer holder.Rollback(ctx) //nolint:errcheck // released at the end of the test
		if err := e.h.q.WithTx(holder).LockOAuthAuthorize(ctx, e.product); err != nil {
			t.Fatal(err)
		}

		begin := time.Now()
		rec := e.authorizeFrom(t, e.authorizeQuery(nil), nil, "198.51.100.40:1")
		elapsed := time.Since(begin)
		if elapsed < 300*time.Millisecond || elapsed > 5*time.Second {
			t.Errorf("authorize with the lock held took %s, want about the %s bound", elapsed, oauthAuthorizeLockTimeout)
		}
		assertRefused(t, e, rec, 0)

		// An already over-cap source is refused by the lock-free pre-count, with the lock held.
		cliMustExec(t, e.pool, `INSERT INTO oauth_authorize_requests (product_id, redirect_uri, scopes, state, code_challenge, binding_hash, source_prefix, source_mid, source_wide, expires_at)
			VALUES ($1, $2, ARRAY['jobs:run'], 's', 'c', '\x00', '198.51.100.41', '198.51.100.0/24', '198.51.100.0/24', now() + interval '5 minutes')`, e.product, oauthTestRedirect)
		begin = time.Now()
		rec = e.authorizeFrom(t, e.authorizeQuery(nil), nil, "198.51.100.41:1")
		if elapsed := time.Since(begin); elapsed > 250*time.Millisecond {
			t.Errorf("an over-cap authorize took %s with the lock held, want no wait", elapsed)
		}
		assertRefused(t, e, rec, 1)
		if err := holder.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		if rec := e.authorizeFrom(t, e.authorizeQuery(nil), nil, "198.51.100.40:1"); !accepted(rec) {
			t.Errorf("after the lock was released = %d %q, want the consent redirect", rec.Code, rec.Header().Get("Location"))
		}
	})

	t.Run("concurrent authorizes from one source store exactly the cap", func(t *testing.T) {
		e := oauthSetup(t)
		const cap, n = 3, 24
		withOAuthPendingCaps(t, cap, 1_000_000, 1_000_000)
		results := make([]string, n)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				rec := e.authorize(t, e.authorizeQuery(nil), nil)
				results[i] = rec.Header().Get("Location")
			}()
		}
		close(start)
		wg.Wait()
		accepted := 0
		for _, loc := range results {
			if strings.HasPrefix(loc, "/connect?request=") {
				accepted++
			}
		}
		if accepted != cap {
			t.Errorf("accepted %d of %d concurrent authorizes, want exactly %d", accepted, n, cap)
		}
		if got := e.countRequests(t); got != cap {
			t.Errorf("rows = %d, want exactly %d", got, cap)
		}
	})

	t.Run("a refused request still validates first", func(t *testing.T) {
		// Over the cap, a request that fails validation still gets its validation error, and an
		// unverified redirect_uri still gets the static page, never the cap redirect.
		e := oauthSetup(t)
		withOAuthPendingCaps(t, 1_000_000, 1, 1_000_000)
		e.start(t, nil, nil)
		rec := e.authorize(t, e.authorizeQuery(func(v url.Values) { v.Set("redirect_uri", "https://evil.example.test/cb") }), nil)
		if rec.Code != http.StatusBadRequest || rec.Header().Get("Location") != "" {
			t.Errorf("unregistered redirect_uri over the cap = %d %q, want the static 400", rec.Code, rec.Header().Get("Location"))
		}
		rec = e.authorize(t, e.authorizeQuery(func(v url.Values) { v.Set("response_type", "token") }), nil)
		if u, err := url.Parse(rec.Header().Get("Location")); err != nil || u.Query().Get("error") != oauthsrv.ErrUnsupportedResponseType {
			t.Errorf("invalid request over the cap: %d %q", rec.Code, rec.Header().Get("Location"))
		}
	})
}
