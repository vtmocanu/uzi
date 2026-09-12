package main

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// PRD #1224 M6 — CLI `uzi run get` per-milestone agent attribution.
//
// The ATTRIBUTED case emits one `NOW <id>` row per effective-attributed in-progress milestone
// (declared role + label; live tool/age only on the D3 unique-matching lane) IN PLACE OF the single
// global NOW row; the UNATTRIBUTED case keeps the single global NOW row byte-for-byte (the M4
// baseline locks that). current_activity is dated in the future so relAge floors the age to
// "0s ago" deterministically.

// countNowRows counts the NOW-family rows in a rendered `run get`: the table left-aligns every key
// at column 0, so a NOW row (global "NOW" or attributed "NOW <id>") is a line starting with "NOW".
// The M4 CLI baseline's region slice stopped at the FIRST NOW line, so a spurious extra NOW row
// after it would have slipped past it — this full-output scan is the tightening that closes that gap.
func countNowRows(out string) int {
	n := 0
	for _, ln := range strings.Split(out, "\n") {
		if strings.HasPrefix(ln, "NOW") {
			n++
		}
	}
	return n
}

func TestMilestoneNowRowsMultiUniqueMatch(t *testing.T) {
	at := time.Now().Add(2 * time.Hour) // relAge floors a not-yet timestamp to "0s"
	r := apitypes.RunDTO{
		ID: "run-1224-cli-multi", Kind: "issue", Status: "running", IssueTitle: "Add rate limiting",
		Milestones: []apitypes.Milestone{
			{ID: "m1", Title: "Alpha"}, {ID: "m2", Title: "Beta"}, {ID: "m3", Title: "Gamma"},
		},
		MilestonesCompleted:  []string{"m1"},
		MilestonesInProgress: []string{"m2", "m3"},
		MilestonesAgents: []apitypes.MilestoneAgent{
			{ID: "m2", Agent: "coder", AgentLabel: "Wire the limiter"},
			{ID: "m3", Agent: "tester", AgentLabel: "Add the sweep"},
		},
		// The live activity's agent is "coder" so m2 is the D3 unique-matching lane (tool/age); its
		// declared role/label ("coder"/"Wire the limiter") are shown, NOT the activity's own label.
		CurrentActivity: &apitypes.RunActivity{
			Agent: "coder", AgentLabel: "coder busy", Tool: "Edit",
			Detail: "api/internal/limits/window.go", At: at, Seq: 12,
		},
	}

	got := milestoneNowRows(r)
	want := [][]string{
		{"NOW m2", "coder · Wire the limiter · Edit api/internal/limits/window.go · 0s ago"},
		{"NOW m3", "tester · Add the sweep"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("PRD #1224 M6 milestoneNowRows drifted.\n--- got ---\n%#v\n--- want ---\n%#v", got, want)
	}

	// Through the composed render: the two NOW <id> rows appear IN PLACE OF the single global NOW.
	var buf bytes.Buffer
	p := uzicli.NewPrinter(&buf, false, false, true, false) // non-tty, non-json, no colour
	if err := renderRunDetail(p, r); err != nil {
		t.Fatalf("renderRunDetail: %v", err)
	}
	out := buf.String()
	if n := countNowRows(out); n != 2 {
		t.Errorf("PRD #1224 M6: attributed `run get` must emit exactly 2 NOW rows (one per attributed milestone), got %d:\n%s", n, out)
	}
	for _, sub := range []string{
		"NOW m2",
		"coder · Wire the limiter · Edit api/internal/limits/window.go · 0s ago",
		"NOW m3",
		"tester · Add the sweep",
	} {
		if !strings.Contains(out, sub) {
			t.Errorf("PRD #1224 M6: attributed `run get` output missing %q:\n%s", sub, out)
		}
	}
}

// TestMilestoneNowRowsRepeatedRoleSuppressesTool pins D3's repeated-role suppression on the CLI: two
// milestones attributed to the SAME role that also matches the activity means the unique-match is
// ambiguous, so NEITHER row carries the live tool/age — each shows declared role + label only.
func TestMilestoneNowRowsRepeatedRoleSuppressesTool(t *testing.T) {
	at := time.Now().Add(2 * time.Hour)
	r := apitypes.RunDTO{
		ID: "run-1224-cli-repeated", Kind: "issue", Status: "running", IssueTitle: "Add rate limiting",
		Milestones: []apitypes.Milestone{
			{ID: "m1", Title: "Alpha"}, {ID: "m2", Title: "Beta"}, {ID: "m3", Title: "Gamma"},
		},
		MilestonesCompleted:  []string{"m1"},
		MilestonesInProgress: []string{"m2", "m3"},
		MilestonesAgents: []apitypes.MilestoneAgent{
			{ID: "m2", Agent: "coder", AgentLabel: "Wire the limiter"},
			{ID: "m3", Agent: "coder", AgentLabel: "Add the sweep"},
		},
		CurrentActivity: &apitypes.RunActivity{
			Agent: "coder", AgentLabel: "coder busy", Tool: "Edit",
			Detail: "api/internal/limits/window.go", At: at, Seq: 12,
		},
	}
	got := milestoneNowRows(r)
	want := [][]string{
		{"NOW m2", "coder · Wire the limiter"},
		{"NOW m3", "coder · Add the sweep"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("PRD #1224 D3 CLI repeated-role render drifted.\n--- got ---\n%#v\n--- want ---\n%#v", got, want)
	}
}

// TestRenderRunDetailUnattributedExactlyOneNowRow closes the known narrowing in the M4 CLI baseline
// (its region slice stopped at the first NOW, so an extra NOW row after it would slip through): with
// MilestonesAgents nil and two in-progress milestones, the whole `run get` output must carry EXACTLY
// ONE NOW row — the single GLOBAL current_activity row — and no per-milestone `NOW <id>` row at all.
func TestRenderRunDetailUnattributedExactlyOneNowRow(t *testing.T) {
	at := time.Now().Add(2 * time.Hour)
	r := apitypes.RunDTO{
		ID: "run-1224-cli-unattributed", Kind: "issue", Status: "running", IssueTitle: "Add rate limiting",
		Milestones: []apitypes.Milestone{
			{ID: "m1", Title: "Alpha"}, {ID: "m2", Title: "Beta"}, {ID: "m3", Title: "Gamma"},
		},
		MilestonesCompleted:  []string{"m1"},
		MilestonesInProgress: []string{"m2", "m3"},
		MilestonesAgents:     nil,
		CurrentActivity: &apitypes.RunActivity{
			Agent: "coder", AgentLabel: "Wire the limiter", Tool: "Edit",
			Detail: "api/internal/limits/window.go", At: at, Seq: 12,
		},
	}
	var buf bytes.Buffer
	p := uzicli.NewPrinter(&buf, false, false, true, false)
	if err := renderRunDetail(p, r); err != nil {
		t.Fatalf("renderRunDetail: %v", err)
	}
	out := buf.String()
	if n := countNowRows(out); n != 1 {
		t.Errorf("PRD #1224 D8: unattributed `run get` must emit exactly ONE global NOW row, got %d:\n%s", n, out)
	}
	// And it is the GLOBAL row (no milestone id), carrying the activity's OWN label — never a
	// per-milestone `NOW <id>` row.
	if strings.Contains(out, "NOW m2") || strings.Contains(out, "NOW m3") {
		t.Errorf("PRD #1224 D8: unattributed `run get` must not emit a per-milestone NOW row:\n%s", out)
	}
	if !strings.Contains(out, "coder · Wire the limiter · Edit api/internal/limits/window.go · 0s ago") {
		t.Errorf("PRD #1224 D8: unattributed `run get` lost the global NOW row content:\n%s", out)
	}
}
