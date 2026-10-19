package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Lead runs this through the serial LiveDB wrapper, never ordinary focused checks.
func TestOwnerSemanticMaxRoundTripLiveDB(t *testing.T) {
	h, router, pool := cliLiveDB(t)
	q := store.New(pool)
	h.wsvc = workersvc.New(q, h.box, workersvc.Params{RunMaxIterations: 5, RunTimeout: 2 * time.Hour, RunWallCeiling: 24 * time.Hour})
	h.wsvc.SetTxBeginner(pool)
	ctx := t.Context()
	owner := cliSeedUser(t, pool, false)
	repo := rmSeedRepo(t, pool, rmSeedConn(t, pool, owner), 1995, true)
	id := rmSeedRun(t, pool, owner, repo, "running")
	worker := uuid.New()
	cliMustExec(t, pool, `INSERT INTO workers(id,user_id,name,token_hash,status) VALUES($1,$2,'semantic-owner-max',$3,'online')`, worker, owner, worker[:])
	body := semanticOwnerMaxRequest(t)
	cliMustExec(t, pool, `UPDATE runs SET worker_id=$2,claim_generation=1,completion_contract_version=1,issue_description=$3 WHERE id=$1`, id, worker, string(body.Sources[0].Bytes))
	cliMustExec(t, pool, `INSERT INTO semantic_planning(run_id,enrollment) VALUES($1,'{"profile":"semantic","policy":{"version":1,"model":"sonnet","max_reviews":3,"max_invocations":2,"max_tokens":120000,"max_seconds":900}}')`, id)
	candidate, err := h.wsvc.SubmitSemanticCandidate(ctx, worker, id, 1, body.Structural, body.Proposal, body.Sources)
	if err != nil {
		t.Fatal(err)
	}
	// Compare all proposal fields through the real compiler seam, accounting only
	// for the compiler's canonical criterion order.
	var proposal apitypes.SemanticProposal
	if err := json.Unmarshal(body.Proposal, &proposal); err != nil {
		t.Fatal(err)
	}
	expected := map[string]apitypes.ProposedSemanticCriterion{}
	for _, c := range proposal.Criteria {
		expected[c.Text] = c
	}
	counts := map[string]int{}
	if len(candidate.Criteria) != 256 {
		t.Fatal("lost canonical criteria")
	}
	for _, c := range candidate.Criteria {
		if !reflect.DeepEqual(c.ProposedSemanticCriterion, expected[c.Text]) {
			t.Fatal("changed criterion arrays or fields")
		}
		counts[c.MilestoneID]++
	}
	if len(counts) != 8 {
		t.Fatal("lost milestones")
	}
	for _, n := range counts {
		if n != 32 {
			t.Fatal("lost milestone criteria")
		}
	}
	var milestones []workersvc.Milestone
	if err := json.Unmarshal(body.Structural.Milestones, &milestones); err != nil {
		t.Fatal(err)
	}
	presentation := uuid.New()
	generation := int64(1)
	_, applied, _, err := h.wsvc.SetStateReport(ctx, store.Worker{ID: worker, UserID: owner}, id, workersvc.StateRequest{State: "awaiting_approval", ClaimGeneration: &generation, PresentationID: &presentation, CandidateDigest: candidate.Digest, PlanMd: &body.Structural.PlanMd, Milestones: &milestones, SizeClass: &body.Structural.SizeClass, RequiredCapabilities: &body.Structural.RequiredCapabilities, RequiredTools: &body.Structural.RequiredTools})
	if err != nil || !applied {
		t.Fatalf("present gate applied=%v err=%v", applied, err)
	}
	srv := httptest.NewServer(router)
	defer srv.Close()
	token := cliMintToken(t, pool, owner, clitoken.ScopeUser)
	client := uzicli.NewHTTPClient(uzicli.Settings{URL: srv.URL, Token: token})
	before, err := client.GetRun(ctx, id.String())
	if err != nil || before.SemanticGate == nil {
		t.Fatalf("owner GET: %v", err)
	}
	gate := before.SemanticGate
	if !reflect.DeepEqual(gate.Presentation.Candidate, candidate) ||
		!bytes.Equal(gate.Plan, []byte(body.Structural.PlanMd)) ||
		len(gate.Sources) != 1 || !bytes.Equal(gate.Sources[0].Bytes, body.Sources[0].Bytes) ||
		gate.Sources[0].ID != body.Sources[0].ID {
		t.Fatal("GET changed exact candidate or snapshots")
	}
	rec := bearerReq(router, http.MethodGet, "/api/runs/"+id.String(), token)
	if rec.Code != http.StatusOK || rec.Body.Len() <= 64<<20 || rec.Body.Len() > apitypes.OwnerRunMaxBytes ||
		rec.Header().Get(apitypes.SemanticIdentityHeader) != apitypes.SemanticIdentityVersion {
		t.Fatalf("max GET status=%d bytes=%d", rec.Code, rec.Body.Len())
	}
	input := apitypes.RunInputRequest{Kind: "approve_plan", SemanticApproval: &gate.Approval, ExpectedGateRevision: &before.GateRevision}
	raw, err := json.Marshal(input)
	if err != nil || len(raw) <= 1<<20 || len(raw) > apitypes.SemanticRunInputMaxBytes {
		t.Fatalf("max echo bytes=%d err=%v", len(raw), err)
	}
	firstReceipt, err := client.SubmitRunInputWithOptions(ctx, id.String(), input)
	if err != nil {
		t.Fatalf("full exact approval: %v", err)
	}
	replayedReceipt, err := client.SubmitRunInputWithOptions(ctx, id.String(), input)
	if err != nil || !reflect.DeepEqual(firstReceipt, replayedReceipt) {
		t.Fatalf("response-loss approval replay changed receipt: first=%+v replay=%+v err=%v", firstReceipt, replayedReceipt, err)
	}
	after, err := client.GetRun(ctx, id.String())
	if err != nil || !reflect.DeepEqual(after.SemanticGate, gate) {
		t.Fatalf("post-freeze owner GET changed presentation: %v", err)
	}
	run, err := q.GetRunByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	var contract struct {
		Criteria             []apitypes.SemanticCriterion       `json:"criteria"`
		SemanticApproval     *apitypes.SemanticGateApproval     `json:"semantic_approval"`
		SemanticPresentation *apitypes.SemanticGatePresentation `json:"semantic_presentation"`
	}
	if err := json.Unmarshal(run.CompletionContract, &contract); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(contract.Criteria, candidate.Criteria) || !reflect.DeepEqual(contract.SemanticApproval, &gate.Approval) ||
		!reflect.DeepEqual(contract.SemanticPresentation, &gate.Presentation) || len(run.MilestonesFrozen) == 0 {
		t.Fatal("freeze lost exact semantic contract")
	}
	admin := cliSeedUser(t, pool, true)
	adminClient := uzicli.NewHTTPClient(uzicli.Settings{URL: srv.URL, Token: cliMintToken(t, pool, admin, clitoken.ScopeAdminRO)})
	adminRun, err := adminClient.GetRun(ctx, id.String())
	if err != nil || adminRun.SemanticGate != nil {
		t.Fatalf("admin observer semantic disclosure: %v", err)
	}
	t.Logf("real owner GET=%d full POST=%d criteria=256 milestones=8", rec.Body.Len(), len(raw))
}
