package recovery

import (
	"github.com/vtmocanu/uzi/api/internal/capability"
	"testing"
)

func TestExactHoldMergedPolicyObservationLiveDB(t *testing.T) {
	e, snapshot := newMergedPredecessorEnv(t)
	e.w.ProtocolCapabilities = append(e.w.ProtocolCapabilities, capability.RecoveryHeldPublicationV2)
	n, err := e.svc.DiscardMergedPredecessorHolds(e.ctx, snapshot)
	if err != nil || n != 1 {
		t.Fatalf("policy settlement = %d, %v", n, err)
	}
	out, err := e.svc.ObserveExactHold(e.ctx, e.w, e.run, 1)
	if err != nil || out == nil || out.State != "discarded" || out.ReleaseEvidence != "merged_predecessor_discard" {
		t.Fatalf("known policy settlement must be observed: %+v, %v", out, err)
	}
	if out.FinalReceipt != nil {
		t.Fatal("policy observation fabricated FINAL")
	}
}
