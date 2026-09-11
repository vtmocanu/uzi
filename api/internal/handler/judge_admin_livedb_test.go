package handler

import (
	"net/http"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/clitoken"
)

// TestAdminJudgeRoutesCeilingLiveDB pins PRD #1184 M1's read/write split for the admin "All
// users" judge routes against the REAL h.Routes() router — the only place the RequireUser +
// RequireAdminRO chain that gates them is actually wired. A fake handler call cannot prove the
// mount, and the token-masking ceiling (cli_auth.go clears IsAdmin for any non-admin_ro token)
// is a property of the real middleware, not of a handler.
//
// For each of the three GET routes: no credential is 401, a masked uzc_ (default-scope) token
// is 403 whether its owner is an admin or not, a non-admin session cookie is 403, and only a
// uza_ (admin_ro) token reaches the handler (200).
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; ./e2e/run-store-it.sh
// provides one and sweeps this package for the LiveDB suffix.
func TestAdminJudgeRoutesCeilingLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)

	admin := cliSeedUser(t, pool, true)
	member := cliSeedUser(t, pool, false)

	adminUza := cliMintToken(t, pool, admin, clitoken.ScopeAdminRO) // keeps IsAdmin — the only 200
	adminUzc := cliMintToken(t, pool, admin, clitoken.ScopeUser)    // masked to IsAdmin=false
	memberUzc := cliMintToken(t, pool, member, clitoken.ScopeUser)
	memberJWT := cliMintJWT(t, pool, member) // a non-admin browser session

	routes := []string{
		"/api/admin/judge/recommendations",
		"/api/admin/judge/stats",
		"/api/admin/judge/category-stats",
	}
	for _, p := range routes {
		// No credential at all: RequireUser 401s before the admin gate.
		if rec := bearerReq(router, http.MethodGet, p, ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("no-credential GET %s = %d, want 401\nbody: %s", p, rec.Code, rec.Body.String())
		}
		// An admin's DEFAULT-scope token is masked to IsAdmin=false ⇒ 403 (the F1 ceiling).
		if rec := bearerReq(router, http.MethodGet, p, adminUzc); rec.Code != http.StatusForbidden {
			t.Errorf("admin uzc_ GET %s = %d, want 403 (masked to IsAdmin=false)\nbody: %s", p, rec.Code, rec.Body.String())
		}
		// A non-admin's token: 403.
		if rec := bearerReq(router, http.MethodGet, p, memberUzc); rec.Code != http.StatusForbidden {
			t.Errorf("member uzc_ GET %s = %d, want 403\nbody: %s", p, rec.Code, rec.Body.String())
		}
		// A non-admin browser session (cookie): 403 too — RequireAdminRO is on IsAdmin, not scope.
		if rec := cookieReq(t, router, http.MethodGet, p, memberJWT, ""); rec.Code != http.StatusForbidden {
			t.Errorf("member session GET %s = %d, want 403\nbody: %s", p, rec.Code, rec.Body.String())
		}
		// The admin_ro token is the one credential that reaches the handler.
		if rec := bearerReq(router, http.MethodGet, p, adminUza); rec.Code != http.StatusOK {
			t.Errorf("admin uza_ GET %s = %d, want 200 (admin_ro reads the aggregate)\nbody: %s", p, rec.Code, rec.Body.String())
		}
	}
}
