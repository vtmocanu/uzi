package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/clitoken"
)

// ciaResp is the PUT /api/runs/{id}/ci-autofix response, decoded loosely.
type ciaResp struct {
	Run struct {
		CiAutofixEnabled *bool `json:"ci_autofix_enabled"`
	} `json:"run"`
	Governing *string  `json:"ci_autofix_governing_run_id"`
	Inflight  []string `json:"ci_autofix_inflight_fix_run_ids"`
}

func ciaPut(t *testing.T, router http.Handler, runID uuid.UUID, uzc, body string) (int, ciaResp, string) {
	t.Helper()
	rec := bearerReqBody(router, http.MethodPut, "/api/runs/"+runID.String()+"/ci-autofix", uzc, body)
	var out ciaResp
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode %s: %v", rec.Body.String(), err)
		}
	}
	return rec.Code, out, rec.Body.String()
}

func ciaColumn(t *testing.T, pool *pgxpool.Pool, runID uuid.UUID) *bool {
	t.Helper()
	var v *bool
	if err := pool.QueryRow(context.Background(), `SELECT ci_autofix_enabled FROM runs WHERE id = $1`, runID).Scan(&v); err != nil {
		t.Fatalf("read run: %v", err)
	}
	return v
}

// TestCLIReachesCIAutofixOverBearerLiveDB proves PUT /api/runs/{id}/ci-autofix is mounted
// in the RequireUser group (a uzc_ Bearer reaches it) and is owner-scoped: a foreign uzc_
// is a 404 that leaves the column untouched, and no credential is a 401.
func TestCLIReachesCIAutofixOverBearerLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	owner := cliSeedUser(t, pool, false)
	other := cliSeedUser(t, pool, false)
	ownerUzc := cliMintToken(t, pool, owner, clitoken.ScopeUser)
	otherUzc := cliMintToken(t, pool, other, clitoken.ScopeUser)

	repoID := cliSeedOwnedRepo(t, pool, owner)
	runID := uuid.New()
	cliMustExec(t, pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind)
		 VALUES ($1, $2, $3, 46170, 't', 'd', 'completed', 'issue')`, runID, owner, repoID)

	if code, _, body := ciaPut(t, router, runID, otherUzc, `{"enabled":false}`); code != http.StatusNotFound {
		t.Fatalf("foreign uzc_ PUT = %d, want 404\nbody: %s", code, body)
	}
	if v := ciaColumn(t, pool, runID); v != nil {
		t.Fatalf("a foreign uzc_ changed ci_autofix_enabled to %v; want NULL", *v)
	}
	code, resp, body := ciaPut(t, router, runID, ownerUzc, `{"enabled":false}`)
	if code != http.StatusOK {
		t.Fatalf("owner uzc_ PUT = %d, want 200\nbody: %s", code, body)
	}
	if v := ciaColumn(t, pool, runID); v == nil || *v {
		t.Fatalf("column = %v, want explicit false", v)
	}
	if resp.Run.CiAutofixEnabled == nil || *resp.Run.CiAutofixEnabled {
		t.Fatalf("response run.ci_autofix_enabled = %v, want false", resp.Run.CiAutofixEnabled)
	}
	if code, _, body := ciaPut(t, router, runID, "", `{"enabled":false}`); code != http.StatusUnauthorized {
		t.Fatalf("no-credential PUT = %d, want 401\nbody: %s", code, body)
	}
}

func TestSetRunCIAutofixEnabledTriStateLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	owner := cliSeedUser(t, pool, false)
	uzc := cliMintToken(t, pool, owner, clitoken.ScopeUser)
	repoID := cliSeedOwnedRepo(t, pool, owner)
	runID := uuid.New()
	cliMustExec(t, pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind)
		 VALUES ($1, $2, $3, 46171, 't', 'd', 'completed', 'issue')`, runID, owner, repoID)

	for _, tc := range []struct {
		body string
		want *bool
	}{
		{`{"enabled":true}`, ptrBool(true)},
		{`{"enabled":false}`, ptrBool(false)},
		{`{"enabled":null}`, nil},
	} {
		code, resp, body := ciaPut(t, router, runID, uzc, tc.body)
		if code != http.StatusOK {
			t.Fatalf("PUT %s = %d\nbody: %s", tc.body, code, body)
		}
		got := ciaColumn(t, pool, runID)
		if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
			t.Fatalf("PUT %s: column = %v, want %v", tc.body, got, tc.want)
		}
		if (resp.Run.CiAutofixEnabled == nil) != (tc.want == nil) {
			t.Fatalf("PUT %s: response run.ci_autofix_enabled = %v, want %v", tc.body, resp.Run.CiAutofixEnabled, tc.want)
		}
	}
	if code, _, body := ciaPut(t, router, runID, uzc, `{"enabled":`); code != http.StatusBadRequest {
		t.Fatalf("malformed body = %d, want 400\nbody: %s", code, body)
	}
}

func ptrBool(b bool) *bool { return &b }

// TestSetRunCIAutofixGoverningAndInflightLiveDB covers the response's governing run id and
// in-flight fix list on one branch.
func TestSetRunCIAutofixGoverningAndInflightLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	owner := cliSeedUser(t, pool, false)
	uzc := cliMintToken(t, pool, owner, clitoken.ScopeUser)
	repoID := cliSeedOwnedRepo(t, pool, owner)
	const branch = "agent/issue-46172"

	runA, runB, runC := uuid.New(), uuid.New(), uuid.New()
	cliMustExec(t, pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind, branch, mr_iid, created_at)
		 VALUES ($1, $2, $3, 46172, 't', 'd', 'completed', 'issue', $4, 7, now() - interval '2 hours')`, runA, owner, repoID, branch)
	cliMustExec(t, pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_title, issue_description, status, kind, branch, mr_iid, pipeline_ref, pipeline_id, created_at)
		 VALUES ($1, $2, $3, 't', 'd', 'completed', 'ci_fix', $4, 7, $4, 1, now() - interval '1 hour')`, runB, owner, repoID, branch)

	// A is not the selected run (B is newer): B governs.
	code, resp, body := ciaPut(t, router, runA, uzc, `{"enabled":false}`)
	if code != http.StatusOK {
		t.Fatalf("PUT on A = %d\nbody: %s", code, body)
	}
	if resp.Governing == nil || *resp.Governing != runB.String() {
		t.Fatalf("governing on A = %v, want %s", resp.Governing, runB)
	}
	// The edited run is the selected one: nothing else governs.
	code, resp, body = ciaPut(t, router, runB, uzc, `{"enabled":false}`)
	if code != http.StatusOK {
		t.Fatalf("PUT on B = %d\nbody: %s", code, body)
	}
	if resp.Governing != nil {
		t.Fatalf("governing on B = %v, want null", *resp.Governing)
	}
	if !strings.Contains(body, `"ci_autofix_governing_run_id":null`) || !strings.Contains(body, `"ci_autofix_inflight_fix_run_ids":[]`) {
		t.Fatalf("body must carry explicit null governing and empty array: %s", body)
	}

	// An active ci_fix on the ref is listed as in flight, as an array.
	cliMustExec(t, pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_title, issue_description, status, kind, pipeline_ref, pipeline_id)
		 VALUES ($1, $2, $3, 't', 'd', 'queued', 'ci_fix', $4, 2)`, runC, owner, repoID, branch)
	code, resp, body = ciaPut(t, router, runA, uzc, `{"enabled":true}`)
	if code != http.StatusOK {
		t.Fatalf("PUT on A = %d\nbody: %s", code, body)
	}
	if len(resp.Inflight) != 1 || resp.Inflight[0] != runC.String() {
		t.Fatalf("inflight = %v, want [%s]\nbody: %s", resp.Inflight, runC, body)
	}
}

// TestSetRunCIAutofixRefDerivationLiveDB pins the ref derivation per run kind when
// runs.branch is NULL, via the in-flight list of an active ci_fix on the derived ref, and
// the no-ref case.
func TestSetRunCIAutofixRefDerivationLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	owner := cliSeedUser(t, pool, false)
	uzc := cliMintToken(t, pool, owner, clitoken.ScopeUser)
	repoID := cliSeedOwnedRepo(t, pool, owner)

	issueRun, selfRun, promptRun, fixRun := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	issueRef := "agent/issue-46173"
	selfRef := "uzi/self-improve/" + selfRun.String()
	promptRef := "uzi/prompt-" + promptRun.String()
	fixRef := "agent/issue-46174"

	cliMustExec(t, pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind)
		 VALUES ($1, $2, $3, 46173, 't', 'd', 'completed', 'issue')`, issueRun, owner, repoID)
	cliMustExec(t, pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind)
		 VALUES ($1, $2, $3, 46175, 't', 'd', 'completed', 'self_improve')`, selfRun, owner, repoID)
	cliMustExec(t, pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_title, issue_description, status, kind)
		 VALUES ($1, $2, $3, 't', 'd', 'completed', 'prompt')`, promptRun, owner, repoID)
	cliMustExec(t, pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_title, issue_description, status, kind, pipeline_ref, pipeline_id)
		 VALUES ($1, $2, $3, 't', 'd', 'completed', 'ci_fix', $4, 3)`, fixRun, owner, repoID, fixRef)

	// One active ci_fix per derived ref (uq_runs_one_active_ci_fix allows one per ref).
	active := map[string]uuid.UUID{}
	for i, ref := range []string{issueRef, selfRef, promptRef, fixRef} {
		id := uuid.New()
		active[ref] = id
		cliMustExec(t, pool,
			`INSERT INTO runs (id, user_id, repo_id, issue_title, issue_description, status, kind, pipeline_ref, pipeline_id)
			 VALUES ($1, $2, $3, 't', 'd', 'queued', 'ci_fix', $4, $5)`, id, owner, repoID, ref, 100+i)
	}

	for _, tc := range []struct {
		name string
		run  uuid.UUID
		ref  string
	}{
		{"issue", issueRun, issueRef},
		{"self_improve", selfRun, selfRef},
		{"prompt", promptRun, promptRef},
		{"ci_fix", fixRun, fixRef},
	} {
		code, resp, body := ciaPut(t, router, tc.run, uzc, `{"enabled":true}`)
		if code != http.StatusOK {
			t.Fatalf("%s: PUT = %d\nbody: %s", tc.name, code, body)
		}
		if len(resp.Inflight) != 1 || resp.Inflight[0] != active[tc.ref].String() {
			t.Fatalf("%s: inflight = %v, want [%s] (ref %s)\nbody: %s", tc.name, resp.Inflight, active[tc.ref], tc.ref, body)
		}
	}

	// A run with no repo and so no derivable ref (a chat run) reports no governing run and
	// an empty array.
	noRef := uuid.New()
	cliMustExec(t, pool,
		`INSERT INTO runs (id, user_id, issue_title, issue_description, status, kind)
		 VALUES ($1, $2, 't', 'd', 'completed', 'chat')`, noRef, owner)
	code, resp, body := ciaPut(t, router, noRef, uzc, `{"enabled":false}`)
	if code != http.StatusOK {
		t.Fatalf("no-ref PUT = %d\nbody: %s", code, body)
	}
	if resp.Governing != nil || len(resp.Inflight) != 0 {
		t.Fatalf("no-ref: governing=%v inflight=%v, want null and []", resp.Governing, resp.Inflight)
	}
	if !strings.Contains(body, `"ci_autofix_inflight_fix_run_ids":[]`) {
		t.Fatalf("no-ref body must carry an empty array: %s", body)
	}
}
