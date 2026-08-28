package main

import (
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// issue #750: while a "revise" replan is in flight the server keeps runs.status at
// awaiting_approval but sets is_revising. The TUI must treat such a run as its own calm
// "revising" state — a working state, NOT a needs-you park — and drop it OUT of the NEEDS
// YOU band until the next plan lands (server flips is_revising back to false). is_revising
// is NOT status-gated server-side, so every helper gates on
// status == "awaiting_approval" && isRevising itself, mirroring is_planning.

// TestStateGlyphWordRevising pins the glyph + word. A revising run reads the ↻ / "revising"
// vocabulary (a calm sibling of planning's ○), never the ⚑ / "plan gate" that a genuine
// awaiting_approval gate draws. The gate is honoured: is_revising off awaiting_approval has
// no effect. Reddening mutation: drop the "revising" case from stateGlyphWord → an
// awaiting_approval+is_revising run renders ⚑ / "plan gate", so both assertions fail.
func TestStateGlyphWordRevising(t *testing.T) {
	glyph, word := stateGlyphWord("awaiting_approval", "", false, true)
	if glyph != "↻" {
		t.Errorf("revising glyph = %q, want %q", glyph, "↻")
	}
	if word != "revising" {
		t.Errorf("revising word = %q, want %q (must not be the awaiting_approval plan-gate label)", word, "revising")
	}
	// A distinct glyph from the plan gate, so the spine reads the two apart under NO_COLOR.
	if gateGlyph, _ := stateGlyphWord("awaiting_approval", "", false, false); glyph == gateGlyph {
		t.Errorf("revising and the plan gate share glyph %q — they must be distinguishable without colour", glyph)
	}
	// is_revising is gated on awaiting_approval: it has no effect on any other status.
	if _, w := stateGlyphWord("running", "", false, true); w != "running" {
		t.Errorf("is_revising leaked onto a running run: word = %q, want %q", w, "running")
	}
}

// TestStateColorRevising pins the colour to indigo, the calm/info ink planning uses — NOT
// the amber attention ink an awaiting_approval gate draws. Reddening mutation: drop
// "revising" from stateColor's planning case → it falls to amber (awaiting_approval's), so
// it stops matching indigo and starts matching amber.
func TestStateColorRevising(t *testing.T) {
	p := newPalette(true)
	revising := bgFillSGR(p.stateColor("awaiting_approval", "", false, true))
	if revising != bgFillSGR(p.indigo) {
		t.Error("revising colour is not indigo; a re-planning run must share planning's calm/info ink")
	}
	if revising == bgFillSGR(p.amber) {
		t.Error("revising resolved to amber, the needs-you attention ink; it must read as working, not waiting")
	}
	// Health precedence is unchanged: a stalled revising run is still the stall colour.
	if bgFillSGR(p.stateColor("awaiting_approval", "stalled", false, true)) != bgFillSGR(p.stall) {
		t.Error("stalled health no longer overrides the revising bucket; the precedence rule regressed")
	}
}

// TestRunBandRevising pins the triage band. A revising run (awaiting_approval + is_revising)
// drops to ON THE FLOOR; the SAME status without is_revising stays in NEEDS YOU. The other
// two parks are unaffected by is_revising. Reddening mutation: drop the is_revising check
// from runBand → the revising run stays in bandNeedsYou.
func TestRunBandRevising(t *testing.T) {
	if got := runBand("awaiting_approval", true); got != bandFloor {
		t.Errorf("runBand(awaiting_approval, true) = %d, want bandFloor (%d) — a revising run is not the user's turn", got, bandFloor)
	}
	if got := runBand("awaiting_approval", false); got != bandNeedsYou {
		t.Errorf("runBand(awaiting_approval, false) = %d, want bandNeedsYou (%d) — a genuine plan gate is the user's turn", got, bandNeedsYou)
	}
	// The other needs-you parks are not affected by is_revising (it is gated on awaiting_approval).
	if got := runBand("awaiting_input", true); got != bandNeedsYou {
		t.Errorf("runBand(awaiting_input, true) = %d, want bandNeedsYou (%d) — is_revising must not leak past awaiting_approval", got, bandNeedsYou)
	}
	if got := runBand("awaiting_followup", true); got != bandNeedsYou {
		t.Errorf("runBand(awaiting_followup, true) = %d, want bandNeedsYou (%d) — is_revising must not leak past awaiting_approval", got, bandNeedsYou)
	}
}

// TestBandOrderPlacesRevisingOnFloor proves the whole-list partition: a revising row lands
// in the ON THE FLOOR slice while a genuine plan-gate row stays in NEEDS YOU, so the board's
// NEEDS YOU → ON THE FLOOR → DONE concatenation puts the revising run after the gate run.
func TestBandOrderPlacesRevisingOnFloor(t *testing.T) {
	runs := []apitypes.RunListItemDTO{
		{RunDTO: apitypes.RunDTO{ID: "revising-1", Kind: "issue", Status: "awaiting_approval"}, IsRevising: true},
		{RunDTO: apitypes.RunDTO{ID: "gate-1", Kind: "issue", Status: "awaiting_approval"}},
	}
	out := bandOrder(runs)
	if len(out) != 2 {
		t.Fatalf("bandOrder returned %d rows, want 2", len(out))
	}
	// NEEDS YOU comes first, so the genuine gate leads and the revising run is demoted below it.
	if out[0].ID != "gate-1" {
		t.Errorf("bandOrder[0] = %q, want the plan-gate run \"gate-1\" in NEEDS YOU", out[0].ID)
	}
	if out[1].ID != "revising-1" {
		t.Errorf("bandOrder[1] = %q, want the revising run \"revising-1\" on the floor", out[1].ID)
	}
}

// TestBoardRendersRevisingGlyphTwin proves the state's glyph is its NO_COLOR twin: it
// survives an SGR-strip, so a reader on an Ascii/NoTTY profile still sees the "revising"
// word and ↻ glyph. Reddening mutation: drop the revising case from stateGlyphWord → the
// row draws ⚑ / "plan gate", so the ↻ / "revising" assertions fail.
func TestBoardRendersRevisingGlyphTwin(t *testing.T) {
	fake := &uzicli.FakeClient{Runs: []apitypes.RunListItemDTO{
		{RunDTO: apitypes.RunDTO{ID: "aaaaaaaa-1", Kind: "issue", Status: "awaiting_approval", IssueTitle: "replanning one"}, IsRevising: true},
	}}
	m := tuiTestModel(t, fake, "")
	next, _ := m.Update(boardRunsMsg{runs: fake.Runs})
	m = next.(tuiModel)
	// Strip every SGR escape: what remains is exactly what a NO_COLOR/Ascii terminal shows.
	plain := stripANSI(m.View().Content)
	if !strings.Contains(plain, "↻") {
		t.Errorf("revising glyph ↻ absent from the colour-stripped board — it is not a NO_COLOR twin\n%s", plain)
	}
	if !strings.Contains(plain, "revising") {
		t.Errorf("revising word \"revising\" absent from the colour-stripped board\n%s", plain)
	}
}
