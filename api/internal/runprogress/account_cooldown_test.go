package runprogress

import "testing"

func TestAccountCooldownProgress(t *testing.T) {
	in := Input{Kind: "issue", Status: "running", Health: "ok", FrozenIDs: []string{"one", "two"}, Completed: []string{"one"}}
	healthy := Derive(in)
	if healthy.State != StatePercent || healthy.Pct == nil || *healthy.Pct != 55 {
		t.Fatalf("healthy progress: %+v", healthy)
	}
	in.Health = "waiting_account_cooldown"
	waiting := Derive(in)
	if waiting.State != StateWaiting || waiting.Pct != nil || waiting.MilestoneDone != 1 || waiting.MilestoneTotal != 2 {
		t.Fatalf("cooldown progress: %+v", waiting)
	}
	in.Health = "ok"
	resumed := Derive(in)
	if resumed.State != StatePercent || resumed.Pct == nil || *resumed.Pct != 55 {
		t.Fatalf("resumed progress: %+v", resumed)
	}
	in.Health = "stalled"
	fault := Derive(in)
	if fault.State != StateStalled || fault.Pct != nil {
		t.Fatalf("fault progress: %+v", fault)
	}
}
