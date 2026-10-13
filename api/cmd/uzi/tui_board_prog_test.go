package main

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// The board's merged milestone/percentage column.

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
	pct := 70
	cases := []struct {
		name, status, health, state string
		planning                    bool
		want                        string
	}{
		{"percent", "running", "ok", "percent", false, "▰▰▱        70%"},
		{"queued", "queued", "ok", "queued", false, "▰▰▱"},
		{"planning", "running", "ok", "planning", true, "▰▰▱"},
		{"stalled", "running", "stalled", "stalled", false, "▰▰▱"},
		{"looping", "running", "looping", "stalled", false, "▰▰▱"},
		{"waiting", "awaiting_input", "ok", "waiting", false, "▰▰▱"},
		{"parked", "limit_wait", "ok", "parked", false, "▰▰▱"},
		{"recovery stale health", "recovery_wait", "stalled", "parked", false, "▰▰▱"},
		{"none", "running", "ok", "none", false, "▰▰▱"},
		{"unknown", "running", "ok", "future-state", false, "▰▰▱"},
		{"terminal", "completed", "ok", "percent", false, "▰▰▱"},
	}
	for _, prof := range []colorprofile.Profile{colorprofile.TrueColor, colorprofile.Ascii, colorprofile.NoTTY} {
		for _, c := range cases {
			run := progRun("aaaaaaaa", c.status, &apitypes.RunProgress{State: c.state, Pct: &pct})
			run.Health, run.IsPlanning = c.health, c.planning
			m := progBoard(t, 140, run)
			next, _ := m.Update(tea.ColorProfileMsg{Profile: prof})
			m = next.(tuiModel)
			cell := strings.TrimSpace(stripANSI(m.boardProgressCell(run, c.status == "completed", nil, m.pal.runStateToken(run.RunDTO, run.IsRevising).word)))
			if cell != c.want {
				t.Errorf("%s/%v cell=%q want=%q", c.name, prof, cell, c.want)
			}
			line := progLine(m, run.ID)
			_, word := stateGlyphWord(run.Status, run.Health, run.IsPlanning, false, "", "")
			if strings.Count(line, word) != 1 {
				t.Errorf("%s duplicated/missing status %q in %q", c.name, word, line)
			}
		}
	}
}

func TestTUIBoardProgWaitingParkedUnderHealth(t *testing.T) {
	for _, prof := range []colorprofile.Profile{colorprofile.TrueColor, colorprofile.Ascii, colorprofile.NoTTY} {
		for _, health := range []string{"stalled", "looping"} {
			for _, c := range []struct{ status, state, wide, narrow string }{
				{"awaiting_input", "waiting", "waits on you", "on you"},
				{"limit_wait", "parked", "⏸ limit", "⏸ limit"},
				{"pool_wait", "parked", "⏸ pool", "⏸ pool"},
				{"paused", "parked", "⏸ paused", "⏸ paused"},
			} {
				run := progRun("bbbbbbbb", c.status, &apitypes.RunProgress{State: c.state})
				run.Health = health
				for _, width := range []int{80, 140} {
					m := progBoard(t, width, run)
					next, _ := m.Update(tea.ColorProfileMsg{Profile: prof})
					m = next.(tuiModel)
					want := c.narrow
					cellWidth := boardMileWidth
					if m.boardShowProg() {
						want = c.wide
						cellWidth += 1 + boardProgPctWidth
					}
					cell := stripANSI(m.boardProgressCell(run, false, nil, m.pal.runStateToken(run.RunDTO, run.IsRevising).word))
					if strings.TrimSpace(cell) != want || visualWidth(cell) != cellWidth {
						t.Errorf("%s/%s/%d/%v: cell %q width %d, want %q width %d", c.status, health, width, prof, cell, visualWidth(cell), want, cellWidth)
					}
					if m.boardShowMile() && !strings.Contains(progLine(m, run.ID), want) {
						t.Errorf("flag absent from row: %q", progLine(m, run.ID))
					}
				}
			}
		}
	}
}

func TestTUIBoardProgMilestoneFallbacks(t *testing.T) {
	pct := 34
	run := progRun("bbbbbbbb", "running", &apitypes.RunProgress{State: "percent", Pct: &pct})
	for i := 4; i <= 12; i++ {
		run.Milestones = append(run.Milestones, apitypes.Milestone{ID: itoa(i)})
	}
	m := progBoard(t, 140, run)
	if cell := strings.TrimSpace(stripANSI(m.boardProgressCell(run, false, nil, m.pal.runStateToken(run.RunDTO, run.IsRevising).word))); cell != "2/12       34%" {
		t.Errorf("count+pct %q", cell)
	}
	for _, p := range []*apitypes.RunProgress{nil, {State: "percent"}, {State: "none"}, {State: "future-state"}} {
		run.Progress = p
		if cell := strings.TrimSpace(stripANSI(m.boardProgressCell(run, false, nil, m.pal.runStateToken(run.RunDTO, run.IsRevising).word))); cell != "2/12" {
			t.Errorf("marker lost or fabricated pct: %q", cell)
		}
	}
	run.Milestones = nil
	run.Progress = &apitypes.RunProgress{State: "percent", Pct: &pct}
	if cell := strings.TrimSpace(stripANSI(m.boardProgressCell(run, false, nil, m.pal.runStateToken(run.RunDTO, run.IsRevising).word))); cell != "" {
		t.Errorf("non-milestone row fabricated progress: %q", cell)
	}
}

func TestTUIBoardProgWidthShedding(t *testing.T) {
	pct := 70
	run := progRun("cccccccc", "running", &apitypes.RunProgress{State: "percent", Pct: &pct})
	for _, cfg := range []struct {
		name        string
		admin, cred bool
	}{{"own", false, false}, {"own+cred", false, true}, {"admin", true, false}} {
		prevProg, prevMile, sawMarkerOnly, sawPct := false, false, false, false
		for w := 60; w <= 220; w++ {
			m := progBoard(t, w, run)
			m.board.admin = cfg.admin
			if cfg.cred {
				m.tokenCount = 2
			}
			prog, mile := m.boardShowProg(), m.boardShowMile()
			if prevProg && !prog || prevMile && !mile {
				t.Fatalf("%s/%d: visibility flapped", cfg.name, w)
			}
			line := progLine(m, run.ID)
			if (line == "" && mile) || visualWidth(line) > w {
				t.Fatalf("%s/%d: missing title/overflow: %q", cfg.name, w, line)
			}
			if strings.Contains(line, "70%") != prog || strings.Contains(line, "▰▰▱") != mile {
				t.Errorf("%s/%d: visibility disagrees: %q", cfg.name, w, line)
			}
			sawMarkerOnly = sawMarkerOnly || mile && !prog
			sawPct = sawPct || prog
			prevProg, prevMile = prog, mile
		}
		if !sawPct || (cfg.admin && !sawMarkerOnly) {
			t.Errorf("%s missing pct band or admin marker-only band", cfg.name)
		}
	}
	m := progBoard(t, 100, run)
	if !m.boardShowCost() || !m.boardShowMile() {
		t.Fatal("#379: milestones/COST displaced at 100")
	}
	if !strings.Contains(progLine(m, run.ID), "70%") {
		t.Fatal("percentage must remain visible on the 100-column own board")
	}
}

// At every width the title keeps a sane floor wherever milestones are shown, for the
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
					t.Errorf("%s width %d: only %d title cols left with progress shown", cfg.name, w, tw)
				}
				if min, ok := minTitle[cfg.name]; !ok || tw < min {
					minTitle[cfg.name] = tw
				}
			}
		}
	}
}

// The percentage slot stays padded for every row, including missing plans and flags.
func TestTUIBoardProgTitleAlignment(t *testing.T) {
	pct := 9
	percent := progRun("11111111", "running", &apitypes.RunProgress{State: "percent", Pct: &pct})
	noPercent := progRun("22222222", "queued", &apitypes.RunProgress{State: "queued"})
	noMilestones := progRun("33333333", "running", nil)
	noMilestones.Milestones = nil
	flag := progRun("44444444", "awaiting_approval", &apitypes.RunProgress{State: "waiting"})
	flag.Health = "stalled"
	runs := []apitypes.RunListItemDTO{percent, noPercent, noMilestones, flag}
	for _, cfg := range []struct {
		name       string
		width      int
		admin, pct bool
	}{
		{"wide", 140, false, true}, {"narrow admin", 125, true, false},
	} {
		m := progBoard(t, cfg.width, runs...)
		m.board.admin = cfg.admin
		if !m.boardShowMile() || m.boardShowProg() != cfg.pct {
			t.Fatalf("%s: unexpected milestone/percentage visibility", cfg.name)
		}
		col := -1
		for _, run := range runs {
			line := progLine(m, run.ID)
			i := strings.Index(line, "title "+run.ID)
			if i < 0 {
				t.Fatalf("%s: missing title in %q", cfg.name, line)
			}
			got := visualWidth(line[:i])
			if col < 0 {
				col = got
			} else if col != got {
				t.Errorf("%s: %s title column %d, want %d: %q", cfg.name, run.ID, got, col, line)
			}
		}
	}
}
