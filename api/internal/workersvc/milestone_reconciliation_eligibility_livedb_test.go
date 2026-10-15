package workersvc

import (
	"testing"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Run only against a throwaway UZI_TEST_DATABASE_URL; the lead runs this focused
// regression after integrating the bounded placement unit.
func TestMilestoneReconciliationEligibilityLiveDB(t *testing.T) {
	for _, mode := range []string{"capability scheduling off", "owner override"} {
		t.Run(mode, func(t *testing.T) {
			f, id, opts := newMilestoneReconciliationFix(t)
			f.svc.p.WorkerAffinityCeiling = 0
			f.svc.SetCapabilitySettings(fakeCapabilitySettings{on: mode != "capability scheduling off"})
			if mode == "owner override" {
				n, err := f.q.ClearRunRequiredCapabilities(f.ctx, store.ClearRunRequiredCapabilitiesParams{
					ID: id, UserID: f.userID, GateRevision: 1,
				})
				if err != nil || n != 1 {
					t.Fatalf("owner override = %d, %v; want one cleared run", n, err)
				}
			}
			if _, err := f.svc.SubmitInputWithOptions(f.ctx, f.userID, id, "approve_plan", "", nil, opts); err != nil {
				t.Fatal(err)
			}
			f.wkr.ProtocolCapabilities = []string{capability.CompletionInterlockV1}
			f.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", f.wkr.ID, f.wkr.ProtocolCapabilities)
			// The worker supports completion but lacks generation stamping, isolating
			// the reconciliation requirement from the existing completion fence.
			claim, err := f.svc.Claim(f.ctx, f.wkr, nil)
			if err != nil || claim != nil {
				t.Fatalf("legacy worker claimed audited run: claim=%+v err=%v", claim, err)
			}

			ordinary := f.newRun("issue")
			f.exec("UPDATE runs SET status='queued', required_capabilities='{}' WHERE id=$1", ordinary)
			claim, err = f.svc.Claim(f.ctx, f.wkr, nil)
			if err != nil || claim == nil || claim.RunID != ordinary.String() {
				t.Fatalf("ordinary run became ineligible: claim=%+v err=%v", claim, err)
			}

			// Free the ordinary claim's slot before testing the audited reclaim.
			f.exec("UPDATE runs SET status='failed' WHERE id=$1", ordinary)
			f.wkr.ProtocolCapabilities = append(f.wkr.ProtocolCapabilities, capability.CredentialSwitchV1)
			f.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", f.wkr.ID, f.wkr.ProtocolCapabilities)
			claim, err = f.svc.Claim(f.ctx, f.wkr, nil)
			if err != nil || claim == nil || claim.RunID != id.String() || claim.ClaimGeneration != 2 {
				t.Fatalf("generation-capable worker cannot reclaim audited run: claim=%+v err=%v", claim, err)
			}
		})
	}
}
