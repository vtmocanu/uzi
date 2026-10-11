package main

import (
	"regexp"
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
		if m.boardShowProg() && !m.boardShowMile() {
			t.Fatalf("width %d: PROG shown without MILES", w)
		}
		line := progLine(m, "cccccccc")
		if line == "" {
			t.Fatalf("width %d: title cut from the row", w)
		}
		if vw := visualWidth(line); vw > w {
			t.Errorf("width %d: row is %d cols wide: %q", w, vw, line)
		}
		hasPct, hasBar := strings.Contains(line, "70%"), strings.Contains(line, "▰▰▰▰▰▰▱▱")
		if hasPct != m.boardShowProg() || hasBar != m.boardShowProgBar() {
			t.Errorf("width %d: pct=%v bar=%v, want %v/%v: %q", w, hasPct, hasBar, m.boardShowProg(), m.boardShowProgBar(), line)
		}
		sawBarless = sawBarless || (hasPct && !hasBar)
		sawNone = sawNone || !hasPct
	}
	if !sawBarless || !sawNone {
		t.Errorf("expected a bar-less band (%v) and a width without PROG (%v)", sawBarless, sawNone)
	}
}

// The PROG column and its bar never flap: widening the terminal only ever turns them on, for
// the own board with and without the credential column and the admin board.
func TestTUIBoardProgMonotonicInWidth(t *testing.T) {
	pct := 70
	run := progRun("gggggggg", "running", &apitypes.RunProgress{State: "percent", Pct: &pct})
	for _, cfg := range []struct {
		name        string
		admin, cred bool
	}{{"own", false, false}, {"own+cred", false, true}, {"admin", true, false}} {
		prevProg, prevBar := false, false
		for w := 80; w <= 220; w++ {
			m := progBoard(t, w, run)
			m.board.admin = cfg.admin
			if cfg.cred {
				m.tokenCount = 2
			}
			prog, bar := m.boardShowProg(), m.boardShowProgBar()
			if prevProg && !prog {
				t.Errorf("%s width %d: PROG went on -> off as the terminal widened", cfg.name, w)
			}
			if prevBar && !bar {
				t.Errorf("%s width %d: PROG bar went on -> off as the terminal widened", cfg.name, w)
			}
			prevProg, prevBar = prog, bar
		}
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

// Regression (#379 shed order): PROG rides with MILES and must not raise the MILES threshold.
// At 100 cols with COST shown (and no credential column) MILES is on and the row carries both
// the milestone marker and the PROG percent.
func TestTUIBoardProgDoesNotDisplaceMiles(t *testing.T) {
	pct := 70
	run := progRun("eeeeeeee", "running", &apitypes.RunProgress{State: "percent", Pct: &pct})
	m := progBoard(t, 100, run)
	if !m.boardShowCost() || m.boardShowCred() {
		t.Fatalf("setup: want COST and no credential column at 100 cols")
	}
	if !m.boardShowMile() {
		t.Fatalf("MILES must be shown at 100 cols with COST (shed order, #379)")
	}
	line := progLine(m, "eeeeeeee")
	if !strings.Contains(line, "▰▰▱") || !strings.Contains(line, "70%") {
		t.Errorf("row lacks MILES marker or 70%%: %q", line)
	}
}

// At every width the title keeps a sane floor wherever MILES (and so PROG) is shown, for the
// own board with/without the credential column and the admin board.
func TestTUIBoardProgTitleFloor(t *testing.T) {
	pct := 70
	run := progRun("ffffffff", "running", &apitypes.RunProgress{State: "percent", Pct: &pct})
	run.IssueTitle = "abcdefghijklmnopqrstuvwxyz0123456789abcdefghijklmnopqrstuvwxyz"
	minTitle := map[string]int{}
	defer func() { t.Logf("min title cols while MILES shown: %v", minTitle) }()
	for _, cfg := range []struct {
		name        string
		admin, cred bool
	}{{"own", false, false}, {"own+cred", false, true}, {"admin", true, false}} {
		for w := 60; w <= 220; w++ {
			m := progBoard(t, w, run)
			m.board.admin = cfg.admin
			if cfg.cred {
				m.tokenCount = 2
			}
			rows := []apitypes.RunListItemDTO{run}
			next, _ := m.Update(boardRunsMsg{reqID: m.board.waitID, runs: rows})
			m = next.(tuiModel)
			line := stripANSI(lineContaining(viewLines(m), "abcdefghij"))
			if line == "" {
				if m.boardShowMile() {
					t.Errorf("%s width %d: title cut with MILES shown", cfg.name, w)
				}
				continue
			}
			if vw := visualWidth(line); vw > w {
				t.Errorf("%s width %d: row %d cols wide", cfg.name, w, vw)
			}
			if m.boardShowMile() {
				i := strings.Index(line, "abcdefghij")
				tw := 0
				for _, r := range line[i:] {
					if r < '0' || r > 'z' {
						break
					}
					tw++
				}
				if tw < 20 {
					t.Errorf("%s width %d: only %d title cols left with MILES+PROG shown", cfg.name, w, tw)
				}
				if min, ok := minTitle[cfg.name]; !ok || tw < min {
					minTitle[cfg.name] = tw
				}
			}
		}
	}
}

// The stalled flag keeps the app-wide stall ink and "waits on you" the amber one; the two stay
// distinct SGR sequences (colour profile TrueColor so the foreground survives).
func TestTUIBoardProgFlagInks(t *testing.T) {
	sel := progRun("11111111", "running", nil)
	stalled := progRun("22222222", "running", &apitypes.RunProgress{State: "stalled"})
	waiting := progRun("33333333", "awaiting_input", &apitypes.RunProgress{State: "waiting"})
	m := progBoard(t, 140, sel, stalled, waiting)
	next, _ := m.Update(tea.ColorProfileMsg{Profile: colorprofile.TrueColor})
	m = next.(tuiModel)
	if m.pal.stall == nil || m.pal.amber == nil || paintSeg(m.pal.stall, nil, false, "x") == paintSeg(m.pal.amber, nil, false, "x") {
		t.Fatal("setup: stall and amber inks must differ")
	}
	rawStalled := lineContaining(strings.Split(m.View().Content, "\n"), "title 22222222")
	rawWaiting := lineContaining(strings.Split(m.View().Content, "\n"), "title 33333333")
	// The cell's SGR opens with the palette foreground triplet (a background may follow it).
	inInk := func(raw string, c interface{ RGBA() (r, g, b, a uint32) }, word string) bool {
		return regexp.MustCompile(regexp.QuoteMeta(toneCode(t, c)) + `[0-9;]*m` + regexp.QuoteMeta(word)).MatchString(raw)
	}
	if !inInk(rawStalled, m.pal.stall, "stalled") || inInk(rawStalled, m.pal.amber, "stalled") {
		t.Errorf("stalled cell must use the stall ink, not amber: %q", rawStalled)
	}
	if !inInk(rawWaiting, m.pal.amber, "waits on you") || inInk(rawWaiting, m.pal.stall, "waits on you") {
		t.Errorf("waiting cell must use amber, not the stall ink: %q", rawWaiting)
	}
}
