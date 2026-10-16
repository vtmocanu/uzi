package workersvc

import (
	"errors"
	"reflect"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/capability"
)

func TestPolicyHoldPrerequisitesLiveDB(t *testing.T) {
	for _, missing := range []string{"api", "snapshot", "lease", "worker", "generation"} {
		t.Run(missing, func(t *testing.T) {
			e := setupPolicyHold(t)
			switch missing {
			case "api":
				e.svc.p.PolicyHoldNegotiated = false
			case "snapshot":
				e.svc.p.PolicyHoldSnapshotEnabled = false
			case "lease":
				e.svc.p.TerminalPendingLease = 0
			case "worker":
				e.wkr.ProtocolCapabilities = []string{capability.CredentialSwitchV1}
			case "generation":
				e.wkr.ProtocolCapabilities = []string{capability.ProviderPolicyHoldV1}
			}
			before := mustRun(t, e.codexTestEnv, e.runID)
			_, d, err := e.svc.ReportPolicyHold(e.ctx, e.wkr, e.runID, policyRequest())
			if !errors.Is(err, ErrInvalidState) || d != PolicyHoldPending {
				t.Fatalf("missing %s accepted: %s %v", missing, d, err)
			}
			if after := mustRun(t, e.codexTestEnv, e.runID); !reflect.DeepEqual(before, after) {
				t.Fatal("unnegotiated hold mutated run")
			}
		})
	}
}
