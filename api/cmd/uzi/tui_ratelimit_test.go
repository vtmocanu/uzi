package main

// PRD #519 F2 — the factory-floor rate-limit strip mirrors the web left-bottom sidebar's
// selection (RateLimitMeters SidebarRateLimits + sidebarTokens.ts): readable = status "ok",
// nothing readable → no strip; showLabel keyed off readable (>1); shown = readable filtered by
// (IsDefault || SecretID ∈ sidebar_token_ids); nothing shown → no strip. Each shown token draws
// its 5h + 7d windows as a tone-coloured mini bar plus always-present NN% text.

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// okMeter builds one readable ("ok") token meter with the given windows.
func okMeter(secretID, label string, isDefault bool, fiveHour, sevenDay int) apitypes.TokenRateLimitDTO {
	return apitypes.TokenRateLimitDTO{
		SecretID: secretID, Label: label, IsDefault: isDefault,
		Limits: apitypes.RateLimitDTO{
			Status:   "ok",
			FiveHour: &apitypes.RateLimitWindow{Pct: fiveHour},
			SevenDay: &apitypes.RateLimitWindow{Pct: sevenDay},
		},
	}
}

// stripModel feeds the meters + sidebar selection into a fresh board model and returns it.
func stripModel(t *testing.T, meters []apitypes.TokenRateLimitDTO, sidebar []string) tuiModel {
	t.Helper()
	m := tuiTestModel(t, nil, "")
	next, _ := m.Update(rateLimitsMsg{tokens: meters})
	m = next.(tuiModel)
	next, _ = m.Update(settingsMsg{settings: apitypes.UserSettingsDTO{SidebarTokenIds: sidebar}})
	m = next.(tuiModel)
	return m
}

// TestBoardRateLimitStripDropsUnreadable — a token with Status != "ok" never appears.
// The unreadable fixture is IsDefault:true, so it WOULD clear the shown (sidebar) filter — only
// the readable (Status=="ok") filter can drop it. That isolates the readable filter as the cause
// (deleting `if t.Limits.Status == "ok"` in boardRateLimitStrip makes this test fail red). A
// second readable default keeps the strip non-empty so the drop is observable against real chrome.
func TestBoardRateLimitStripDropsUnreadable(t *testing.T) {
	meters := []apitypes.TokenRateLimitDTO{
		okMeter("sec-personal", "personal", true, 33, 61),
		{SecretID: "sec-down", Label: "throttled", IsDefault: true, Limits: apitypes.RateLimitDTO{
			Status: "unavailable", FiveHour: &apitypes.RateLimitWindow{Pct: 77}}},
	}
	m := stripModel(t, meters, nil)
	out := m.View().Content
	if !strings.Contains(out, "33%") {
		t.Fatalf("readable default token's 5h pct 33%% missing from board:\n%s", out)
	}
	if strings.Contains(out, "throttled") {
		t.Errorf("a token with Status != \"ok\" leaked its label into the strip:\n%s", out)
	}
	if strings.Contains(out, "77%") {
		t.Errorf("a token with Status != \"ok\" leaked its window pct into the strip:\n%s", out)
	}
}

// TestBoardRateLimitStripSelection — default AND a listed non-default show; an unlisted
// non-default does not.
func TestBoardRateLimitStripSelection(t *testing.T) {
	meters := []apitypes.TokenRateLimitDTO{
		okMeter("sec-personal", "personal", true, 33, 61),
		okMeter("sec-meta", "meta", false, 87, 45),
		okMeter("sec-unlisted", "unlisted", false, 11, 22),
	}
	m := stripModel(t, meters, []string{"sec-meta"})
	out := m.View().Content
	if !strings.Contains(out, "personal") {
		t.Errorf("the default token is not shown:\n%s", out)
	}
	if !strings.Contains(out, "meta") {
		t.Errorf("a non-default token listed in sidebar_token_ids is not shown:\n%s", out)
	}
	if strings.Contains(out, "unlisted") {
		t.Errorf("a non-default token NOT in sidebar_token_ids leaked into the strip:\n%s", out)
	}
}

// TestBoardRateLimitStripLabelKeyedOffReadable — the label shows only when len(readable) > 1,
// and it is keyed off readable, not the shown subset.
func TestBoardRateLimitStripLabelKeyedOffReadable(t *testing.T) {
	// Single readable token → no label (showLabel false), but the windows still render.
	single := stripModel(t, []apitypes.TokenRateLimitDTO{
		okMeter("sec-personal", "solotoken", true, 33, 61),
	}, nil)
	singleOut := single.View().Content
	if strings.Contains(singleOut, "solotoken") {
		t.Errorf("label rendered for a single readable token (showLabel must be false):\n%s", singleOut)
	}
	if !strings.Contains(singleOut, "33%") || !strings.Contains(singleOut, "5h") {
		t.Errorf("single-token windows are missing from the strip:\n%s", singleOut)
	}

	// Two readable tokens where only the default is SHOWN — showLabel is still true because it is
	// keyed off readable (2 > 1), not off shown (1). The default's label must render.
	multi := stripModel(t, []apitypes.TokenRateLimitDTO{
		okMeter("sec-personal", "labelled", true, 33, 61),
		okMeter("sec-unlisted", "unlisted", false, 11, 22),
	}, nil)
	multiOut := multi.View().Content
	if !strings.Contains(multiOut, "labelled") {
		t.Errorf("label absent when len(readable) > 1 (showLabel is keyed off readable):\n%s", multiOut)
	}
}

// TestBoardRateLimitStripTonePct — both windows' server-rounded NN% text renders AND the
// tone colour is actually painted onto the bar: a danger-band window (Pct 88 ≥ 85) fills with
// m.pal.alarm, an ok-band window (Pct 20 < 40) with m.pal.sage. Asserting the exact paintSeg
// fragment (same fg SGR + same ▰ run) fails if rateTone returned the wrong band.
func TestBoardRateLimitStripTonePct(t *testing.T) {
	m := stripModel(t, []apitypes.TokenRateLimitDTO{
		okMeter("sec-personal", "personal", true, 88, 20),
	}, nil)
	out := m.View().Content
	for _, want := range []string{"88%", "20%"} {
		if !strings.Contains(out, want) {
			t.Errorf("window pct %q missing from the strip:\n%s", want, out)
		}
	}
	// The danger band's filled run is painted alarm; the ok band's filled run is painted sage.
	alarmFilled, _ := rateBarParts(88)
	if frag := paintSeg(m.pal.alarm, nil, false, alarmFilled); !strings.Contains(out, frag) {
		t.Errorf("danger-band bar (Pct 88) is not painted with m.pal.alarm; want fragment %q in:\n%s", frag, out)
	}
	sageFilled, _ := rateBarParts(20)
	if frag := paintSeg(m.pal.sage, nil, false, sageFilled); !strings.Contains(out, frag) {
		t.Errorf("ok-band bar (Pct 20) is not painted with m.pal.sage; want fragment %q in:\n%s", frag, out)
	}
}

// TestBoardRateLimitStripAccentBarPerToken — every shown token is prefixed with exactly one
// per-group accent bar ▎ (U+258E), so a 3-account fixture renders 3 bars. Count-anchored so it
// fails red if the accent-bar prefix is removed.
func TestBoardRateLimitStripAccentBarPerToken(t *testing.T) {
	m := stripModel(t, []apitypes.TokenRateLimitDTO{
		okMeter("sec-cristi", "cristi", true, 4, 82),
		okMeter("sec-meta", "meta", true, 4, 94),
		okMeter("sec-personal", "personal", true, 7, 100),
	}, nil)
	out := m.View().Content
	if n := strings.Count(out, "▎"); n != 3 {
		t.Errorf("want exactly 3 per-group accent bars (one per shown token), got %d:\n%s", n, out)
	}
}

// TestBoardRateLimitStripAccentTint — the accent bar is a status light, tinted by the token's
// PEAK window pct: alarm SGR when peak ≥ rateDangerPct, faint SGR otherwise. Asserted at the
// SGR level (mirrors TestBoardRateLimitStripTonePct). `personal` (5h 7 low, 7d 100 danger)
// proves PEAK drives the tint — a low 5h with a danger 7d must still redden, so flipping the
// peak/threshold logic reddens this test.
func TestBoardRateLimitStripAccentTint(t *testing.T) {
	m := stripModel(t, []apitypes.TokenRateLimitDTO{
		okMeter("sec-cristi", "cristi", true, 4, 82),      // peak 82 < 85 → faint
		okMeter("sec-meta", "meta", true, 4, 94),          // peak 94 ≥ 85 → alarm
		okMeter("sec-personal", "personal", true, 7, 100), // peak 100 ≥ 85 via 7d → alarm
	}, nil)
	out := m.View().Content
	if frag := paintSeg(m.pal.alarm, nil, false, "▎"); !strings.Contains(out, frag) {
		t.Errorf("a peak-≥-85 token's accent bar is not painted with m.pal.alarm; want fragment %q in:\n%s", frag, out)
	}
	if frag := paintSeg(m.pal.faintC, nil, false, "▎"); !strings.Contains(out, frag) {
		t.Errorf("a peak-<-85 token's accent bar is not painted with m.pal.faintC; want fragment %q in:\n%s", frag, out)
	}
}

// TestBoardRateLimitStripNilWindow — a nil window mirrors windowPct's "-" (no NN%).
func TestBoardRateLimitStripNilWindow(t *testing.T) {
	m := stripModel(t, []apitypes.TokenRateLimitDTO{
		{SecretID: "sec-personal", Label: "personal", IsDefault: true, Limits: apitypes.RateLimitDTO{
			Status: "ok", FiveHour: &apitypes.RateLimitWindow{Pct: 50}, SevenDay: nil}},
	}, nil)
	out := m.View().Content
	if !strings.Contains(out, "7d -") {
		t.Errorf("a nil 7d window must render `7d -` (mirroring windowPct):\n%s", out)
	}
}

// TestBoardRateLimitStripSingleLineAndClamps — the chrome-count correctness depends on the
// strip being exactly ONE physical line clamped to the terminal width. Pin both at a narrow and
// a wide width.
func TestBoardRateLimitStripSingleLineAndClamps(t *testing.T) {
	m := stripModel(t, []apitypes.TokenRateLimitDTO{
		okMeter("sec-personal", "personal", true, 88, 44),
		okMeter("sec-meta", "meta", true, 12, 30),
	}, nil)

	// Narrow terminal: one physical line, no wider than m.width.
	next, _ := m.Update(tea.WindowSizeMsg{Width: 40, Height: 20})
	m = next.(tuiModel)
	strip := m.boardRateLimitStrip()
	if strings.Contains(strip, "\n") {
		t.Errorf("strip must be one physical line at width 40, got a newline:\n%q", strip)
	}
	if w := visualWidth(strip); w > m.width {
		t.Errorf("strip visual width %d exceeds m.width %d:\n%q", w, m.width, strip)
	}

	// Wide terminal: still one physical line.
	next, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: 20})
	m = next.(tuiModel)
	if strip := m.boardRateLimitStrip(); strings.Contains(strip, "\n") {
		t.Errorf("strip must be one physical line at width 120, got a newline:\n%q", strip)
	}
}

// TestBoardRateLimitStripAsciiSignalSurvives — under an Ascii (NO_COLOR) profile the
// always-present cues (the NN% text and the ▰/▱ bar glyphs) must survive, so the signal is
// legible when tone colour is gone. NOTE: boardRateLimitStrip emits paintSeg SGR regardless of
// profile; the colorprofile Writer strips the colour downstream at flush, which is NOT observable
// in the returned strip / View().Content — so this asserts signal-survival, not SGR removal (the
// latter is a downstream-writer property this layer cannot see).
func TestBoardRateLimitStripAsciiSignalSurvives(t *testing.T) {
	m := stripModel(t, []apitypes.TokenRateLimitDTO{
		okMeter("sec-personal", "personal", true, 88, 20),
	}, nil)
	next, _ := m.Update(tea.ColorProfileMsg{Profile: colorprofile.Ascii})
	m = next.(tuiModel)
	strip := m.boardRateLimitStrip()
	for _, want := range []string{"88%", "20%", "▰", "▱"} {
		if !strings.Contains(strip, want) {
			t.Errorf("Ascii-profile strip dropped the always-present cue %q:\n%q", want, strip)
		}
	}
}

// TestBoardRateLimitStripEmptyCases — no strip line when nothing is readable/shown.
func TestBoardRateLimitStripEmptyCases(t *testing.T) {
	// (a) no tokens at all.
	none := stripModel(t, nil, nil)
	if s := none.boardRateLimitStrip(); s != "" {
		t.Errorf("no tokens must yield no strip, got %q", s)
	}
	// (b) all tokens unreadable (Status != "ok").
	allDown := stripModel(t, []apitypes.TokenRateLimitDTO{
		{SecretID: "a", Label: "a", Limits: apitypes.RateLimitDTO{Status: "unavailable"}},
		{SecretID: "b", Label: "b", IsDefault: true, Limits: apitypes.RateLimitDTO{Status: "no_token"}},
	}, nil)
	if s := allDown.boardRateLimitStrip(); s != "" {
		t.Errorf("all-unreadable tokens must yield no strip, got %q", s)
	}
	// (c) readable but nothing shown (a single non-default token not in the sidebar).
	hidden := stripModel(t, []apitypes.TokenRateLimitDTO{
		okMeter("sec-x", "hiddenlabel", false, 40, 40),
	}, nil)
	if s := hidden.boardRateLimitStrip(); s != "" {
		t.Errorf("readable-but-not-shown token must yield no strip, got %q", s)
	}

	// The empty strip draws NO extra line: the board render has the wordmark then a blank, with
	// no strip line between. Compare against a control model that DOES draw a strip.
	emptyBoard := none.renderBoard()
	if strings.Contains(emptyBoard, "5h") || strings.Contains(emptyBoard, "7d") {
		t.Errorf("an empty strip must not draw any window cell into the board:\n%s", emptyBoard)
	}
}
