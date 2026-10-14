package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// renderedFleetTitle isolates the title from CHECKS tables and body readouts.
func renderedFleetTitle(m tuiModel) string {
	lines := strings.Split(stripANSI(m.View().Content), "\n")
	n := 1
	if !strings.Contains(lines[0], "workers ·") {
		n = 2
	}
	return strings.Join(lines[:n], "\n")
}

func TestIdleCrossChecksRenderedTitles(t *testing.T) {
	for _, split := range []bool{false, true} {
		for _, width := range []int{200, 80} {
			for _, active := range []int{0, 1} {
				t.Run(fmt.Sprintf("split=%v/width=%d/active=%d", split, width, active), func(t *testing.T) {
					m := crossCheckFleetScene(true, split, width, active)
					if m.splitDrawn() != split {
						t.Fatalf("split=%v, want %v", m.splitDrawn(), split)
					}
					title := renderedFleetTitle(m)
					if active == 0 {
						if strings.Contains(title, "checks") {
							t.Errorf("idle title contains cross-check segment: %q", title)
						}
					} else {
						want := "1/4 checks"
						if width == 200 {
							want = "1/4 cross-checks"
						}
						if !strings.Contains(title, want) {
							t.Errorf("active title missing %q: %q", want, title)
						}
					}
					if !strings.Contains(title, "workers") || !strings.Contains(title, "runs") && !strings.Contains(title, "run slots") {
						t.Errorf("other fleet segments lost: %q", title)
					}
				})
			}
		}
	}
}

func TestIdleCrossChecksWorkersTitle(t *testing.T) {
	for _, split := range []bool{false, true} {
		m := crossCheckFleetScene(true, split, 200, 0)
		m = key(m, keyViewWorkers)
		title := renderedFleetTitle(m)
		if split {
			if strings.Contains(title, "checks") {
				t.Fatalf("split Workers-top idle shared title: %q", title)
			}
		} else if !strings.Contains(title, "0/4 cross-checks") {
			t.Fatalf("full Workers title lost idle capacity: %q", title)
		}
		if !strings.Contains(stripANSI(m.View().Content), "CHECKS") {
			t.Fatal("Workers CHECKS table missing")
		}
	}
}

func TestIdleCrossChecksBoardRecoversBodyRow(t *testing.T) {
	m := crossCheckFleetScene(true, false, 80, 0)
	// At this fixed width the offline attention segment makes the visible idle
	// title wrap; the hidden title fits beside the board wordmark.
	if visible := m.workerFleetTitleLines(" "+m.boardTitle(true), false); len(visible) != 2 {
		t.Fatalf("fixture requires two title rows with idle count visible, got %d", len(visible))
	}
	var runs []apitypes.RunListItemDTO
	for i := 0; i < 30; i++ {
		r := milestoneRunItem(fmt.Sprintf("idle-%02d", i), "running", 1, "m2")
		r.IssueTitle = fmt.Sprintf("body-%02d", i)
		runs = append(runs, r)
	}
	m = step(m, boardRunsMsg{reqID: m.board.waitID, runs: runs})
	for i := 0; i < len(runs)-1; i++ {
		m = key(m, "j")
	}
	sel, ok := m.board.selected()
	if !ok || sel.ID != "idle-29" {
		t.Fatalf("bottom selection=%q, ok=%v", sel.ID, ok)
	}
	frame := stripANSI(m.View().Content)
	lines := strings.Split(frame, "\n")
	if !strings.Contains(lines[0], "workers ·") {
		t.Errorf("rendered title is two rows, want one: %q / %q", lines[0], lines[1])
	}
	// Title, run summary, blank and footer consume four rows. The band
	// eyebrow is outside the bottom window, so all twelve items are runs.
	if got := m.boardCapacity(); got != 12 {
		t.Errorf("capacity=%d, want 12", got)
	}
	count := 0
	for _, line := range lines {
		if strings.Contains(line, "body-") {
			count++
		}
	}
	if count != 12 {
		t.Errorf("visible body rows=%d, want 12", count)
	}
	if !strings.Contains(frame, "body-18") {
		t.Error("recovered body row body-18 missing")
	}
	if !strings.Contains(frame, "body-29") {
		t.Error("selected bottom row missing")
	}
	if len(lines) != 16 || !strings.Contains(lines[15], "quit") {
		t.Errorf("footer position: height=%d last=%q", len(lines), lines[len(lines)-1])
	}
}
