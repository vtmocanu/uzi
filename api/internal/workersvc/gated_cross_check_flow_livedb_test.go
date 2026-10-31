package workersvc

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

func (f *gateFix) gateReasonOf(t *testing.T, run uuid.UUID) (string, bool) {
	t.Helper()
	var reason *string
	if err := f.pool.QueryRow(f.ctx, `SELECT plan_cross_check_gate_reason FROM runs WHERE id=$1`, run).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	if reason == nil {
		return "", false
	}
	return *reason, true
}

func (f *gateFix) crossCheckRows(t *testing.T, run uuid.UUID) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM cross_checks WHERE lead_run_id=$1`, run).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestGatedOldWorkerGateFlowLiveDB drives a worker that does not advertise cross_check_gated_v1
// through gate -> human revise -> re-gate -> approve -> running (PRD #3708 seam 2: the api
// is new, the worker is old). The worker declares no reason; the server records worker_unsupported
// itself, and an old worker's ordinary gate keeps every pre-existing guarantee.
func TestGatedOldWorkerGateFlowLiveDB(t *testing.T) {
	f := newGateFix(t, 3)
	f.liveWorker()
	run := f.newRun("issue")
	f.exec(`UPDATE runs SET plan_cross_check_gated=true WHERE id=$1`, run)

	// Generation 1: the old worker presents the ordinary gate with no declaration.
	a := uid()
	f.mustPublish(run, gateReq(i64(1), a), 1)
	if reason, ok := f.gateReasonOf(t, run); !ok || reason != "worker_unsupported" {
		t.Fatalf("generation 1 gate reason = %q (set %v), want worker_unsupported", reason, ok)
	}
	if n := f.crossCheckRows(t, run); n != 0 {
		t.Fatalf("an old worker must create no cross_checks row, got %d", n)
	}
	// The owner revises; the worker re-gates the replacement. The reason is about the
	// replaced candidate, so it clears, and the revision adds no check.
	input := approvalGuardRevise(t, f, run)
	approvalGuardDeliver(t, f, run, input)
	req := approvalGuardReplacement(input)
	if _, applied, err := f.svc.SetState(f.ctx, f.wkr, run, req); err != nil || !applied {
		t.Fatalf("re-gate: applied=%v err=%v", applied, err)
	}
	if reason, ok := f.gateReasonOf(t, run); ok {
		t.Fatalf("a human revision must clear the reason, got %q", reason)
	}
	if n := f.crossCheckRows(t, run); n != 0 {
		t.Fatalf("a human revision after the gate created %d cross_checks rows", n)
	}

	gen := int64(1)
	// Approval starts implementation: without it the running report is refused.
	if _, applied, err := f.svc.SetState(f.ctx, f.wkr, run, StateRequest{State: "running", ClaimGeneration: &gen}); err != nil || applied {
		t.Fatalf("running without an approval: applied=%v err=%v", applied, err)
	}
	if _, err := f.submit(run, "approve_plan", nil, SubmitInputOptions{ExpectedGateRevision: i64(2)}); err != nil {
		t.Fatalf("approve: %v", err)
	}
	var approval int64
	if err := f.pool.QueryRow(f.ctx, `SELECT id FROM run_user_inputs WHERE run_id=$1 AND kind='approve_plan'`, run).Scan(&approval); err != nil {
		t.Fatal(err)
	}
	if receipt, err := f.svc.AckInputs(f.ctx, f.wkr, run, gen, []int64{approval}); err != nil || !receipt.Active {
		t.Fatalf("ack approval: %+v err=%v", receipt, err)
	}
	if receipt, err := f.svc.ApplyInputs(f.ctx, f.wkr, run, gen, []int64{approval}); err != nil || !receipt.Active {
		t.Fatalf("apply approval: %+v err=%v", receipt, err)
	}
	run2, applied, err := f.svc.SetState(f.ctx, f.wkr, run, StateRequest{State: "running", ClaimGeneration: &gen})
	if err != nil || !applied || run2.Status != "running" {
		t.Fatalf("approved gate did not start implementing: status=%s applied=%v err=%v", run2.Status, applied, err)
	}
}

// The server-recorded reason also covers a later claim generation, and a reclaim that
// re-presents the same gate keeps it.
func TestGatedOldWorkerLaterGenerationLiveDB(t *testing.T) {
	f := newGateFix(t, 3)
	run := f.newRun("issue")
	f.exec(`UPDATE runs SET plan_cross_check_gated=true, claim_generation=2 WHERE id=$1`, run)
	a := uid()
	gen := int64(2)
	f.mustPublish(run, gateReq(&gen, a), 1)
	if reason, _ := f.gateReasonOf(t, run); reason != "worker_unsupported" {
		t.Fatalf("generation 2 gate reason = %q, want worker_unsupported", reason)
	}
	gen = f.reclaim(run)
	f.mustPublish(run, gateReq(&gen, a), 1)
	if reason, _ := f.gateReasonOf(t, run); reason != "worker_unsupported" {
		t.Fatalf("a re-presented gate lost its reason: %q", reason)
	}
}

// A worker can never declare the server-only reason, and a capable worker with no check is not
// given it either.
func TestGatedWorkerUnsupportedIsServerOnlyLiveDB(t *testing.T) {
	f := newGateFix(t, 3)
	run := f.newRun("issue")
	f.exec(`UPDATE runs SET plan_cross_check_gated=true WHERE id=$1`, run)
	req := gateReq(i64(1), uid())
	declared := "worker_unsupported"
	req.PlanCrossCheckGateReason = &declared
	if _, _, err := f.svc.SetState(f.ctx, f.wkr, run, req); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("a worker-declared worker_unsupported must be refused, got %v", err)
	}
	if row := f.row(run); row.status == "awaiting_approval" {
		t.Fatalf("a refused declaration still gated the run")
	}
}

// After the gate is presented, a reclaimed (or hostile) gated lead can never start a first check.
// This is the server half of D16; the agent half skips discovery on a stored plan_md.
func TestGatedNoCheckAfterGateLiveDB(t *testing.T) {
	f := newGateFix(t, 3)
	f.liveWorker()
	run := f.newRun("issue")
	f.exec(`UPDATE runs SET plan_cross_check_gated=true WHERE id=$1`, run)
	f.mustPublish(run, gateReq(i64(1), uid()), 1)
	gen := f.reclaim(run)
	f.exec(`UPDATE runs SET status='claimed' WHERE id=$1`, run)
	f.exec(`UPDATE workers SET protocol_capabilities=ARRAY['cross_check_v1','cross_check_rounds_v1','cross_check_gated_v1'] WHERE id=$1`, f.wkr.ID)
	wkr := f.wkr
	wkr.ProtocolCapabilities = []string{"cross_check_v1", "cross_check_rounds_v1", "cross_check_gated_v1"}
	c := PlanCrossCheckCandidate{PlanMd: "plan", Milestones: []byte("[]"), RequiredCapabilities: []string{}, RequiredTools: []string{}, SizeClass: "s",
		BaseCommit: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", PlanningDiff: "diff"}
	if _, err := f.svc.SubmitPlanCrossCheck(f.ctx, wkr, run, gen, c, 1); !errors.Is(err, ErrCrossCheckRefused) {
		t.Fatalf("a check after the gate was presented: %v", err)
	}
	if n := f.crossCheckRows(t, run); n != 0 {
		t.Fatalf("cross_checks rows = %d", n)
	}
}
