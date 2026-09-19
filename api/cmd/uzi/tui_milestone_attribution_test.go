package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// PRD #1224 M6 — TUI per-milestone agent attribution render (crew rail).
//
// These drive real model state through Update/View and assert on the COMPOSED View().Content
// milestone block (milestoneBlockRegion, the same slice the M4 baseline uses — the composed view
// clamps every rail line to laneRailWidth, repo memory). The color profile is pinned to Ascii so
// the golden is deterministic text + shape glyphs. The driving frame is dated in the FUTURE so
// relAge floors the live age to "0s" regardless of when the test runs.

// tuiAttribModel builds a milestone-structured detail model with the given attribution and a single
// tool_use frame whose agent/label drive current_activity (railActivity → the D3 join key). Returns
// the composed milestone block region.
func tuiAttribModel(t *testing.T, run apitypes.RunDTO, frameAgent, frameLabel string) string {
	t.Helper()
	m := tuiTestModel(t, &uzicli.FakeClient{}, run.ID)
	next, _ := m.Update(tea.ColorProfileMsg{Profile: colorprofile.Ascii})
	m = next.(tuiModel)
	at := time.Now().Add(2 * time.Hour) // relAge floors a not-yet timestamp to "0s"
	editPayload := json.RawMessage(`{"name":"Edit","input":{"file_path":"api/internal/limits/window.go"}}`)
	m = applyDetail(m, run, []apitypes.MessageDTO{
		{Seq: 1, Kind: "tool_use", Agent: &frameAgent, AgentLabel: &frameLabel, CreatedAt: at, Payload: editPayload},
	})
	return milestoneBlockRegion(t, m.View().Content)
}

// TestTUIMilestoneAttributionMultiUniqueMatch pins D3's unique-match enrichment: m2 and m3 are both
// in progress and both attributed (declared coder / tester); the live activity's agent is "coder",
// so ONLY m2 (the single lane whose declared agent byte-matches current_activity.agent) carries the
// live age, while its sibling m3 shows its DECLARED role + label with NO age token. Both keep the ◕
// in-progress mark (D9) and the eyebrow micro-bar + `· m2, m3` suffix are unchanged.
func TestTUIMilestoneAttributionMultiUniqueMatch(t *testing.T) {
	runID := "run-1224-tui-multi"
	run := apitypes.RunDTO{
		ID: runID, Kind: "issue", Status: "running", Health: "ok", IssueTitle: "Add rate limiting",
		Milestones: []apitypes.Milestone{
			{ID: "m1", Title: "Alpha"}, {ID: "m2", Title: "Beta"}, {ID: "m3", Title: "Gamma"},
		},
		MilestonesCompleted:  []string{"m1"},
		MilestonesInProgress: []string{"m2", "m3"},
		MilestonesAgents: []apitypes.MilestoneAgent{
			{ID: "m2", Agent: "coder", AgentLabel: "Wire the limiter"},
			{ID: "m3", Agent: "tester", AgentLabel: "Add the sweep"},
		},
	}
	// The frame's own label ("coder busy") is DISTINCT from the declared m2 label so the exact-match
	// assertion proves the declared label is drawn, never the frame's.
	got := tuiAttribModel(t, run, "coder", "coder busy")

	// m2 (coder) is the unique match: ↳ coder · 0s + declared label. m3 (tester) shows role + label
	// but NO age. Both ◕ in progress; eyebrow + `· m2, m3` unchanged from the unattributed baseline.
	const want = "MILESTONES ▰▱▱ 1/3\n" +
		"· m2, m3\n" +
		" ✓ Alpha\n" +
		" ◕ Beta\n" +
		"   ↳ coder · 0s\n" +
		"     Wire the limiter\n" +
		" ◕ Gamma\n" +
		"   ↳ tester\n" +
		"     Add the sweep"

	if got != want {
		t.Errorf("PRD #1224 M6 TUI multi-attributed render drifted.\n--- got ---\n%s\n--- want ---\n%s\n--- got (quoted) ---\n%q", got, want, got)
	}
}

// TestTUIMilestoneAttributionRepeatedRoleSuppressesAge pins D3's repeated-role suppression AND the PRD
// #1353 M1 honesty fix: m2 and m3 are both attributed to the SAME role ("coder"), which is also the live
// activity's agent. Two effective entries share the role, so the unique-match is ambiguous and live age
// is suppressed on BOTH per-milestone owner lines (the lead lacks agent_instance to tell the lanes
// apart). PRD #1353 M1 then adds an unattached eyebrow now-line so the ambiguous LIVE agent stays
// visible (with age) instead of vanishing, while the two owner lines remain quiet (no age).
func TestTUIMilestoneAttributionRepeatedRoleSuppressesAge(t *testing.T) {
	runID := "run-1353-tui-repeated"
	run := apitypes.RunDTO{
		ID: runID, Kind: "issue", Status: "running", Health: "ok", IssueTitle: "Add rate limiting",
		Milestones: []apitypes.Milestone{
			{ID: "m1", Title: "Alpha"}, {ID: "m2", Title: "Beta"}, {ID: "m3", Title: "Gamma"},
		},
		MilestonesCompleted:  []string{"m1"},
		MilestonesInProgress: []string{"m2", "m3"},
		MilestonesAgents: []apitypes.MilestoneAgent{
			{ID: "m2", Agent: "coder", AgentLabel: "Wire the limiter"},
			{ID: "m3", Agent: "coder", AgentLabel: "Add the sweep"},
		},
	}
	got := tuiAttribModel(t, run, "coder", "coder busy")

	// The ambiguous live "coder" now rides an unattached eyebrow now-line (↳ coder · 0s + its own
	// frame label "coder busy"); BOTH per-milestone owner lines still show declared role + label
	// with NO age.
	const want = "MILESTONES ▰▱▱ 1/3\n" +
		"· m2, m3\n" +
		" ↳ coder · 0s\n" +
		"   coder busy\n" +
		" ✓ Alpha\n" +
		" ◕ Beta\n" +
		"   ↳ coder\n" +
		"     Wire the limiter\n" +
		" ◕ Gamma\n" +
		"   ↳ coder\n" +
		"     Add the sweep"

	if got != want {
		t.Errorf("PRD #1353 M1 TUI repeated-role render drifted.\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	// The age appears EXACTLY once — on the eyebrow now-line for the live agent — proving the per-
	// milestone owner lines still suppress the guessed age (a regression that stamped an age onto an
	// owner line would push the count to two).
	if n := strings.Count(got, "0s"); n != 1 {
		t.Errorf("PRD #1353 M1: ambiguous live agent shows once (with age) at the eyebrow while owner lines suppress age; want exactly one \"0s\" token, got %d:\n%s", n, got)
	}
}

// TestTUIMilestoneAttributionNonOwnerLiveAgent is the PRD #1353 M1 regression pin for the crew rail: a
// live agent that matches NO declared owner (a reviewer) must stay visible. m2→coder and m3→tester are
// both attributed and in progress, but the live activity is "reviewer", matching neither owner. So both
// owners render as quiet declared lines (no age) and the live reviewer rides an unattached eyebrow
// now-line (↳ reviewer · 0s + its frame label). Pre-M1 the eyebrow now-line was suppressed whenever
// attribution was present, so the live reviewer vanished — this render is the fix.
func TestTUIMilestoneAttributionNonOwnerLiveAgent(t *testing.T) {
	runID := "run-1353-tui-nonowner"
	run := apitypes.RunDTO{
		ID: runID, Kind: "issue", Status: "running", Health: "ok", IssueTitle: "Add rate limiting",
		Milestones: []apitypes.Milestone{
			{ID: "m1", Title: "Alpha"}, {ID: "m2", Title: "Beta"}, {ID: "m3", Title: "Gamma"},
		},
		MilestonesCompleted:  []string{"m1"},
		MilestonesInProgress: []string{"m2", "m3"},
		MilestonesAgents: []apitypes.MilestoneAgent{
			{ID: "m2", Agent: "coder", AgentLabel: "Wire the limiter"},
			{ID: "m3", Agent: "tester", AgentLabel: "Add the sweep"},
		},
	}
	// The live frame is a reviewer — matches neither declared owner, so it must show as the unattached
	// eyebrow now-line rather than vanish.
	got := tuiAttribModel(t, run, "reviewer", "reviewing the diff")

	const want = "MILESTONES ▰▱▱ 1/3\n" +
		"· m2, m3\n" +
		" ↳ reviewer · 0s\n" +
		"   reviewing the diff\n" +
		" ✓ Alpha\n" +
		" ◕ Beta\n" +
		"   ↳ coder\n" +
		"     Wire the limiter\n" +
		" ◕ Gamma\n" +
		"   ↳ tester\n" +
		"     Add the sweep"

	if got != want {
		t.Errorf("PRD #1353 M1 TUI non-owner live agent render drifted.\n--- got ---\n%s\n--- want ---\n%s\n--- got (quoted) ---\n%q", got, want, got)
	}
	// The explicit honesty guarantee: the live reviewer is present as an unattached now-line with age.
	if !strings.Contains(got, "↳ reviewer · 0s") {
		t.Errorf("PRD #1353 M1: a live non-owner agent must appear as an unattached `↳ reviewer · <age>` now-line:\n%s", got)
	}
}

// TestTUIMilestoneLanesRender is the PRD #1353 M6 crew-rail LIVE LANES golden: when the run has lanes
// (MilestonesLive), the lanes are the authoritative live display. Under m2 (a declared owner) the rail
// draws a QUIET owner line (no age) THEN the live lane line (↳ coder · 0s + the tool/detail + italic
// label); under m3 (no declared owner) it draws one lane line per lane — two reviewer lanes here,
// distinguished by tool arg + label. The eyebrow now-line is SUPPRESSED (the frame's "coder busy"
// must not appear). The frame is future-dated so relAge floors the age to "0s".
func TestTUIMilestoneLanesRender(t *testing.T) {
	runID := "run-1353-tui-lanes"
	at := time.Now().Add(2 * time.Hour) // relAge floors a not-yet timestamp to "0s"
	run := apitypes.RunDTO{
		ID: runID, Kind: "issue", Status: "running", Health: "ok", IssueTitle: "Add rate limiting",
		Milestones: []apitypes.Milestone{
			{ID: "m1", Title: "Alpha"}, {ID: "m2", Title: "Beta"}, {ID: "m3", Title: "Gamma"},
		},
		MilestonesCompleted:  []string{"m1"},
		MilestonesInProgress: []string{"m2", "m3"},
		// m2 has a declared owner (a QUIET owner line beside its lane); m3 has none (lanes only).
		MilestonesAgents: []apitypes.MilestoneAgent{
			{ID: "m2", Agent: "coder", AgentLabel: "Wire the limiter"},
		},
		MilestonesLive: []apitypes.MilestoneLive{
			{MilestoneID: "m2", Lanes: []apitypes.MilestoneLane{
				{Agent: "coder", AgentInstance: "toolu_1", AgentLabel: "Wire the limiter", Tool: "Edit", Detail: "window.go", At: at},
			}},
			{MilestoneID: "m3", Lanes: []apitypes.MilestoneLane{
				{Agent: "reviewer", AgentInstance: "toolu_2", AgentLabel: "Review A", Tool: "Read", Detail: "a.go", At: at},
				{Agent: "reviewer", AgentInstance: "toolu_3", AgentLabel: "Review B", Tool: "Read", Detail: "b.go", At: at},
			}},
		},
	}
	m := tuiTestModel(t, &uzicli.FakeClient{}, runID)
	next, _ := m.Update(tea.ColorProfileMsg{Profile: colorprofile.Ascii})
	m = next.(tuiModel)
	// A driving frame supplies current_activity; the lanes branch SUPPRESSES the eyebrow now-line, so
	// the frame's own label "coder busy" must NOT appear anywhere in the block.
	frameAgent, frameLabel := "coder", "coder busy"
	editPayload := json.RawMessage(`{"name":"Edit","input":{"file_path":"api/internal/limits/window.go"}}`)
	m = applyDetail(m, run, []apitypes.MessageDTO{
		{Seq: 1, Kind: "tool_use", Agent: &frameAgent, AgentLabel: &frameLabel, CreatedAt: at, Payload: editPayload},
	})
	got := milestoneBlockRegion(t, m.View().Content)

	const want = "MILESTONES ▰▱▱ 1/3\n" +
		"· m2, m3\n" +
		" ✓ Alpha\n" +
		" ◕ Beta\n" +
		"   ↳ coder\n" +
		"     Wire the limiter\n" +
		"   ↳ coder · 0s\n" +
		"     Edit window.go\n" +
		"     Wire the limiter\n" +
		" ◕ Gamma\n" +
		"   ↳ reviewer · 0s\n" +
		"     Read a.go\n" +
		"     Review A\n" +
		"   ↳ reviewer · 0s\n" +
		"     Read b.go\n" +
		"     Review B"

	if got != want {
		t.Errorf("PRD #1353 M6 TUI lanes render drifted.\n--- got ---\n%s\n--- want ---\n%s\n--- got (quoted) ---\n%q", got, want, got)
	}
	// The eyebrow now-line is suppressed in the lanes branch: the frame's own label never appears.
	if strings.Contains(got, "coder busy") {
		t.Errorf("PRD #1353 M6: the lanes branch must suppress the current_activity eyebrow now-line, but the frame label appeared:\n%s", got)
	}
}
