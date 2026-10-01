package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/oauthsrv"
	"github.com/vtmocanu/uzi/api/internal/producttoken"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #1910 M3: POST /api/oauth/token (authorization_code) through the PRODUCTION router
// (h.Routes), against a real Postgres. Skipped unless UZI_TEST_DATABASE_URL points at a
// throwaway one; ./e2e/run-store-it.sh sweeps this package for the LiveDB suffix.

type tokenEnv struct {
	*oauthEnv
	secret  string
	limiter *mw.Limiter
}

func oauthTokenRouter(h *Handler, oauthLimiter *mw.Limiter) http.Handler {
	lim := func() *mw.Limiter { return mw.NewLimiter(1_000_000, time.Hour, nil) }
	return h.Routes(lim(), lim(), lim(), lim(), lim(), lim(), lim(), lim(), lim(), lim(), oauthLimiter)
}

// oauthRotateKnownSecret gives the product a client secret whose plaintext the test knows.
func oauthRotateKnownSecret(t *testing.T, h *Handler, productID uuid.UUID) string {
	t.Helper()
	secret, hash, prefix, err := oauthsrv.GenerateSecret()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.q.RotateProductClientSecret(context.Background(), store.RotateProductClientSecretParams{ID: productID, ClientSecretHash: hash, ClientSecretPrefix: prefix}); err != nil {
		t.Fatalf("RotateProductClientSecret: %v", err)
	}
	return secret
}

func tokenSetup(t *testing.T) *tokenEnv {
	t.Helper()
	e := oauthSetup(t)
	te := &tokenEnv{oauthEnv: e, limiter: mw.NewLimiter(1_000_000, time.Hour, nil)}
	te.secret = oauthRotateKnownSecret(t, e.h, e.product)
	te.routes = oauthTokenRouter(e.h, te.limiter)
	return te
}

// approvedCode runs a real authorize and approve and returns the authorization code and the
// request id.
func (e *tokenEnv) approvedCode(t *testing.T) (code, requestID string) {
	t.Helper()
	id, binding := e.start(t, nil, nil)
	rec := e.consent(t, http.MethodPost, id, "/approve", e.jwt, binding)
	code = decodeRedirect(t, rec).Query().Get("code")
	if code == "" {
		t.Fatalf("approve redirect carries no code: %s", rec.Body.String())
	}
	return code, id
}

func (e *tokenEnv) form(code string) url.Values {
	return url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {oauthTestRedirect},
		"code_verifier": {oauthTestVerifier},
	}
}

// post sends POST /api/oauth/token as the product with its Basic credentials; mut can alter the
// request (drop or replace the credentials, change the target or the content type).
func (e *tokenEnv) post(t *testing.T, form url.Values, mut func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/oauth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(e.product.String(), e.secret)
	if mut != nil {
		mut(req)
	}
	rec := httptest.NewRecorder()
	e.routes.ServeHTTP(rec, req)
	return rec
}

func (e *tokenEnv) exchange(t *testing.T, code string) *httptest.ResponseRecorder {
	t.Helper()
	return e.post(t, e.form(code), nil)
}

func assertOAuthNoStore(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store (status %d)", got, rec.Code)
	}
	if got := rec.Header().Get("Pragma"); got != "no-cache" {
		t.Errorf("Pragma = %q, want no-cache (status %d)", got, rec.Code)
	}
}

func decodeStrict[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	dec := json.NewDecoder(bytes.NewReader(rec.Body.Bytes()))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("decode %T from %d %q: %v", v, rec.Code, rec.Body.String(), err)
	}
	return v
}

func decodeOAuthToken(t *testing.T, rec *httptest.ResponseRecorder) apitypes.OAuthTokenResponse {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("token exchange = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	assertOAuthNoStore(t, rec)
	return decodeStrict[apitypes.OAuthTokenResponse](t, rec)
}

func requireOAuthError(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	assertOAuthNoStore(t, rec)
	if rec.Code != status {
		t.Fatalf("status = %d, want %d (%s)\n%s", rec.Code, status, code, rec.Body.String())
	}
	if got := decodeStrict[apitypes.OAuthErrorResponse](t, rec); got.Error != code {
		t.Fatalf("error = %q, want %q", got.Error, code)
	}
}

func (e *tokenEnv) whoami(token string) v1Resp {
	return v1Send(e.routes, http.MethodGet, "/api/v1/whoami", token, nil)
}

func (e *tokenEnv) grantByID(t *testing.T, id uuid.UUID) store.OauthGrant {
	t.Helper()
	g, err := e.h.q.LockOAuthGrant(context.Background(), id) // pool-bound: the row lock ends with the statement
	if err != nil {
		t.Fatalf("read grant: %v", err)
	}
	return g
}

func (e *tokenEnv) grantTokenCount(t *testing.T, grant uuid.UUID, where string) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM product_tokens WHERE grant_id = $1 `+where, grant).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (e *tokenEnv) assertCodeUnconsumed(t *testing.T, requestID string) {
	t.Helper()
	if st := e.requestRow(t, requestID).Status; st != "approved" {
		t.Fatalf("request status = %q, want approved: a refused exchange must not consume the code", st)
	}
}

func (e *tokenEnv) seedClient2(t *testing.T) (uuid.UUID, string) {
	t.Helper()
	id := oauthSeedClient(t, e.h, e.user, []string{oauthTestRedirect}, []string{"jobs:run", "jobs:read"})
	return id, oauthRotateKnownSecret(t, e.h, id)
}

func TestOAuthTokenExchangeSuccessLiveDB(t *testing.T) {
	e := tokenSetup(t)
	code, reqID := e.approvedCode(t)

	tok := decodeOAuthToken(t, e.exchange(t, code))
	if !strings.HasPrefix(tok.AccessToken, producttoken.Prefix) || !strings.HasPrefix(tok.RefreshToken, oauthsrv.RefreshPrefix) {
		t.Fatalf("token classes: access %q refresh %q", tok.AccessToken[:4], tok.RefreshToken[:4])
	}
	if tok.TokenType != "Bearer" || tok.ExpiresIn != 3600 || tok.Scope != "jobs:run jobs:read" {
		t.Fatalf("response = type %q expires_in %d scope %q", tok.TokenType, tok.ExpiresIn, tok.Scope)
	}

	// Storage: the request is redeemed; the access token is a grant row with an hour of life; the
	// grant holds sha256(refresh) and its display prefix, never the plaintext.
	if st := e.requestRow(t, reqID).Status; st != "redeemed" {
		t.Fatalf("request status = %q, want redeemed", st)
	}
	g := e.liveGrant(t)
	if !bytes.Equal(g.RefreshTokenHash, oauthsrv.HashSecret(tok.RefreshToken)) || !g.RefreshIssuedAt.Valid ||
		g.RefreshTokenPrefix.String != tok.RefreshToken[:8] || g.RefreshLastUsedAt.Valid {
		t.Fatalf("grant refresh state = %+v", g)
	}
	var grantID uuid.UUID
	var expires time.Time
	var scopes []string
	if err := e.pool.QueryRow(context.Background(),
		`SELECT grant_id, expires_at, scopes FROM product_tokens WHERE token_hash = $1`, producttoken.Hash(tok.AccessToken)).
		Scan(&grantID, &expires, &scopes); err != nil {
		t.Fatalf("access token row: %v", err)
	}
	if grantID != g.ID || !slices.Equal(scopes, []string{"jobs:run", "jobs:read"}) || time.Until(expires) < 59*time.Minute || time.Until(expires) > 61*time.Minute {
		t.Fatalf("access token row: grant %s scopes %v expires in %s", grantID, scopes, time.Until(expires))
	}

	// Enforcement parity: the OAuth access token is an ordinary product token on /api/v1.
	got := e.whoami(tok.AccessToken)
	if got.status != http.StatusOK {
		t.Fatalf("whoami with the OAuth access token = %d %q, want 200", got.status, got.body)
	}
	w := v1DecodeWhoami(t, got.body)
	if w.User.ID != e.user.String() || w.Product == nil || w.Product.ID != e.product.String() || !slices.Equal(w.Scopes, []string{"jobs:run", "jobs:read"}) {
		t.Fatalf("whoami = %+v, want the consenting user, the product and both scopes", w)
	}
	// The refresh token is not a bearer anywhere.
	if got := e.whoami(tok.RefreshToken); got.status != http.StatusUnauthorized {
		t.Fatalf("whoami with the refresh token = %d %q, want 401", got.status, got.body)
	}
}

func TestOAuthTokenAccessTokenIsRefusedWhereAPastedTokenIsLiveDB(t *testing.T) {
	e := tokenSetup(t)
	code, _ := e.approvedCode(t)
	tok := decodeOAuthToken(t, e.exchange(t, code))
	unknownAccess := v1UnknownProductToken(t)
	unknownRefresh, _, _, err := oauthsrv.GenerateRefreshToken()
	if err != nil {
		t.Fatal(err)
	}

	cr, ok := e.routes.(chi.Routes)
	if !ok {
		t.Fatal("routes is not a chi.Routes")
	}
	placeholder := uuid.NewString()
	for _, tc := range []struct{ name, real, unknown string }{
		{"OAuth access token", tok.AccessToken, unknownAccess},
		{"refresh token", tok.RefreshToken, unknownRefresh},
	} {
		t.Run(tc.name, func(t *testing.T) {
			walked, requireUserRefusals := 0, 0
			err := chi.Walk(cr, func(method, pattern string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
				if v1IsV1(pattern) {
					return nil
				}
				path, err := v1ConcretePath(pattern, placeholder)
				if err != nil {
					t.Errorf("%s %s: %v", method, pattern, err)
					return nil
				}
				if found := cr.Find(chi.NewRouteContext(), method, path); found != pattern {
					t.Errorf("%s %s: concrete path %q routes to %q", method, pattern, path, found)
					return nil
				}
				walked++
				got0 := v1Send(e.routes, method, path, tc.unknown, nil)
				got1 := v1Send(e.routes, method, path, tc.real, nil)
				if v1RoutingMiss(got0) || v1RoutingMiss(got1) {
					t.Errorf("%s %s: a chi routing miss", method, pattern)
				}
				if got0.status == http.StatusUnauthorized && got0.body == "{\"error\":\"invalid CLI token\"}\n" {
					requireUserRefusals++
				}
				if got0 != got1 {
					t.Errorf("%s %s (%s): real token got %d %q, unknown got %d %q", method, pattern, path, got1.status, got1.body, got0.status, got0.body)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if walked < 100 || requireUserRefusals < 50 {
				t.Fatalf("walked %d routes, %d RequireUser refusals: the walk is vacuous", walked, requireUserRefusals)
			}
		})
	}
	// Spot checks by name: an internal RequireUser route answers 401 for both.
	for _, bearer := range []string{tok.AccessToken, tok.RefreshToken} {
		if got := v1Send(e.routes, http.MethodGet, "/api/me/product-tokens/", bearer, nil); got.status != http.StatusUnauthorized {
			t.Errorf("GET /api/me/product-tokens/ with %s = %d %q, want 401", bearer[:4], got.status, got.body)
		}
		if got := v1Send(e.routes, http.MethodGet, "/api/runs", bearer, nil); got.status != http.StatusUnauthorized {
			t.Errorf("GET /api/runs with %s = %d %q, want 401", bearer[:4], got.status, got.body)
		}
	}
}

func TestOAuthTokenCodeExpiresAfterSixtySecondsLiveDB(t *testing.T) {
	e := tokenSetup(t)
	code, reqID := e.approvedCode(t)
	cliMustExec(t, e.pool, `UPDATE oauth_authorize_requests SET code_expires_at = now() - interval '1 second' WHERE id = $1`, reqID)
	requireOAuthError(t, e.exchange(t, code), http.StatusBadRequest, "invalid_grant")
	e.assertCodeUnconsumed(t, reqID)
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM product_tokens WHERE product_id = $1 AND grant_id IS NOT NULL`, e.product).Scan(&n); err != nil || n != 0 {
		t.Fatalf("grant tokens after an expired exchange = %d (%v), want 0", n, err)
	}
}

func TestOAuthTokenReplayRevokesGrantLiveDB(t *testing.T) {
	e := tokenSetup(t)
	code, reqID := e.approvedCode(t)
	tok := decodeOAuthToken(t, e.exchange(t, code))
	g := e.liveGrant(t)
	// An older, already expired access token of the grant: the revoke must reach it too, so the
	// ListRevokedProductJobs sweep cancels the jobs it created.
	expired := e.seedGrantToken(t, g.ID, []string{"jobs:run"})
	cliMustExec(t, e.pool, `UPDATE product_tokens SET expires_at = now() - interval '1 minute' WHERE id = $1`, expired)

	requireOAuthError(t, e.exchange(t, code), http.StatusBadRequest, "invalid_grant")

	rg := e.grantByID(t, g.ID)
	if !rg.RevokedAt.Valid || rg.RefreshTokenHash != nil || rg.RefreshTokenPrefix.Valid || rg.RefreshIssuedAt.Valid {
		t.Fatalf("grant after replay = revoked_at %v, refresh hash %v: want revoked with the refresh token cleared", rg.RevokedAt, rg.RefreshTokenHash)
	}
	if n := e.grantTokenCount(t, g.ID, `AND NOT revoked`); n != 0 {
		t.Fatalf("%d grant tokens still unrevoked after a replay", n)
	}
	if !e.tokenRevoked(t, expired) {
		t.Fatal("the expired grant token must be revoked too")
	}
	if got := e.whoami(tok.AccessToken); got.status != http.StatusUnauthorized {
		t.Fatalf("whoami with the replayed grant's access token = %d, want 401", got.status)
	}
	if st := e.requestRow(t, reqID).Status; st != "redeemed" {
		t.Fatalf("request status = %q, want redeemed", st)
	}
}

func TestOAuthTokenReplayWithBrokenBindingRevokesNothingLiveDB(t *testing.T) {
	e := tokenSetup(t)
	code, _ := e.approvedCode(t)
	tok := decodeOAuthToken(t, e.exchange(t, code))
	g := e.liveGrant(t)
	other, otherSecret := e.seedClient2(t)

	for _, tc := range []struct {
		name string
		form func(url.Values)
		mut  func(*http.Request)
	}{
		{name: "wrong verifier", form: func(v url.Values) { v.Set("code_verifier", strings.Repeat("a", 43)) }},
		{name: "missing verifier", form: func(v url.Values) { v.Del("code_verifier") }},
		{name: "another registered redirect URI", form: func(v url.Values) { v.Set("redirect_uri", "http://127.0.0.1:9090/cb?tenant=a") }},
		{name: "missing redirect URI", form: func(v url.Values) { v.Del("redirect_uri") }},
		{name: "another client authenticated", mut: func(r *http.Request) { r.SetBasicAuth(other.String(), otherSecret) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := e.form(code)
			if tc.form != nil {
				tc.form(f)
			}
			requireOAuthError(t, e.post(t, f, tc.mut), http.StatusBadRequest, "invalid_grant")
			if rg := e.grantByID(t, g.ID); rg.RevokedAt.Valid || !bytes.Equal(rg.RefreshTokenHash, oauthsrv.HashSecret(tok.RefreshToken)) {
				t.Fatal("a replay that failed a binding check revoked the grant or its refresh token")
			}
			if got := e.whoami(tok.AccessToken); got.status != http.StatusOK {
				t.Fatalf("access token after a broken-binding replay = %d, want still 200", got.status)
			}
		})
	}
	// Bad client authentication is 401 invalid_client, and also revokes nothing.
	rec := e.post(t, e.form(code), func(r *http.Request) { r.SetBasicAuth(e.product.String(), e.secret+"x") })
	requireOAuthError(t, rec, http.StatusUnauthorized, "invalid_client")
	if rg := e.grantByID(t, g.ID); rg.RevokedAt.Valid {
		t.Fatal("a replay with failed client authentication revoked the grant")
	}
}

func TestOAuthTokenAnotherClientsCodeLiveDB(t *testing.T) {
	e := tokenSetup(t)
	code, reqID := e.approvedCode(t)
	other, otherSecret := e.seedClient2(t)
	rec := e.post(t, e.form(code), func(r *http.Request) { r.SetBasicAuth(other.String(), otherSecret) })
	requireOAuthError(t, rec, http.StatusBadRequest, "invalid_grant")
	e.assertCodeUnconsumed(t, reqID)
	decodeOAuthToken(t, e.exchange(t, code)) // the rightful client still redeems it
}

func TestOAuthTokenFailedBindingsDoNotConsumeTheCodeLiveDB(t *testing.T) {
	e := tokenSetup(t)
	code, reqID := e.approvedCode(t)
	for _, tc := range []struct {
		name string
		mut  func(url.Values)
	}{
		{"wrong redirect URI", func(v url.Values) { v.Set("redirect_uri", "https://p.example.test/other") }},
		{"another registered redirect URI", func(v url.Values) { v.Set("redirect_uri", "http://127.0.0.1:9090/cb?tenant=a") }},
		{"missing redirect URI", func(v url.Values) { v.Del("redirect_uri") }},
		{"wrong verifier", func(v url.Values) { v.Set("code_verifier", strings.Repeat("a", 43)) }},
		{"missing verifier", func(v url.Values) { v.Del("code_verifier") }},
		{"verifier too short", func(v url.Values) { v.Set("code_verifier", oauthTestVerifier[:42]) }},
		{"verifier with a reserved character", func(v url.Values) { v.Set("code_verifier", oauthTestVerifier[:42]+"+") }},
		{"unknown code", func(v url.Values) { v.Set("code", code+"x") }},
		{"oversized code", func(v url.Values) { v.Set("code", strings.Repeat("a", 5000)) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := e.form(code)
			tc.mut(f)
			requireOAuthError(t, e.post(t, f, nil), http.StatusBadRequest, "invalid_grant")
			e.assertCodeUnconsumed(t, reqID)
		})
	}
	decodeOAuthToken(t, e.exchange(t, code)) // and the correct exchange still succeeds
}

func TestOAuthTokenLiveTokenCapLiveDB(t *testing.T) {
	e := tokenSetup(t)
	code, _ := e.approvedCode(t)
	g := e.liveGrant(t)
	// Tokens that do not count: three revoked, three expired. Then nine live: one slot is left.
	for i := 0; i < 3; i++ {
		cliMustExec(t, e.pool, `UPDATE product_tokens SET revoked = true WHERE id = $1`, e.seedGrantToken(t, g.ID, []string{"jobs:read"}))
		cliMustExec(t, e.pool, `UPDATE product_tokens SET expires_at = now() - interval '1 minute' WHERE id = $1`, e.seedGrantToken(t, g.ID, []string{"jobs:read"}))
	}
	live := make([]uuid.UUID, 0, 10)
	for i := 0; i < 9; i++ {
		live = append(live, e.seedGrantToken(t, g.ID, []string{"jobs:read"}))
	}
	// The last slot: the dead tokens did not count, so this exchange mints the tenth live token.
	// Use a second code for the capped half below (this one is consumed here).
	decodeOAuthToken(t, e.exchange(t, code))
	if n := e.grantTokenCount(t, g.ID, `AND NOT revoked AND expires_at > now()`); n != 10 {
		t.Fatalf("live grant tokens = %d, want 10", n)
	}

	// Ten live tokens: a fresh code is refused with 429 and a Retry-After until the oldest expires,
	// and stays redeemable.
	code2, reqID2 := e.approvedCode(t)
	cliMustExec(t, e.pool, `UPDATE product_tokens SET expires_at = now() + interval '90 seconds' WHERE id = $1`, live[0])
	rec := e.exchange(t, code2)
	requireOAuthError(t, rec, http.StatusTooManyRequests, "temporarily_unavailable")
	ra, err := strconv.Atoi(rec.Header().Get("Retry-After"))
	if err != nil || ra < 85 || ra > 92 {
		t.Fatalf("Retry-After = %q, want about 90 (seconds until the oldest live token expires)", rec.Header().Get("Retry-After"))
	}
	e.assertCodeUnconsumed(t, reqID2)
	if n := e.grantTokenCount(t, g.ID, `AND NOT revoked AND expires_at > now()`); n != 10 {
		t.Fatalf("a capped exchange changed the live token count to %d", n)
	}

	// Free a slot: the very same code now succeeds.
	cliMustExec(t, e.pool, `UPDATE product_tokens SET expires_at = now() - interval '1 second' WHERE id = $1`, live[0])
	decodeOAuthToken(t, e.exchange(t, code2))
	if n := e.grantTokenCount(t, g.ID, `AND NOT revoked AND expires_at > now()`); n != 10 {
		t.Fatalf("live grant tokens = %d, want 10", n)
	}
}

// pgActivityWaiting reports whether some backend is blocked on a row lock in a statement
// containing fragment.
func pgActivityWaiting(t *testing.T, e *tokenEnv, fragment string) bool {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND query LIKE '%' || $1 || '%'`, fragment).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
}

// TestOAuthTokenCapIsCountedUnderTheGrantLockLiveDB: with nine live tokens, an exchange that waits
// on the grant lock sees a token minted by the holder of that lock. If the count were taken before
// the lock (or without it) the exchange would mint an eleventh. It proves the bound is checked
// under the grant lock (D8), the only serialization point any mint path (the exchange here, the
// refresh in M4) shares; two code exchanges cannot race each other for one code, which is single use.
func TestOAuthTokenCapIsCountedUnderTheGrantLockLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name       string
		holderMint bool
		wantStatus int
		wantLive   int
	}{
		{"the lock holder takes the tenth slot", true, http.StatusTooManyRequests, 10},
		{"the lock holder takes nothing", false, http.StatusOK, 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := tokenSetup(t)
			code, reqID := e.approvedCode(t)
			g := e.liveGrant(t)
			for i := 0; i < 9; i++ {
				e.seedGrantToken(t, g.ID, []string{"jobs:read"})
			}
			ctx := context.Background()
			tx, err := e.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit
			if _, err := tx.Exec(ctx, `SELECT 1 FROM oauth_grants WHERE id = $1 FOR UPDATE`, g.ID); err != nil {
				t.Fatal(err)
			}
			var wg sync.WaitGroup
			var rec *httptest.ResponseRecorder
			wg.Add(1)
			go func() { defer wg.Done(); rec = e.exchange(t, code) }()
			deadline := time.Now().Add(10 * time.Second)
			for !pgActivityWaiting(t, e, "FROM oauth_grants WHERE id = $1 FOR UPDATE") {
				if time.Now().After(deadline) {
					t.Fatal("the exchange never blocked on the grant lock")
				}
				time.Sleep(20 * time.Millisecond)
			}
			if tc.holderMint {
				if _, err := tx.Exec(ctx,
					`INSERT INTO product_tokens (user_id, product_id, name, token_hash, token_prefix, scopes, expires_at, grant_id)
					 VALUES ($1, $2, 'oauth access', $3, 'uzp_test', ARRAY['jobs:read'], now() + interval '1 hour', $4)`,
					e.user, e.product, producttoken.Hash(uuid.NewString()), g.ID); err != nil {
					t.Fatal(err)
				}
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			wg.Wait()
			if rec.Code != tc.wantStatus {
				t.Fatalf("exchange = %d, want %d\n%s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if n := e.grantTokenCount(t, g.ID, `AND NOT revoked AND expires_at > now()`); n != tc.wantLive {
				t.Fatalf("live grant tokens = %d, want %d: the bound was passed", n, tc.wantLive)
			}
			if tc.wantStatus != http.StatusOK {
				e.assertCodeUnconsumed(t, reqID)
			}
		})
	}
}

// TestOAuthTokenConcurrentSameCodeLiveDB: two simultaneous exchanges of one code. The grant lock
// serializes them: exactly one gets tokens; the other finds the code redeemed and, with every
// binding matching, treats it as a replay and revokes the grant (D6). A product that retries a
// request it never got a response for therefore loses the connection, which is the RFC 6749
// section 4.1.2 rule, not a bug.
func TestOAuthTokenConcurrentSameCodeLiveDB(t *testing.T) {
	e := tokenSetup(t)
	code, _ := e.approvedCode(t)
	g := e.liveGrant(t)
	recs := make([]*httptest.ResponseRecorder, 2)
	var wg sync.WaitGroup
	for i := range recs {
		wg.Add(1)
		go func() { defer wg.Done(); recs[i] = e.exchange(t, code) }()
	}
	wg.Wait()
	ok := 0
	for _, r := range recs {
		switch r.Code {
		case http.StatusOK:
			ok++
		case http.StatusBadRequest:
			requireOAuthError(t, r, http.StatusBadRequest, "invalid_grant")
		default:
			t.Fatalf("unexpected status %d: %s", r.Code, r.Body.String())
		}
	}
	if ok != 1 {
		t.Fatalf("%d of 2 concurrent exchanges succeeded, want exactly 1", ok)
	}
	if rg := e.grantByID(t, g.ID); !rg.RevokedAt.Valid {
		t.Fatal("the losing exchange was a full-binding replay and must have revoked the grant")
	}
	if n := e.grantTokenCount(t, g.ID, `AND NOT revoked`); n != 0 {
		t.Fatalf("%d grant tokens survive the replay revoke", n)
	}
}

func TestOAuthTokenCodeApprovedBeforeGrantRevokeIsDeadLiveDB(t *testing.T) {
	e := tokenSetup(t)
	code, reqID := e.approvedCode(t)
	g := e.liveGrant(t)
	expired := e.seedGrantToken(t, g.ID, []string{"jobs:read"})
	cliMustExec(t, e.pool, `UPDATE product_tokens SET expires_at = now() - interval '1 minute' WHERE id = $1`, expired)
	live := e.seedGrantToken(t, g.ID, []string{"jobs:read"})

	// The shared revoke helper, called the way every revoke path must: grant locked first.
	ctx := context.Background()
	if err := e.h.inTx(ctx, func(q *store.Queries) error {
		if _, err := q.LockOAuthGrant(ctx, g.ID); err != nil {
			return err
		}
		return revokeGrantLocked(ctx, q, g.ID)
	}); err != nil {
		t.Fatalf("revokeGrantLocked: %v", err)
	}
	rg := e.grantByID(t, g.ID)
	if !rg.RevokedAt.Valid || rg.RefreshTokenHash != nil {
		t.Fatalf("grant after revoke = %+v", rg)
	}
	if !e.tokenRevoked(t, expired) || !e.tokenRevoked(t, live) {
		t.Fatal("revokeGrantLocked must set revoked on every grant token, expired or not")
	}
	if st := e.requestRow(t, reqID).Status; st != "superseded" {
		t.Fatalf("the grant's unredeemed code is %q, want superseded", st)
	}
	requireOAuthError(t, e.exchange(t, code), http.StatusBadRequest, "invalid_grant")
	if n := e.grantTokenCount(t, g.ID, `AND NOT revoked`); n != 0 {
		t.Fatalf("%d unrevoked grant tokens after the revoke and a refused exchange", n)
	}
}

func TestOAuthTokenCodeApprovedBeforeReconsentIsSupersededLiveDB(t *testing.T) {
	e := tokenSetup(t)
	oldCode, oldReq := e.approvedCode(t)
	newCode, _ := e.approvedCode(t) // re-consent supersedes the earlier code
	if st := e.requestRow(t, oldReq).Status; st != "superseded" {
		t.Fatalf("first request status = %q, want superseded", st)
	}
	requireOAuthError(t, e.exchange(t, oldCode), http.StatusBadRequest, "invalid_grant")
	decodeOAuthToken(t, e.exchange(t, newCode))
}

func TestOAuthTokenRechecksClientAndUserAtRedemptionLiveDB(t *testing.T) {
	e := tokenSetup(t)
	code, reqID := e.approvedCode(t)
	ctx := context.Background()
	restoreClient := func() {
		if _, err := e.h.q.SetProductOAuthClient(ctx, store.SetProductOAuthClientParams{
			ID: e.product, RedirectUris: []string{oauthTestRedirect, "http://127.0.0.1:9090/cb?tenant=a"}, OauthScopes: []string{"jobs:run", "jobs:read"},
		}); err != nil {
			t.Fatal(err)
		}
	}
	refused := func(name string) {
		t.Helper()
		requireOAuthError(t, e.exchange(t, code), http.StatusBadRequest, "invalid_grant")
		e.assertCodeUnconsumed(t, reqID)
		t.Logf("%s: refused, code unconsumed", name)
	}

	// The registered URI was removed after the code was issued.
	if _, err := e.h.q.SetProductOAuthClient(ctx, store.SetProductOAuthClientParams{
		ID: e.product, RedirectUris: []string{"http://127.0.0.1:9090/cb?tenant=a"}, OauthScopes: []string{"jobs:run", "jobs:read"},
	}); err != nil {
		t.Fatal(err)
	}
	refused("redirect URI no longer registered")
	restoreClient()

	// The allowed scopes no longer cover the grant's.
	if _, err := e.h.q.SetProductOAuthClient(ctx, store.SetProductOAuthClientParams{
		ID: e.product, RedirectUris: []string{oauthTestRedirect}, OauthScopes: []string{"jobs:read"},
	}); err != nil {
		t.Fatal(err)
	}
	refused("scope no longer allowed")
	restoreClient()

	// The user was deactivated.
	cliMustExec(t, e.pool, `UPDATE users SET is_active = false WHERE id = $1`, e.user)
	refused("user inactive")
	cliMustExec(t, e.pool, `UPDATE users SET is_active = true WHERE id = $1`, e.user)

	// The grant was revoked.
	g := e.liveGrant(t)
	cliMustExec(t, e.pool, `UPDATE oauth_grants SET revoked_at = now() WHERE id = $1`, g.ID)
	refused("grant revoked")
	cliMustExec(t, e.pool, `UPDATE oauth_grants SET revoked_at = NULL WHERE id = $1`, g.ID)

	decodeOAuthToken(t, e.exchange(t, code)) // everything restored: the same code redeems
}

func TestOAuthTokenRequestRulesLiveDB(t *testing.T) {
	e := tokenSetup(t)
	code, reqID := e.approvedCode(t)

	invalid := func(name string, form url.Values, mut func(*http.Request)) {
		t.Run(name, func(t *testing.T) {
			requireOAuthError(t, e.post(t, form, mut), http.StatusBadRequest, "invalid_request")
			e.assertCodeUnconsumed(t, reqID)
		})
	}
	repeated := e.form(code)
	repeated.Add("redirect_uri", oauthTestRedirect)
	invalid("repeated redirect_uri", repeated, nil)
	repeatedUnknown := e.form(code)
	repeatedUnknown["whatever"] = []string{"a", "b"}
	invalid("a repeated unknown parameter", repeatedUnknown, nil)
	withSecret := e.form(code)
	withSecret.Set("client_secret", e.secret)
	invalid("Basic plus a body client_secret", withSecret, nil)
	withEmptySecret := e.form(code)
	withEmptySecret.Set("client_secret", "")
	invalid("Basic plus an empty body client_secret", withEmptySecret, nil)
	otherID := e.form(code)
	otherID.Set("client_id", uuid.NewString())
	invalid("a body client_id that differs from Basic's", otherID, nil)
	invalid("a query string", e.form(code), func(r *http.Request) { r.URL.RawQuery = "x=1" })
	invalid("a client_id in the query string", e.form(code), func(r *http.Request) { r.URL.RawQuery = "client_id=" + e.product.String() })
	invalid("a JSON content type", e.form(code), func(r *http.Request) { r.Header.Set("Content-Type", "application/json") })
	invalid("no content type", e.form(code), func(r *http.Request) { r.Header.Del("Content-Type") })
	noGrantType := e.form(code)
	noGrantType.Del("grant_type")
	invalid("a missing grant_type", noGrantType, nil)
	noCode := e.form(code)
	noCode.Del("code")
	invalid("a missing code", noCode, nil)
	invalid("a body over 16 KiB", url.Values{"code": {strings.Repeat("a", 17<<10)}}, nil)

	t.Run("a body client_id equal to Basic's is allowed", func(t *testing.T) {
		f := e.form(code)
		f.Set("client_id", e.product.String())
		decodeOAuthToken(t, e.post(t, f, nil))
	})

	t.Run("an unsupported grant type", func(t *testing.T) {
		f := e.form(code)
		f.Set("grant_type", "client_credentials")
		requireOAuthError(t, e.post(t, f, nil), http.StatusBadRequest, "unsupported_grant_type")
	})
}

func TestOAuthTokenClientAuthenticationLiveDB(t *testing.T) {
	e := tokenSetup(t)
	code, reqID := e.approvedCode(t)
	ctx := context.Background()

	var firstBody string
	assert401 := func(name string, mut func(*http.Request)) {
		t.Run(name, func(t *testing.T) {
			rec := e.post(t, e.form(code), mut)
			requireOAuthError(t, rec, http.StatusUnauthorized, "invalid_client")
			if got := rec.Header().Get("WWW-Authenticate"); got != `Basic realm="uzi"` {
				t.Fatalf("WWW-Authenticate = %q", got)
			}
			// Every failure is the same answer: an unknown client is not distinguishable.
			if firstBody == "" {
				firstBody = rec.Body.String()
			} else if rec.Body.String() != firstBody {
				t.Fatalf("body %q differs from the first failure's %q", rec.Body.String(), firstBody)
			}
			e.assertCodeUnconsumed(t, reqID)
		})
	}
	assert401("no credentials", func(r *http.Request) { r.Header.Del("Authorization") })
	assert401("a bearer instead of Basic", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+e.secret) })
	assert401("malformed base64", func(r *http.Request) { r.Header.Set("Authorization", "Basic !!!") })
	assert401("no colon", func(r *http.Request) { r.Header.Set("Authorization", "Basic YWJj") })
	assert401("a wrong secret", func(r *http.Request) { r.SetBasicAuth(e.product.String(), e.secret+"x") })
	assert401("an empty secret", func(r *http.Request) { r.SetBasicAuth(e.product.String(), "") })
	assert401("an unknown client", func(r *http.Request) { r.SetBasicAuth(uuid.NewString(), e.secret) })
	assert401("a client_id that is not a uuid", func(r *http.Request) { r.SetBasicAuth("not-a-uuid", e.secret) })
	t.Run("client_secret_post only", func(t *testing.T) {
		f := e.form(code)
		f.Set("client_id", e.product.String())
		f.Set("client_secret", e.secret)
		rec := e.post(t, f, func(r *http.Request) { r.Header.Del("Authorization") })
		requireOAuthError(t, rec, http.StatusUnauthorized, "invalid_client")
		e.assertCodeUnconsumed(t, reqID)
	})

	// A secret with reserved characters is form-urlencoded inside Basic (RFC 6749 section 2.3.1):
	// the product's id and secret are sent percent-decoded, and a '+' there is a space, not a plus.
	t.Run("Basic credentials are form-urlencoded", func(t *testing.T) {
		rec := e.post(t, e.form(code), func(r *http.Request) {
			r.SetBasicAuth(url.QueryEscape(e.product.String()), url.QueryEscape(e.secret))
		})
		decodeOAuthToken(t, rec)
	})

	t.Run("a disabled product", func(t *testing.T) {
		code2, _ := e.approvedCode(t)
		cliMustExec(t, e.pool, `UPDATE products SET enabled = false WHERE id = $1`, e.product)
		requireOAuthError(t, e.exchange(t, code2), http.StatusUnauthorized, "invalid_client")
		cliMustExec(t, e.pool, `UPDATE products SET enabled = true WHERE id = $1`, e.product)
	})
	t.Run("a deleted product", func(t *testing.T) {
		code2, _ := e.approvedCode(t)
		cliMustExec(t, e.pool, `UPDATE products SET enabled = false, deleted_at = now() WHERE id = $1`, e.product)
		requireOAuthError(t, e.exchange(t, code2), http.StatusUnauthorized, "invalid_client")
		cliMustExec(t, e.pool, `UPDATE products SET enabled = true, deleted_at = NULL WHERE id = $1`, e.product)
	})
	t.Run("a product that is no longer a client", func(t *testing.T) {
		code2, _ := e.approvedCode(t)
		if _, err := e.h.q.SetProductOAuthClient(ctx, store.SetProductOAuthClientParams{ID: e.product, RedirectUris: []string{}, OauthScopes: []string{}}); err != nil {
			t.Fatal(err)
		}
		requireOAuthError(t, e.exchange(t, code2), http.StatusUnauthorized, "invalid_client")
	})
}

// TestOAuthTokenRotatedSecretCutsOffTheOldOneLiveDB: rotation replaces the secret immediately.
func TestOAuthTokenRotatedSecretCutsOffTheOldOneLiveDB(t *testing.T) {
	e := tokenSetup(t)
	code, _ := e.approvedCode(t)
	old := e.secret
	e.secret = oauthRotateKnownSecret(t, e.h, e.product)
	rec := e.post(t, e.form(code), func(r *http.Request) { r.SetBasicAuth(e.product.String(), old) })
	requireOAuthError(t, rec, http.StatusUnauthorized, "invalid_client")
	decodeOAuthToken(t, e.exchange(t, code))
}

var errOAuthInjected = errors.New("injected storage failure")

type oauthErrRow struct{}

func (oauthErrRow) Scan(...any) error { return errOAuthInjected }

// oauthFailTx is a pgx.Tx that fails the statements containing failOn (and optionally the commit).
type oauthFailTx struct {
	pgx.Tx
	failOn     string
	failCommit bool
}

func (f oauthFailTx) hit(sql string) bool { return f.failOn != "" && strings.Contains(sql, f.failOn) }

func (f oauthFailTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if f.hit(sql) {
		return pgconn.CommandTag{}, errOAuthInjected
	}
	return f.Tx.Exec(ctx, sql, args...)
}

func (f oauthFailTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if f.hit(sql) {
		return nil, errOAuthInjected
	}
	return f.Tx.Query(ctx, sql, args...)
}

func (f oauthFailTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if f.hit(sql) {
		return oauthErrRow{}
	}
	return f.Tx.QueryRow(ctx, sql, args...)
}

func (f oauthFailTx) Commit(ctx context.Context) error {
	if f.failCommit {
		_ = f.Rollback(ctx)
		return errOAuthInjected
	}
	return f.Tx.Commit(ctx)
}

// oauthFailDB is a store.DBTX that fails the statements containing failOn.
type oauthFailDB struct {
	store.DBTX
	failOn string
}

func (f oauthFailDB) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if strings.Contains(sql, f.failOn) {
		return pgconn.CommandTag{}, errOAuthInjected
	}
	return f.DBTX.Exec(ctx, sql, args...)
}

func (f oauthFailDB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if strings.Contains(sql, f.failOn) {
		return nil, errOAuthInjected
	}
	return f.DBTX.Query(ctx, sql, args...)
}

func (f oauthFailDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if strings.Contains(sql, f.failOn) {
		return oauthErrRow{}
	}
	return f.DBTX.QueryRow(ctx, sql, args...)
}

// TestOAuthTokenStorageErrorIsNeverACredentialErrorLiveDB (D7): a storage or lookup error at ANY
// step answers 503 temporarily_unavailable with Retry-After, never invalid_grant (a product that
// sees invalid_grant discards the connection) or invalid_client, and consumes nothing: the same
// code redeems afterwards.
func TestOAuthTokenStorageErrorIsNeverACredentialErrorLiveDB(t *testing.T) {
	e := tokenSetup(t)
	code, reqID := e.approvedCode(t)
	realQ := e.h.q
	pool := e.pool
	t.Cleanup(func() { e.h.q, e.h.oauthBeginTx = realQ, nil })

	want503 := func(t *testing.T) {
		t.Helper()
		rec := e.exchange(t, code)
		requireOAuthError(t, rec, http.StatusServiceUnavailable, "temporarily_unavailable")
		if rec.Header().Get("Retry-After") == "" {
			t.Fatal("503 without Retry-After")
		}
		e.h.q, e.h.oauthBeginTx = realQ, nil
		e.assertCodeUnconsumed(t, reqID)
		if n := e.grantTokenCount(t, e.liveGrant(t).ID, ``); n != 0 {
			t.Fatalf("%d grant tokens after a failed exchange, want 0", n)
		}
	}

	t.Run("client lookup", func(t *testing.T) {
		e.h.q = store.New(oauthFailDB{DBTX: pool, failOn: "FROM products WHERE id = $1"})
		want503(t)
	})
	t.Run("begin", func(t *testing.T) {
		e.h.oauthBeginTx = func(context.Context) (pgx.Tx, error) { return nil, errOAuthInjected }
		want503(t)
	})
	for _, step := range []struct{ name, sql string }{
		{"code lookup", "WHERE code_hash = $1"},
		{"grant lock", "FROM oauth_grants WHERE id = $1 FOR UPDATE"},
		{"request re-read", "FROM oauth_authorize_requests WHERE id = $1"},
		{"product re-read", "FROM products WHERE id = $1"},
		{"user re-read", "FROM users WHERE id = $1"},
		{"live token count", "min(expires_at)"},
		{"redeem", "SET status = 'redeemed'"},
		{"access token insert", "INSERT INTO product_tokens"},
		{"refresh token store", "refresh_token_hash = $1::bytea"},
	} {
		t.Run(step.name, func(t *testing.T) {
			e.h.oauthBeginTx = func(ctx context.Context) (pgx.Tx, error) {
				tx, err := pool.Begin(ctx)
				return oauthFailTx{Tx: tx, failOn: step.sql}, err
			}
			want503(t)
		})
	}
	t.Run("commit", func(t *testing.T) {
		e.h.oauthBeginTx = func(ctx context.Context) (pgx.Tx, error) {
			tx, err := pool.Begin(ctx)
			return oauthFailTx{Tx: tx, failCommit: true}, err
		}
		want503(t)
	})

	// And the same code still redeems on a healthy store.
	e.h.q, e.h.oauthBeginTx = realQ, nil
	decodeOAuthToken(t, e.exchange(t, code))
}

// TestOAuthTokenPerClientBudgetIgnoresFailedAuthenticationLiveDB (D7): failed or missing client
// authentication spends only the per-IP budget; the per-client budget is drawn ONLY after the
// client authenticated. Driven through the real h.Routes with a budget of 3.
func TestOAuthTokenPerClientBudgetIgnoresFailedAuthenticationLiveDB(t *testing.T) {
	e := tokenSetup(t)
	e.limiter = mw.NewLimiter(3, time.Hour, nil)
	e.routes = oauthTokenRouter(e.h, e.limiter)
	other, otherSecret := e.seedClient2(t)

	from := func(ip string) func(*http.Request) {
		return func(r *http.Request) { r.RemoteAddr = ip + ":4000" }
	}
	// Five failed authentications for client X, each from its own address: none may touch X's
	// per-client budget (3), so none is ever a 429.
	for i := 0; i < 5; i++ {
		rec := e.post(t, e.form("nope"), func(r *http.Request) {
			from(fmt.Sprintf("192.0.2.%d", 10+i))(r)
			r.SetBasicAuth(e.product.String(), e.secret+"x")
		})
		requireOAuthError(t, rec, http.StatusUnauthorized, "invalid_client")
	}
	// Authenticated requests do spend it: three pass (invalid_grant for a bogus code), the fourth is 429.
	for i := 0; i < 3; i++ {
		requireOAuthError(t, e.post(t, e.form("nope"), from(fmt.Sprintf("192.0.2.%d", 30+i))), http.StatusBadRequest, "invalid_grant")
	}
	rec := e.post(t, e.form("nope"), from("192.0.2.40"))
	requireOAuthError(t, rec, http.StatusTooManyRequests, "temporarily_unavailable")
	if ra, err := strconv.Atoi(rec.Header().Get("Retry-After")); err != nil || ra < 1 {
		t.Fatalf("Retry-After = %q, want a positive number of seconds", rec.Header().Get("Retry-After"))
	}
	// Another client is unaffected by X's exhaustion.
	rec = e.post(t, e.form("nope"), func(r *http.Request) {
		from("192.0.2.50")(r)
		r.SetBasicAuth(other.String(), otherSecret)
	})
	requireOAuthError(t, rec, http.StatusBadRequest, "invalid_grant")
	// The per-IP budget still applies to failed authentication: the fourth request from one address is 429,
	// with the cache headers, from the limiter and not the handler.
	var last *httptest.ResponseRecorder
	for i := 0; i < 4; i++ {
		last = e.post(t, e.form("nope"), func(r *http.Request) {
			from("192.0.2.60")(r)
			r.Header.Del("Authorization")
		})
	}
	if last.Code != http.StatusTooManyRequests {
		t.Fatalf("fourth failed request from one address = %d, want 429 from the per-IP limiter", last.Code)
	}
	assertOAuthNoStore(t, last)
}

// TestOAuthTokenReplayAfterCodeExpiryStillRevokesLiveDB (D6): a fully bound replay of a redeemed
// code revokes the grant even after the code's 60 s expired (the row is kept for ten more minutes
// precisely so the replay stays detectable).
func TestOAuthTokenReplayAfterCodeExpiryStillRevokesLiveDB(t *testing.T) {
	e := tokenSetup(t)
	code, reqID := e.approvedCode(t)
	tok := decodeOAuthToken(t, e.exchange(t, code))
	g := e.liveGrant(t)
	cliMustExec(t, e.pool, `UPDATE oauth_authorize_requests SET code_expires_at = now() - interval '5 minutes' WHERE id = $1`, reqID)

	requireOAuthError(t, e.exchange(t, code), http.StatusBadRequest, "invalid_grant")
	if rg := e.grantByID(t, g.ID); !rg.RevokedAt.Valid || rg.RefreshTokenHash != nil {
		t.Fatalf("grant after an expired-code replay = %+v, want revoked", rg)
	}
	e.wantWhoami(t, "the replayed grant's access token", tok.AccessToken, http.StatusUnauthorized)
}

// TestOAuthTokenReplayTakesNoRequestRowLockLiveDB (D8): the replay arm locks the grant and its
// tokens and never takes a lock on the code's request row first. A foreign transaction holding that
// row FOR UPDATE therefore does not stall the replay revoke.
func TestOAuthTokenReplayTakesNoRequestRowLockLiveDB(t *testing.T) {
	e := tokenSetup(t)
	code, reqID := e.approvedCode(t)
	decodeOAuthToken(t, e.exchange(t, code))
	g := e.liveGrant(t)
	ctx := context.Background()
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit
	if _, err := tx.Exec(ctx, `SELECT 1 FROM oauth_authorize_requests WHERE id = $1 FOR UPDATE`, reqID); err != nil {
		t.Fatal(err)
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- e.exchange(t, code) }()
	select {
	case rec := <-done:
		requireOAuthError(t, rec, http.StatusBadRequest, "invalid_grant")
	case <-time.After(10 * time.Second):
		t.Fatal("the replay blocked on the request row: it must take no lock on it before the grant's tokens")
	}
	if rg := e.grantByID(t, g.ID); !rg.RevokedAt.Valid {
		t.Fatal("the replay did not revoke the grant")
	}
}

// TestOAuthTokenReplayStorageErrorIsA503LiveDB (D7): a storage error on any step of the replay
// revoke, or on its commit, answers 503 temporarily_unavailable (never invalid_grant), and rolls
// the revoke back: the grant stays live. A healthy replay then revokes it.
func TestOAuthTokenReplayStorageErrorIsA503LiveDB(t *testing.T) {
	e := tokenSetup(t)
	code, _ := e.approvedCode(t)
	tok := decodeOAuthToken(t, e.exchange(t, code))
	g := e.liveGrant(t)
	pool := e.pool
	t.Cleanup(func() { e.h.oauthBeginTx = nil })

	for _, step := range []struct {
		name string
		tx   func(pgx.Tx) pgx.Tx
	}{
		{"revoke the grant's tokens", func(tx pgx.Tx) pgx.Tx {
			return oauthFailTx{Tx: tx, failOn: "UPDATE product_tokens SET revoked = true WHERE grant_id"}
		}},
		{"supersede the grant's codes", func(tx pgx.Tx) pgx.Tx { return oauthFailTx{Tx: tx, failOn: "SET status = 'superseded'"} }},
		{"revoke the grant row", func(tx pgx.Tx) pgx.Tx { return oauthFailTx{Tx: tx, failOn: "SET revoked_at = now()"} }},
		{"commit", func(tx pgx.Tx) pgx.Tx { return oauthFailTx{Tx: tx, failCommit: true} }},
	} {
		t.Run(step.name, func(t *testing.T) {
			e.h.oauthBeginTx = func(ctx context.Context) (pgx.Tx, error) {
				tx, err := pool.Begin(ctx)
				return step.tx(tx), err
			}
			rec := e.exchange(t, code)
			e.h.oauthBeginTx = nil
			requireOAuthError(t, rec, http.StatusServiceUnavailable, "temporarily_unavailable")
			if rec.Header().Get("Retry-After") == "" {
				t.Fatal("503 without Retry-After")
			}
			if rg := e.grantByID(t, g.ID); rg.RevokedAt.Valid || rg.RefreshTokenHash == nil {
				t.Fatalf("a failed replay revoke changed the grant: %+v", rg)
			}
			e.wantWhoami(t, "the access token after a failed replay revoke", tok.AccessToken, http.StatusOK)
		})
	}
	requireOAuthError(t, e.exchange(t, code), http.StatusBadRequest, "invalid_grant")
	if rg := e.grantByID(t, g.ID); !rg.RevokedAt.Valid {
		t.Fatal("the healthy replay did not revoke the grant")
	}
}

// TestOAuthTokenAccessTokenCarriesOnlyTheGrantsScopesLiveDB: a grant approved with only jobs:read
// yields an access token (and a response) with only jobs:read.
func TestOAuthTokenAccessTokenCarriesOnlyTheGrantsScopesLiveDB(t *testing.T) {
	e := tokenSetup(t)
	tok := e.connectAs(t, e.jwt, func(v url.Values) { v.Set("scope", "jobs:read") })
	if tok.Scope != "jobs:read" {
		t.Fatalf("response scope = %q, want jobs:read", tok.Scope)
	}
	w := v1DecodeWhoami(t, e.whoami(tok.AccessToken).body)
	if !slices.Equal(w.Scopes, []string{"jobs:read"}) {
		t.Fatalf("whoami scopes = %v, want [jobs:read]", w.Scopes)
	}
	var scopes []string
	if err := e.pool.QueryRow(context.Background(), `SELECT scopes FROM product_tokens WHERE token_hash = $1`, producttoken.Hash(tok.AccessToken)).Scan(&scopes); err != nil || !slices.Equal(scopes, []string{"jobs:read"}) {
		t.Fatalf("stored scopes = %v, %v; want [jobs:read]", scopes, err)
	}
}

// TestOAuthTokenGrantOfAnotherProductIsInvalidGrantLiveDB: the belt check at redemption. A code
// whose grant belongs to a different product than its request is refused and consumes nothing.
func TestOAuthTokenGrantOfAnotherProductIsInvalidGrantLiveDB(t *testing.T) {
	e := tokenSetup(t)
	code, reqID := e.approvedCode(t)
	other, _ := e.seedClient2(t)
	cliMustExec(t, e.pool, `UPDATE oauth_grants SET product_id = $1 WHERE user_id = $2 AND revoked_at IS NULL`, other, e.user)
	requireOAuthError(t, e.exchange(t, code), http.StatusBadRequest, "invalid_grant")
	e.assertCodeUnconsumed(t, reqID)
}

// TestOAuthTokenNonClientProductIsInvalidClientLiveDB: a product without a secret (or not a
// client) is the same 401 invalid_client as a wrong secret; the dummy hash stands in for the
// missing one so the request does the same hashing work.
func TestOAuthTokenNonClientProductIsInvalidClientLiveDB(t *testing.T) {
	e := tokenSetup(t)
	noSecret := v1SeedProduct(t, e.h.q, e.user) // a product with no OAuth client registration at all
	rec := e.post(t, e.form("x"), func(r *http.Request) { r.SetBasicAuth(noSecret.String(), "anything") })
	requireOAuthError(t, rec, http.StatusUnauthorized, "invalid_client")
	if rec.Header().Get("WWW-Authenticate") == "" {
		t.Fatal("401 without WWW-Authenticate")
	}
}
