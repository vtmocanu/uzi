package main

import (
	"image/color"
	"strings"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/runprogress"
)

const (
	// boardProgBarCells is the PROG bar length (PRD #2602): round(pct*8/100) filled of 8.
	boardProgBarCells = 8
	// boardProgWideWidth is the PROG cell with the bar ("70% ▰▰▰▰▰▰▱▱"); boardProgNarrowWidth
	// is the cell once the bar drops (percent alone or a short flag). The cell is one fixed
	// width per terminal width so the TITLE column stays aligned down the board.
	boardProgWideWidth   = 12
	boardProgNarrowWidth = 8
)

// boardProgTitleFloor is the computed title room (before the variable "#iid " prefix and the
// judge-marker allowance, ~10 cols together) the PROG cell must leave. It keeps at least ~20
// real title columns at every width where PROG is shown.
const boardProgTitleFloor = 25

// boardWorkerColumnWidth is the worker column when it is shown.
const boardWorkerColumnWidth = 18

// boardWorkerWidth is the width of the trailing worker column (two spaces and a fixed 16-column
// cell): 18 from 120 columns up, else 0. boardRow and boardProgRoom both read it.
func (m tuiModel) boardWorkerWidth() int {
	if m.width >= 120 {
		return boardWorkerColumnWidth
	}
	return 0
}

// boardProgRoom is the title width left once the PROG cell of width cell is drawn next to the
// MILES cell, with worker columns set aside for the trailing worker column.
func (m tuiModel) boardProgRoom(cell, worker int) int {
	prefix := boardRowPrefixWidth(m.board.admin, m.boardShowCred(), m.boardShowCost())
	return m.width - worker - (prefix + boardMileWidth + 2 + cell + 2)
}

// boardShowProg reports whether the PROG column is drawn (PRD #2602). It is drawn exactly when
// boardShowMile is, taking its narrow cell out of the flexible title width so MILES keeps its
// #379 threshold; only where that would leave the title under boardProgTitleFloor (the admin
// board with its owner and credential cells) does PROG shed first, before MILES.
func (m tuiModel) boardShowProg() bool {
	return m.boardShowMile() && m.boardProgRoom(boardProgNarrowWidth, m.boardWorkerWidth()) >= boardProgTitleFloor
}

// boardShowProgBar reports whether the PROG cell keeps its 8-cell bar. The bar drops before
// the percent (PRD #2602): it needs boardProgBarMinWidth (shifted by the same extra prefix
// columns that shift boardShowMile) and the wide cell must still leave the title its floor.
// The floor check always reserves the full worker column: counting it only from 120 up made the
// bar flap off at 120-123 after being on at 115-119 (own board with the credential column), so
// the bar now starts at the width where it stays on.
func (m tuiModel) boardShowProgBar() bool {
	min := boardProgBarMinWidth
	min += boardRowPrefixWidth(m.board.admin, m.boardShowCred(), m.boardShowCost()) - boardRowPrefixWidth(false, false, false)
	return m.boardShowProg() && m.width >= min && m.boardProgRoom(boardProgWideWidth, boardWorkerColumnWidth) >= boardProgTitleFloor
}

func (m tuiModel) boardProgWidth() int {
	if m.boardShowProgBar() {
		return boardProgWideWidth
	}
	return boardProgNarrowWidth
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

// boardProgCell renders the PROG cell (without padding): "<pct> <8-cell bar>", or a text flag
// that replaces the number when it would mislead. Every state is plain text, so the colorprofile
// downgrade keeps it. Only the closed state enum and the integer pct are drawn.
func (m tuiModel) boardProgCell(p *apitypes.RunProgress, status string, bg color.Color, bar bool) string {
	if p == nil {
		return ""
	}
	faint := func(s string) string { return paintSeg(m.pal.faintC, bg, false, s) }
	switch p.State {
	case runprogress.StatePercent:
		if p.Pct == nil {
			return ""
		}
		pct := min(max(*p.Pct, 0), 100)
		out := paintSeg(m.pal.faintC, bg, false, padLeftVisual(itoa(pct)+"%", 3))
		if !bar {
			return out
		}
		filled := (pct*boardProgBarCells + 50) / 100
		return out + paintSeg(nil, bg, false, " ") +
			paintSeg(m.pal.tungsten, bg, false, strings.Repeat("▰", filled)) +
			faint(strings.Repeat("▱", boardProgBarCells-filled))
	case runprogress.StateStalled:
		return paintSeg(m.pal.stall, bg, false, "stalled")
	case runprogress.StateWaiting:
		if bar {
			return paintSeg(m.pal.amber, bg, false, "waits on you")
		}
		return paintSeg(m.pal.amber, bg, false, "on you")
	case runprogress.StateParked:
		return faint("⏸ " + progParkWord(status))
	case runprogress.StateQueued:
		return faint("queued")
	case runprogress.StatePlanning:
		return faint("planning")
	}
	return ""
}

func padLeftVisual(s string, n int) string {
	if w := visualWidth(s); w < n {
		return strings.Repeat(" ", n-w) + s
	}
	return s
}
