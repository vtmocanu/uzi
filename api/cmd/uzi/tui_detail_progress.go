package main

import (
	"strings"

	"github.com/charmbracelet/x/ansi"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/runprogress"
)

// nowLabel is the hanging-indent label of the wrapped model summary; nowMaxRows bounds its height.
const (
	nowLabel   = "now "
	nowMaxRows = 3
)

// wrapWords greedily word-wraps already-sanitized single-line text to width columns and at most
// maxRows rows; text that does not fit ends its last row with an ellipsis. A word wider than a
// row is split across rows.
func wrapWords(text string, width, maxRows int) []string {
	if width < 2 || maxRows < 1 {
		return nil
	}
	var rows []string
	cur := ""
	flush := func() {
		if cur != "" {
			rows = append(rows, cur)
			cur = ""
		}
	}
	for _, word := range strings.Fields(text) {
		for visualWidth(word) > width {
			flush()
			// ansi.Truncate cuts on grapheme-cluster boundaries, so a CJK pair or a VS16 emoji
			// ("\u2764\ufe0f") is never split or mis-measured per rune.
			head := ansi.Truncate(word, width, "")
			cut := len(head)
			if cut == 0 || cut >= len(word) {
				return rows
			}
			rows = append(rows, word[:cut])
			word = word[cut:]
		}
		switch {
		case cur == "":
			cur = word
		case visualWidth(cur)+1+visualWidth(word) <= width:
			cur += " " + word
		default:
			flush()
			cur = word
		}
	}
	flush()
	if len(rows) <= maxRows {
		return rows
	}
	return append(rows[:maxRows-1], clampVisual(rows[maxRows-1]+" "+rows[maxRows], width))
}

// renderProgress is the crew rail's PROGRESS block (PRD #2602), drawn above MILESTONES and
// separate from renderMilestones so it still renders for a planning, plan-gate or
// no-milestone run (renderMilestones returns "" for an empty frozen list). The rail is
// laneRailWidth columns: the block is a top line (the percent or a state flag, with the
// milestone done/total counts when available in the percent state) followed by optional
// phase, Now-note, since, planning and may-be-blocked-by lines.
//
// Every state has a text form, so the colorprofile Ascii/NoTTY downgrade loses only colour.
// Phase and MaybeBlockedByRunID are server strings derived from untrusted repo/agent/question
// text, so each is drawn only through m.renderer.Plain (D7); the Now note takes the same path
// in nowNoteLines. The block is "" for a nil, none or unknown state with no blocked-by hint.
func (m tuiModel) renderProgress() string {
	run := m.detail.run
	p := run.Progress
	if p == nil {
		return ""
	}
	faint := func(s string) string { return m.pal.faint.Render(s) }
	head := faint("PROGRESS")
	var lines []string
	switch p.State {
	case runprogress.StatePercent:
		if p.Pct == nil {
			break
		}
		// MILESTONES draws position, title and bar just below, so this is the compact two-row form
		// that keeps the crew roster unfolded on the standard running scene when there is no Now note;
		// with a note the roster may auto-fold on short terminals (railAutoFolded), the graceful degrade.
		top := head + " " + paintSeg(m.pal.tungsten, nil, true, "≈"+itoa(min(max(*p.Pct, 0), 100))+"%")
		if p.MilestoneTotal > 0 {
			top += faint(" · " + itoa(p.MilestoneDone) + "/" + itoa(p.MilestoneTotal))
		}
		lines = append(lines, top)
		if p.Phase != "" {
			lines = append(lines, faint("phase ▸ ")+m.renderer.Plain(p.Phase, laneRailWidth-visualWidth("phase ▸ ")))
		}
		if p.NowNote != nil {
			lines = append(lines, m.nowNoteLines(p.NowNote, faint)...)
		}
	case runprogress.StateStalled:
		lines = append(lines, head+" "+paintSeg(m.pal.stall, nil, false, "◼ stalled"))
		if at := run.HealthSince; at != nil {
			lines = append(lines, faint("since "+at.UTC().Format("15:04")))
		}
	case runprogress.StateWaiting:
		lines = append(lines, head+" "+paintSeg(m.pal.amber, nil, false, "● waits on you"))
		kind := ""
		switch run.Status {
		case "awaiting_approval":
			kind = "plan gate"
		case "awaiting_input":
			kind = "question"
		case "awaiting_followup":
			kind = "follow-up"
		}
		since := run.UpdatedAt
		if run.StatusSince != nil {
			since = *run.StatusSince
		}
		switch {
		case kind != "" && !since.IsZero():
			lines = append(lines, faint(kind+" since "+since.UTC().Format("15:04")))
		case kind != "":
			lines = append(lines, faint(kind))
		}
	case runprogress.StateParked:
		lines = append(lines, head+" "+faint("⏸ "+progressParkWord(run.Status)))
	case runprogress.StateQueued:
		lines = append(lines, head+" "+faint("queued"))
	case runprogress.StatePlanning:
		lines = append(lines, head+" "+faint("planning"))
		if len(run.Milestones) == 0 {
			lines = append(lines, faint("no milestones frozen yet"))
		}
	}
	if p.MaybeBlockedByRunID != nil && *p.MaybeBlockedByRunID != "" {
		id := shortRunID(m.renderer.Plain(*p.MaybeBlockedByRunID, 64))
		if len(lines) == 0 {
			lines = append(lines, head)
		}
		lines = append(lines, paintSeg(m.pal.wait, nil, false, "⧗ may be blocked by"), "  "+paintSeg(m.pal.wait, nil, false, id))
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n")
}

// nowNoteLines draws the model-written (untrusted) Now summary (PRD #2603 mock 4) inside the rail:
// the text wraps under a `now` label, capped at nowMaxRows rows, and `model summary · <age> ago`
// rides the last text row when it fits laneRailWidth, else its own faint row, so the marking is
// never clamped away by joinColumns. It returns nil for a blank note.
func (m tuiModel) nowNoteLines(note *apitypes.ProgressNote, faint func(string) string) []string {
	indent := strings.Repeat(" ", visualWidth(nowLabel))
	rowW := laneRailWidth - visualWidth(nowLabel)
	text := m.renderer.Plain(note.Text, rowW*nowMaxRows+nowMaxRows)
	wrapped := wrapWords(text, rowW, nowMaxRows)
	if len(wrapped) == 0 {
		return nil
	}
	tag := "model summary"
	if !note.At.IsZero() {
		tag += " · " + relAge(note.At) + " ago"
	}
	last := len(wrapped) - 1
	// The label and the hanging indent are the same width, so one test covers every row count.
	inline := visualWidth(indent)+visualWidth(wrapped[last])+3+visualWidth(tag) <= laneRailWidth
	var lines []string
	for i, row := range wrapped {
		prefix := indent
		if i == 0 {
			prefix = faint(nowLabel)
		}
		if i == last && inline {
			row += faint(" · " + tag)
		}
		lines = append(lines, prefix+row)
	}
	if !inline {
		lines = append(lines, faint(tag))
	}
	return lines
}
