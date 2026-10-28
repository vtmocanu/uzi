package main

import (
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

func TestAccountCooldownPresentation(t *testing.T) {
	now := time.Now()
	future, expired := now.Add(time.Hour), now.Add(-time.Hour)
	class := apitypes.AccountCooldownClass("overload")
	for _, state := range []struct {
		status   string
		revising bool
	}{
		{"claimed", false}, {"running", false},
		{"awaiting_approval", false}, {"awaiting_approval", true},
	} {
		for _, metadata := range []struct {
			name     string
			class    *apitypes.AccountCooldownClass
			at       *time.Time
			detailed bool
		}{
			{"valid", &class, &future, true},
			{"expired", &class, &expired, false},
			{"missing class", nil, &future, false},
			{"missing timestamp", &class, nil, false},
			{"missing both", nil, nil, false},
		} {
			name := state.status + "/" + metadata.name
			if state.revising {
				name += "/revising"
			}
			t.Run(name, func(t *testing.T) {
				r := apitypes.RunDTO{
					ID: "cooldown-presentation", Kind: "issue",
					Status: state.status, IsRevising: state.revising,
					Health:               "waiting_account_cooldown",
					HealthReason:         sp(apitypes.AccountCooldownReason),
					AccountCooldownClass: metadata.class, AccountCooldownEligibleAt: metadata.at,
					Progress:        &apitypes.RunProgress{State: "waiting"},
					CurrentActivity: &apitypes.RunActivity{Agent: "coder", Tool: "Edit", Detail: "old-completed-tool.go", At: now.Add(-time.Minute)},
				}
				for _, attributed := range []bool{false, true} {
					if attributed {
						r.Milestones = []apitypes.Milestone{{ID: "m1", Title: "Implement"}}
						r.MilestonesInProgress = []string{"m1"}
						r.MilestonesAgents = []apitypes.MilestoneAgent{{ID: "m1", Agent: "coder"}}
					}
					out := renderDetail(t, r)
					if got := lineWith(t, out, "PROGRESS"); !strings.Contains(got, "waiting on account") || strings.Contains(got, "on you") || strings.Contains(got, "plan gate") {
						t.Errorf("CLI progress = %q", got)
					}
					if countNowRows(out) != 0 || strings.Contains(out, "old-completed-tool.go") {
						t.Errorf("retained activity shown during cooldown (attributed=%v):\n%s", attributed, out)
					}
					if metadata.detailed && !strings.Contains(out, apitypes.AccountCooldownLine(class, future)) {
						t.Errorf("missing typed cooldown detail:\n%s", out)
					}
					if !metadata.detailed && strings.Contains(out, "try after") {
						t.Errorf("incomplete/expired cooldown has retry detail:\n%s", out)
					}
				}
				got := stripANSI(ownerDetail(t, r).renderProgress())
				if !strings.Contains(got, "waiting on account") || strings.Contains(got, "on you") || strings.Contains(got, "plan gate") {
					t.Errorf("TUI progress = %q", got)
				}
				// Fresh health restores normal activity even if typed cooldown fields remain.
				r.Health = "ok"
				if out := renderDetail(t, r); !strings.Contains(out, "old-completed-tool.go") || countNowRows(out) != 1 {
					t.Errorf("normal activity lost after fresh health:\n%s", out)
				}
			})
		}
	}
}

func TestAccountCooldownPresentationControls(t *testing.T) {
	for _, tc := range []struct {
		status, health, state, want string
	}{
		{"awaiting_approval", "ok", "waiting", "waits on you"},
		{"awaiting_input", "waiting_account_cooldown", "waiting", "waits on you"},
		{"awaiting_followup", "ok", "waiting", "waits on you"},
		{"running", "stalled", "stalled", "stalled"},
		{"running", "looping", "stalled", "stalled"},
		{"running", "waiting_account_cooldown", "stalled", "stalled"},
		{"queued", "waiting_account_cooldown", "queued", "queued"},
		{"claimed", "ok", "queued", "queued"},
	} {
		t.Run(tc.status+"/"+tc.health+"/"+tc.state, func(t *testing.T) {
			r := apitypes.RunDTO{ID: "control", Status: tc.status, Health: tc.health, Progress: &apitypes.RunProgress{State: tc.state}}
			cli := lineWith(t, renderDetail(t, r), "PROGRESS")
			tui := stripANSI(ownerDetail(t, r).renderProgress())
			for _, got := range []string{cli, tui} {
				if !strings.Contains(got, tc.want) || strings.Contains(got, "waiting on account") {
					t.Errorf("progress = %q, want %q", got, tc.want)
				}
			}
			switch tc.status {
			case "awaiting_approval":
				if !strings.Contains(cli, "plan gate") || !strings.Contains(tui, "plan gate") {
					t.Error("lost genuine plan gate")
				}
			case "awaiting_input":
				if !strings.Contains(cli, "question") || !strings.Contains(tui, "question") {
					t.Error("lost genuine question")
				}
			case "awaiting_followup":
				if !strings.Contains(cli, "follow-up") || !strings.Contains(tui, "follow-up") {
					t.Error("lost genuine follow-up")
				}
			}
		})
	}
	for _, status := range []string{"queued", "completed", "failed", "cancelled"} {
		t.Run(status+"/activity", func(t *testing.T) {
			r := apitypes.RunDTO{ID: "activity-control", Status: status, Health: "waiting_account_cooldown",
				CurrentActivity: &apitypes.RunActivity{Agent: "lead", Tool: "Read", Detail: "activity-control.go", At: time.Now()},
			}
			out := renderDetail(t, r)
			want := 0
			if status == "queued" {
				want = 1
			}
			if countNowRows(out) != want {
				t.Errorf("%s activity changed:\n%s", status, out)
			}
		})
	}
}
