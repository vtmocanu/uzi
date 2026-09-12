package handler

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/clitoken"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forge/forgetest"
)

// fakeForgeView is the injected forge for the auth suite: it returns canned rows so an
// authorized, enabled-repo read reaches a 200 without a live forge. Only the forge-view
// reads are overridden; everything else keeps BaseFake's loud default.
type fakeForgeView struct {
	forgetest.BaseFake
}

func (*fakeForgeView) ListMergeRequestRefs(context.Context, int64, forge.ListMergeRequestsOptions) ([]forge.MergeRequestRef, error) {
	return []forge.MergeRequestRef{{IID: 7, HeadSHA: "deadbeef"}}, nil
}

func (*fakeForgeView) GetMergeRequestSummary(_ context.Context, _ int64, iid int64) (forge.MergeRequestSummary, error) {
	return forge.MergeRequestSummary{IID: iid, Title: "t", HeadSHA: "deadbeef", State: forge.MRStateOpened}, nil
}

func (*fakeForgeView) ListChecks(context.Context, int64, string) ([]forge.Check, error) {
	return []forge.Check{{Name: "ci", Status: "completed", Conclusion: "success"}}, nil
}

func (*fakeForgeView) ListMergeRequestReviews(context.Context, int64, int64) ([]forge.Review, error) {
	return []forge.Review{{Author: "r", State: "APPROVED"}}, nil
}

func (*fakeForgeView) ListWorkflowRuns(context.Context, int64, forge.ListWorkflowRunsOptions) ([]forge.WorkflowRun, error) {
	return []forge.WorkflowRun{{ID: 100, Name: "CI"}}, nil
}

func (*fakeForgeView) ListPipelineJobs(context.Context, int64, int64) ([]forge.Job, error) {
	return []forge.Job{{ID: 1, Name: "build", Status: "success"}}, nil
}

// TestForgeViewRoutesAuthLiveDB is the router-level auth + enabled-gate proof for the
// four PRD #1255 M2a read routes AND the D12-moved ci-fix POST, driving the REAL
// h.Routes() router (the auth MOUNT is the point). It asserts, for each: a uzc_ Bearer
// is ACCEPTED (not 401), a cookie session is accepted, an anonymous request is 401,
// another user's repo is 404, and a disabled repo is 404 (the enabled gate; ci-fix is
// exempt — it has no enabled gate, so a disabled repo falls through to its own 409). A
// forge is injected via h.forgeFactory so no real forge is hit.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.
func TestForgeViewRoutesAuthLiveDB(t *testing.T) {
	h, router, pool := cliLiveDB(t)
	h.forgeFactory = func(string, string, []byte) (forge.Forge, error) { return &fakeForgeView{}, nil }

	owner := cliSeedUser(t, pool, false)
	stranger := cliSeedUser(t, pool, false)
	uzc := cliMintToken(t, pool, owner, clitoken.ScopeUser)
	jwt := cliMintJWT(t, pool, owner)

	connID := rmSeedConn(t, pool, owner)
	enabled := rmSeedRepo(t, pool, connID, 1255001, true)
	disabled := rmSeedRepo(t, pool, connID, 1255002, false)
	strangerConn := rmSeedConn(t, pool, stranger)
	foreign := rmSeedRepo(t, pool, strangerConn, 1255003, true)

	// The four GET reads share the enabled-gate + owner-scope contract.
	reads := []struct {
		name string
		path func(repo uuid.UUID) string
	}{
		{"pulls", func(id uuid.UUID) string { return fmt.Sprintf("/api/repos/%s/pulls", id) }},
		{"pull_detail", func(id uuid.UUID) string { return fmt.Sprintf("/api/repos/%s/pulls/7", id) }},
		{"ci_runs", func(id uuid.UUID) string { return fmt.Sprintf("/api/repos/%s/ci/runs", id) }},
		{"ci_run_detail", func(id uuid.UUID) string { return fmt.Sprintf("/api/repos/%s/ci/runs/100", id) }},
	}
	for _, rd := range reads {
		t.Run(rd.name, func(t *testing.T) {
			// Bearer, own ENABLED repo → 200 (Bearer accepted, gate passed, forge served).
			if rec := bearerReq(router, http.MethodGet, rd.path(enabled), uzc); rec.Code != http.StatusOK {
				t.Fatalf("Bearer GET enabled = %d, want 200 (Bearer accepted)\nbody: %s", rec.Code, rec.Body.String())
			}
			// Cookie session, own enabled repo → 200 too.
			if rec := cookieReq(t, router, http.MethodGet, rd.path(enabled), jwt, ""); rec.Code != http.StatusOK {
				t.Fatalf("cookie GET enabled = %d, want 200 (cookie accepted)\nbody: %s", rec.Code, rec.Body.String())
			}
			// Anonymous → 401 (the routes are Bearer-OR-cookie, never public).
			if rec := bearerReq(router, http.MethodGet, rd.path(enabled), ""); rec.Code != http.StatusUnauthorized {
				t.Fatalf("anonymous GET = %d, want 401\nbody: %s", rec.Code, rec.Body.String())
			}
			// Another user's repo → 404 (owner-scoped, existence-hidden), NOT 401/403.
			if rec := bearerReq(router, http.MethodGet, rd.path(foreign), uzc); rec.Code != http.StatusNotFound {
				t.Fatalf("Bearer GET foreign repo = %d, want 404\nbody: %s", rec.Code, rec.Body.String())
			}
			// Unknown repo id → 404 too.
			if rec := bearerReq(router, http.MethodGet, rd.path(uuid.New()), uzc); rec.Code != http.StatusNotFound {
				t.Fatalf("Bearer GET unknown repo = %d, want 404\nbody: %s", rec.Code, rec.Body.String())
			}
			// Own but DISABLED repo → 404 (the enabled gate; a disabled repo has no forge
			// view). This 404 (not 401) also proves the Bearer reached the handler.
			if rec := bearerReq(router, http.MethodGet, rd.path(disabled), uzc); rec.Code != http.StatusNotFound {
				t.Fatalf("Bearer GET disabled repo = %d, want 404 (enabled gate)\nbody: %s", rec.Code, rec.Body.String())
			}
		})
	}

	// The D12-moved ci-fix POST: now Bearer-reachable (was cookie-only). It has no
	// enabled gate, so an authorized request on an owned repo falls through to the
	// pipeline-cache precondition and 409s ("no cached pipeline for this ref") — a
	// not-401/not-404 that proves the Bearer reached the handler.
	t.Run("ci_fix_moved_to_bearer", func(t *testing.T) {
		ciFix := func(id uuid.UUID) string { return fmt.Sprintf("/api/repos/%s/ci-fix-runs", id) }
		body := `{"ref":"main"}`
		// Bearer, own repo → 409 (reached the handler; no cached pipeline).
		if rec := bearerReqBody(router, http.MethodPost, ciFix(enabled), uzc, body); rec.Code != http.StatusConflict {
			t.Fatalf("Bearer POST ci-fix own repo = %d, want 409 (Bearer accepted, no cached pipeline)\nbody: %s", rec.Code, rec.Body.String())
		}
		// Cookie session → 409 too (the cookie path still works after the move).
		if rec := cookieReq(t, router, http.MethodPost, ciFix(enabled), jwt, body); rec.Code != http.StatusConflict {
			t.Fatalf("cookie POST ci-fix = %d, want 409 (cookie accepted)\nbody: %s", rec.Code, rec.Body.String())
		}
		// Anonymous → 401.
		if rec := bearerReqBody(router, http.MethodPost, ciFix(enabled), "", body); rec.Code != http.StatusUnauthorized {
			t.Fatalf("anonymous POST ci-fix = %d, want 401\nbody: %s", rec.Code, rec.Body.String())
		}
		// Another user's repo → 404 (owner-scoped).
		if rec := bearerReqBody(router, http.MethodPost, ciFix(foreign), uzc, body); rec.Code != http.StatusNotFound {
			t.Fatalf("Bearer POST ci-fix foreign repo = %d, want 404\nbody: %s", rec.Code, rec.Body.String())
		}
	})
}
