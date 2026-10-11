package handler

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// runProgressParityFile is the recorded cross-surface parity fixture (PRD #2602).
// The TUI test in api/cmd/uzi and the web test in web/src/lib read the SAME file and
// assert that progress.milestone_done / active_milestone_id agree with their own
// milestone counters, so the surfaces cannot disagree. Recorded, not authored: on a
// mismatch this test prints the exact JSON to copy into the file (there is no
// -update flag). Run with -count=1 after a fixture-only edit.
const runProgressParityFile = "../../../fixtures/run-progress/parity.json"

type runProgressParityCase struct {
	Name                 string                `json:"name"`
	Kind                 string                `json:"kind"`
	Status               string                `json:"status"`
	Health               string                `json:"health"`
	IsPlanning           bool                  `json:"is_planning"`
	Milestones           []apitypes.Milestone  `json:"milestones"`
	MilestonesCompleted  []string              `json:"milestones_completed"`
	MilestonesInProgress []string              `json:"milestones_in_progress"`
	Progress             *apitypes.RunProgress `json:"progress"`
}

func TestRunProgressParityFixture(t *testing.T) {
	const frozen = `[{"id":"m1","title":"One"},{"id":"m2","title":"Two"},{"id":"m3","title":"Three"}]`
	cases := []struct {
		name string
		run  store.Run
	}{
		{"frozen list, nothing reported", store.Run{Kind: "issue", Status: "running", Health: "ok", IterationCount: 1, MilestonesFrozen: []byte(frozen)}},
		{"two of three done, third in progress", store.Run{Kind: "issue", Status: "running", Health: "ok", IterationCount: 1, MilestonesFrozen: []byte(frozen),
			MilestonesCompleted: []byte(`["m1","m2"]`), MilestonesInProgress: []byte(`["m3"]`)}},
		{"completed id outside the frozen list is ignored", store.Run{Kind: "issue", Status: "running", Health: "ok", IterationCount: 1, MilestonesFrozen: []byte(frozen),
			MilestonesCompleted: []byte(`["m1","dropped"]`), MilestonesInProgress: []byte(`["m2"]`)}},
		{"in-progress id that is also completed is not active", store.Run{Kind: "issue", Status: "running", Health: "ok", IterationCount: 1, MilestonesFrozen: []byte(frozen),
			MilestonesCompleted: []byte(`["m1"]`), MilestonesInProgress: []byte(`["m1","m3"]`)}},
		{"two in progress, first in frozen order is active", store.Run{Kind: "issue", Status: "running", Health: "ok", IterationCount: 1, MilestonesFrozen: []byte(frozen),
			MilestonesInProgress: []byte(`["m3","m2"]`)}},
		{"all done is capped below 100", store.Run{Kind: "issue", Status: "running", Health: "ok", IterationCount: 1, MilestonesFrozen: []byte(frozen),
			MilestonesCompleted: []byte(`["m1","m2","m3"]`)}},
		{"empty frozen list", store.Run{Kind: "issue", Status: "running", Health: "ok", IterationCount: 1, MilestonesFrozen: []byte(`[]`),
			MilestonesCompleted: []byte(`["m1"]`), MilestonesInProgress: []byte(`["m1"]`)}},
		{"no frozen list", store.Run{Kind: "issue", Status: "running", Health: "ok", IterationCount: 1}},
		{"awaiting input outranks stalled health", store.Run{Kind: "issue", Status: "awaiting_input", Health: "stalled", IterationCount: 1, MilestonesFrozen: []byte(frozen),
			MilestonesCompleted: []byte(`["m1"]`)}},
		{"plan gate waits", store.Run{Kind: "issue", Status: "awaiting_approval", Health: "ok", MilestonesCandidate: []byte(frozen)}},
		{"limit wait is parked", store.Run{Kind: "issue", Status: "limit_wait", Health: "ok", IterationCount: 1, MilestonesFrozen: []byte(frozen)}},
		{"claimed is queued", store.Run{Kind: "issue", Status: "claimed", Health: "ok"}},
		{"looping health is stalled", store.Run{Kind: "issue", Status: "running", Health: "looping", IterationCount: 1, MilestonesFrozen: []byte(frozen),
			MilestonesCompleted: []byte(`["m1"]`), MilestonesInProgress: []byte(`["m2"]`)}},
		{"planning turn", store.Run{Kind: "issue", Status: "running", Health: "ok"}},
		{"non-issue kind with a frozen list shows none", store.Run{Kind: "mr_rework", Status: "running", Health: "ok", IterationCount: 1, MilestonesFrozen: []byte(frozen)}},
	}

	out := make([]runProgressParityCase, 0, len(cases))
	for _, c := range cases {
		c.run.ID = uuid.New()
		dto := runToDTO(c.run, "normal", 0, 0, 0, dtoTestNow, 0)
		out = append(out, runProgressParityCase{
			Name: c.name, Kind: dto.Kind, Status: dto.Status, Health: dto.Health, IsPlanning: dto.IsPlanning,
			Milestones: dto.Milestones, MilestonesCompleted: dto.MilestonesCompleted,
			MilestonesInProgress: dto.MilestonesInProgress, Progress: dto.Progress,
		})
	}
	got, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(filepath.FromSlash(runProgressParityFile))
	if err != nil {
		t.Fatalf("parity fixture unreadable: %v -- record it from this exact output:\n%s", err, got)
	}
	if !bytes.Equal(bytes.TrimRight(want, "\n"), got) {
		t.Errorf("fixtures/run-progress/parity.json is stale -- re-record it from this exact output (recorded, not authored):\n%s", got)
	}
}
