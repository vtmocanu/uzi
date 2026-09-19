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
	// PRD #1353 M1: the D3 unique-matching (live) lane m2 keeps its `NOW m2` live row; the idle
	// declared owner m3 demotes to a QUIET `OWNER m3` tag row (role + label only, no NOW, no age).
	want := [][]string{
		{"NOW m2", "coder · Wire the limiter · Edit api/internal/limits/window.go · 0s ago"},
		{"OWNER m3", "tester · Add the sweep"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("PRD #1353 M1 milestoneNowRows drifted.\n--- got ---\n%#v\n--- want ---\n%#v", got, want)
	}

	// Through the composed render: exactly ONE NOW row (the live NOW m2). The global nowRow is
	// SKIPPED because "coder" uniquely matches m2 — its live info already rides that row, so
	// emitting the global too would duplicate it. The idle owner reads as OWNER m3, never NOW m3.
	var buf bytes.Buffer
	p := uzicli.NewPrinter(&buf, false, false, true, false) // non-tty, non-json, no colour
	if err := renderRunDetail(p, r); err != nil {
		t.Fatalf("renderRunDetail: %v", err)
	}
	out := buf.String()
	if n := countNowRows(out); n != 1 {
		t.Errorf("PRD #1353 M1: a uniquely-matched live agent must emit exactly 1 NOW row (the live NOW <id>, global skipped), got %d:\n%s", n, out)
	}
	for _, sub := range []string{
		"NOW m2",
		"coder · Wire the limiter · Edit api/internal/limits/window.go · 0s ago",
		"OWNER m3",
		"tester · Add the sweep",
	} {
		if !strings.Contains(out, sub) {
			t.Errorf("PRD #1353 M1: attributed `run get` output missing %q:\n%s", sub, out)
		}
	}
	if strings.Contains(out, "NOW m3") {
		t.Errorf("PRD #1353 M1: an idle declared owner must read as `OWNER m3`, never `NOW m3`:\n%s", out)
	}
}

// TestMilestoneNowRowsRepeatedRoleShowsGlobalNow pins D3's repeated-role suppression AND the PRD #1353
// M1 honesty fix on the CLI: two milestones attributed to the SAME role that also matches the activity
// means the unique-match is ambiguous, so NEITHER milestone row carries the live tool/age — each demotes
// to a QUIET `OWNER <id>` tag row (declared role + label only). Because no `NOW <id>` row then carries
// the live agent, the composed render emits the single GLOBAL NOW row so the live "coder"/tool/age stays
// visible (honesty: the live agent is never suppressed).
func TestMilestoneNowRowsRepeatedRoleShowsGlobalNow(t *testing.T) {
	at := time.Now().Add(2 * time.Hour)
	r := apitypes.RunDTO{
		ID: "run-1353-cli-repeated", Kind: "issue", Status: "running", IssueTitle: "Add rate limiting",
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
		{"OWNER m2", "coder · Wire the limiter"},
		{"OWNER m3", "coder · Add the sweep"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("PRD #1353 M1 CLI repeated-role render drifted.\n--- got ---\n%#v\n--- want ---\n%#v", got, want)
	}

	// Through the composed render: NO `NOW <id>` row carries the ambiguous live agent, so the single
	// global NOW row appears (countNowRows == 1) carrying "coder" + tool + age — the honesty guarantee.
	var buf bytes.Buffer
	p := uzicli.NewPrinter(&buf, false, false, true, false) // non-tty, non-json, no colour
	if err := renderRunDetail(p, r); err != nil {
		t.Fatalf("renderRunDetail: %v", err)
	}
	out := buf.String()
	if n := countNowRows(out); n != 1 {
		t.Errorf("PRD #1353 M1: an ambiguous repeated role must show the single global NOW row (countNowRows == 1), got %d:\n%s", n, out)
	}
	if strings.Contains(out, "NOW m2") || strings.Contains(out, "NOW m3") {
		t.Errorf("PRD #1353 M1: ambiguous owners must read as `OWNER <id>`, never `NOW <id>`:\n%s", out)
	}
	if !strings.Contains(out, "coder · coder busy · Edit api/internal/limits/window.go · 0s ago") {
		t.Errorf("PRD #1353 M1: the global NOW row must carry the live coder/tool/age:\n%s", out)
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

// TestRenderRunDetailNonOwnerLiveAgentShowsGlobalNow is the PRD #1353 M1 regression pin: a live agent
// that matches NO declared owner (a reviewer/tester) must stay visible. m2→coder and m3→tester are both
// in progress, but the live activity is "reviewer", which matches neither owner. So both milestones read
// as QUIET `OWNER <id>` tag rows, and the single GLOBAL NOW row is emitted carrying "reviewer" + tool +
// age. Pre-fix, milestoneNowRows emitted `NOW m2`/`NOW m3` and the `else if` suppressed the global, so
// the live reviewer vanished entirely — this test fails on that code and passes after M1.
func TestRenderRunDetailNonOwnerLiveAgentShowsGlobalNow(t *testing.T) {
	at := time.Now().Add(2 * time.Hour)
	r := apitypes.RunDTO{
		ID: "run-1353-cli-nonowner", Kind: "issue", Status: "running", IssueTitle: "Add rate limiting",
		Milestones: []apitypes.Milestone{
			{ID: "m1", Title: "Alpha"}, {ID: "m2", Title: "Beta"}, {ID: "m3", Title: "Gamma"},
		},
		MilestonesCompleted:  []string{"m1"},
		MilestonesInProgress: []string{"m2", "m3"},
		MilestonesAgents: []apitypes.MilestoneAgent{
			{ID: "m2", Agent: "coder", AgentLabel: "Wire the limiter"},
			{ID: "m3", Agent: "tester", AgentLabel: "Add the sweep"},
		},
		// The live agent is a reviewer — it matches NO declared owner, so it must not vanish.
		CurrentActivity: &apitypes.RunActivity{
			Agent: "reviewer", AgentLabel: "Review the diff", Tool: "Read",
			Detail: "api/internal/limits/window.go", At: at, Seq: 12,
		},
	}
	var buf bytes.Buffer
	p := uzicli.NewPrinter(&buf, false, false, true, false) // non-tty, non-json, no colour
	if err := renderRunDetail(p, r); err != nil {
		t.Fatalf("renderRunDetail: %v", err)
	}
	out := buf.String()
	// Both declared owners read as quiet OWNER tag rows, not live NOW rows.
	for _, sub := range []string{"OWNER m2", "OWNER m3"} {
		if !strings.Contains(out, sub) {
			t.Errorf("PRD #1353 M1: idle declared owner missing quiet tag row %q:\n%s", sub, out)
		}
	}
	if strings.Contains(out, "NOW m2") || strings.Contains(out, "NOW m3") {
		t.Errorf("PRD #1353 M1: an idle declared owner must read as `OWNER <id>`, never `NOW <id>`:\n%s", out)
	}
	// The live non-owner reviewer is visible on the single global NOW row.
	if n := countNowRows(out); n != 1 {
		t.Errorf("PRD #1353 M1: a live non-owner agent must show the single global NOW row (countNowRows == 1), got %d:\n%s", n, out)
	}
	if !strings.Contains(out, "reviewer · Review the diff · Read api/internal/limits/window.go · 0s ago") {
		t.Errorf("PRD #1353 M1: the global NOW row must carry the live reviewer/tool/age (the honesty fix):\n%s", out)
	}
}
