package main

import (
	"strings"

	"github.com/vtmocanu/uzi/api/internal/runprogress"
)

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
		// MILESTONES draws position, title and bar just below, so this is the compact two-row form
		// that keeps the crew roster unfolded on the standard running scene.
		top := head + " " + paintSeg(m.pal.tungsten, nil, true, "≈"+itoa(min(max(*p.Pct, 0), 100))+"%")
		if p.MilestoneTotal > 0 {
			top += faint(" · " + itoa(p.MilestoneDone) + "/" + itoa(p.MilestoneTotal))
		}
		lines = append(lines, top)
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
