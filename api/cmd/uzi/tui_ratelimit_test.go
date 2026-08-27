package main

// PRD #519 F2 — the factory-floor rate-limit strip mirrors the web left-bottom sidebar's
// selection (RateLimitMeters SidebarRateLimits + sidebarTokens.ts): readable = status "ok",
// nothing readable → no strip; showLabel keyed off readable (>1); shown = readable filtered by
// (IsDefault || SecretID ∈ sidebar_token_ids); nothing shown → no strip. Each shown token draws
// its 5h + 7d windows as a tone-coloured mini bar plus always-present NN% text.

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
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
	alarmFilled, _ := rateBarParts(88, rateBarWidth)
	if frag := paintSeg(m.pal.alarm, nil, false, alarmFilled); !strings.Contains(out, frag) {
		t.Errorf("danger-band bar (Pct 88) is not painted with m.pal.alarm; want fragment %q in:\n%s", frag, out)
	}
	sageFilled, _ := rateBarParts(20, rateBarWidth)
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
	strip := m.boardRateLimitStrip(time.Now())
	if strings.Contains(strip, "\n") {
		t.Errorf("strip must be one physical line at width 40, got a newline:\n%q", strip)
	}
	if w := visualWidth(strip); w > m.width {
		t.Errorf("strip visual width %d exceeds m.width %d:\n%q", w, m.width, strip)
	}

	// Wide terminal: still one physical line.
	next, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: 20})
	m = next.(tuiModel)
	if strip := m.boardRateLimitStrip(time.Now()); strings.Contains(strip, "\n") {
		t.Errorf("strip must be one physical line at width 120, got a newline:\n%q", strip)
	}

	// Countdown-present clamp (issue #588): okMeter builds nil-ResetsAt windows, so the checks
	// above never widen the strip with a reset countdown. Exercise the countdown branch against
	// the same one-physical-line / no-wider-than-m.width invariant at a narrow terminal, so the
	// suffix cannot break the clamp (e.g. by being appended after clampVisual).
	now := time.Unix(1_700_000_000, 0)
	mc := stripModel(t, []apitypes.TokenRateLimitDTO{
		{
			SecretID: "sec-personal", Label: "personal", IsDefault: true,
			Limits: apitypes.RateLimitDTO{
				Status:   "ok",
				FiveHour: &apitypes.RateLimitWindow{Pct: 88, ResetsAt: ptrInt64(now.Unix() + 2*3600 + 13*60)},
				SevenDay: &apitypes.RateLimitWindow{Pct: 44, ResetsAt: ptrInt64(now.Unix() + 18*3600 + 2*60)},
			},
		},
		{
			SecretID: "sec-meta", Label: "meta", IsDefault: true,
			Limits: apitypes.RateLimitDTO{
				Status:   "ok",
				FiveHour: &apitypes.RateLimitWindow{Pct: 12, ResetsAt: ptrInt64(now.Unix() + 55*60)},
				SevenDay: &apitypes.RateLimitWindow{Pct: 30, ResetsAt: ptrInt64(now.Unix() + 4*24*3600)},
			},
		},
	}, nil)
	nextc, _ := mc.Update(tea.WindowSizeMsg{Width: 40, Height: 20})
	mc = nextc.(tuiModel)
	cstrip := mc.boardRateLimitStrip(now)
	if strings.Contains(cstrip, "\n") {
		t.Errorf("countdown strip must be one physical line at width 40, got a newline:\n%q", cstrip)
	}
	if w := visualWidth(cstrip); w > mc.width {
		t.Errorf("countdown strip visual width %d exceeds m.width %d:\n%q", w, mc.width, cstrip)
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
	strip := m.boardRateLimitStrip(time.Now())
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
	if s := none.boardRateLimitStrip(time.Now()); s != "" {
		t.Errorf("no tokens must yield no strip, got %q", s)
	}
	// (b) all tokens unreadable (Status != "ok").
	allDown := stripModel(t, []apitypes.TokenRateLimitDTO{
		{SecretID: "a", Label: "a", Limits: apitypes.RateLimitDTO{Status: "unavailable"}},
		{SecretID: "b", Label: "b", IsDefault: true, Limits: apitypes.RateLimitDTO{Status: "no_token"}},
	}, nil)
	if s := allDown.boardRateLimitStrip(time.Now()); s != "" {
		t.Errorf("all-unreadable tokens must yield no strip, got %q", s)
	}
	// (c) readable but nothing shown (a single non-default token not in the sidebar).
	hidden := stripModel(t, []apitypes.TokenRateLimitDTO{
		okMeter("sec-x", "hiddenlabel", false, 40, 40),
	}, nil)
	if s := hidden.boardRateLimitStrip(time.Now()); s != "" {
		t.Errorf("readable-but-not-shown token must yield no strip, got %q", s)
	}

	// The empty strip draws NO extra line: the board render has the wordmark then a blank, with
	// no strip line between. Compare against a control model that DOES draw a strip.
	emptyBoard := none.renderBoard()
	if strings.Contains(emptyBoard, "5h") || strings.Contains(emptyBoard, "7d") {
		t.Errorf("an empty strip must not draw any window cell into the board:\n%s", emptyBoard)
	}
}

// PRD #530 — the run-detail crew rail's stacked per-account rate-limit block (railRateMeters).
// It reuses the SAME #519 selection seam (selectedRateMeters) as the board strip, so the two
// surfaces cannot disagree, but stacks each account's 5h/7d windows vertically at rail width
// under an ACCOUNTS eyebrow, capped to whole entries so joinColumns' bottom-line clamp never
// leaves a half-drawn account.

// twoMilestones is a small frozen milestone list so renderMilestones is non-empty and the
// account block sits under it, matching the real rail layout.
func twoMilestones() []apitypes.Milestone {
	return []apitypes.Milestone{{ID: "m1", Title: "seam"}, {ID: "m2", Title: "rail block"}}
}

// railModel seeds a detail model with meters + sidebar selection and a frozen milestone list,
// then returns it at a comfortable 100x40 so transcriptViewport() is generous. renderLaneRail()
// takes the no-lanes branch (no frames seeded), so the account block sits directly under the
// milestone block.
func railModel(t *testing.T, meters []apitypes.TokenRateLimitDTO, sidebar []string) tuiModel {
	t.Helper()
	m := tuiTestModel(t, &uzicli.FakeClient{}, "run-detail")
	next, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	m = next.(tuiModel)
	next, _ = m.Update(rateLimitsMsg{tokens: meters})
	m = next.(tuiModel)
	next, _ = m.Update(settingsMsg{settings: apitypes.UserSettingsDTO{SidebarTokenIds: sidebar}})
	m = next.(tuiModel)
	next, _ = m.Update(detailLoadedMsg{run: apitypes.RunDTO{
		ID: "run-detail", Status: "running", Health: "ok", Milestones: twoMilestones(),
	}})
	m = next.(tuiModel)
	return m
}

// TestRailRateMetersDropsUnreadable — a token with Status != "ok" never appears in the rail
// block. The unreadable fixture is IsDefault:true (would clear the shown filter), so only the
// readable filter can drop it. A second readable default keeps the block non-empty.
func TestRailRateMetersDropsUnreadable(t *testing.T) {
	m := railModel(t, []apitypes.TokenRateLimitDTO{
		okMeter("sec-personal", "personal", true, 33, 61),
		{SecretID: "sec-down", Label: "throttled", IsDefault: true, Limits: apitypes.RateLimitDTO{
			Status: "unavailable", FiveHour: &apitypes.RateLimitWindow{Pct: 77}}},
	}, nil)
	out := stripANSI(m.renderLaneRail())
	if !strings.Contains(out, "ACCOUNTS") {
		t.Fatalf("readable account block missing its ACCOUNTS header:\n%s", out)
	}
	if !strings.Contains(out, "33%") {
		t.Errorf("readable default token's 5h pct 33%% missing from the rail:\n%s", out)
	}
	if strings.Contains(out, "throttled") {
		t.Errorf("a token with Status != \"ok\" leaked its label into the rail:\n%s", out)
	}
	if strings.Contains(out, "77%") {
		t.Errorf("a token with Status != \"ok\" leaked its window pct into the rail:\n%s", out)
	}
}

// TestRailRateMetersSelection — default AND a listed non-default show; an unlisted non-default
// does not (the shared #519 shown filter).
func TestRailRateMetersSelection(t *testing.T) {
	m := railModel(t, []apitypes.TokenRateLimitDTO{
		okMeter("sec-personal", "personal", true, 33, 61),
		okMeter("sec-meta", "metaacct", false, 87, 45),
		okMeter("sec-unlisted", "unlistedacct", false, 11, 22),
	}, []string{"sec-meta"})
	out := stripANSI(m.renderLaneRail())
	if !strings.Contains(out, "personal") {
		t.Errorf("the default token is not shown in the rail:\n%s", out)
	}
	if !strings.Contains(out, "metaacct") {
		t.Errorf("a non-default token listed in sidebar_token_ids is not shown in the rail:\n%s", out)
	}
	if strings.Contains(out, "unlistedacct") {
		t.Errorf("a non-default token NOT in sidebar_token_ids leaked into the rail:\n%s", out)
	}
}

// TestRailRateMetersLabelKeyedOffReadable — a single readable default shows NO label eyebrow
// (showLabel false) but still renders the 5h/7d cells and the ACCOUNTS header; two readable
// tokens force the label even when only the default is shown (showLabel keyed off readable).
func TestRailRateMetersLabelKeyedOffReadable(t *testing.T) {
	single := railModel(t, []apitypes.TokenRateLimitDTO{
		okMeter("sec-personal", "solorail", true, 33, 61),
	}, nil)
	singleOut := stripANSI(single.renderLaneRail())
	if strings.Contains(singleOut, "solorail") {
		t.Errorf("label rendered for a single readable token (showLabel must be false):\n%s", singleOut)
	}
	for _, want := range []string{"ACCOUNTS", "5h", "7d", "33%", "61%"} {
		if !strings.Contains(singleOut, want) {
			t.Errorf("single-token rail block missing %q:\n%s", want, singleOut)
		}
	}

	multi := railModel(t, []apitypes.TokenRateLimitDTO{
		okMeter("sec-personal", "labelledrail", true, 33, 61),
		okMeter("sec-unlisted", "unlistedrail", false, 11, 22),
	}, nil)
	multiOut := stripANSI(multi.renderLaneRail())
	if !strings.Contains(multiOut, "labelledrail") {
		t.Errorf("label absent when len(readable) > 1 (showLabel keyed off readable):\n%s", multiOut)
	}
}

// TestRailRateMetersNilWindow — a nil window renders `label -` via rateWindowCell, mirroring
// the board strip and windowPct.
func TestRailRateMetersNilWindow(t *testing.T) {
	m := railModel(t, []apitypes.TokenRateLimitDTO{
		{SecretID: "sec-personal", Label: "personal", IsDefault: true, Limits: apitypes.RateLimitDTO{
			Status: "ok", FiveHour: &apitypes.RateLimitWindow{Pct: 50}, SevenDay: nil}},
	}, nil)
	out := stripANSI(m.renderLaneRail())
	if !strings.Contains(out, "7d -") {
		t.Errorf("a nil 7d window must render `7d -` in the rail (mirroring windowPct):\n%s", out)
	}
}

// TestRailRateMetersAsciiSignalSurvives — under an Ascii (NO_COLOR) profile the always-present
// NN% text and the ▰/▱ bar glyphs survive in the DETAIL block, so the signal is legible when
// tone colour is gone. Mirrors TestBoardRateLimitStripAsciiSignalSurvives.
func TestRailRateMetersAsciiSignalSurvives(t *testing.T) {
	m := railModel(t, []apitypes.TokenRateLimitDTO{
		okMeter("sec-personal", "personal", true, 88, 20),
	}, nil)
	next, _ := m.Update(tea.ColorProfileMsg{Profile: colorprofile.Ascii})
	m = next.(tuiModel)
	rail := m.renderLaneRail()
	for _, want := range []string{"88%", "20%", "▰", "▱"} {
		if !strings.Contains(rail, want) {
			t.Errorf("Ascii-profile rail dropped the always-present cue %q:\n%q", want, rail)
		}
	}
}

// TestRailRateMetersEmptyCases — an empty selection renders NO account block AND no stray blank
// line at the rail's tail.
func TestRailRateMetersEmptyCases(t *testing.T) {
	assertNoBlock := func(t *testing.T, label string, m tuiModel) {
		t.Helper()
		if s := m.railRateMeters(time.Now(), 3); s != "" {
			t.Errorf("%s: railRateMeters must be empty, got %q", label, s)
		}
		out := m.renderLaneRail()
		if strings.Contains(out, "ACCOUNTS") || strings.Contains(out, "5h") || strings.Contains(out, "7d") {
			t.Errorf("%s: rail drew an account block for an empty selection:\n%s", label, out)
		}
		if strings.HasSuffix(out, "\n") {
			t.Errorf("%s: rail has a stray trailing newline with no account block:\n%q", label, out)
		}
	}
	// (a) no tokens at all.
	assertNoBlock(t, "no tokens", railModel(t, nil, nil))
	// (b) all tokens unreadable.
	assertNoBlock(t, "all unreadable", railModel(t, []apitypes.TokenRateLimitDTO{
		{SecretID: "a", Label: "a", Limits: apitypes.RateLimitDTO{Status: "unavailable"}},
		{SecretID: "b", Label: "b", IsDefault: true, Limits: apitypes.RateLimitDTO{Status: "no_token"}},
	}, nil))
	// (c) readable but nothing shown (a single non-default token not in the sidebar).
	assertNoBlock(t, "readable but hidden", railModel(t, []apitypes.TokenRateLimitDTO{
		okMeter("sec-x", "hiddenlabel", false, 40, 40),
	}, nil))
}

// railRunModel is railModel plus a run whose AnthropicSecretID/Label name the account the run is
// spending (PRD #623), so railRateMeters' force-show/highlight fold has a run to key off.
func railRunModel(t *testing.T, meters []apitypes.TokenRateLimitDTO, sidebar []string, runSecretID, runLabel string) tuiModel {
	t.Helper()
	m := tuiTestModel(t, &uzicli.FakeClient{}, "run-detail")
	next, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	m = next.(tuiModel)
	next, _ = m.Update(rateLimitsMsg{tokens: meters})
	m = next.(tuiModel)
	next, _ = m.Update(settingsMsg{settings: apitypes.UserSettingsDTO{SidebarTokenIds: sidebar}})
	m = next.(tuiModel)
	sid, lbl := runSecretID, runLabel
	next, _ = m.Update(detailLoadedMsg{run: apitypes.RunDTO{
		ID: "run-detail", Status: "running", Health: "ok", Milestones: twoMilestones(),
		AnthropicSecretID: &sid, AnthropicSecretLabel: &lbl,
	}})
	m = next.(tuiModel)
	return m
}

// TestRailRateMetersRunAccountHighlighted — PRD #623 M1. The account THIS run is spending is the
// FIRST entry under ACCOUNTS, its label rendered in tungsten normal weight (not faint), and it is
// force-shown even when it is NEITHER IsDefault NOR listed in sidebar_token_ids.
func TestRailRateMetersRunAccountHighlighted(t *testing.T) {
	// sec-run is the run's account: readable, but not default and not in the sidebar selection, so
	// only the PRD #623 force-show can surface it. sec-default is a shown sibling.
	m := railRunModel(t, []apitypes.TokenRateLimitDTO{
		okMeter("sec-default", "defaultacct", true, 33, 61),
		okMeter("sec-run", "runacct", false, 87, 45),
	}, nil, "sec-run", "runacct")

	block := m.railRateMeters(time.Now(), 3)
	plain := stripANSI(block)

	// (b) force-show: runacct is neither default nor listed, yet it appears.
	if !strings.Contains(plain, "runacct") {
		t.Fatalf("the run's deselected account was not force-shown in the rail:\n%s", plain)
	}
	// (a) first: runacct precedes the default sibling under the ACCOUNTS header.
	acctIdx := strings.Index(plain, "ACCOUNTS")
	runIdx := strings.Index(plain, "runacct")
	sibIdx := strings.Index(plain, "defaultacct")
	if acctIdx < 0 || sibIdx < 0 || acctIdx >= runIdx || runIdx >= sibIdx {
		t.Fatalf("the run's account must be the FIRST ACCOUNTS entry (ACCOUNTS=%d run=%d sibling=%d):\n%s",
			acctIdx, runIdx, sibIdx, plain)
	}
	// runacct must NOT be double-listed (it is IsDefault false here, but this also guards the
	// move-to-front dedupe from regressing).
	if n := strings.Count(plain, "runacct"); n != 1 {
		t.Errorf("the run's account appears %d times, want exactly 1 (dedupe/move-to-front):\n%s", n, plain)
	}
	// (a) tungsten SGR on the run label, faint on the sibling. Reproduce the exact styled lines the
	// render path builds and assert they are present verbatim.
	runLine := lipgloss.NewStyle().Foreground(m.pal.tungsten).Render(m.renderer.Plain("runacct", laneRailWidth))
	if !strings.Contains(block, runLine) {
		t.Errorf("the run's account label is not rendered in tungsten normal weight:\n%q", block)
	}
	sibLine := m.pal.faint.Render(m.renderer.Plain("defaultacct", laneRailWidth))
	if !strings.Contains(block, sibLine) {
		t.Errorf("a sibling account label is not rendered faint:\n%q", block)
	}
	// The run label must NOT carry the faint style (that would defeat the highlight).
	if runFaint := m.pal.faint.Render(m.renderer.Plain("runacct", laneRailWidth)); strings.Contains(block, runFaint) {
		t.Errorf("the run's account label is faint, not highlighted:\n%q", block)
	}
}

// TestRailRateMetersRunAccountSynthesized — PRD #623 M1 step 3c. With NO rate-limit rows seeded,
// the run's account still shows (force-show, single entry) from AnthropicSecretLabel with "-"
// meters, and its label carries the tungsten highlight even though showLabel is false.
func TestRailRateMetersRunAccountSynthesized(t *testing.T) {
	m := railRunModel(t, nil, nil, "sec-run", "lonelyacct")
	block := m.railRateMeters(time.Now(), 3)
	plain := stripANSI(block)
	if !strings.Contains(plain, "ACCOUNTS") || !strings.Contains(plain, "lonelyacct") {
		t.Fatalf("a run with no rate data must still show its synthesized account block:\n%s", plain)
	}
	if !strings.Contains(plain, "5h -") || !strings.Contains(plain, "7d -") {
		t.Errorf("the synthesized entry must render nil windows as `5h -`/`7d -`:\n%s", plain)
	}
	runLine := lipgloss.NewStyle().Foreground(m.pal.tungsten).Render(m.renderer.Plain("lonelyacct", laneRailWidth))
	if !strings.Contains(block, runLine) {
		t.Errorf("the single synthesized run label must render tungsten (unconditional, showLabel false):\n%q", block)
	}
}

// TestDetailHeaderDropsCredentialLabel — PRD #623 M2. The run-detail header's first line no longer
// renders the credential label; it now lives only in the rail ACCOUNTS block.
func TestDetailHeaderDropsCredentialLabel(t *testing.T) {
	runID := "dddddddd-2222"
	secretID, label := "sec-cred", "credlabel"
	m := tuiTestModel(t, &uzicli.FakeClient{}, runID)
	m.width = 200 // wide enough to combine into a single header row where the tag used to pin right
	next, _ := m.Update(detailLoadedMsg{run: apitypes.RunDTO{
		ID: runID, Kind: "issue", Status: "running", IssueTitle: "Short title",
		AnthropicSecretID: &secretID, AnthropicSecretLabel: &label,
	}})
	m = next.(tuiModel)
	for i, line := range m.detailHeaderLines() {
		if strings.Contains(stripANSI(line), label) {
			t.Errorf("header line %d still renders the credential label %q (PRD #623 removed it):\n%s", i, label, stripANSI(line))
		}
	}
}

// countWindows returns how many 5h and 7d window cells the rendered rail block carries. Every
// whole account entry contributes exactly one of each, so a mismatch means a half-drawn entry.
func countWindows(out string) (fiveH, sevenD int) {
	return strings.Count(out, "5h "), strings.Count(out, "7d ")
}

// TestRailRateMetersTruncatesAtWholeEntry — an over-full rail drops WHOLE account entries, never
// half of one. Seeded with several accounts and a small height so only some entries fit, the rail
// must (a) show strictly fewer accounts than seeded, yet (b) keep every visible account complete:
// its 5h cell count equals its 7d cell count equals its label-eyebrow count.
func TestRailRateMetersTruncatesAtWholeEntry(t *testing.T) {
	// One default + four sidebar-listed non-defaults: all readable, all shown, showLabel true.
	meters := []apitypes.TokenRateLimitDTO{
		okMeter("sec-0", "acctzero", true, 10, 11),
		okMeter("sec-1", "acctone", false, 20, 21),
		okMeter("sec-2", "accttwo", false, 30, 31),
		okMeter("sec-3", "acctthree", false, 40, 41),
		okMeter("sec-4", "acctfour", false, 50, 51),
	}
	sidebar := []string{"sec-1", "sec-2", "sec-3", "sec-4"}

	full := railModel(t, meters, sidebar)
	fullOut := stripANSI(full.renderLaneRail())
	full5h, full7d := countWindows(fullOut)
	if full5h != len(meters) || full7d != len(meters) {
		t.Fatalf("at full height all %d accounts must show, got 5h=%d 7d=%d:\n%s", len(meters), full5h, full7d, fullOut)
	}

	// Shrink the terminal so the viewport leaves room for only some whole entries. The exact
	// cutoff depends on chrome; sweep a band of small heights and require at least one that
	// truncates, and the whole-entry invariant on EVERY height.
	truncatedSeen := false
	for h := 14; h <= 26; h++ {
		next, _ := full.Update(tea.WindowSizeMsg{Width: 100, Height: h})
		m := next.(tuiModel)
		out := stripANSI(m.renderLaneRail())
		five, seven := countWindows(out)
		if five != seven {
			t.Fatalf("height %d left a half-drawn entry: 5h=%d != 7d=%d:\n%s", h, five, seven, out)
		}
		// When the block renders, its label-eyebrow count must equal its window count too, and it
		// must carry exactly one ACCOUNTS header.
		if seven > 0 {
			labels := 0
			for _, want := range []string{"acctzero", "acctone", "accttwo", "acctthree", "acctfour"} {
				if strings.Contains(out, want) {
					labels++
				}
			}
			if labels != seven {
				t.Fatalf("height %d: %d label eyebrows for %d 7d cells (half-entry):\n%s", h, labels, seven, out)
			}
			if n := strings.Count(out, "ACCOUNTS"); n != 1 {
				t.Fatalf("height %d: expected exactly one ACCOUNTS header, got %d:\n%s", h, n, out)
			}
		}
		if seven > 0 && seven < len(meters) {
			truncatedSeen = true
		}
	}
	if !truncatedSeen {
		t.Errorf("no small height truncated the block to a strict subset of whole entries; the cap never fired")
	}
}

// TestRailRateMetersSanitizesLabel — an account Label carrying control/bidi bytes is scrubbed by
// renderer.Plain (D7) before it reaches the rail, exactly as the board strip and lane rows do.
func TestRailRateMetersSanitizesLabel(t *testing.T) {
	// A second readable token forces showLabel so the eyebrow renders; the hostile label carries a
	// bidi override, an ESC-based SGR and a carriage return, all written as Go escapes (never raw).
	hostile := "acct\u202ehostile\x1b[31m\rmeta"
	m := railModel(t, []apitypes.TokenRateLimitDTO{
		okMeter("sec-personal", "benign", true, 33, 61),
		okMeter("sec-evil", hostile, false, 44, 55),
	}, []string{"sec-evil"})
	rail := m.renderLaneRail()
	for _, bad := range []string{"\u202e", "\x1b[31m", "\r"} {
		if strings.Contains(rail, bad) {
			t.Errorf("hostile label byte %q survived into the rail block; Plain (D7) did not scrub it:\n%q", bad, rail)
		}
	}
}

// TestSelectedRateMetersSharedByBoardAndRail — the anti-drift guarantee: selectedRateMeters()
// returns the SAME shown set and showLabel the board strip derives, from one shared fixture. The
// board strip and the rail block both consume this method, so they cannot disagree on selection.
func TestSelectedRateMetersSharedByBoardAndRail(t *testing.T) {
	meters := []apitypes.TokenRateLimitDTO{
		okMeter("sec-personal", "personal", true, 33, 61),
		okMeter("sec-meta", "metaacct", false, 87, 45),
		okMeter("sec-unlisted", "unlistedacct", false, 11, 22),
		{SecretID: "sec-down", Label: "down", IsDefault: true, Limits: apitypes.RateLimitDTO{Status: "no_token"}},
	}
	sidebar := []string{"sec-meta"}
	m := stripModel(t, meters, sidebar)

	shown, showLabel := m.selectedRateMeters()
	if !showLabel {
		t.Errorf("showLabel must be true (two readable tokens), got false")
	}
	gotIDs := make([]string, 0, len(shown))
	for _, t := range shown {
		gotIDs = append(gotIDs, t.SecretID)
	}
	wantIDs := []string{"sec-personal", "sec-meta"}
	if strings.Join(gotIDs, ",") != strings.Join(wantIDs, ",") {
		t.Errorf("selectedRateMeters shown set = %v, want %v", gotIDs, wantIDs)
	}
	// The board strip renders exactly this selection: both shown labels present, the unlisted and
	// the unreadable absent — proving the board consumes the same seam.
	board := stripANSI(m.boardRateLimitStrip(time.Now()))
	for _, want := range []string{"personal", "metaacct"} {
		if !strings.Contains(board, want) {
			t.Errorf("board strip missing shown token %q; it diverged from selectedRateMeters:\n%s", want, board)
		}
	}
	for _, bad := range []string{"unlistedacct", "down"} {
		if strings.Contains(board, bad) {
			t.Errorf("board strip drew %q, which selectedRateMeters excluded:\n%s", bad, board)
		}
	}
}

// TestRailRateMetersFullWidth — a full-rail account meter whose windows carry no reset time
// (okMeter builds them with nil ResetsAt) renders no countdown: each 5h/7d line is 18 visual
// cols (a railRateBarWidth (10) ▰/▱ bar + label + right-aligned percent), which stays within
// laneRailWidth (26). The server pct is right-aligned so the line ends flush with "  33%".
// The countdown case, where the row fills to the full 26, is TestRailRateMetersResetCountdown.
// This is the wider full-rail meter, distinct from the board strip's fixed 6-wide bar.
func TestRailRateMetersFullWidth(t *testing.T) {
	m := railModel(t, []apitypes.TokenRateLimitDTO{
		okMeter("sec-personal", "personal", true, 33, 61),
	}, nil)
	lines := strings.Split(stripANSI(m.renderLaneRail()), "\n")

	for _, tc := range []struct {
		prefix  string
		endsPct string
	}{
		{"5h ", "  33%"},
		{"7d ", "  61%"},
	} {
		var line string
		found := false
		for _, ln := range lines {
			if strings.HasPrefix(ln, tc.prefix) {
				line, found = ln, true
				break
			}
		}
		if !found {
			t.Fatalf("no %q account line in the rail:\n%s", tc.prefix, strings.Join(lines, "\n"))
		}
		if w := visualWidth(line); w > laneRailWidth {
			t.Errorf("%q line width %d exceeds the rail budget %d:\n%q", tc.prefix, w, laneRailWidth, line)
		}
		if bars := strings.Count(line, "▰") + strings.Count(line, "▱"); bars != railRateBarWidth {
			t.Errorf("%q bar run = %d glyphs, want railRateBarWidth %d:\n%q", tc.prefix, bars, railRateBarWidth, line)
		}
		if !strings.HasSuffix(line, tc.endsPct) {
			t.Errorf("%q line must end with the right-aligned percent %q:\n%q",
				tc.prefix, tc.endsPct, line)
		}
	}
}

// TestRailRateMetersResetCountdown — issue #588: when a window carries a ResetsAt, the full-rail
// meter appends a 2-col gap + 6-col right-aligned reset countdown after the percent, filling the
// row to the full laneRailWidth (26). A window with no ResetsAt renders no countdown (shorter row).
func TestRailRateMetersResetCountdown(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	m := railModel(t, []apitypes.TokenRateLimitDTO{
		{
			SecretID: "sec-personal", Label: "personal", IsDefault: true,
			Limits: apitypes.RateLimitDTO{
				Status:   "ok",
				FiveHour: &apitypes.RateLimitWindow{Pct: 33, ResetsAt: ptrInt64(now.Unix() + 2*3600 + 13*60)},
				SevenDay: &apitypes.RateLimitWindow{Pct: 61},
			},
		},
	}, nil)
	block := stripANSI(m.railRateMeters(now, 3))
	if !strings.Contains(block, "2h13m") {
		t.Fatalf("rail block missing the 5h reset countdown %q:\n%s", "2h13m", block)
	}
	lines := strings.Split(block, "\n")
	var five, seven string
	for _, ln := range lines {
		if strings.HasPrefix(ln, "5h ") {
			five = ln
		}
		if strings.HasPrefix(ln, "7d ") {
			seven = ln
		}
	}
	if five == "" || seven == "" {
		t.Fatalf("rail block missing a 5h/7d line:\n%s", block)
	}
	if w := visualWidth(five); w != laneRailWidth {
		t.Errorf("5h line with a countdown width = %d, want the full laneRailWidth %d:\n%q", w, laneRailWidth, five)
	}
	if !strings.HasSuffix(five, " 2h13m") {
		t.Errorf("5h line must end with the right-aligned countdown %q (6-col field):\n%q", " 2h13m", five)
	}
	// The 7d window has no ResetsAt: no countdown, ends with its right-aligned percent, within budget.
	if !strings.HasSuffix(seven, "  61%") {
		t.Errorf("7d line (nil ResetsAt) must end with its right-aligned percent %q:\n%q", "  61%", seven)
	}
	if w := visualWidth(seven); w > laneRailWidth {
		t.Errorf("7d line (no countdown) width %d exceeds the rail budget %d:\n%q", w, laneRailWidth, seven)
	}
}

// TestBoardRateLimitStripResetCountdown — issue #588: on the board strip a window with a ResetsAt
// gets a single space + bare reset duration after its NN%; a window without one gets no countdown.
func TestBoardRateLimitStripResetCountdown(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	m := stripModel(t, []apitypes.TokenRateLimitDTO{
		{
			SecretID: "sec-personal", Label: "personal", IsDefault: true,
			Limits: apitypes.RateLimitDTO{
				Status:   "ok",
				FiveHour: &apitypes.RateLimitWindow{Pct: 72, ResetsAt: ptrInt64(now.Unix() + 2*3600 + 13*60)},
				SevenDay: &apitypes.RateLimitWindow{Pct: 30},
			},
		},
	}, nil)
	strip := stripANSI(m.boardRateLimitStrip(now))
	if !strings.Contains(strip, "72% 2h13m") {
		t.Errorf("board strip must show the 5h percent immediately followed by its countdown %q:\n%s", "72% 2h13m", strip)
	}
	// The 7d window has no ResetsAt, so its percent carries no trailing duration token.
	if !strings.Contains(strip, "30%") {
		t.Errorf("board strip missing the 7d percent %q:\n%s", "30%", strip)
	}
	if strings.Count(strip, "2h13m") != 1 {
		t.Errorf("board strip must carry exactly one reset countdown (only the 5h window has ResetsAt):\n%s", strip)
	}
}

// TestBoardRateLimitStripBarStaysSix — parametrizing the meter widths must NOT touch the board's
// two-up rate strip: a 100%% window there is still exactly rateBarWidth (6) ▰ glyphs, never the
// rail's railRateBarWidth (10) bar. Guards that rateWindowCell(barW=rateBarWidth, pctW=0) is byte-unchanged.
func TestBoardRateLimitStripBarStaysSix(t *testing.T) {
	m := stripModel(t, []apitypes.TokenRateLimitDTO{
		okMeter("sec-personal", "personal", true, 100, 100),
	}, nil)
	out := stripANSI(m.View().Content)
	if !strings.Contains(out, strings.Repeat("▰", rateBarWidth)) {
		t.Errorf("board strip 100%% window must render a %d-glyph filled bar:\n%s", rateBarWidth, out)
	}
	if strings.Contains(out, strings.Repeat("▰", rateBarWidth+1)) {
		t.Errorf("board strip must NOT widen its bar beyond rateBarWidth (%d):\n%s", rateBarWidth, out)
	}
	if strings.Contains(out, strings.Repeat("▰", railRateBarWidth)) {
		t.Errorf("board strip must NOT carry the rail's %d-wide bar:\n%s", railRateBarWidth, out)
	}
}
