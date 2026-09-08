package main

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/colorprofile"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// TestStateGlyphWordSlowReadsNearTimeout: the display map relabels the `slow` enum
// (kept, D1) to "near timeout" on the status token, while every other health value passes
// through unchanged. This is the color-independent core; the render test below proves the
// word survives on the actual board and detail surfaces.
func TestStateGlyphWordSlowReadsNearTimeout(t *testing.T) {
	if g, w := stateGlyphWord("running", "slow", false, false); g != "▲" || w != "near timeout" {
		t.Fatalf("stateGlyphWord(running, slow) = (%q, %q), want (\"▲\", \"near timeout\")", g, w)
	}
	// The other WARN flags still read their own word (the map only remaps slow).
	if _, w := stateGlyphWord("running", "stalled", false, false); w != "stalled" {
		t.Errorf("stateGlyphWord(running, stalled) word = %q, want \"stalled\"", w)
	}
	if _, w := stateGlyphWord("running", "looping", false, false); w != "looping" {
		t.Errorf("stateGlyphWord(running, looping) word = %q, want \"looping\"", w)
	}
	// displayHealth is the shared map, so the CLI HEALTH row and the token cannot drift.
	if got := displayHealth("slow"); got != "near timeout" {
		t.Errorf("displayHealth(slow) = %q, want \"near timeout\"", got)
	}
	if got := displayHealth("stalled"); got != "stalled" {
		t.Errorf("displayHealth(stalled) = %q, want it unchanged", got)
	}
}

// TestNearTimeoutWordVisibleUnderEveryProfile is the specs/human.md "keep the health words
// visible on the board" requirement: a near-timeout run's word "near timeout" (not the raw
// "slow") must be legible on the board AND read "▲ near timeout" on the detail header,
// under BOTH a colour profile and NO_COLOR/Ascii (the glyph + word carry the meaning
// without any colour).
func TestNearTimeoutWordVisibleUnderEveryProfile(t *testing.T) {
	now := time.Now()
	future := now.Add(2 * time.Hour)
	runID := "d1170aaa-0000"

	for _, prof := range []colorprofile.Profile{colorprofile.TrueColor, colorprofile.Ascii} {
		// Board: the status-word column shows "near timeout", never "slow".
		fake := &uzicli.FakeClient{Runs: []apitypes.RunListItemDTO{
			{RunDTO: apitypes.RunDTO{ID: runID, Kind: "issue", Status: "running", IssueTitle: "a run", Health: "slow"}},
		}}
		bm := tuiTestModel(t, fake, "")
		bm.profile = prof
		next, _ := bm.Update(boardRunsMsg{reqID: bm.board.waitID, runs: fake.Runs})
		bm = next.(tuiModel)
		board := stripANSI(bm.View().Content)
		if !strings.Contains(board, "near timeout") {
			t.Errorf("profile %v: the board does not show \"near timeout\" for a slow run:\n%s", prof, board)
		}
		if strings.Contains(board, "slow") {
			t.Errorf("profile %v: the raw enum \"slow\" leaked onto the board:\n%s", prof, board)
		}

		// Detail header: the status token reads "▲ near timeout" contiguously.
		dm := tuiTestModel(t, &uzicli.FakeClient{}, runID)
		dm.profile = prof
		dm = applyDetail(dm, apitypes.RunDTO{
			ID: runID, Kind: "issue", Status: "running", IssueTitle: "a run",
			Health: "slow", DeadlineAt: &future,
		}, nil)
		header := stripANSI(strings.Join(dm.detailHeaderLines(), "\n"))
		if !strings.Contains(header, "▲ near timeout") {
			t.Errorf("profile %v: detail header token = %q, want it to read \"▲ near timeout\"", prof, header)
		}
		if strings.Contains(header, "slow") {
			t.Errorf("profile %v: the raw enum \"slow\" leaked into the detail header: %q", prof, header)
		}
	}
}

// TestDetailNearTimeoutRowIsOnePhysicalRow is the #379 invariant on the run detail: a
// flagged running run draws its near-timeout countdown on exactly ONE physical row below
// the (one-row) header, so transcriptViewport's line budget stays exact and the footer is
// not pushed off the bottom. The row is identified by "stops at", which the header token
// (glyph + word + runtime) does not carry.
func TestDetailNearTimeoutRowIsOnePhysicalRow(t *testing.T) {
	now := time.Now()
	future := now.Add(2 * time.Hour)
	bw := 8 * 60 * 60
	runID := "d1170379-0000"

	m := tuiTestModel(t, &uzicli.FakeClient{}, runID)
	m.width = 100
	m = applyDetail(m, apitypes.RunDTO{
		ID: runID, Kind: "issue", Status: "running", IssueTitle: "working run",
		Health: "slow", DeadlineAt: &future, BudgetWallSeconds: &bw,
	}, []apitypes.MessageDTO{msgDTO(1, "text", "lead", "", "", "hello", now)})

	// The header is exactly one physical row (issue #666 / #379).
	if n := len(m.detailHeaderLines()); n != 1 {
		t.Fatalf("detail header is %d physical rows, want exactly 1 (#379)", n)
	}

	lines := strings.Split(stripANSI(m.renderDetail()), "\n")
	var ntLines []string
	for _, ln := range lines {
		if strings.Contains(ln, "stops at") {
			ntLines = append(ntLines, ln)
		}
	}
	if len(ntLines) != 1 {
		t.Fatalf("the near-timeout row appears on %d physical lines, want exactly 1 (#379):\n%s", len(ntLines), strings.Join(lines, "\n"))
	}
	row := ntLines[0]
	if !strings.Contains(row, "▲ near timeout") || !strings.Contains(row, "left") {
		t.Errorf("near-timeout row = %q, want it to carry the \"▲ near timeout\" floor and the countdown", row)
	}
	if w := visualWidth(row); w > m.width {
		t.Errorf("near-timeout row is %d cells wide, want <= m.width=%d (one physical row, #379)", w, m.width)
	}
}
