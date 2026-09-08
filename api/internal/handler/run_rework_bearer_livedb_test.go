package handler

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/clitoken"
)

// TestCLIReachesRunReworkOverBearerLiveDB proves the PRD #1202 on-demand rework route
// (POST /api/runs/{id}/rework) is mounted in the RequireUser group so the `uzi run rework`
// CLI verb reaches it from a uzc_ Bearer (the cookie-only-mis-mount trap the sibling
// mr-rework toggle guards against, cloned from TestCLIReachesMrReworkOverBearerLiveDB), and
// that it is owner-scoped (a foreign run 404s, not 403).
//
// It lives in its OWN file, not beside the mr-rework clone in cli_auth_livedb_test.go, on
// purpose: this suite's harness (cliLiveDB, bearerReqBody, cliSeed*) is shared across the
// package, but that file carries pre-existing cookie-based gosec findings that the
// whole-files lint ratchet would pull into the gate the moment it is touched — unrelated
// debt this PRD does not own.
//
// The owner's own call is driven against a run with NO merge request, so it reaches the
// handler and returns the service's 409 "this run has no merge request" WITHOUT touching the
// forge (h.svc is nil in this ceiling harness): the point here is the auth ceiling and the
// mount, not the forge read, which the service/handler unit tests cover.
func TestCLIReachesRunReworkOverBearerLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	owner := cliSeedUser(t, pool, false)
	other := cliSeedUser(t, pool, false)
	ownerUzc := cliMintToken(t, pool, owner, clitoken.ScopeUser)
	otherUzc := cliMintToken(t, pool, other, clitoken.ScopeUser)

	// A completed issue run owned by `owner` with NO mr_iid (mr_state left NULL): the handler
	// reaches the service, which 409s on "no merge request" before any forge read.
	repoID := cliSeedOwnedRepo(t, pool, owner)
	runID := uuid.New()
	cliMustExec(t, pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind)
		 VALUES ($1, $2, $3, 12020, 'rework me', 'desc', 'completed', 'issue')`, runID, owner, repoID)

	path := "/api/runs/" + runID.String() + "/rework"
	const body = `{"guidance":"only the migration thread"}`

	// A cross-user uzc_ is owner-scoped out: GetRunByIDForUser finds no row for `other` → 404,
	// never 403 (which would confirm the run's existence to someone who cannot see it).
	if rec := bearerReqBody(router, http.MethodPost, path, otherUzc, body); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-user uzc_ POST %s = %d, want 404 (rework is owner-scoped)\nbody: %s", path, rec.Code, rec.Body.String())
	}

	// The owner's uzc_ REACHES the handler (RequireUser mount, not 401): with no MR the
	// service returns 409 "this run has no merge request". A 401 here would mean the route was
	// cookie-only mis-mounted; a 404 would mean owner-scoping wrongly hid the owner's own run.
	rec := bearerReqBody(router, http.MethodPost, path, ownerUzc, body)
	if rec.Code != http.StatusConflict {
		t.Fatalf("owner uzc_ POST %s = %d, want 409 (reached the handler over Bearer)\nbody: %s", path, rec.Code, rec.Body.String())
	}
	if b := rec.Body.String(); !strings.Contains(b, "no merge request") {
		t.Fatalf("owner rework 409 body = %s, want the no-merge-request reason", b)
	}

	// No credential at all is still refused (401): proves the codes above are the credential
	// being honoured, not an unauthenticated route.
	if rec := bearerReqBody(router, http.MethodPost, path, "", body); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no-credential POST %s = %d, want 401 (rework is authenticated)\nbody: %s", path, rec.Code, rec.Body.String())
	}
}
