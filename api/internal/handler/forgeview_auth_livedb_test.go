package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forge/forgetest"
)

// fakeForgeView is the injected forge for the auth suite: it returns canned rows so an
// authorized, enabled-repo read reaches a 200 without a live forge. Only the forge-view
// reads are overridden; everything else keeps BaseFake's loud default.
//
// The three fields make the two forge-view reads controllable per iid without disturbing
// the default: all are nil in the zero fake, so ListMergeRequestRefs returns the single
// canned ref and GetMergeRequestSummary returns an OPENED summary with no error — the
// exact behavior the router-auth subtests below rely on. A non-nil field overrides only
// that axis: refs the ref list; summaryErr[iid] an error GetMergeRequestSummary returns
// for that iid (e.g. forge.ErrMergeRequestNotFound); summaryState[iid] the State it
// reports (default forge.MRStateOpened).
type fakeForgeView struct {
	forgetest.BaseFake
	refs         []forge.MergeRequestRef
	summaryErr   map[int64]error
	summaryState map[int64]string
}

func (f *fakeForgeView) ListMergeRequestRefs(context.Context, int64, forge.ListMergeRequestsOptions) ([]forge.MergeRequestRef, error) {
	if f.refs != nil {
		return f.refs, nil
	}
	return []forge.MergeRequestRef{{IID: 7, HeadSHA: "deadbeef"}}, nil
}

func (f *fakeForgeView) GetMergeRequestSummary(_ context.Context, _ int64, iid int64) (forge.MergeRequestSummary, error) {
	if err := f.summaryErr[iid]; err != nil {
		return forge.MergeRequestSummary{}, err
	}
	state := forge.MRStateOpened
	if s, ok := f.summaryState[iid]; ok {
		state = s
	}
	return forge.MergeRequestSummary{IID: iid, Title: "t", HeadSHA: "deadbeef", State: state}, nil
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

	// The following M2b subtests reconfigure the injected forge per case. They run AFTER
	// the table + ci_fix subtests above (t.Run executes in call order), so reassigning
	// h.forgeFactory here never perturbs the default-behavior cases already run. Since
	// M2b wired the forgememo under these routes, a subtest that re-requests a repo the
	// table cases already hit would be served the cached refs/summary rather than its
	// reconfigured fake; the two ListPulls cases therefore each seed their OWN enabled
	// repo (a distinct tenant prefix → cold memo), and the two GetPull cases use iids no
	// earlier case requested (cold summary keys), so each reconfigured fake is consulted.

	// GetPull: a PR that EXISTS but is not open (closed/merged/locked) must 404 — the
	// forge returns a summary with no error, so only the `s.State != MRStateOpened` guard
	// stops it. Genuine gate: drop that guard and this reaches ListChecks/ListReviews
	// (both canned-OK in the fake) and returns 200, failing this 404 assertion.
	t.Run("pull_detail_non_open_404", func(t *testing.T) {
		const iid = 42
		h.forgeFactory = func(string, string, []byte) (forge.Forge, error) {
			return &fakeForgeView{summaryState: map[int64]string{iid: forge.MRStateMerged}}, nil
		}
		path := fmt.Sprintf("/api/repos/%s/pulls/%d", enabled, iid)
		if rec := bearerReq(router, http.MethodGet, path, uzc); rec.Code != http.StatusNotFound {
			t.Fatalf("Bearer GET merged pull = %d, want 404 (PR exists but not open)\nbody: %s", rec.Code, rec.Body.String())
		}
	})

	// GetPull: a PR the forge 404s (ErrMergeRequestNotFound) also 404s here. Genuine
	// gate: drop the errors.Is branch and this error maps through writeForgeError to 502.
	t.Run("pull_detail_not_found_404", func(t *testing.T) {
		const iid = 43
		h.forgeFactory = func(string, string, []byte) (forge.Forge, error) {
			return &fakeForgeView{summaryErr: map[int64]error{iid: forge.ErrMergeRequestNotFound}}, nil
		}
		path := fmt.Sprintf("/api/repos/%s/pulls/%d", enabled, iid)
		if rec := bearerReq(router, http.MethodGet, path, uzc); rec.Code != http.StatusNotFound {
			t.Fatalf("Bearer GET not-found pull = %d, want 404\nbody: %s", rec.Code, rec.Body.String())
		}
	})

	// ListPulls: the refs list yields two PRs (A, B); B races closed between the list and
	// its per-iid summary fetch (ErrMergeRequestNotFound). The list must SURVIVE, returning
	// only A. Genuine gate: drop the `continue` skip and B's ErrMergeRequestNotFound maps
	// through writeForgeError to 502 — the list would not return 200 with A at all.
	t.Run("pulls_list_skips_raced_closed", func(t *testing.T) {
		const iidA, iidB = 11, 22
		repo := rmSeedRepo(t, pool, connID, 1255004, true) // fresh repo → cold memo for this case
		h.forgeFactory = func(string, string, []byte) (forge.Forge, error) {
			return &fakeForgeView{
				refs:       []forge.MergeRequestRef{{IID: iidA, HeadSHA: "aaa"}, {IID: iidB, HeadSHA: "bbb"}},
				summaryErr: map[int64]error{iidB: forge.ErrMergeRequestNotFound},
			}, nil
		}
		rec := bearerReq(router, http.MethodGet, fmt.Sprintf("/api/repos/%s/pulls", repo), uzc)
		if rec.Code != http.StatusOK {
			t.Fatalf("Bearer GET pulls with one raced-closed = %d, want 200 (raced-closed skipped, not fatal)\nbody: %s", rec.Code, rec.Body.String())
		}
		var got struct {
			Pulls []apitypes.PullDTO `json:"pulls"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode pulls body: %v\nbody: %s", err, rec.Body.String())
		}
		if len(got.Pulls) != 1 {
			t.Fatalf("pulls len = %d, want 1 (only the surviving PR)\nbody: %s", len(got.Pulls), rec.Body.String())
		}
		if got.Pulls[0].IID != iidA {
			t.Fatalf("surviving pull IID = %d, want %d (PR A, not the raced-closed B)", got.Pulls[0].IID, iidA)
		}
	})

	// ListPulls: a NON-sentinel error from one ref's summary fetch must still fail the
	// whole list (writeForgeError → 502), proving only ErrMergeRequestNotFound is skipped
	// — the skip is not a blanket swallow of every per-iid error.
	t.Run("pulls_list_real_error_502", func(t *testing.T) {
		const iidA, iidB = 11, 22
		repo := rmSeedRepo(t, pool, connID, 1255005, true) // fresh repo → cold memo for this case
		h.forgeFactory = func(string, string, []byte) (forge.Forge, error) {
			return &fakeForgeView{
				refs:       []forge.MergeRequestRef{{IID: iidA, HeadSHA: "aaa"}, {IID: iidB, HeadSHA: "bbb"}},
				summaryErr: map[int64]error{iidB: errors.New("boom")},
			}, nil
		}
		rec := bearerReq(router, http.MethodGet, fmt.Sprintf("/api/repos/%s/pulls", repo), uzc)
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("Bearer GET pulls with a non-sentinel error = %d, want 502 (only ErrMergeRequestNotFound is skipped)\nbody: %s", rec.Code, rec.Body.String())
		}
	})
}
