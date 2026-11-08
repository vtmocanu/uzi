package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/clitoken"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forge/forgetest"
)

// ciFixBindingForge serves what the Fix CI handler needs (failed jobs, a log tail, the CI
// config path) plus a scripted GetMergeRequestSummary per iid (issue #4079).
type ciFixBindingForge struct {
	forgetest.BaseFake
	summaries map[int64]forge.MergeRequestSummary
	errs      map[int64]error
	calls     atomic.Int64
}

func (*ciFixBindingForge) ListPipelineJobs(context.Context, int64, int64) ([]forge.Job, error) {
	return []forge.Job{{ID: 1, Name: "unit", Stage: "test", Status: "failed"}}, nil
}

func (*ciFixBindingForge) JobLogTail(context.Context, int64, int64, int) (string, error) {
	return "--- FAIL: TestFoo", nil
}

func (*ciFixBindingForge) ProjectCIConfigPath(context.Context, int64) (string, error) {
	return ".gitlab-ci.yml", nil
}

func (f *ciFixBindingForge) GetMergeRequestSummary(_ context.Context, _ int64, iid int64) (forge.MergeRequestSummary, error) {
	f.calls.Add(1)
	if err := f.errs[iid]; err != nil {
		return forge.MergeRequestSummary{}, err
	}
	s, ok := f.summaries[iid]
	if !ok {
		return forge.MergeRequestSummary{}, forge.ErrMergeRequestNotFound
	}
	return s, nil
}

type ciFixBindingBuilder struct {
	f   forge.Forge
	err error
}

func (b ciFixBindingBuilder) ForgeForConnection(string, string, []byte) (forge.Forge, error) {
	return b.f, b.err
}

func ciFixBindingSummary(iid int64, state, branch string) forge.MergeRequestSummary {
	return forge.MergeRequestSummary{IID: iid, State: state, SourceBranch: branch}
}

func ciFixBindingSeedPR(t *testing.T, pool *pgxpool.Pool, userID, repoID uuid.UUID, ref string, iid int64, age time.Duration) {
	t.Helper()
	cliMustExec(t, pool, `INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, branch, mr_iid, mr_state, status, harness, created_at)
	          VALUES ($1, $2, $3, 'issue', $4, 'prior', 'd', $5, $6, 'opened', 'completed', 'claude', $7)`,
		uuid.New(), userID, repoID, iid+500000, ref, iid, time.Now().Add(-age))
}

func ciFixBindingCount(t *testing.T, pool *pgxpool.Pool, repoID uuid.UUID) int {
	t.Helper()
	var c int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM runs WHERE repo_id = $1 AND kind = 'ci_fix'`, repoID).Scan(&c); err != nil {
		t.Fatalf("count ci_fix runs: %v", err)
	}
	return c
}

// TestCIFixBindingRouteLiveDB drives POST /api/repos/{id}/ci-fix-runs through the real router
// with a scripted forge: a branch whose older PR merged and whose newer PR is open binds to the
// open one; ambiguity and unverifiable branches are refused with no run (issue #4079).
func TestCIFixBindingRouteLiveDB(t *testing.T) {
	h, router, pool := cliLiveDB(t)
	h.cfg.CIFixMaxJobs = 5
	h.cfg.CIFixLogTailBytes = 4096
	h.wsvc.SetTxBeginner(pool)

	owner := cliSeedUser(t, pool, false)
	cliMustExec(t, pool, `INSERT INTO user_secrets (id, user_id, kind, label, is_default, ciphertext, sealed_with)
	          VALUES ($1, $2, 'anthropic_token', 'anthropic-default', true, $3, 'master')`, uuid.New(), owner, []byte("ct"))
	uzc := cliMintToken(t, pool, owner, clitoken.ScopeUser)
	connID := rmSeedConn(t, pool, owner)

	cases := []struct {
		name      string
		project   int64
		summaries func(ref string) map[int64]forge.MergeRequestSummary
		errs      map[int64]error
		wantCode  int
		wantBound int64  // 0: no bound PR expected
		buildErr  error  // the workersvc forge builder fails with this
		wantBody  string // substring the response body must contain
		notBody   string // substring it must not contain
	}{
		{"stale merged plus one open binds the open", 4079001, func(ref string) map[int64]forge.MergeRequestSummary {
			return map[int64]forge.MergeRequestSummary{
				40790010: ciFixBindingSummary(40790010, forge.MRStateMerged, ref),
				40790011: ciFixBindingSummary(40790011, forge.MRStateOpened, ref),
			}
		}, nil, http.StatusCreated, 40790011, nil, "", ""},
		{"two open is a conflict", 4079002, func(ref string) map[int64]forge.MergeRequestSummary {
			return map[int64]forge.MergeRequestSummary{
				40790020: ciFixBindingSummary(40790020, forge.MRStateOpened, ref),
				40790021: ciFixBindingSummary(40790021, forge.MRStateOpened, ref),
			}
		}, nil, http.StatusConflict, 0, nil, "more than one open pull request uses this branch", ""},
		{"forge 404 is a conflict", 4079003, func(ref string) map[int64]forge.MergeRequestSummary {
			return map[int64]forge.MergeRequestSummary{
				40790031: ciFixBindingSummary(40790031, forge.MRStateMerged, ref),
			}
		}, nil, http.StatusConflict, 0, nil, "pull request 40790030 was not found", ""},
		{"transport error is a bad gateway", 4079004, func(ref string) map[int64]forge.MergeRequestSummary {
			return map[int64]forge.MergeRequestSummary{
				40790041: ciFixBindingSummary(40790041, forge.MRStateOpened, ref),
			}
		}, map[int64]error{40790040: errors.New("dial tcp: connection refused")}, http.StatusBadGateway, 0, nil, "could not verify the branch's pull request", ""},
		{"forge builder failure hides the raw error", 4079005, func(ref string) map[int64]forge.MergeRequestSummary {
			return nil
		}, nil, http.StatusBadGateway, 0, errors.New("pq: password authentication failed for user uzi_prod at db.internal"), "the pull-request lookup could not run", "db.internal"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repoID := rmSeedRepo(t, pool, connID, tc.project, true)
			ref := fmt.Sprintf("agent/issue-%d", tc.project)
			cliMustExec(t, pool, `INSERT INTO pipeline_statuses (repo_id, ref, pipeline_id, sha, status, web_url, synced_at)
			          VALUES ($1, $2, $3, 'abc1234', 'failed', 'https://forge.e2e/p', now())`, repoID, ref, tc.project*10)
			base := tc.project * 10
			ciFixBindingSeedPR(t, pool, owner, repoID, ref, base, 48*time.Hour)
			ciFixBindingSeedPR(t, pool, owner, repoID, ref, base+1, time.Hour)

			ff := &ciFixBindingForge{summaries: tc.summaries(ref), errs: tc.errs}
			h.forgeFactory = func(string, string, []byte) (forge.Forge, error) { return ff, nil }
			h.wsvc.SetForges(ciFixBindingBuilder{f: ff, err: tc.buildErr})

			path := fmt.Sprintf("/api/repos/%s/ci-fix-runs", repoID)
			rec := bearerReqBody(router, http.MethodPost, path, uzc, fmt.Sprintf(`{"ref":%q}`, ref))
			if rec.Code != tc.wantCode {
				t.Fatalf("POST ci-fix = %d, want %d\nbody: %s", rec.Code, tc.wantCode, rec.Body.String())
			}
			body := rec.Body.String()
			if tc.wantBody != "" && !strings.Contains(body, tc.wantBody) {
				t.Fatalf("body %q does not contain %q", body, tc.wantBody)
			}
			if tc.notBody != "" && strings.Contains(body, tc.notBody) {
				t.Fatalf("body %q must not contain %q", body, tc.notBody)
			}
			if tc.wantCode == http.StatusBadGateway {
				if n := strings.Count(body, "could not verify the branch's pull request"); n != 1 {
					t.Fatalf("502 body repeats the prefix %d times, want once: %s", n, body)
				}
			}
			if tc.wantCode != http.StatusCreated {
				if got := ciFixBindingCount(t, pool, repoID); got != 0 {
					t.Fatalf("ci_fix runs after a refusal = %d, want 0", got)
				}
				return
			}
			var created struct {
				Run map[string]json.RawMessage `json:"run"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
				t.Fatalf("decode 201 body: %v", err)
			}
			if raw, ok := created.Run["mr_iid"]; !ok || string(raw) != "null" {
				t.Fatalf("run.mr_iid = %s (present=%v), want null: the binding is not exposed", raw, ok)
			}
			var stored *int64
			if err := pool.QueryRow(context.Background(), `SELECT ci_fix_mr_iid FROM runs WHERE repo_id = $1 AND kind = 'ci_fix'`, repoID).Scan(&stored); err != nil {
				t.Fatalf("read stored binding: %v", err)
			}
			if stored == nil || *stored != tc.wantBound {
				t.Fatalf("stored ci_fix_mr_iid = %v, want %d", stored, tc.wantBound)
			}
		})
	}
}
