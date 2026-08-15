package factoryui

import (
	"fmt"
	"image/color"
	"strings"

	lipgloss "charm.land/lipgloss/v2"
)

// RenderBoard draws the factory shift board: a status spine + chip per row, a top summary
// that answers "does anything need me" before the eye reaches a row, and AGE + judge marker
// columns the shipped board lacks.
func RenderBoard(p Palette, rows []Run, cursor int, admin, filtering bool, filter string, width int) string {
	var sb strings.Builder

	visible := filterRows(rows, filter)

	needs, stalled := 0, 0
	for _, r := range rows {
		if r.Status == "awaiting_approval" || r.Status == "awaiting_input" {
			needs++
		}
		if r.Health == "stalled" {
			stalled++
		}
	}
	title := "factory floor"
	if admin {
		// Admin honesty label — AdminListRuns returns non-terminal runs only, so it is
		// "active runs (factory-wide)", never "all crews" (matches the shipped board).
		title = "active runs (factory-wide)"
	}
	brand := p.fg(p.brand).Bold(true).Render("▚ uzi") + p.fg(p.muted).Render("  "+title)
	summary := p.fg(p.muted).Render(fmt.Sprintf("%d runs", len(rows)))
	if needs > 0 {
		summary += p.fg(p.muted).Render(" · ") + p.chip(fmt.Sprintf("%d needs you", needs), p.statusColor("awaiting_approval"))
	}
	if stalled > 0 {
		summary += p.fg(p.muted).Render(" · ") + p.fg(p.statusColor("stalled")).Render(fmt.Sprintf("%d stalled", stalled))
	}
	sb.WriteString(padVisual(brand, width-visualWidth(summary)-1) + summary + "\n")
	sb.WriteString(p.fg(p.rule).Render(strings.Repeat("─", width-1)) + "\n")

	// filter line, when active
	if filtering || filter != "" {
		cur := ""
		if filtering {
			cur = p.fg(p.brand).Render("▌")
		}
		sb.WriteString("  " + p.fg(p.muted).Render("/"+filter) + cur + "\n")
	}

	ownerCol := ""
	if admin {
		ownerCol = "  " + padCell("OWNER", 18)
	}
	// 3-space prefix aligns RUN with the data rows' spine(1)+gutter(1)+space(1).
	sb.WriteString("   " + p.eyebrow(padCell("RUN", 9)+ownerCol+"  "+padCell("STATUS", 19)+"  "+padCell("HEALTH", 10)+"  "+padCell("AGE", 5)+"  TITLE") + "\n")

	if len(visible) == 0 {
		sb.WriteString("  " + p.fg(p.muted).Render("No runs yet. Start one from the web board or the command line.") + "\n")
	}

	for i, r := range visible {
		sel := i == cursor
		sc := p.rowColor(r.Status, r.Health)

		// The spine carries the status glyph on the status-colour bg; the glyph is the
		// NO_COLOR fallback (survives when lipgloss strips the fill). Spine stays status-
		// coloured on the selected row so its status is never hidden.
		spine := lipgloss.NewStyle().Background(sc).Foreground(p.chipFg).Render(statusGlyph(r.Status))
		gutter := " "
		if sel {
			gutter = p.fg(p.brand).Bold(true).Render("▸")
		}

		idStyle := p.fg(p.muted)
		titleStyle := p.fg(p.muted)
		if sel || (r.Status != "completed" && r.Status != "cancelled" && r.Status != "failed") {
			titleStyle = p.fg(p.ink)
		}
		if sel {
			idStyle = p.fg(p.ink).Bold(true)
		}

		statusCell := padVisual(p.chip(r.Status, sc), 19)
		// HEALTH column: stalled keeps its ▲; other non-ok health shows its WORD (as the
		// shipped board does), in the default/ink colour; ok is blank.
		health := strings.Repeat(" ", 10)
		switch r.Health {
		case "stalled":
			health = padVisual(p.fg(p.statusColor("stalled")).Render("▲"), 10)
		case "":
		default:
			health = padVisual(p.fg(p.ink).Render(capCell(r.Health, 10)), 10)
		}
		owner := ""
		if admin {
			owner = "  " + p.fg(p.muted).Render(padCell(r.Owner, 18))
		}

		// F4: judge marker with the todo count when > 0 (⚖ issues · N).
		marker := ""
		if r.Verdict != "" {
			vc := p.muted
			switch r.Verdict {
			case "ideal", "ok":
				vc = p.statusColor("completed")
			case "issues":
				vc = p.statusColor("failed")
			}
			label := "⚖ " + r.Verdict
			if r.Todo > 0 {
				label += fmt.Sprintf(" · %d", r.Todo)
			}
			marker = "  " + p.fg(vc).Render(label)
		}
		// F2: cap the title to what remains so no row exceeds the rule width at ~100 cols.
		prefix := 3 + 9 + 2 + 19 + 2 + 10 + 2 + 5 + 2
		if admin {
			prefix += 2 + 18
		}
		avail := (width - 1) - prefix - visualWidth(marker)
		if avail < 10 {
			avail = 10
		}
		title := titleStyle.Render(capCell(r.Title, avail)) + marker

		line := spine + gutter + " " + idStyle.Render(padCell(r.ID, 9)) + owner + "  " +
			statusCell + "  " + health + "  " + p.fg(p.faint).Render(padCell(r.Age, 5)) + "  " + title
		sb.WriteString(line + "\n")
	}

	sb.WriteString(p.fg(p.rule).Render(strings.Repeat("─", width-1)) + "\n")
	sb.WriteString(p.fg(p.faint).Render("enter open · / filter · a admin · r refresh · ? keys · q quit"))
	return sb.String()
}

// statusGlyph is the per-status spine marker, the NO_COLOR fallback — single-cell, reads
// without colour. Mirrors the shipped board's statusGlyph.
func statusGlyph(status string) string {
	switch status {
	case "running":
		return "●"
	case "awaiting_approval":
		return "!"
	case "awaiting_input":
		return "?"
	case "limit_wait":
		return "~"
	case "completed":
		return "✓"
	case "failed":
		return "✗"
	default:
		return "·"
	}
}

// filterRows applies the board's `/` filter over the fields a human searches by. Shared by
// RenderBoard and the model so the cursor and the drawn list can never disagree.
func filterRows(rows []Run, filter string) []Run {
	f := strings.TrimSpace(filter)
	if f == "" {
		return rows
	}
	out := make([]Run, 0, len(rows))
	for _, r := range rows {
		hay := strings.ToLower(r.ID + " " + r.Kind + " " + r.Status + " " + r.Title)
		if strings.Contains(hay, strings.ToLower(f)) {
			out = append(out, r)
		}
	}
	return out
}

func laneDot(p Palette, state string) string {
	switch state {
	case "working":
		return p.fg(p.statusColor("running")).Render("●")
	case "waiting":
		return p.fg(p.statusColor("limit_wait")).Render("◑")
	case "stalled":
		return p.fg(p.statusColor("stalled")).Render("▲")
	case "done":
		return p.fg(p.muted).Render("✓")
	default:
		return p.fg(p.faint).Render("○")
	}
}

// Focus targets for the detail view's two panes. left/right (and tab) move focus between
// them; up/down act WITHIN the focused pane (move between agents, or scroll the transcript).
const (
	FocusRail = iota
	FocusTranscript
)

// isLive reports whether a run is actively producing output, so follow-live applies.
func isLive(status string) bool { return status == "running" || status == "claimed" }

// detailViewport is the transcript's visible line budget for a given terminal height.
func detailViewport(height int) int {
	if h := height - 11; h > 6 {
		return h
	}
	return 6
}

// TranscriptExtent gives the model the total transcript line count and the viewport height,
// so it can clamp the scroll offset without duplicating the layout.
func TranscriptExtent(p Palette, r Run, selLane, height int) (total, viewport int) {
	return len(buildTranscript(p, r, selLane)), detailViewport(height)
}

// RenderDetail draws one run: context bar, then two FOCUSABLE panes — the crew rail and the
// transcript — side by side, with the focused pane titled bright and the other dimmed. When
// the run needs a human it adds the amber PLAN GATE banner with promoted action keys. The
// transcript follows live (auto-tails) unless the reader has scrolled back.
func RenderDetail(p Palette, r Run, focus, selLane, scroll int, following bool, width, height int) string {
	sc := p.rowColor(r.Status, r.Health)
	needsHuman := r.Status == "awaiting_approval" || r.Status == "awaiting_input"

	var sb strings.Builder
	ctx := p.fg(p.muted).Render("run ") + p.fg(p.ink).Bold(true).Render(r.ID) + "  " +
		p.chip(r.Kind, p.brand) + "  " + p.chip(r.Status, sc) + "  " + p.fg(p.ink).Render(capCell(r.Title, width-40))
	sb.WriteString(ctx + "\n")

	transport := p.fg(p.statusColor("running")).Render("● live")
	if r.ParkLine != "" {
		transport = p.fg(sc).Render("⏸ " + r.ParkLine)
	}
	sb.WriteString(transport + "\n")
	sb.WriteString(p.fg(p.rule).Render(strings.Repeat("─", width-1)) + "\n")

	vp := detailViewport(height)
	colW := width - laneRailWidth - 4

	// Left pane: title + crew rail.
	railTitle := paneTitle(p, "crew", focus == FocusRail, "")
	rail := railTitle + "\n" + renderRail(p, r.Lanes, selLane, focus == FocusRail)

	// Right pane: title (with the follow badge) + the windowed transcript.
	badge := followBadge(p, r.Status, following, scroll)
	transTitle := paneTitleRight(p, "transcript", focus == FocusTranscript, badge, colW)
	trans := transTitle + "\n" + windowTranscript(p, r, selLane, scroll, following, vp)

	sb.WriteString(joinColumns(p, rail, trans, laneRailWidth, vp+1) + "\n")

	// The keymap is ONE compact line (bracketless, middot-separated) so it never wraps at a
	// normal width. At a plan gate the banner carries the context, so the footer leads with
	// approve/reject and drops review (a judge review only exists after a run finishes).
	if needsHuman {
		banner := lipgloss.NewStyle().Background(sc).Foreground(p.chipFg).Bold(true).
			Width(width-1).Padding(0, 1).Render("⚑  PLAN GATE · this run is waiting on your approval")
		sb.WriteString(banner + "\n")
		sb.WriteString(p.hintbar(p.hint("y", "approve"), p.hint("n", "reject"), p.hint("f", "follow-up"),
			p.hint("x", "cancel"), p.hint("←→", "pane"), p.hint("↑↓", "move"), p.hint("esc", "back"), p.hint("?", "keys")))
	} else {
		sb.WriteString(p.hintbar(p.hint("←→", "pane"), p.hint("↑↓", "move"), p.hint("g", "live"), p.hint("f", "follow-up"),
			p.hint("v", "review"), p.hint("x", "cancel"), p.hint("esc", "back"), p.hint("?", "keys")))
	}
	return sb.String()
}

// paneTitle renders a pane's title with a focus indicator: a bright brand bar + bold title
// when focused, a dim title otherwise.
func paneTitle(p Palette, title string, focused bool, _ string) string {
	if focused {
		return p.fg(p.brand).Render("▎") + p.fg(p.brand).Bold(true).Render(strings.ToUpper(title))
	}
	return " " + p.fg(p.faint).Render(strings.ToUpper(title))
}

// paneTitleRight is paneTitle with a right-aligned badge (the follow indicator).
func paneTitleRight(p Palette, title string, focused bool, badge string, colW int) string {
	left := paneTitle(p, title, focused, "")
	if badge == "" {
		return left
	}
	return padVisual(left, colW-visualWidth(badge)) + badge
}

// followBadge is the live-follow indicator, shown only for a live run. FOLLOWING (green) when
// auto-tailing; PAUSED (amber) with a "new below" count when the reader has scrolled back.
func followBadge(p Palette, status string, following bool, scroll int) string {
	if !isLive(status) {
		return ""
	}
	if following {
		return p.fg(p.statusColor("running")).Bold(true).Render("● FOLLOWING")
	}
	badge := p.fg(p.statusColor("awaiting_approval")).Bold(true).Render("⏸ PAUSED")
	if scroll > 0 {
		badge += p.fg(p.faint).Render(fmt.Sprintf(" ↓%d new", scroll))
	}
	return badge
}

func renderRail(p Palette, lanes []Lane, sel int, focused bool) string {
	var sb strings.Builder
	for i, l := range lanes {
		selected := i == sel
		marker := " "
		nameStyle := p.fg(p.ink)
		if selected {
			// The selected lane's marker is bright only when the rail is focused; dimmed
			// when focus is elsewhere, so "which pane am I driving" stays unambiguous.
			bar := p.brand
			if !focused {
				bar = p.faint
			}
			marker = p.fg(bar).Render("▊")
			nameStyle = p.fg(p.ink).Bold(true)
		}
		row := marker + laneDot(p, l.State) + " " + nameStyle.Render(l.Role)
		if l.Inst != "" {
			row += p.fg(p.faint).Render("·" + l.Inst)
		}
		sb.WriteString(row + "\n")
		if l.Label != "" {
			sb.WriteString("   " + p.fg(p.muted).Render(l.Label) + "\n")
		}
	}
	return strings.TrimRight(sb.String(), "\n")
}

// buildTranscript returns the selected lane's transcript as styled display lines (no
// windowing). The lane filter mirrors the shipped TUI: one lane's frames at a time.
func buildTranscript(p Palette, r Run, selLane int) []string {
	role := ""
	if selLane >= 0 && selLane < len(r.Lanes) {
		role = r.Lanes[selLane].Role
	}
	var sb strings.Builder
	seq := 0
	for _, f := range r.Transcript {
		seq++
		if role != "" && f.Role != role {
			continue
		}
		head := p.fg(p.brand).Bold(true).Render(f.Role) + p.fg(p.muted).Render(" · "+f.Kind)
		seqTag := p.fg(p.faint).Render(fmt.Sprintf("#%d", seq))
		colW := width0()
		sb.WriteString(padVisual(head, colW-visualWidth(seqTag)) + seqTag + "\n")
		for _, ln := range strings.Split(f.Text, "\n") {
			sb.WriteString("  " + p.fg(p.ink).Render(ln) + "\n")
		}
		sb.WriteString("\n")
	}
	lines := strings.Split(strings.TrimRight(sb.String(), "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return []string{p.fg(p.faint).Render("(no activity in this lane yet)")}
	}
	return lines
}

// width0 is the transcript header padding width; kept modest so headers don't over-pad on
// very wide terminals. It only affects the seq tag's right-alignment.
func width0() int { return 40 }

// windowTranscript applies the follow/scroll window over the built lines. Following shows the
// last vp lines (auto-tail); paused shows a window offset `scroll` lines up from the bottom.
func windowTranscript(p Palette, r Run, selLane, scroll int, following bool, vp int) string {
	lines := buildTranscript(p, r, selLane)
	if len(lines) <= vp {
		return strings.Join(lines, "\n")
	}
	maxScroll := len(lines) - vp
	if following || scroll < 0 {
		scroll = 0
	}
	if scroll > maxScroll {
		scroll = maxScroll
	}
	start := len(lines) - vp - scroll
	return strings.Join(lines[start:start+vp], "\n")
}

func joinColumns(p Palette, left, right string, railW, minRows int) string {
	l := strings.Split(left, "\n")
	rr := strings.Split(right, "\n")
	n := len(l)
	if len(rr) > n {
		n = len(rr)
	}
	if n < minRows {
		n = minRows
	}
	bar := p.fg(p.rule).Render("│")
	var sb strings.Builder
	for i := 0; i < n; i++ {
		var lv, rv string
		if i < len(l) {
			lv = l[i]
		}
		if i < len(rr) {
			rv = rr[i]
		}
		sb.WriteString(padVisual(lv, railW) + " " + bar + " " + rv + "\n")
	}
	return strings.TrimRight(sb.String(), "\n")
}

// RenderReview draws the judge review overlay with a SEVERITY-coloured verdict chip and
// confidence dots, instead of the shipped all-blue treatment.
func RenderReview(p Palette, r Run, rv DemoReview, cursor, width int) string {
	var sb strings.Builder
	ctx := p.fg(p.muted).Render("run ") + p.fg(p.ink).Bold(true).Render(r.ID) + "  " +
		p.chip(r.Status, p.statusColor(r.Status)) + "  " + p.fg(p.ink).Render(capCell(r.Title, width-30))
	sb.WriteString(ctx + "\n")
	sb.WriteString(p.eyebrow("judge review") + "\n")
	sb.WriteString(p.fg(p.rule).Render(strings.Repeat("─", width-1)) + "\n")

	verdict := p.chip(rv.Verdict, p.statusColor("failed"))
	if rv.Verdict == "ideal" || rv.Verdict == "ok" {
		verdict = p.chip(rv.Verdict, p.statusColor("completed"))
	}
	triage := p.fg(p.muted).Render(fmt.Sprintf("%d findings · ", rv.Total)) +
		p.fg(p.statusColor("awaiting_approval")).Render(fmt.Sprintf("%d to do", rv.Todo)) +
		p.fg(p.muted).Render(" · ") + p.fg(p.statusColor("completed")).Render(fmt.Sprintf("%d done", rv.Done))
	sb.WriteString(verdict + "   " + triage + "\n\n")

	box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(p.rule).Padding(0, 1).Width(width - 6).
		Render(p.fg(p.faint).Render("summary · model-authored") + "\n" + p.fg(p.ink).Render(rv.Summary))
	sb.WriteString(box + "\n\n")

	confColor := map[string]color.Color{"high": p.statusColor("failed"), "medium": p.statusColor("awaiting_approval"), "low": p.muted}
	for i, rec := range rv.Recommendations {
		marker := "  "
		catStyle := p.fg(p.ink)
		if i == cursor {
			marker = p.fg(p.brand).Bold(true).Render("▸ ")
			catStyle = p.fg(p.ink).Bold(true)
		}
		conf := lipgloss.NewStyle().Foreground(confColor[rec.Confidence]).Render("●") + p.fg(p.muted).Render(" "+rec.Confidence)
		head := marker + catStyle.Render(rec.Category) + p.fg(p.muted).Render(" · "+rec.Target) + "  " + conf + p.fg(p.faint).Render("  "+rec.ID)
		if rec.Disposition == "done" {
			head += "  " + p.chip("done", p.statusColor("completed"))
		}
		sb.WriteString(head + "\n")
	}

	sb.WriteString("\n" + p.fg(p.rule).Render(strings.Repeat("─", width-1)) + "\n")
	sb.WriteString(p.hintbar(p.hint("j/k", "move"), p.hint("r", "resolve"), p.hint("d", "dismiss"),
		p.hint("u", "undo"), p.hint("esc", "close")))
	return sb.String()
}
