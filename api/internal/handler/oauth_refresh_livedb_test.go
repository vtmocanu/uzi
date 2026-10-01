package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #1910 M4: POST /api/oauth/token with grant_type=refresh_token, through the PRODUCTION router
// against a real Postgres. Skipped unless UZI_TEST_DATABASE_URL points at a throwaway one.

// postTo is post for any /api/oauth path.
func (e *tokenEnv) postTo(t *testing.T, path string, form url.Values, mut func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(e.product.String(), e.secret)
	if mut != nil {
		mut(req)
	}
	rec := httptest.NewRecorder()
	e.routes.ServeHTTP(rec, req)
	return rec
}

func refreshForm(refresh string, scope ...string) url.Values {
	v := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}}
	if len(scope) > 0 {
		v.Set("scope", scope[0])
	}
	return v
}

func (e *tokenEnv) refresh(t *testing.T, refresh string, scope ...string) *httptest.ResponseRecorder {
	t.Helper()
	return e.postTo(t, "/api/oauth/token", refreshForm(refresh, scope...), nil)
}

func decodeOAuthRefresh(t *testing.T, rec *httptest.ResponseRecorder) apitypes.OAuthRefreshResponse {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	assertOAuthNoStore(t, rec)
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if _, has := raw["refresh_token"]; has {
		t.Fatalf("a refresh response carries refresh_token (no rotation): %s", rec.Body.String())
	}
	return decodeStrict[apitypes.OAuthRefreshResponse](t, rec)
}

// connect runs the real authorize, approve and code exchange for e.user.
func (e *tokenEnv) connect(t *testing.T) apitypes.OAuthTokenResponse {
	t.Helper()
	return e.connectAs(t, e.jwt, nil)
}

func (e *tokenEnv) requireRefreshRefused(t *testing.T, name, refresh string, status int, code string) {
	t.Helper()
	before := e.countAllGrantTokens(t)
	rec := e.refresh(t, refresh)
	if rec.Code != status {
		t.Fatalf("%s: refresh = %d %q, want %d %s", name, rec.Code, rec.Body.String(), status, code)
	}
	requireOAuthError(t, rec, status, code)
	if after := e.countAllGrantTokens(t); after != before {
		t.Fatalf("%s: a refused refresh minted %d token(s)", name, after-before)
	}
}

func (e *tokenEnv) countAllGrantTokens(t *testing.T) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM product_tokens WHERE grant_id IS NOT NULL AND user_id = $1`, e.user).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestOAuthRefreshSuccessLiveDB (D5): a refresh mints a working one-hour access token, does not
// rotate the refresh token (the response omits it and the stored hash is unchanged), stamps the
// idle clock, and the same refresh token works again.
func TestOAuthRefreshSuccessLiveDB(t *testing.T) {
	e := tokenSetup(t)
	tok := e.connect(t)
	g0 := e.liveGrant(t)
	if g0.RefreshLastUsedAt.Valid {
		t.Fatal("refresh_last_used_at is set before any refresh")
	}

	r1 := decodeOAuthRefresh(t, e.refresh(t, tok.RefreshToken))
	if r1.TokenType != "Bearer" || r1.ExpiresIn != 3600 || r1.Scope != "jobs:run jobs:read" {
		t.Fatalf("refresh response = %+v", r1)
	}
	if r1.AccessToken == tok.AccessToken || !strings.HasPrefix(r1.AccessToken, "uzp_") {
		t.Fatalf("refresh access token %q is not a new uzp_ token", r1.AccessToken)
	}
	if got := e.whoami(r1.AccessToken); got.status != http.StatusOK {
		t.Fatalf("whoami with the refreshed token = %d %q", got.status, got.body)
	}
	e.wantWhoami(t, "the first access token", tok.AccessToken, http.StatusOK)

	g1 := e.liveGrant(t)
	if string(g1.RefreshTokenHash) != string(g0.RefreshTokenHash) || !g1.RefreshLastUsedAt.Valid || g1.RevokedAt.Valid {
		t.Fatalf("grant after refresh: hash changed=%v last_used=%v revoked=%v", string(g1.RefreshTokenHash) != string(g0.RefreshTokenHash), g1.RefreshLastUsedAt.Valid, g1.RevokedAt.Valid)
	}
	if !slices.Equal(g1.Scopes, g0.Scopes) || !g1.ConsentedAt.Time.Equal(g0.ConsentedAt.Time) {
		t.Fatal("a refresh changed the grant's scopes or consented_at")
	}

	// The same refresh token again: a new token, still no rotation.
	r2 := decodeOAuthRefresh(t, e.refresh(t, tok.RefreshToken))
	if r2.AccessToken == r1.AccessToken {
		t.Fatal("two refreshes returned the same access token")
	}
	e.wantWhoami(t, "the second refreshed token", r2.AccessToken, http.StatusOK)
	if n := e.grantTokenCount(t, g0.ID, `AND NOT revoked AND expires_at > now()`); n != 3 {
		t.Fatalf("live grant tokens = %d, want 3", n)
	}
}

// TestOAuthRefreshConcurrentDoubleLiveDB: two simultaneous refreshes with one refresh token both
// succeed (no rotation, so a retry or a second replica never revokes the grant).
func TestOAuthRefreshConcurrentDoubleLiveDB(t *testing.T) {
	e := tokenSetup(t)
	tok := e.connect(t)
	var wg sync.WaitGroup
	recs := make([]*httptest.ResponseRecorder, 2)
	for i := range recs {
		wg.Add(1)
		go func() { defer wg.Done(); recs[i] = e.refresh(t, tok.RefreshToken) }()
	}
	wg.Wait()
	a, b := decodeOAuthRefresh(t, recs[0]), decodeOAuthRefresh(t, recs[1])
	if a.AccessToken == b.AccessToken {
		t.Fatal("both refreshes returned one access token")
	}
	e.wantWhoami(t, "first concurrent token", a.AccessToken, http.StatusOK)
	e.wantWhoami(t, "second concurrent token", b.AccessToken, http.StatusOK)
	if g := e.liveGrant(t); g.RevokedAt.Valid {
		t.Fatal("a concurrent double refresh revoked the grant")
	}
}

// TestOAuthRefreshClientAuthenticationLiveDB (D7): refresh authenticates the client exactly like
// the code exchange, and another client's valid credentials neither refresh nor change anything.
func TestOAuthRefreshClientAuthenticationLiveDB(t *testing.T) {
	e := tokenSetup(t)
	tok := e.connect(t)
	other, otherSecret := e.seedClient2(t)
	before := e.liveGrant(t)
	count := e.countAllGrantTokens(t)

	// Another client, authenticated, presenting this client's refresh token.
	rec := e.postTo(t, "/api/oauth/token", refreshForm(tok.RefreshToken), func(r *http.Request) { r.SetBasicAuth(other.String(), otherSecret) })
	requireOAuthError(t, rec, http.StatusBadRequest, "invalid_grant")
	after := e.liveGrant(t)
	if after.RevokedAt.Valid || after.RefreshLastUsedAt.Valid != before.RefreshLastUsedAt.Valid || string(after.RefreshTokenHash) != string(before.RefreshTokenHash) {
		t.Fatal("another client's refresh attempt changed the grant")
	}
	if e.countAllGrantTokens(t) != count {
		t.Fatal("another client's refresh attempt minted a token")
	}
	e.wantWhoami(t, "the access token after another client's attempt", tok.AccessToken, http.StatusOK)
	decodeOAuthRefresh(t, e.refresh(t, tok.RefreshToken)) // the real client still refreshes

	for name, mut := range map[string]func(*http.Request){
		"no credentials":     func(r *http.Request) { r.Header.Del("Authorization") },
		"wrong secret":       func(r *http.Request) { r.SetBasicAuth(e.product.String(), e.secret+"x") },
		"unknown client":     func(r *http.Request) { r.SetBasicAuth(uuid.NewString(), e.secret) },
		"malformed id":       func(r *http.Request) { r.SetBasicAuth("not-a-uuid", e.secret) },
		"client_secret body": nil,
	} {
		t.Run(name, func(t *testing.T) {
			form := refreshForm(tok.RefreshToken)
			if mut == nil {
				form.Set("client_secret", e.secret)
			}
			rec := e.postTo(t, "/api/oauth/token", form, mut)
			if mut == nil {
				requireOAuthError(t, rec, http.StatusBadRequest, "invalid_request")
				return
			}
			requireOAuthError(t, rec, http.StatusUnauthorized, "invalid_client")
			if got := rec.Header().Get("WWW-Authenticate"); !strings.HasPrefix(got, "Basic") {
				t.Fatalf("WWW-Authenticate = %q, want Basic", got)
			}
		})
	}
}

// TestOAuthRefreshRequestRulesLiveDB: the token endpoint's request rules apply to refresh.
func TestOAuthRefreshRequestRulesLiveDB(t *testing.T) {
	e := tokenSetup(t)
	tok := e.connect(t)
	unknown := "uzr_" + strings.Repeat("A", 43)
	for _, tc := range []struct {
		name   string
		form   url.Values
		path   string
		status int
		code   string
	}{
		{"missing refresh_token", url.Values{"grant_type": {"refresh_token"}}, "", 400, "invalid_request"},
		{"empty refresh_token", refreshForm(""), "", 400, "invalid_request"},
		{"repeated refresh_token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tok.RefreshToken, tok.RefreshToken}}, "", 400, "invalid_request"},
		{"repeated scope", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tok.RefreshToken}, "scope": {"jobs:read", "jobs:read"}}, "", 400, "invalid_request"},
		{"query string", refreshForm(tok.RefreshToken), "?refresh_token=x", 400, "invalid_request"},
		{"unknown refresh token", refreshForm(unknown), "", 400, "invalid_grant"},
		{"garbage refresh token", refreshForm("not a token"), "", 400, "invalid_grant"},
		{"oversized refresh token", refreshForm(strings.Repeat("a", 5000)), "", 400, "invalid_grant"},
		{"an access token as a refresh token", refreshForm(tok.AccessToken), "", 400, "invalid_grant"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := e.postTo(t, "/api/oauth/token"+tc.path, tc.form, nil)
			requireOAuthError(t, rec, tc.status, tc.code)
		})
	}
}

// TestOAuthRefreshLifetimesLiveDB (D5): 30 days idle, counted from the last refresh (else from
// issue), and 90 days absolute from consented_at. A refused refresh mints nothing.
func TestOAuthRefreshLifetimesLiveDB(t *testing.T) {
	e := tokenSetup(t)
	tok := e.connect(t)
	g := e.liveGrant(t)
	exec := func(sql string, args ...any) { t.Helper(); cliMustExec(t, e.pool, sql, args...) }

	// Idle, from refresh_last_used_at.
	exec(`UPDATE oauth_grants SET refresh_last_used_at = now() - interval '31 days' WHERE id = $1`, g.ID)
	e.requireRefreshRefused(t, "idle 31 days", tok.RefreshToken, http.StatusBadRequest, "invalid_grant")
	exec(`UPDATE oauth_grants SET refresh_last_used_at = now() - interval '29 days' WHERE id = $1`, g.ID)
	decodeOAuthRefresh(t, e.refresh(t, tok.RefreshToken)) // within the idle window, and it restarts the clock
	if got := e.liveGrant(t); time.Since(got.RefreshLastUsedAt.Time) > time.Minute {
		t.Fatalf("a refresh did not restart the idle clock: %v", got.RefreshLastUsedAt.Time)
	}

	// Idle, from refresh_issued_at while the refresh token was never used.
	exec(`UPDATE oauth_grants SET refresh_last_used_at = NULL, refresh_issued_at = now() - interval '31 days' WHERE id = $1`, g.ID)
	e.requireRefreshRefused(t, "idle from issue", tok.RefreshToken, http.StatusBadRequest, "invalid_grant")

	// Absolute, from consented_at, however recently the token was used.
	exec(`UPDATE oauth_grants SET refresh_issued_at = now(), refresh_last_used_at = now(), consented_at = now() - interval '91 days' WHERE id = $1`, g.ID)
	e.requireRefreshRefused(t, "absolute 91 days", tok.RefreshToken, http.StatusBadRequest, "invalid_grant")
	exec(`UPDATE oauth_grants SET consented_at = now() - interval '89 days' WHERE id = $1`, g.ID)
	decodeOAuthRefresh(t, e.refresh(t, tok.RefreshToken))
	if got := e.liveGrant(t); got.RevokedAt.Valid {
		t.Fatal("an expired-refresh refusal revoked the grant")
	}
}

// TestOAuthRefreshAfterReconsentRestartsTheAbsoluteClockLiveDB is the approved test for "90 days
// counts from consented_at": a grant consented 91 days ago is refused, and the same user approving
// again through the real approve path (consented_at = now, a new refresh token from the new code)
// refreshes successfully, although the grant row itself is 91 days old.
func TestOAuthRefreshAfterReconsentRestartsTheAbsoluteClockLiveDB(t *testing.T) {
	e := tokenSetup(t)
	tok := e.connect(t)
	g := e.liveGrant(t)
	cliMustExec(t, e.pool, `UPDATE oauth_grants SET created_at = now() - interval '91 days', consented_at = now() - interval '91 days' WHERE id = $1`, g.ID)
	e.requireRefreshRefused(t, "consented 91 days ago", tok.RefreshToken, http.StatusBadRequest, "invalid_grant")

	// Re-consent: the real authorize and approve, then the exchange of the new code.
	tok2 := e.connectAs(t, e.jwt, nil)
	g2 := e.liveGrant(t)
	if g2.ID != g.ID {
		t.Fatal("re-consent created a second grant")
	}
	if time.Since(g2.ConsentedAt.Time) > time.Minute || time.Since(g2.CreatedAt.Time) < 90*24*time.Hour {
		t.Fatalf("after re-consent consented_at = %v (want now), created_at = %v (want 91 days ago)", g2.ConsentedAt.Time, g2.CreatedAt.Time)
	}
	r := decodeOAuthRefresh(t, e.refresh(t, tok2.RefreshToken))
	e.wantWhoami(t, "the refreshed token after re-consent", r.AccessToken, http.StatusOK)
	// The previous consent's refresh token died with the re-consent.
	e.requireRefreshRefused(t, "refresh token of the previous consent", tok.RefreshToken, http.StatusBadRequest, "invalid_grant")
}

// requireInvalidClient asserts the 401 invalid_client (with WWW-Authenticate) that a client
// registration which stopped qualifying gets: the product must keep its refresh token.
func requireInvalidClient(t *testing.T, name string, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("%s: status = %d %q, want 401 invalid_client", name, rec.Code, rec.Body.String())
	}
	requireOAuthError(t, rec, http.StatusUnauthorized, "invalid_client")
	if got := rec.Header().Get("WWW-Authenticate"); !strings.HasPrefix(got, "Basic") {
		t.Fatalf("%s: WWW-Authenticate = %q, want a Basic challenge", name, got)
	}
}

// TestOAuthRefreshRechecksClientAndUserLiveDB (D7, D8): the product and user are re-read under the
// grant lock on every refresh, and the answer says WHOSE state is wrong. The client's own
// registration (disabled, deleted, scopes narrowed below the grant) is 401 invalid_client, so a
// product keeps its refresh token; an inactive user or a dead grant is invalid_grant. Nothing is
// revoked by any of them: restoring the state makes the same refresh token work again.
func TestOAuthRefreshRechecksClientAndUserLiveDB(t *testing.T) {
	e := tokenSetup(t)
	tok := e.connect(t)
	ctx := context.Background()
	exec := func(sql string, args ...any) { t.Helper(); cliMustExec(t, e.pool, sql, args...) }
	setClientScopes := func(scopes ...string) {
		t.Helper()
		if _, err := e.h.q.SetProductOAuthClient(ctx, store.SetProductOAuthClientParams{
			ID: e.product, RedirectUris: []string{oauthTestRedirect}, OauthScopes: scopes,
		}); err != nil {
			t.Fatal(err)
		}
	}
	noMint := func(name string, rec *httptest.ResponseRecorder, before int) {
		t.Helper()
		if after := e.countAllGrantTokens(t); after != before {
			t.Fatalf("%s: a refused refresh minted %d token(s)", name, after-before)
		}
		if g := e.liveGrant(t); g.RevokedAt.Valid || len(g.RefreshTokenHash) == 0 {
			t.Fatalf("%s: the refusal revoked the grant or cleared its refresh token", name)
		}
	}
	clientRefused := func(name string) {
		t.Helper()
		before := e.countAllGrantTokens(t)
		rec := e.refresh(t, tok.RefreshToken)
		requireInvalidClient(t, name, rec)
		noMint(name, rec, before)
	}
	grantRefused := func(name string) {
		t.Helper()
		e.requireRefreshRefused(t, name, tok.RefreshToken, http.StatusBadRequest, "invalid_grant")
	}

	exec(`UPDATE products SET enabled = false WHERE id = $1`, e.product)
	// A disabled product cannot authenticate as a client at all: the refresh is a 401 before the grant.
	clientRefused("product disabled")
	exec(`UPDATE products SET enabled = true WHERE id = $1`, e.product)

	exec(`UPDATE products SET enabled = false, deleted_at = now() WHERE id = $1`, e.product)
	clientRefused("product deleted")
	exec(`UPDATE products SET enabled = true, deleted_at = NULL WHERE id = $1`, e.product)

	// The allowed scopes no longer cover the grant's (the client still authenticates): the whole
	// grant cannot be refreshed, and that is the registration's fault, not the connection's.
	setClientScopes("jobs:read")
	clientRefused("scope no longer allowed")
	// A request that narrows to what the product still allows is served, with exactly those scopes;
	// one that asks for a scope the product lost is refused the same way, and one outside the
	// grant is invalid_scope whatever the product allows.
	narrowed := decodeOAuthRefresh(t, e.refresh(t, tok.RefreshToken, "jobs:read"))
	if narrowed.Scope != "jobs:read" {
		t.Fatalf("narrowed refresh scope = %q, want jobs:read", narrowed.Scope)
	}
	e.wantWhoami(t, "the token narrowed to the product's remaining scope", narrowed.AccessToken, http.StatusOK)
	before := e.countAllGrantTokens(t)
	requireInvalidClient(t, "requested scope the product lost", e.refresh(t, tok.RefreshToken, "jobs:run"))
	requireInvalidClient(t, "requested set including a lost scope", e.refresh(t, tok.RefreshToken, "jobs:run jobs:read"))
	requireOAuthError(t, e.refresh(t, tok.RefreshToken, "jobs:admin"), http.StatusBadRequest, "invalid_scope")
	if e.countAllGrantTokens(t) != before {
		t.Fatal("a refused scoped refresh minted a token")
	}
	setClientScopes("jobs:run", "jobs:read")

	exec(`UPDATE users SET is_active = false WHERE id = $1`, e.user)
	grantRefused("user inactive")
	exec(`UPDATE users SET is_active = true WHERE id = $1`, e.user)

	g := e.liveGrant(t)
	exec(`UPDATE oauth_grants SET revoked_at = now() WHERE id = $1`, g.ID)
	grantRefused("grant revoked")
	exec(`UPDATE oauth_grants SET revoked_at = NULL WHERE id = $1`, g.ID)

	if r := decodeOAuthRefresh(t, e.refresh(t, tok.RefreshToken)); r.Scope != "jobs:run jobs:read" { // everything restored
		t.Fatalf("restored refresh scope = %q", r.Scope)
	}
}

// TestOAuthRefreshProductDisabledWhileWaitingForTheLockLiveDB: the in-transaction product check
// (not only client authentication) refuses a product that stopped being an enabled client, or whose
// allowed scopes stopped covering what the token would carry, between the authentication and the
// lock. The product row is flipped inside the transaction that holds the lock. The refusal is the
// 401 invalid_client of the auth-time check, mints nothing, and the same refresh token works once
// the registration is restored.
func TestOAuthRefreshProductDisabledWhileWaitingForTheLockLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mutate    string
		restore   string
		scope     []string
		wantScope string // "" = refused with 401
	}{
		{"product disabled", `UPDATE products SET enabled = false WHERE id = $1`, `UPDATE products SET enabled = true WHERE id = $1`, nil, ""},
		{"product deleted", `UPDATE products SET enabled = false, deleted_at = now() WHERE id = $1`, `UPDATE products SET enabled = true, deleted_at = NULL WHERE id = $1`, nil, ""},
		{"no longer a client (redirect URIs cleared)", `UPDATE products SET redirect_uris = '{}' WHERE id = $1`, `UPDATE products SET redirect_uris = ARRAY['` + oauthTestRedirect + `'] WHERE id = $1`, nil, ""},
		{"allowed scopes narrowed below the grant", `UPDATE products SET oauth_scopes = ARRAY['jobs:read'] WHERE id = $1`, `UPDATE products SET oauth_scopes = ARRAY['jobs:run','jobs:read'] WHERE id = $1`, nil, ""},
		{"allowed scopes narrowed, request narrows to what remains", `UPDATE products SET oauth_scopes = ARRAY['jobs:read'] WHERE id = $1`, `UPDATE products SET oauth_scopes = ARRAY['jobs:run','jobs:read'] WHERE id = $1`, []string{"jobs:read"}, "jobs:read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := tokenSetup(t)
			tok := e.connect(t)
			g := e.liveGrant(t)
			rec := e.refreshBlockedBy(t, g.ID, tok.RefreshToken, func(ctx context.Context, tx pgx.Tx) {
				if _, err := tx.Exec(ctx, tc.mutate, e.product); err != nil {
					t.Fatal(err)
				}
			}, tc.scope...)
			if tc.wantScope != "" {
				if got := decodeOAuthRefresh(t, rec); got.Scope != tc.wantScope {
					t.Fatalf("scope = %q, want %q", got.Scope, tc.wantScope)
				}
				return
			}
			requireInvalidClient(t, tc.name, rec)
			if n := e.countAllGrantTokens(t); n != 1 {
				t.Fatalf("grant tokens = %d, want only the exchange's", n)
			}
			if got := e.liveGrant(t); got.RevokedAt.Valid || len(got.RefreshTokenHash) == 0 {
				t.Fatal("the refusal revoked the grant or cleared its refresh token")
			}
			cliMustExec(t, e.pool, tc.restore, e.product)
			decodeOAuthRefresh(t, e.refresh(t, tok.RefreshToken))
		})
	}
}

// refreshBlockedBy holds the grant row lock in a transaction, starts a refresh that must block on
// it (with the optional scope parameter), runs mutate inside the holding transaction, commits, and
// returns the refresh's response.
func (e *tokenEnv) refreshBlockedBy(t *testing.T, grantID uuid.UUID, refresh string, mutate func(context.Context, pgx.Tx), scope ...string) *httptest.ResponseRecorder {
	t.Helper()
	ctx := context.Background()
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit
	if _, err := tx.Exec(ctx, `SELECT 1 FROM oauth_grants WHERE id = $1 FOR UPDATE`, grantID); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var rec *httptest.ResponseRecorder
	wg.Add(1)
	go func() { defer wg.Done(); rec = e.refresh(t, refresh, scope...) }()
	waitForPgWaiting(t, e, "FROM oauth_grants WHERE id = $1 FOR UPDATE", 1)
	mutate(ctx, tx)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	return rec
}

// TestOAuthRefreshScopeLiveDB (D5): scope may narrow to a subset of the grant's CURRENT scopes,
// never widen; the grant is unchanged by a narrowing.
func TestOAuthRefreshScopeLiveDB(t *testing.T) {
	e := tokenSetup(t)
	tok := e.connect(t) // both scopes
	g := e.liveGrant(t)

	r := decodeOAuthRefresh(t, e.refresh(t, tok.RefreshToken, "jobs:read"))
	if r.Scope != "jobs:read" {
		t.Fatalf("narrowed scope = %q, want jobs:read", r.Scope)
	}
	var scopes []string
	if err := e.pool.QueryRow(context.Background(), `SELECT scopes FROM product_tokens WHERE id = $1`, e.tokenID(t, r.AccessToken)).Scan(&scopes); err != nil || !slices.Equal(scopes, []string{"jobs:read"}) {
		t.Fatalf("narrowed token scopes = %v (%v), want [jobs:read]", scopes, err)
	}
	if got := e.liveGrant(t); !slices.Equal(got.Scopes, g.Scopes) {
		t.Fatalf("a narrowing refresh changed the grant's scopes to %v", got.Scopes)
	}
	// A later refresh without scope gets the grant's full scopes again.
	if r2 := decodeOAuthRefresh(t, e.refresh(t, tok.RefreshToken)); r2.Scope != "jobs:run jobs:read" {
		t.Fatalf("scope without narrowing = %q", r2.Scope)
	}

	for _, bad := range []string{"jobs:run jobs:read jobs:admin", "bogus", "", "jobs:read jobs:read", " jobs:read", "jobs:read "} {
		before := e.countAllGrantTokens(t)
		requireOAuthError(t, e.refresh(t, tok.RefreshToken, bad), http.StatusBadRequest, "invalid_scope")
		if e.countAllGrantTokens(t) != before {
			t.Fatalf("scope %q: an invalid_scope refresh minted a token", bad)
		}
	}
}

// TestOAuthRefreshScopeCannotWidenLiveDB: a grant of jobs:read only cannot be refreshed into
// jobs:run, although the product is allowed it and the scope is a known one.
func TestOAuthRefreshScopeCannotWidenLiveDB(t *testing.T) {
	e := tokenSetup(t)
	tok := e.connectAs(t, e.jwt, func(v url.Values) { v.Set("scope", "jobs:read") })
	if tok.Scope != "jobs:read" {
		t.Fatalf("granted scope = %q", tok.Scope)
	}
	before := e.countAllGrantTokens(t)
	requireOAuthError(t, e.refresh(t, tok.RefreshToken, "jobs:run"), http.StatusBadRequest, "invalid_scope")
	requireOAuthError(t, e.refresh(t, tok.RefreshToken, "jobs:read jobs:run"), http.StatusBadRequest, "invalid_scope")
	if e.countAllGrantTokens(t) != before {
		t.Fatal("a widening refresh minted a token")
	}
	if r := decodeOAuthRefresh(t, e.refresh(t, tok.RefreshToken, "jobs:read")); r.Scope != "jobs:read" {
		t.Fatalf("scope = %q", r.Scope)
	}
}

// TestOAuthRefreshLiveTokenCapLiveDB (D5): the eleventh live access token is a 429 with a
// Retry-After until the oldest live one expires; it mints nothing and does not stamp the grant.
func TestOAuthRefreshLiveTokenCapLiveDB(t *testing.T) {
	e := tokenSetup(t)
	tok := e.connect(t)
	g := e.liveGrant(t)
	live := []uuid.UUID{}
	for i := 0; i < 9; i++ { // the exchange's token plus nine make ten
		live = append(live, e.seedGrantToken(t, g.ID, []string{"jobs:read"}))
	}
	cliMustExec(t, e.pool, `UPDATE product_tokens SET expires_at = now() + interval '90 seconds' WHERE id = $1`, live[0])
	cliMustExec(t, e.pool, `UPDATE oauth_grants SET refresh_last_used_at = NULL WHERE id = $1`, g.ID)

	rec := e.refresh(t, tok.RefreshToken)
	requireOAuthError(t, rec, http.StatusTooManyRequests, "temporarily_unavailable")
	if ra, err := strconv.Atoi(rec.Header().Get("Retry-After")); err != nil || ra < 85 || ra > 92 {
		t.Fatalf("Retry-After = %q, want about 90", rec.Header().Get("Retry-After"))
	}
	if n := e.grantTokenCount(t, g.ID, `AND NOT revoked AND expires_at > now()`); n != 10 {
		t.Fatalf("live grant tokens = %d after a capped refresh, want 10", n)
	}
	if e.liveGrant(t).RefreshLastUsedAt.Valid {
		t.Fatal("a capped refresh stamped refresh_last_used_at")
	}
	cliMustExec(t, e.pool, `UPDATE product_tokens SET expires_at = now() - interval '1 second' WHERE id = $1`, live[0])
	decodeOAuthRefresh(t, e.refresh(t, tok.RefreshToken))
}

// TestOAuthRefreshCapIsCountedUnderTheGrantLockLiveDB: with nine live tokens, a refresh waiting
// on the grant lock sees the tenth that the lock holder committed, and is refused.
func TestOAuthRefreshCapIsCountedUnderTheGrantLockLiveDB(t *testing.T) {
	e := tokenSetup(t)
	tok := e.connect(t)
	g := e.liveGrant(t)
	for i := 0; i < 8; i++ {
		e.seedGrantToken(t, g.ID, []string{"jobs:read"})
	}
	rec := e.refreshBlockedBy(t, g.ID, tok.RefreshToken, func(ctx context.Context, tx pgx.Tx) {
		if _, err := tx.Exec(ctx,
			`INSERT INTO product_tokens (user_id, product_id, name, token_hash, token_prefix, scopes, expires_at, grant_id)
			 VALUES ($1, $2, 'oauth access', $3, 'uzp_test', ARRAY['jobs:read'], now() + interval '1 hour', $4)`,
			e.user, e.product, sha256Of(uuid.NewString()), g.ID); err != nil {
			t.Fatal(err)
		}
	})
	requireOAuthError(t, rec, http.StatusTooManyRequests, "temporarily_unavailable")
	if n := e.grantTokenCount(t, g.ID, `AND NOT revoked AND expires_at > now()`); n != 10 {
		t.Fatalf("live grant tokens = %d, want 10: the bound was passed", n)
	}
}

// TestOAuthRefreshVersusGrantMutationLiveDB (D8): a refresh that found its grant before a revoke or
// a re-consent committed, and was blocked on the grant lock, re-checks under the lock and mints
// nothing: no live token survives a revoke, and a re-consent never yields a token with the old
// consent's refresh token.
func TestOAuthRefreshVersusGrantMutationLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(t *testing.T, e *tokenEnv, g store.OauthGrant) func(context.Context, pgx.Tx)
	}{
		{"owner revoke of a grant token (revokeGrantOfTokenTx)", func(t *testing.T, e *tokenEnv, g store.OauthGrant) func(context.Context, pgx.Tx) {
			return func(ctx context.Context, tx pgx.Tx) {
				if err := revokeGrantOfTokenTx(ctx, e.h.q.WithTx(tx), g.ID, &e.user); err != nil {
					t.Fatal(err)
				}
			}
		}},
		{"revokeGrantLocked (RFC 7009 and Revoke all)", func(t *testing.T, e *tokenEnv, g store.OauthGrant) func(context.Context, pgx.Tx) {
			return func(ctx context.Context, tx pgx.Tx) {
				if err := revokeGrantLocked(ctx, e.h.q.WithTx(tx), g.ID); err != nil {
					t.Fatal(err)
				}
			}
		}},
		{"re-consent with narrower scopes", func(t *testing.T, e *tokenEnv, g store.OauthGrant) func(context.Context, pgx.Tx) {
			return func(ctx context.Context, tx pgx.Tx) {
				q := e.h.q.WithTx(tx)
				if _, err := q.ReconsentOAuthGrant(ctx, store.ReconsentOAuthGrantParams{ID: g.ID, Scopes: []string{"jobs:read"}}); err != nil {
					t.Fatal(err)
				}
				// The new consent's code exchange stores its own refresh token.
				if _, err := q.SetOAuthGrantRefreshToken(ctx, store.SetOAuthGrantRefreshTokenParams{
					ID: g.ID, RefreshTokenHash: sha256Of(uuid.NewString()), RefreshTokenPrefix: "uzr_test",
				}); err != nil {
					t.Fatal(err)
				}
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := tokenSetup(t)
			tok := e.connect(t)
			g := e.liveGrant(t)
			before := e.countAllGrantTokens(t)
			rec := e.refreshBlockedBy(t, g.ID, tok.RefreshToken, tc.mutate(t, e, g))
			requireOAuthError(t, rec, http.StatusBadRequest, "invalid_grant")
			if got := e.countAllGrantTokens(t); got != before {
				t.Fatalf("the refresh minted %d token(s) after the grant changed", got-before)
			}
			if strings.HasPrefix(tc.name, "re-consent") {
				return
			}
			if n := e.grantTokenCount(t, g.ID, `AND NOT revoked`); n != 0 {
				t.Fatalf("%d unrevoked grant tokens survive the revoke", n)
			}
			e.wantWhoami(t, "the access token of a revoked grant", tok.AccessToken, http.StatusUnauthorized)
		})
	}
}

// TestOAuthRefreshVersusRevokeAllLiveDB: Revoke all racing a refresh, both waiting on the grant
// lock, ends with no live grant and no live token whichever wins.
func TestOAuthRefreshVersusRevokeAllLiveDB(t *testing.T) {
	e := tokenSetup(t)
	ctx := context.Background()
	var refreshedFirst, refusedAfter int
	for i := 0; i < 6; i++ {
		tok := e.connect(t)
		g := e.liveGrant(t)
		tx, err := e.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `SELECT 1 FROM oauth_grants WHERE id = $1 FOR UPDATE`, g.ID); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		var ref, rev *httptest.ResponseRecorder
		wg.Add(2)
		go func() { defer wg.Done(); ref = e.refresh(t, tok.RefreshToken) }()
		go func() {
			defer wg.Done()
			rev = cookieReq(t, e.routes, http.MethodPost, "/api/me/cli-tokens/revoke-all", e.jwt, "")
		}()
		waitForPgWaiting(t, e, "FROM oauth_grants WHERE id = $1 FOR UPDATE", 1)
		waitForPgWaiting(t, e, "ORDER BY id", 1)
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		wg.Wait()
		if rev.Code != http.StatusNoContent {
			t.Fatalf("revoke-all = %d %q", rev.Code, rev.Body.String())
		}
		switch ref.Code {
		case http.StatusOK:
			refreshedFirst++
			e.wantWhoami(t, "a token refreshed before revoke-all", decodeOAuthRefresh(t, ref).AccessToken, http.StatusUnauthorized)
		case http.StatusBadRequest:
			refusedAfter++
			requireOAuthError(t, ref, http.StatusBadRequest, "invalid_grant")
		default:
			t.Fatalf("refresh = %d %q, want 200 (then revoked) or invalid_grant", ref.Code, ref.Body.String())
		}
		if n := e.grantTokenCount(t, g.ID, `AND NOT revoked`); n != 0 {
			t.Fatalf("iteration %d: %d unrevoked grant tokens survive revoke-all", i, n)
		}
		if n := e.liveGrantCount(t, e.user); n != 0 {
			t.Fatalf("iteration %d: %d live grants survive revoke-all", i, n)
		}
	}
	t.Logf("refresh won %d times, revoke-all won %d times", refreshedFirst, refusedAfter)
}

// TestOAuthRefreshStorageErrorIsNeverACredentialErrorLiveDB (D7): a storage or lookup error at any
// step of a refresh is 503 temporarily_unavailable with Retry-After, never invalid_grant (the
// product would discard the connection), and changes nothing: the same refresh token works after.
func TestOAuthRefreshStorageErrorIsNeverACredentialErrorLiveDB(t *testing.T) {
	e := tokenSetup(t)
	tok := e.connect(t)
	realQ := e.h.q
	pool := e.pool
	t.Cleanup(func() { e.h.q, e.h.oauthBeginTx = realQ, nil })

	want503 := func(t *testing.T) {
		t.Helper()
		before := e.countAllGrantTokens(t)
		rec := e.refresh(t, tok.RefreshToken)
		requireOAuthError(t, rec, http.StatusServiceUnavailable, "temporarily_unavailable")
		if rec.Header().Get("Retry-After") == "" {
			t.Fatal("503 without Retry-After")
		}
		e.h.q, e.h.oauthBeginTx = realQ, nil
		if got := e.countAllGrantTokens(t); got != before {
			t.Fatalf("a failed refresh left %d new token(s)", got-before)
		}
		if g := e.liveGrant(t); g.RevokedAt.Valid || g.RefreshLastUsedAt.Valid {
			t.Fatal("a failed refresh changed the grant")
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
		{"grant lookup by hash", "WHERE refresh_token_hash = $1"},
		{"grant lock", "FROM oauth_grants WHERE id = $1 FOR UPDATE"},
		{"product re-read", "FROM products WHERE id = $1"},
		{"user re-read", "FROM users WHERE id = $1"},
		{"live token count", "min(expires_at)"},
		{"access token insert", "INSERT INTO product_tokens"},
		{"idle stamp", "SET refresh_last_used_at = now()"},
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
	e.h.q, e.h.oauthBeginTx = realQ, nil
	decodeOAuthRefresh(t, e.refresh(t, tok.RefreshToken))
}

// TestOAuthRefreshTokenIsNotABearerLiveDB: a uzr_ refresh token presented as a Bearer is refused
// like an unknown credential on /api/v1 and on an internal RequireUser route. The exhaustive
// route walk is TestOAuthTokenAccessTokenIsRefusedWhereAPastedTokenIsLiveDB; this adds the
// refresh-token-specific names, after a refresh has happened.
func TestOAuthRefreshTokenIsNotABearerLiveDB(t *testing.T) {
	e := tokenSetup(t)
	tok := e.connect(t)
	decodeOAuthRefresh(t, e.refresh(t, tok.RefreshToken))
	for _, path := range []string{"/api/v1/whoami", "/api/me/product-tokens/", "/api/runs"} {
		if got := v1Send(e.routes, http.MethodGet, path, tok.RefreshToken, nil); got.status != http.StatusUnauthorized {
			t.Errorf("GET %s with a refresh token = %d %q, want 401", path, got.status, got.body)
		}
	}
}

// TestOAuthRefreshThenRevokeLoopIsRateBoundedLiveDB (D5, Resource bounds): refreshing and then
// revoking each new access token (RFC 7009) keeps the live count at zero, so the ten-live-token
// bound never fires; the per-grant mint-rate bound does. The grant's thirtieth mint of the hour
// (the exchange's plus twenty-nine refreshes) is the last: the next refresh is a 429
// temporarily_unavailable with a Retry-After, mints and stamps nothing, and a mint that has left
// the hour no longer counts.
func TestOAuthRefreshThenRevokeLoopIsRateBoundedLiveDB(t *testing.T) {
	e := tokenSetup(t)
	tok := e.connect(t)
	g := e.liveGrant(t)
	for i := 1; i < oauthMaxGrantMintsPerWindow; i++ {
		r := decodeOAuthRefresh(t, e.refresh(t, tok.RefreshToken))
		e.requireRevoked200(t, e.revoke(t, r.AccessToken, "access_token"))
	}
	if n := e.grantTokenCount(t, g.ID, ``); n != oauthMaxGrantMintsPerWindow {
		t.Fatalf("grant tokens = %d, want %d after the loop", n, oauthMaxGrantMintsPerWindow)
	}
	stamp := e.liveGrant(t).RefreshLastUsedAt

	rec := e.refresh(t, tok.RefreshToken)
	requireOAuthError(t, rec, http.StatusTooManyRequests, "temporarily_unavailable")
	ra, err := strconv.Atoi(rec.Header().Get("Retry-After"))
	if err != nil || ra < 1 || ra > 3600 {
		t.Fatalf("Retry-After = %q, want 1..3600 seconds", rec.Header().Get("Retry-After"))
	}
	if n := e.grantTokenCount(t, g.ID, ``); n != oauthMaxGrantMintsPerWindow {
		t.Fatalf("a refused refresh minted a token: %d rows", n)
	}
	if got := e.liveGrant(t); got.RevokedAt.Valid || !got.RefreshLastUsedAt.Time.Equal(stamp.Time) {
		t.Fatal("a rate-refused refresh revoked the grant or stamped refresh_last_used_at")
	}

	// Retry-After counts down to when the oldest mint leaves the hour: age it to 59 minutes.
	cliMustExec(t, e.pool, `UPDATE product_tokens SET created_at = now() - interval '59 minutes'
	                           WHERE id = (SELECT id FROM product_tokens WHERE grant_id = $1 ORDER BY created_at LIMIT 1)`, g.ID)
	rec = e.refresh(t, tok.RefreshToken)
	requireOAuthError(t, rec, http.StatusTooManyRequests, "temporarily_unavailable")
	if ra, err := strconv.Atoi(rec.Header().Get("Retry-After")); err != nil || ra < 55 || ra > 65 {
		t.Fatalf("Retry-After = %q, want about 60", rec.Header().Get("Retry-After"))
	}
	cliMustExec(t, e.pool, `UPDATE product_tokens SET created_at = now() - interval '61 minutes'
	                           WHERE id = (SELECT id FROM product_tokens WHERE grant_id = $1 ORDER BY created_at LIMIT 1)`, g.ID)
	decodeOAuthRefresh(t, e.refresh(t, tok.RefreshToken))
}
