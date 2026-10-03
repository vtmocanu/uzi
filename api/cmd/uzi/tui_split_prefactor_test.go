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
		name, frame, selected string
	}{
		{"board", board.renderBoardBody(paneHeight, false), "board item 11"},
		{"ci", ci.renderCIBody(paneHeight, false), "release"},
		{"pulls", pr.renderPullsBody(paneHeight, false), "Bump golangci-lint"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plain := stripANSI(tc.frame)
			if !strings.Contains(plain, tc.selected) {
				t.Errorf("selected row %q absent from pane:\n%s", tc.selected, plain)
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
	m.view = viewCI
	if got, want := m.renderCI(), m.renderCIBody(m.height, true); got != want {
		t.Error("CI full-screen frame differs from body wrapper")
	}
	m.view = viewPulls
	if got, want := m.renderPulls(), m.renderPullsBody(m.height, true); got != want {
		t.Error("pulls full-screen frame differs from body wrapper")
	}
}
