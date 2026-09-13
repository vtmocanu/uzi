package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// shortDuration boundaries — the header's compact elapsed format. The unit edges (59s→60s,
// 59m→60m, exact hour, 24h, exact day) and the negative clamp are exactly where an off-by-one
// or a bogus second unit would hide.
func TestShortDuration(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{-5 * time.Second, "0s"},
		{0, "0s"},
		{45 * time.Second, "45s"},
		{59 * time.Second, "59s"},
		{60 * time.Second, "1m"},
		{90 * time.Second, "1m"},
		{59 * time.Minute, "59m"},
		{60 * time.Minute, "1h"},
		{2 * time.Hour, "2h"}, // exact hour: no trailing minutes
		{time.Hour + 4*time.Minute, "1h4m"},
		{3*time.Hour + 4*time.Minute, "3h4m"},
		{24 * time.Hour, "1d"}, // exact day: no trailing hours
		{2*24*time.Hour + 5*time.Hour, "2d5h"},
		{3 * 24 * time.Hour, "3d"},
	}
	for _, c := range cases {
		if got := shortDuration(c.d); got != c.want {
			t.Errorf("shortDuration(%s) = %q, want %q", c.d, got, c.want)
		}
	}
}

// runDuration is WORK time, not queue-wait time: a queued run (no ClaimedAt/StartedAt) shows
// nothing rather than dressing its time-since-created up as elapsed run time. StartedAt wins
// over ClaimedAt; a terminal run measures to FinishedAt, not to now.
func TestRunDuration(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	tp := func(d time.Duration) *time.Time { x := now.Add(d); return &x }

	// Queued: CreatedAt is set (it always is) but there is no start stamp → "".
	if got := runDuration(apitypes.RunDTO{CreatedAt: now.Add(-30 * time.Second)}, now); got != "" {
		t.Errorf("queued run should show no duration, got %q", got)
	}
	// Claimed but not started → since ClaimedAt.
	if got := runDuration(apitypes.RunDTO{CreatedAt: now.Add(-time.Hour), ClaimedAt: tp(-3 * time.Minute)}, now); got != "3m" {
		t.Errorf("claimed run: got %q, want 3m", got)
	}
	// Running → since StartedAt, which wins over ClaimedAt.
	if got := runDuration(apitypes.RunDTO{ClaimedAt: tp(-10 * time.Minute), StartedAt: tp(-5 * time.Minute)}, now); got != "5m" {
		t.Errorf("running run: got %q, want 5m", got)
	}
	// Terminal → FinishedAt - StartedAt, independent of now.
	if got := runDuration(apitypes.RunDTO{StartedAt: tp(-2 * time.Hour), FinishedAt: tp(-time.Hour)}, now); got != "1h" {
		t.Errorf("finished run: got %q, want 1h", got)
	}
}

// applyMeta refreshes the non-streamed fields (milestones, health, …) but must PRESERVE the
// stream-owned Status, so a stale GetRun response cannot revert a status the live socket just
// advanced. It is also a no-op before the initial load has set the baseline.
func TestApplyMetaPreservesStatus(t *testing.T) {
	d := &detailState{runLoaded: true, run: apitypes.RunDTO{ID: "r1", Status: "awaiting_approval", Health: "ok"}}
	d.applyMeta(apitypes.RunDTO{ID: "r1", Status: "running", Health: "stalled",
		Milestones:          []apitypes.Milestone{{ID: "m1", Title: "A"}},
		MilestonesCompleted: []string{"m1"}})
	if d.run.Status != "awaiting_approval" {
		t.Errorf("applyMeta clobbered stream-owned Status: got %q, want awaiting_approval", d.run.Status)
	}
	if d.run.Health != "stalled" || len(d.run.Milestones) != 1 {
		t.Errorf("applyMeta did not refresh non-streamed fields: health=%q milestones=%d", d.run.Health, len(d.run.Milestones))
	}

	notLoaded := &detailState{runLoaded: false, run: apitypes.RunDTO{Status: "queued"}}
	notLoaded.applyMeta(apitypes.RunDTO{Status: "running", Health: "stalled"})
	if notLoaded.run.Status != "queued" || notLoaded.run.Health != "" {
		t.Errorf("applyMeta ran before the initial load: %+v", notLoaded.run)
	}
}

// The transport indicator: the healthy/transient states fold into the header tag, a degradation
// takes its own line, exactly one of the two is non-empty for any non-terminal state, and a
// terminal run carries NO transport chrome at all (its stream closing is expected, not a fault).
func TestTransportChrome(t *testing.T) {
	mk := func(status string, mut func(d *detailState)) tuiModel {
		m := tuiTestModel(t, &uzicli.FakeClient{}, "r1")
		m.detail.run = apitypes.RunDTO{ID: "r1", Status: status}
		m.detail.runLoaded = true
		m.detail.tailLoaded = true
		if mut != nil {
			mut(&m.detail)
		}
		return m
	}

	live := mk("running", func(d *detailState) { d.stream = &uzicli.RunStream{} })
	if !strings.Contains(live.transportHeaderTag(), "live") {
		t.Errorf("live: header tag missing: %q", live.transportHeaderTag())
	}
	if live.transportLine() != "" {
		t.Errorf("live: should not draw a transport line: %q", live.transportLine())
	}

	connecting := mk("running", nil)
	if !strings.Contains(connecting.transportHeaderTag(), "connecting") {
		t.Errorf("connecting: header tag missing: %q", connecting.transportHeaderTag())
	}

	degraded := mk("running", func(d *detailState) { d.polling = true; d.streamErr = errors.New("boom") })
	if degraded.transportHeaderTag() != "" {
		t.Errorf("degraded: should fold to the line, not the header: %q", degraded.transportHeaderTag())
	}
	if !strings.Contains(degraded.transportLine(), "unavailable") {
		t.Errorf("degraded: transport line missing: %q", degraded.transportLine())
	}

	for _, tc := range []struct {
		name string
		mut  func(d *detailState)
	}{
		{"live", func(d *detailState) { d.stream = &uzicli.RunStream{} }},
		{"connecting", nil},
		{"polling", func(d *detailState) { d.polling = true }},
	} {
		m := mk("running", tc.mut)
		tag, line := m.transportHeaderTag(), m.transportLine()
		if (tag != "") == (line != "") {
			t.Errorf("%s: exactly one of tag/line must be non-empty (tag=%q line=%q)", tc.name, tag, line)
		}
	}

	for _, s := range []string{"completed", "failed", "cancelled"} {
		withStream := mk(s, func(d *detailState) { d.stream = &uzicli.RunStream{} })
		if withStream.transportHeaderTag() != "" || withStream.transportLine() != "" {
			t.Errorf("terminal %s showed transport chrome (tag=%q line=%q)", s,
				withStream.transportHeaderTag(), withStream.transportLine())
		}
		polling := mk(s, func(d *detailState) { d.polling = true; d.streamErr = errors.New("x") })
		if polling.transportHeaderTag() != "" || polling.transportLine() != "" {
			t.Errorf("terminal %s (polling) showed transport chrome", s)
		}
	}
}

// The header is a single physical row (issue #666), clamped to the terminal width so the duration,
// status word and folded "● live" tag on a long title cannot wrap it into a second physical row —
// which would make transcriptViewport under-count and push the footer off the bottom (the #379
// invariant). The status WORD, the elapsed duration and the transport tag must NEVER be the field
// that truncates — the title truncates with … instead. B1 regression: padVisual never truncates, so
// a long title could fill the row and let the final clampVisual cut the status off the end; the
// title cap + the status-preserving clamp prevent that. #666 CodeRabbit regression: at a narrow
// width the final clamp used to cut the title down to a bare "…" (its 10-col floor defeated by the
// concatenated left-block clamp); the breadcrumb now yields instead, so a title PREFIX survives with
// the status intact. Exercised at 60/70/80/90/100 — 60 is the width that reproduced the bare-"…".
func TestDetailHeaderFitsWidthWithDuration(t *testing.T) {
	now := time.Now()
	runID := "abcdabcd-1111"
	started := now.Add(-2*time.Hour - 13*time.Minute)
	run := apitypes.RunDTO{ID: runID, Kind: "issue", Status: "running", StartedAt: &started,
		IssueTitle: strings.Repeat("Refactor the forge sync loop for the GitHub driver ", 3)}

	render := func(w int) []string {
		m := tuiTestModel(t, &uzicli.FakeClient{}, runID)
		m.width, m.height = w, 34
		m = applyDetail(m, run,
			[]apitypes.MessageDTO{msgDTO(1, "text", "lead", "", "", "planning", now)})
		m.detail.stream = &uzicli.RunStream{} // live socket → the "● live" tag is folded into the header too
		return strings.Split(m.View().Content, "\n")
	}

	for _, w := range []int{60, 70, 80, 90, 100} {
		rows := render(w)
		if len(rows) != 34 {
			t.Fatalf("width %d: detail rendered %d rows, want the terminal height 34\n%s", w, len(rows), strings.Join(rows, "\n"))
		}
		// The single header row must never exceed the width (a wrap would clip the footer).
		if vw := visualWidth(rows[0]); vw > w {
			t.Errorf("width %d: header row is %d cols (must clamp, else it wraps and clips the footer): %q", w, vw, rows[0])
		}
		// The status WORD, the "· <dur>" segment and the "● live" tag all render in FULL on the one
		// header row, at every width — the title is what truncates, never the right block.
		if !strings.Contains(rows[0], "running") {
			t.Errorf("width %d: header row missing the status word 'running' (it must never be the field that truncates): %q", w, rows[0])
		}
		if !strings.Contains(rows[0], "· 2h13m") {
			t.Errorf("width %d: header row missing the elapsed duration '· 2h13m': %q", w, rows[0])
		}
		if !strings.Contains(rows[0], "live") {
			t.Errorf("width %d: header row missing the ● live transport tag: %q", w, rows[0])
		}
		// A title PREFIX must survive at every width — at 60 the breadcrumb yields to guarantee it,
		// rather than the title collapsing to a bare "…" (the #666 CodeRabbit regression). "Refac" is
		// the leading text of the fixture title; before the fix, width 60 rendered no title text at all.
		if !strings.Contains(rows[0], "Refac") {
			t.Errorf("width %d: header row missing the title prefix 'Refac' (the title must not collapse to a bare …): %q", w, rows[0])
		}
	}
}

// The crew rail auto-folds (PRD #1257): a tall roster that would push the MILESTONES block below
// the height-clamped, non-scrolling rail folds by itself, with no key press, to a count caret +
// the selected lane so the block stays in view. `c` is a sticky override that pins the OPPOSITE of
// what the user currently sees — first press pins Open (roster back, MILESTONES pushed off again),
// next press pins Closed (folded again). Toggling with no lanes is a no-op.
func TestDetailCollapsibleCrewRevealsMilestones(t *testing.T) {
	now := time.Now()
	runID := "beefbeef-1111"
	run := apitypes.RunDTO{ID: runID, Status: "running", IssueTitle: "many lanes",
		Milestones: []apitypes.Milestone{{ID: "m1", Title: "Alpha"}, {ID: "m2", Title: "Beta"},
			{ID: "m3", Title: "Gamma"}, {ID: "m4", Title: "Delta"}},
		MilestonesCompleted: []string{"m1", "m2"}, MilestonesInProgress: []string{"m3"}}
	msgs := []apitypes.MessageDTO{
		msgDTO(1, "text", "lead", "", "plan", "p", now),
		msgDTO(2, "text", "coder", "toolu_a", "impl", "a", now),
		msgDTO(3, "text", "tester", "toolu_b", "sweep", "b", now),
		msgDTO(4, "text", "reviewer", "toolu_c", "review", "c", now),
		msgDTO(5, "text", "auditor", "toolu_d", "audit", "d", now),
		msgDTO(6, "text", "researcher", "toolu_e", "dig", "e", now),
		msgDTO(7, "text", "documenter", "toolu_f", "docs", "f", now),
		msgDTO(8, "text", "release", "toolu_g", "ship", "g", now),
	}
	m := tuiTestModel(t, &uzicli.FakeClient{}, runID)
	m.width, m.height = 100, 20
	m = applyDetail(m, run, msgs)

	// Opens FOLDED with no key press: the expanded roster would push MILESTONES below the fold, so
	// the rail auto-folds to the count caret + selected lane, keeping MILESTONES in view.
	auto := m.View().Content
	if !strings.Contains(auto, "MILESTONES") {
		t.Errorf("an auto-folded rail should keep the MILESTONES block in view\n%s", auto)
	}
	// 8 real lanes plus the aggregated "all agents" lane the rail prepends = 9 rows.
	if !strings.Contains(auto, "9 ▸") {
		t.Errorf("an auto-folded rail should show the closed caret with the lane count (9 ▸)\n%s", auto)
	}
	if !strings.Contains(auto, "crew") {
		t.Errorf("footer should carry the 'c crew' hint\n%s", auto)
	}

	// `c` pins OPEN: the roster returns (open caret ▾) and pushes MILESTONES back below the fold.
	pinnedOpen := press(t, m, keyCollapseCrew)
	openView := pinnedOpen.View().Content
	if !strings.Contains(openView, "▾") {
		t.Errorf("first `c` should pin the roster open (▾)\n%s", openView)
	}
	if strings.Contains(openView, "MILESTONES") {
		t.Errorf("a pinned-open tall roster should push MILESTONES below the fold\n%s", openView)
	}

	// `c` again pins CLOSED: folded to the count caret, MILESTONES revealed once more.
	pinnedClosed := press(t, pinnedOpen, keyCollapseCrew)
	closedView := pinnedClosed.View().Content
	if !strings.Contains(closedView, "9 ▸") {
		t.Errorf("second `c` should pin the roster folded (9 ▸)\n%s", closedView)
	}
	if !strings.Contains(closedView, "MILESTONES") {
		t.Errorf("a pinned-closed rail should reveal the MILESTONES block\n%s", closedView)
	}

	// Toggling with no lanes is a no-op (the caret and footer hint are hidden there): the mode
	// stays at the auto zero value.
	m0 := tuiTestModel(t, &uzicli.FakeClient{}, runID)
	m0 = applyDetail(m0, run, nil)
	if press(t, m0, keyCollapseCrew).detail.railFold != railFoldAuto {
		t.Errorf("collapse toggled with no lanes; should be a no-op leaving railFoldAuto")
	}
}

// railTitleLine is the crew rail's first line — the pane title carrying the fold caret (▾ / N ▸)
// and nothing that moves per tick (no relAge, no blink phase). Tests assert on this alone so a
// caret check does not chase legitimately-shifting cells elsewhere in the frame.
func railTitleLine(m tuiModel) string {
	return strings.SplitN(stripANSI(m.renderLaneRail()), "\n", 2)[0]
}

// autoFoldRun is the 8-lane (+ synthetic "all agents" = 9) / 4-milestone run the auto-fold seam
// tests share, optionally carrying Usage (SPEND) and the run's own account (ACCOUNTS).
func autoFoldRun(withUsage, withAccount bool) (apitypes.RunDTO, []apitypes.MessageDTO) {
	now := time.Now()
	run := apitypes.RunDTO{ID: "beefbeef-1111", Status: "running", Health: "ok", IssueTitle: "many lanes",
		Milestones: []apitypes.Milestone{{ID: "m1", Title: "Alpha"}, {ID: "m2", Title: "Beta"},
			{ID: "m3", Title: "Gamma"}, {ID: "m4", Title: "Delta"}},
		MilestonesCompleted: []string{"m1", "m2"}, MilestonesInProgress: []string{"m3"}}
	if withUsage {
		run.Usage = spendUsage()
	}
	if withAccount {
		sid, lbl := "sec-run", "runacct"
		run.AnthropicSecretID, run.AnthropicSecretLabel = &sid, &lbl
	}
	msgs := []apitypes.MessageDTO{
		msgDTO(1, "text", "lead", "", "plan", "p", now),
		msgDTO(2, "text", "coder", "toolu_a", "impl", "a", now),
		msgDTO(3, "text", "tester", "toolu_b", "sweep", "b", now),
		msgDTO(4, "text", "reviewer", "toolu_c", "review", "c", now),
		msgDTO(5, "text", "auditor", "toolu_d", "audit", "d", now),
		msgDTO(6, "text", "researcher", "toolu_e", "dig", "e", now),
		msgDTO(7, "text", "documenter", "toolu_f", "docs", "f", now),
		msgDTO(8, "text", "release", "toolu_g", "ship", "g", now),
	}
	return run, msgs
}

// A tall terminal fits the whole expanded rail, so the same many-lane/milestone run that folds at
// 100x20 opens EXPANDED at 100x60: the open caret, every lane, and MILESTONES + SPEND + ACCOUNTS
// all present (PRD #1257 D1 — the roster stays when it fits).
func TestDetailRailExpandsWhenTall(t *testing.T) {
	run, msgs := autoFoldRun(true, true)
	m := tuiTestModel(t, &uzicli.FakeClient{}, run.ID)
	m.width, m.height = 100, 60
	next, _ := m.Update(rateLimitsMsg{tokens: []apitypes.TokenRateLimitDTO{okMeter(*run.AnthropicSecretID, "runacct", true, 33, 61)}})
	m = next.(tuiModel)
	m = applyDetail(m, run, msgs)

	rail := stripANSI(m.renderLaneRail())
	if !strings.Contains(railTitleLine(m), "▾") {
		t.Errorf("a tall terminal should open the rail expanded (▾)\n%s", rail)
	}
	// Every lane visible: the synthetic "all agents" row through the last real lane ("release").
	for _, want := range []string{"all agents", "release", "MILESTONES", "SPEND", "ACCOUNTS"} {
		if !strings.Contains(rail, want) {
			t.Errorf("expanded rail at 100x60 missing %q\n%s", want, rail)
		}
	}
}

// The auto-fold is a pure function of height with no stored state, so a resize re-decides on the
// next render with no key press (PRD #1257 D1): 60→20 folds, 20→60 unfolds.
func TestDetailRailAutoFoldFollowsResize(t *testing.T) {
	run, msgs := autoFoldRun(false, false)
	m := tuiTestModel(t, &uzicli.FakeClient{}, run.ID)
	m.width, m.height = 100, 60
	m = applyDetail(m, run, msgs)
	if !strings.Contains(railTitleLine(m), "▾") {
		t.Fatalf("precondition: 100x60 should be expanded (▾), got %q", railTitleLine(m))
	}

	n2, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 20})
	m = n2.(tuiModel)
	if !strings.Contains(railTitleLine(m), "9 ▸") {
		t.Errorf("resize to 100x20 should auto-fold with no key press (9 ▸), got %q", railTitleLine(m))
	}

	n3, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 60})
	m = n3.(tuiModel)
	if !strings.Contains(railTitleLine(m), "▾") {
		t.Errorf("resize back to 100x60 should unfold with no key press (▾), got %q", railTitleLine(m))
	}
}

// A `c` pin is sticky: a resize re-decides only while the mode is Auto, never clearing a pin (PRD
// #1257 D3). An Open pin taken at 100x20 (where the rail auto-folds, so `c` flips it Open) keeps
// the roster expanded across a resize to 100x60 and back to 100x20 — the blocks drop at 20 as they
// did before this PRD, which is fine.
func TestDetailRailOpenPinSurvivesResize(t *testing.T) {
	run, msgs := autoFoldRun(false, false)
	m := tuiTestModel(t, &uzicli.FakeClient{}, run.ID)
	m.width, m.height = 100, 20
	m = applyDetail(m, run, msgs)
	if !strings.Contains(railTitleLine(m), "9 ▸") {
		t.Fatalf("precondition: 100x20 should auto-fold (9 ▸), got %q", railTitleLine(m))
	}

	m = press(t, m, keyCollapseCrew) // auto-folded → `c` pins Open
	if m.detail.railFold != railFoldOpen {
		t.Fatalf("`c` on an auto-folded rail should pin it Open, got %v", m.detail.railFold)
	}
	if !strings.Contains(railTitleLine(m), "▾") {
		t.Errorf("a pinned-open rail should show the open caret at 100x20 (▾), got %q", railTitleLine(m))
	}
	// Blocks drop at 100x20 with the roster open (the pre-PRD expanded behavior): the height-clamped
	// View drops MILESTONES below the fold (joinColumns clamps the composed frame, not the rail
	// string, so this asserts on View().Content).
	if strings.Contains(m.View().Content, "MILESTONES") {
		t.Errorf("a pinned-open tall roster at 100x20 should push MILESTONES below the fold")
	}

	n2, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 60})
	m = n2.(tuiModel)
	if !strings.Contains(railTitleLine(m), "▾") {
		t.Errorf("the Open pin should hold at 100x60 (▾), got %q", railTitleLine(m))
	}
	n3, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 20})
	m = n3.(tuiModel)
	if m.detail.railFold != railFoldOpen {
		t.Errorf("a resize must not clear the Open pin, got %v", m.detail.railFold)
	}
	if !strings.Contains(railTitleLine(m), "▾") {
		t.Errorf("the Open pin should survive the resize back to 100x20 (▾, roster stays), got %q", railTitleLine(m))
	}
}

// Reopening a run resets the fold to Auto (PRD #1257 D3): the pin is a per-run view preference, so
// the drill-in path (newDetailState) restores the height-driven decision, and a run pinned Open at
// 100x20 folds by itself again on reopen.
func TestDetailRailReopenReturnsToAuto(t *testing.T) {
	run, msgs := autoFoldRun(false, false)
	m := tuiTestModel(t, &uzicli.FakeClient{}, run.ID)
	m.width, m.height = 100, 20
	m = applyDetail(m, run, msgs)
	m = press(t, m, keyCollapseCrew) // pin Open
	if m.detail.railFold != railFoldOpen || !strings.Contains(railTitleLine(m), "▾") {
		t.Fatalf("precondition: expected an Open pin (▾), got fold=%v title=%q", m.detail.railFold, railTitleLine(m))
	}

	// Reopen the run exactly as the board drill-in does: a fresh per-run view state.
	m.detail = newDetailState(run.ID)
	m = applyDetail(m, run, msgs)
	if m.detail.railFold != railFoldAuto {
		t.Errorf("reopening the run should reset the fold to Auto, got %v", m.detail.railFold)
	}
	if !strings.Contains(railTitleLine(m), "9 ▸") {
		t.Errorf("a reopened run should auto-fold again at 100x20 (9 ▸), got %q", railTitleLine(m))
	}
}

// The run's own account is the fold floor (PRD #1257 D2, #623): a run whose roster + MILESTONES
// would fit stays expanded, but adding the requirement that its OWN account entry also fit tips it
// into an auto-fold at a height where the account would not fit; give it one more row of height and
// the account fits, so the rail stays expanded.
func TestDetailRailAccountsBoundary(t *testing.T) {
	now := time.Now()
	sid, lbl := "sec-run", "runacct"
	base := apitypes.RunDTO{ID: "acct-boundary", Status: "running", Health: "ok", IssueTitle: "boundary",
		Milestones:          []apitypes.Milestone{{ID: "m1", Title: "a"}, {ID: "m2", Title: "b"}},
		MilestonesCompleted: []string{"m1"}}
	withAcct := base
	withAcct.AnthropicSecretID, withAcct.AnthropicSecretLabel = &sid, &lbl
	msgs := []apitypes.MessageDTO{
		msgDTO(1, "text", "lead", "", "", "plan", now),
		msgDTO(2, "text", "coder", "toolu_a", "", "impl", now),
	}
	mk := func(run apitypes.RunDTO, h int) tuiModel {
		m := tuiTestModel(t, &uzicli.FakeClient{}, run.ID)
		m.width, m.height = 100, h
		if run.AnthropicSecretID != nil {
			next, _ := m.Update(rateLimitsMsg{tokens: []apitypes.TokenRateLimitDTO{okMeter(sid, lbl, true, 33, 61)}})
			m = next.(tuiModel)
		}
		return applyDetail(m, run, msgs)
	}

	// At 100x16 the roster + MILESTONES fit, so the run WITHOUT an own account stays expanded —
	// isolating the account requirement as the sole reason the WITH-account run folds.
	if !strings.Contains(railTitleLine(mk(base, 16)), "▾") {
		t.Errorf("without an own account, roster + MILESTONES fit at 100x16 and should stay expanded (▾)")
	}
	// Same geometry, but now the run's own account must also fit — it does not, so the rail folds.
	if !strings.Contains(railTitleLine(mk(withAcct, 16)), "3 ▸") {
		t.Errorf("with an own account that would not fit at 100x16, the rail should auto-fold (3 ▸)")
	}
	// Two more rows and the own account fits, so the rail stays expanded.
	if !strings.Contains(railTitleLine(mk(withAcct, 18)), "▾") {
		t.Errorf("with two more rows of height the own account fits and the rail should stay expanded (▾)")
	}
}

// The empty required set never folds (PRD #1257 D2/D5): a run with NO milestone list, NO usage and
// NO own account has nothing below the roster to protect, so a tall roster in a short terminal
// stays EXPANDED and simply clips at the bottom, exactly as before this PRD.
func TestDetailRailEmptyRequiredSetNeverFolds(t *testing.T) {
	now := time.Now()
	var msgs []apitypes.MessageDTO
	for i := 0; i < 12; i++ {
		msgs = append(msgs, msgDTO(int32(i+1), "text", "agent"+itoa(i), "toolu_"+itoa(i), "", "x", now))
	}
	run := apitypes.RunDTO{ID: "empty-set", Status: "running", IssueTitle: "no blocks"}
	for _, h := range []int{12, 16, 20} {
		m := tuiTestModel(t, &uzicli.FakeClient{}, run.ID)
		m.width, m.height = 100, h
		m = applyDetail(m, run, msgs)
		if !strings.Contains(railTitleLine(m), "▾") {
			t.Errorf("a run with no protected blocks must never auto-fold, even at 100x%d (expected ▾), got %q", h, railTitleLine(m))
		}
	}
}

// The fold caret is stable (PRD #1257 D8): with unchanged inputs the glyph is identical across two
// consecutive renders and across a blink-phase flip — the row count is clock- and phase-independent,
// so nothing about the fold decision oscillates on a tick. Only the caret line is asserted; the
// blink phase legitimately moves the milestone micro-bar cells elsewhere in the frame.
func TestDetailRailCaretStableAcrossRenders(t *testing.T) {
	run, msgs := autoFoldRun(false, false)
	m := tuiTestModel(t, &uzicli.FakeClient{}, run.ID)
	m.width, m.height = 100, 20
	m = applyDetail(m, run, msgs)

	first := railTitleLine(m)
	if first != railTitleLine(m) {
		t.Errorf("caret changed across two consecutive renders with no input change")
	}
	m.blinkOn = !m.blinkOn
	if got := railTitleLine(m); got != first {
		t.Errorf("caret changed across a blink-phase flip: %q -> %q", first, got)
	}
}

// A run with no lanes shows no fold caret and `c` is a no-op (PRD #1257 D3/D4): there is no roster
// to fold, so the caret and the toggle are both suppressed and the mode stays Auto.
func TestDetailRailZeroLanesNoCaret(t *testing.T) {
	run := apitypes.RunDTO{ID: "no-lanes", Status: "running", IssueTitle: "queued",
		Milestones: []apitypes.Milestone{{ID: "m1", Title: "a"}}}
	m := tuiTestModel(t, &uzicli.FakeClient{}, run.ID)
	m.width, m.height = 100, 20
	m = applyDetail(m, run, nil)

	title := railTitleLine(m)
	if strings.Contains(title, "▾") || strings.Contains(title, "▸") {
		t.Errorf("a no-lanes rail should carry no fold caret, got %q", title)
	}
	if press(t, m, keyCollapseCrew).detail.railFold != railFoldAuto {
		t.Errorf("`c` with no lanes should be a no-op leaving railFoldAuto")
	}
}

// M2 / D6 property: exactly one blank row separates any two consecutive rendered rail blocks —
// never the double blank the roster→SPEND join produced before the appendRailBlock helper. Asserted
// across the four presence combinations (MILESTONES present/absent × SPEND present/absent, ACCOUNTS
// present) in the expanded, folded and no-lanes render paths. A block's own rows are never blank
// (roster rows, milestone rows, spend/account lines all carry glyphs or padding), so "no two
// consecutive blank rows anywhere in the rail" is exactly the no-double-blank invariant.
func TestDetailRailBlocksSingleBlankSeparator(t *testing.T) {
	now := time.Now()
	sid, lbl := "sec-run", "runacct"
	msgs := []apitypes.MessageDTO{
		msgDTO(1, "text", "lead", "", "", "plan", now),
		msgDTO(2, "text", "coder", "toolu_a", "", "impl", now),
	}
	noDoubleBlank := func(t *testing.T, label, rail string) {
		t.Helper()
		lines := strings.Split(stripANSI(rail), "\n")
		for i := 1; i < len(lines); i++ {
			if strings.TrimSpace(lines[i]) == "" && strings.TrimSpace(lines[i-1]) == "" {
				t.Errorf("%s: double blank row at lines %d-%d (only one blank should separate blocks):\n%s", label, i-1, i, rail)
				return
			}
		}
	}

	for _, milestones := range []bool{false, true} {
		for _, usage := range []bool{false, true} {
			run := apitypes.RunDTO{ID: "sep", Status: "running", Health: "ok", IssueTitle: "sep",
				AnthropicSecretID: &sid, AnthropicSecretLabel: &lbl} // ACCOUNTS always present
			if milestones {
				run.Milestones = []apitypes.Milestone{{ID: "m1", Title: "a"}, {ID: "m2", Title: "b"}}
				run.MilestonesCompleted = []string{"m1"}
			}
			if usage {
				run.Usage = spendUsage()
			}
			label := "M=" + boolStr(milestones) + " S=" + boolStr(usage)

			// Expanded and folded: a lane roster present, at a tall height so every block renders.
			build := func(frames []apitypes.MessageDTO, h int) tuiModel {
				m := tuiTestModel(t, &uzicli.FakeClient{}, run.ID)
				m.width, m.height = 100, h
				next, _ := m.Update(rateLimitsMsg{tokens: []apitypes.TokenRateLimitDTO{okMeter(sid, lbl, true, 33, 61)}})
				m = next.(tuiModel)
				return applyDetail(m, run, frames)
			}
			mExp := build(msgs, 50)
			mExp.detail.railFold = railFoldOpen
			noDoubleBlank(t, label+" expanded", mExp.renderLaneRail())

			mFold := build(msgs, 50)
			mFold.detail.railFold = railFoldClosed
			noDoubleBlank(t, label+" folded", mFold.renderLaneRail())

			// No-lanes path: no frames, so the rail draws "(no activity yet)" + the blocks.
			mNo := build(nil, 50)
			noDoubleBlank(t, label+" no-lanes", mNo.renderLaneRail())
		}
	}
}

// The MILE column's width guard is calibrated for the non-admin prefix; the admin board's extra
// OWNER column AND its always-on credential column (PRD #295) make its rows wider, so the column
// must drop on a narrow admin board rather than overflow the edge (it clipped at widths 90-91
// before the admin-aware threshold), and only returns once the terminal is wide enough for both.
func TestTUIAdminBoardRowsFitNarrowWidth(t *testing.T) {
	fake := &uzicli.FakeClient{Runs: []apitypes.RunListItemDTO{
		{RunDTO: apitypes.RunDTO{ID: "aaaaaaaa-1", Kind: "issue", Status: "running",
			IssueTitle:          "Migrate per-user secrets into the vault hierarchy",
			Milestones:          []apitypes.Milestone{{ID: "m1"}, {ID: "m2"}, {ID: "m3"}, {ID: "m4"}},
			MilestonesCompleted: []string{"m1", "m2"}}},
	}}
	render := func(w int) string {
		m := tuiTestModel(t, fake, "")
		m.board.admin = true
		m.width, m.height = w, 34
		next, _ := m.Update(boardRunsMsg{reqID: m.board.waitID, runs: fake.Runs, admin: true})
		return next.(tuiModel).View().Content
	}
	for _, w := range []int{90, 91} {
		out := render(w)
		for _, r := range strings.Split(out, "\n") {
			if vw := visualWidth(r); vw > w {
				t.Errorf("admin board row is %d cols at width %d (overflows the edge): %q", vw, w, r)
			}
		}
		if strings.Contains(out, "▰") {
			t.Errorf("milestone micro-bar should be hidden on the narrow admin board at width %d\n%s", w, out)
		}
	}
	if wide := stripANSI(render(130)); !strings.Contains(wide, "▰▰▱▱") {
		t.Errorf("milestone micro-bar should return on a wide admin board\n%s", wide)
	}
}
