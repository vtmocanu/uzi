package main

// PRD #1209 M3 / #1519 M3-M4 — the Codex per-account rate-limit meters shown beside the Claude
// meters on the board strip (boardCodexAccountsSeg, placed by boardMeterLayout) and the detail
// rail (railCodexRateMeters). The
// selection mirrors the Claude #519 seam via selectedCodexRateMeters: readable = a status
// carrying a reading (fresh|stale), nothing readable → nothing; shown = readable filtered by
// (IsDefault || AccountID ∈ sidebar_codex_account_ids), deduped to one meter per account. A
// stale reading is shown DIMMED; a nil/no-reading window renders "—", never 0. Aliases and
// bucket display names ride renderer.Plain (D7). PRD #1653 (D-T2..D-T5): every drawn account
// is named, windows are labelled by their reported length ("5h"/"7d"/"3h", "?" unknown), the
// main "codex" bucket draws no name, and every Codex-bearing board line carries one tag.

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

// Window lengths in seconds, as Codex reports them in limit_window_seconds.
const (
	secs5h = int64(5 * 3600)
	secs3h = int64(3 * 3600)
	secs7d = int64(7 * 86400)
)

// withLen returns a copy of w carrying the given limit_window_seconds, unless w is nil or
// already carries a length.
func withLen(w *apitypes.CodexRateLimitWindowDTO, secs int64) *apitypes.CodexRateLimitWindowDTO {
	if w == nil || w.LimitWindowSeconds != nil {
		return w
	}
	c := *w
	c.LimitWindowSeconds = &secs
	return &c
}

// codexAcct builds one Codex account in the shape codexauth produces: a single main bucket
// (id codexMainBucketID, empty display name) carrying the given primary and secondary windows
// (either may be nil). A window with no reported length gets the usual 5h primary / 7d
// secondary, so the labels read like a real reading.
func codexAcct(accountID, alias string, isDefault bool, status string, primary, secondary *apitypes.CodexRateLimitWindowDTO) apitypes.CodexAccountRateLimitDTO {
	return apitypes.CodexAccountRateLimitDTO{
		AccountID: accountID, Aliases: []string{alias}, IsDefault: isDefault, Status: status,
		Buckets: []apitypes.CodexRateLimitBucketDTO{
			{ID: codexMainBucketID, Primary: withLen(primary, secs5h), Secondary: withLen(secondary, secs7d)},
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
	strip := stripANSI(codexStripModel(t, accts, nil).boardCodexAccountsSeg(time.Now()))
	if !strings.Contains(strip, "33%") {
		t.Fatalf("readable default account's primary pct 33%% missing:\n%s", strip)
	}
	if strings.Contains(strip, "deadacct") {
		t.Errorf("a no_reading account leaked its alias into the Codex strip:\n%s", strip)
	}
}

// TestBoardCodexStripSelection — default AND a listed non-default show; an unlisted
// non-default does not. Every drawn account is named (PRD #1653 D-T2), so the aliases render.
// The accounts section carries NO provider tag (that is the layout's job, PRD 1519 M3) —
// asserted here by the absence of the retired "Codex " group prefix; the single "codex"
// provider tag is proved by the boardMeterLayout tests below.
func TestBoardCodexStripSelection(t *testing.T) {
	accts := []apitypes.CodexAccountRateLimitDTO{
		codexAcct("cx-primary", "primary", true, "fresh", cwin(33), cwin(61)),
		codexAcct("cx-team", "teamacct", false, "fresh", cwin(80), cwin(45)),
		codexAcct("cx-unlisted", "unlistedacct", false, "fresh", cwin(11), cwin(22)),
	}
	strip := stripANSI(codexStripModel(t, accts, []string{"cx-team"}).boardCodexAccountsSeg(time.Now()))
	if strings.Contains(strip, "Codex") {
		t.Errorf("the retired hardcoded \"Codex \" group prefix must be gone from the accounts section:\n%s", strip)
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
	shown := m.selectedCodexRateMeters()
	if len(shown) != 1 {
		t.Fatalf("two rows with the same AccountID must dedup to one meter, got %d: %+v", len(shown), shown)
	}
	// Exactly one per-account meter renders: the accent bar ▎ prefixes each account segment, so
	// there must be exactly one in the strip.
	seg := stripANSI(m.boardCodexAccountsSeg(time.Now()))
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
	seg := m.boardCodexAccountsSeg(time.Now())
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
	seg := m.boardCodexAccountsSeg(time.Now())
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
// renders "5h —" (its length label, PRD #1653 D-T3), never "5h 0%": partial/unknown is
// preserved.
func TestBoardCodexStripNilWindow(t *testing.T) {
	m := codexStripModel(t, []apitypes.CodexAccountRateLimitDTO{
		codexAcct("cx-primary", "primary", true, "fresh",
			&apitypes.CodexRateLimitWindowDTO{UsedPercent: nil}, nil),
	}, nil)
	strip := stripANSI(m.boardCodexAccountsSeg(time.Now()))
	if !strings.Contains(strip, "5h —") {
		t.Errorf("a no-reading window must render `5h —`:\n%s", strip)
	}
	if strings.Contains(strip, "0%") {
		t.Errorf("a no-reading window must NOT render 0%%:\n%s", strip)
	}
}

// TestBoardCodexStripEmptyCases — no Codex segment when nothing is readable or shown.
func TestBoardCodexStripEmptyCases(t *testing.T) {
	// (a) no accounts.
	if s := codexStripModel(t, nil, nil).boardCodexAccountsSeg(time.Now()); s != "" {
		t.Errorf("no accounts must yield no Codex segment, got %q", s)
	}
	// (b) all unreadable.
	allDown := codexStripModel(t, []apitypes.CodexAccountRateLimitDTO{
		{AccountID: "a", Aliases: []string{"a"}, Status: "no_reading"},
		{AccountID: "b", Aliases: []string{"b"}, IsDefault: true, Status: "pending"},
	}, nil)
	if s := allDown.boardCodexAccountsSeg(time.Now()); s != "" {
		t.Errorf("all-unreadable accounts must yield no Codex segment, got %q", s)
	}
	// (c) readable but nothing shown (a single non-default account not in the sidebar).
	hidden := codexStripModel(t, []apitypes.CodexAccountRateLimitDTO{
		codexAcct("cx-x", "hiddenacct", false, "fresh", cwin(40), nil),
	}, nil)
	if s := hidden.boardCodexAccountsSeg(time.Now()); s != "" {
		t.Errorf("a readable-but-not-shown account must yield no Codex segment, got %q", s)
	}
}

// bothProvidersModel seeds a board model with the given Claude tokens (all shown via sidebar) and
// Codex accounts (all shown via sidebar) at the given width, for the adaptive layout tests. The
// sidebar lists every non-default id so the selection shows them all regardless of default flag.
func bothProvidersModel(t *testing.T, width int, claude []apitypes.TokenRateLimitDTO, codex []apitypes.CodexAccountRateLimitDTO) tuiModel {
	t.Helper()
	m := tuiTestModel(t, &uzicli.FakeClient{}, "")
	m.width, m.height = width, 40
	next, _ := m.Update(rateLimitsMsg{tokens: claude})
	m = next.(tuiModel)
	next, _ = m.Update(codexRateLimitsMsg{accounts: codex})
	m = next.(tuiModel)
	var tokIDs, acctIDs []string
	for _, tk := range claude {
		tokIDs = append(tokIDs, tk.SecretID)
	}
	for _, a := range codex {
		acctIDs = append(acctIDs, a.AccountID)
	}
	next, _ = m.Update(settingsMsg{settings: apitypes.UserSettingsDTO{
		SidebarTokenIds:        tokIDs,
		SidebarCodexAccountIds: acctIDs,
	}})
	return next.(tuiModel)
}

// TestBoardMeterLayoutCombinedWide (PRD 1519 M5a) — at a WIDE width with one Claude token AND one
// Codex account, boardMeterLayout returns ONE combined line carrying BOTH providers' meters, with
// exactly ONE provider-level "codex" tag (the D3 combined-line invariant). The single account is
// named "primary" and its main bucket draws no name (PRD #1653), so the only "codex" is the tag.
func TestBoardMeterLayoutCombinedWide(t *testing.T) {
	m := bothProvidersModel(t, 200,
		[]apitypes.TokenRateLimitDTO{okMeter("sec-personal", "personal", true, 35, 62)},
		[]apitypes.CodexAccountRateLimitDTO{codexAcct("cx-primary", "primary", true, "fresh", cwin(71), cwin(29))})
	layout := m.boardMeterLayout(time.Now())
	if len(layout.lines) != 1 {
		t.Fatalf("wide Claude+Codex must combine onto ONE line, got %d lines: %q", len(layout.lines), layout.lines)
	}
	plain := stripANSI(layout.lines[0])
	// Both providers' readings ride the one line.
	for _, want := range []string{"35%", "71%"} {
		if !strings.Contains(plain, want) {
			t.Errorf("the combined line is missing a reading %q:\n%s", want, plain)
		}
	}
	// Exactly one provider-level "codex" token (the retired "Codex " prefix would be a second).
	if n := strings.Count(plain, "codex"); n != 1 {
		t.Errorf("the combined line must carry exactly ONE provider tag \"codex\", got %d:\n%s", n, plain)
	}
	if strings.Contains(plain, "Codex") {
		t.Errorf("the retired capitalised \"Codex \" group prefix must never return:\n%s", plain)
	}
	// Anti-clip invariant: the combined branch is taken ONLY when it fits, so a len==1 line must
	// never exceed m.width — this locks the load-bearing anti-clip guarantee so a future change to
	// the section gap or the provider-tag width cannot silently reintroduce a clipped combined line.
	if w := visualWidth(layout.lines[0]); w > m.width {
		t.Errorf("a combined (len==1) line must fit m.width=%d, got visual width %d:\n%s", m.width, w, plain)
	}

	// Through the full renderBoard seam: both readings ride the SAME physical line, which carries
	// exactly one provider tag.
	var combinedLine string
	for _, ln := range strings.Split(stripANSI(m.renderBoard()), "\n") {
		if strings.Contains(ln, "35%") {
			combinedLine = ln
			break
		}
	}
	if combinedLine == "" {
		t.Fatalf("the Claude reading did not appear in the composed board render")
	}
	if !strings.Contains(combinedLine, "71%") {
		t.Errorf("the Codex reading must share the SAME physical line as the Claude reading:\n%q", combinedLine)
	}
	if n := strings.Count(combinedLine, "codex"); n != 1 {
		t.Errorf("the composed combined line must carry exactly ONE provider tag \"codex\", got %d:\n%q", n, combinedLine)
	}
}

// TestBoardMeterLayoutCodexOnlyIncludesProviderTag (PRD 1519 D3, Codex-only row) — when the viewer
// has a readable Codex account but no readable/selected Anthropic token, boardMeterLayout renders the
// Codex meters on ONE line that MUST carry the single provider tag "codex": there is no Claude line
// above to disambiguate the provider, and under Ascii/NoTTY the accent-bar tint is stripped (the ▎
// glyph is identical for both providers), so the tag is the provider signal. This pins the
// `case !hasClaude:` branch, which the both-providers layout tests never exercise. PRD #1653: the
// lone account is named, its windows read "5h"/"7d", and its main bucket draws no "codex" name.
func TestBoardMeterLayoutCodexOnlyIncludesProviderTag(t *testing.T) {
	m := codexStripModel(t, []apitypes.CodexAccountRateLimitDTO{
		codexAcct("cx-primary", "primary", true, "fresh", cwin(71), cwin(29)),
	}, nil)
	layout := m.boardMeterLayout(time.Now())
	if len(layout.lines) != 1 {
		t.Fatalf("a Codex-only viewer must render ONE meter line, got %d: %q", len(layout.lines), layout.lines)
	}
	plain := stripANSI(layout.lines[0])
	// The alias is not "codex" and the main bucket draws no name, so the only "codex" token must be
	// the provider tag — dropping it would leave the section with no provider mark at all.
	if !strings.HasPrefix(plain, " codex ▎primary 5h ") {
		t.Errorf("the Codex-only line must read ` codex ▎primary 5h …` (tag, named account, length label):\n%s", plain)
	}
	if n := strings.Count(plain, "codex"); n != 1 {
		t.Errorf("the Codex-only line must carry exactly ONE provider tag \"codex\", got %d:\n%s", n, plain)
	}
	if strings.Contains(plain, "Codex") {
		t.Errorf("the retired capitalised \"Codex \" group prefix must never return:\n%s", plain)
	}
	if !strings.Contains(plain, "71%") {
		t.Errorf("the Codex-only line is missing the reading %q:\n%s", "71%", plain)
	}
	if w := visualWidth(layout.lines[0]); w > m.width {
		t.Errorf("the Codex-only line must fit m.width=%d, got visual width %d:\n%s", m.width, w, plain)
	}
}

// TestBoardMeterLayoutNarrowFallback (PRD 1519 M5b) — at a NARROW width where the combined line
// does not fit (two Claude tokens + two Codex accounts ≈ 163 cols at width 100), boardMeterLayout
// falls back to TWO lines: the Claude line, then a separate Codex line. The Codex readings stay
// reachable on their own line, and — PRD #1653 D-T4, superseding PRD 1519's default-omit — the
// fallback Codex line STARTS with the single "codex" tag, since its "5h"/"7d" window labels no
// longer say Codex on their own.
func TestBoardMeterLayoutNarrowFallback(t *testing.T) {
	m := bothProvidersModel(t, 100,
		[]apitypes.TokenRateLimitDTO{
			okMeter("sec-personal", "personal", true, 35, 62),
			okMeter("sec-meta", "meta", false, 88, 44),
		},
		[]apitypes.CodexAccountRateLimitDTO{
			codexAcct("cx-primary", "primary", true, "fresh", cwin(71), cwin(29)),
			codexAcct("cx-team", "team", false, "fresh", cwin(66), cwin(13)),
		})
	layout := m.boardMeterLayout(time.Now())
	if len(layout.lines) != 2 {
		t.Fatalf("a combined line that does not fit must fall back to TWO lines, got %d: %q", len(layout.lines), layout.lines)
	}
	claudeLine := stripANSI(layout.lines[0])
	codexLine := stripANSI(layout.lines[1])
	if !strings.Contains(claudeLine, "35%") {
		t.Errorf("the fallback Claude line must carry the Claude reading:\n%s", claudeLine)
	}
	if strings.Contains(claudeLine, "codex") || strings.Contains(claudeLine, "Codex") {
		t.Errorf("the Claude line must not carry any provider tag:\n%s", claudeLine)
	}
	if !strings.Contains(codexLine, "71%") {
		t.Errorf("the fallback Codex line must carry the Codex reading (reachable, not clipped):\n%s", codexLine)
	}
	// The fallback Codex line leads with the tag, exactly once (the aliases here are not "codex").
	if !strings.HasPrefix(codexLine, " codex ▎primary ") {
		t.Errorf("the fallback Codex line must start with the provider tag then the first account:\n%s", codexLine)
	}
	if n := strings.Count(codexLine, "codex"); n != 1 {
		t.Errorf("the fallback Codex line must carry exactly ONE provider tag \"codex\", got %d:\n%s", n, codexLine)
	}
	// The Claude line is labelled too (PRD #1653 D-T2) and keeps its Claude-only look.
	if !strings.HasPrefix(claudeLine, " ▎personal 5h ") {
		t.Errorf("the fallback Claude line must start with the labelled first token:\n%s", claudeLine)
	}
}

// TestBoardCodexReachableAtStandardWidth (PRD #1209 M3 / 1519 M4) — at width 100 with TWO readable
// Claude tokens AND TWO selected readable Codex accounts, a Codex account's percentage digits are
// present in the composed board render, i.e. the Codex data is reachable, not clipped to a bare
// provider label the way a forced single combined line was at ≤120 cols. At this width and config
// the layout falls back to a second Codex line (see TestBoardMeterLayoutNarrowFallback), which is
// exactly what keeps the readings reachable. The Codex percentages (71/29, 66/13) are chosen to NOT
// collide with the Claude readings, so a hit proves the Codex bytes survived, not a substring match.
func TestBoardCodexReachableAtStandardWidth(t *testing.T) {
	m := bothProvidersModel(t, 100,
		[]apitypes.TokenRateLimitDTO{
			okMeter("sec-personal", "personal", true, 35, 62),
			okMeter("sec-meta", "meta", false, 88, 44),
		},
		[]apitypes.CodexAccountRateLimitDTO{
			codexAcct("cx-primary", "primary", true, "fresh", cwin(71), cwin(29)),
			codexAcct("cx-team", "team", false, "fresh", cwin(66), cwin(13)),
		})
	board := stripANSI(m.renderBoard())
	// The load-bearing assertion: a Codex account's utilization percentage is actually present,
	// not clipped away. Both the default account's window (71%) and the listed account's (66%).
	for _, want := range []string{"71%", "66%"} {
		if !strings.Contains(board, want) {
			t.Errorf("Codex reading %q was clipped from the width-100 board (unreachable):\n%s", want, board)
		}
	}
}

// TestBoardMeterLayoutAliasEqualsCodex (PRD 1519 M5c) — with two Codex accounts where ONE account's
// alias is literally "codex", on a combined line, BOTH the provider tag AND that account's own
// label render: the "codex" provider tag PLUS the aliased account's "codex" label, two distinct
// tokens, NOT the old redundant single prefix. The tag is never suppressed to match an alias, and
// the main bucket draws no "codex" name (PRD #1653 D-T3), so "codex" appears exactly twice.
func TestBoardMeterLayoutAliasEqualsCodex(t *testing.T) {
	m := bothProvidersModel(t, 200,
		[]apitypes.TokenRateLimitDTO{okMeter("sec-personal", "personal", true, 35, 62)},
		[]apitypes.CodexAccountRateLimitDTO{
			codexAcct("cx-a", "codex", true, "fresh", cwin(71), cwin(29)), // alias literally "codex"
			codexAcct("cx-team", "team", false, "fresh", cwin(66), cwin(13)),
		})
	layout := m.boardMeterLayout(time.Now())
	if len(layout.lines) != 1 {
		t.Fatalf("this config must combine onto ONE line, got %d: %q", len(layout.lines), layout.lines)
	}
	plain := stripANSI(layout.lines[0])
	// The tag (at the section start) AND the aliased account's own "codex" label both render →
	// "codex" appears exactly twice. Suppressing the tag to match the alias would give 1; dropping
	// the label would give 1; a main-bucket "codex" name would give 3. So the count discriminates.
	if n := strings.Count(plain, "codex"); n != 2 {
		t.Errorf("an account aliased \"codex\" must yield the provider tag plus its own label (\"codex\" ×2), got %d:\n%s", n, plain)
	}
	// The tag, then the aliased account's label straight after its accent bar — proving the label
	// (not just the tag) drew. The label carries no "(default)" badge (PRD #1653 amendment).
	if !strings.Contains(plain, "codex ▎codex 5h ") {
		t.Errorf("the aliased account's label did not render right after the tag:\n%s", plain)
	}
	if strings.Contains(plain, "(default)") {
		t.Errorf("the Codex account label must not carry a \"(default)\" badge:\n%s", plain)
	}
}

// TestBoardMeterLayoutAsciiCombinedMultiAccount (PRD 1519 M5d) — under a colorprofile.Ascii/NoTTY
// downgrade, with TWO Codex accounts on a combined line, the accent tint is stripped and the ▎ glyph
// is identical for both providers, so the single "codex" provider tag is the ONLY provider signal.
// The Codex section still renders the tag once plus a per-account label per account, and stays
// provider-distinguishable with no colour at all.
func TestBoardMeterLayoutAsciiCombinedMultiAccount(t *testing.T) {
	m := bothProvidersModel(t, 200,
		[]apitypes.TokenRateLimitDTO{okMeter("sec-personal", "personal", true, 35, 62)},
		[]apitypes.CodexAccountRateLimitDTO{
			codexAcct("cx-primary", "primaryacct", true, "fresh", cwin(71), cwin(29)),
			codexAcct("cx-team", "teamacct", false, "fresh", cwin(66), cwin(13)),
		})
	next, _ := m.Update(tea.ColorProfileMsg{Profile: colorprofile.Ascii})
	m = next.(tuiModel)
	layout := m.boardMeterLayout(time.Now())
	if len(layout.lines) != 1 {
		t.Fatalf("this config must combine onto ONE line, got %d: %q", len(layout.lines), layout.lines)
	}
	plain := stripANSI(layout.lines[0])
	// The provider tag survives the downgrade as plain text and is the provider distinguisher.
	if n := strings.Count(plain, "codex"); n != 1 {
		t.Errorf("under Ascii the combined line must carry exactly ONE provider tag \"codex\", got %d:\n%s", n, plain)
	}
	// Both per-account labels render, so each account is still identified.
	for _, want := range []string{"primaryacct", "teamacct"} {
		if !strings.Contains(plain, want) {
			t.Errorf("under Ascii the per-account label %q is missing:\n%s", want, plain)
		}
	}
	// Both providers' readings are still legible with no colour.
	for _, want := range []string{"35%", "71%", "66%"} {
		if !strings.Contains(plain, want) {
			t.Errorf("under Ascii a reading %q is missing from the combined line:\n%s", want, plain)
		}
	}
}

// TestBoardMeterCapacityCombinedVsSplit (PRD 1519 M5e) — with the vault hint present, switching the
// SAME meter config from combined (one line, wide) to split (two lines, narrow) changes the reserved
// capacity by EXACTLY one row, and leaves the vault-hint row reservation undisturbed. This is the
// row-math anti-drift guarantee: boardCapacityWith reserves exactly the meter row count the snapshot
// draws, and the separate vault reservation is unchanged either way.
func TestBoardMeterCapacityCombinedVsSplit(t *testing.T) {
	claude := []apitypes.TokenRateLimitDTO{
		okMeter("sec-personal", "personal", true, 35, 62),
		okMeter("sec-meta", "meta", false, 88, 44),
	}
	codex := []apitypes.CodexAccountRateLimitDTO{
		codexAcct("cx-primary", "primary", true, "fresh", cwin(71), cwin(29)),
		codexAcct("cx-team", "team", false, "fresh", cwin(66), cwin(13)),
	}
	m := bothProvidersModel(t, 200, claude, codex)
	// Lock the vault → the tier-1 hint line is reserved (no runs loaded, so it is the hint, not the band).
	next, _ := m.Update(vaultStatusMsg{user: apitypes.UserDTO{Email: "me@x.io"}, locked: true})
	m = next.(tuiModel)
	if m.vaultIndicatorLine() == "" {
		t.Fatalf("precondition: a locked vault must show the tier-1 hint line")
	}

	m.width = 200
	wide := m.boardMeterLayout(time.Now())
	m.width = 100
	narrow := m.boardMeterLayout(time.Now())
	if len(wide.lines) != 1 {
		t.Fatalf("precondition: at width 200 the meters must combine to ONE line, got %d", len(wide.lines))
	}
	if len(narrow.lines) != 2 {
		t.Fatalf("precondition: at width 100 the meters must split to TWO lines, got %d", len(narrow.lines))
	}

	capCombined := m.boardCapacityWith(len(wide.lines))
	capSplit := m.boardCapacityWith(len(narrow.lines))
	if capCombined-capSplit != 1 {
		t.Errorf("combined vs split must change capacity by exactly one row: combined=%d split=%d", capCombined, capSplit)
	}

	// The vault-hint reservation is undisturbed: at a fixed meter row count, a locked vault reserves
	// exactly one row more than an unlocked one.
	unlocked := m
	next, _ = unlocked.Update(vaultStatusMsg{user: apitypes.UserDTO{Email: "me@x.io"}, locked: false})
	unlocked = next.(tuiModel)
	if unlocked.vaultIndicatorLine() != "" {
		t.Fatalf("precondition: an unlocked vault must show no hint line")
	}
	if got, want := m.boardCapacityWith(1), unlocked.boardCapacityWith(1)-1; got != want {
		t.Errorf("the vault-hint reservation drifted: locked capacity %d, want unlocked-1 = %d", got, want)
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
	seg := m.boardCodexAccountsSeg(time.Now())
	for _, want := range []string{"88%", "20%", "▰", "▱"} {
		if !strings.Contains(seg, want) {
			t.Errorf("Ascii-profile Codex strip dropped the always-present cue %q:\n%q", want, seg)
		}
	}
}

// TestSelectedCodexRateMetersSharedByBoardAndRail — the anti-drift guarantee: the board strip
// and the detail rail both consume selectedCodexRateMeters, so they cannot disagree on the
// shown set (and both name every drawn account, PRD #1653 D-T2).
func TestSelectedCodexRateMetersSharedByBoardAndRail(t *testing.T) {
	accts := []apitypes.CodexAccountRateLimitDTO{
		codexAcct("cx-primary", "primary", true, "fresh", cwin(33), cwin(61)),
		codexAcct("cx-team", "teamacct", false, "fresh", cwin(87), cwin(45)),
		codexAcct("cx-unlisted", "unlistedacct", false, "fresh", cwin(11), cwin(22)),
		{AccountID: "cx-dead", Aliases: []string{"deadacct"}, IsDefault: true, Status: "no_reading"},
	}
	sidebar := []string{"cx-team"}

	board := codexStripModel(t, accts, sidebar)
	shown := board.selectedCodexRateMeters()
	gotIDs := make([]string, 0, len(shown))
	for _, a := range shown {
		gotIDs = append(gotIDs, a.AccountID)
	}
	if strings.Join(gotIDs, ",") != "cx-primary,cx-team" {
		t.Errorf("selectedCodexRateMeters shown = %v, want [cx-primary cx-team]", gotIDs)
	}
	// Board strip draws exactly this selection.
	bs := stripANSI(board.boardCodexAccountsSeg(time.Now()))
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

// TestRailCodexRateMetersNilWindow — a present window with no reading renders "5h —" in the
// rail too (its length label, PRD #1653 D-T3), mirroring the board strip.
func TestRailCodexRateMetersNilWindow(t *testing.T) {
	rail := stripANSI(codexRailModel(t, []apitypes.CodexAccountRateLimitDTO{
		codexAcct("cx-primary", "primary", true, "fresh",
			&apitypes.CodexRateLimitWindowDTO{UsedPercent: nil}, nil),
	}, nil).renderLaneRail())
	if !strings.Contains(rail, "5h —") {
		t.Errorf("a no-reading window must render `5h —` in the rail:\n%s", rail)
	}
	if strings.Contains(rail, "0%") {
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
// EITHER the board strip or the detail rail. The alias is always drawn (PRD #1653 D-T2) and the
// hostile bucket is a NON-main bucket, so its name is drawn too (the main "codex" bucket draws
// none) — both untrusted fields actually render (a non-vacuous test).
func TestCodexMetersSanitizeUntrustedText(t *testing.T) {
	hostileAlias := "acct\u202ehostile\x1b[31m\rmeta"
	hostileBucket := "5h\u202e\x07evil\x1b[32m"
	nasty := codexAcct("cx-evil", hostileAlias, true, "fresh", cwin(44), cwin(55))
	nasty.Buckets[0].ID = "gpt-extra" // a non-main bucket: its (hostile) name is drawn
	nasty.Buckets[0].DisplayName = hostileBucket
	accts := []apitypes.CodexAccountRateLimitDTO{
		nasty,
		codexAcct("cx-second", "benignacct", false, "fresh", cwin(12), cwin(20)),
	}
	sidebar := []string{"cx-second"}

	board := codexStripModel(t, accts, sidebar).boardCodexAccountsSeg(time.Now())
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
