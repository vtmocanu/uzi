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
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func TestPlanCrossCheckAutomaticRoundsContractLiveDB(t *testing.T) {
	h, router, pool := cliLiveDB(t)
	ctx := context.Background()
	h.wsvc = workersvc.New(h.q, h.box, workersvc.Params{PlanCrossCheckMaxRevisions: 2})
	h.wsvc.SetTxBeginner(pool)
	owner := cliSeedUser(t, pool, false)
	conn := rmSeedConn(t, pool, owner)
	repo := rmSeedRepo(t, pool, conn, 21950, true)
	lead := rmSeedRun(t, pool, owner, repo, "running")
	token := "round-contract-" + uuid.NewString()
	hash := sha256.Sum256([]byte(token))
	worker := uuid.New()
	caps := []string{"cross_check_v1", "cross_check_rounds_v1", "codex_harness_v1", "codex_runtime_v2", "completion_interlock_v1", "codex_completion_interlock_v1"}
	cliMustExec(t, pool, `INSERT INTO workers(id,user_id,name,token_hash,status,protocol_capabilities,max_concurrent_runs) VALUES($1,$2,'rounds',$3,'online',$4,8)`, worker, owner, hash[:], caps)
	cliMustExec(t, pool, `UPDATE runs SET worker_id=$2,harness='claude',auto_approve=true,plan_cross_check_required=true,claim_generation=1 WHERE id=$1`, lead, worker)
	sealed := mustSealT(ctx, t, h.box, []byte("round-contract-fixture-"+uuid.NewString()))
	secret := uuid.New()
	cliMustExec(t, pool, `UPDATE forge_connections SET token_ciphertext=$2 WHERE id=$1`, conn, sealed)
	cliMustExec(t, pool, `INSERT INTO user_secrets(id,user_id,kind,label,is_default,ciphertext,sealed_with) VALUES($1,$2,'openai_api_key','rounds',true,$3,'master')`, secret, owner, sealed)
	if _, err := h.q.InsertCodexCredentialState(ctx, store.InsertCodexCredentialStateParams{UserSecretID: secret, UserID: owner, Status: "static"}); err != nil {
		t.Fatal(err)
	}
	call := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	base := "/api/worker/runs/" + lead.String() + "/cross-checks"
	candidate := workersvc.PlanCrossCheckCandidate{PlanMd: "  canonical plan  ", Milestones: json.RawMessage("[]"), RequiredCapabilities: []string{}, RequiredTools: []string{}, SizeClass: "s", BaseCommit: strings.Repeat("a", 40), PlanningDiff: "diff"}
	submit := func(round int32) planCrossCheckCandidateResponse {
		t.Helper()
		gen := int64(1)
		body, err := json.Marshal(planCrossCheckRequest{Stage: "plan", Round: &round, ClaimGeneration: &gen, PlanCrossCheckCandidate: candidate})
		if err != nil {
			t.Fatal(err)
		}
		rec := call(http.MethodPost, base, string(body))
		var got planCrossCheckCandidateResponse
		if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &got) != nil || got.Round != round || got.CheckerRunID == nil || !got.AutomaticRoundsEnabled || got.AutomaticRevisionLimit != 2 || got.CandidateGeneration != 1 {
			t.Fatalf("submit round %d: %d %s", round, rec.Code, rec.Body.String())
		}
		return got
	}
	decide := func(cc planCrossCheckCandidateResponse, verdict string) {
		t.Helper()
		rec := call(http.MethodPost, "/api/worker/runs/claim", "")
		var payload workersvc.ClaimPayload
		if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &payload) != nil || payload.CrossCheck == nil || payload.CrossCheck.Round != cc.Round || payload.CrossCheck.CandidateDigest != cc.CandidateDigest {
			var diagnostic string
			if err := pool.QueryRow(ctx, `SELECT jsonb_build_object(
                'child_status',child.status,'child_generation',child.claim_generation,'child_failure',child.failure_reason,
                'lead_status',parent.status,'lead_generation',parent.claim_generation,'lead_gate',parent.gate_revision,
                'round',check_row.round,'verdict',check_row.verdict,'enabled',check_row.automatic_rounds_enabled,
                'limit',check_row.automatic_revision_limit,'protocols',w.protocol_capabilities,
                'maintenance',w.maintenance_fenced,'draining',w.draining_since,'capacity',w.max_concurrent_runs)::text
                FROM runs child JOIN cross_checks check_row ON check_row.checker_run_id=child.id
                JOIN runs parent ON parent.id=check_row.lead_run_id JOIN workers w ON w.id=$2
                WHERE child.id=$1`, uuid.MustParse(*cc.CheckerRunID), worker).Scan(&diagnostic); err != nil {
				t.Logf("claim diagnostic read: %v", err)
			}
			t.Fatalf("claim round %d: %d %s; %s", cc.Round, rec.Code, rec.Body.String(), diagnostic)
		}
		child := uuid.MustParse(*cc.CheckerRunID)
		run, err := h.q.GetRunByID(ctx, child)
		if err != nil {
			t.Fatal(err)
		}
		if payload.RunID != child.String() {
			t.Fatalf("claimed %s, expected %s", payload.RunID, child)
		}
		items := "[]"
		if verdict == "revise" {
			items = `[{"file":"plan","severity":"warning","summary":"clarify validation","rationale":"make the checks executable"}]`
		}
		rec = call(http.MethodPost, "/api/worker/runs/"+child.String()+"/cross-check-verdict", fmt.Sprintf(`{"claim_generation":%d,"verdict":%q,"reason_class":%q,"summary":"checked","items":%s}`, run.ClaimGeneration, verdict, verdict, items))
		if rec.Code != 200 {
			t.Fatalf("verdict: %d %s", rec.Code, rec.Body.String())
		}
		// Checkers release ordinary slots through their terminal state report.
		rec = call(http.MethodPost, "/api/worker/runs/"+child.String()+"/state", fmt.Sprintf(`{"status":"completed","claim_generation":%d,"report_only":false}`, run.ClaimGeneration))
		if rec.Code != http.StatusOK {
			t.Fatalf("checker completion: %d %s", rec.Code, rec.Body.String())
		}
		finished, err := h.q.GetRunByID(ctx, child)
		if err != nil || finished.Status != "completed" || !finished.ReportOnly {
			t.Fatalf("checker kind invariant on completion: status=%s report_only=%v err=%v", finished.Status, finished.ReportOnly, err)
		}
	}
	first := submit(1)
	decide(first, "revise")
	retry := submit(1)
	if *retry.CheckerRunID != *first.CheckerRunID || retry.Verdict != "revise" {
		t.Fatal("decided retry created another child")
	}
	rec := call(http.MethodGet, base+"/plan/latest?claim_generation=1", "")
	var metadata workersvc.PlanCrossCheckLatestMetadata
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &metadata) != nil || !metadata.NextRoundEligible || metadata.NextRound == nil || *metadata.NextRound != 2 {
		t.Fatalf("latest: %d %s", rec.Code, rec.Body.String())
	}
	second := submit(2)
	if *second.CheckerRunID == *first.CheckerRunID || second.CandidateDigest != first.CandidateDigest {
		t.Fatal("explicit identical next candidate did not create a distinct round")
	}
	decide(second, "approve")
	rec = call(http.MethodGet, base+"/plan/2?claim_generation=1", "")
	var approved planCrossCheckCandidateResponse
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &approved) != nil || approved.Verdict != "approve" || approved.Candidate.PlanMd != "canonical plan" {
		t.Fatalf("exact approval: %d %s", rec.Code, rec.Body.String())
	}
	digest, err := hex.DecodeString(approved.CandidateDigest)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := h.q.SetRunAutopilotPlan(ctx, store.SetRunAutopilotPlanParams{ID: lead, WorkerID: pgconv.UUID(worker), PlanMd: pgtype.Text{String: approved.Candidate.PlanMd, Valid: true}, CandidateDigest: digest, MilestonesFrozen: approved.Candidate.Milestones, InferredCapabilities: approved.Candidate.RequiredCapabilities, InferredTools: approved.Candidate.RequiredTools, SizeClass: pgtype.Text{String: approved.Candidate.SizeClass, Valid: true}})
	if err != nil || rows != 1 {
		t.Fatalf("canonical guarded plan write: rows=%d err=%v", rows, err)
	}
	var children, reviseCount int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM runs WHERE target_run_id=$1 AND kind='cross_check'),revise_count FROM runs WHERE id=$1`, lead).Scan(&children, &reviseCount); err != nil {
		t.Fatal(err)
	}
	if children != 2 || reviseCount != 0 {
		t.Fatalf("children=%d human revisions=%d", children, reviseCount)
	}
}
