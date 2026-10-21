package workersvc

import (
	"context"
	"strings"
	"testing"
)

// An unverifiable semantic-profile contract on the permit path holds like a corrupt
// structural one: deny contract_not_frozen, no internal error, capped or not.
func TestPermitInvalidSemanticContractDenies(t *testing.T) {
	for _, capped := range []bool{false, true} {
		name := "uncapped"
		if capped {
			name = "capped"
		}
		t.Run(name, func(t *testing.T) {
			w := worker()
			run := interlockedRun(w)
			run.MilestonesFrozen = frozenJSON(t, "m1", "m2")
			run.CompletionContract = []byte(strings.Replace(string(contractJSON(t, "m1", "m2")), "structural", "semantic", 1))
			svc := New(&fakeStore{runOwned: run}, newBox(t), testParams())
			req := CompletionPermitRequest{ContractRevision: 1, Branch: "b", Head: "h"}
			if capped {
				req = cappedPermitRequest(t, true)
			}
			res, err := svc.RequestCompletionPermit(context.Background(), w, run.ID, req)
			if err != nil || res.Granted || res.DenyReason != CompletionDenyContractNotFrozen {
				t.Fatalf("result=%+v err=%v", res, err)
			}
		})
	}
}
