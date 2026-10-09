package main

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// PRD #2602: the board PROG column.

func progRun(id, status string, p *apitypes.RunProgress) apitypes.RunListItemDTO {
	return apitypes.RunListItemDTO{RunDTO: apitypes.RunDTO{ID: id, Kind: "issue", Status: status,
		IssueTitle: "title " + id, Milestones: []apitypes.Milestone{{ID: "m1"}, {ID: "m2"}, {ID: "m3"}},
		MilestonesCompleted: []string{"m1", "m2"}, Progress: p}}
}

func progBoard(t *testing.T, width int, runs ...apitypes.RunListItemDTO) tuiModel {
	t.Helper()
	m := tuiTestModel(t, &uzicli.FakeClient{Runs: runs}, "")
	m.width, m.height = width, 40
	next, _ := m.Update(boardRunsMsg{reqID: m.board.waitID, runs: runs})
	return next.(tuiModel)
}

func progLine(m tuiModel, id string) string {
	return stripANSI(lineContaining(viewLines(m), "title "+id))
}

func TestTUIBoardProgStates(t *testing.T) {
	pct70, pct11 := 70, 11
	cases := []struct {
		name   string
		status string
		p      *apitypes.RunProgress
		want   string
	}{
		{"percent", "running", &apitypes.RunProgress{State: "percent", Pct: &pct70}, "70% ▰▰▰▰▰▰▱▱"},
		{"low percent", "running", &apitypes.RunProgress{State: "percent", Pct: &pct11}, "11% ▰▱▱▱▱▱▱▱"},
		{"stalled", "running", &apitypes.RunProgress{State: "stalled"}, "stalled"},
		{"waiting", "awaiting_input", &apitypes.RunProgress{State: "waiting"}, "waits on you"},
		{"parked limit", "limit_wait", &apitypes.RunProgress{State: "parked"}, "⏸ limit"},
		{"parked pool", "pool_wait", &apitypes.RunProgress{State: "parked"}, "⏸ pool"},
		{"parked recov", "recovery_wait", &apitypes.RunProgress{State: "parked"}, "⏸ recov"},
		{"parked paused", "paused", &apitypes.RunProgress{State: "parked"}, "⏸ paused"},
		{"queued", "queued", &apitypes.RunProgress{State: "queued"}, "queued"},
		{"planning", "planning", &apitypes.RunProgress{State: "planning"}, "planning"},
	}
	for _, prof := range []colorprofile.Profile{colorprofile.TrueColor, colorprofile.Ascii, colorprofile.NoTTY} {
		for _, c := range cases {
			m := progBoard(t, 140, progRun("aaaaaaaa", c.status, c.p))
			next, _ := m.Update(tea.ColorProfileMsg{Profile: prof})
			m = next.(tuiModel)
			if line := progLine(m, "aaaaaaaa"); !strings.Contains(line, c.want) {
				t.Errorf("%s (profile %v): PROG %q missing in %q", c.name, prof, c.want, line)
			}
		}
	}
}

// none, nil progress and an unknown state draw nothing in the cell.
func TestTUIBoardProgBlank(t *testing.T) {
	for name, p := range map[string]*apitypes.RunProgress{
		"nil": nil, "none": {State: "none"}, "unknown": {State: "future-state"},
	} {
		line := progLine(progBoard(t, 140, progRun("bbbbbbbb", "running", p)), "bbbbbbbb")
		for _, bad := range []string{"%", "stalled", "queued", "planning", "on you", "⏸"} {
			if strings.Contains(line, bad) {
				t.Errorf("%s: unexpected %q in %q", name, bad, line)
			}
		}
	}
}

// The PROG bar drops before the percent, and the column leaves with MILES; no row overflows
// and the title is never the first thing cut.
func TestTUIBoardProgWidthShedding(t *testing.T) {
	pct := 70
	run := progRun("cccccccc", "running", &apitypes.RunProgress{State: "percent", Pct: &pct})
	sawBarless, sawNone := false, false
	for w := 60; w <= 200; w++ {
		m := progBoard(t, w, run)
		if m.boardShowProgBar() && !m.boardShowMile() {
			t.Fatalf("width %d: PROG bar shown without MILES", w)
		}
		line := progLine(m, "cccccccc")
		if line == "" {
			t.Fatalf("width %d: title cut from the row", w)
		}
		if vw := visualWidth(line); vw > w {
			t.Errorf("width %d: row is %d cols wide: %q", w, vw, line)
		}
		hasPct, hasBar := strings.Contains(line, "70%"), strings.Contains(line, "▰▰▰▰▰▰▱▱")
		if hasPct != m.boardShowMile() || hasBar != m.boardShowProgBar() {
			t.Errorf("width %d: pct=%v bar=%v, want %v/%v: %q", w, hasPct, hasBar, m.boardShowMile(), m.boardShowProgBar(), line)
		}
		sawBarless = sawBarless || (hasPct && !hasBar)
		sawNone = sawNone || !hasPct
	}
	if !sawBarless || !sawNone {
		t.Errorf("expected a bar-less band (%v) and a width without PROG (%v)", sawBarless, sawNone)
	}
}

// Narrow flags use the short forms: the first width that shows MILES has no bar yet.
func TestTUIBoardProgNarrowFlags(t *testing.T) {
	run := progRun("dddddddd", "awaiting_input", &apitypes.RunProgress{State: "waiting"})
	for w := 60; w <= 200; w++ {
		m := progBoard(t, w, run)
		if !m.boardShowMile() {
			continue
		}
		if m.boardShowProgBar() {
			t.Fatalf("width %d: first MILES width already shows the PROG bar", w)
		}
		line := progLine(m, "dddddddd")
		if !strings.Contains(line, "on you") || strings.Contains(line, "waits on you") {
			t.Errorf("narrow waiting flag should read 'on you': %q", line)
		}
		return
	}
	t.Fatal("no width showed MILES")
}
