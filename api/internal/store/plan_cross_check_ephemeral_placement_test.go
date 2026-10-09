package store_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/capability"
)

func TestPlanCrossCheckEphemeralProvisioningMirrorsLiveDB(t *testing.T) {
	for _, lane := range []string{"dedicated", "legacy"} {
		for _, scenario := range []string{"free", "full", "missing-harness", "provisioned-child", "foreign-parent"} {
			t.Run(lane+"/"+scenario, func(t *testing.T) {
				fx := newFleetFixture(t)
				optInEphemeral(fx)
				t.Cleanup(func() {
					mustExec(fx.ctx, t, fx.pool, `UPDATE users SET ephemeral_workers_enabled=false WHERE id=$1`, fx.userID)
				})
				lead, child := fx.queuedRun(), fx.queuedRun()
				mustExec(fx.ctx, t, fx.pool, `UPDATE runs SET status='running',claim_generation=1 WHERE id=$1`, lead)
				mustExec(fx.ctx, t, fx.pool, `UPDATE runs SET kind='cross_check',issue_iid=NULL,target_run_id=$2,
					harness='codex',report_only=true,budget_wall_seconds=1800,required_tools='{}',
					status_since=now()-interval '1 hour' WHERE id=$1`, child, lead)
				mustExec(fx.ctx, t, fx.pool, `INSERT INTO cross_checks
					(id,lead_run_id,checker_run_id,stage,round,lead_claim_generation,
					plan_md,milestones,size_class,base_commit,candidate_digest,checker_harness,deadline_at)
					VALUES ($1,$2,$3,'plan',1,1,'plan','[]'::jsonb,'s',repeat('a',40),
					$4,'codex',now()+interval '5 minutes')`, uuid.New(), lead, child, []byte("test-digest"))
				bound := lead
				switch scenario {
				case "provisioned-child":
					bound = child
				case "foreign-parent":
					bound = fx.queuedRun()
				}
				w := seedEphemeralWorkerBound(fx, bound)
				markOnline(fx, w, capOf(2))
				protocols := []string{capability.CrossCheckV1, capability.CodexHarnessV1, capability.CodexRuntimeV2}
				if lane == "dedicated" {
					protocols = append(protocols, "cross_check_lane_v1")
					mustExec(fx.ctx, t, fx.pool, `UPDATE workers SET max_cross_check_slots=1 WHERE id=$1`, w)
				}
				if scenario == "missing-harness" {
					protocols = []string{capability.CrossCheckV1}
					if lane == "dedicated" {
						protocols = append(protocols, "cross_check_lane_v1")
					}
				}
				mustExec(fx.ctx, t, fx.pool, `UPDATE workers SET protocol_capabilities=$2 WHERE id=$1`, w, protocols)
				if bound == lead {
					mustExec(fx.ctx, t, fx.pool, `UPDATE runs SET worker_id=$2 WHERE id=$1`, lead, w)
				}
				if scenario == "full" {
					fx.holdActive(w, 1)
					if lane == "dedicated" {
						mustExec(fx.ctx, t, fx.pool, `UPDATE runs SET kind='cross_check',issue_iid=NULL,
							target_run_id=$2,harness='codex',report_only=true,budget_wall_seconds=1800,cross_check_lane=true
							WHERE worker_id=$1 AND id<>$2`, w, lead)
					}
				}
				iv := leaseInterval(leaseTwoHours)
				gap, sat := listedUnplaceable(fx, 1000, iv), listedSaturation(fx, 1000, iv)
				wantGap := scenario == "missing-harness" || scenario == "foreign-parent"
				wantSat := scenario == "full"
				if gap[child] != wantGap || sat[child] != wantSat {
					t.Fatalf("provisioning: gap=%v saturation=%v want gap=%v saturation=%v",
						gap[child], sat[child], wantGap, wantSat)
				}
				wantCount := int64(1)
				if wantGap || wantSat {
					wantCount = 0
				}
				if got := claimableCount(fx, child, iv); got != wantCount {
					t.Fatalf("claimable=%d want=%d", got, wantCount)
				}
			})
		}
	}
}
