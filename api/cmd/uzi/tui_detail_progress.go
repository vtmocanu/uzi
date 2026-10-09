package main

import (
	"strings"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/runprogress"
)

// progressBlockedIDWidth is the shortened blocked-by run id width (the board and CLI show 8 chars).
const progressBlockedIDWidth = 8

// renderProgress is the crew rail's PROGRESS block (PRD #2602), drawn above MILESTONES and
// separate from renderMilestones so it still renders for a planning, plan-gate or
// no-milestone run (renderMilestones returns "" for an empty frozen list). The rail is
// laneRailWidth (26) columns, so the mock's one-line block wraps to one fact per row.
//
// Every state has a text form, so the colorprofile Ascii/NoTTY downgrade loses only colour.
// ActiveMilestoneID, Phase and MaybeBlockedByRunID are server strings derived from untrusted
// repo/agent/question text, so each is drawn only through m.renderer.Plain (D7). The block is
// "" for a nil, none or unknown state with no blocked-by hint.
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
		lines = append(lines, head+" "+paintSeg(m.pal.tungsten, nil, true, "≈"+itoa(min(max(*p.Pct, 0), 100))+"%"))
		lines = append(lines, m.progressMilestoneLines(run, p)...)
		if p.Phase != "" {
			lines = append(lines, faint("phase ▸ ")+m.renderer.Plain(p.Phase, laneRailWidth-visualWidth("phase ▸ ")))
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
		id := []rune(m.renderer.Plain(*p.MaybeBlockedByRunID, 64))
		if len(id) > progressBlockedIDWidth {
			id = id[:progressBlockedIDWidth]
		}
		if len(lines) == 0 {
			lines = append(lines, head)
		}
		lines = append(lines, paintSeg(m.pal.wait, nil, false, "⧗ may be blocked by"), "  "+paintSeg(m.pal.wait, nil, false, string(id)))
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n")
}

// progressMilestoneLines draws the percent state's position rows: "milestone k of M" (or
// "N of M done" when no milestone is active), the active milestone title, and the segmented
// bar (plan cell, then one cell per frozen milestone). The bar falls back to the N/M text
// when it would not fit the rail.
func (m tuiModel) progressMilestoneLines(run apitypes.RunDTO, p *apitypes.RunProgress) []string {
	total := p.MilestoneTotal
	if total <= 0 {
		return nil
	}
	faint := func(s string) string { return m.pal.faint.Render(s) }
	var lines []string
	title := ""
	pos := -1
	for i, ms := range run.Milestones {
		if p.ActiveMilestoneID != "" && ms.ID == p.ActiveMilestoneID {
			pos, title = i, ms.Title
			break
		}
	}
	if pos >= 0 {
		lines = append(lines, faint("milestone "+itoa(pos+1)+" of "+itoa(total)))
		if title == "" {
			title = p.ActiveMilestoneID
		}
		lines = append(lines, m.renderer.Plain(title, laneRailWidth))
	} else {
		lines = append(lines, faint(itoa(p.MilestoneDone)+" of "+itoa(total)+" done"))
	}
	// plan cell + "│" + one cell per milestone; the count text is the fallback.
	if len(run.Milestones) > 0 && len(run.Milestones)+2 <= laneRailWidth {
		completed := make(map[string]bool, len(run.MilestonesCompleted))
		for _, id := range run.MilestonesCompleted {
			completed[id] = true
		}
		var sb strings.Builder
		sb.WriteString(paintSeg(m.pal.tungsten, nil, false, "▰") + faint("│"))
		for i, ms := range run.Milestones {
			switch {
			case i == pos:
				sb.WriteString(paintSeg(m.pal.tungsten, nil, true, "▣"))
			case completed[ms.ID]:
				sb.WriteString(paintSeg(m.pal.tungsten, nil, false, "▰"))
			default:
				sb.WriteString(faint("▱"))
			}
		}
		lines = append(lines, sb.String())
	} else {
		lines = append(lines, faint(itoa(p.MilestoneDone)+"/"+itoa(total)))
	}
	return lines
}
