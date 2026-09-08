package main

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// PRD #1190 M4 — the TUI's paused vocabulary: the ‖ paused state token, the two row-2
// lines (paused / pause-requested) drawn beside the rate-limit park line, the clause
// shedding, the runBand floor placement, and the reworded rate-limit line.

// TestPausedStateGlyphWord — the state token for a paused run is the ‖ pause bar plus the word
// "paused", in the wait colour (shared with the two involuntary parks; the word tells them
// apart). MUTATION PROOF: remove the statusPaused arm and the default arm draws "· paused" in
// faintC, so both the glyph and the colour assertions redden.
func TestPausedStateGlyphWord(t *testing.T) {
	glyph, word := stateGlyphWord(statusPaused, "ok", false, false)
	if glyph != "‖" || word != "paused" {
		t.Errorf("stateGlyphWord(paused) = (%q, %q), want (%q, %q)", glyph, word, "‖", "paused")
	}
	p := newPalette(true)
	if got := p.stateColor(statusPaused, "ok", false, false); got != p.wait {
		t.Errorf("stateColor(paused) = %v, want the wait colour %v", got, p.wait)
	}
}

// TestPausedDetailShowsGlyphAndWord — the paused run's detail header renders the ‖ glyph and
// the word "paused", and both survive an Ascii (NO_COLOR) profile so the signal is legible
// without colour.
func TestPausedDetailShowsGlyphAndWord(t *testing.T) {
	build := func(profile colorprofile.Profile) string {
		m := tuiTestModel(t, &uzicli.FakeClient{}, "r1")
		if profile == colorprofile.Ascii {
			next, _ := m.Update(tea.ColorProfileMsg{Profile: colorprofile.Ascii})
			m = next.(tuiModel)
		}
		m = applyDetail(m, pausedFixture(), nil)
		return stripANSI(m.View().Content)
	}
	for _, tc := range []struct {
		name    string
		profile colorprofile.Profile
	}{
		{"colour", colorprofile.TrueColor},
		{"ascii", colorprofile.Ascii},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := build(tc.profile)
			if !strings.Contains(out, "‖") {
				t.Errorf("paused detail is missing the ‖ glyph under the %s profile:\n%s", tc.name, out)
			}
			if !strings.Contains(out, "paused") {
				t.Errorf("paused detail is missing the word 'paused' under the %s profile:\n%s", tc.name, out)
			}
		})
	}
}

// TestPauseRow2LinesPerStatus — the row-2 line drawn in the detail depends on status: a paused
// run draws the "‖ paused by you" line; a running run with a pending request draws the
// "pause requested" line; a plain running run draws neither.
func TestPauseRow2LinesPerStatus(t *testing.T) {
	req := time.Date(2026, 9, 8, 11, 0, 0, 0, time.UTC)
	pending := apitypes.RunDTO{
		ID: "r1", Kind: "issue", Status: "running", Health: "ok", Milestones: sixMilestones(),
		PauseRequestedAt: &req, PauseMode: sp("milestone"), PauseAfterCount: intPtr(3),
	}
	plain := apitypes.RunDTO{ID: "r1", Kind: "issue", Status: "running", Health: "ok"}

	render := func(run apitypes.RunDTO) string {
		m := tuiTestModel(t, &uzicli.FakeClient{}, "r1")
		m = applyDetail(m, run, nil)
		return stripANSI(m.View().Content)
	}

	pausedOut := render(pausedFixture())
	if !strings.Contains(pausedOut, "‖ paused by you") {
		t.Errorf("paused detail is missing the row-2 paused line:\n%s", pausedOut)
	}
	if strings.Contains(pausedOut, "pause requested ·") {
		t.Errorf("paused detail must NOT draw the pending-request line:\n%s", pausedOut)
	}

	pendingOut := render(pending)
	if !strings.Contains(pendingOut, "pause requested") {
		t.Errorf("pending-pause detail is missing the row-2 pause-requested line:\n%s", pendingOut)
	}
	if strings.Contains(pendingOut, "paused by you") {
		t.Errorf("a running run with a pending pause must NOT draw the paused line:\n%s", pendingOut)
	}

	plainOut := render(plain)
	if strings.Contains(plainOut, "paused by you") || strings.Contains(plainOut, "pause requested") {
		t.Errorf("a plain running run must draw neither pause line:\n%s", plainOut)
	}
}

// TestPausedLineSheds — the paused row-2 builder is one line that sheds clauses from the right
// (checkpoint first, then done) to fit width, with "‖ paused by you" always surviving. The
// widths are chosen against the fixed-`now` content whose full form is
// "‖ paused by you · since 11:02 (2h40m) · 3/6 done · checkpoint 2h41m ago".
func TestPausedLineSheds(t *testing.T) {
	r := pausedFixture()
	now := time.Date(2026, 9, 8, 13, 42, 0, 0, time.UTC)

	// Wide: every clause present.
	wide := pausedLine(r, now, 100)
	if w := visualWidth(wide); w > 100 {
		t.Errorf("wide line width %d exceeds budget 100: %q", w, wide)
	}
	for _, want := range []string{"‖ paused by you", "since 11:02", "3/6 done", "checkpoint"} {
		if !strings.Contains(wide, want) {
			t.Errorf("wide paused line missing %q: %q", want, wide)
		}
	}

	// Medium: checkpoint sheds first, done survives.
	medium := pausedLine(r, now, 60)
	if w := visualWidth(medium); w > 60 {
		t.Errorf("medium line width %d exceeds budget 60: %q", w, medium)
	}
	if !strings.Contains(medium, "3/6 done") {
		t.Errorf("medium paused line dropped 'done' before 'checkpoint': %q", medium)
	}
	if strings.Contains(medium, "checkpoint") {
		t.Errorf("medium paused line should have shed 'checkpoint' first: %q", medium)
	}

	// Narrow: both optional clauses shed, the lead survives.
	narrow := pausedLine(r, now, 40)
	if w := visualWidth(narrow); w > 40 {
		t.Errorf("narrow line width %d exceeds budget 40: %q", w, narrow)
	}
	if !strings.Contains(narrow, "paused by you") {
		t.Errorf("narrow paused line must keep the 'paused by you' lead: %q", narrow)
	}
	if strings.Contains(narrow, "done") || strings.Contains(narrow, "checkpoint") {
		t.Errorf("narrow paused line must shed both 'done' and 'checkpoint': %q", narrow)
	}
}

// TestPauseRequestedLineContent — the pending row-2 builder names the boundary and carries the
// hint, and sheds the hint first (then the boundary) when width is tight, keeping
// "pause requested".
func TestPauseRequestedLineContent(t *testing.T) {
	r := apitypes.RunDTO{
		Status: "running", Milestones: sixMilestones(), PauseMode: sp("milestone"), PauseAfterCount: intPtr(3),
	}
	full := pauseRequestedLine(r, 120)
	if full != "pause requested · after M4 · cancel or pause now from the web or CLI" {
		t.Errorf("full pause-requested line = %q", full)
	}
	// Tight: the hint sheds first, the boundary survives.
	mid := pauseRequestedLine(r, 30)
	if mid != "pause requested · after M4" {
		t.Errorf("mid pause-requested line = %q, want the hint shed but the boundary kept", mid)
	}
	// Very tight: only the lead survives.
	narrow := pauseRequestedLine(r, 5)
	if narrow != "pause requested" {
		t.Errorf("narrow pause-requested line = %q, want just 'pause requested'", narrow)
	}
}

// TestRunBandPausedOnFloor — a paused run lands ON THE FLOOR (like the other holds), never in
// NEEDS YOU or DONE. runBand needs no code change for this (paused is neither awaiting_* nor
// terminal), and this test guards that property against a future edit.
func TestRunBandPausedOnFloor(t *testing.T) {
	if b := runBand(statusPaused, false); b != bandFloor {
		t.Errorf("runBand(paused) = %d, want bandFloor (%d)", b, bandFloor)
	}
}

// TestLimitWaitLineReworded — D13: the rate-limit park line's leading word is "waiting:", and
// the old "paused: Anthropic" wording is gone from it (so it cannot be read as a user pause).
func TestLimitWaitLineReworded(t *testing.T) {
	retry := time.Now().Add(4 * time.Hour)
	r := apitypes.RunDTO{Status: statusLimitWait, RetryNotBefore: &retry, RateLimitType: sp("five_hour")}
	line := limitWaitLine(r, time.Now())
	if !strings.HasPrefix(line, "waiting: Anthropic usage limit") {
		t.Errorf("rate-limit line must start with 'waiting: Anthropic usage limit', got %q", line)
	}
	if strings.Contains(line, "paused: Anthropic") {
		t.Errorf("the old 'paused: Anthropic' wording must be gone from the rate-limit line, got %q", line)
	}
}
