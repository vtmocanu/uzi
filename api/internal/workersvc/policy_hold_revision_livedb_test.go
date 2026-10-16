package workersvc

import (
	"testing"
)

func TestPolicyHoldRequiresCurrentRevisionEvidenceLiveDB(t *testing.T) {
	e := setupPolicyHold(t)
	if _, err := e.pool.Exec(e.ctx, "UPDATE runs SET status='awaiting_approval',plan_md='Original plan',gate_revision=7 WHERE id=$1", e.runID); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.AppendMessagesForClaim(e.ctx, e.wkr, e.runID, []IncomingMessage{{Seq: 1, Kind: "plan_revising", Payload: []byte("{}")}}, i64p(1)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(e.ctx, "UPDATE runs SET claim_generation=2 WHERE id=$1", e.runID); err != nil {
		t.Fatal(err)
	}
	req := policyRequest()
	req.ClaimGeneration, req.SourceGeneration, req.ExpectedGateRevision = 2, 2, i64p(7)
	r, d, err := e.svc.ReportPolicyHold(e.ctx, e.wkr, e.runID, req)
	if err != nil || d != PolicyHoldPending || r.Status != "awaiting_approval" || r.PolicyRefusalCount != 0 {
		t.Fatalf("old revision evidence accepted: status=%s count=%d disposition=%s err=%v", r.Status, r.PolicyRefusalCount, d, err)
	}
	if err := e.svc.AppendMessagesForClaim(e.ctx, e.wkr, e.runID, []IncomingMessage{{Seq: 2, Kind: "plan_revising", Payload: []byte("{}")}}, i64p(2)); err != nil {
		t.Fatal(err)
	}
	r, d, err = e.svc.ReportPolicyHold(e.ctx, e.wkr, e.runID, req)
	if err != nil || d != PolicyHoldApplied || r.Status != "paused" || r.PolicyRefusalCount != 1 {
		t.Fatalf("current revision evidence refused: status=%s count=%d disposition=%s err=%v", r.Status, r.PolicyRefusalCount, d, err)
	}
}
