package main

// Crew-rail rendering for the run detail view (PRD #1009 M3): the lane rail, per-lane
// identities/rows, and the milestone-progress fold.

import (
	"image/color"
	"strings"
	"time"

	lipgloss "charm.land/lipgloss/v2"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

func (m tuiModel) renderLaneRail() string {
	d := &m.detail
	now := time.Now()
	active := activeLaneKey(d.run.Status, d.frames)

	var sb strings.Builder
	// A caret advertises the collapse state (`c`) once there is a roster to collapse: ▾ open,
	// ▸ with the lane count when folded to a summary + the selected lane.
	title := m.paneTitle("crew", d.focus == focusRail)
	if len(d.lanes) > 0 {
		if d.railCollapsed {
			title += m.pal.faint.Render("  " + itoa(len(d.lanes)) + " ▸")
		} else {
			title += m.pal.faint.Render("  ▾")
		}
	}
	sb.WriteString(title + "\n")

	if len(d.lanes) == 0 {
		sb.WriteString(m.pal.faint.Render("(no activity yet)"))
		// A milestone-structured run has a frozen list before any frame arrives (queued /
		// just-claimed), so the block must show even with no lanes yet.
		if block := m.renderMilestones(); block != "" {
			sb.WriteString("\n\n" + block)
		}
		if sp := m.renderSpend(strings.Count(sb.String(), "\n") + 1); sp != "" {
			sb.WriteString("\n\n" + sp)
		}
		if rb := m.railRateMeters(now, strings.Count(sb.String(), "\n")+1); rb != "" {
			sb.WriteString("\n\n" + rb)
		}
		return sb.String()
	}

	suffixes := laneSuffixes(d.lanes)
	// The ALL row wears a fixed neutral ◉ (laneRow ignores the state it is handed), so short-circuit
	// it here rather than computing a crew state nothing reads. Every real row is a plain per-key
	// ladder — no rollup.
	st := func(l agentLane) crewState {
		if l.Key == laneAllKey {
			return crewIdle
		}
		return crewStateFor(d.run.Status, d.run.Health, l.Key, active, l.LastActivity, now)
	}
	// The lead's context-window meter (#565): latest-wins across LEAD frames only, memoized in
	// rebuild() (d.leadCtx) so a render does not re-scan+unmarshal lead frames, and shown ONLY on
	// the lead lane. A subagent lane and the synthetic ALL lane never get it.
	fill, hasCtx := d.leadCtx, d.leadCtxOK
	if d.railCollapsed {
		// Just the selected lane, so the reader still knows whose transcript is on screen while
		// the milestones get the rest of the column.
		if l, ok := d.selectedLane(); ok {
			sb.WriteString(m.laneRow(l, true, st(l), suffixes[l.Key], fill, hasCtx && l.Key == laneLead))
		}
	} else {
		for i, l := range d.lanes {
			sb.WriteString(m.laneRow(l, i == d.laneIdx, st(l), suffixes[l.Key], fill, hasCtx && l.Key == laneLead))
		}
	}
	if block := m.renderMilestones(); block != "" {
		sb.WriteString("\n" + block)
	}
	if sp := m.renderSpend(strings.Count(sb.String(), "\n") + 1); sp != "" {
		sb.WriteString("\n\n" + sp)
	}
	if rb := m.railRateMeters(now, strings.Count(sb.String(), "\n")+1); rb != "" {
		sb.WriteString("\n\n" + rb)
	}
	return sb.String()
}

// laneIdentities maps each real lane's key to the identity string the crew rail shows for it,
// so the aggregated transcript can tag every frame with the SAME token its lane row carries:
// the role, plus a ·N ordinal suffix (laneSuffixes) only when the run has two lanes of one
// role. The ALL lane itself is skipped. The role goes through Plain (D7) at the same cap
// laneRow uses, so the tag and the rail row are byte-identical.
func (m tuiModel) laneIdentities() map[string]string {
	suffixes := laneSuffixes(m.detail.lanes)
	out := make(map[string]string, len(m.detail.lanes))
	for _, l := range m.detail.lanes {
		if l.Key == laneAllKey {
			continue
		}
		out[l.Key] = m.renderer.Plain(l.Role, 14) + suffixes[l.Key]
	}
	return out
}

// laneRow renders one crew-rail row: the ▸ cursor, the status dot, the model-authored role and
// an optional ·N disambiguating ordinal, and the optional italic label line beneath it. A
// selected lane rides the warm selection bar (like the board). The role and label are UNTRUSTED
// and go through renderer.Plain (D7); the ordinal suffix is derived, not untrusted. Keeping
// every cell's sanitizing in this one helper is why the collapsed and expanded paths share it.
//
// When showMeter is set (the lead lane, and only when leadContextFill found a valid reading) the
// row carries the inline context-window meter AFTER the role/suffix. The meter is DERIVED, not
// untrusted (no Plain needed), and is built here so it rides this row's own selection bg.
func (m tuiModel) laneRow(l agentLane, selected bool, st crewState, suffix string, fill contextFill, showMeter bool) string {
	var bg color.Color
	if selected {
		bg = m.pal.selBg
	}
	dotC := m.pal.state(st).GetForeground()
	dot := laneDot(st)
	if l.Key == laneAllKey {
		// The aggregated row is a META lane, not an actor, so it wears a neutral tungsten ◉ (a
		// circle-family glyph, distinct from the plain ● state dot) rather than a crew state
		// dot — it never flashes the alarming ▲ a worst-state rollup would put on the whole run.
		dot, dotC = "◉", m.pal.tungsten
	}
	cursor := paintSeg(nil, bg, false, " ")
	var roleFg color.Color
	if selected {
		cursor = paintSeg(m.pal.tungsten, bg, true, "▸")
		// A selected row needs an explicit role foreground or its default-ink title vanishes into
		// the warm selection bar (issue #938, mirroring boardRow's selected floor title): tungsten
		// (ld(#7c5200,#c9a061)) reads on both selBg variants and matches the ▸ cursor and selected
		// id ink. Unselected rows keep nil (the intended quiet default ink).
		roleFg = m.pal.tungsten
	}
	line := cursor + paintSeg(dotC, bg, false, dot) + paintSeg(roleFg, bg, false, " "+m.renderer.Plain(l.Role, 14))
	if suffix != "" {
		// A ·N ordinal disambiguates two lanes of one role (laneSuffixes). It is DERIVED, not
		// the opaque SDK invocation id, so the id tail never reaches the rail — a lone role
		// shows no suffix at all — and there is nothing untrusted here to sanitize.
		line += paintSeg(m.pal.faintC, bg, false, suffix)
	}
	if showMeter {
		// The bar fills whatever the rail has left after the ▸● <role><suffix> prefix,
		// the meter's own leading space, its separator space, and the 4-col percent (=6).
		barW := laneRailWidth - visualWidth(line) - 6
		if barW < 0 {
			barW = 0
		}
		line += m.contextMeterCell(bg, fill, barW)
	}
	var sb strings.Builder
	sb.WriteString(padSeg(line, laneRailWidth, bg) + "\n")
	if l.Label != "" {
		// The machine's own words about its task: italic (degrading to plain under NO_COLOR),
		// faint. Still UNTRUSTED → renderer.Plain.
		lst := lipgloss.NewStyle().Foreground(m.pal.faintC).Italic(true)
		if bg != nil {
			lst = lst.Background(bg)
		}
		label := paintSeg(nil, bg, false, "   ") + lst.Render(m.renderer.Plain(l.Label, laneLabelCap))
		sb.WriteString(padSeg(label, laneRailWidth, bg) + "\n")
	}
	return sb.String()
}

// milestoneTitleCap is the per-row title budget in the crew rail (laneRailWidth 26 minus
// the " ✓ " glyph prefix). joinColumns clamps the whole left column anyway, but Plain must
// cap the UNTRUSTED title itself (D7), so it caps at the width the rail can actually show.
const milestoneTitleCap = 22

// milestoneProgress folds a run's frozen milestone list into (done, total, reported), shared by
// the crew-rail block and the board badge so their COUNTS cannot disagree. `done` counts frozen
// MEMBERS present in the completed set (immune to a duplicate id and to a completed id naming a
// milestone dropped after it was ticked). `reported` is whether ANY completion was ever reported:
// a nil completed slice (JSON null) means never. The crew rail and the board's text fallback use
// it to read `–/N` rather than a `0/N` that looks like failure; the board's own micro-bar instead
// draws an all-empty ▱ bar for an unreported run (see milestoneMarker), trading the never/zero
// distinction for an at-a-glance bar. total is 0 for a run with no frozen list — caller renders
// nothing.
func milestoneProgress(run apitypes.RunDTO) (done, total int, reported bool) {
	total = len(run.Milestones)
	if total == 0 {
		return 0, 0, false
	}
	completed := make(map[string]bool, len(run.MilestonesCompleted))
	for _, id := range run.MilestonesCompleted {
		completed[id] = true
	}
	for _, mi := range run.Milestones {
		if completed[mi.ID] {
			done++
		}
	}
	return done, total, run.MilestonesCompleted != nil
}

// milestoneCount is the compact `2/4` (or `–/4` when nothing was reported) shared by the
// crew-rail block's summary and the board's `M…` badge.
func milestoneCount(done, total int, reported bool) string {
	if !reported {
		return "–/" + itoa(total)
	}
	return itoa(done) + "/" + itoa(total)
}

// renderMilestones is the crew rail's milestone progress block (the TUI twin of the web's
// MilestoneChecklist and the CLI `uzi run get` milestoneRows): a compact `{done}/{total}`
// summary and one glyph-marked row per milestone in FROZEN order — done ✓, in progress ◐,
// not started ○.
//
// Empty for a run with no frozen milestone list, so a pre-#122 (or non-milestone) run's
// rail is byte-for-byte unchanged — the same back-compat contract the nil Milestones slice
// carries elsewhere.
//
// The count says neither "verified" nor "complete" (PRD #122 Decision 6): the worker only
// REPORTS a milestone done and nothing in uzi has verified it, so the bare N/M and the ✓
// glyph must not imply verification. A run that has reported nothing shows `–/N`, not a
// `0/N` that reads as failure (matching the web's `–/N` treatment). `done` counts frozen
// members present in the completed set, immune to a duplicate id and to a completed id
// that names a milestone dropped after it was ticked — the same rule milestoneBadge uses.
//
// Titles are UNTRUSTED repo/agent-authored text (apitypes.Milestone.Title), so each goes
// through renderer.Plain — the D7 obligation the lane rail's Role/Label already carry, and
// why "Title" is in d7UntrustedFields.
func (m tuiModel) renderMilestones() string {
	ms := m.detail.run.Milestones
	if len(ms) == 0 {
		return ""
	}
	completed := make(map[string]bool, len(m.detail.run.MilestonesCompleted))
	for _, id := range m.detail.run.MilestonesCompleted {
		completed[id] = true
	}
	inProgress := make(map[string]bool, len(m.detail.run.MilestonesInProgress))
	for _, id := range m.detail.run.MilestonesInProgress {
		inProgress[id] = true
	}
	done, total, reported := milestoneProgress(m.detail.run)

	var sb strings.Builder
	// The eyebrow gets a milestone micro-bar (▰ done / ▱ remaining) beside the count, the rail
	// twin of the board's micro-bar. Dropped for a very long list, where the per-milestone rows
	// below carry the detail anyway.
	bar := ""
	if reported && total <= boardMileCap {
		bar = lipgloss.NewStyle().Foreground(m.pal.tungsten).Render(strings.Repeat("▰", done)) +
			m.pal.faint.Render(strings.Repeat("▱", total-done)) + " "
	}
	sb.WriteString(m.pal.faint.Render("MILESTONES") + " " + bar + m.pal.faint.Render(milestoneCount(done, total, reported)) + "\n")
	for _, mi := range ms {
		glyph := m.pal.faint.Render("○") // not started
		style := m.pal.faint
		switch {
		case completed[mi.ID]:
			glyph = m.pal.state(crewWorking).Render("✓")
			// done → muted, like the web's text-muted; the ✓ glyph carries completion, so no
			// strikethrough (which lipgloss emits per-rune, bloating the frame for no signal).
			style = m.pal.faint
		case inProgress[mi.ID]:
			glyph = m.pal.state(crewWaiting).Render("◐")
			style = lipgloss.NewStyle() // current — plain terminal fg, like the web's text-fg
		}
		sb.WriteString(" " + glyph + " " + style.Render(m.renderer.Plain(mi.Title, milestoneTitleCap)) + "\n")
	}
	return strings.TrimRight(sb.String(), "\n")
}
