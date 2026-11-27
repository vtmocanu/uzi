package handler

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/clitoken"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
)

// The live-DB Bearer proof for PRD #4173 M6: two read-only issue tracker routes moved from the
// session-only RequireAuth group to RequireUser, so the CLI's uzc_ token reaches them
// (GET /api/issue-trackers/connections and GET /api/repos/{id}/issue-tracker-link/states).
// Every other issue tracker route stays session-only, so the same token gets a 401 there: no
// route that takes a token and not the link, the State mapping write, the project and tag
// reads or the allowlist read moved. It drives the real h.Routes() router because the auth
// mount is the point.
func TestIssueTrackerCLIBearerReachesOnlyTheTwoReadsLiveDB(t *testing.T) {
	f := newStateMappingFixture(t)
	lim := mw.NewLimiter(100000, time.Minute, nil)
	router := f.h.Routes(lim, lim, lim, lim, lim, lim, lim, lim, lim, lim, lim)
	uzc := cliMintToken(t, f.pool, f.user.ID, clitoken.ScopeUser)
	repo := f.repo.String()
	conn := uuid.NewString()

	for _, path := range []string{
		"/api/issue-trackers/connections",
		"/api/repos/" + repo + "/issue-tracker-link/states",
	} {
		if rec := bearerReq(router, http.MethodGet, path, uzc); rec.Code != http.StatusOK {
			t.Errorf("Bearer GET %s = %d, want 200\nbody: %s", path, rec.Code, rec.Body.String())
		}
		if rec := bearerReq(router, http.MethodGet, path, ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("no-credential GET %s = %d, want 401", path, rec.Code)
		}
	}

	// Another user's CLI token reaches the route but not this repo's mapping: 404, as with a
	// cookie (PR #4757 review). The route is open to CLI tokens; the repo ownership check is not.
	stranger := uuid.New()
	if _, err := f.pool.Exec(context.Background(), `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		stranger, "cli-stranger-"+stranger.String()[:8]+"@e2e"); err != nil {
		t.Fatal(err)
	}
	strangerTok := cliMintToken(t, f.pool, stranger, clitoken.ScopeUser)
	if rec := bearerReq(router, http.MethodGet, "/api/repos/"+repo+"/issue-tracker-link/states", strangerTok); rec.Code != http.StatusNotFound {
		t.Errorf("another user's Bearer GET states = %d, want 404\nbody: %s", rec.Code, rec.Body.String())
	}

	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/api/issue-trackers/connections"},
		{http.MethodDelete, "/api/issue-trackers/connections/" + conn},
		{http.MethodGet, "/api/issue-trackers/connections/" + conn + "/projects"},
		{http.MethodGet, "/api/issue-trackers/connections/" + conn + "/tags"},
		{http.MethodPut, "/api/repos/" + repo + "/issue-tracker-link"},
		{http.MethodPut, "/api/repos/" + repo + "/issue-tracker-link/states"},
		{http.MethodGet, "/api/issue-trackers/config"},
	} {
		if rec := bearerReqBody(router, tc.method, tc.path, uzc, "{}"); rec.Code != http.StatusUnauthorized {
			t.Errorf("Bearer %s %s = %d, want 401 (session-only)\nbody: %s", tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}
}
