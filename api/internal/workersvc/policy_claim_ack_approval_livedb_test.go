package workersvc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
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
	makeCodexDefault(t, e, f.aliasID)
	created, err := f.svc.CreateTaskRun(e.ctx, f.userID, f.repoID, "ACK approval", "", false, false, false, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if created.Harness != string(HarnessCodex) {
		t.Fatalf("created harness=%s, want codex", created.Harness)
	}
	// CreateTaskRun returns the row before the atomic credential freeze; read
	// the committed binding rather than supplying it after creation.
	born := mustRun(t, e, created.ID)
	if born.Harness != string(HarnessCodex) || !born.CodexSecretID.Valid || born.CodexSecretID.Bytes != f.aliasID ||
		!born.CodexAuthMode.Valid || born.CodexAuthMode.String != codexAuthModeSubscription {
		t.Fatal("creation did not freeze the selected Codex subscription")
	}
	source, err := apitypes.ParsePolicyRevisionSourceV1(created.PolicyRevisionSource)
	if err != nil || source.Version != 1 || source.Gate != nil || source.PrimaryRevision != nil ||
		source.AssociatedCrossChecks == nil || len(source.AssociatedCrossChecks) != 0 {
		t.Fatalf("initial source=%s err=%v", created.PolicyRevisionSource, err)
	}
	if !bytes.Equal(created.PolicyRevisionSource, born.PolicyRevisionSource) {
		t.Fatal("creation did not persist the initial source")
	}
	e.exec("UPDATE runs SET plan_cross_check_required=false,auto_approve=false,policy_refusal_count=1 WHERE id=$1", created.ID)
	// Dispatch is a fixture precondition; no forge branch seeding is exercised.
	if _, err := f.svc.DispatchTaskRun(e.ctx, f.userID, created.ID); err != nil {
		t.Fatal(err)
	}
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
	if current.PolicyRefusalCount != 1 || !bytes.Equal(born.PolicyRevisionSource, current.PolicyRevisionSource) {
		t.Fatal("Claim changed the initial source or refusal count")
	}
	gen := current.ClaimGeneration
	req := gateReq(&gen, uid())
	req.RequiredCapabilities = &[]string{}
	candidate, err := f.svc.SetStateReportWithReconciliation(e.ctx, w, created.ID, req)
	if err != nil || !candidate.Applied || candidate.PlanApproved == nil || *candidate.PlanApproved || mustRun(t, e, created.ID).PolicyRefusalCount != 1 {
		t.Fatalf("candidate: %+v %v", candidate, err)
	}
	assertRevisionGateBound(t, e, created.ID, candidate.GateRevision)
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
	if err != nil || !approved.Applied || approved.PlanApproved == nil || !*approved.PlanApproved || mustRun(t, e, created.ID).PolicyRefusalCount != 1 {
		t.Fatalf("current human approval: %+v %v", approved, err)
	}

	// An applied G1 approval remains true in GetRunClaimContext for this
	// no-cross-check run, but must not approve a newly published G2 candidate.
	req = gateReq(&gen, uid())
	plan := "A newer plan requiring its own human approval"
	req.PlanMd, req.RequiredCapabilities = &plan, &[]string{}
	newer, err := f.svc.SetStateReportWithReconciliation(e.ctx, w, created.ID, req)
	if err != nil || !newer.Applied || newer.GateRevision != candidate.GateRevision+1 {
		t.Fatalf("newer candidate: %+v %v", newer, err)
	}
	current = mustRun(t, e, created.ID)
	if current.Status != "awaiting_approval" || !current.PlanMd.Valid || current.PlanMd.String != plan ||
		current.PolicyRefusalCount != 1 || current.AutoApprove || current.PlanCrossCheckRequired || current.PlanSource != "agent" ||
		!current.GatePresentationID.Valid || current.GatePresentationID.Bytes != *req.PresentationID {
		t.Fatal("G2 did not persist the new human-only candidate with refusal count 1")
	}
	source = assertRevisionGateBound(t, e, created.ID, newer.GateRevision)
	var ackGate apitypes.PolicyStopGateContext
	if source.Gate == nil || source.PrimaryRevision != nil || len(source.AssociatedCrossChecks) != 0 ||
		json.Unmarshal(newer.PolicyRevisionGate, &ackGate) != nil || ackGate != *source.Gate {
		t.Fatalf("G2 ACK/source mismatch: gate=%s source=%s", newer.PolicyRevisionGate, current.PolicyRevisionSource)
	}
	inputs, err := e.q.ListRunInputsForRun(e.ctx, store.ListRunInputsForRunParams{RunID: created.ID, Lim: 2})
	if err != nil || len(inputs) != 1 {
		t.Fatalf("approval inventory: count=%d err=%v", len(inputs), err)
	}
	receipt := inputs[0]
	if receipt.ID != approval.ID || receipt.Kind != "approve_plan" || !receipt.AppliedAt.Valid ||
		!receipt.ConsumedAt.Valid || !receipt.ConsumedWorkerID.Valid || receipt.ConsumedWorkerID.Bytes != w.ID ||
		!receipt.ConsumedClaimGeneration.Valid || receipt.ConsumedClaimGeneration.Int64 != gen ||
		!receipt.GateBinding.Valid || receipt.GateBinding.String != "bound" ||
		!receipt.GateRevision.Valid || receipt.GateRevision.Int64 != candidate.GateRevision {
		t.Fatal("the sole persisted approval must be the applied G1 receipt")
	}
	currentApproved, err := e.q.HasPolicyCurrentGateApproval(e.ctx, store.HasPolicyCurrentGateApprovalParams{
		RunID: created.ID, GateRevision: newer.GateRevision,
	})
	if err != nil || currentApproved {
		t.Fatalf("G2 unexpectedly has an applied approval: approved=%v err=%v", currentApproved, err)
	}
	legacy, err := e.q.GetRunClaimContext(e.ctx, created.ID)
	if err != nil || !legacy.HumanPlanApproved {
		t.Fatalf("old applied approval must keep legacy human approval true: approved=%v err=%v", legacy.HumanPlanApproved, err)
	}
	if newer.PlanApproved == nil || *newer.PlanApproved {
		t.Fatalf("G1 approval must not approve G2 in the state ACK: %+v", newer)
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
				e.exec(`UPDATE runs SET harness='claude', codex_secret_id=NULL, codex_auth_mode=NULL,
					codex_secret_label=NULL, codex_account_key=NULL, codex_material_revision=NULL,
					codex_account_revision=NULL WHERE id=$1`, run)
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
