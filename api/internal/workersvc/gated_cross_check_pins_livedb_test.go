package workersvc

import (
	"encoding/hex"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// gatedApproveReport records the checker's APPROVE for the lead's first round and returns the
// awaiting_approval report that presents exactly that candidate, bound by its digest. The
// optional mutate hook edits the report (for example to omit a field).
func gatedApproveReport(t *testing.T, f gatedFixture, mutate func(*StateRequest)) StateRequest {
	t.Helper()
	cc, err := f.svc.SubmitPlanCrossCheck(f.env.ctx, f.w, f.lead, 1, f.c, 1)
	if err != nil {
		t.Fatal(err)
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
	if mutate != nil {
		mutate(&req)
	}
	return req
}

// PRD #3708 plan section 5: setStateWithTx writes SetRunAwaitingApproval BEFORE it publishes the
// gate presentation, in one transaction. SetRunAwaitingApproval's gated arm (a) keeps the stored
// reason once gate_revision > 0, so a publication that ran first would make a first
// presentation with an unbound approve read as "passed". The row below has the report's
// candidate already stored at revision 0, the only shape where the order is observable: reason
// must be derived (interrupted for the digest-less report), never retained as NULL.
func TestGatedAwaitingApprovalWritesReasonBeforePublishingLiveDB(t *testing.T) {
	f := newGatedFixture(t, 2)
	req := gatedApproveReport(t, f, func(r *StateRequest) { r.CandidateDigest = "" })
	// Store the report's own candidate, so the human-revision arm (a changed candidate) does not
	// apply and only the gate_revision > 0 retention arm could change the outcome.
	f.env.exec(`UPDATE runs SET plan_md=$2, milestones_candidate='[]', size_class='s' WHERE id=$1`, f.lead, *req.PlanMd)
	if _, applied, err := f.svc.SetState(f.env.ctx, f.w, f.lead, req); err != nil || !applied {
		t.Fatalf("gate report: applied=%v err=%v", applied, err)
	}
	run := mustRun(t, f.env, f.lead)
	if run.GateRevision != 1 {
		t.Fatalf("gate revision = %d, want the first publication (1)", run.GateRevision)
	}
	if !run.PlanCrossCheckGateReason.Valid || run.PlanCrossCheckGateReason.String != "interrupted" {
		t.Fatalf("a first presentation without a candidate digest must derive interrupted, got %+v", run.PlanCrossCheckGateReason)
	}
}

// A gated run whose requirement columns diverge from the checked candidate: the report's empty
// tool set is written by SetRunAwaitingApproval (the bound candidate), so the presented payload
// must carry the same set. Before it did, the payload kept the stale tools, the
// SetRunRunning payload-versus-columns guard refused a legitimately approved gate.
func TestGatedBoundApprovalWithDivergingRequiredToolsLiveDB(t *testing.T) {
	f := newGatedFixture(t, 2)
	req := gatedApproveReport(t, f, nil)
	f.env.exec(`UPDATE runs SET required_tools=ARRAY['go'] WHERE id=$1`, f.lead)
	if _, applied, err := f.svc.SetState(f.env.ctx, f.w, f.lead, req); err != nil || !applied {
		t.Fatalf("gate report: applied=%v err=%v", applied, err)
	}
	run := mustRun(t, f.env, f.lead)
	if run.Status != "awaiting_approval" || run.PlanCrossCheckGateReason.Valid || len(run.RequiredTools) != 0 {
		t.Fatalf("want the passed gate bound to the checked (empty) tools: status=%s reason=%+v tools=%v", run.Status, run.PlanCrossCheckGateReason, run.RequiredTools)
	}
	input, err := f.env.q.CreateApprovePlanInput(f.env.ctx, store.CreateApprovePlanInputParams{
		RunID: f.lead, Body: pgtype.Text{String: "{}", Valid: true}, AgentSource: pgtype.Text{String: "own", Valid: true},
		AgentExclusions: []byte("[]"), ExpectedGateRevision: pgtype.Int8{Int64: run.GateRevision, Valid: true},
		ContractSource: []byte("[]"),
	})
	if err != nil {
		t.Fatal(err)
	}
	f.env.exec(`UPDATE run_user_inputs SET applied_at = now(), disposition = 'applied' WHERE id = $1`, input.ID)
	gen := int64(1)
	got, applied, err := f.svc.SetState(f.env.ctx, f.w, f.lead, StateRequest{State: "running", ClaimGeneration: &gen})
	if err != nil || !applied || got.Status != "running" {
		t.Fatalf("a bound approval of the presented gate did not start implementing: status=%s applied=%v err=%v", got.Status, applied, err)
	}
}

// A verdict from a checker whose lead is no longer claimed or running at the round's generation
// is inert: refused, the round stays pending, and no event reaches the lead's log.
func TestGatedDecideIsInertWhenLeadLeftTheClaimLiveDB(t *testing.T) {
	for name, set := range map[string]string{
		"lead awaiting_approval": "status='awaiting_approval'",
		"lead claim released":    "claim_released_at=now()",
		"lead new generation":    "claim_generation=2",
	} {
		t.Run(name, func(t *testing.T) {
			f := newGatedFixture(t, 2)
			cc, err := f.svc.SubmitPlanCrossCheck(f.env.ctx, f.w, f.lead, 1, f.c, 1)
			if err != nil {
				t.Fatal(err)
			}
			child := uuid.UUID(cc.CheckerRunID.Bytes)
			f.env.exec("UPDATE runs SET worker_id=$2,status='running',claim_generation=1 WHERE id=$1", child, f.w.ID)
			f.env.exec("UPDATE runs SET "+set+" WHERE id=$1", f.lead)
			before := mustRun(t, f.env, f.lead)
			rowOf := func() (verdict, reason string, decided bool) {
				if err := f.env.pool.QueryRow(f.env.ctx, `SELECT verdict, COALESCE(reason_class, ''), decided_at IS NOT NULL FROM cross_checks WHERE id=$1`, cc.ID).Scan(&verdict, &reason, &decided); err != nil {
					t.Fatal(err)
				}
				return verdict, reason, decided
			}
			verdictBefore, reasonBefore, decidedBefore := rowOf()
			_, err = f.svc.DecidePlanCrossCheck(f.env.ctx, f.w, child, 1, "approve", "approve", []byte(`{"summary":"approve","items":[]}`))
			if !errors.Is(err, ErrCrossCheckRefused) {
				t.Fatalf("decide on a lead that left the claim: %v", err)
			}
			verdict, reason, decided := rowOf()
			if verdict == "approve" || verdict != verdictBefore || reason != reasonBefore || decided != decidedBefore {
				t.Fatalf("an inert decide changed the round: %s/%s/%v -> %s/%s/%v", verdictBefore, reasonBefore, decidedBefore, verdict, reason, decided)
			}
			var events int
			if err := f.env.pool.QueryRow(f.env.ctx, `SELECT count(*) FROM run_messages WHERE run_id=$1 AND kind='cross_check'`, f.lead).Scan(&events); err != nil || events != 0 {
				t.Fatalf("an inert decide appended %d cross_check events (err=%v)", events, err)
			}
			after := mustRun(t, f.env, f.lead)
			if after.Status != before.Status || after.GateRevision != before.GateRevision || after.PlanCrossCheckGateReason != before.PlanCrossCheckGateReason {
				t.Fatalf("an inert decide changed the lead: before=%+v after=%+v", before.Status, after.Status)
			}
		})
	}
}
