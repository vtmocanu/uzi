package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func splitFillModel(t *testing.T, meterRows int) tuiModel {
	t.Helper()
	m := tuiTestModel(t, &uzicli.FakeClient{}, "")
	if meterRows > 0 {
		m = bothProvidersModel(t, 100,
			[]apitypes.TokenRateLimitDTO{okMeter("first", "personal", true, 35, 62), okMeter("second", "team", false, 88, 44)},
			[]apitypes.CodexAccountRateLimitDTO{codexAcct("primary", "primary", true, "fresh", cwin(71), cwin(29)), codexAcct("team", "team", false, "fresh", cwin(66), cwin(13))})
		if meterRows == 1 {
			m.codexRateLimits = nil
		}
	}
	m = resizeSplit(m, 100, 60)
	if got := len(m.boardMeterLayout(time.Now()).lines); got != meterRows {
		t.Fatalf("meter fixture rows=%d, want %d", got, meterRows)
	}
	return m
}

func assertSplitFilled(t *testing.T, m tuiModel) []string {
	t.Helper()
	if !m.splitDrawn() {
		t.Fatal("fixture is not split")
	}
	lines := strings.Split(stripANSI(m.View().Content), "\n")
	if len(lines) != m.height {
		t.Errorf("frame has %d lines, want terminal height %d", len(lines), m.height)
	}
	if got, want := lines[len(lines)-1], stripANSI(m.splitFooterLine()); got != want || !strings.Contains(got, "q quit") {
		t.Errorf("last line is not the footer: %q", got)
	}
	return lines
}

func TestSplitFillsTerminalWithLiveHeader(t *testing.T) {
	for meters := 0; meters <= 2; meters++ {
		for hints := 0; hints < 8; hints++ {
			for _, bottom := range []tuiView{viewCI, viewPulls} {
				t.Run(fmt.Sprintf("meters=%d/hints=%d/bottom=%d", meters, hints, bottom), func(t *testing.T) {
					m := splitFillModel(t, meters)
					m.vaultLocked = hints&1 != 0
					m.board.adminDenied = hints&2 != 0
					if hints&4 != 0 {
						m.board.err = fmt.Errorf("refresh failed")
					}
					m.bottomTab = bottom
					assertSplitFilled(t, m)
				})
			}
		}
	}
}

func TestSplitHeaderGrowthKeepsSelectedRows(t *testing.T) {
	for _, bottom := range []tuiView{viewCI, viewPulls} {
		t.Run(fmt.Sprintf("bottom=%d", bottom), func(t *testing.T) {
			m := splitFillModel(t, 1)
			m.bottomTab = bottom
			m.repos, m.reposLoaded, m.repoChosen = []apitypes.RepoDTO{oneRepo()}, true, true
			now := time.Now()
			for i := 0; i < 40; i++ {
				m.board.runs = append(m.board.runs, apitypes.RunListItemDTO{RunDTO: apitypes.RunDTO{ID: fmt.Sprintf("floor-%d", i), Kind: "issue", Status: "running", IssueTitle: fmt.Sprintf("floor-item-%02d", i)}})
				ci := sampleCIRuns(now)[0]
				ci.ID, ci.Number = int64(3000+i), int64(3000+i)
				m.ci.runs = append(m.ci.runs, ci)
				p := samplePulls(now)[0]
				p.IID, p.Title = int64(4000+i), fmt.Sprintf("pull-item-%02d", i)
				m.pulls.pulls = append(m.pulls.pulls, p)
			}
			m.board.cursor, m.ci.cursor, m.pulls.cursor = 39, 39, 39
			m.ci.loaded, m.pulls.loaded = true, true
			m.board.scroll = m.syncedScrollAt(m.boardScrollCapacity())
			m.ci.scroll = m.ciSyncedScrollAt(m.ciScrollCapacity())
			m.pulls.scroll = m.pullsSyncedScrollAt(m.pullsScrollCapacity())
			for _, growth := range []bool{false, true} {
				if growth {
					m.board.err = fmt.Errorf("refresh failed")
					m.vaultLocked = true
				}
				lines := assertSplitFilled(t, m)
				top, forge := m.splitHeights()
				header := len(lines) - top - forge - splitSeparator - splitFooter
				floorFrame := strings.Join(lines[header:header+top], "\n")
				forgeFrame := strings.Join(lines[header+top+splitSeparator:len(lines)-splitFooter], "\n")
				if !strings.Contains(floorFrame, "floor-item-39") {
					t.Errorf("growth=%v selected floor row missing from pane", growth)
				}
				want := "#3039"
				if bottom == viewPulls {
					want = "pull-item-39"
				}
				if !strings.Contains(forgeFrame, want) {
					t.Errorf("growth=%v selected forge row %q missing from pane", growth, want)
				}
				if !m.splitLatch {
					t.Fatal("header growth changed latch without resize")
				}
			}
		})
	}
}

func TestSplitFutureHeaderFitsPaneBudget(t *testing.T) {
	m := splitFillModel(t, 0)
	m = resizeSplit(m, 100, splitMinHeight)
	// More header rows than today's maximum must shrink the panes, not overdraw.
	headerRows := splitSharedChrome + 4
	top, bottom := m.splitHeightsWithHeader(headerRows)
	if top < 1 || bottom < 1 || top+bottom+headerRows+splitSeparator+splitFooter != m.height {
		t.Fatalf("future header overdraws: header=%d top=%d bottom=%d height=%d", headerRows, top, bottom, m.height)
	}
}
