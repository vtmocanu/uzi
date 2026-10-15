package workersvc

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pushbroker"
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
			// rc.17 already advertises both older fences. Neither promises the
			// generation fields newly required on checkpoint and memory writes.
			// The always-present rc.17 set, without optional Codex/DinD probes.
			f.wkr.ProtocolCapabilities = []string{
				"completion_interlock_v1", "recovery_archive_v1", "recovery_archive_v2", "recovery_inventory_v1",
				"recovery_completed_publication_v1", "recovery_held_publication_v1", "credential_switch_v1",
				"codex_refresh_recovery_v1", "codex_account_park_v1", "wall_park_v1", "park_run_v1", "input_receipts_v1",
				"input_inclusion_v1", "gate_revision_v1", "advice_claim_fence_v1", "job_runner_v1", "job_files_v1",
				"cross_check_v1", "cross_check_rounds_v1", "cross_check_pins_v1", "cross_check_codex_lead_v1", "ci_config_guard_v1",
			}
			f.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", f.wkr.ID, f.wkr.ProtocolCapabilities)
			claimable := func() int64 {
				t.Helper()
				now := time.Now()
				cutoff := pgtype.Timestamptz{Time: now.Add(time.Hour), Valid: true}
				n, err := f.q.CountOnlineWorkersClaimableForRun(f.ctx, store.CountOnlineWorkersClaimableForRunParams{
					RunID: id, HeartbeatCutoff: pgtype.Timestamptz{Time: now.Add(-time.Minute), Valid: true},
					AffinityCutoff: cutoff, CrossCheckEvaluatedAt: cutoff, CrossCheckAffinityCutoff: cutoff,
					WorkerDockerEnabled: true, CapabilityAware: mode != "capability scheduling off",
					CodexCuratedModels: codexCuratedModelsSlice(), EphemeralLease: pgtype.Interval{Valid: true},
				})
				if err != nil {
					t.Fatal(err)
				}
				return n.Claimable
			}
			if n := claimable(); n != 0 {
				t.Fatalf("health counts rc.17 worker as claimable: %d", n)
			}
			// Isolate the new reconciliation fence from the existing completion fence.
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
			f.wkr.ProtocolCapabilities = append(f.wkr.ProtocolCapabilities, capability.MilestoneReconciliationClaimV1)
			f.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", f.wkr.ID, f.wkr.ProtocolCapabilities)
			if n := claimable(); n != 1 {
				t.Fatalf("health excludes upgraded worker: claimable=%d", n)
			}
			claim, err = f.svc.Claim(f.ctx, f.wkr, nil)
			if err != nil || claim == nil || claim.RunID != id.String() || claim.ClaimGeneration != 2 {
				t.Fatalf("generation-capable worker cannot reclaim audited run: claim=%+v err=%v", claim, err)
			}
			f.exec("UPDATE runs SET status='running' WHERE id=$1", id)
			const tip = "3333333333333333333333333333333333333333"
			calls := 0
			f.svc.SetPublishFn(func(_ context.Context, o pushbroker.Options) (pushbroker.Result, error) {
				calls++
				if o.DeclaredTip != tip {
					t.Fatalf("declared tip=%q", o.DeclaredTip)
				}
				return pushbroker.Result{Disposition: pushbroker.PublishAdvanced}, nil
			})
			published, err := f.svc.Publish(f.ctx, f.wkr, id, tip, []byte("pack"), &claim.ClaimGeneration)
			if err != nil || !published.Published || calls != 1 {
				t.Fatalf("upgraded publish=%+v err=%v broker calls=%d", published, err, calls)
			}
			if got := reconciliationRow(t, f, id).CheckpointTip; !got.Valid || got.String != tip {
				t.Fatalf("checkpoint not persisted: %v", got)
			}
			memory, err := f.svc.SaveMemory(f.ctx, f.wkr, id, "Reset lesson", "Retain the new contract", "inferred", "", &claim.ClaimGeneration)
			if err != nil || memory.Title != "Reset lesson" || memory.Body != "Retain the new contract" {
				t.Fatalf("upgraded memory=%+v err=%v", memory, err)
			}
		})
	}
}
