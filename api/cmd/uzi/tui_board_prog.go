package main

import (
	"image/color"
	"strings"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/runprogress"
)

// The merged progress cell keeps the milestone renderer's nine columns and an
// optional four-column percentage. Annotation visibility reserves the worker column
// even below 120, so it cannot flap off when that column appears.
const boardProgPctWidth = 4
const boardProgTitleFloor = 25
const boardWorkerColumnWidth = 18

func (m tuiModel) boardWorkerWidth() int {
	if m.width >= 120 {
		return boardWorkerColumnWidth
	}
	return 0
}

func (m tuiModel) boardProgRoom() int {
	prefix := boardRowPrefixWidth(m.board.admin, m.boardShowCred(), m.boardShowCost())
	return m.width - boardWorkerColumnWidth - (prefix + boardMileWidth + 1 + boardProgPctWidth + 2)
}

// boardShowProg adds the percentage only when the title retains its floor.
// Milestones keep their existing #379 threshold and COST shedding order.
func (m tuiModel) boardShowProg() bool {
	return m.boardShowMile() && m.boardProgRoom() >= boardProgTitleFloor
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
func (m tuiModel) boardProgressCell(r apitypes.RunListItemDTO, terminal bool, bg color.Color) string {
	wide := m.boardShowProg()
	width := boardMileWidth
	if wide {
		width += 1 + boardProgPctWidth
	}
	if !terminal && r.Progress != nil {
		tok := m.pal.runStateToken(r.RunDTO, r.IsRevising)
		if tok.word == "stalled" || tok.word == "looping" {
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
	out := padSeg(m.milestoneMarker(r, terminal, bg), boardMileWidth, bg)
	if wide {
		pct := ""
		if !terminal {
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
