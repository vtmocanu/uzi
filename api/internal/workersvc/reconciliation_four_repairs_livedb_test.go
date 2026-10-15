package workersvc

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/capability"
)

func TestReconciliationMemoryOversizeInclusionLiveDB(t *testing.T) {
	f, id, opts := newMilestoneReconciliationFix(t)
	f.wkr.ProtocolCapabilities = append(f.wkr.ProtocolCapabilities, capability.InputInclusionV1)
	f.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", f.wkr.ID, f.wkr.ProtocolCapabilities)
	var input int64
	if err := f.pool.QueryRow(f.ctx, "INSERT INTO run_user_inputs(run_id,kind,body,consumed_at,consumed_claim_generation,consumed_worker_id) VALUES($1,'follow_up','old followup',now(),1,$2) RETURNING id", id, f.wkr.ID).Scan(&input); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.SubmitInputWithOptions(f.ctx, f.userID, id, "approve_plan", "", nil, opts); err != nil {
		t.Fatal(err)
	}
	check := func() {
		t.Helper()
		for _, gen := range []*int64{nil, i64(0), i64(1), i64(99)} {
			_, err := f.svc.SaveMemory(f.ctx, f.wkr, id, "old", "old memory", "", "", gen)
			want := ErrStaleClaim
			if gen == nil || *gen == 0 {
				want = ErrMissingClaimGeneration
			}
			if !errors.Is(err, want) {
				t.Errorf("memory gen=%v: %v, want %v", gen, err, want)
			}
		}
		for _, gen := range []int64{0, 1, 99} {
			receipt, err := f.svc.IncludeInputs(f.ctx, f.wkr, id, gen, []int64{input})
			if err != nil || receipt.Active || receipt.Reason != ReceiptStale {
				t.Errorf("old inclusion gen=%d: %+v %v", gen, receipt, err)
			}
		}
		f.svc.NoteOversizeBatch(f.ctx, f.wkr, id)
		if got := f.svc.persistFail.stats(id); got.streak != 0 {
			t.Errorf("old oversize recorded: %+v", got)
		}
		var included bool
		if err := f.pool.QueryRow(f.ctx, "SELECT included_at IS NOT NULL FROM run_user_inputs WHERE id=$1", input).Scan(&included); err != nil || included {
			t.Errorf("old inclusion effect: %v %v", included, err)
		}
		var memories int
		if err := f.pool.QueryRow(f.ctx, "SELECT count(*) FROM agent_memory WHERE run_id=$1", id).Scan(&memories); err != nil || memories != 0 {
			t.Errorf("old memory effects: %d %v", memories, err)
		}
	}
	check()
	claim, err := f.svc.Claim(f.ctx, f.wkr, nil)
	if err != nil || claim == nil || claim.ClaimGeneration != 2 {
		t.Fatalf("reclaim: %+v %v", claim, err)
	}
	if _, applied, err := f.svc.SetState(f.ctx, f.wkr, id, StateRequest{State: "running", ClaimGeneration: i64(2)}); err != nil || !applied {
		t.Fatalf("run: %v %v", applied, err)
	}
	check()
	if _, err := f.svc.SaveMemory(f.ctx, f.wkr, id, "new", "current memory", "", "", i64(2)); err != nil {
		t.Fatalf("current memory: %v", err)
	}
	receipt, err := f.svc.IncludeInputs(f.ctx, f.wkr, id, 2, []int64{input})
	if err != nil || !receipt.Active || len(receipt.Inputs) != 1 {
		t.Fatalf("current recovered inclusion: %+v %v", receipt, err)
	}
	f.svc.NoteOversizeBatch(f.ctx, f.wkr, id)
	if got := f.svc.persistFail.stats(id); got.streak != 0 {
		t.Fatalf("current oversized body inferred authority: %+v", got)
	}
	f.exec("UPDATE runs SET claim_released_at=now() WHERE id=$1", id)
	if _, err := f.svc.SaveMemory(f.ctx, f.wkr, id, "released", "released memory", "", "", i64(2)); !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("released current memory: %v", err)
	}
	receipt, err = f.svc.IncludeInputs(f.ctx, f.wkr, id, 2, []int64{input})
	if err != nil || receipt.Active || receipt.Reason != ReceiptStale {
		t.Fatalf("released current inclusion: %+v %v", receipt, err)
	}
}

func TestReconciliationWitnessedEmptyGateLiveDB(t *testing.T) {
	f, id, _ := newMilestoneReconciliationFix(t)
	before := reconciliationRow(t, f, id)
	req := gateReq(i64(1), uid())
	req.Milestones = &[]Milestone{}
	req.GateCheckpointWitness = &apitypes.GateCheckpointWitness{SourceHead: "1111111111111111111111111111111111111111", PublishedCheckpoint: retentionTestTip}
	f.mustPublish(id, req, 2)
	presented := reconciliationRow(t, f, id)
	if !jsonEqual(t, presented.MilestonesCandidate, "[]") {
		t.Fatalf("explicit empty candidate retained old list: %s", presented.MilestonesCandidate)
	}
	payload, ok, err := decodeGatePresentedPayload(presented.GatePresentedPayload)
	if err != nil || !ok || !jsonEqual(t, payload.Milestones, "[]") {
		t.Fatalf("immutable empty payload: %+v %v", payload, err)
	}
	digest, err := ResetStateDigest(presented)
	if err != nil {
		t.Fatal(err)
	}
	revision := 1
	_, err = f.svc.SubmitInputWithOptions(f.ctx, f.userID, id, "approve_plan", "", nil, SubmitInputOptions{ExpectedGateRevision: i64(2), ExpectedResetStateDigest: digest, Reconciliation: &apitypes.MilestoneReconciliation{Mode: "reset", Reason: "remove criteria", ExpectedCompletionRevision: &revision}})
	if err != nil {
		t.Fatal(err)
	}
	after := reconciliationRow(t, f, id)
	if !jsonEqual(t, after.MilestonesFrozen, "[]") || after.ContractRevision.Int32 != 2 {
		t.Fatalf("empty reset: %s %v", after.MilestonesFrozen, after.ContractRevision)
	}
	assertReconciliationProgressEmpty(t, after)
	if after.CheckpointTip != before.CheckpointTip {
		t.Fatal("checkpoint moved")
	}
	claim, err := f.svc.Claim(f.ctx, f.wkr, nil)
	if err != nil || claim == nil || claim.ClaimGeneration != 2 || !claim.PlanApproved {
		t.Fatalf("empty reclaim: %+v %v", claim, err)
	}
}

func TestReconciliationWitnessedMilestoneCompatibilityLiveDB(t *testing.T) {
	for _, mode := range []string{"absent", "malformed", "unwitnessed", "retry"} {
		t.Run(mode, func(t *testing.T) {
			f, id, _ := newMilestoneReconciliationFix(t)
			before := reconciliationRow(t, f, id)
			req := gateReq(i64(1), uid())
			req.GateCheckpointWitness = &apitypes.GateCheckpointWitness{SourceHead: "1111111111111111111111111111111111111111", PublishedCheckpoint: retentionTestTip}
			switch mode {
			case "absent":
				req.Milestones = nil
			case "malformed":
				req.Milestones = &[]Milestone{{ID: "", Title: "invalid"}}
			case "unwitnessed":
				req.Milestones = &[]Milestone{}
				req.GateCheckpointWitness = nil
			case "retry":
				pid := uuid.UUID(before.GatePresentationID.Bytes)
				req.PresentationID = &pid
				req.Milestones = &[]Milestone{}
			}
			revision := int64(2)
			if mode == "retry" {
				revision = 1
			}
			f.mustPublish(id, req, revision)
			after := reconciliationRow(t, f, id)
			if mode == "malformed" {
				if len(after.MilestonesCandidate) != 0 {
					t.Fatalf("malformed candidate accepted: %s", after.MilestonesCandidate)
				}
			} else if !jsonEqual(t, after.MilestonesCandidate, string(before.MilestonesCandidate)) {
				t.Fatalf("compatibility changed: %s", after.MilestonesCandidate)
			}
		})
	}
}
