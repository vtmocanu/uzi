package handler

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func TestSemanticWorkerRoutesLiveDB(t *testing.T) {
	h, router, pool := cliLiveDB(t)
	h.wsvc.SetTxBeginner(pool)
	owner := cliSeedUser(t, pool, false)
	repo := rmSeedRepo(t, pool, rmSeedConn(t, pool, owner), 1992, true)
	run := rmSeedRun(t, pool, owner, repo, "running")
	worker := uuid.New()
	token := "semantic-worker-" + uuid.NewString()
	hash := sha256.Sum256([]byte(token))
	cliMustExec(t, pool, `INSERT INTO workers(id,user_id,name,token_hash,status) VALUES($1,$2,'semantic',$3,'online')`, worker, owner, hash[:])
	const issue = "implement behavior"
	cliMustExec(t, pool, `UPDATE runs SET worker_id=$2,claim_generation=7,completion_contract_version=1,issue_description=$3 WHERE id=$1`, run, worker, issue)
	path := "/api/worker/runs/" + run.String() + "/semantic"
	call := func(method, suffix string, body any) *httptest.ResponseRecorder {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(method, path+suffix, strings.NewReader(string(raw)))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	structural := workersvc.PlanCrossCheckCandidate{PlanMd: "implement behavior", Milestones: json.RawMessage(`[{"id":"m1","title":"Behavior"}]`), RequiredCapabilities: []string{}, RequiredTools: []string{}, SizeClass: "s", BaseCommit: strings.Repeat("a", 40), PlanningDiff: "diff"}
	issueHash := sha256.Sum256([]byte(issue))
	proposal := apitypes.SemanticProposal{Criteria: []apitypes.ProposedSemanticCriterion{{
		MilestoneID: "m1", Kind: "code", Text: "behavior", DoneWhen: "behavior works",
		Citations:            []apitypes.SemanticCitation{{SourceID: "issue", Hash: hex.EncodeToString(issueHash[:]), Start: 0, End: len(issue)}},
		EvidenceRequirements: []apitypes.SemanticEvidence{{Description: "test output"}},
	}}}
	capture := apitypes.SemanticSourceCapture{ID: "issue", Kind: "issue", Provenance: "worker:untrusted-provenance", Completeness: "unknown", Bytes: []byte(issue)}
	unknown := apitypes.SemanticSourceCapture{ID: "missing-prd", Kind: "prd", Provenance: "repo:missing", Completeness: "unknown", Bytes: []byte{}}
	body := map[string]any{"claim_generation": 7, "structural": structural, "proposal": proposal, "sources": []apitypes.SemanticSourceCapture{capture, unknown}}
	if rec := call(http.MethodPost, "/candidate", body); rec.Code != http.StatusConflict {
		t.Fatalf("unenrolled: %d %s", rec.Code, rec.Body.String())
	}
	var count int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM semantic_planning WHERE run_id=$1`, run).Scan(&count); err != nil || count != 0 {
		t.Fatalf("route enrolled a run: %d %v", count, err)
	}
	cliMustExec(t, pool, `INSERT INTO semantic_planning(run_id,enrollment) VALUES($1,'{"profile":"semantic","policy":{"version":1,"model":"sonnet","max_reviews":3,"max_invocations":2,"max_tokens":120000,"max_seconds":900}}')`, run)
	rec := call(http.MethodPost, "/candidate", body)
	var compiled apitypes.SemanticCandidateResponse
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &compiled) != nil {
		t.Fatalf("candidate: %d %s", rec.Code, rec.Body.String())
	}
	if compiled.Candidate.Sources[0].Provenance != "server:run-issue" || compiled.Candidate.Sources[0].Completeness != "complete" {
		t.Fatal("worker issue provenance survived")
	}
	var persisted []byte
	if err := pool.QueryRow(t.Context(), `SELECT raw FROM semantic_snapshots WHERE run_id=$1 AND hash=$2`, run, compiled.Candidate.Sources[0].Hash).Scan(&persisted); err != nil || string(persisted) != issue {
		t.Fatalf("unpersisted server bytes: %v", err)
	}
	if len(compiled.Sources) != 2 || !bytes.Equal(compiled.Sources[0].Bytes, persisted) || compiled.Sources[0].Provenance != "server:run-issue" || compiled.Sources[0].Completeness != "complete" || !reflect.DeepEqual(compiled.Sources[1], unknown) {
		t.Fatalf("response did not return canonical persisted sources: %+v", compiled.Sources)
	}
	for i, ref := range compiled.Candidate.Sources {
		var raw []byte
		if err := pool.QueryRow(t.Context(), `SELECT raw FROM semantic_snapshots WHERE run_id=$1 AND hash=$2`, run, ref.Hash).Scan(&raw); err != nil || !bytes.Equal(raw, compiled.Sources[i].Bytes) {
			t.Fatalf("source %s differs from persisted data: %v", ref.ID, err)
		}
	}
	var plan []byte
	if err := pool.QueryRow(t.Context(), `SELECT raw FROM semantic_snapshots WHERE run_id=$1 AND hash=$2`, run, compiled.Candidate.PlanHash).Scan(&plan); err != nil || !bytes.Equal(compiled.Plan, plan) || !bytes.Equal(plan, []byte(structural.PlanMd)) {
		t.Fatalf("plan differs from persisted data: %v", err)
	}
	planHash := sha256.Sum256(compiled.Plan)
	if hex.EncodeToString(planHash[:]) != compiled.Candidate.PlanHash {
		t.Fatal("response plan hash mismatch")
	}
	// A lost candidate response can be retried without changing identity or bytes.
	rec = call(http.MethodPost, "/candidate", body)
	var replay apitypes.SemanticCandidateResponse
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &replay) != nil || !reflect.DeepEqual(replay, compiled) {
		t.Fatalf("candidate replay changed payload: %d %s", rec.Code, rec.Body.String())
	}
	capture.Bytes = []byte("forged issue")
	body["sources"] = []apitypes.SemanticSourceCapture{capture}
	if rec := call(http.MethodPost, "/candidate", body); rec.Code != http.StatusConflict {
		t.Fatal("forged issue accepted")
	}
	reserve := map[string]any{"claim_generation": 7, "request_id": uuid.NewString(), "candidate_digest": compiled.Binding.CandidateDigest, "source_digest": compiled.Binding.SourceDigest, "criteria_digest": compiled.Binding.CriteriaDigest, "supported": true}
	var reservation apitypes.SemanticCoverageReservation
	rec = call(http.MethodPost, "/coverage/reserve", reserve)
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &reservation) != nil || reservation.Outcome != "reserved" || string(reservation.Sources[0].Bytes) != issue {
		t.Fatalf("reserve: %d %s", rec.Code, rec.Body.String())
	}
	rec = call(http.MethodPost, "/coverage/settle", map[string]any{"claim_generation": 7, "review_id": reservation.ReviewID, "invocation_id": reservation.InvocationID, "status": "success", "tokens": 15})
	if rec.Code != http.StatusOK {
		t.Fatalf("settle: %d %s", rec.Code, rec.Body.String())
	}
	report := apitypes.SemanticCoverageReport{SemanticCoverageBinding: compiled.Binding,
		Requirements: []apitypes.SemanticCoverageResult{{RequirementID: "r1", Status: "represented", Explanation: "represented", Citations: compiled.Candidate.Criteria[0].Citations, CriterionIDs: []string{compiled.Candidate.Criteria[0].ID}}},
		Criteria:     []apitypes.SemanticCoverageAtomicity{{CriterionID: compiled.Candidate.Criteria[0].ID, Atomic: false, Explanation: "bundled behavior"}},
	}
	rec = call(http.MethodPost, "/coverage/publish", map[string]any{"claim_generation": 7, "review_id": reservation.ReviewID, "invocation_id": reservation.InvocationID, "report": report})
	if rec.Code != http.StatusOK {
		t.Fatalf("publish: %d %s", rec.Code, rec.Body.String())
	}
	var status apitypes.SemanticCoverageStatus
	rec = call(http.MethodGet, "/coverage?claim_generation=7", nil)
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &status) != nil || status.Outcome != "incomplete" || status.Report == nil || status.Report.Criteria[0].Atomic || status.ChargedTokens != 15 {
		t.Fatalf("status: %d %s", rec.Code, rec.Body.String())
	}
	// Seed a checker attempt with semantic history identity. No model credential
	// exists: exact retries must rehydrate history before credential resolution.
	cliMustExec(t, pool, `UPDATE runs SET harness='claude',auto_approve=true,plan_cross_check_required=true WHERE id=$1`, run)
	child := uuid.New()
	cliMustExec(t, pool, `INSERT INTO runs(id,user_id,repo_id,kind,target_run_id,harness,report_only,budget_wall_seconds,issue_title,issue_description)
 VALUES($1,$2,$3,'cross_check',$4,'codex',true,1800,'checker','candidate')`, child, owner, repo, run)
	digest, err := hex.DecodeString(compiled.Candidate.Digest)
	if err != nil {
		t.Fatal(err)
	}
	cliMustExec(t, pool, `INSERT INTO cross_checks(lead_run_id,checker_run_id,stage,round,lead_claim_generation,plan_md,milestones,required_capabilities,required_tools,size_class,base_commit,planning_diff,candidate_digest,verdict,deadline_at)
 VALUES($1,$2,'plan',1,7,$3,$4,'{}','{}','s',$5,'diff',$6,'pending',now()+interval '30 minutes')`, run, child, structural.PlanMd, structural.Milestones, structural.BaseCommit, digest)
	checkerCall := func(method, suffix string, body any) *httptest.ResponseRecorder {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(method, "/api/worker/runs/"+run.String()+"/cross-checks"+suffix, strings.NewReader(string(raw)))
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	retryBody := map[string]any{"claim_generation": 7, "stage": "plan", "round": 1, "plan_md": structural.PlanMd, "milestones": structural.Milestones, "required_capabilities": []string{}, "required_tools": []string{}, "size_class": "s", "base_commit": structural.BaseCommit, "planning_diff": "diff"}
	var checker planCrossCheckCandidateResponse
	rec = checkerCall(http.MethodPost, "", retryBody)
	if rec.Header().Get("X-Uzi-Candidate-Identity-Version") != "semantic_v1" || rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &checker) != nil || checker.Candidate.CandidateIdentityVersion != "semantic_v1" || checker.Candidate.SemanticCandidate == nil || checker.Candidate.SemanticCandidate.Digest != compiled.Candidate.Digest {
		t.Fatalf("semantic exact retry: %d %s", rec.Code, rec.Body.String())
	}
	if !reflect.DeepEqual(checker.Candidate.SemanticCandidate, &compiled.Candidate) {
		t.Fatal("submit changed the server candidate")
	}
	rec = checkerCall(http.MethodGet, "/plan/1?claim_generation=7", nil)
	if rec.Code != http.StatusOK || rec.Header().Get("X-Uzi-Candidate-Identity-Version") != "semantic_v1" ||
		json.Unmarshal(rec.Body.Bytes(), &checker) != nil || !reflect.DeepEqual(checker.Candidate.SemanticCandidate, &compiled.Candidate) {
		t.Fatalf("semantic status: %d %s", rec.Code, rec.Body.String())
	}
	// Exercise the production checker claim writer with the service-rehydrated
	// frozen candidate. Claim admission/credential minting is outside this seam.
	payload := &workersvc.ClaimPayload{Kind: "cross_check", RunID: child.String(), CrossCheck: &workersvc.ClaimPlanCrossCheck{
		Stage: "plan", Round: 1, LeadRunID: run.String(), CandidateDigest: checker.CandidateDigest,
		DeadlineAt: checker.DeadlineAt, PlanCrossCheckCandidate: checker.Candidate,
	}}
	rawClaim, err := workersvc.MarshalCrossCheckClaim(payload)
	if err != nil {
		t.Fatal(err)
	}
	claimRec := httptest.NewRecorder()
	writeWorkerRunClaim(claimRec, payload)
	if claimRec.Code != http.StatusOK || claimRec.Header().Get("X-Uzi-Claim-Kind") != "cross_check" ||
		claimRec.Header().Get("X-Uzi-Candidate-Identity-Version") != "semantic_v1" || !bytes.Equal(claimRec.Body.Bytes(), rawClaim) {
		t.Fatal("semantic checker claim marker or bytes changed")
	}
	for _, corrupt := range []bool{false, true} {
		c := compiled.Candidate
		c.Criteria = append([]apitypes.SemanticCriterion(nil), c.Criteria...)
		payload.CrossCheck.SemanticCandidate = nil
		if corrupt {
			c.Criteria[0].DoneWhen += "corrupt"
			payload.CrossCheck.SemanticCandidate = &c
		}
		refused := httptest.NewRecorder()
		writeWorkerRunClaim(refused, payload)
		if refused.Code != http.StatusInternalServerError || refused.Header().Get("X-Uzi-Candidate-Identity-Version") != "" {
			t.Fatal("invalid semantic checker emitted marker")
		}
	}
	proposal.Criteria[0].Text = "revised behavior"
	body["proposal"] = proposal
	body["sources"] = []apitypes.SemanticSourceCapture{{ID: "issue", Kind: "issue", Provenance: "worker:capture", Completeness: "complete", Bytes: []byte(issue)}}
	if rec := call(http.MethodPost, "/candidate", body); rec.Code != http.StatusOK {
		t.Fatalf("revision: %d %s", rec.Code, rec.Body.String())
	}
	if rec := checkerCall(http.MethodPost, "", retryBody); rec.Code != http.StatusConflict {
		t.Fatal("old checker authorized a replacement candidate")
	}
	rec = checkerCall(http.MethodGet, "/plan/1?claim_generation=7", nil)
	if rec.Header().Get("X-Uzi-Candidate-Identity-Version") != "semantic_v1" || rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &checker) != nil || checker.Candidate.SemanticCandidate == nil || checker.Candidate.SemanticCandidate.Digest != compiled.Candidate.Digest {
		t.Fatalf("historical checker lost immutable payload: %d %s", rec.Code, rec.Body.String())
	}
	// Corrupt and missing immutable history must fail before either response
	// writer can emit a semantic marker. The mutable current candidate is newer.
	cliMustExec(t, pool, `UPDATE semantic_candidate_history SET candidate=jsonb_set(candidate,'{criteria}', '[]') WHERE run_id=$1 AND revision=1`, run)
	rec = checkerCall(http.MethodGet, "/plan/1?claim_generation=7", nil)
	if rec.Code == http.StatusOK || rec.Header().Get("X-Uzi-Candidate-Identity-Version") != "" {
		t.Fatal("corrupt history emitted semantic status marker")
	}
	cliMustExec(t, pool, `DELETE FROM semantic_candidate_history WHERE run_id=$1 AND revision=1`, run)
	rec = checkerCall(http.MethodGet, "/plan/1?claim_generation=7", nil)
	if rec.Code == http.StatusOK || rec.Header().Get("X-Uzi-Candidate-Identity-Version") != "" {
		t.Fatal("missing history emitted semantic status marker")
	}
	for _, suffix := range []string{"/coverage?claim_generation=6", "/coverage?claim_generation=8"} {
		if rec := call(http.MethodGet, suffix, nil); rec.Code != http.StatusConflict {
			t.Fatalf("stale generation %d", rec.Code)
		}
	}
}
