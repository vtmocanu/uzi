package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/auth"
	"github.com/vtmocanu/uzi/api/internal/producttoken"
)

// PRD #1910 M5: users and admins see and revoke connections. The user list/revoke
// (/api/me/oauth-connections) and the admin list/revoke (/api/admin/products/{id}/connections,
// /api/admin/oauth-connections/{id}/revoke) through the PRODUCTION router against a real Postgres.
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway one.

const (
	meConnectionsPath   = "/api/me/oauth-connections/"
	adminRevokeConnPath = "/api/admin/oauth-connections/"
)

func (e *tokenEnv) adminConnectionsPath() string {
	return "/api/admin/products/" + e.product.String() + "/connections"
}

func (e *tokenEnv) listMyConnections(t *testing.T, jwt string) []apitypes.OAuthConnectionDTO {
	t.Helper()
	rec := cookieReq(t, e.routes, http.MethodGet, meConnectionsPath, jwt, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d %q", meConnectionsPath, rec.Code, rec.Body.String())
	}
	var body struct {
		Connections []apitypes.OAuthConnectionDTO `json:"connections"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body.Connections
}

type adminConnectionList struct {
	Connections []apitypes.AdminOAuthConnectionDTO `json:"connections"`
	Truncated   bool                               `json:"truncated"`
}

func (e *tokenEnv) listAdminConnections(t *testing.T, jwt string) (string, adminConnectionList) {
	t.Helper()
	rec := cookieReq(t, e.routes, http.MethodGet, e.adminConnectionsPath(), jwt, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d %q", e.adminConnectionsPath(), rec.Code, rec.Body.String())
	}
	var body adminConnectionList
	dec := json.NewDecoder(strings.NewReader(rec.Body.String()))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	if body.Connections == nil {
		t.Fatalf("connections is null in %q, want []", rec.Body.String())
	}
	return rec.Body.String(), body
}

// expireGrantTokens makes every access token of the grant expired, so the connection is live with
// no usable access token (the case a token-based list would hide).
func (e *tokenEnv) expireGrantTokens(t *testing.T, grant uuid.UUID) {
	t.Helper()
	cliMustExec(t, e.pool, `UPDATE product_tokens SET expires_at = now() - interval '1 minute' WHERE grant_id = $1`, grant)
}

// revokeVia names one way to end a connection: the owner's route or the admin's.
type revokeVia struct {
	name   string
	revoke func(t *testing.T, e *tokenEnv, adminJWT string, grant uuid.UUID) int
}

var connectionRevokers = []revokeVia{
	{"user", func(t *testing.T, e *tokenEnv, _ string, grant uuid.UUID) int {
		return cookieReq(t, e.routes, http.MethodPost, "/api/me/oauth-connections/"+grant.String()+"/revoke", e.jwt, "").Code
	}},
	{"admin", func(t *testing.T, e *tokenEnv, adminJWT string, grant uuid.UUID) int {
		return cookieReq(t, e.routes, http.MethodPost, adminRevokeConnPath+grant.String()+"/revoke", adminJWT, "").Code
	}},
}

// TestConnectionWithOnlyExpiredTokensIsListedAndRevocableLiveDB: a grant whose access tokens have
// all expired is a live connection: it is listed for its owner and for the admin, and either can
// revoke it. A revoke sets revoked on the expired tokens too, so the job sweep still reaches the
// jobs they created (TestCancelRevokedProductJobsExpiredGrantTokenLiveDB).
func TestConnectionWithOnlyExpiredTokensIsListedAndRevocableLiveDB(t *testing.T) {
	for _, via := range connectionRevokers {
		t.Run(via.name, func(t *testing.T) {
			e := tokenSetup(t)
			adminJWT := cliMintJWT(t, e.pool, cliSeedUser(t, e.pool, true))
			e.connectAs(t, e.jwt, nil)
			g := e.liveGrant(t)
			extra := e.seedGrantToken(t, g.ID, []string{"jobs:run"})
			e.expireGrantTokens(t, g.ID)

			mine := e.listMyConnections(t, e.jwt)
			if len(mine) != 1 || mine[0].ID != g.ID.String() {
				t.Fatalf("user list = %+v, want the grant whose tokens all expired", mine)
			}
			_, admin := e.listAdminConnections(t, adminJWT)
			if len(admin.Connections) != 1 || admin.Connections[0].ID != g.ID.String() || admin.Truncated {
				t.Fatalf("admin list = %+v, want the grant whose tokens all expired", admin)
			}
			if admin.Connections[0].UserID != e.user.String() || admin.Connections[0].OwnerEmail == "" {
				t.Fatalf("admin row = %+v, want the connecting user's id and email", admin.Connections[0])
			}

			if code := via.revoke(t, e, adminJWT, g.ID); code != http.StatusNoContent {
				t.Fatalf("%s revoke of a connection with only expired tokens = %d, want 204", via.name, code)
			}
			if rg := e.grantByID(t, g.ID); !rg.RevokedAt.Valid || rg.RefreshTokenHash != nil {
				t.Fatalf("grant after the revoke = %+v", rg)
			}
			if n := e.grantTokenCount(t, g.ID, `AND NOT revoked`); n != 0 {
				t.Fatalf("%d grant tokens still unrevoked (an expired token must be revoked too)", n)
			}
			if !e.tokenRevoked(t, extra) {
				t.Fatal("the expired access token was not revoked")
			}
			if got := e.listMyConnections(t, e.jwt); len(got) != 0 {
				t.Fatalf("user list after the revoke = %+v", got)
			}
			if _, got := e.listAdminConnections(t, adminJWT); len(got.Connections) != 0 {
				t.Fatalf("admin list after the revoke = %+v", got)
			}
		})
	}
}

// TestConnectionRevokeKillsApiAndRefreshLiveDB: after a revoke from either list the next /api/v1
// call with the grant's access token is 401 and the next refresh is invalid_grant; a second revoke
// is a 404.
func TestConnectionRevokeKillsApiAndRefreshLiveDB(t *testing.T) {
	for _, via := range connectionRevokers {
		t.Run(via.name, func(t *testing.T) {
			e := tokenSetup(t)
			adminJWT := cliMintJWT(t, e.pool, cliSeedUser(t, e.pool, true))
			tok := e.connectAs(t, e.jwt, nil)
			g := e.liveGrant(t)
			e.wantWhoami(t, "the access token before", tok.AccessToken, http.StatusOK)

			if code := via.revoke(t, e, adminJWT, g.ID); code != http.StatusNoContent {
				t.Fatalf("%s revoke = %d, want 204", via.name, code)
			}
			e.wantWhoami(t, "the access token after the revoke", tok.AccessToken, http.StatusUnauthorized)
			requireOAuthError(t, e.refresh(t, tok.RefreshToken), http.StatusBadRequest, "invalid_grant")
			if code := via.revoke(t, e, adminJWT, g.ID); code != http.StatusNotFound {
				t.Fatalf("second %s revoke = %d, want 404", via.name, code)
			}
		})
	}
}

// TestUserRevokeConnectionIsOwnerScopedLiveDB: a foreign user's grant id, an unknown id and a
// malformed id are all refused without touching the grant; the owner is unaffected.
func TestUserRevokeConnectionIsOwnerScopedLiveDB(t *testing.T) {
	e := tokenSetup(t)
	other := cliSeedUser(t, e.pool, false)
	otherJWT := cliMintJWT(t, e.pool, other)
	theirs := e.connectAs(t, otherJWT, nil)
	var theirGrant uuid.UUID
	if err := e.pool.QueryRow(context.Background(), `SELECT id FROM oauth_grants WHERE user_id = $1`, other).Scan(&theirGrant); err != nil {
		t.Fatal(err)
	}
	mine := e.connectAs(t, e.jwt, nil)

	path := func(id string) string { return "/api/me/oauth-connections/" + id + "/revoke" }
	if rec := cookieReq(t, e.routes, http.MethodPost, path(theirGrant.String()), e.jwt, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("revoking another user's connection = %d %q, want 404", rec.Code, rec.Body.String())
	}
	if rec := cookieReq(t, e.routes, http.MethodPost, path(uuid.NewString()), e.jwt, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("revoking an unknown id = %d, want 404", rec.Code)
	}
	if rec := cookieReq(t, e.routes, http.MethodPost, path("not-a-uuid"), e.jwt, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("revoking a malformed id = %d, want 400", rec.Code)
	}
	if g := e.grantByID(t, theirGrant); g.RevokedAt.Valid || g.RefreshTokenHash == nil {
		t.Fatalf("the foreign grant changed: %+v", g)
	}
	e.wantWhoami(t, "the foreign user's token", theirs.AccessToken, http.StatusOK)
	e.wantWhoami(t, "the caller's own token", mine.AccessToken, http.StatusOK)
	// A valid cookie without the CSRF header is a 403 and revokes nothing.
	csrfReq := httptest.NewRequest(http.MethodPost, path(e.liveGrant(t).ID.String()), nil)
	csrfReq.AddCookie(&http.Cookie{Name: auth.AuthCookieName, Value: e.jwt}) //nolint:gosec // G124: test-only client cookie on an httptest request; Secure/HttpOnly/SameSite are response-side attributes irrelevant to a cookie a unit test sends.
	csrfRec := httptest.NewRecorder()
	e.routes.ServeHTTP(csrfRec, csrfReq)
	if csrfRec.Code != http.StatusForbidden {
		t.Fatalf("cookie POST without the CSRF header = %d, want 403", csrfRec.Code)
	}
	// No credential at all is a 401 and revokes nothing.
	if rec := bearerReqBody(e.routes, http.MethodPost, path(e.liveGrant(t).ID.String()), "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no credential = %d, want 401", rec.Code)
	}
	if n := e.liveGrantCount(t, e.user); n != 1 {
		t.Fatalf("live grants of the caller = %d, want 1", n)
	}
}

// TestConnectionRevokeRoutesRefuseBearerLiveDB: the user revoke is cookie-only like the list: a
// uzc_, a uza_, a manual uzp_, an OAuth access token and a refresh token (uzr_) are all 401 and
// revoke nothing. The admin revoke is cookie-only too: a uza_ Bearer is 401.
func TestConnectionRevokeRoutesRefuseBearerLiveDB(t *testing.T) {
	e := tokenSetup(t)
	admin := cliSeedUser(t, e.pool, true)
	tok := e.connectAs(t, e.jwt, nil)
	g := e.liveGrant(t)
	bearers := map[string]string{
		"uzc_":          cliMintToken(t, e.pool, e.user, "user"),
		"uza_":          cliMintToken(t, e.pool, admin, "admin_ro"),
		"uzp_ (manual)": v1MintProductToken(t, e.h.q, e.user, e.product, producttoken.Scopes, nil).token,
		"uzp_ (oauth)":  tok.AccessToken,
		"uzr_":          tok.RefreshToken,
	}
	for class, bearer := range bearers {
		for _, p := range []string{"/api/me/oauth-connections/" + g.ID.String() + "/revoke", adminRevokeConnPath + g.ID.String() + "/revoke"} {
			if rec := bearerReqBody(e.routes, http.MethodPost, p, bearer, ""); rec.Code != http.StatusUnauthorized {
				t.Errorf("POST %s with a %s Bearer = %d %q, want 401", p, class, rec.Code, rec.Body.String())
			}
		}
	}
	if rg := e.grantByID(t, g.ID); rg.RevokedAt.Valid {
		t.Fatal("a refused Bearer call revoked the grant")
	}
	e.wantWhoami(t, "the access token after the refused calls", tok.AccessToken, http.StatusOK)
}

// TestAdminConnectionRoutesAuthorizationLiveDB: a non-admin session is 403 on the list and on the
// revoke; the list is a read a uza_ token may make (a uzc_ and a uzp_ Bearer are not admin: 401 or
// 403); the revoke refuses a uza_ Bearer; an unknown product and an unknown grant are 404.
func TestAdminConnectionRoutesAuthorizationLiveDB(t *testing.T) {
	e := tokenSetup(t)
	admin := cliSeedUser(t, e.pool, true)
	adminJWT := cliMintJWT(t, e.pool, admin)
	tok := e.connectAs(t, e.jwt, nil)
	g := e.liveGrant(t)

	if rec := cookieReq(t, e.routes, http.MethodGet, e.adminConnectionsPath(), e.jwt, ""); rec.Code != http.StatusForbidden || strings.Contains(rec.Body.String(), g.ID.String()) {
		t.Fatalf("non-admin list = %d %q, want 403", rec.Code, rec.Body.String())
	}
	if rec := cookieReq(t, e.routes, http.MethodPost, adminRevokeConnPath+g.ID.String()+"/revoke", e.jwt, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin revoke = %d, want 403", rec.Code)
	}
	if rg := e.grantByID(t, g.ID); rg.RevokedAt.Valid {
		t.Fatal("a non-admin revoke ended the grant")
	}
	// uza_ may read the list and may not write.
	uza := cliMintToken(t, e.pool, admin, "admin_ro")
	rec := bearerReqBody(e.routes, http.MethodGet, e.adminConnectionsPath(), uza, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), g.ID.String()) {
		t.Fatalf("uza_ list = %d %q, want 200 with the grant", rec.Code, rec.Body.String())
	}
	if rec := bearerReqBody(e.routes, http.MethodPost, adminRevokeConnPath+g.ID.String()+"/revoke", uza, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("uza_ revoke = %d, want 401", rec.Code)
	}
	for class, bearer := range map[string]string{"uzc_": cliMintToken(t, e.pool, e.user, "user"), "uzp_": tok.AccessToken, "uzr_": tok.RefreshToken} {
		if rec := bearerReqBody(e.routes, http.MethodGet, e.adminConnectionsPath(), bearer, ""); rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
			t.Errorf("list with a %s Bearer = %d, want 401/403", class, rec.Code)
		}
	}
	if rec := cookieReq(t, e.routes, http.MethodGet, "/api/admin/products/"+uuid.NewString()+"/connections", adminJWT, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown product list = %d, want 404", rec.Code)
	}
	if rec := cookieReq(t, e.routes, http.MethodPost, adminRevokeConnPath+uuid.NewString()+"/revoke", adminJWT, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown grant revoke = %d, want 404", rec.Code)
	}
	e.wantWhoami(t, "the access token after the refused calls", tok.AccessToken, http.StatusOK)
}

// TestAdminProductConnectionsListLiveDB: the list is one product's live grants only (another
// product's, a revoked grant and the other users' are right or wrong by product and state), carries
// no token or hash, is newest-consent first, and is cut at the row bound with truncated set.
func TestAdminProductConnectionsListLiveDB(t *testing.T) {
	e := tokenSetup(t)
	adminUser := cliSeedUser(t, e.pool, true)
	adminJWT := cliMintJWT(t, e.pool, adminUser)

	first := e.connectAs(t, e.jwt, nil)
	g1 := e.liveGrant(t)
	second := cliSeedUser(t, e.pool, false)
	e.connectAs(t, cliMintJWT(t, e.pool, second), nil)
	var g2 uuid.UUID
	if err := e.pool.QueryRow(context.Background(), `SELECT id FROM oauth_grants WHERE user_id = $1`, second).Scan(&g2); err != nil {
		t.Fatal(err)
	}
	// A grant of another product is never listed, nor is a revoked grant of this one.
	otherProduct := oauthSeedClient(t, e.h, adminUser, []string{oauthTestRedirect}, []string{"jobs:run"})
	cliMustExec(t, e.pool, `INSERT INTO oauth_grants (user_id, product_id, scopes) VALUES ($1, $2, ARRAY['jobs:run'])`, e.user, otherProduct)
	third := cliSeedUser(t, e.pool, false)
	cliMustExec(t, e.pool, `INSERT INTO oauth_grants (user_id, product_id, scopes, revoked_at) VALUES ($1, $2, ARRAY['jobs:run'], now())`, third, e.product)
	// Consent order: g2 after g1.
	cliMustExec(t, e.pool, `UPDATE oauth_grants SET consented_at = now() - interval '1 hour' WHERE id = $1`, g1.ID)

	raw, got := e.listAdminConnections(t, adminJWT)
	ids := []string{}
	for _, c := range got.Connections {
		ids = append(ids, c.ID)
	}
	if !slices.Equal(ids, []string{g2.String(), g1.ID.String()}) || got.Truncated {
		t.Fatalf("admin list ids = %v truncated=%v, want [%s %s]", ids, got.Truncated, g2, g1.ID)
	}
	for _, secret := range []string{first.AccessToken, first.RefreshToken, "token_hash", "refresh_token"} {
		if strings.Contains(raw, secret) {
			t.Fatalf("the admin list leaks %q: %s", secret[:min(len(secret), 8)], raw)
		}
	}

	e.h.adminProductTokenRowsOverride = 1
	_, cut := e.listAdminConnections(t, adminJWT)
	if len(cut.Connections) != 1 || !cut.Truncated || cut.Connections[0].ID != g2.String() {
		t.Fatalf("cut list = %+v, want the newest connection and truncated", cut)
	}
}
