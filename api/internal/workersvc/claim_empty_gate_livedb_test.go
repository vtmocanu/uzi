package workersvc

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

func TestMilestoneReconciliationEmptyGateClaimLiveDB(t *testing.T) {
	f, id := seedMilestoneReconciliationFix(t)
	req := gateReq(i64(1), uid())
	req.Milestones = &[]Milestone{}
	req.GateCheckpointWitness = &apitypes.GateCheckpointWitness{
		SourceHead: "1111111111111111111111111111111111111111", PublishedCheckpoint: retentionTestTip,
	}
	f.mustPublish(id, req, 1)
	// Recover the persisted gate through the real claim assembly and JSON wire.
	f.exec("UPDATE runs SET status='queued' WHERE id=$1", id)
	claim, err := f.svc.Claim(f.ctx, f.wkr, nil)
	if err != nil || claim == nil || claim.ResumePhase != "awaiting_approval" {
		t.Fatalf("gate reclaim=%+v err=%v", claim, err)
	}
	body, err := json.Marshal(claim)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Milestones          json.RawMessage `json:"milestones"`
		ExecutionMilestones []Milestone     `json:"execution_milestones"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatal(err)
	}
	if string(wire.Milestones) != "[]" || len(wire.ExecutionMilestones) != 4 {
		t.Fatalf("recovered candidate=%s frozen=%+v", wire.Milestones, wire.ExecutionMilestones)
	}
	// Explicit [] cannot authorize an old worker to bypass owner consent.
	f.exec("UPDATE runs SET status='awaiting_approval' WHERE id=$1", id)
	if _, err := f.svc.SubmitInput(f.ctx, f.userID, id, "approve_plan", "", nil); !errors.Is(err, ErrReconciliationRequired) {
		t.Fatalf("ordinary approval of empty replacement=%v", err)
	}
}
