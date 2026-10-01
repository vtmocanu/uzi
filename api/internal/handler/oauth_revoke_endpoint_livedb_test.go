package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/producttoken"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #1910 M4: POST /api/oauth/revoke (RFC 7009) through the PRODUCTION router against a real
// Postgres. Skipped unless UZI_TEST_DATABASE_URL points at a throwaway one.

func revokeForm(token string, hint ...string) url.Values {
	v := url.Values{"token": {token}}
	if len(hint) > 0 {
		v.Set("token_type_hint", hint[0])
	}
	return v
}

func (e *tokenEnv) revoke(t *testing.T, token string, hint ...string) *httptest.ResponseRecorder {
	t.Helper()
	return e.postTo(t, "/api/oauth/revoke", revokeForm(token, hint...), nil)
}

func (e *tokenEnv) requireRevoked200(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke = %d %q, want 200", rec.Code, rec.Body.String())
	}
	assertOAuthNoStore(t, rec)
	if body := strings.TrimSpace(rec.Body.String()); body != "" {
		t.Fatalf("revoke 200 body = %q, want empty", body)
	}
}

// seedManualToken mints a manual (grant_id NULL) product token for e.user on the product.
func (e *tokenEnv) seedManualToken(t *testing.T, product uuid.UUID) string {
	t.Helper()
	tok, hash, prefix, err := producttoken.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.h.q.CreateProductToken(context.Background(), store.CreateProductTokenParams{
		UserID: e.user, ProductID: product, Name: "manual", TokenHash: hash, TokenPrefix: prefix,
		Scopes: []string{"jobs:read"}, ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
	return tok
}

// requireConnectionIntact asserts the grant is live with its refresh token, and the access token
// works. It mints nothing (a refresh would spend the grant's ten-token bound over many subtests);
// requireRefreshWorks proves the refresh token itself.
func (e *tokenEnv) requireConnectionIntact(t *testing.T, tok string) {
	t.Helper()
	if g := e.liveGrant(t); g.RevokedAt.Valid || len(g.RefreshTokenHash) == 0 {
		t.Fatal("the grant was revoked or lost its refresh token")
	}
	e.wantWhoami(t, "the access token", tok, http.StatusOK)
}

func (e *tokenEnv) requireRefreshWorks(t *testing.T, refresh string) {
	t.Helper()
	e.wantWhoami(t, "a freshly refreshed token", decodeOAuthRefresh(t, e.refresh(t, refresh)).AccessToken, http.StatusOK)
}

// TestOAuthRevokeWithRefreshTokenKillsGrantLiveDB (D6): the product revoking its refresh token ends
// the connection: the grant, every access token under it (refreshed ones too) and the refresh token.
func TestOAuthRevokeWithRefreshTokenKillsGrantLiveDB(t *testing.T) {
	e := tokenSetup(t)
	tok := e.connect(t)
	r := decodeOAuthRefresh(t, e.refresh(t, tok.RefreshToken))
	g := e.liveGrant(t)

	e.requireRevoked200(t, e.revoke(t, tok.RefreshToken))
	e.wantWhoami(t, "the first access token", tok.AccessToken, http.StatusUnauthorized)
	e.wantWhoami(t, "the refreshed access token", r.AccessToken, http.StatusUnauthorized)
	e.requireRefreshRefused(t, "refresh after revoke", tok.RefreshToken, http.StatusBadRequest, "invalid_grant")
	if n := e.grantTokenCount(t, g.ID, `AND NOT revoked`); n != 0 {
		t.Fatalf("%d unrevoked grant tokens after revoke", n)
	}
	if n := e.liveGrantCount(t, e.user); n != 0 {
		t.Fatalf("%d live grants after revoke", n)
	}
	// Idempotent: an already revoked token is 200 again.
	e.requireRevoked200(t, e.revoke(t, tok.RefreshToken))
}

// TestOAuthRevokeAnotherClientsTokenIsInvalidGrantLiveDB (RFC 7009 section 2.1): a token issued to
// another client is 400 invalid_grant and revokes nothing, for both token types.
func TestOAuthRevokeAnotherClientsTokenIsInvalidGrantLiveDB(t *testing.T) {
	e := tokenSetup(t)
	tok := e.connect(t)
	other, otherSecret := e.seedClient2(t)
	asOther := func(r *http.Request) { r.SetBasicAuth(other.String(), otherSecret) }
	manual := e.seedManualToken(t, e.product)

	for name, token := range map[string]string{"refresh token": tok.RefreshToken, "access token": tok.AccessToken, "manual token": manual} {
		for _, hint := range []string{"", "refresh_token", "access_token"} {
			rec := e.postTo(t, "/api/oauth/revoke", revokeForm(token, hint), asOther)
			requireOAuthError(t, rec, http.StatusBadRequest, "invalid_grant")
		}
		t.Log(name, "refused for the other client")
	}
	e.requireConnectionIntact(t, tok.AccessToken)
	e.requireRefreshWorks(t, tok.RefreshToken)
	e.wantWhoami(t, "the manual token", manual, http.StatusOK)
}

// TestOAuthRevokeUnknownTokensAreOKLiveDB (RFC 7009 section 2.2): an unknown, malformed, oversized,
// expired or already revoked token answers 200 and changes nothing.
func TestOAuthRevokeUnknownTokensAreOKLiveDB(t *testing.T) {
	e := tokenSetup(t)
	tok := e.connect(t)
	expired := decodeOAuthRefresh(t, e.refresh(t, tok.RefreshToken)).AccessToken
	cliMustExec(t, e.pool, `UPDATE product_tokens SET expires_at = now() - interval '1 minute' WHERE id = $1`, e.tokenID(t, expired))
	revoked := decodeOAuthRefresh(t, e.refresh(t, tok.RefreshToken)).AccessToken
	e.requireRevoked200(t, e.revoke(t, revoked, "access_token"))
	e.wantWhoami(t, "the revoked token", revoked, http.StatusUnauthorized)

	for name, token := range map[string]string{
		"unknown refresh": "uzr_" + strings.Repeat("B", 43),
		"unknown access":  v1UnknownProductToken(t),
		"garbage":         "not a token",
		"oversized":       strings.Repeat("x", 5000),
		"expired access":  expired,
		"revoked access":  revoked,
	} {
		for _, hint := range []string{"", "refresh_token", "access_token", "banana"} {
			rec := e.postTo(t, "/api/oauth/revoke", revokeForm(token, hint), nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("%s with hint %q = %d %q, want 200", name, hint, rec.Code, rec.Body.String())
			}
		}
	}
	e.requireConnectionIntact(t, tok.AccessToken)
	e.requireRefreshWorks(t, tok.RefreshToken)
}

// TestOAuthRevokeHintIsAdvisoryLiveDB (RFC 7009 section 2.1): the hint never decides what is
// revoked. A misleading or unknown hint still finds the token.
func TestOAuthRevokeHintIsAdvisoryLiveDB(t *testing.T) {
	for _, hint := range []string{"access_token", "refresh_token", "banana", ""} {
		t.Run("refresh token with hint "+strconv.Quote(hint), func(t *testing.T) {
			e := tokenSetup(t)
			tok := e.connect(t)
			e.requireRevoked200(t, e.postTo(t, "/api/oauth/revoke", revokeForm(tok.RefreshToken, hint), nil))
			e.wantWhoami(t, "the access token", tok.AccessToken, http.StatusUnauthorized)
			e.requireRefreshRefused(t, "refresh", tok.RefreshToken, http.StatusBadRequest, "invalid_grant")
		})
		t.Run("access token with hint "+strconv.Quote(hint), func(t *testing.T) {
			e := tokenSetup(t)
			tok := e.connect(t)
			e.requireRevoked200(t, e.postTo(t, "/api/oauth/revoke", revokeForm(tok.AccessToken, hint), nil))
			e.wantWhoami(t, "the revoked access token", tok.AccessToken, http.StatusUnauthorized)
			// RFC 7009 section 2.1 lets the server keep the grant: it does, and the product refreshes.
			if g := e.liveGrant(t); g.RevokedAt.Valid {
				t.Fatal("revoking an access token revoked the grant")
			}
			r := decodeOAuthRefresh(t, e.refresh(t, tok.RefreshToken))
			e.wantWhoami(t, "a refreshed token", r.AccessToken, http.StatusOK)
		})
	}
}

// TestOAuthRevokeAccessTokenRevokesOnlyThatTokenLiveDB: sibling access tokens of the grant stay
// valid; a manual token of the client's product is not the client's to revoke (200, untouched).
func TestOAuthRevokeAccessTokenRevokesOnlyThatTokenLiveDB(t *testing.T) {
	e := tokenSetup(t)
	tok := e.connect(t)
	sibling := decodeOAuthRefresh(t, e.refresh(t, tok.RefreshToken)).AccessToken
	manual := e.seedManualToken(t, e.product)

	e.requireRevoked200(t, e.revoke(t, tok.AccessToken, "access_token"))
	e.wantWhoami(t, "the revoked token", tok.AccessToken, http.StatusUnauthorized)
	e.wantWhoami(t, "the sibling token", sibling, http.StatusOK)
	e.requireRevoked200(t, e.revoke(t, tok.AccessToken, "access_token")) // idempotent

	e.requireRevoked200(t, e.revoke(t, manual))
	e.wantWhoami(t, "the manual token after a revoke request", manual, http.StatusOK)
	var revoked bool
	if err := e.pool.QueryRow(context.Background(), `SELECT revoked FROM product_tokens WHERE id = $1`, e.tokenID(t, manual)).Scan(&revoked); err != nil || revoked {
		t.Fatalf("the manual token was revoked through the OAuth endpoint (revoked=%v, err=%v)", revoked, err)
	}
	e.requireConnectionIntact(t, sibling)
	e.requireRefreshWorks(t, tok.RefreshToken)
}

// TestOAuthRevokeRequestRulesLiveDB: the revoke endpoint authenticates and parses like the token
// endpoint.
func TestOAuthRevokeRequestRulesLiveDB(t *testing.T) {
	e := tokenSetup(t)
	tok := e.connect(t)
	post := func(form url.Values, path string, mut func(*http.Request)) *httptest.ResponseRecorder {
		return e.postTo(t, "/api/oauth/revoke"+path, form, mut)
	}
	requireOAuthError(t, post(url.Values{}, "", nil), http.StatusBadRequest, "invalid_request")
	requireOAuthError(t, post(url.Values{"token": {""}}, "", nil), http.StatusBadRequest, "invalid_request")
	requireOAuthError(t, post(url.Values{"token": {tok.RefreshToken, tok.RefreshToken}}, "", nil), http.StatusBadRequest, "invalid_request")
	requireOAuthError(t, post(url.Values{"token": {tok.RefreshToken}, "token_type_hint": {"a", "b"}}, "", nil), http.StatusBadRequest, "invalid_request")
	requireOAuthError(t, post(revokeForm(tok.RefreshToken), "?token=x", nil), http.StatusBadRequest, "invalid_request")
	requireOAuthError(t, post(revokeForm(tok.RefreshToken), "", func(r *http.Request) { r.Header.Set("Content-Type", "application/json") }), http.StatusBadRequest, "invalid_request")
	withSecret := revokeForm(tok.RefreshToken)
	withSecret.Set("client_secret", e.secret)
	requireOAuthError(t, post(withSecret, "", nil), http.StatusBadRequest, "invalid_request")
	for name, mut := range map[string]func(*http.Request){
		"no credentials": func(r *http.Request) { r.Header.Del("Authorization") },
		"wrong secret":   func(r *http.Request) { r.SetBasicAuth(e.product.String(), e.secret+"x") },
		"unknown client": func(r *http.Request) { r.SetBasicAuth(uuid.NewString(), e.secret) },
	} {
		rec := post(revokeForm(tok.RefreshToken), "", mut)
		requireOAuthError(t, rec, http.StatusUnauthorized, "invalid_client")
		if got := rec.Header().Get("WWW-Authenticate"); !strings.HasPrefix(got, "Basic") {
			t.Fatalf("%s: WWW-Authenticate = %q", name, got)
		}
	}
	// Nothing above revoked anything: only a well-formed, authenticated request does.
	e.requireConnectionIntact(t, tok.AccessToken)
	e.requireRefreshWorks(t, tok.RefreshToken)
	if rec := e.postTo(t, "/api/oauth/revoke", revokeForm(tok.RefreshToken), nil); rec.Code != http.StatusOK {
		t.Fatalf("revoke = %d", rec.Code)
	}
}

// TestOAuthRevokeStorageErrorIsA503LiveDB (D7): a storage or lookup error is 503
// temporarily_unavailable with Retry-After, never an OAuth credential error, and revokes nothing:
// the same request succeeds afterwards.
func TestOAuthRevokeStorageErrorIsA503LiveDB(t *testing.T) {
	e := tokenSetup(t)
	realQ := e.h.q
	pool := e.pool
	t.Cleanup(func() { e.h.q, e.h.oauthBeginTx = realQ, nil })

	for _, kind := range []string{"refresh token", "access token"} {
		tok := e.connect(t)
		token := tok.RefreshToken
		if kind == "access token" {
			token = tok.AccessToken
		}
		want503 := func(t *testing.T) {
			t.Helper()
			rec := e.revoke(t, token)
			requireOAuthError(t, rec, http.StatusServiceUnavailable, "temporarily_unavailable")
			if rec.Header().Get("Retry-After") == "" {
				t.Fatal("503 without Retry-After")
			}
			e.h.q, e.h.oauthBeginTx = realQ, nil
			e.requireConnectionIntact(t, tok.AccessToken)
		}
		t.Run(kind+"/client lookup", func(t *testing.T) {
			e.h.q = store.New(oauthFailDB{DBTX: pool, failOn: "FROM products WHERE id = $1"})
			want503(t)
		})
		t.Run(kind+"/begin", func(t *testing.T) {
			e.h.oauthBeginTx = func(context.Context) (pgx.Tx, error) { return nil, errOAuthInjected }
			want503(t)
		})
		// The statements each path runs, in order. A refresh token is tried first, so an access
		// token also passes through the hash lookup (which finds nothing) before its own lookup.
		steps := []struct{ name, sql string }{
			{"grant lookup by hash", "WHERE refresh_token_hash = $1"},
			{"grant lock", "FROM oauth_grants WHERE id = $1 FOR UPDATE"},
		}
		if kind == "refresh token" {
			steps = append(steps, struct{ name, sql string }{"grant revoke", "SET revoked_at = now()"})
		} else {
			t.Run(kind+"/token lookup", func(t *testing.T) {
				e.h.q = store.New(oauthFailDB{DBTX: pool, failOn: "FROM product_tokens WHERE token_hash = $1"})
				want503(t)
			})
			steps = append(steps, struct{ name, sql string }{"token revoke", "UPDATE product_tokens SET revoked = true"})
		}
		for _, step := range steps {
			t.Run(kind+"/"+step.name, func(t *testing.T) {
				e.h.oauthBeginTx = func(ctx context.Context) (pgx.Tx, error) {
					tx, err := pool.Begin(ctx)
					return oauthFailTx{Tx: tx, failOn: step.sql}, err
				}
				want503(t)
			})
		}
		t.Run(kind+"/commit", func(t *testing.T) {
			e.h.oauthBeginTx = func(ctx context.Context) (pgx.Tx, error) {
				tx, err := pool.Begin(ctx)
				return oauthFailTx{Tx: tx, failCommit: true}, err
			}
			want503(t)
		})
	}
}

// TestOAuthRevokePerClientBudgetIgnoresFailedAuthenticationLiveDB (D7): as on /token, failed or
// missing client authentication spends only the per-IP budget; the per-client budget is drawn only
// after authentication.
func TestOAuthRevokePerClientBudgetIgnoresFailedAuthenticationLiveDB(t *testing.T) {
	e := tokenSetup(t)
	e.limiter = mw.NewLimiter(3, time.Hour, nil)
	e.routes = oauthTokenRouter(e.h, e.limiter)
	from := func(ip string) func(*http.Request) {
		return func(r *http.Request) { r.RemoteAddr = ip + ":4000" }
	}
	revokeAs := func(mut func(*http.Request)) *httptest.ResponseRecorder {
		return e.postTo(t, "/api/oauth/revoke", revokeForm("uzr_"+strings.Repeat("C", 43)), mut)
	}
	for i := 0; i < 5; i++ {
		rec := revokeAs(func(r *http.Request) {
			from("192.0.2." + strconv.Itoa(10+i))(r)
			r.SetBasicAuth(e.product.String(), e.secret+"x")
		})
		requireOAuthError(t, rec, http.StatusUnauthorized, "invalid_client")
	}
	for i := 0; i < 3; i++ {
		if rec := revokeAs(from("192.0.2." + strconv.Itoa(30+i))); rec.Code != http.StatusOK {
			t.Fatalf("authenticated revoke %d = %d %q, want 200 (failed auth must not have spent the client's budget)", i+1, rec.Code, rec.Body.String())
		}
	}
	rec := revokeAs(from("192.0.2.40"))
	requireOAuthError(t, rec, http.StatusTooManyRequests, "temporarily_unavailable")
	if ra, err := strconv.Atoi(rec.Header().Get("Retry-After")); err != nil || ra < 1 {
		t.Fatalf("Retry-After = %q", rec.Header().Get("Retry-After"))
	}
}
