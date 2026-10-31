package main

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func ciLabelModel(t *testing.T, width int, runs []apitypes.CIRunDTO) tuiModel {
	t.Helper()
	repo := apitypes.RepoDTO{ID: "r1", PathWithNamespace: "acme/app", Enabled: true, WebURL: "https://forge.example/acme/app"}
	m := loadedCI(t, &uzicli.FakeClient{Repos: []apitypes.RepoDTO{repo}}, runs)
	m = step(m, tea.ColorProfileMsg{Profile: colorprofile.Ascii})
	return step(m, tea.WindowSizeMsg{Width: width, Height: 40})
}

// runesAt is the len(want)-rune slice of line starting at rune column c ("" when out of range).
func runesAt(line string, c, n int) string {
	r := []rune(line)
	if c < 0 || c+n > len(r) {
		return ""
	}
	return string(r[c : c+n])
}

// Every label on a CI band heading starts exactly at its cell's first column in that band's
// rows, at widths spanning every ciColumnsFor tier (30..200, down past the right cell); labels follow the tiers, the RUNNING band
// reads elapsed and jobs/age, finished bands took and age, and no heading overflows.
func TestCIEyebrowLabelsAlignWithCells(t *testing.T) {
	now := time.Now()
	runs := sampleCIRuns(now)
	byNum := map[int64]apitypes.CIRunDTO{}
	for _, r := range runs {
		byNum[r.Number] = r
	}
	sawAll, sawNoRight := false, false
	for w := 30; w <= 200; w++ {
		m := ciLabelModel(t, w, runs)
		out := stripANSI(m.View().Content)
		cols := ciColumnsFor(w)
		for _, band := range []struct {
			heading    string
			num        int64
			dur, right string
			rightCell  string
		}{
			{"RUNNING ·", 1039, "elapsed", "jobs/age", "▰"},
			{"FAILED ·", 1037, "took", "age", relAge(byNum[1037].CreatedAt)},
			{"RECENT ·", 1038, "took", "age", relAge(byNum[1038].CreatedAt)},
		} {
			head := lineWith(t, out, " "+band.heading)
			row := lineWith(t, out, "#"+itoa(int(band.num))+" ")
			r := byNum[band.num]
			if visualWidth(head) > w {
				t.Errorf("w %d: heading overflows: %q", w, head)
			}
			want := map[string]string{
				"event":    r.Event,
				"branch":   string([]rune(r.Branch)[:4]),
				"status":   string([]rune(forgeState(r.Status, r.Conclusion))[:4]),
				band.dur:   ciRunElapsed(r),
				"title":    string([]rune(r.Title)[:5]),
				band.right: band.rightCell,
			}
			shown := map[string]bool{"event": cols.event, "branch": cols.branch, "status": cols.statusElapsed,
				band.dur: cols.statusElapsed, "title": true, band.right: cols.right}
			present := 0
			for label, cell := range want {
				lc := runeCol(head, " "+label) + 1
				if label == band.right {
					lc = runeCol(head, label) // right label may follow "title" text-free gap
				}
				if lc <= 0 {
					continue
				}
				present++
				if !shown[label] {
					t.Errorf("w %d %s: %q labels a hidden column: %q", w, band.heading, label, head)
				}
				if got := runesAt(row, lc, len([]rune(cell))); got != cell {
					t.Errorf("w %d %s: label %q at %d sits over %q, want %q\n%q\n%q", w, band.heading, label, lc, got, cell, head, row)
				}
			}
			for _, other := range []string{"elapsed", "took", "jobs/age"} {
				if other != band.dur && other != band.right && strings.Contains(head, other) {
					t.Errorf("w %d %s: wrong-band label %q: %q", w, band.heading, other, head)
				}
			}
			if cols.event && cols.branch && cols.statusElapsed && cols.right && w >= 120 {
				if present != len(want) {
					t.Errorf("w %d %s: %d of %d labels present: %q", w, band.heading, present, len(want), head)
				}
				sawAll = true
			}
			if !cols.right {
				sawNoRight = true // the right label's hidden-column check above covered this tier
			}
		}
	}
	if !sawAll || !sawNoRight {
		t.Fatalf("sweep missed a tier: full set %t, right cell dropped %t", sawAll, sawNoRight)
	}
}

// The CI ? overlay explains the duration labels and fits 80 columns.
func TestCIHelpExplainsLabels(t *testing.T) {
	var found bool
	for _, l := range helpLines(viewCI) {
		if strings.Contains(l, "elapsed / took") && strings.Contains(l, "age") {
			found = true
			if visualWidth(l) > 80 {
				t.Errorf("help line %d cols: %q", visualWidth(l), l)
			}
		}
	}
	if !found {
		t.Error("CI help lacks the elapsed/took/age legend")
	}
}
