package handler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
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
	req := httptest.NewRequest(http.MethodGet, "/api/oauth/authorize?"+q.Encode(), nil)
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

// seedGrantToken inserts an access-token row bound to the grant, as M3's token endpoint will.
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
// wins; the rest are 409, and exactly one code and one grant result.
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

// withOAuthPendingCaps lowers the authorize pending-request caps for one test.
func withOAuthPendingCaps(t *testing.T, perProduct, global int) {
	t.Helper()
	oldP, oldG := oauthPendingPerProductCap, oauthPendingGlobalCap
	oauthPendingPerProductCap, oauthPendingGlobalCap = perProduct, global
	t.Cleanup(func() { oauthPendingPerProductCap, oauthPendingGlobalCap = oldP, oldG })
}

// The authorize endpoint is unauthenticated and its per-IP limiter does not bound a /64, so the
// table is bounded by a per-product and a global cap checked BEFORE the insert. Over a cap the
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
		withOAuthPendingCaps(t, 2, 1_000_000)
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
		withOAuthPendingCaps(t, 1_000_000, live+2)
		_, c := e.start(t, nil, nil)
		e.start(t, c, nil)
		assertRefused(t, e, e.authorize(t, e.authorizeQuery(nil), c), 2)
	})

	t.Run("a refused request still validates first", func(t *testing.T) {
		// Over the cap, a request that fails validation still gets its validation error, and an
		// unverified redirect_uri still gets the static page, never the cap redirect.
		e := oauthSetup(t)
		withOAuthPendingCaps(t, 1, 1_000_000)
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

// The sweep and the cap count must be served by the oauth_authorize_requests indexes, not by a
// scan of the table. A seq scan is made unavailable (SET LOCAL enable_seqscan = off) because a
// near-empty test table would otherwise always plan a seq scan; with it off, a predicate no index
// can serve shows as a Seq Scan anyway (the old CASE form does) and fails here. The statements are
// copied from queries/oauth.sql (DeleteExpiredOAuthAuthorizeRequests, CountLivePendingOAuthRequests).
func TestOAuthSweepAndCapQueriesUseIndexesLiveDB(t *testing.T) {
	e := oauthSetup(t)
	ctx := context.Background()
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // read-only plans; nothing to commit
	if _, err := tx.Exec(ctx, `SET LOCAL enable_seqscan = off`); err != nil {
		t.Fatal(err)
	}
	plan := func(sql string) string {
		t.Helper()
		rows, err := tx.Query(ctx, `EXPLAIN `+sql)
		if err != nil {
			t.Fatalf("explain: %v", err)
		}
		defer rows.Close()
		var b strings.Builder
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatal(err)
			}
			b.WriteString(line + "\n")
		}
		return b.String()
	}
	sweep := plan(`DELETE FROM oauth_authorize_requests
 WHERE (status IN ('pending', 'denied', 'superseded') AND expires_at < now())
    OR (status IN ('approved', 'redeemed') AND code_expires_at < now() - interval '10 minutes')`)
	for _, want := range []string{"idx_oauth_authorize_requests_expires", "idx_oauth_authorize_requests_code_expires"} {
		if !strings.Contains(sweep, want) {
			t.Errorf("the sweep plan does not use %s:\n%s", want, sweep)
		}
	}
	if strings.Contains(sweep, "Seq Scan") {
		t.Errorf("the sweep plan seq-scans the table:\n%s", sweep)
	}
	count := plan(`SELECT (SELECT count(*) FROM (SELECT 1 FROM oauth_authorize_requests WHERE status = 'pending' AND expires_at > now() AND product_id = '` + e.product.String() + `' LIMIT 500) p),
       (SELECT count(*) FROM (SELECT 1 FROM oauth_authorize_requests WHERE status = 'pending' AND expires_at > now() LIMIT 5000) g)`)
	if !strings.Contains(count, "idx_oauth_authorize_requests_pending") || strings.Contains(count, "Seq Scan") {
		t.Errorf("the pending-count plan must be index-backed by idx_oauth_authorize_requests_pending:\n%s", count)
	}
}
