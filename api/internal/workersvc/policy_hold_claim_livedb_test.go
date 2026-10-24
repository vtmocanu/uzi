package workersvc

import (
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
	"testing"
)

// The durable constraint is deliberately tested independently of harness/account
// admission: even clearing ordinary scheduling requirements cannot clear it.
func TestPolicyHoldClaimCannotOverrideProtocolLiveDB(t *testing.T) {
	for _, mode := range []string{"scheduling off", "owner override"} {
		t.Run(mode, func(t *testing.T) {
			f, old, _ := newMilestoneReconciliationFix(t)
			f.exec("UPDATE runs SET status='failed' WHERE id=$1", old)
			f.svc.p.WorkerAffinityCeiling = 0
			f.svc.SetCapabilitySettings(fakeCapabilitySettings{on: mode != "scheduling off"})
			id := f.newRun("issue")
			f.exec("UPDATE runs SET status='awaiting_approval',gate_revision=1,policy_refusal_count=1,required_capabilities='{docker}' WHERE id=$1", id)
			if mode == "owner override" {
				n, err := f.q.ClearRunRequiredCapabilities(f.ctx, store.ClearRunRequiredCapabilitiesParams{ID: id, UserID: f.userID, GateRevision: 1})
				if err != nil || n != 1 {
					t.Fatalf("owner override: %d %v", n, err)
				}
			}
			f.exec("UPDATE runs SET status='queued' WHERE id=$1", id)
			f.wkr.ProtocolCapabilities = []string{
				"completion_interlock_v1", "recovery_archive_v1", "recovery_archive_v2", "recovery_inventory_v1",
				"recovery_completed_publication_v1", "recovery_held_publication_v1", "credential_switch_v1",
				"codex_refresh_recovery_v1", "codex_account_park_v1", "wall_park_v1", "park_run_v1", "input_receipts_v1",
				"input_inclusion_v1", "gate_revision_v1", "advice_claim_fence_v1", "job_runner_v1", "job_files_v1",
				"cross_check_v1", "cross_check_rounds_v1", "cross_check_pins_v1", "cross_check_codex_lead_v1", "ci_config_guard_v1",
				capability.MilestoneReconciliationClaimV1,
			}
			f.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", f.wkr.ID, f.wkr.ProtocolCapabilities)
			claim, err := f.svc.Claim(f.ctx, f.wkr, nil)
			if err != nil || claim != nil {
				t.Fatalf("missing policy protocol admitted: claim=%+v err=%v", claim, err)
			}
			f.wkr.ProtocolCapabilities = append(f.wkr.ProtocolCapabilities, capability.ProviderPolicyHoldV1)
			f.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", f.wkr.ID, f.wkr.ProtocolCapabilities)
			claim, err = f.svc.Claim(f.ctx, f.wkr, nil)
			if err != nil || claim == nil || claim.RunID != id.String() || !claim.PolicyHoldResume {
				t.Fatalf("capable claim unavailable: claim=%+v err=%v", claim, err)
			}
		})
	}
}
