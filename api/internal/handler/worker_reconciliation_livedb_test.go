package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Exercise the authenticated routes against persisted authority, including recovery
// after the caller loses the successful state ACK.
func TestWorkerReconciliationRoutesLiveDB(t *testing.T) {
	for _, verdict := range []string{"pending", "block", "approve", "no_row", "ordinary"} {
		t.Run(verdict, func(t *testing.T) {
			h, router, pool := cliLiveDB(t)
			h.wsvc.SetTxBeginner(pool)
			owner := cliSeedUser(t, pool, false)
			repo := rmSeedRepo(t, pool, rmSeedConn(t, pool, owner), 994, true)
			lead := rmSeedRun(t, pool, owner, repo, "running")
			worker := uuid.New()
			token := "reconciliation-worker-" + uuid.NewString()
			tokenHash := sha256.Sum256([]byte(token))
			cliMustExec(t, pool, "INSERT INTO workers(id,user_id,name,token_hash,status) VALUES($1,$2,'reconciliation',$3,'online')", worker, owner, tokenHash[:])
			cliMustExec(t, pool, "UPDATE runs SET worker_id=$2,harness='claude',auto_approve=true,plan_source='agent',plan_cross_check_required=true,claim_generation=1 WHERE id=$1", lead, worker)
			if verdict == "no_row" {
				cliMustExec(t, pool, "UPDATE runs SET harness='codex' WHERE id=$1", lead)
			}
			if verdict == "ordinary" {
				cliMustExec(t, pool, "UPDATE runs SET plan_cross_check_required=false,auto_approve=false WHERE id=$1", lead)
			}
			call := func(method, path, body string) *httptest.ResponseRecorder {
				t.Helper()
				req := httptest.NewRequest(method, path, strings.NewReader(body))
				req.Header.Set("Authorization", "Bearer "+token)
				req.Header.Set("Content-Type", "application/json")
				rec := httptest.NewRecorder()
				router.ServeHTTP(rec, req)
				return rec
			}
			base := "/api/worker/runs/" + lead.String()
			statusPath := base + "/cross-checks/plan/1?claim_generation=1"
			decode := func(rec *httptest.ResponseRecorder, code int) map[string]json.RawMessage {
				t.Helper()
				var body map[string]json.RawMessage
				if rec.Code != code || json.Unmarshal(rec.Body.Bytes(), &body) != nil {
					t.Fatalf("route: %d %s", rec.Code, rec.Body.String())
				}
				return body
			}
			assertNoProof := func(rec *httptest.ResponseRecorder, code int) {
				t.Helper()
				body := decode(rec, code)
				for _, key := range []string{"claim_generation", "plan_cross_check_settled", "gate_presentation_id", "current_plan_sha256", "gate_payload_digest"} {
					if _, exists := body[key]; exists {
						t.Fatalf("refusal/ordinary response exposed %s: %s", key, rec.Body.String())
					}
				}
			}
			plan := strings.Repeat("plan text ", 10000) + "end" // 100 KiB, within the plan bound.
			candidate := workersvc.PlanCrossCheckCandidate{PlanMd: plan, Milestones: json.RawMessage("[]"), RequiredCapabilities: []string{}, RequiredTools: []string{}, SizeClass: "s", BaseCommit: strings.Repeat("a", 40), PlanningDiff: strings.Repeat("private diff ", 30000) + "end"}
			wantSeq := int32(0)
			if verdict != "ordinary" {
				absent := call(http.MethodGet, statusPath, "")
				body := decode(absent, 200)
				if string(body["result"]) != `"no_row"` || string(body["reason_class"]) != `"no_candidate"` || string(body["plan_cross_check_settled"]) != "true" {
					t.Fatalf("no-row compatibility/proof: %s", absent.Body.String())
				}
			}
			if verdict != "no_row" && verdict != "ordinary" {
				child := uuid.New()
				digest, err := candidate.Digest()
				if err != nil {
					t.Fatal(err)
				}
				cliMustExec(t, pool, "INSERT INTO runs(id,user_id,repo_id,kind,target_run_id,harness,report_only,budget_wall_seconds,issue_title,issue_description,status,worker_id,claim_generation) VALUES($1,$2,$3,'cross_check',$4,'codex',true,1800,'checker','candidate','running',$5,1)", child, owner, repo, lead, worker)
				cliMustExec(t, pool, "INSERT INTO cross_checks(lead_run_id,checker_run_id,stage,round,lead_claim_generation,plan_md,milestones,required_capabilities,required_tools,size_class,base_commit,planning_diff,candidate_digest,verdict,deadline_at) VALUES($1,$2,'plan',1,1,$3,'[]','{}','{}','s',$4,$5,$6,'pending',now()+interval '30 minutes')", lead, child, plan, candidate.BaseCommit, candidate.PlanningDiff, digest)
				active := call(http.MethodGet, statusPath, "")
				var activeBody planCrossCheckCandidateResponse
				if active.Code != 200 || json.Unmarshal(active.Body.Bytes(), &activeBody) != nil || activeBody.Candidate.PlanMd != plan || activeBody.Candidate.PlanningDiff != candidate.PlanningDiff || activeBody.CandidateDigest != hex.EncodeToString(digest) || activeBody.PlanCrossCheckSettled {
					t.Fatalf("active canonical candidate: %d %s", active.Code, active.Body.String())
				}
				if verdict != "pending" {
					decision := fmt.Sprintf("{\"claim_generation\":1,\"verdict\":%q,\"reason_class\":%q,\"summary\":\"ok\",\"items\":[]}", verdict, verdict)
					decode(call(http.MethodPost, "/api/worker/runs/"+child.String()+"/cross-check-verdict", decision), 200)
					wantSeq = 1
					submit, err := json.Marshal(planCrossCheckRequest{Stage: "plan", ClaimGeneration: func() *int64 { v := int64(1); return &v }(), PlanCrossCheckCandidate: candidate})
					if err != nil {
						t.Fatal(err)
					}
					followup := decode(call(http.MethodPost, base+"/cross-checks", string(submit)), 200)
					if string(followup["lead_last_seq"]) != "1" || string(followup["claim_generation"]) != "1" {
						t.Fatalf("submit follow-up omitted post-event proof: %s", followup)
					}
				}
			}
			pid := uuid.New()
			reason := "interrupted"
			if verdict == "no_row" {
				reason = "codex_lead_unsupported"
			}
			req := map[string]any{"status": "awaiting_approval", "claim_generation": 1, "plan_md": plan, "milestones": []any{}, "required_capabilities": []string{}, "required_tools": []string{}, "size_class": "s", "presentation_id": pid}
			if verdict != "ordinary" {
				req["plan_cross_check_gate_reason"] = reason
			}
			raw, err := json.Marshal(req)
			if err != nil {
				t.Fatal(err)
			}
			state := call(http.MethodPost, base+"/state", string(raw))
			if verdict == "ordinary" {
				assertNoProof(state, 200)
				if string(decode(state, 200)["gate_revision"]) != "1" {
					t.Fatal("ordinary gate revision missing")
				}
				cliMustExec(t, pool, "UPDATE runs SET status='cancelled' WHERE id=$1", lead)
				assertNoProof(call(http.MethodPost, base+"/state", string(raw)), 409)
				return
			}
			assertProof := func(rec *httptest.ResponseRecorder, parked bool) {
				t.Helper()
				body := decode(rec, 200)
				var proof workersvc.LeadReconciliation
				if err := json.Unmarshal(rec.Body.Bytes(), &proof); err != nil {
					t.Fatal(err)
				}
				run, err := h.q.GetRunByID(context.Background(), lead)
				if err != nil {
					t.Fatal(err)
				}
				sum := sha256.Sum256([]byte(run.PlanMd.String))
				if proof.LeadLastSeq != wantSeq || proof.LeadLastSeq != run.LastSeq || proof.ClaimGeneration != run.ClaimGeneration || !proof.PlanCrossCheckSettled || proof.GateRevision != run.GateRevision || proof.GateRevision != 1 || proof.GatePresentationID == nil || *proof.GatePresentationID != pid || *proof.GatePresentationID != uuid.UUID(run.GatePresentationID.Bytes) || proof.GatePayloadDigest != hex.EncodeToString(run.GatePayloadDigest) || proof.GatePayloadDigest == "" || proof.CurrentPlanSHA256 != hex.EncodeToString(sum[:]) {
					t.Fatalf("persisted authority mismatch: %+v", proof)
				}
				// Decode each top-level value; the nested RunDTO has its own revision.
				decoder := json.NewDecoder(strings.NewReader(rec.Body.String()))
				if _, err := decoder.Token(); err != nil {
					t.Fatal(err)
				}
				counts := map[string]int{}
				for decoder.More() {
					key, err := decoder.Token()
					if err != nil {
						t.Fatal(err)
					}
					counts[key.(string)]++
					var value json.RawMessage
					if err := decoder.Decode(&value); err != nil {
						t.Fatal(err)
					}
				}
				for _, key := range []string{"lead_last_seq", "gate_revision"} {
					if counts[key] != 1 {
						t.Fatalf("duplicate/shadowed top-level %s", key)
					}
				}
				if parked {
					if string(body["result"]) != "\"parked\"" || len(rec.Body.Bytes()) > 700 {
						t.Fatalf("unbounded parked response: %s", rec.Body.String())
					}
					for _, key := range []string{"candidate", "candidate_digest", "planning_diff", "findings", "run", "deadline_at"} {
						if _, exists := body[key]; exists {
							t.Fatalf("parked exposed %s", key)
						}
					}
					if verdict == "pending" && (string(body["verdict"]) != "\"failed\"" || string(body["reason_class"]) != "\"superseded\"") {
						t.Fatalf("parked decision: %s", rec.Body.String())
					}
				}
			}
			assertProof(state, false)
			assertProof(call(http.MethodGet, statusPath, ""), true)
			assertProof(call(http.MethodPost, base+"/state", string(raw)), false)
			assertNoProof(call(http.MethodGet, base+"/cross-checks/plan/1?claim_generation=2", ""), 409)
			staleReq := strings.Replace(string(raw), "\"claim_generation\":1", "\"claim_generation\":2", 1)
			assertNoProof(call(http.MethodPost, base+"/state", staleReq), 409)
			missingReq := map[string]any{}
			for key, value := range req {
				if key != "claim_generation" {
					missingReq[key] = value
				}
			}
			missingRaw, err := json.Marshal(missingReq)
			if err != nil {
				t.Fatal(err)
			}
			assertNoProof(call(http.MethodPost, base+"/state", string(missingRaw)), 400)
			cliMustExec(t, pool, "UPDATE runs SET worker_id=NULL WHERE id=$1", lead)
			assertNoProof(call(http.MethodGet, statusPath, ""), 409)
			assertNoProof(call(http.MethodPost, base+"/state", string(raw)), 404)
			cliMustExec(t, pool, "UPDATE runs SET worker_id=$2,claim_released_at=now() WHERE id=$1", lead, worker)
			assertNoProof(call(http.MethodGet, statusPath, ""), 409)
			assertNoProof(call(http.MethodPost, base+"/state", string(raw)), 409)
		})
	}
}
