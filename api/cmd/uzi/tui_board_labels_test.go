package main

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// boardTestStatusW is the status cell width boardRow draws for the model's visible rows (the
// rendered window, for boards short enough to show every row): measured, not the fixed max.
func boardTestStatusW(m tuiModel) int {
	return m.boardAccountCols(m.board.visible()).statusW
}

// boardHeadingText is a band heading line without its faint column labels: the heading's own
// tokens are single-space separated, and labels always sit at least two blanks away.
func boardHeadingText(line string) string {
	return strings.SplitN(strings.TrimSpace(line), "  ", 2)[0]
}

// labelRuns is durRuns with every column populated: account, cost, judge badge, worker and a
// percentage, so each label has a cell to sit over.
func labelRuns() (done, running apitypes.RunListItemDTO) {
	done, running = durRuns()
	pct := 70
	running.Progress = &apitypes.RunProgress{State: "percent", Pct: &pct}
	for _, r := range []*apitypes.RunListItemDTO{&done, &running} {
		r.Harness = "claude"
		r.AnthropicSecretID, r.AnthropicSecretLabel = sp("fixture-account"), sp("credential")
		r.Usage = &apitypes.UsageDTO{CostStatus: "metered", CostUSD: 1234}
		r.WorkerName = sp("wk-alpha")
	}
	done.JudgeVerdict, done.JudgeTodoCount = sp("issues"), 3
	return done, running
}

func asciiBoard(t *testing.T, width int, cred bool, runs ...apitypes.RunListItemDTO) tuiModel {
	t.Helper()
	m := progBoard(t, width, runs...)
	if cred {
		m.tokenCount = 2
	}
	return step(m, tea.ColorProfileMsg{Profile: colorprofile.Ascii})
}

// rowCol is the rune column of sub in line, or -1.
func rowCol(line, sub string) int { return runeCol(line, sub) }

// The status cell takes the width of the longest status word among the rendered rows, so a
// short-word window gives the title the spare columns; one long word restores 13. Column
// visibility (thresholds) does not move.
func TestBoardStatusCellMeasured(t *testing.T) {
	done, running := labelRuns()
	parked := progRun("pppppppp", "recovery_wait", nil)
	word := newPalette(false).runStateToken(parked.RunDTO, false).word
	if visualWidth(word) != 13 {
		t.Fatalf("fixture: recovery_wait word %q is not 13 wide", word)
	}
	short := asciiBoard(t, 160, true, done, running)
	long := asciiBoard(t, 160, true, done, running, parked)
	if got := boardTestStatusW(short); got != len("running") {
		t.Errorf("short window status width = %d, want %d", got, len("running"))
	}
	if got := boardTestStatusW(long); got != 13 {
		t.Errorf("window with %q: status width = %d, want 13", word, got)
	}
	ts, tl := rowCol(progLine(short, done.ID), "title "), rowCol(progLine(long, done.ID), "title ")
	if ts < 0 || tl-ts != 13-len("running") {
		t.Errorf("title column short=%d long=%d, want a %d-column gain", ts, tl, 13-len("running"))
	}
	if short.boardShowMile() != long.boardShowMile() || short.boardShowCost() != long.boardShowCost() || short.boardShowDur() != long.boardShowDur() {
		t.Error("column visibility depends on the measured status width")
	}
}

// Every label on a band heading starts exactly where its cell starts in that band's rows
// (right-aligned cells: ends where the cell's text ends), under the Ascii profile.
func TestBoardEyebrowLabelsAlignWithCells(t *testing.T) {
	done, running := labelRuns()
	for _, cred := range []bool{false, true} {
		for _, w := range []int{100, 123, 160, 200} {
			m := asciiBoard(t, w, cred, done, running)
			out := stripANSI(m.View().Content)
			for _, band := range []struct {
				heading, id, age, dur string
			}{{"ON THE FLOOR", running.ID, "age", ""}, {"DONE", done.ID, "ended", "1h12m"}} {
				head := lineWith(t, out, " "+band.heading+" ")
				row := progLine(m, band.id)
				check := func(label, cell string, rightAligned bool) {
					lc := rowCol(head, label)
					if lc < 0 {
						return // dropped or column hidden; visibility is checked below
					}
					cc := rowCol(row, cell)
					if rightAligned {
						lc, cc = lc+len([]rune(label)), cc+len([]rune(cell))
					}
					if cc < 0 || lc != cc {
						t.Errorf("cred %t w %d %s: label %q at %d, cell %q at %d\n%q\n%q", cred, w, band.heading, label, lc, cell, cc, head, row)
					}
				}
				ageCell := "3h"
				if band.heading == "DONE" {
					ageCell = "20m"
				}
				check(band.age, ageCell, false)
				if band.dur != "" && m.boardShowDur() {
					check("took", band.dur, true)
				}
				check("miles", "▰", false)
				check("acct", "credential", false)
				check("cost", "$1234", true)
				check("title", "title ", false)
				if band.heading == "DONE" {
					check("judge", "⚖", false)
				} else if rowCol(head, "judge") >= 0 {
					t.Errorf("cred %t w %d %s: judge labelled on a band with no verdicts: %q", cred, w, band.heading, head)
				}
				check("worker", "wk-alpha", false)
				// Labels follow their columns.
				for label, shown := range map[string]bool{
					"miles": m.boardShowMile(), "acct": m.boardShowCred(), "cost": m.boardShowCost(),
					"worker": m.boardWorkerWidth() > 0,
				} {
					if !shown && rowCol(head, label) >= 0 {
						t.Errorf("cred %t w %d %s: %q labels a hidden column", cred, w, band.heading, label)
					}
				}
				// Not vacuous: on a wide board every shown column's label is actually present.
				if w >= 160 {
					want := []string{band.age, "miles", "cost", "title", "worker"}
					if band.dur != "" {
						want = append(want, "took", "judge")
					}
					if cred {
						want = append(want, "acct")
					}
					for _, l := range want {
						if rowCol(head, l) < 0 {
							t.Errorf("cred %t w %d %s: label %q missing: %q", cred, w, band.heading, l, head)
						}
					}
				}
				if band.heading != "DONE" && rowCol(head, "took") >= 0 {
					t.Errorf("w %d: took on an active band", w)
				}
				if visualWidth(head) > w {
					t.Errorf("w %d: heading overflows: %q", w, head)
				}
			}
		}
	}
}

// The admin board's labels align too, and it never carries took/cost/judge (and no owner
// label: the owner cell sits under the heading text).
func TestBoardEyebrowLabelsAdmin(t *testing.T) {
	_, running := labelRuns()
	running.OwnerEmail = sp("owner@example.test")
	m := asciiBoard(t, 160, false, running)
	m.board.admin = true
	m = step(m, boardRunsMsg{reqID: m.board.waitID, admin: true, runs: []apitypes.RunListItemDTO{running}})
	out := stripANSI(m.View().Content)
	head := lineWith(t, out, " ON THE FLOOR ")
	row := progLine(m, running.ID)
	if lc, cc := rowCol(head, "title"), rowCol(row, "title "); lc < 0 || lc != cc {
		t.Errorf("admin title label at %d, cell at %d\n%q\n%q", lc, cc, head, row)
	}
	for _, l := range []string{"took", "cost", "judge", "owner"} {
		if rowCol(head, l) >= 0 {
			t.Errorf("admin heading carries %q: %q", l, head)
		}
	}
}

// placeBoardLabels never moves a label: it drops one that would overlap the heading (two blank
// columns kept), the previous label (one kept) or the right edge.
func TestPlaceBoardLabels(t *testing.T) {
	labels := []boardColLabel{{"ended", 10}, {"took", 14}, {"miles", 20}, {"acct", 30}, {"cost", 32}, {"title", 40}, {"worker", 46}}
	got := placeBoardLabels(9, 50, labels)
	// "ended" needs heading+2 = 11: dropped. "took" at 14 fits. "cost" at 32 overlaps "acct"
	// (30..33): dropped. "worker" ends at 52 > 50: dropped.
	want := strings.Repeat(" ", 14-9) + "took" + strings.Repeat(" ", 20-18) + "miles" + strings.Repeat(" ", 30-25) + "acct" + strings.Repeat(" ", 40-34) + "title"
	if got != want {
		t.Errorf("placeBoardLabels =\n%q\nwant\n%q", got, want)
	}
	// "ended" (five wide) sits over the four-column age cell when the heading leaves room.
	if got := placeBoardLabels(3, 50, labels[:1]); got != strings.Repeat(" ", 7)+"ended" {
		t.Errorf("ended over the age cell = %q", got)
	}
}

// An account column with no labels in the window (zero measured width) and a short cost
// collapse their cells; overlapping labels drop instead of shifting a column.
func TestBoardEyebrowLabelsCollapsedCells(t *testing.T) {
	done, running := labelRuns()
	for _, r := range []*apitypes.RunListItemDTO{&done, &running} {
		r.AnthropicSecretID, r.AnthropicSecretLabel = nil, nil
		r.Usage = &apitypes.UsageDTO{CostStatus: "metered", CostUSD: 3}
	}
	m := asciiBoard(t, 200, true, done, running)
	head := lineWith(t, stripANSI(m.View().Content), " DONE ")
	labels := strings.Fields(strings.TrimPrefix(strings.TrimSpace(head), boardHeadingText(head)))
	known := map[string]bool{"ended": true, "took": true, "miles": true, "acct": true, "cost": true, "title": true, "judge": true, "worker": true}
	for _, l := range labels {
		if !known[l] {
			t.Errorf("garbled label %q in %q", l, head)
		}
	}
	if rowCol(head, "acct") >= 0 && rowCol(head, "cost") >= 0 {
		t.Errorf("zero-width account and 2-wide cost both labelled (they overlap): %q", head)
	}
	if rowCol(head, "title") < 0 {
		t.Errorf("title label lost: %q", head)
	}
}

// The board's ? legend still explains the DONE numbers in the labels' words.
func TestBoardHelpMatchesLabels(t *testing.T) {
	help := strings.Join(helpLines(viewBoard), "\n")
	for _, w := range []string{"ended", "took"} {
		if !strings.Contains(help, w) {
			t.Errorf("board help lacks %q:\n%s", w, help)
		}
	}
}

// A verdict on an active run (rare, but possible) labels that band's judge column too.
func TestBoardEyebrowJudgeLabelFollowsVerdicts(t *testing.T) {
	done, running := labelRuns()
	running.JudgeVerdict = sp("ok")
	m := asciiBoard(t, 200, true, done, running)
	out := stripANSI(m.View().Content)
	for _, h := range []string{" ON THE FLOOR ", " DONE "} {
		head := lineWith(t, out, h)
		if lc, cc := rowCol(head, "judge"), rowCol(out[strings.Index(out, head):], "⚖"); lc < 0 || cc < 0 {
			t.Errorf("%s: judge label %d with a verdict on the band: %q", h, lc, head)
		}
	}
}

// A long DONE heading (three-digit count, two-digit failures) keeps its `ended` label: the
// status cell grows just enough (never past the fixed max) instead of dropping the label the
// two DONE numbers depend on (tui-ux review of #3732).
func TestBoardDoneHeadingKeepsEndedLabel(t *testing.T) {
	done, running := labelRuns()
	runs := []apitypes.RunListItemDTO{running}
	for i := 0; i < 120; i++ {
		r := done
		r.ID = "d" + itoa(1000000+i)
		if i < 35 {
			r.Status = "failed"
		}
		runs = append(runs, r)
	}
	for _, w := range []int{100, 160} {
		m := asciiBoard(t, w, false, runs...)
		m.height = 200
		out := stripANSI(m.View().Content)
		head := lineWith(t, out, " DONE ")
		if !strings.Contains(head, "DONE · 120 · 35 failed") {
			t.Fatalf("fixture heading: %q", head)
		}
		lc := rowCol(head, "ended")
		if lc < 0 {
			t.Fatalf("w %d: ended dropped from %q", w, head)
		}
		row := lineWith(t, out, runs[1].ID)
		if cc := rowCol(row, "20m"); cc != lc {
			t.Errorf("w %d: ended at %d, age cell at %d\n%q\n%q", w, lc, cc, head, row)
		}
		if got := m.boardAccountCols(m.board.visible()).statusW; got > boardStatusWordWidth {
			t.Errorf("status width %d past the max", got)
		}
	}
}

// A band heading at the window's bottom edge with none of its rows shown carries no labels.
// The floor-band size is swept until the DONE heading lands on the last body line.
func TestBoardOrphanHeadingUnlabelled(t *testing.T) {
	done, running := labelRuns()
	found := false
	for n := 1; n <= 30 && !found; n++ {
		runs := []apitypes.RunListItemDTO{done}
		for i := 0; i < n; i++ {
			r := running
			r.ID = "r" + itoa(1000000+i)
			runs = append(runs, r)
		}
		m := asciiBoard(t, 160, false, runs...)
		m.height = 16
		lines := strings.Split(stripANSI(m.View().Content), "\n")
		for i, l := range lines {
			if !strings.HasPrefix(l, " DONE ") || i+1 >= len(lines) || strings.Contains(lines[i+1], "▌") {
				continue
			}
			found = true
			if boardHeadingText(l) != strings.TrimSpace(l) {
				t.Errorf("n %d: orphan DONE heading carries labels: %q", n, l)
			}
		}
	}
	if !found {
		t.Fatal("fixture: no floor size put the DONE heading on the window's last body line")
	}
}
