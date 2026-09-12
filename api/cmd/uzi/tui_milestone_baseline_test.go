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

// PRD #1224 M4 — BASELINE CHARACTERIZATION (TUI View() bytes).
//
// This byte-locks TODAY's crew-rail milestone block for the UNATTRIBUTED
// multi-in-progress render, captured on the pre-renderer-change tree (before M6). It is
// the concrete, recorded baseline for the hard back-compat contract (PRD SC2 / Decision
// 8): a run with NO EFFECTIVE ATTRIBUTION (milestones_agents nil) must render EXACTLY as
// today. It is a CHARACTERIZATION test, NOT mutation-pinned (that is M7): it must pass on
// the CURRENT tree AND stay byte-identical after M6, which preserves the unattributed
// render.
//
// Today's behavior it locks: with m2 and m3 BOTH in progress, the crew-rail now-line
// rides ONLY the first in-progress milestone (m2); m3 keeps its bare ◕ in-progress mark
// and gets no now-line.
//
// It asserts through the COMPOSED View().Content, not the unclamped renderMilestones()
// helper, because the composed view clamps every rail line to laneRailWidth (repo memory).
// The golden is the NO_COLOR/Ascii projection of the milestone block: the color profile is
// pinned to Ascii for determinism, so the byte-lock is over the text + shape glyphs (the
// meaning-bearing channel), stable against unrelated palette chrome. The activity age is
// pinned to "0s" by dating the driving frame in the future (relAge floors a not-yet
// timestamp to "0s"), the Go analog of the web test's useNow pin.
//
// D8 baseline: M6 MUST keep this byte-identical for the unattributed case.

// milestoneBlockRegion extracts the crew-rail milestone block from a composed View().Content:
// the left column (before the ▏ divider joinColumns inserts) of the contiguous run of lines
// from the MILESTONES eyebrow through the last non-empty rail line, trailing pad stripped. It
// stops at the first empty left column after the block (the "\n\n" gap before spend/rate meters,
// or the viewport padding), so unrelated rail chrome never enters the golden.
func milestoneBlockRegion(t *testing.T, view string) string {
	t.Helper()
	lines := strings.Split(stripANSI(view), "\n")
	var out []string
	started := false
	for _, ln := range lines {
		left := ln
		if i := strings.Index(ln, "▏"); i >= 0 {
			left = ln[:i]
		}
		left = strings.TrimRight(left, " ")
		if !started {
			if strings.Contains(left, "MILESTONES") {
				started = true
				out = append(out, left)
			}
			continue
		}
		if left == "" {
			break
		}
		out = append(out, left)
	}
	if !started {
		t.Fatalf("no MILESTONES block found in the composed view:\n%s", stripANSI(view))
	}
	return strings.Join(out, "\n")
}

func TestTUIMilestoneBaselineUnattributedMultiInProgress(t *testing.T) {
	runID := "run-1224-tui-baseline"
	// The canonical scenario: three frozen milestones, m1 done, BOTH m2 and m3 in progress at
	// once, and milestones_agents nil (unattributed). MilestonesAgents is left nil explicitly.
	run := apitypes.RunDTO{
		ID: runID, Kind: "issue", Status: "running", Health: "ok", IssueTitle: "Add rate limiting",
		Milestones: []apitypes.Milestone{
			{ID: "m1", Title: "Alpha"}, {ID: "m2", Title: "Beta"}, {ID: "m3", Title: "Gamma"},
		},
		MilestonesCompleted:  []string{"m1"},
		MilestonesInProgress: []string{"m2", "m3"},
		MilestonesAgents:     nil,
	}

	m := tuiTestModel(t, &uzicli.FakeClient{}, runID)
	// Pin the color profile to Ascii (NO_COLOR) so the golden is deterministic text + shape
	// glyphs with no truecolor SGR chrome, matching the repo's NO_COLOR determinism idiom.
	next, _ := m.Update(tea.ColorProfileMsg{Profile: colorprofile.Ascii})
	m = next.(tuiModel)
	// The TUI now-line derives from the crew rail's OWN frames (railActivity → runactivity.Latest),
	// not RunDTO.CurrentActivity — so an Edit tool_use frame by "coder" drives it. Dating it in the
	// future pins relAge to "0s" regardless of when the test renders (a flaky age golden is worse
	// than none). The frame's agent_label is the italic task label the now-line shows.
	at := time.Now().Add(2 * time.Hour)
	agent, label := "coder", "Wire the limiter"
	editPayload := json.RawMessage(`{"name":"Edit","input":{"file_path":"api/internal/limits/window.go"}}`)
	m = applyDetail(m, run, []apitypes.MessageDTO{
		{Seq: 1, Kind: "tool_use", Agent: &agent, AgentLabel: &label, CreatedAt: at, Payload: editPayload},
	})

	got := milestoneBlockRegion(t, m.View().Content)

	// The recorded baseline: m1 ✓ done, m2 ◕ in progress carrying the single now-line (↳ coder ·
	// 0s + the italic label), m3 ◕ in progress BARE (no now-line). The eyebrow micro-bar and the
	// `· m2, m3` in-flight suffix list both in-progress ids (D9).
	const want = "MILESTONES ▰▱▱ 1/3\n" +
		"· m2, m3\n" +
		" ✓ Alpha\n" +
		" ◕ Beta\n" +
		"   ↳ coder · 0s\n" +
		"     Wire the limiter\n" +
		" ◕ Gamma"

	if got != want {
		t.Errorf("PRD #1224 M4 TUI baseline drifted.\n--- got ---\n%s\n--- want ---\n%s\n--- got (quoted) ---\n%q", got, want, got)
	}
}
