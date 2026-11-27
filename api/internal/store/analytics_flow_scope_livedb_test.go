package store_test

import (
	"testing"

	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4729: kind scoping of the Flow gate evidence. Plan response covers every selected
// kind, chat included; the funnel and approval cohorts are restricted to issue and
// self_improve, and settled_auto is not specific to kind issue.

// TestAnalyticsFlowChatInPlanResponseOnlyLiveDB: a chat run with a presentation and an
// applied bound receipt is measured by plan_response when kinds include chat, and is absent
// from the funnel and the approval cohort.
func TestAnalyticsFlowChatInPlanResponseOnlyLiveDB(t *testing.T) {
	f := newSpendFixture(t)
	o := f.owner("flowchat")
	chat := f.run(spendRun{Owner: o.User, Kind: runkind.Chat, Status: "completed", At: flowAt(12, 10, 0), Finished: flowAt(12, 12, 0)})
	f.patch(chat, "gate_revision = 1")
	f.presentation(chat, 1, gatePres)
	f.receipt(chat, receipt{Revision: 1, CreatedAt: gateIn, AppliedAt: gateApplied})

	for _, kinds := range [][]string{runkind.All(), {runkind.Chat}} {
		d := f.flow(flowCall{Owner: o.User, Kinds: kinds}).Doc
		if d.PlanResponse.N != 1 {
			t.Errorf("kinds %v: plan_response n = %d, want 1", kinds, d.PlanResponse.N)
		} else {
			wantNum(t, "plan_response p50", d.PlanResponse.P50Seconds, 1800)
		}
		if d.DeliveryFunnel.Stages.Created != 0 || d.DeliveryFunnel.Planning != (store.AnalyticsFlowPlanning{}) ||
			d.DeliveryFunnel.Disclosure != (store.AnalyticsFlowFunnelDisclosure{}) {
			t.Errorf("kinds %v: chat run leaked into the funnel: %+v", kinds, d.DeliveryFunnel)
		}
		if d.Approval.Total != 0 || d.Approval.Human != 0 {
			t.Errorf("kinds %v: chat run leaked into approval: %+v", kinds, d.Approval)
		}
	}

	// Without chat in the selector the run is out of scope for plan_response too.
	d := f.flow(flowCall{Owner: o.User, Kinds: []string{runkind.Issue}}).Doc
	if d.PlanResponse.N != 0 {
		t.Errorf("issue-only kinds: plan_response n = %d, want 0", d.PlanResponse.N)
	}
}

// TestAnalyticsFlowSelfImproveSettledAutoLiveDB: a self_improve run with auto_approve at
// gate revision 0 and an agent plan settles auto, like an issue run.
func TestAnalyticsFlowSelfImproveSettledAutoLiveDB(t *testing.T) {
	f := newSpendFixture(t)
	o := f.owner("flowsi")
	repo, _ := f.repo(o, "flowsi")
	id := f.run(spendRun{Owner: o.User, Repo: repo, Kind: runkind.SelfImprove, Status: "completed", At: flowAt(12, 10, 0), Finished: flowAt(12, 12, 0)})
	f.patch(id, "auto_approve = true, plan_md = 'p', plan_source = 'agent', gate_revision = 0")

	d := f.flow(flowCall{Owner: o.User}).Doc
	if want := (store.AnalyticsFlowPlanning{SettledAuto: 1}); d.DeliveryFunnel.Planning != want {
		t.Errorf("planning = %+v, want %+v", d.DeliveryFunnel.Planning, want)
	}
	if d.DeliveryFunnel.Stages.Created != 1 || d.DeliveryFunnel.Stages.PlanSettled != 1 {
		t.Errorf("stages = %+v, want created 1 and plan_settled 1", d.DeliveryFunnel.Stages)
	}
	if d.Approval.AutoApproveConfigured != 1 || d.Approval.Total != 1 {
		t.Errorf("approval = %+v, want one auto_approve_configured", d.Approval)
	}
}
