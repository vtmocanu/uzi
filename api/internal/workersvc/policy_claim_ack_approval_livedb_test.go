package workersvc

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/capability"
)

// Public creation supplies source coverage. Claim and reports use the same
// authenticated worker identity through the service API, not HTTP authentication.
// Credentials, admission state and refusal count are fixtures; no accepted Stop,
// worker frame producer or zero-root execution is proved by this test.
func TestPolicyClaimAckHumanApprovalLiveDB(t *testing.T) {
	e := setupCodexLiveDB(t)
	f := newCodexClaimFix(t, e, false)
	w := f.claimant(t, true)
	enableRevisionProjection(f.svc, &w)
	if f.svc.p.PolicyStopNegotiated {
		t.Fatal("fixture must keep Stop off")
	}
	w.ProtocolCapabilities = append(w.ProtocolCapabilities, capability.GateRevisionV1, capability.InputReceiptsV1)
	e.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", w.ID, w.ProtocolCapabilities)
	e.exec("UPDATE runs SET status='cancelled' WHERE id=$1", f.runID)
	setDefaultHarness(t, e, f.userID, "codex")
	created, err := f.svc.CreateTaskRun(e.ctx, f.userID, f.repoID, "ACK approval", "", false, false, false, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.svc.FreezeCodexBinding(e.ctx, f.userID, created.ID, f.aliasID, codexAuthModeSubscription); err != nil {
		t.Fatal(err)
	}
	e.exec("UPDATE runs SET plan_cross_check_required=false,auto_approve=false,policy_refusal_count=1 WHERE id=$1", created.ID)
	t.Cleanup(func() { dropCustodyHolds(t, e, f.userID) })
	claim, err := f.svc.Claim(e.ctx, w, nil)
	if err != nil || claim == nil {
		t.Fatalf("Claim: %v", err)
	}
	if claim.RunID != created.ID.String() {
		t.Fatalf("claimed %s instead of %s", claim.RunID, created.ID)
	}
	current := mustRun(t, e, created.ID)
	if !current.WorkerID.Valid || current.WorkerID.Bytes != w.ID || claim.PlanApproved {
		t.Fatal("Claim did not hand off the unapproved candidate")
	}
	gen := current.ClaimGeneration
	req := gateReq(&gen, uid())
	req.RequiredCapabilities = &[]string{}
	candidate, err := f.svc.SetStateReportWithReconciliation(e.ctx, w, created.ID, req)
	if err != nil || !candidate.Applied || candidate.PlanApproved == nil || *candidate.PlanApproved {
		t.Fatalf("candidate: %+v %v", candidate, err)
	}
	approval, err := f.svc.SubmitInputWithOptions(e.ctx, f.userID, created.ID, "approve_plan", "", nil, SubmitInputOptions{ExpectedGateRevision: &candidate.GateRevision})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.AckInputs(e.ctx, w, created.ID, gen, []int64{approval.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.ApplyInputs(e.ctx, w, created.ID, gen, []int64{approval.ID}); err != nil {
		t.Fatal(err)
	}
	approved, err := f.svc.SetStateReportWithReconciliation(e.ctx, w, created.ID, StateRequest{State: "running", ClaimGeneration: &gen})
	if err != nil || !approved.Applied || approved.PlanApproved == nil || !*approved.PlanApproved {
		t.Fatalf("current human approval: %+v %v", approved, err)
	}
}

// A fatal transaction query error must roll back the actual gate/source writes,
// rather than degrade like the legacy claim plan-frame reads.
func TestPolicyAckApprovalClaimContextFailureLiveDB(t *testing.T) {
	e, f, run := revisionGateFixture(t)
	enableRevisionProjection(f.svc, &f.wkr)
	fault := &ackApprovalBeginner{TxBeginner: e.pool}
	f.svc.SetTxBeginner(fault)
	before := mustRun(t, e, run)
	result, err := f.svc.SetStateReportWithReconciliation(e.ctx, f.wkr, run, gateReq(i64(1), uid()))
	after := mustRun(t, e, run)
	if err == nil || fault.hits != 1 || result.Applied || result.PlanApproved != nil || result.PolicyRevisionGate != nil {
		t.Fatalf("failed read: hits=%d result=%+v error=%v", fault.hits, result, err)
	}
	if after.GateRevision != before.GateRevision || after.Status != before.Status || string(after.PolicyRevisionSource) != string(before.PolicyRevisionSource) {
		t.Fatal("failed context read committed state/source")
	}
}

// Actual autopilot plan/source writes happen inside the same report whose ACK
// approval is captured. No preapproved V1 source is planted by this fixture.
func TestPolicyAckApprovalAutopilotLiveDB(t *testing.T) {
	e, f, run, req := autopilotRevisionFixture(t)
	enableRevisionProjection(f.svc, &f.wkr)
	if f.svc.p.PolicyStopNegotiated {
		t.Fatal("fixture must keep Stop off")
	}
	e.exec("UPDATE runs SET policy_refusal_count=1 WHERE id=$1", run)
	result, err := f.svc.SetStateReportWithReconciliation(e.ctx, f.wkr, run, req)
	if err != nil || !result.Applied || result.PlanApproved == nil || !*result.PlanApproved || string(result.PolicyRevisionGate) != "null" {
		t.Fatalf("autopilot: %+v %v", result, err)
	}
}

func TestPolicyAckApprovalNegotiationControlsLiveDB(t *testing.T) {
	for _, mode := range []string{"off", "old_worker", "claude", "interactive"} {
		t.Run(mode, func(t *testing.T) {
			e, f, run := revisionGateFixture(t)
			enableRevisionProjection(f.svc, &f.wkr)
			switch mode {
			case "off":
				f.svc.p.PolicyHoldNegotiated = false
			case "old_worker":
				f.wkr.ProtocolCapabilities = []string{capability.GateRevisionV1}
			case "claude":
				e.exec("UPDATE runs SET harness='claude' WHERE id=$1", run)
			case "interactive":
				e.exec("UPDATE runs SET interactive=true WHERE id=$1", run)
			}
			result, err := f.svc.SetStateReportWithReconciliation(e.ctx, f.wkr, run, gateReq(i64(1), uid()))
			if err != nil || !result.Applied || result.PlanApproved != nil || result.PolicyRevisionGate != nil {
				t.Fatalf("mode=%s result=%+v err=%v", mode, result, err)
			}
		})
	}
}

type ackApprovalBeginner struct {
	TxBeginner
	hits int
}
type ackApprovalTx struct {
	pgx.Tx
	owner *ackApprovalBeginner
}

func (b *ackApprovalBeginner) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := b.TxBeginner.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &ackApprovalTx{Tx: tx, owner: b}, nil
}
func (tx *ackApprovalTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if strings.HasPrefix(sql, "-- name: GetRunClaimContext :one") {
		tx.owner.hits++
		return revisionWriterFailureRow{err: errors.New("injected ACK claim-context failure")}
	}
	return tx.Tx.QueryRow(ctx, sql, args...)
}
