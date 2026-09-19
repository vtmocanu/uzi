package main

import (
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// Issue #1418 M4: the shared status token must read "needs landing" for a `failed` run whose
// server-derived landing_state is "needs_landing" — its committed work is human-landable — while
// keeping the ✗ glyph and the failed/alarm colour. Every other landing_state reads "failed".

// TestStateGlyphWordLanding pins the word/glyph/colour of the landing bucket. Reddening mutation:
// drop the needs_landing branch in stateGlyphWord's failed arm → the word falls back to "failed"
// and the first assertion fails.
func TestStateGlyphWordLanding(t *testing.T) {
	glyph, word := stateGlyphWord("failed", "", false, false, landingStateNeedsLanding)
	if glyph != "✗" {
		t.Errorf("needs_landing glyph = %q, want %q (still the failed glyph)", glyph, "✗")
	}
	if word != "needs landing" {
		t.Errorf("needs_landing word = %q, want %q", word, "needs landing")
	}
	// Every other landing_state keeps the plain failed word.
	for _, ls := range []string{"unrecoverable", "none", ""} {
		if g, w := stateGlyphWord("failed", "", false, false, ls); g != "✗" || w != "failed" {
			t.Errorf("failed run with landing_state %q = (%q, %q), want (✗, failed)", ls, g, w)
		}
	}
	// The colour stays the failed/alarm ink for every landing bucket — only the word changes.
	p := newPalette(true)
	if bgFillSGR(p.stateColor("failed", "", false, false)) != bgFillSGR(p.alarm) {
		t.Error("failed colour is not the alarm ink; the needs-landing split must not change the colour")
	}
	// The full token via stateToken carries the needs-landing word AND the alarm colour together.
	tok := p.stateToken("failed", "", false, false, landingStateNeedsLanding)
	if tok.word != "needs landing" {
		t.Errorf("stateToken needs_landing word = %q, want %q", tok.word, "needs landing")
	}
	if bgFillSGR(tok.color) != bgFillSGR(p.alarm) {
		t.Error("stateToken needs_landing colour is not the alarm ink")
	}
}

// TestBoardRowLandingNotTruncated proves the board status-word cell (widened to 14 in issue #1418)
// renders "needs landing" (13 runes) in full rather than truncating it to "needs landi…". Reddening
// mutation: revert boardStatusWordWidth to 12 → capCell clips the word and the ellipsis assertion
// fires.
func TestBoardRowLandingNotTruncated(t *testing.T) {
	runs := []apitypes.RunListItemDTO{
		{RunDTO: apitypes.RunDTO{ID: "land-1", Kind: "issue", Status: "failed", LandingState: "needs_landing", IssueTitle: "landable one"}},
	}
	m := tuiTestModel(t, &uzicli.FakeClient{}, "")
	m = step(m, boardRunsMsg{reqID: m.board.waitID, runs: runs})
	out := stripANSI(m.View().Content)
	if !strings.Contains(out, "needs landing") {
		t.Errorf("board must render the full \"needs landing\" word:\n%s", out)
	}
	// The ellipsis capCell would insert on an over-narrow cell must NOT appear on this word.
	if strings.Contains(out, "needs landi…") || strings.Contains(out, "needs landin…") {
		t.Errorf("\"needs landing\" was truncated by the status-word cell:\n%s", out)
	}
}

// TestBoardFilterMatchesLanding proves the board `/` filter matches the RENDERED word "needs
// landing": a needs_landing failed run matches the query "needs landing" (the word threaded
// through stateGlyphWord at the filter call site), so a user can find it by what they see.
func TestBoardFilterMatchesLanding(t *testing.T) {
	b := newBoardState()
	b.runs = []apitypes.RunListItemDTO{
		{RunDTO: apitypes.RunDTO{ID: "land-1", Kind: "issue", Status: "failed", LandingState: "needs_landing", IssueTitle: "aaa"}},
		{RunDTO: apitypes.RunDTO{ID: "fail-2", Kind: "issue", Status: "failed", LandingState: "none", IssueTitle: "bbb"}},
	}
	b.filter = "needs landing"
	got := b.visible()
	if len(got) != 1 || got[0].ID != "land-1" {
		t.Errorf("filter \"needs landing\" must match only the needs_landing run, got %d rows: %+v", len(got), got)
	}
}
