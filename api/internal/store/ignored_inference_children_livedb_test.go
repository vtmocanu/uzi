package store_test

import (
	"reflect"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestCrossCheckChildrenInheritFrozenInferencePolicyLiveDB(t *testing.T) {
	for _, stage := range []string{"plan", "code"} {
		for _, leadHarness := range []string{"claude", "codex"} {
			t.Run(stage+"/"+leadHarness, func(t *testing.T) {
				fx := newFleetFixture(t)
				t.Cleanup(func() { mustExec(fx.ctx, t, fx.pool, "DELETE FROM users WHERE id=$1", fx.userID) })
				lead, worker := seedCrossCheckLead(fx, leadHarness)
				mustExec(fx.ctx, t, fx.pool, `UPDATE runs SET code_cross_check_required=true,
				 required_capabilities='{jvm}',ignored_inferred_capabilities='{jvm}' WHERE id=$1`, lead)
				// A repo edit after lead creation must not replace its captured policy.
				// Keep the existing live-repo requirement choice distinct from the lead's.
				mustExec(fx.ctx, t, fx.pool, `UPDATE repos SET required_capabilities='{docker}',
				 ignored_inferred_capabilities='{docker}' WHERE id=$1`, fx.repoID)
				mustExec(fx.ctx, t, fx.pool, `UPDATE workers SET max_cross_check_slots=1,
				 protocol_capabilities=ARRAY['cross_check_code_v1','cross_check_lane_v1'] WHERE id=$1`, worker)
				childHarness := "codex"
				if leadHarness == "codex" {
					childHarness = "claude"
				}
				var child store.Run
				var err error
				if stage == "plan" {
					child, err = fx.q.CreatePlanCrossCheckChild(fx.ctx, store.CreatePlanCrossCheckChildParams{
						ChildID: uuid.New(), ChildHarness: childHarness, LeadRunID: lead, UserID: fx.userID,
						WorkerID: pgU(worker), ClaimGeneration: 1, BudgetWallSeconds: 300})
				} else {
					child, err = fx.q.CreateCodeCrossCheckChild(fx.ctx, store.CreateCodeCrossCheckChildParams{
						ChildID: uuid.New(), ChildHarness: childHarness, LeadRunID: lead, UserID: fx.userID,
						WorkerID: pgU(worker), ClaimGeneration: 1, BudgetWallSeconds: 300})
				}
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(child.IgnoredInferredCapabilities, []string{"jvm"}) {
					t.Fatalf("child policy=%v, want frozen lead policy [jvm], not live repo [docker] or default []", child.IgnoredInferredCapabilities)
				}
				if !reflect.DeepEqual(child.RequiredCapabilities, []string{"docker"}) {
					t.Fatalf("existing live-repo requirement changed: %v", child.RequiredCapabilities)
				}
			})
		}
	}
}
