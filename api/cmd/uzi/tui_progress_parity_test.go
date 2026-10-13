package main

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// PRD #2602 parity: the server-derived progress counts and active milestone equal what the TUI
// computes today from the same run (milestoneProgress / milestoneInProgress), so the surfaces
// cannot disagree. The shared fixture is also read by the runprogress and web tests.
func TestProgressParityFixture(t *testing.T) {
	raw, err := os.ReadFile("../../../fixtures/run-progress/parity.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name                 string                `json:"name"`
		Kind                 string                `json:"kind"`
		Status               string                `json:"status"`
		Milestones           []apitypes.Milestone  `json:"milestones"`
		MilestonesCompleted  []string              `json:"milestones_completed"`
		MilestonesInProgress []string              `json:"milestones_in_progress"`
		Progress             *apitypes.RunProgress `json:"progress"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, c := range cases {
		if c.Progress == nil {
			continue
		}
		checked++
		run := apitypes.RunDTO{Kind: c.Kind, Status: c.Status, Milestones: c.Milestones,
			MilestonesCompleted: c.MilestonesCompleted, MilestonesInProgress: c.MilestonesInProgress}
		done, total, _ := milestoneProgress(run)
		if done != c.Progress.MilestoneDone || total != c.Progress.MilestoneTotal {
			t.Errorf("%s: TUI counts %d/%d, progress %d/%d", c.Name, done, total, c.Progress.MilestoneDone, c.Progress.MilestoneTotal)
		}
		if id, _ := milestoneInProgress(run); id != c.Progress.ActiveMilestoneID {
			t.Errorf("%s: TUI active %q, progress %q", c.Name, id, c.Progress.ActiveMilestoneID)
		}
	}
	if checked == 0 {
		t.Fatal("fixture carried no case with progress")
	}
}
