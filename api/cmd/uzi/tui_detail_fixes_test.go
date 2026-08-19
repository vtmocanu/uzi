package main

import (
	"errors"
	"strings"
	"testing"
	"time"

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
	d := &detailState{loaded: true, run: apitypes.RunDTO{ID: "r1", Status: "awaiting_approval", Health: "ok"}}
	d.applyMeta(apitypes.RunDTO{ID: "r1", Status: "running", Health: "stalled",
		Milestones:          []apitypes.Milestone{{ID: "m1", Title: "A"}},
		MilestonesCompleted: []string{"m1"}})
	if d.run.Status != "awaiting_approval" {
		t.Errorf("applyMeta clobbered stream-owned Status: got %q, want awaiting_approval", d.run.Status)
	}
	if d.run.Health != "stalled" || len(d.run.Milestones) != 1 {
		t.Errorf("applyMeta did not refresh non-streamed fields: health=%q milestones=%d", d.run.Health, len(d.run.Milestones))
	}

	notLoaded := &detailState{loaded: false, run: apitypes.RunDTO{Status: "queued"}}
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
		m.detail.loaded = true
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

// The header is clamped to the terminal width, so the duration and folded "● live" tag on a long
// title cannot wrap the header into a second physical row — which would make transcriptViewport
// under-count and push the footer off the bottom (the #379 invariant). The status WORD and the
// elapsed duration ride line 2's RIGHT edge and must NEVER be the field that truncates — the title
// truncates with … instead. B1 regression: before the title was capped to leave room for `right`,
// padVisual (which never truncates) let a long title fill the left and the final clampVisual cut
// `right` off the end, so at width 80 the status word vanished entirely. Exercised at 70/80/90/100.
func TestDetailHeaderFitsWidthWithDuration(t *testing.T) {
	now := time.Now()
	runID := "abcdabcd-1111"
	started := now.Add(-2*time.Hour - 13*time.Minute)
	run := apitypes.RunDTO{ID: runID, Kind: "issue", Status: "running", StartedAt: &started,
		IssueTitle: strings.Repeat("Refactor the forge sync loop for the GitHub driver ", 3)}

	render := func(w int) []string {
		m := tuiTestModel(t, &uzicli.FakeClient{}, runID)
		m.width, m.height = w, 34
		next, _ := m.Update(detailLoadedMsg{run: run,
			msgs: []apitypes.MessageDTO{msgDTO(1, "text", "lead", "", "", "planning", now)}})
		m = next.(tuiModel)
		m.detail.stream = &uzicli.RunStream{} // live socket → the "● live" tag is folded into the header too
		return strings.Split(m.View().Content, "\n")
	}

	for _, w := range []int{70, 80, 90, 100} {
		rows := render(w)
		if len(rows) != 34 {
			t.Fatalf("width %d: detail rendered %d rows, want the terminal height 34\n%s", w, len(rows), strings.Join(rows, "\n"))
		}
		// The two-line header: neither physical row may exceed the width (a wrap would clip the footer).
		if vw := visualWidth(rows[0]); vw > w {
			t.Errorf("width %d: header line 1 is %d cols (must clamp, else it wraps and clips the footer): %q", w, vw, rows[0])
		}
		if vw := visualWidth(rows[1]); vw > w {
			t.Errorf("width %d: header line 2 is %d cols (must clamp): %q", w, vw, rows[1])
		}
		// The status WORD and the "· <dur>" segment both render in FULL on line 2, at every width —
		// the title is what truncates, never `right`.
		if !strings.Contains(rows[1], "running") {
			t.Errorf("width %d: header line 2 missing the status word 'running' (it must never be the field that truncates): %q", w, rows[1])
		}
		if !strings.Contains(rows[1], "· 2h13m") {
			t.Errorf("width %d: header line 2 missing the elapsed duration '· 2h13m': %q", w, rows[1])
		}
		// "● live" keeps its own reserved slot on line 1, so it never clips either.
		if !strings.Contains(rows[0], "live") {
			t.Errorf("width %d: header line 1 missing the ● live transport tag: %q", w, rows[0])
		}
	}
}

// The collapsible crew rail (`c`) exists so a tall roster cannot hide the milestone block: the
// rail is height-clamped and does not scroll. Expanded, a many-lane run pushes MILESTONES below
// the fold; collapsed shows a count caret + only the selected lane, revealing the block. Toggling
// with no lanes is a no-op.
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
	next, _ := m.Update(detailLoadedMsg{run: run, msgs: msgs})
	m = next.(tuiModel)

	expanded := m.View().Content
	if strings.Contains(expanded, "MILESTONES") {
		t.Fatalf("precondition: a tall crew rail should push MILESTONES below the fold when expanded\n%s", expanded)
	}
	if !strings.Contains(expanded, "▾") {
		t.Errorf("expanded rail should show the open caret ▾\n%s", expanded)
	}

	collapsed := press(t, m, keyCollapseCrew).View().Content
	if !strings.Contains(collapsed, "MILESTONES") {
		t.Errorf("collapsing the crew should reveal the MILESTONES block\n%s", collapsed)
	}
	if !strings.Contains(collapsed, "8 ▸") {
		t.Errorf("collapsed rail should show the closed caret with the lane count (8 ▸)\n%s", collapsed)
	}
	if !strings.Contains(collapsed, "crew") {
		t.Errorf("footer should carry the 'c crew' hint\n%s", collapsed)
	}

	// Toggling with no lanes is a no-op (the caret and footer hint are hidden there).
	m0 := tuiTestModel(t, &uzicli.FakeClient{}, runID)
	n0, _ := m0.Update(detailLoadedMsg{run: run})
	if press(t, n0.(tuiModel), keyCollapseCrew).detail.railCollapsed {
		t.Errorf("collapse toggled with no lanes; should be a no-op")
	}
}

// The MILE column's width guard is calibrated for the non-admin prefix; the admin board's extra
// OWNER column makes its rows wider, so the column must drop on a narrow admin board rather than
// overflow the edge (it clipped at widths 90-91 before the admin-aware threshold).
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
		next, _ := m.Update(boardRunsMsg{runs: fake.Runs, admin: true})
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
	if wide := stripANSI(render(120)); !strings.Contains(wide, "▰▰▱▱") {
		t.Errorf("milestone micro-bar should return on a wide admin board\n%s", wide)
	}
}
