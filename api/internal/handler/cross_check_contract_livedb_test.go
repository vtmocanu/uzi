package handler

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func TestCrossCheckAuthoritativeRoutesLiveDB(t *testing.T) {
	h, router, pool := cliLiveDB(t)
	h.wsvc.SetTxBeginner(pool)
	owner := cliSeedUser(t, pool, false)
	repo := rmSeedRepo(t, pool, rmSeedConn(t, pool, owner), 992, true)
	lead := rmSeedRun(t, pool, owner, repo, "running")
	worker := uuid.New()
	token := "contract-worker-" + uuid.NewString()
	hash := sha256.Sum256([]byte(token))
	cliMustExec(t, pool, `INSERT INTO workers(id,user_id,name,token_hash,status) VALUES($1,$2,'contract',$3,'online')`, worker, owner, hash[:])
	cliMustExec(t, pool, `UPDATE runs SET worker_id=$2,harness='claude',auto_approve=true,plan_cross_check_required=true,claim_generation=1 WHERE id=$1`, lead, worker)
	call := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	path := "/api/worker/runs/" + lead.String() + "/cross-checks"
	noRow := call(http.MethodGet, path+"/plan/1?claim_generation=1", "")
	var absent planCrossCheckNoRowResponse
	if noRow.Code != 200 || json.Unmarshal(noRow.Body.Bytes(), &absent) != nil || absent.Result != "no_row" || absent.ReasonClass != "no_candidate" {
		t.Fatalf("authoritative no-row: %d %s", noRow.Code, noRow.Body.String())
	}
	candidate := workersvc.PlanCrossCheckCandidate{PlanMd: "plan", Milestones: json.RawMessage("[]"), RequiredCapabilities: []string{}, RequiredTools: []string{}, SizeClass: "s", BaseCommit: strings.Repeat("a", 40), PlanningDiff: "diff"}
	digest, err := candidate.Digest()
	if err != nil {
		t.Fatal(err)
	}
	child := uuid.New()
	cliMustExec(t, pool, `INSERT INTO runs(id,user_id,repo_id,kind,target_run_id,harness,report_only,budget_wall_seconds,issue_title,issue_description)
 VALUES($1,$2,$3,'cross_check',$4,'codex',true,1800,'checker','candidate')`, child, owner, repo, lead)
	cliMustExec(t, pool, `INSERT INTO cross_checks(lead_run_id,checker_run_id,stage,round,lead_claim_generation,plan_md,milestones,size_class,base_commit,planning_diff,candidate_digest,verdict,reason_class,deadline_at)
 VALUES($1,$2,'plan',1,1,'plan','[]','s',$3,'diff',$4,'pending',NULL,now()+interval '30 minutes')`, lead, child, candidate.BaseCommit, digest)
	cliMustExec(t, pool, `UPDATE runs SET worker_id=$2, claim_generation=1, status='running' WHERE id=$1`, child, worker)
	verdictPath := "/api/worker/runs/" + child.String() + "/cross-check-verdict"
	invalid := call(http.MethodPost, verdictPath, `{"claim_generation":1,"verdict":"failed","reason_class":"timed_out","summary":"","items":[]}`)
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("worker supplied server-only timeout reason: %d %s", invalid.Code, invalid.Body.String())
	}
	passed := call(http.MethodPost, verdictPath, `{"claim_generation":1,"verdict":"approve","reason_class":"approve","summary":"ok","items":[]}`)
	if passed.Code != http.StatusOK {
		t.Fatalf("accepted verdict route: %d %s", passed.Code, passed.Body.String())
	}
	body, err := json.Marshal(planCrossCheckRequest{Stage: "plan", ClaimGeneration: func() *int64 { v := int64(1); return &v }(), PlanCrossCheckCandidate: candidate})
	if err != nil {
		t.Fatal(err)
	}
	// No Codex credential exists: the decided lost-ACK retry must precede availability.
	retry := call(http.MethodPost, path, string(body))
	var got planCrossCheckCandidateResponse
	if retry.Code != 200 || json.Unmarshal(retry.Body.Bytes(), &got) != nil || got.Verdict != "approve" || got.CandidateDigest != hex.EncodeToString(digest) || got.Candidate.PlanMd != "plan" || got.CandidateGeneration != 1 {
		t.Fatalf("decided same-generation reuse: %d %s", retry.Code, retry.Body.String())
	}
	cliMustExec(t, pool, `UPDATE runs SET claim_generation=2,last_seq=7 WHERE id=$1`, lead)
	stale := call(http.MethodGet, path+"/plan/1?claim_generation=1", "")
	if stale.Code != 409 {
		t.Fatalf("stale caller: %d %s", stale.Code, stale.Body.String())
	}
	historical := call(http.MethodGet, path+"/plan/1?claim_generation=2", "")
	if historical.Code != 200 || json.Unmarshal(historical.Body.Bytes(), &got) != nil || got.Verdict != "failed" || got.ReasonClass != "interrupted" || got.LeadLastSeq != 7 || got.CandidateGeneration != 1 {
		t.Fatalf("historical approval became authority: %d %s", historical.Code, historical.Body.String())
	}
	foreignOwner := cliSeedUser(t, pool, false)
	foreignRepo := rmSeedRepo(t, pool, rmSeedConn(t, pool, foreignOwner), 993, true)
	foreign := rmSeedRun(t, pool, foreignOwner, foreignRepo, "running")
	if rec := call(http.MethodGet, "/api/worker/runs/"+foreign.String()+"/cross-checks/plan/1?claim_generation=1", ""); rec.Code != 409 {
		t.Fatalf("foreign ownership: %d %s", rec.Code, rec.Body.String())
	}
}
