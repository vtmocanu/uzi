package main

// PRD #1209 M3 — the Codex per-account rate-limit meters shown beside the Claude meters on
// the board strip (boardCodexMeterSeg) and the detail rail (railCodexRateMeters). The
// selection mirrors the Claude #519 seam via selectedCodexRateMeters: readable = a status
// carrying a reading (fresh|stale), nothing readable → nothing; showLabel keyed off readable
// (>1); shown = readable filtered by (IsDefault || AccountID ∈ sidebar_codex_account_ids),
// deduped to one meter per account. A stale reading is shown DIMMED; a nil/no-reading window
// renders "—", never 0. Aliases and bucket display names ride renderer.Plain (D7).

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// cwin builds a Codex window carrying a whole-percent reading.
func cwin(pct int) *apitypes.CodexRateLimitWindowDTO {
	return &apitypes.CodexRateLimitWindowDTO{UsedPercent: fPtr(float64(pct))}
}

// codexAcct builds one Codex account with a single "5h" bucket carrying the given primary
// and secondary windows (either may be nil).
func codexAcct(accountID, alias string, isDefault bool, status string, primary, secondary *apitypes.CodexRateLimitWindowDTO) apitypes.CodexAccountRateLimitDTO {
	return apitypes.CodexAccountRateLimitDTO{
		AccountID: accountID, Aliases: []string{alias}, IsDefault: isDefault, Status: status,
		Buckets: []apitypes.CodexRateLimitBucketDTO{
			{ID: "5h", DisplayName: "5h", Primary: primary, Secondary: secondary},
		},
	}
}

// codexStripModel feeds Codex meters + the Codex sidebar selection into a fresh board model
// at a wide width (so the strip is not clamped in cell-value assertions).
func codexStripModel(t *testing.T, accts []apitypes.CodexAccountRateLimitDTO, sidebar []string) tuiModel {
	t.Helper()
	m := tuiTestModel(t, nil, "")
	m.width = 200
	next, _ := m.Update(codexRateLimitsMsg{accounts: accts})
	m = next.(tuiModel)
	next, _ = m.Update(settingsMsg{settings: apitypes.UserSettingsDTO{SidebarCodexAccountIds: sidebar}})
	m = next.(tuiModel)
	return m
}

// codexRailModel seeds a detail model with Codex meters + selection and a frozen milestone
// list, at a comfortable 100x40, so railCodexRateMeters sits under the milestone block.
func codexRailModel(t *testing.T, accts []apitypes.CodexAccountRateLimitDTO, sidebar []string) tuiModel {
	t.Helper()
	m := tuiTestModel(t, &uzicli.FakeClient{}, "run-detail")
	next, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	m = next.(tuiModel)
	next, _ = m.Update(codexRateLimitsMsg{accounts: accts})
	m = next.(tuiModel)
	next, _ = m.Update(settingsMsg{settings: apitypes.UserSettingsDTO{SidebarCodexAccountIds: sidebar}})
	m = next.(tuiModel)
	m = applyDetail(m, apitypes.RunDTO{
		ID: "run-detail", Status: "running", Health: "ok", Milestones: twoMilestones(),
	}, nil)
	return m
}

// TestBoardCodexStripDropsUnreadable — an account whose status carries no reading
// (no_reading here) never appears; the default fresh account still renders. The unreadable
// fixture is IsDefault:true so it clears the shown filter — only the readable filter can drop
// it.
func TestBoardCodexStripDropsUnreadable(t *testing.T) {
	accts := []apitypes.CodexAccountRateLimitDTO{
		codexAcct("cx-primary", "primary", true, "fresh", cwin(33), cwin(61)),
		{AccountID: "cx-dead", Aliases: []string{"deadacct"}, IsDefault: true, Status: "no_reading"},
	}
	strip := stripANSI(codexStripModel(t, accts, nil).boardCodexMeterSeg(time.Now()))
	if !strings.Contains(strip, "33%") {
		t.Fatalf("readable default account's primary pct 33%% missing:\n%s", strip)
	}
	if strings.Contains(strip, "deadacct") {
		t.Errorf("a no_reading account leaked its alias into the Codex strip:\n%s", strip)
	}
}

// TestBoardCodexStripSelection — default AND a listed non-default show; an unlisted
// non-default does not. showLabel is true (two readable), so the aliases render.
func TestBoardCodexStripSelection(t *testing.T) {
	accts := []apitypes.CodexAccountRateLimitDTO{
		codexAcct("cx-primary", "primary", true, "fresh", cwin(33), cwin(61)),
		codexAcct("cx-team", "teamacct", false, "fresh", cwin(80), cwin(45)),
		codexAcct("cx-unlisted", "unlistedacct", false, "fresh", cwin(11), cwin(22)),
	}
	strip := stripANSI(codexStripModel(t, accts, []string{"cx-team"}).boardCodexMeterSeg(time.Now()))
	if !strings.Contains(strip, "Codex") {
		t.Errorf("the Codex provider label is missing:\n%s", strip)
	}
	if !strings.Contains(strip, "primary") {
		t.Errorf("the default account is not shown:\n%s", strip)
	}
	if !strings.Contains(strip, "teamacct") {
		t.Errorf("a non-default account listed in sidebar_codex_account_ids is not shown:\n%s", strip)
	}
	if strings.Contains(strip, "unlistedacct") {
		t.Errorf("an unlisted non-default account leaked into the Codex strip:\n%s", strip)
	}
}

// TestBoardCodexStripDedupOneMeterPerAccount — an account both IsDefault AND listed in the
// sidebar appears exactly once (the selection dedups on AccountID).
func TestBoardCodexStripDedupOneMeterPerAccount(t *testing.T) {
	accts := []apitypes.CodexAccountRateLimitDTO{
		codexAcct("cx-primary", "onlyacct", true, "fresh", cwin(33), cwin(61)),
	}
	m := codexStripModel(t, accts, []string{"cx-primary"}) // default AND listed
	shown, _ := m.selectedCodexRateMeters()
	if len(shown) != 1 {
		t.Fatalf("an account both default and listed must appear once, got %d: %+v", len(shown), shown)
	}
	// A single readable account → showLabel false, so no alias eyebrow, but the windows render.
	strip := stripANSI(m.boardCodexMeterSeg(time.Now()))
	if strings.Contains(strip, "onlyacct") {
		t.Errorf("alias rendered for a single readable account (showLabel must be false):\n%s", strip)
	}
	if !strings.Contains(strip, "33%") || !strings.Contains(strip, "61%") {
		t.Errorf("the single account's windows are missing:\n%s", strip)
	}
}

// TestBoardCodexStripTonePct — a danger-band window (88 ≥ 85) fills with m.pal.alarm and an
// ok-band window (20 < 40) with m.pal.sage; both server-rounded percents render. Asserted at
// the exact paintSeg fragment level, mirroring the Claude strip tone test.
func TestBoardCodexStripTonePct(t *testing.T) {
	m := codexStripModel(t, []apitypes.CodexAccountRateLimitDTO{
		codexAcct("cx-primary", "primary", true, "fresh", cwin(88), cwin(20)),
	}, nil)
	seg := m.boardCodexMeterSeg(time.Now())
	plain := stripANSI(seg)
	for _, want := range []string{"88%", "20%"} {
		if !strings.Contains(plain, want) {
			t.Errorf("window pct %q missing from the Codex strip:\n%s", want, plain)
		}
	}
	alarmFilled, _ := rateBarParts(88, rateBarWidth)
	if frag := paintSeg(m.pal.alarm, nil, false, alarmFilled); !strings.Contains(seg, frag) {
		t.Errorf("danger-band window (88) is not painted with m.pal.alarm; want %q in:\n%q", frag, seg)
	}
	sageFilled, _ := rateBarParts(20, rateBarWidth)
	if frag := paintSeg(m.pal.sage, nil, false, sageFilled); !strings.Contains(seg, frag) {
		t.Errorf("ok-band window (20) is not painted with m.pal.sage; want %q in:\n%q", frag, seg)
	}
}

// TestBoardCodexStripStaleDimmed — a selected stale account is shown (readable) but its
// window bar is DIMMED to faint rather than tone-coloured, so a high-utilization stale
// reading does not light an alarm the reading can no longer justify.
func TestBoardCodexStripStaleDimmed(t *testing.T) {
	m := codexStripModel(t, []apitypes.CodexAccountRateLimitDTO{
		codexAcct("cx-old", "archive", true, "stale", cwin(88), nil),
	}, nil)
	seg := m.boardCodexMeterSeg(time.Now())
	if !strings.Contains(stripANSI(seg), "88%") {
		t.Fatalf("a selected stale account must still show its reading:\n%s", stripANSI(seg))
	}
	filled, _ := rateBarParts(88, rateBarWidth)
	if frag := paintSeg(m.pal.faintC, nil, false, filled); !strings.Contains(seg, frag) {
		t.Errorf("a stale account's bar must be dimmed (faint), want %q in:\n%q", frag, seg)
	}
	if frag := paintSeg(m.pal.alarm, nil, false, filled); strings.Contains(seg, frag) {
		t.Errorf("a stale account's bar must NOT be tone-coloured (found alarm fill):\n%q", seg)
	}
}

// TestBoardCodexStripNilWindow — a present window with no reading (used_percent null)
// renders "P —", never "P 0%": partial/unknown is preserved.
func TestBoardCodexStripNilWindow(t *testing.T) {
	m := codexStripModel(t, []apitypes.CodexAccountRateLimitDTO{
		codexAcct("cx-primary", "primary", true, "fresh",
			&apitypes.CodexRateLimitWindowDTO{UsedPercent: nil}, nil),
	}, nil)
	strip := stripANSI(m.boardCodexMeterSeg(time.Now()))
	if !strings.Contains(strip, "P —") {
		t.Errorf("a no-reading window must render `P —`:\n%s", strip)
	}
	if strings.Contains(strip, "P 0%") {
		t.Errorf("a no-reading window must NOT render 0%%:\n%s", strip)
	}
}

// TestBoardCodexStripEmptyCases — no Codex segment when nothing is readable or shown.
func TestBoardCodexStripEmptyCases(t *testing.T) {
	// (a) no accounts.
	if s := codexStripModel(t, nil, nil).boardCodexMeterSeg(time.Now()); s != "" {
		t.Errorf("no accounts must yield no Codex segment, got %q", s)
	}
	// (b) all unreadable.
	allDown := codexStripModel(t, []apitypes.CodexAccountRateLimitDTO{
		{AccountID: "a", Aliases: []string{"a"}, Status: "no_reading"},
		{AccountID: "b", Aliases: []string{"b"}, IsDefault: true, Status: "pending"},
	}, nil)
	if s := allDown.boardCodexMeterSeg(time.Now()); s != "" {
		t.Errorf("all-unreadable accounts must yield no Codex segment, got %q", s)
	}
	// (c) readable but nothing shown (a single non-default account not in the sidebar).
	hidden := codexStripModel(t, []apitypes.CodexAccountRateLimitDTO{
		codexAcct("cx-x", "hiddenacct", false, "fresh", cwin(40), nil),
	}, nil)
	if s := hidden.boardCodexMeterSeg(time.Now()); s != "" {
		t.Errorf("a readable-but-not-shown account must yield no Codex segment, got %q", s)
	}
}

// TestBoardCodexStripRidesSameLineAsClaude — the Codex meters ride the SAME single strip line
// after the Claude ones, and a Codex-only viewer (no readable Claude token) still gets the
// strip. Also pins the strip to exactly one physical line.
func TestBoardCodexStripRidesSameLineAsClaude(t *testing.T) {
	// Codex-only: no Claude meters seeded, so the strip is the Codex section alone.
	m := codexStripModel(t, []apitypes.CodexAccountRateLimitDTO{
		codexAcct("cx-primary", "primary", true, "fresh", cwin(33), cwin(61)),
	}, nil)
	strip := m.boardRateLimitStrip(time.Now())
	if strip == "" {
		t.Fatalf("a Codex-only viewer must still get a rate-limit strip")
	}
	if strings.Contains(strip, "\n") {
		t.Errorf("the strip must be one physical line:\n%q", strip)
	}
	plain := stripANSI(strip)
	if !strings.Contains(plain, "Codex") || !strings.Contains(plain, "33%") {
		t.Errorf("the Codex section is missing from the combined strip:\n%s", plain)
	}

	// Claude + Codex: both providers render, with the Claude 5h/7d ahead of the Codex section.
	next, _ := m.Update(rateLimitsMsg{tokens: []apitypes.TokenRateLimitDTO{
		okMeter("sec-personal", "claudeacct", true, 44, 55),
	}})
	both := next.(tuiModel)
	bs := stripANSI(both.boardRateLimitStrip(time.Now()))
	claudeIdx := strings.Index(bs, "44%")
	codexIdx := strings.Index(bs, "Codex")
	if claudeIdx < 0 || codexIdx < 0 || claudeIdx >= codexIdx {
		t.Errorf("Claude meters must precede the Codex section (claude=%d codex=%d):\n%s", claudeIdx, codexIdx, bs)
	}
}

// TestBoardCodexStripAsciiSignalSurvives — under an Ascii (NO_COLOR) profile the
// always-present NN% text and the ▰/▱ bar glyphs survive, so the signal is legible when tone
// colour is gone. Mirrors the Claude strip's Ascii test.
func TestBoardCodexStripAsciiSignalSurvives(t *testing.T) {
	m := codexStripModel(t, []apitypes.CodexAccountRateLimitDTO{
		codexAcct("cx-primary", "primary", true, "fresh", cwin(88), cwin(20)),
	}, nil)
	next, _ := m.Update(tea.ColorProfileMsg{Profile: colorprofile.Ascii})
	m = next.(tuiModel)
	seg := m.boardCodexMeterSeg(time.Now())
	for _, want := range []string{"88%", "20%", "▰", "▱"} {
		if !strings.Contains(seg, want) {
			t.Errorf("Ascii-profile Codex strip dropped the always-present cue %q:\n%q", want, seg)
		}
	}
}

// TestSelectedCodexRateMetersSharedByBoardAndRail — the anti-drift guarantee: the board strip
// and the detail rail both consume selectedCodexRateMeters, so they cannot disagree on the
// shown set or showLabel.
func TestSelectedCodexRateMetersSharedByBoardAndRail(t *testing.T) {
	accts := []apitypes.CodexAccountRateLimitDTO{
		codexAcct("cx-primary", "primary", true, "fresh", cwin(33), cwin(61)),
		codexAcct("cx-team", "teamacct", false, "fresh", cwin(87), cwin(45)),
		codexAcct("cx-unlisted", "unlistedacct", false, "fresh", cwin(11), cwin(22)),
		{AccountID: "cx-dead", Aliases: []string{"deadacct"}, IsDefault: true, Status: "no_reading"},
	}
	sidebar := []string{"cx-team"}

	board := codexStripModel(t, accts, sidebar)
	shown, showLabel := board.selectedCodexRateMeters()
	if !showLabel {
		t.Errorf("showLabel must be true (three readable accounts), got false")
	}
	gotIDs := make([]string, 0, len(shown))
	for _, a := range shown {
		gotIDs = append(gotIDs, a.AccountID)
	}
	if strings.Join(gotIDs, ",") != "cx-primary,cx-team" {
		t.Errorf("selectedCodexRateMeters shown = %v, want [cx-primary cx-team]", gotIDs)
	}
	// Board strip draws exactly this selection.
	bs := stripANSI(board.boardCodexMeterSeg(time.Now()))
	for _, want := range []string{"primary", "teamacct"} {
		if !strings.Contains(bs, want) {
			t.Errorf("board Codex strip missing shown account %q:\n%s", want, bs)
		}
	}
	for _, bad := range []string{"unlistedacct", "deadacct"} {
		if strings.Contains(bs, bad) {
			t.Errorf("board Codex strip drew excluded account %q:\n%s", bad, bs)
		}
	}
	// Rail draws the same selection under a CODEX header.
	rail := stripANSI(codexRailModel(t, accts, sidebar).renderLaneRail())
	if !strings.Contains(rail, "CODEX") {
		t.Fatalf("the rail Codex block is missing its CODEX header:\n%s", rail)
	}
	for _, want := range []string{"primary", "teamacct"} {
		if !strings.Contains(rail, want) {
			t.Errorf("rail Codex block missing shown account %q:\n%s", want, rail)
		}
	}
	for _, bad := range []string{"unlistedacct", "deadacct"} {
		if strings.Contains(rail, bad) {
			t.Errorf("rail Codex block drew excluded account %q:\n%s", bad, rail)
		}
	}
}

// TestRailCodexRateMetersRendersReading — the rail Codex block renders the shown accounts'
// bucket windows (percent text present) under the CODEX header, and drops unreadable accounts.
func TestRailCodexRateMetersRendersReading(t *testing.T) {
	rail := stripANSI(codexRailModel(t, []apitypes.CodexAccountRateLimitDTO{
		codexAcct("cx-primary", "primary", true, "fresh", cwin(33), cwin(61)),
		{AccountID: "cx-dead", Aliases: []string{"deadacct"}, IsDefault: true, Status: "no_reading"},
	}, nil).renderLaneRail())
	if !strings.Contains(rail, "CODEX") {
		t.Fatalf("rail Codex block header missing:\n%s", rail)
	}
	for _, want := range []string{"33%", "61%"} {
		if !strings.Contains(rail, want) {
			t.Errorf("rail Codex block missing window pct %q:\n%s", want, rail)
		}
	}
	if strings.Contains(rail, "deadacct") {
		t.Errorf("a no_reading account leaked into the rail Codex block:\n%s", rail)
	}
}

// TestRailCodexRateMetersNilWindow — a present window with no reading renders "P —" in the
// rail too, mirroring the board strip and the CLI table.
func TestRailCodexRateMetersNilWindow(t *testing.T) {
	rail := stripANSI(codexRailModel(t, []apitypes.CodexAccountRateLimitDTO{
		codexAcct("cx-primary", "primary", true, "fresh",
			&apitypes.CodexRateLimitWindowDTO{UsedPercent: nil}, nil),
	}, nil).renderLaneRail())
	if !strings.Contains(rail, "P —") {
		t.Errorf("a no-reading window must render `P —` in the rail:\n%s", rail)
	}
	if strings.Contains(rail, "P 0%") {
		t.Errorf("a no-reading window must NOT render 0%% in the rail:\n%s", rail)
	}
}

// TestCodexMetersSanitizeUntrustedText — a hostile alias AND a hostile bucket display name,
// each carrying control + bidi bytes, are scrubbed by renderer.Plain (D7) before they reach
// EITHER the board strip or the detail rail. The account is default+sidebar-listed and a
// second readable account forces showLabel so both the alias eyebrow and the bucket name
// eyebrow actually render (a non-vacuous test).
func TestCodexMetersSanitizeUntrustedText(t *testing.T) {
	hostileAlias := "acct\u202ehostile\x1b[31m\rmeta"
	hostileBucket := "5h\u202e\x07evil\x1b[32m"
	nasty := codexAcct("cx-evil", hostileAlias, true, "fresh", cwin(44), cwin(55))
	nasty.Buckets[0].DisplayName = hostileBucket
	accts := []apitypes.CodexAccountRateLimitDTO{
		nasty,
		codexAcct("cx-second", "benignacct", false, "fresh", cwin(12), cwin(20)),
	}
	sidebar := []string{"cx-second"}

	board := codexStripModel(t, accts, sidebar).boardCodexMeterSeg(time.Now())
	rail := codexRailModel(t, accts, sidebar).renderLaneRail()
	for _, surface := range []struct{ name, out string }{{"board", board}, {"rail", rail}} {
		for _, bad := range []string{"\u202e", "\x1b[31m", "\x1b[32m", "\x07", "\r"} {
			if strings.Contains(surface.out, bad) {
				t.Errorf("%s: hostile byte %q survived; Plain (D7) did not scrub it:\n%q", surface.name, bad, surface.out)
			}
		}
		// The sanitized residue must still be present, proving the untrusted field was drawn
		// (and folded), not silently dropped.
		if !strings.Contains(stripANSI(surface.out), "hostile") {
			t.Errorf("%s: the sanitized alias residue is missing — the alias was not drawn:\n%s", surface.name, stripANSI(surface.out))
		}
	}
}
