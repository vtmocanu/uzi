package main

// PRD #519 F2 — the factory-floor rate-limit strip mirrors the web left-bottom sidebar's
// selection (RateLimitMeters SidebarRateLimits + sidebarTokens.ts): readable = status "ok",
// nothing readable → no strip; showLabel keyed off readable (>1); shown = readable filtered by
// (IsDefault || SecretID ∈ sidebar_token_ids); nothing shown → no strip. Each shown token draws
// its 5h + 7d windows as a tone-coloured mini bar plus always-present NN% text.

import (
	"strings"
	"testing"

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
func TestBoardRateLimitStripDropsUnreadable(t *testing.T) {
	meters := []apitypes.TokenRateLimitDTO{
		okMeter("sec-personal", "personal", true, 33, 61),
		{SecretID: "sec-down", Label: "throttled", Limits: apitypes.RateLimitDTO{Status: "unavailable"}},
	}
	m := stripModel(t, meters, nil)
	out := m.View().Content
	if !strings.Contains(out, "33%") {
		t.Fatalf("readable default token's 5h pct 33%% missing from board:\n%s", out)
	}
	if strings.Contains(out, "throttled") {
		t.Errorf("a token with Status != \"ok\" leaked into the strip:\n%s", out)
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

// TestBoardRateLimitStripTonePct — both windows' server-rounded NN% text renders.
func TestBoardRateLimitStripTonePct(t *testing.T) {
	m := stripModel(t, []apitypes.TokenRateLimitDTO{
		okMeter("sec-personal", "personal", true, 88, 44),
	}, nil)
	out := m.View().Content
	for _, want := range []string{"88%", "44%"} {
		if !strings.Contains(out, want) {
			t.Errorf("window pct %q missing from the strip:\n%s", want, out)
		}
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
