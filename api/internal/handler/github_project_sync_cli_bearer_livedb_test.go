package handler

import (
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/clitoken"
)

// The live-DB Bearer-accept proof for PRD #576 M7: the status READ
// (GET /api/repos/{id}/github-project-sync) and resync
// (POST /api/repos/{id}/github-project-sync/resync) were moved out of the cookie-only
// RequireAuth group into RequireUser so a uzc_ CLI Bearer reaches them (issue #428
// class — a cookie-only mount 401s a Bearer before the handler ever runs). This drives
// the REAL h.Routes() router because the auth MOUNT is the point, and only the real
// mounted middleware chain proves which group handler.go wired the path into.
//
// The discriminator needs no forge/service seeding, exactly as the sibling preflight
// suite: cliLiveDB builds the Handler with projectSync == nil, so a request that PASSES
// the owner preflight falls through to the handler's `h.projectSync == nil` guard and
// returns 500 ("service not wired"), while a request BLOCKED by the preflight returns
// 404. Crucially, a Bearer 401'd at a cookie-only mount would never reach EITHER — so a
// 404 (foreign repo) or 500 (owned repo) both prove the Bearer was accepted, and only a
// 401 would betray a mis-mount. A no-credential request still 401s (the move is
// cookie→also-Bearer, not authenticated→public).
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.
func TestGithubProjectSyncStatusCLIBearerAcceptedLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	owner := cliSeedUser(t, pool, false)
	uzc := cliMintToken(t, pool, owner, clitoken.ScopeUser)
	connID := rmSeedConn(t, pool, owner)
	ownRepo := rmSeedRepo(t, pool, connID, 576701, false)

	// (a) The headline: a Bearer GET for a nonexistent/unowned repo id is the
	// owner-scoped 404, NOT 401 — the Bearer reached the handler (RequireUser mount) and
	// was existence-hidden at the preflight. A cookie-only mount would have 401'd it here.
	if rec := bearerReq(router, http.MethodGet, gpsStatusPath(uuid.New()), uzc); rec.Code != http.StatusNotFound {
		t.Fatalf("Bearer GET unknown repo status = %d, want 404 (owner-scoped, NOT the 401 a cookie-only mount would return)\nbody: %s", rec.Code, rec.Body.String())
	}

	// (b) A Bearer GET for the caller's OWN repo passes the preflight and falls through
	// to the service-not-wired 500 — further proof the Bearer reached the handler, on a
	// repo where the preflight cannot be the thing that answered.
	if rec := bearerReq(router, http.MethodGet, gpsStatusPath(ownRepo), uzc); rec.Code != http.StatusInternalServerError {
		t.Fatalf("Bearer GET own repo status = %d, want 500 (preflight passes → service-not-wired)\nbody: %s", rec.Code, rec.Body.String())
	}

	// (c) resync moved with the status read: a Bearer POST for an unknown repo is the
	// owner-scoped 404 too, never a 401.
	if rec := bearerReq(router, http.MethodPost, gpsStatusPath(uuid.New())+"/resync", uzc); rec.Code != http.StatusNotFound {
		t.Fatalf("Bearer POST resync unknown repo = %d, want 404 (owner-scoped, NOT 401)\nbody: %s", rec.Code, rec.Body.String())
	}

	// (d) No credential at all → 401: the move opened the route to a Bearer, it did not
	// make it public.
	if rec := bearerReq(router, http.MethodGet, gpsStatusPath(ownRepo), ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no-credential GET status = %d, want 401\nbody: %s", rec.Code, rec.Body.String())
	}
}
