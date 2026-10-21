package main

import (
	"image/color"
	"strings"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/runprogress"
)

// boardProgPctWidth fits the longest bounded percentage, "100%".
const boardProgPctWidth = 4

// boardProgTitleFloor is title room before the issue-id prefix and judge marker.
// The merged percentage must leave at least twenty real title columns.
const boardProgTitleFloor = 25

// boardWorkerColumnWidth reserves a gap plus the sixteen-column worker cell.
const boardWorkerColumnWidth = 18

// boardWorkerWidth is the trailing worker column, present from 120 columns up.
func (m tuiModel) boardWorkerWidth() int {
	if m.width >= 120 {
		return boardWorkerColumnWidth
	}
	return 0
}

// boardProgRoom is title room after the merged cell and actual worker column.
func (m tuiModel) boardProgRoom() int {
	prefix := boardRowPrefixWidth(m.board.admin, m.boardShowCred(), m.boardShowCost())
	return m.width - m.boardWorkerWidth() - (prefix + boardMileWidth + 1 + boardProgPctWidth + 2)
}

// boardShowProg adds the percentage only when the title retains its floor.
// Milestones keep their existing #379 threshold and COST shedding order.
func (m tuiModel) boardShowProg() bool {
	return m.boardShowMile() && m.boardProgRoom() >= boardProgTitleFloor
}

// floorProgressPct is the `[p]` sort key (#3207): the percentage boardProgCell draws, or -1
// for every run it draws none for (nil progress, a non-percent or unknown state, a percent
// state without pct), so those sink below every measured run.
func floorProgressPct(r apitypes.RunListItemDTO) int {
	p := r.Progress
	if p == nil || p.State != runprogress.StatePercent || p.Pct == nil {
		return -1
	}
	return min(max(*p.Pct, 0), 100)
}

func compareFloorProgress(a, b apitypes.RunListItemDTO) int {
	return floorProgressPct(b) - floorProgressPct(a)
}

// progParkWord is the short park reason shown after "⏸". Only the closed status enum is
// mapped; nothing from the run is echoed.
func progParkWord(status string) string {
	switch status {
	case "limit_wait":
		return "limit"
	case "pool_wait":
		return "pool"
	case "recovery_wait":
		return "recov"
	default:
		return "paused"
	}
}

// boardProgCell draws only the server percentage; milestone cells are rendered
// alongside it by boardProgressCell. Queued, planning and stalled words already
// appear in the status column.
func (m tuiModel) boardProgCell(p *apitypes.RunProgress, bg color.Color) string {
	if p == nil || p.State != runprogress.StatePercent || p.Pct == nil {
		return ""
	}
	pct := min(max(*p.Pct, 0), 100)
	return paintSeg(m.pal.faintC, bg, false, padLeftVisual(itoa(pct)+"%", boardProgPctWidth))
}

// boardProgressCell is the merged column. A waiting/parked annotation replaces
// the marker only when health precedence hides that actionable status word.
func (m tuiModel) boardProgressCell(r apitypes.RunListItemDTO, terminal bool, bg color.Color, statusWord string) string {
	wide := m.boardShowProg()
	width := boardMileWidth
	if wide {
		width += 1 + boardProgPctWidth
	}
	if !terminal && r.Progress != nil {
		if statusWord == "stalled" || statusWord == "looping" {
			switch r.Progress.State {
			case runprogress.StateWaiting:
				word := "on you"
				if wide {
					word = "waits on you"
				}
				return padSeg(paintSeg(m.pal.amber, bg, false, word), width, bg)
			case runprogress.StateParked:
				return padSeg(paintSeg(m.pal.faintC, bg, false, "⏸ "+progParkWord(r.Status)), width, bg)
			}
		}
	}
	marker := m.milestoneMarker(r, terminal, bg)
	out := padSeg(marker, boardMileWidth, bg)
	if wide {
		pct := ""
		if !terminal && marker != "" {
			pct = m.boardProgCell(r.Progress, bg)
		}
		out += paintSeg(nil, bg, false, " ") + padSeg(pct, boardProgPctWidth, bg)
	}
	return out
}

func padLeftVisual(s string, n int) string {
	if w := visualWidth(s); w < n {
		return strings.Repeat(" ", n-w) + s
	}
	return s
}
