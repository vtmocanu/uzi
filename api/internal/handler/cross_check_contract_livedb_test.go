package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func TestCrossCheckAuthoritativeRoutesLiveDB(t *testing.T) {
	h, router, pool := cliLiveDB(t)
	h.wsvc.SetTxBeginner(pool)
	owner := cliSeedUser(t, pool, false)
	ctx := context.Background()
	conn := rmSeedConn(t, pool, owner)
	repo := rmSeedRepo(t, pool, conn, 992, true)
	lead := rmSeedRun(t, pool, owner, repo, "running")
	worker := uuid.New()
	token := "contract-worker-" + uuid.NewString()
	hash := sha256.Sum256([]byte(token))
	caps := []string{"cross_check_v1", "codex_harness_v1", "codex_runtime_v2", "completion_interlock_v1", "codex_completion_interlock_v1"}
	cliMustExec(t, pool, `INSERT INTO workers(id,user_id,name,token_hash,status,protocol_capabilities) VALUES($1,$2,'contract',$3,'online',$4)`, worker, owner, hash[:], caps)
	sealed := mustSealT(ctx, t, h.box, []byte("contract-fixture-"+uuid.NewString()))
	secret := uuid.New()
	cliMustExec(t, pool, `UPDATE forge_connections SET token_ciphertext=$2 WHERE id=$1`, conn, sealed)
	cliMustExec(t, pool, `INSERT INTO user_secrets(id,user_id,kind,label,is_default,ciphertext,sealed_with) VALUES($1,$2,'openai_api_key','contract',true,$3,'master')`, secret, owner, sealed)
	if _, err := h.q.InsertCodexCredentialState(ctx, store.InsertCodexCredentialStateParams{UserSecretID: secret, UserID: owner, Status: "static"}); err != nil {
		t.Fatal(err)
	}
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
	body, err := json.Marshal(planCrossCheckRequest{Stage: "plan", ClaimGeneration: func() *int64 { v := int64(1); return &v }(), PlanCrossCheckCandidate: candidate})
	if err != nil {
		t.Fatal(err)
	}
	created := call(http.MethodPost, path, string(body))
	var got planCrossCheckCandidateResponse
	if created.Code != http.StatusOK || json.Unmarshal(created.Body.Bytes(), &got) != nil || got.CheckerRunID == nil || got.Verdict != "pending" {
		t.Fatalf("new Submit: %d %s", created.Code, created.Body.String())
	}
	child, err := uuid.Parse(*got.CheckerRunID)
	if err != nil {
		t.Fatal(err)
	}
	var rowID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM cross_checks WHERE lead_run_id=$1 AND stage='plan' AND round=1`, lead).Scan(&rowID); err != nil {
		t.Fatal(err)
	}
	if got.CrossCheckID != rowID.String() || got.CrossCheckID == child.String() {
		t.Fatalf("Submit row=%s child=%s, stored row=%s", got.CrossCheckID, child, rowID)
	}
	poll := call(http.MethodGet, path+"/plan/1?claim_generation=1", "")
	got = planCrossCheckCandidateResponse{}
	if poll.Code != http.StatusOK || json.Unmarshal(poll.Body.Bytes(), &got) != nil || got.CrossCheckID != rowID.String() || got.CheckerRunID == nil || *got.CheckerRunID != child.String() {
		t.Fatalf("pending poll identity: %d %s", poll.Code, poll.Body.String())
	}
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
	// Credential removal must not prevent immutable lost-ACK recovery.
	cliMustExec(t, pool, `DELETE FROM user_secrets WHERE id=$1`, secret)
	retry := call(http.MethodPost, path, string(body))
	got = planCrossCheckCandidateResponse{}
	if retry.Code != 200 || json.Unmarshal(retry.Body.Bytes(), &got) != nil || got.Verdict != "approve" || got.CandidateDigest != hex.EncodeToString(digest) || got.Candidate.PlanMd != "plan" || got.CandidateGeneration != 1 || got.CrossCheckID != rowID.String() || got.CheckerRunID == nil || *got.CheckerRunID != child.String() {
		t.Fatalf("decided same-generation reuse: %d %s", retry.Code, retry.Body.String())
	}
	// Simulate pre-upgrade unknown coverage and loss of the child provenance.
	// Replay must neither restore selection/coverage nor rewrite the stored row.
	cliMustExec(t, pool, `UPDATE runs SET policy_candidate_cross_check_id=NULL,policy_revision_source=NULL WHERE id=$1`, lead)
	cliMustExec(t, pool, `DELETE FROM runs WHERE id=$1`, child)
	snapshot := func() string {
		t.Helper()
		var raw string
		if err := pool.QueryRow(ctx, `SELECT jsonb_build_object('check',to_jsonb(cc),'run',to_jsonb(r))::text
 FROM cross_checks cc JOIN runs r ON r.id=cc.lead_run_id WHERE cc.id=$1`, rowID).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		return raw
	}
	before := snapshot()
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		endpoint, payload := path+"/plan/1?claim_generation=1", ""
		if method == http.MethodPost {
			endpoint, payload = path, string(body)
		}
		rec := call(method, endpoint, payload)
		got = planCrossCheckCandidateResponse{}
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &got) != nil || got.CrossCheckID != rowID.String() || got.CheckerRunID != nil || got.Verdict != "approve" || got.CandidateDigest != hex.EncodeToString(digest) {
			t.Fatalf("child-independent %s recovery: %d %s", method, rec.Code, rec.Body.String())
		}
		if after := snapshot(); after != before {
			t.Fatalf("%s recovery mutated stored evidence:\nbefore %s\nafter %s", method, before, after)
		}
	}
	cliMustExec(t, pool, `UPDATE runs SET claim_generation=2,last_seq=7 WHERE id=$1`, lead)
	stale := call(http.MethodGet, path+"/plan/1?claim_generation=1", "")
	if stale.Code != 409 {
		t.Fatalf("stale caller: %d %s", stale.Code, stale.Body.String())
	}
	historical := call(http.MethodGet, path+"/plan/1?claim_generation=2", "")
	got = planCrossCheckCandidateResponse{}
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
