package workersvc

import (
	"context"
	"testing"
)

func TestPolicyHoldHealthCapabilityReason(t *testing.T) {
	for _, count := range []int64{0, 1} {
		r := runRow("queued")
		r.Kind = "prompt"
		r.PolicyRefusalCount = 1
		fs := &healthFakeStore{onlineWorkers: 1, freeSlotWorkers: 1, eligibleWorkers: 1, policyHoldCount: &count}
		svc := healthSvc(fs, defaultHealthSettings())
		ctx := context.Background()
		got := svc.queuedReason(ctx, t0, r, svc.healthThresholds(ctx))
		if (got == reasonNoPolicyHoldCapableWorker) != (count == 0) {
			t.Fatalf("count=%d reason=%q", count, got)
		}
		if len(fs.policyHoldCalls) != 1 || fs.policyHoldCalls[0].RunID != r.ID {
			t.Fatalf("per-run policy diagnostic=%+v", fs.policyHoldCalls)
		}
	}
}
