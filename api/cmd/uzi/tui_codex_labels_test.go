package main

// PRD #1653 M5/M6 — the TUI half of "provider logos and account names on usage meters":
// every drawn account is named on the board strip (Claude and Codex, a lone one included,
// D-T2), Codex windows are labelled by their reported limit_window_seconds instead of fixed
// primary/secondary letters ("5h"/"7d"/"3h", "?" when unknown — the TUI amendment to D-T3),
// the main "codex" bucket draws no bucket name, the split Codex line keeps the one "codex"
// tag (D-T4), an over-wide line clips at m.width (D-T5), and the detail rail's CODEX block
// and its fold floor (railCodexFloorRows) move together (M6).

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// lwin builds a Codex window with a reading and an explicit limit_window_seconds (nil = unknown).
func lwin(pct int, secs *int64) *apitypes.CodexRateLimitWindowDTO {
	return &apitypes.CodexRateLimitWindowDTO{UsedPercent: fPtr(float64(pct)), LimitWindowSeconds: secs}
}

// viewLines returns the ANSI-stripped lines of the model's full View() frame.
func viewLines(m tuiModel) []string {
	return strings.Split(stripANSI(m.View().Content), "\n")
}

// lineContaining returns the first line holding sub, or "".
func lineContaining(lines []string, sub string) string {
	for _, ln := range lines {
		if strings.Contains(ln, sub) {
			return ln
		}
	}
	return ""
}

// TestCodexWindowLabel pins the web formatCodexWindowLabel rules plus the TUI "?" fallback.
func TestCodexWindowLabel(t *testing.T) {
	cases := []struct {
		secs *int64
		want string
	}{
		{nil, "?"},
		{i64(0), "?"},
		{i64(-60), "?"},
		{i64(5 * 3600), "5h"},
		{i64(3 * 3600), "3h"},
		{i64(7 * 86400), "7d"},
		{i64(30 * 86400), "30d"},
		{i64(86400), "1d"}, // whole days win over whole hours
		{i64(90 * 60), "90m"},
		{i64(45), "45s"},
		{i64(3601), "3601s"},
	}
	for _, c := range cases {
		if got := codexWindowLabel(c.secs); got != c.want {
			t.Errorf("codexWindowLabel(%v) = %q, want %q", c.secs, got, c.want)
		}
	}
}

// TestBoardSingleCodexAccountNamedWithLengthLabels — at the View() seam a lone Codex account
// reads `codex ▎<alias> 7d …`: named, its window labelled by length, no "codex" bucket name for
// the main bucket, and no P/S letters.
func TestBoardSingleCodexAccountNamedWithLengthLabels(t *testing.T) {
	m := codexStripModel(t, []apitypes.CodexAccountRateLimitDTO{{
		AccountID: "cx-solo", Aliases: []string{"solo"}, IsDefault: true, Status: "fresh",
		Buckets: []apitypes.CodexRateLimitBucketDTO{
			{ID: codexMainBucketID, Primary: lwin(42, i64(secs7d))},
		},
	}}, nil)
	line := lineContaining(viewLines(m), "42%")
	if line == "" {
		t.Fatalf("the Codex meter line is missing from the board view")
	}
	if !strings.Contains(line, "codex ▎solo 7d ") {
		t.Errorf("a lone Codex account must read `codex ▎solo 7d …`:\n%q", line)
	}
	if n := strings.Count(line, "codex"); n != 1 {
		t.Errorf("only the provider tag may say \"codex\" (no main-bucket name), got %d:\n%q", n, line)
	}
	for _, bad := range []string{" P ", " S ", "P —", "S —"} {
		if strings.Contains(line, bad) {
			t.Errorf("retired window letter %q is back:\n%q", bad, line)
		}
	}
}

// TestBoardCodexExtraBucketKeepsName — a non-main bucket keeps its (Plain-routed) name before
// its windows while the main bucket stays unnamed, and a 3-hour window reads "3h".
func TestBoardCodexExtraBucketKeepsName(t *testing.T) {
	m := codexStripModel(t, []apitypes.CodexAccountRateLimitDTO{{
		AccountID: "cx-a", Aliases: []string{"alpha"}, IsDefault: true, Status: "fresh",
		Buckets: []apitypes.CodexRateLimitBucketDTO{
			{ID: codexMainBucketID, Primary: lwin(11, i64(secs5h)), Secondary: lwin(22, i64(secs7d))},
			{ID: "gpt-5-codex-mini", DisplayName: "mini", Primary: lwin(33, i64(secs3h))},
		},
	}}, nil)
	line := lineContaining(viewLines(m), "11%")
	if !strings.Contains(line, "▎alpha 5h ") {
		t.Errorf("the main bucket must draw no name — its windows follow the account label:\n%q", line)
	}
	if !strings.Contains(line, "   mini 3h ") {
		t.Errorf("an extra bucket must keep its name before its windows, and a 3-hour window reads 3h:\n%q", line)
	}
	if n := strings.Count(line, "codex"); n != 1 {
		t.Errorf("only the provider tag may say \"codex\", got %d:\n%q", n, line)
	}
}

// TestBoardCodexUnknownWindowLength — a window with no or a non-positive limit_window_seconds
// reads "?" (the TUI amendment to D-T3), with its reading intact.
func TestBoardCodexUnknownWindowLength(t *testing.T) {
	m := codexStripModel(t, []apitypes.CodexAccountRateLimitDTO{{
		AccountID: "cx-u", Aliases: []string{"unk"}, IsDefault: true, Status: "fresh",
		Buckets: []apitypes.CodexRateLimitBucketDTO{
			{ID: codexMainBucketID, Primary: lwin(17, nil), Secondary: lwin(58, i64(0))},
		},
	}}, nil)
	line := lineContaining(viewLines(m), "17%")
	if !strings.Contains(line, "▎unk ? ") {
		t.Errorf("an unknown window length must read `?`:\n%q", line)
	}
	if strings.Count(line, " ? ") != 2 {
		t.Errorf("both the nil and the zero length must read `?`:\n%q", line)
	}
	if !strings.Contains(line, "58%") {
		t.Errorf("the reading must survive an unknown length:\n%q", line)
	}
}

// TestBoardSplitLinesAtViewSeam — at a width where the combined line does not fit, the View()
// draws the Claude line (labelled, no tag) and then the Codex line, which STARTS with the one
// "codex" tag (D-T4); at a wide width the single combined line carries exactly one tag.
func TestBoardSplitLinesAtViewSeam(t *testing.T) {
	claude := []apitypes.TokenRateLimitDTO{okMeter("sec-personal", "personal", true, 35, 62)}
	codex := []apitypes.CodexAccountRateLimitDTO{
		codexAcct("cx-primary", "primary", true, "fresh", cwin(71), cwin(29)),
		codexAcct("cx-team", "team", false, "fresh", cwin(66), cwin(13)),
	}
	narrow := bothProvidersModel(t, 90, claude, codex)
	if n := len(narrow.boardMeterLayout(time.Now()).lines); n != 2 {
		t.Fatalf("precondition: width 90 must split the meters onto two lines, got %d", n)
	}
	lines := viewLines(narrow)
	claudeLine := lineContaining(lines, "35%")
	codexLine := lineContaining(lines, "71%")
	if claudeLine == "" || codexLine == "" || claudeLine == codexLine {
		t.Fatalf("expected distinct Claude and Codex lines, got %q / %q", claudeLine, codexLine)
	}
	if !strings.HasPrefix(claudeLine, " ▎personal 5h ") || strings.Contains(claudeLine, "codex") {
		t.Errorf("the split Claude line must be labelled and untagged:\n%q", claudeLine)
	}
	if !strings.HasPrefix(codexLine, " codex ▎primary 5h ") {
		t.Errorf("the split Codex line must start with the codex tag then the first account:\n%q", codexLine)
	}

	wide := bothProvidersModel(t, 200, claude, codex)
	combined := lineContaining(viewLines(wide), "35%")
	if !strings.Contains(combined, "71%") {
		t.Fatalf("precondition: width 200 must combine both providers on one line:\n%q", combined)
	}
	if n := strings.Count(combined, "codex"); n != 1 {
		t.Errorf("the combined line must carry exactly one codex tag, got %d:\n%q", n, combined)
	}
}

// TestBoardCodexLineClipsAt80 (D-T5) — at 80 columns, two Codex accounts plus an extra bucket
// overflow the line; it is clipped to exactly m.width visual columns (no wrap, no third line)
// and still starts with `codex ▎<first alias>` after the layout's leading space.
func TestBoardCodexLineClipsAt80(t *testing.T) {
	first := codexAcct("cx-alpha", "alpha", true, "fresh", cwin(71), cwin(29))
	first.Buckets = append(first.Buckets, apitypes.CodexRateLimitBucketDTO{
		ID: "gpt-5-codex-mini", DisplayName: "mini", Primary: lwin(12, i64(secs5h)), Secondary: lwin(34, i64(secs7d)),
	})
	m := codexStripModel(t, []apitypes.CodexAccountRateLimitDTO{
		first,
		codexAcct("cx-beta", "beta", false, "fresh", cwin(66), cwin(13)),
	}, []string{"cx-beta"})
	m.width = 80
	if raw := " " + m.boardCodexProviderTag() + m.boardCodexAccountsSeg(time.Now()); visualWidth(raw) <= m.width {
		t.Fatalf("precondition: the unclipped Codex line must overflow 80 cols, got %d", visualWidth(raw))
	}
	layout := m.boardMeterLayout(time.Now())
	if len(layout.lines) != 1 {
		t.Fatalf("a Codex-only viewer draws one line even when it overflows, got %d", len(layout.lines))
	}
	if w := visualWidth(layout.lines[0]); w != m.width {
		t.Errorf("the clipped Codex line must be exactly m.width=%d columns, got %d", m.width, w)
	}
	plain := stripANSI(layout.lines[0])
	if !strings.HasPrefix(plain, " codex ▎alpha ") {
		t.Errorf("the clipped line must start with ` codex ▎alpha`:\n%q", plain)
	}
	if line := lineContaining(viewLines(m), "codex ▎alpha"); visualWidth(line) != m.width {
		t.Errorf("the View() line must be clipped to %d columns, got %d:\n%q", m.width, visualWidth(line), line)
	}
}

// TestBoardProvidersDistinctUnderDowngrade — under Ascii and NoTTY the accent tint is gone and
// the ▎ glyph is shared, so the "codex" tag text must carry the provider distinction on both
// the combined and the split layouts; the Claude line never carries it.
func TestBoardProvidersDistinctUnderDowngrade(t *testing.T) {
	claude := []apitypes.TokenRateLimitDTO{okMeter("sec-personal", "personal", true, 35, 62)}
	codex := []apitypes.CodexAccountRateLimitDTO{
		codexAcct("cx-primary", "primary", true, "fresh", cwin(71), cwin(29)),
		codexAcct("cx-team", "team", false, "fresh", cwin(66), cwin(13)),
	}
	for _, prof := range []colorprofile.Profile{colorprofile.Ascii, colorprofile.NoTTY} {
		for _, width := range []int{200, 90} {
			m := bothProvidersModel(t, width, claude, codex)
			next, _ := m.Update(tea.ColorProfileMsg{Profile: prof})
			m = next.(tuiModel)
			lines := viewLines(m)
			claudeLine := lineContaining(lines, "35%")
			codexLine := lineContaining(lines, "71%")
			if claudeLine == "" || codexLine == "" {
				t.Fatalf("profile %v width %d: meter lines missing", prof, width)
			}
			if n := strings.Count(codexLine, "codex"); n != 1 {
				t.Errorf("profile %v width %d: the Codex-bearing line must keep one codex tag, got %d:\n%q", prof, width, n, codexLine)
			}
			if !strings.Contains(codexLine, "codex ▎primary") {
				t.Errorf("profile %v width %d: the tag must sit right before the first Codex account:\n%q", prof, width, codexLine)
			}
			if claudeLine != codexLine && strings.Contains(claudeLine, "codex") {
				t.Errorf("profile %v width %d: the split Claude line must stay untagged:\n%q", prof, width, claudeLine)
			}
		}
	}
}

// codexRailBlock returns the CODEX block of the rendered rail: the "CODEX" header line through
// the line before the next blank separator (or the end).
func codexRailBlock(rail string) []string {
	lines := strings.Split(stripANSI(rail), "\n")
	for i, ln := range lines {
		if ln != "CODEX" {
			continue
		}
		end := len(lines)
		for j := i + 1; j < len(lines); j++ {
			if strings.TrimSpace(lines[j]) == "" {
				end = j
				break
			}
		}
		return lines[i:end]
	}
	return nil
}

// TestRailCodexNamedLengthLabelled (M6) — the rail's CODEX block names a lone account, labels
// its windows by length, draws no name for the main bucket, keeps an extra bucket's name, and
// never shows P/S.
func TestRailCodexNamedLengthLabelled(t *testing.T) {
	m := codexRailModel(t, []apitypes.CodexAccountRateLimitDTO{{
		AccountID: "cx-a", Aliases: []string{"alpha"}, IsDefault: true, Status: "fresh",
		Buckets: []apitypes.CodexRateLimitBucketDTO{
			{ID: codexMainBucketID, Primary: lwin(11, i64(secs5h)), Secondary: lwin(22, i64(secs7d))},
			{ID: "gpt-5-codex-mini", DisplayName: "mini", Primary: lwin(33, i64(secs3h))},
		},
	}}, nil)
	block := codexRailBlock(m.renderLaneRail())
	want := []string{"CODEX", "alpha", "5h ", "7d ", "mini", "3h "}
	if len(block) != len(want) {
		t.Fatalf("CODEX block has %d rows, want %d:\n%s", len(block), len(want), strings.Join(block, "\n"))
	}
	for i, w := range want {
		if !strings.HasPrefix(block[i], w) {
			t.Errorf("CODEX row %d = %q, want prefix %q", i, block[i], w)
		}
	}
	if rows, ok := m.railCodexFloorRows(); !ok || rows != len(block) {
		t.Errorf("railCodexFloorRows = (%d, %v), want (%d, true) — the floor must equal the drawn entry", rows, ok, len(block))
	}
}

// TestRailCodexUnknownLengthWithResetFitsRail (M6) — an unknown-length window with a reset time,
// a 30-day window with the widest real countdown, and an odd 5-column length ("3601s") each keep
// their percent and countdown intact within laneRailWidth.
func TestRailCodexUnknownLengthWithResetFitsRail(t *testing.T) {
	reset := int64(29*86400 + 23*3600) // shortDuration → "29d23h", the widest sub-100-day countdown
	win := func(secs *int64) *apitypes.CodexRateLimitWindowDTO {
		w := lwin(100, secs)
		w.ResetAfterSeconds = &reset
		return w
	}
	m := codexRailModel(t, []apitypes.CodexAccountRateLimitDTO{{
		AccountID: "cx-a", Aliases: []string{"an-account-alias-longer-than-the-rail"}, IsDefault: true, Status: "fresh",
		Buckets: []apitypes.CodexRateLimitBucketDTO{
			{ID: codexMainBucketID, Primary: win(nil), Secondary: win(i64(30 * 86400))},
			{ID: "odd", DisplayName: "odd", Primary: win(i64(3601))},
		},
	}}, nil)
	block := codexRailBlock(m.renderLaneRail())
	if len(block) != 6 {
		t.Fatalf("CODEX block has %d rows, want 6:\n%s", len(block), strings.Join(block, "\n"))
	}
	for _, ln := range block {
		if w := visualWidth(ln); w > laneRailWidth {
			t.Errorf("rail row %q is %d cols, over laneRailWidth=%d", ln, w, laneRailWidth)
		}
	}
	for _, i := range []int{2, 3, 5} {
		if !strings.Contains(block[i], " 100% ") || !strings.HasSuffix(block[i], "29d23h") {
			t.Errorf("window row %q lost its percent or countdown", block[i])
		}
	}
	for i, prefix := range map[int]string{2: "? ", 3: "30d ", 5: "3601s "} {
		if !strings.HasPrefix(block[i], prefix) {
			t.Errorf("window row %d = %q, want prefix %q", i, block[i], prefix)
		}
	}
}

// codexFloorRunAt is codexFoldRun's crew/milestones/spend/own-account fixture at a chosen
// height, with ONE selected Codex account (main bucket + an extra bucket, floor = 7 rows).
func codexFloorRunAt(t *testing.T, height int, withCodex bool) tuiModel {
	t.Helper()
	now := time.Now()
	run := apitypes.RunDTO{ID: "cx-floor", Kind: "issue", Status: "running", Health: "ok",
		IssueTitle: "codex floor",
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
	next, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: height})
	m = next.(tuiModel)
	next, _ = m.Update(rateLimitsMsg{tokens: []apitypes.TokenRateLimitDTO{okMeter(sid, lbl, true, 33, 61)}})
	m = next.(tuiModel)
	if withCodex {
		next, _ = m.Update(codexRateLimitsMsg{accounts: []apitypes.CodexAccountRateLimitDTO{{
			AccountID: "cx-a", Aliases: []string{"alpha"}, IsDefault: true, Status: "fresh",
			Buckets: []apitypes.CodexRateLimitBucketDTO{
				{ID: codexMainBucketID, Primary: lwin(41, i64(secs5h)), Secondary: lwin(63, i64(secs7d))},
				{ID: "gpt-5-codex-mini", DisplayName: "mini", Primary: lwin(12, i64(secs5h)), Secondary: lwin(34, i64(secs7d))},
			},
		}}})
		m = next.(tuiModel)
	}
	next, _ = m.Update(settingsMsg{settings: apitypes.UserSettingsDTO{SidebarTokenIds: []string{sid}}})
	m = next.(tuiModel)
	return applyDetail(m, run, msgs)
}

// TestRailCodexFloorDecidesFold (M6) — across a sweep of short viewports: (1) at least one
// height folds ONLY because of the CODEX floor (the same run without Codex stays expanded), and
// there the drawn CODEX block equals the floor exactly (7 rows: header, account eyebrow, main
// 5h/7d, "mini" eyebrow, mini 5h/7d — no eyebrow for the main bucket); (2) whenever the rail
// stays EXPANDED with Codex, the CODEX block is drawn whole — the decision is never optimistic.
func TestRailCodexFloorDecidesFold(t *testing.T) {
	const wantFloor = 7
	decided := false
	for h := 20; h <= 60; h++ {
		m := codexFloorRunAt(t, h, true)
		floor, ok := m.railCodexFloorRows()
		if !ok || floor != wantFloor {
			t.Fatalf("height %d: railCodexFloorRows = (%d, %v), want (%d, true)", h, floor, ok, wantFloor)
		}
		rail := m.renderLaneRail()
		expanded := strings.Contains(strings.SplitN(stripANSI(rail), "\n", 2)[0], "▾")
		block := codexRailBlock(rail)
		if expanded && len(block) != floor {
			t.Errorf("height %d: rail stayed expanded but drew %d CODEX rows, want the floor %d:\n%s",
				h, len(block), floor, strings.Join(block, "\n"))
		}
		controlExpanded := strings.Contains(railTitleLine(codexFloorRunAt(t, h, false)), "▾")
		if !expanded && controlExpanded {
			decided = true
			if len(block) != floor {
				t.Errorf("height %d: the floor folded the rail but the drawn CODEX block has %d rows, want %d:\n%s",
					h, len(block), floor, strings.Join(block, "\n"))
			}
			if view := stripANSI(m.View().Content); !strings.Contains(view, "alpha") || !strings.Contains(view, "34%") {
				t.Errorf("height %d: the folded view must show the whole first Codex account:\n%s", h, view)
			}
		}
	}
	if !decided {
		t.Fatalf("no height in the sweep folded because of the CODEX floor alone; the fixture no longer exercises it")
	}
}
