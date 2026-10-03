package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// The scroll and renderer must use the same pane capacity, including the
// selected row's reserved second line.
func TestTUIListsFitPaneHeight(t *testing.T) {
	const paneHeight = 8
	runs := make([]apitypes.RunListItemDTO, 12)
	for i := range runs {
		runs[i] = apitypes.RunListItemDTO{RunDTO: apitypes.RunDTO{
			ID: fmt.Sprintf("run-%02d", i), Kind: "issue", Status: "running",
			IssueTitle: fmt.Sprintf("board item %02d", i),
		}}
	}
	board := tuiTestModel(t, &uzicli.FakeClient{}, "")
	runs[len(runs)-1].CurrentActivity = activityFor("coder", "task-last", time.Now())
	board.board.runs = runs
	board.board.cursor = len(runs) - 1
	board.board.scroll = board.syncedScrollAt(board.boardCapacityAt(paneHeight, 0, false))

	now := time.Now()
	ciRuns := sampleCIRuns(now)
	ci := loadedCI(t, &uzicli.FakeClient{Repos: []apitypes.RepoDTO{oneRepo()}}, ciRuns)
	ci.ci.cursor = len(ciRuns) - 1
	ci.ci.scroll = ci.ciSyncedScrollAt(ci.ciCapacityAt(paneHeight, false))

	pulls := samplePulls(now)
	pr := loadedPulls(t, &uzicli.FakeClient{Repos: []apitypes.RepoDTO{oneRepo()}}, pulls)
	pr.pulls.cursor = len(pulls) - 1
	pr.pulls.scroll = pr.pullsSyncedScrollAt(pr.pullsCapacityAt(paneHeight, false))

	for _, tc := range []struct {
		name, frame, selected, secondLine string
	}{
		{"board", board.renderBoardBody(paneHeight, false), "board item 11", "task-last"},
		{"ci", ci.renderCIBody(paneHeight, false), "release", "chore(release): 0.77.0"},
		{"pulls", pr.renderPullsBody(paneHeight, false), "Bump golangci-lint", "no conflicts with main"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plain := stripANSI(tc.frame)
			if !strings.Contains(plain, tc.selected) {
				t.Errorf("selected row %q absent from pane:\n%s", tc.selected, plain)
			}
			if !strings.Contains(plain, tc.secondLine) {
				t.Errorf("selected second line %q absent from pane:\n%s", tc.secondLine, plain)
			}
			if lines := strings.Count(strings.TrimSuffix(tc.frame, "\n"), "\n") + 1; lines > paneHeight {
				t.Errorf("pane uses %d lines, height %d:\n%s", lines, paneHeight, plain)
			}
		})
	}
}

func TestTUIFullScreenWrappersMatchBodies(t *testing.T) {
	m := tuiTestModel(t, &uzicli.FakeClient{}, "")
	if got, want := m.renderBoard(), m.renderBoardBody(m.height, true); got != want {
		t.Error("board full-screen frame differs from body wrapper")
	}
	m.board.admin = true
	if first := strings.SplitN(stripANSI(m.renderBoard()), "\n", 2)[0]; !strings.Contains(first, "active runs") {
		t.Errorf("admin board full-screen strip should label active runs: %q", first)
	}
	m.view = viewCI
	if got, want := m.renderCI(), m.renderCIBody(m.height, true); got != want {
		t.Error("CI full-screen frame differs from body wrapper")
	}
	ciFrame := m.renderCI()
	m.view = viewPulls
	if got, want := m.renderPulls(), m.renderPullsBody(m.height, true); got != want {
		t.Error("pulls full-screen frame differs from body wrapper")
	}
	for _, tc := range []struct {
		name, frame string
	}{
		{"ci", ciFrame},
		{"pulls", m.renderPulls()},
	} {
		first := strings.SplitN(stripANSI(tc.frame), "\n", 2)[0]
		if !strings.Contains(first, "floor") || strings.Contains(first, "active runs") {
			t.Errorf("%s admin full-screen strip should label floor: %q", tc.name, first)
		}
	}
}
