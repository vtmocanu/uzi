package workersvc

import (
	"encoding/hex"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestGatedCreateRunSubmitGateSeamLiveDB is the PRD #3708 seam proof: a run enrolled by the
// REAL CreateRun insert (owner consent on, not auto-approved, agent-written plan) takes the
// whole path CreateRun -> submit -> APPROVE -> awaiting_approval, with no hand-set gated flag.
// The human gate opens with a passed (NULL) reason, and approval authority stays with the human:
// running is refused until a bound approval is applied.
func TestGatedCreateRunSubmitGateSeamLiveDB(t *testing.T) {
	f := newGatedFixture(t, 2)
	var userID, repoID uuid.UUID
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT user_id, repo_id FROM runs WHERE id=$1`, f.lead).Scan(&userID, &repoID); err != nil {
		t.Fatal(err)
	}
	// Replace the hand-built lead with one written by CreateRun itself.
	f.env.exec(`DELETE FROM runs WHERE id=$1`, f.lead)
	f.env.exec(`UPDATE users SET plan_cross_check_enabled=true WHERE id=$1`, userID)
	created, err := f.env.q.CreateRun(f.env.ctx, store.CreateRunParams{
		UserID: userID, RepoID: repoID, IssueIid: pgtype.Int8{Int64: 3708, Valid: true},
		IssueTitle: "seam", IssueDescription: "body", AutoApprove: false, PlanSource: "agent",
		TriggerSource: "manual", Harness: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if !created.PlanCrossCheckGated || created.PlanCrossCheckRequired {
		t.Fatalf("CreateRun snapshot gated=%v required=%v, want gated only", created.PlanCrossCheckGated, created.PlanCrossCheckRequired)
	}
	f.lead = created.ID
	f.env.exec(`UPDATE runs SET status='running', worker_id=$2, claim_generation=1 WHERE id=$1`, f.lead, f.w.ID)

	cc, err := f.svc.SubmitPlanCrossCheck(f.env.ctx, f.w, f.lead, 1, f.c, 1)
	if err != nil {
		t.Fatalf("submit on a CreateRun-enrolled gated run: %v", err)
	}
	f.decide(t, cc, "approve")
	stored, err := f.env.q.GetExactPlanCrossCheck(f.env.ctx, store.GetExactPlanCrossCheckParams{LeadRunID: f.lead, Round: 1})
	if err != nil {
		t.Fatal(err)
	}
	plan, gen, size := stored.PlanMd.String, int64(1), "s"
	caps, tools, ms := []string{}, []string{}, []Milestone{}
	pid := uuid.New()
	req := StateRequest{State: "awaiting_approval", ClaimGeneration: &gen, PlanMd: &plan, Milestones: &ms,
		RequiredCapabilities: &caps, RequiredTools: &tools, SizeClass: &size, PresentationID: &pid,
		CandidateDigest: hex.EncodeToString(stored.CandidateDigest)}
	if _, applied, err := f.svc.SetState(f.env.ctx, f.w, f.lead, req); err != nil || !applied {
		t.Fatalf("gate report: applied=%v err=%v", applied, err)
	}
	run := mustRun(t, f.env, f.lead)
	if run.Status != "awaiting_approval" || run.AutoApprove || run.PlanCrossCheckGateReason.Valid {
		t.Fatalf("want the human gate open with a passed check: status=%s auto=%v reason=%+v", run.Status, run.AutoApprove, run.PlanCrossCheckGateReason)
	}
	if _, applied, err := f.svc.SetState(f.env.ctx, f.w, f.lead, StateRequest{State: "running", ClaimGeneration: &gen}); err != nil || applied {
		t.Fatalf("running without a bound approval must be refused: applied=%v err=%v", applied, err)
	}
}

// TestGatedApprovalBindsOnlyToAPresentedGateLiveDB pins CreateApprovePlanInput's binding rule
// for a gated run (PRD #3708 section 5): an approve_plan is 'bound' to a gate revision only
// when the run is awaiting_approval at gate_revision > 0, so the bound-approval backstop in
// SetRunRunning and the trigger never accept an approval recorded before the gate was published.
func TestGatedApprovalBindsOnlyToAPresentedGateLiveDB(t *testing.T) {
	f := newGateFix(t, 3)
	run := f.newRun("issue")
	f.exec(`UPDATE runs SET plan_cross_check_gated=true WHERE id=$1`, run)
	f.mustPublish(run, gateReq(i64(1), uid()), 1)
	if _, err := f.submit(run, "approve_plan", nil, SubmitInputOptions{ExpectedGateRevision: i64(1)}); err != nil {
		t.Fatal(err)
	}
	var binding *string
	var revision *int64
	if err := f.pool.QueryRow(f.ctx, `SELECT gate_binding, gate_revision FROM run_user_inputs WHERE run_id=$1 AND kind='approve_plan'`, run).Scan(&binding, &revision); err != nil {
		t.Fatal(err)
	}
	if binding == nil || *binding != "bound" || revision == nil || *revision != 1 {
		t.Fatalf("approval at a presented gate: binding=%v revision=%v, want bound at revision 1", binding, revision)
	}
}
