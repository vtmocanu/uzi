package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// PRD #1224 M4 — BASELINE CHARACTERIZATION (CLI `uzi run get` text).
//
// This locks TODAY's `uzi run get` detail render for the UNATTRIBUTED multi-in-progress
// case, captured on the pre-renderer-change tree (before M6). It is the concrete, recorded
// baseline for the hard back-compat contract (PRD SC2 / Decision 8): a run with NO
// EFFECTIVE ATTRIBUTION (milestones_agents nil) must render EXACTLY as today. It is a
// CHARACTERIZATION test, NOT mutation-pinned (that is M7): it must pass on the CURRENT tree
// AND stay byte-identical after M6, which preserves the unattributed render.
//
// Today's behavior it locks: the milestone rows render in frozen order, then a SINGLE
// global NOW row derived from current_activity with NO milestone id — there is NO
// per-milestone `NOW <id>` row (that is what M6 adds only for the ATTRIBUTED case). The
// activity age is pinned to "0s ago" by dating current_activity in the future (relAge
// floors a not-yet timestamp to "0s"), the deterministic analog of the web useNow pin.
//
// D8 baseline: M6 MUST keep the unattributed `run get` output byte-for-byte.

// milestoneNowRegion slices the milestone rows + the single NOW row out of a `run get`
// render: the contiguous lines from the MILESTONES summary through the NOW row inclusive.
// The surrounding table chrome (ID/STATUS above, WAIT_ON_LIMIT/MR_REWORK below) is excluded
// so the golden byte-locks the milestone render without pinning unrelated rows.
func milestoneNowRegion(t *testing.T, out string) string {
	t.Helper()
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	start, end := -1, -1
	for i, ln := range lines {
		if start < 0 && strings.HasPrefix(ln, "MILESTONES") {
			start = i
		}
		if start >= 0 && strings.HasPrefix(strings.TrimLeft(ln, " "), "NOW ") {
			end = i
			break
		}
	}
	if start < 0 || end < 0 {
		t.Fatalf("no MILESTONES…NOW region found in `run get` output:\n%s", out)
	}
	return strings.Join(lines[start:end+1], "\n")
}

func TestRenderRunDetailMilestoneBaselineUnattributed(t *testing.T) {
	// current_activity dated in the future so relAge floors to "0s ago" deterministically.
	at := time.Now().Add(2 * time.Hour)
	// The canonical scenario: three frozen milestones, m1 done, BOTH m2 and m3 in progress at
	// once, an activity present, and milestones_agents nil (unattributed). MilestonesAgents is
	// left nil explicitly.
	r := apitypes.RunDTO{
		ID: "run-1224-cli-baseline", Kind: "issue", Status: "running", IssueTitle: "Add rate limiting",
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
	p := uzicli.NewPrinter(&buf, false, false, true, false) // non-tty, non-json, no colour
	if err := renderRunDetail(p, r); err != nil {
		t.Fatalf("renderRunDetail: %v", err)
	}

	got := milestoneNowRegion(t, buf.String())

	// The recorded baseline: the frozen-order milestone rows, then ONE global NOW row (no
	// milestone id). The milestone-state keys sit at column 0 because Table runs CellText,
	// which trims the "  " indent milestoneRows adds; column alignment is the table's own
	// (the widest key, WAIT_ON_LIMIT, drives the value column).
	const want = "MILESTONES     1/3 reported complete\n" +
		"done           Alpha\n" +
		"in progress    Beta\n" +
		"in progress    Gamma\n" +
		"NOW            coder · Wire the limiter · Edit api/internal/limits/window.go · 0s ago"

	if got != want {
		t.Errorf("PRD #1224 M4 CLI baseline drifted.\n--- got ---\n%s\n--- want ---\n%s\n--- got (quoted) ---\n%q", got, want, got)
	}
}
