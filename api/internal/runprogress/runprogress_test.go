package runprogress

import (
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

func TestDerive(t *testing.T) {
	three := []string{"m1", "m2", "m3"}
	pct := func(n int) *int { return &n }
	tests := []struct {
		name  string
		in    Input
		want  apitypes.RunProgress
		isNil bool
	}{
		{name: "completed is null", in: Input{Kind: "issue", Status: "completed", FrozenIDs: three}, isNil: true},
		{name: "failed is null", in: Input{Kind: "issue", Status: "failed"}, isNil: true},
		{name: "cancelled is null", in: Input{Kind: "issue", Status: "cancelled"}, isNil: true},
		{name: "awaiting_approval waits", in: Input{Kind: "issue", Status: "awaiting_approval"}, want: apitypes.RunProgress{State: StateWaiting}},
		{name: "awaiting_input waits", in: Input{Kind: "issue", Status: "awaiting_input"}, want: apitypes.RunProgress{State: StateWaiting}},
		{name: "awaiting_followup waits", in: Input{Kind: "issue", Status: "awaiting_followup"}, want: apitypes.RunProgress{State: StateWaiting}},
		{name: "limit_wait parked", in: Input{Kind: "issue", Status: "limit_wait"}, want: apitypes.RunProgress{State: StateParked}},
		{name: "pool_wait parked", in: Input{Kind: "issue", Status: "pool_wait"}, want: apitypes.RunProgress{State: StateParked}},
		{name: "recovery_wait parked", in: Input{Kind: "issue", Status: "recovery_wait"}, want: apitypes.RunProgress{State: StateParked}},
		{name: "paused parked", in: Input{Kind: "issue", Status: "paused"}, want: apitypes.RunProgress{State: StateParked}},
		{name: "queued", in: Input{Kind: "issue", Status: "queued"}, want: apitypes.RunProgress{State: StateQueued}},
		{name: "claimed", in: Input{Kind: "issue", Status: "claimed"}, want: apitypes.RunProgress{State: StateQueued}},
		{name: "stalled", in: Input{Kind: "issue", Status: "running", Health: "stalled"}, want: apitypes.RunProgress{State: StateStalled}},
		{name: "looping flags like stalled", in: Input{Kind: "issue", Status: "running", Health: "looping"}, want: apitypes.RunProgress{State: StateStalled}},
		{name: "slow health is not a flag", in: Input{Kind: "issue", Status: "running", Health: "slow", FrozenIDs: three}, want: apitypes.RunProgress{State: StatePercent, Pct: pct(11), MilestoneTotal: 3}},
		{name: "planning", in: Input{Kind: "issue", Status: "running", IsPlanning: true}, want: apitypes.RunProgress{State: StatePlanning}},
		{name: "awaiting_input beats stalled", in: Input{Kind: "issue", Status: "awaiting_input", Health: "stalled"}, want: apitypes.RunProgress{State: StateWaiting}},
		{name: "parked beats stalled", in: Input{Kind: "issue", Status: "limit_wait", Health: "looping"}, want: apitypes.RunProgress{State: StateParked}},
		{name: "queued beats stalled", in: Input{Kind: "issue", Status: "queued", Health: "stalled"}, want: apitypes.RunProgress{State: StateQueued}},
		{name: "stalled beats planning", in: Input{Kind: "issue", Status: "running", Health: "stalled", IsPlanning: true}, want: apitypes.RunProgress{State: StateStalled}},
		{name: "planning beats percent", in: Input{Kind: "issue", Status: "running", IsPlanning: true, FrozenIDs: three}, want: apitypes.RunProgress{State: StatePlanning, MilestoneTotal: 3}},
		{name: "nil frozen on issue run", in: Input{Kind: "issue", Status: "running"}, want: apitypes.RunProgress{State: StateNone}},
		{name: "empty frozen on issue run", in: Input{Kind: "issue", Status: "running", FrozenIDs: []string{}}, want: apitypes.RunProgress{State: StateNone}},
		{name: "non-issue kind with frozen list", in: Input{Kind: "prompt", Status: "running", FrozenIDs: three, Completed: []string{"m1"}}, want: apitypes.RunProgress{State: StateNone, MilestoneDone: 1, MilestoneTotal: 3}},
		{name: "0 of N", in: Input{Kind: "issue", Status: "running", FrozenIDs: three}, want: apitypes.RunProgress{State: StatePercent, Pct: pct(11), MilestoneTotal: 3}},
		{name: "example: 2 of 3 is 70", in: Input{Kind: "issue", Status: "running", FrozenIDs: three, Completed: []string{"m1", "m2"}, InProgress: []string{"m3"}}, want: apitypes.RunProgress{State: StatePercent, Pct: pct(70), MilestoneDone: 2, MilestoneTotal: 3, ActiveMilestoneID: "m3"}},
		{name: "N of N is capped at 99", in: Input{Kind: "issue", Status: "running", FrozenIDs: three, Completed: three}, want: apitypes.RunProgress{State: StatePercent, Pct: pct(99), MilestoneDone: 3, MilestoneTotal: 3}},
		{name: "completed id outside frozen list ignored", in: Input{Kind: "issue", Status: "running", FrozenIDs: three, Completed: []string{"m1", "gone"}}, want: apitypes.RunProgress{State: StatePercent, Pct: pct(40), MilestoneDone: 1, MilestoneTotal: 3}},
		{name: "two in progress picks first frozen", in: Input{Kind: "issue", Status: "running", FrozenIDs: three, InProgress: []string{"m3", "m2"}}, want: apitypes.RunProgress{State: StatePercent, Pct: pct(11), MilestoneTotal: 3, ActiveMilestoneID: "m2"}},
		{name: "id both in progress and completed is not active", in: Input{Kind: "issue", Status: "running", FrozenIDs: three, Completed: []string{"m1"}, InProgress: []string{"m1", "m3"}}, want: apitypes.RunProgress{State: StatePercent, Pct: pct(40), MilestoneDone: 1, MilestoneTotal: 3, ActiveMilestoneID: "m3"}},
		{name: "in-progress id outside frozen list not active", in: Input{Kind: "issue", Status: "running", FrozenIDs: three, InProgress: []string{"gone"}}, want: apitypes.RunProgress{State: StatePercent, Pct: pct(11), MilestoneTotal: 3}},
		{name: "counts survive a waiting state", in: Input{Kind: "issue", Status: "awaiting_input", FrozenIDs: three, Completed: []string{"m1"}, InProgress: []string{"m2"}}, want: apitypes.RunProgress{State: StateWaiting, MilestoneDone: 1, MilestoneTotal: 3, ActiveMilestoneID: "m2"}},
		{name: "unknown status from a newer server is none", in: Input{Kind: "issue", Status: "hibernating", FrozenIDs: three, Completed: []string{"m1"}}, want: apitypes.RunProgress{State: StateNone, MilestoneDone: 1, MilestoneTotal: 3}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Derive(tc.in)
			if tc.isNil {
				if got != nil {
					t.Fatalf("want nil, got %+v", *got)
				}
				return
			}
			if got == nil {
				t.Fatal("got nil")
			}
			if got.State != tc.want.State || got.MilestoneDone != tc.want.MilestoneDone ||
				got.MilestoneTotal != tc.want.MilestoneTotal || got.ActiveMilestoneID != tc.want.ActiveMilestoneID || got.Phase != "" {
				t.Fatalf("got %+v want %+v", *got, tc.want)
			}
			switch {
			case tc.want.Pct == nil && got.Pct != nil:
				t.Fatalf("pct = %d, want nil", *got.Pct)
			case tc.want.Pct != nil && (got.Pct == nil || *got.Pct != *tc.want.Pct):
				t.Fatalf("pct = %v, want %d", got.Pct, *tc.want.Pct)
			}
		})
	}
}

func TestPhase(t *testing.T) {
	tests := map[string]string{
		"":             "",
		"reviewer":     PhaseReview,
		"auditor":      PhaseReview,
		"fact-checker": PhaseReview,
		"architect":    PhaseReview,
		"web-ux":       PhaseReview,
		"tui-ux":       PhaseReview,
		"dba":          PhaseReview,
		"tester":       PhaseValidate,
		"lead":         PhaseImplement,
		"coder":        PhaseImplement,
		"unknown":      PhaseImplement,
		"Reviewer":     PhaseImplement,
		"<b>x</b>":     PhaseImplement,
	}
	for agent, want := range tests {
		if got := Phase(agent); got != want {
			t.Errorf("Phase(%q) = %q, want %q", agent, got, want)
		}
	}
}
