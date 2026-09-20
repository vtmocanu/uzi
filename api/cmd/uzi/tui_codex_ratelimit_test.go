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

// TestBoardCodexStripDedupOneMeterPerAccount — TWO m.codexRateLimits rows carrying the SAME
// AccountID (one default, one sidebar-listed) collapse to ONE meter: the `seen` map in
// selectedCodexRateMeters keeps the first and drops the duplicate. Feeding two same-id rows is
// what actually exercises the dedup — a single-row fixture never reaches the `seen` guard, so it
// could pass with the dedup deleted.
func TestBoardCodexStripDedupOneMeterPerAccount(t *testing.T) {
	accts := []apitypes.CodexAccountRateLimitDTO{
		codexAcct("cx-dup", "dupacct", true, "fresh", cwin(33), cwin(61)),  // default → clears the shown filter
		codexAcct("cx-dup", "dupacct", false, "fresh", cwin(77), cwin(88)), // SAME id, sidebar-listed → also clears it
	}
	m := codexStripModel(t, accts, []string{"cx-dup"}) // the non-default duplicate is sidebar-listed
	shown, _ := m.selectedCodexRateMeters()
	if len(shown) != 1 {
		t.Fatalf("two rows with the same AccountID must dedup to one meter, got %d: %+v", len(shown), shown)
	}
	// Exactly one per-account meter renders: the accent bar ▎ prefixes each account segment, so
	// there must be exactly one in the strip.
	seg := stripANSI(m.boardCodexMeterSeg(time.Now()))
	if n := strings.Count(seg, "▎"); n != 1 {
		t.Errorf("expected exactly one Codex account meter (one ▎ accent bar), got %d:\n%s", n, seg)
	}
	// The kept meter is the FIRST row (its 33%/61% windows); the dropped duplicate's 77%/88% must
	// not appear.
	if !strings.Contains(seg, "33%") || !strings.Contains(seg, "61%") {
		t.Errorf("the first (kept) account's windows are missing:\n%s", seg)
	}
	if strings.Contains(seg, "77%") || strings.Contains(seg, "88%") {
		t.Errorf("the dropped duplicate row's windows leaked into the strip:\n%s", seg)
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

// TestBoardCodexStripOwnLine — the Codex meters ride their OWN board strip line
// (boardCodexRateLimitStrip), separate from the Claude strip (boardRateLimitStrip), so at a
// standard width the Codex percentages stay legible instead of being clipped off the end of a
// combined line (PRD #1209 M3). The Claude strip carries NO Codex bytes, each strip is one
// physical line, and a Codex-only viewer (no readable Claude token) still gets its Codex line.
func TestBoardCodexStripOwnLine(t *testing.T) {
	// Codex-only: no Claude meters seeded. The Claude strip is empty; the Codex strip carries it.
	m := codexStripModel(t, []apitypes.CodexAccountRateLimitDTO{
		codexAcct("cx-primary", "primary", true, "fresh", cwin(33), cwin(61)),
	}, nil)
	if claude := m.boardRateLimitStrip(time.Now()); claude != "" {
		t.Errorf("no readable Claude token: the Claude strip must be empty, got %q", claude)
	}
	codex := m.boardCodexRateLimitStrip(time.Now())
	if codex == "" {
		t.Fatalf("a Codex-only viewer must still get a Codex strip line")
	}
	if strings.Contains(codex, "\n") {
		t.Errorf("the Codex strip must be one physical line:\n%q", codex)
	}
	if plain := stripANSI(codex); !strings.Contains(plain, "Codex") || !strings.Contains(plain, "33%") {
		t.Errorf("the Codex line is missing its label or reading:\n%s", plain)
	}

	// Claude + Codex: the two providers render on SEPARATE lines. The Claude strip carries the
	// Claude reading and NO "Codex" label; the Codex strip carries the Codex reading.
	next, _ := m.Update(rateLimitsMsg{tokens: []apitypes.TokenRateLimitDTO{
		okMeter("sec-personal", "claudeacct", true, 44, 55),
	}})
	both := next.(tuiModel)
	claudeLine := stripANSI(both.boardRateLimitStrip(time.Now()))
	codexLine := stripANSI(both.boardCodexRateLimitStrip(time.Now()))
	if !strings.Contains(claudeLine, "44%") {
		t.Errorf("the Claude strip must carry the Claude reading:\n%s", claudeLine)
	}
	if strings.Contains(claudeLine, "Codex") {
		t.Errorf("the Claude strip must NOT carry the Codex section (it rides its own line):\n%s", claudeLine)
	}
	if !strings.Contains(codexLine, "Codex") || !strings.Contains(codexLine, "33%") {
		t.Errorf("the Codex strip must carry the Codex section:\n%s", codexLine)
	}

	// In the composed board, the Claude line is drawn BEFORE the Codex line (Claude above Codex).
	both.width, both.height = 100, 40
	board := stripANSI(both.renderBoard())
	claudeIdx := strings.Index(board, "44%")
	codexIdx := strings.Index(board, "Codex")
	if claudeIdx < 0 || codexIdx < 0 || claudeIdx >= codexIdx {
		t.Errorf("the Claude strip must be drawn above the Codex line (claude=%d codex=%d):\n%s", claudeIdx, codexIdx, board)
	}
}

// TestBoardCodexReachableAtStandardWidth is the PRD #1209 M3 acceptance test for FIX 1: at width
// 100 with TWO readable Claude tokens AND TWO selected readable Codex accounts, a Codex account's
// percentage digits are present in the composed board render — i.e. the Codex data is reachable,
// not clipped to just the "Codex" label the way a single combined strip line was at ≤120 cols. The
// Codex percentages here (71/29, 66/13) are chosen to NOT collide with the Claude readings, so a
// hit proves the Codex bytes themselves survived, not an accidental substring match.
func TestBoardCodexReachableAtStandardWidth(t *testing.T) {
	m := tuiTestModel(t, &uzicli.FakeClient{}, "")
	m.width, m.height = 100, 40
	next, _ := m.Update(rateLimitsMsg{tokens: []apitypes.TokenRateLimitDTO{
		okMeter("sec-personal", "personal", true, 35, 62),
		okMeter("sec-meta", "meta", false, 88, 44),
	}})
	m = next.(tuiModel)
	next, _ = m.Update(codexRateLimitsMsg{accounts: []apitypes.CodexAccountRateLimitDTO{
		codexAcct("cx-primary", "primary", true, "fresh", cwin(71), cwin(29)),
		codexAcct("cx-team", "team", false, "fresh", cwin(66), cwin(13)),
	}})
	m = next.(tuiModel)
	next, _ = m.Update(settingsMsg{settings: apitypes.UserSettingsDTO{
		SidebarTokenIds:        []string{"sec-meta"},
		SidebarCodexAccountIds: []string{"cx-team"},
	}})
	m = next.(tuiModel)

	board := stripANSI(m.renderBoard())
	if !strings.Contains(board, "Codex") {
		t.Fatalf("the Codex provider label is missing from the width-100 board:\n%s", board)
	}
	// The load-bearing assertion: a Codex account's utilization percentage is actually present,
	// not clipped away. Both the default account's window (71%) and the listed account's (66%).
	for _, want := range []string{"71%", "66%"} {
		if !strings.Contains(board, want) {
			t.Errorf("Codex reading %q was clipped from the width-100 board (unreachable):\n%s", want, board)
		}
	}
}

// TestBoardCodexStripReservesOwnRow is the FIX 1 row-accounting acceptance test: showing a Codex
// strip must drop boardCapacity by EXACTLY one (reserve one extra physical line), the same way the
// Claude strip's row is reserved, so the second provider line never overdraws the run list.
func TestBoardCodexStripReservesOwnRow(t *testing.T) {
	m := tuiTestModel(t, &uzicli.FakeClient{}, "")
	m.width, m.height = 100, 40
	// Two readable Claude tokens shown, so the Claude strip already reserves its own row.
	next, _ := m.Update(rateLimitsMsg{tokens: []apitypes.TokenRateLimitDTO{
		okMeter("sec-personal", "personal", true, 35, 62),
		okMeter("sec-meta", "meta", false, 88, 44),
	}})
	m = next.(tuiModel)
	next, _ = m.Update(settingsMsg{settings: apitypes.UserSettingsDTO{SidebarTokenIds: []string{"sec-meta"}}})
	m = next.(tuiModel)
	if m.boardCodexRateLimitStrip(time.Now()) != "" {
		t.Fatalf("precondition: no Codex accounts, so the Codex strip must be empty")
	}
	withoutCodex := m.boardCapacity()

	// Add two selected readable Codex accounts → the Codex strip renders on its own line.
	next, _ = m.Update(codexRateLimitsMsg{accounts: []apitypes.CodexAccountRateLimitDTO{
		codexAcct("cx-primary", "primary", true, "fresh", cwin(71), cwin(29)),
		codexAcct("cx-team", "team", false, "fresh", cwin(66), cwin(13)),
	}})
	m = next.(tuiModel)
	next, _ = m.Update(settingsMsg{settings: apitypes.UserSettingsDTO{
		SidebarTokenIds:        []string{"sec-meta"},
		SidebarCodexAccountIds: []string{"cx-team"},
	}})
	m = next.(tuiModel)
	if m.boardCodexRateLimitStrip(time.Now()) == "" {
		t.Fatalf("precondition: two selected readable Codex accounts, so the Codex strip must render")
	}
	withCodex := m.boardCapacity()

	if withoutCodex-withCodex != 1 {
		t.Errorf("showing a Codex strip must reserve exactly one more row: capacity %d → %d (want a drop of 1)", withoutCodex, withCodex)
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

// codexFoldRun builds the FIX-2 fixture: a running, metered run with a milestone list, the run's
// own Claude account, a five-lane crew, and (optionally) two selected readable Codex accounts. At
// 100x34 the roster + MILESTONES + SPEND + ACCOUNTS fit the height-clamped rail when expanded, so
// without a Codex block the rail stays expanded — which isolates the Codex block as the sole reason
// the WITH-Codex run must auto-fold.
func codexFoldRun(t *testing.T, withCodex bool) tuiModel {
	t.Helper()
	now := time.Now()
	run := apitypes.RunDTO{ID: "cx-fold", Kind: "issue", Status: "running", Health: "ok",
		IssueTitle: "many lanes with codex",
		Milestones: []apitypes.Milestone{{ID: "m1", Title: "Alpha"}, {ID: "m2", Title: "Beta"},
			{ID: "m3", Title: "Gamma"}, {ID: "m4", Title: "Delta"}},
		MilestonesCompleted: []string{"m1", "m2"}, MilestonesInProgress: []string{"m3"}}
	sid, lbl := "sec-meta", "meta"
	run.AnthropicSecretID, run.AnthropicSecretLabel = &sid, &lbl
	run.Usage = spendUsage()
	roles := []string{"lead", "coder", "tester", "reviewer", "auditor"}
	msgs := make([]apitypes.MessageDTO, 0, len(roles))
	for i, role := range roles {
		msgs = append(msgs, msgDTO(int32(i+1), "text", role, "toolu_"+itoa(i), "lbl"+itoa(i), "x", now))
	}
	m := tuiTestModel(t, &uzicli.FakeClient{}, run.ID)
	next, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 34})
	m = next.(tuiModel)
	next, _ = m.Update(rateLimitsMsg{tokens: []apitypes.TokenRateLimitDTO{okMeter(sid, lbl, true, 33, 61)}})
	m = next.(tuiModel)
	settings := apitypes.UserSettingsDTO{SidebarTokenIds: []string{sid}}
	if withCodex {
		next, _ = m.Update(codexRateLimitsMsg{accounts: []apitypes.CodexAccountRateLimitDTO{
			codexAcct("cx-primary", "primary", true, "fresh", cwin(41), cwin(63)),
			codexAcct("cx-team", "team", false, "fresh", cwin(88), cwin(52)),
		}})
		m = next.(tuiModel)
		settings.SidebarCodexAccountIds = []string{"cx-team"}
	}
	next, _ = m.Update(settingsMsg{settings: settings})
	m = next.(tuiModel)
	return applyDetail(m, run, msgs)
}

// TestRailCodexBlockAutoFoldsIntoView is the FIX-2 acceptance test (PRD #1209 M3): at a normal
// 100x34 running+metered run with a selected readable Codex account, the crew rail must auto-fold
// so the CODEX block renders (header + an account line), instead of staying expanded and letting
// the block silently overflow to nothing. The control run (no Codex) stays expanded at the same
// geometry, proving the Codex block's floor is what tips the auto-fold — the exact railAutoFolded
// gap this fix closes (the CODEX block was appended last and never counted there).
func TestRailCodexBlockAutoFoldsIntoView(t *testing.T) {
	// Control: without a Codex block, roster + MILESTONES + SPEND + ACCOUNTS fit at 100x34, so the
	// rail stays EXPANDED (open caret ▾).
	if got := railTitleLine(codexFoldRun(t, false)); !strings.Contains(got, "▾") {
		t.Fatalf("without a Codex block the rail should stay expanded at 100x34 (▾), got %q", got)
	}

	// With a selected readable Codex account, the crew auto-folds to make room (closed count caret).
	m := codexFoldRun(t, true)
	if got := railTitleLine(m); !strings.Contains(got, "▸") {
		t.Errorf("with a Codex block the rail must auto-fold at 100x34 (N ▸), got %q", got)
	}
	// The CODEX header and the first shown account's line are in the composed view (not overflowed).
	view := stripANSI(m.View().Content)
	if !strings.Contains(view, "CODEX") {
		t.Errorf("the CODEX header must be present after the crew auto-folds:\n%s", view)
	}
	if !strings.Contains(view, "primary") {
		t.Errorf("a selected Codex account line (its alias) must be present:\n%s", view)
	}
	if !strings.Contains(view, "41%") {
		t.Errorf("the shown Codex account's reading must be present:\n%s", view)
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
		// The sanitized residue of BOTH untrusted fields must still be present, proving each was
		// drawn (and folded), not silently dropped: "hostile" from the alias, and "evil" from the
		// bucket DisplayName. Pinning both keeps the two Plain-folds (codexAccountLabel and
		// codexBucketLabel) honest, not just the alias one.
		plain := stripANSI(surface.out)
		if !strings.Contains(plain, "hostile") {
			t.Errorf("%s: the sanitized alias residue is missing — the alias was not drawn:\n%s", surface.name, plain)
		}
		if !strings.Contains(plain, "evil") {
			t.Errorf("%s: the sanitized bucket display-name residue is missing — DisplayName was not drawn:\n%s", surface.name, plain)
		}
	}
}
