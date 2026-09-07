package main

import (
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// PRD #517 M6: the TUI must render awaiting_followup as its own "your-turn / needs-you"
// state, distinctly labelled "follow-up" — never falling through to a default arm that
// would draw the raw enum or a generic dot. These pin the three shared render helpers plus
// the board triage band, each with a stated reddening mutation.

// TestStateGlyphWordAwaitingFollowup pins the glyph + word. Reddening mutation: remove the
// awaiting_followup case from stateGlyphWord → the default arm returns ("·", cellText of
// the lowercased status), i.e. glyph "·" and word "awaiting_followup", so both assertions
// fail. The word must NOT be "needs input" (awaiting_input's) either.
func TestStateGlyphWordAwaitingFollowup(t *testing.T) {
	glyph, word := stateGlyphWord("awaiting_followup", "", false, false)
	if glyph != "➤" {
		t.Errorf("awaiting_followup glyph = %q, want %q", glyph, "➤")
	}
	if word != "follow-up" {
		t.Errorf("awaiting_followup word = %q, want %q (must not be the raw enum or awaiting_input's label)", word, "follow-up")
	}
	// A distinct glyph from the neighbouring parks, so the spine reads three different
	// states under NO_COLOR where colour cannot tell them apart.
	if inputGlyph, _ := stateGlyphWord("awaiting_input", "", false, false); glyph == inputGlyph {
		t.Errorf("awaiting_followup and awaiting_input share glyph %q — they must be distinguishable without colour", glyph)
	}
}

// TestStateColorAwaitingFollowup pins the colour to amber, the shared "needs-you" ink.
// Reddening mutation: remove awaiting_followup from stateColor's amber case → it falls to
// the faintC default, so it stops matching amber (and awaiting_input) and matches faintC.
func TestStateColorAwaitingFollowup(t *testing.T) {
	p := newPalette(true)
	followup := bgFillSGR(p.stateColor("awaiting_followup", "", false, false))
	if followup != bgFillSGR(p.amber) {
		t.Errorf("awaiting_followup colour is not amber; a needs-you park must share the amber attention ink")
	}
	if followup != bgFillSGR(p.stateColor("awaiting_input", "", false, false)) {
		t.Error("awaiting_followup and awaiting_input resolve to different colours; both are needs-you parks and share amber")
	}
	if followup == bgFillSGR(p.faintC) {
		t.Error("awaiting_followup resolved to the faint default; its explicit amber case is not being read")
	}
	// Health precedence is unchanged: a stalled park is still the stall colour.
	if bgFillSGR(p.stateColor("awaiting_followup", "stalled", false, false)) != bgFillSGR(p.stall) {
		t.Error("stalled health no longer overrides the awaiting_followup bucket; the precedence rule regressed")
	}
}

// TestRunBandAwaitingFollowup pins the triage band to NEEDS YOU. Reddening mutation: remove
// awaiting_followup from runBand's needs-you case → it falls through to bandFloor (it is not
// terminal), so it stops matching bandNeedsYou / awaiting_input's band.
func TestRunBandAwaitingFollowup(t *testing.T) {
	if got := runBand("awaiting_followup", false); got != bandNeedsYou {
		t.Errorf("runBand(awaiting_followup) = %d, want bandNeedsYou (%d) — a follow-up park is the user's turn", got, bandNeedsYou)
	}
	if runBand("awaiting_followup", false) != runBand("awaiting_input", false) {
		t.Error("awaiting_followup and awaiting_input land in different bands; both are rows a human must act on")
	}
	if runBand("awaiting_followup", false) == bandFloor {
		t.Error("awaiting_followup fell through to ON THE FLOOR; it must be promoted to NEEDS YOU")
	}
}

// TestBoardRendersAwaitingFollowupGlyphTwin proves the state's glyph is its NO_COLOR twin:
// it survives an SGR-strip, so a reader on an Ascii/NoTTY profile still sees the state.
// Reddening mutation: remove the awaiting_followup case from stateGlyphWord → the row draws
// the "·" default glyph and the raw enum word, so the ➤ / "follow-up" assertions fail.
func TestBoardRendersAwaitingFollowupGlyphTwin(t *testing.T) {
	fake := &uzicli.FakeClient{Runs: []apitypes.RunListItemDTO{
		{RunDTO: apitypes.RunDTO{ID: "aaaaaaaa-1", Kind: "task", Status: "awaiting_followup", IssueTitle: "interactive one"}},
	}}
	m := tuiTestModel(t, fake, "")
	next, _ := m.Update(boardRunsMsg{reqID: m.board.waitID, runs: fake.Runs})
	m = next.(tuiModel)
	// Strip every SGR escape: what remains is exactly what a NO_COLOR/Ascii terminal shows.
	plain := stripANSI(m.View().Content)
	if !strings.Contains(plain, "➤") {
		t.Errorf("awaiting_followup glyph ➤ absent from the colour-stripped board — it is not a NO_COLOR twin\n%s", plain)
	}
	if !strings.Contains(plain, "follow-up") {
		t.Errorf("awaiting_followup word \"follow-up\" absent from the colour-stripped board\n%s", plain)
	}
}
