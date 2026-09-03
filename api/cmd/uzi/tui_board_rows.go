package main

// Board row rendering (PRD #1009 M3): the rate-limit strip and its meters, empty/row
// rendering with the credential/cost/milestone cells, and the judge/milestone markers.

import (
	"fmt"
	"image/color"
	"slices"
	"strings"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// rateBarWidth is the mini rate-limit meter's fixed cell width.
const rateBarWidth = 6

// rateDangerPct / rateWarnPct are the shared tone-band cutoffs (mirror the web toneFor): a
// rounded pct ≥ rateDangerPct is danger, ≥ rateWarnPct is warn, else ok. The per-group accent
// bar reddens on a token's peak window pct ≥ rateDangerPct.
const (
	rateDangerPct = 85
	rateWarnPct   = 40
)

// tokenPeakPct is the max .Pct across a token's non-nil windows; a nil window contributes
// nothing, and a token with no window at all returns the sentinel -1 (stays faint).
func tokenPeakPct(t apitypes.TokenRateLimitDTO) int {
	peak := -1
	for _, w := range []*apitypes.RateLimitWindow{t.Limits.FiveHour, t.Limits.SevenDay} {
		if w != nil && w.Pct > peak {
			peak = w.Pct
		}
	}
	return peak
}

// selectedRateMeters applies the #519 board/sidebar selection to the model's own
// per-token meters, shared by the board strip and the detail rail so the two surfaces
// cannot disagree on which accounts show OR on showLabel. readable = Limits.Status "ok";
// showLabel is keyed off the READABLE count (>1), not the shown count, so a readable-but-
// unlisted token still forces per-token labels; shown = readable filtered by
// (IsDefault || SecretID in sidebar_token_ids). Empty selection => (nil, false).
func (m tuiModel) selectedRateMeters() (shown []apitypes.TokenRateLimitDTO, showLabel bool) {
	readable := make([]apitypes.TokenRateLimitDTO, 0, len(m.rateLimits))
	for _, t := range m.rateLimits {
		if t.Limits.Status == "ok" {
			readable = append(readable, t)
		}
	}
	if len(readable) == 0 {
		return nil, false
	}
	showLabel = len(readable) > 1
	shown = make([]apitypes.TokenRateLimitDTO, 0, len(readable))
	for _, t := range readable {
		if t.IsDefault || slices.Contains(m.sidebarTokenIds, t.SecretID) {
			shown = append(shown, t)
		}
	}
	return shown, showLabel
}

// boardRateLimitStrip is the single-line rate-limit meter strip drawn under the wordmark,
// mirroring the web left-bottom sidebar's token SELECTION (SidebarRateLimits +
// sidebarTokens.ts):
//   - readable = tokens whose Limits.Status == "ok"; nothing readable → no strip.
//   - showLabel is keyed off readable (len > 1), NOT off the shown subset.
//   - shown = readable filtered by isShownInSidebar (IsDefault || SecretID ∈ sidebarTokenIds);
//     nothing shown → no strip.
//
// The SELECTION is mirrored, not the web's rendering: when readable > 0 but shown == 0 the web
// still draws an empty aria container plus a "+N more in Settings" deep-link, whereas this strip
// returns "" — there is no TUI analog for that Settings affordance. That is a render difference,
// not a selection one.
//
// Each shown token renders its 5h and 7d windows as a faint label + tone-coloured mini bar +
// NN% text. The NN% text is always present so an Ascii/NO_COLOR terminal (which strips the SGR
// tone) keeps the legible signal — colour is never the only cue. Clamped to one physical line.
// The Label is USER-AUTHORED and drawn through renderer.Plain (D7).
func (m tuiModel) boardRateLimitStrip(now time.Time) string {
	shown, showLabel := m.selectedRateMeters()
	if len(shown) == 0 {
		return ""
	}
	segs := make([]string, 0, len(shown))
	for _, t := range shown {
		seg := ""
		if showLabel {
			seg = paintSeg(m.pal.faintC, nil, false, m.renderer.Plain(t.Label, 16)+" ")
		}
		seg += m.rateWindowCell("5h", t.Limits.FiveHour, rateBarWidth, 0, now) + "   " + m.rateWindowCell("7d", t.Limits.SevenDay, rateBarWidth, 0, now)
		// Prefix a per-group accent bar TIGHT against the label: it both delimits the group and
		// doubles as a status light — alarm when the token's peak window pct ≥ rateDangerPct,
		// faint otherwise. Emitted unconditionally via paintSeg so the group DELIMITER survives
		// colour stripping (NO_COLOR/Ascii); the tint is the danger cue and is stripped there, so
		// danger legibility falls back on the always-present bar-fill + NN% text, not this glyph.
		accent := m.pal.faintC
		if tokenPeakPct(t) >= rateDangerPct {
			accent = m.pal.alarm
		}
		seg = paintSeg(accent, nil, false, "▎") + seg
		segs = append(segs, seg)
	}
	// A faint leading space aligns the strip under the brand block (the brand line starts " ").
	// Tokens are joined with three spaces; the per-group accent bar ▎ (prefixed above) is the
	// group delimiter, so each token's two windows still read as a group; the intra-token 5h↔7d
	// gap stays 3 spaces.
	strip := " " + strings.Join(segs, "   ")
	return clampVisual(strip, m.width)
}

// rateWindowCell renders one rate-limit window as `label <bar> NN%`: a faint label ("5h"/"7d"),
// a tone-coloured mini block-bar filled proportional to pct, then the server-rounded NN% text.
// A nil window (Anthropic reported none) draws a faint `label -`, mirroring windowPct's "-".
func (m tuiModel) rateWindowCell(label string, w *apitypes.RateLimitWindow, barW, pctW int, now time.Time) string {
	if w == nil {
		return paintSeg(m.pal.faintC, nil, false, label+" -")
	}
	filled, empty := rateBarParts(w.Pct, barW)
	// pctW == 0 emits the board strip's byte-identical " NN%"; pctW > 0 right-aligns the
	// percent to pctW cols so the full-rail account meters land flush at the rail edge.
	pctSeg := " " + windowPct(w) + "%"
	if pctW > 0 {
		pctSeg = " " + fmt.Sprintf("%*s", pctW, windowPct(w)+"%")
	}
	// Inline reset countdown after the percent, when Anthropic reported a reset time for this
	// window (issue #588). shortDuration clamps a past reset to "0s". The strip is width-clamped
	// downstream and the rail row is width-locked to laneRailWidth (26), which is why the two
	// surfaces space the countdown differently: the board appends a single space + bare duration,
	// the rail a 2-col gap + a 6-col right-aligned field (max real value "23h59m" = 6 cols).
	if w.ResetsAt != nil {
		reset := shortDuration(time.Duration(*w.ResetsAt-now.Unix()) * time.Second)
		if pctW > 0 {
			pctSeg += "  " + fmt.Sprintf("%6s", reset)
		} else {
			pctSeg += " " + reset
		}
	}
	return paintSeg(m.pal.faintC, nil, false, label+" ") +
		paintSeg(m.rateTone(w.Pct), nil, false, filled) +
		paintSeg(m.pal.faintC, nil, false, empty) +
		paintSeg(m.pal.faintC, nil, false, pctSeg)
}

// rateTone maps a rounded pct to the shared tone (mirrors the web toneFor): danger ≥ 85,
// warn ≥ 40, else ok.
func (m tuiModel) rateTone(pct int) color.Color {
	switch {
	case pct >= rateDangerPct:
		return m.pal.alarm
	case pct >= rateWarnPct:
		return m.pal.amber
	default:
		return m.pal.sage
	}
}

// rateBarParts splits a width-cell mini bar into its filled (▰) and empty (▱) runs,
// filled proportional to pct (the same ▰/▱ glyph vocabulary the milestone micro-bar uses).
func rateBarParts(pct, width int) (filled, empty string) {
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	f := (pct*width + 50) / 100
	if f > width {
		f = width
	}
	return strings.Repeat("▰", f), strings.Repeat("▱", width-f)
}

// boardEmptyState replaces the old dead-end "no runs to show": it keeps the footer (added
// by the caller) and, on the own board, guides the user to start a run. The guidance
// deliberately does NOT name a `uzi <subcommand>` — a printed command reference in a view
// would need a knownInstructions entry (TestPrintedInstructionsAreRegistered) it cannot
// earn, since a lipgloss Render site classifies as neither an emitter nor a cobra field.
func (m tuiModel) boardEmptyState() string {
	// Distinguish an empty board from one the [h] toggle has emptied by hiding finished runs,
	// so the view does not read as "no runs" when it is really "no active runs".
	if m.board.hideDone && strings.TrimSpace(m.board.filter) == "" {
		for _, r := range m.board.runs {
			if terminalRunStatuses[r.Status] {
				return m.pal.faint.Render(" No active runs — finished runs are folded (h to show).") + "\n"
			}
		}
	}
	if m.board.admin {
		return m.pal.faint.Render(" No active runs across the factory right now.") + "\n"
	}
	return m.pal.faint.Render(" No runs yet. Start one from the web board or the command line.") + "\n"
}

// boardRow renders one run: the per-row ▌ andon strip and state glyph (state colour), the id,
// the owner on the admin board, the status WORD (state colour, not a filled chip), AGE, the
// milestone micro-bar, and the title, with an own-board-only judge marker flushed right. A
// selected row rides the full-width warm selection bar with a ▸ cursor; a DONE-band row is
// faint end to end.
func (m tuiModel) boardRow(r apitypes.RunListItemDTO, sel bool, mc boardMarkerCols) string {
	band := runBand(r.Status, r.IsRevising)
	terminal := band == bandDone
	tok := m.pal.stateToken(r.Status, r.Health, r.IsPlanning, r.IsRevising)
	showCred := m.boardShowCred()

	var bg color.Color
	if sel {
		bg = m.pal.selBg
	}

	// A DONE row drops to faint even for a failed run — the ✗ glyph plus faint says "finished,
	// and it failed" without lighting an alarm the board can no longer act on (the detail
	// header keeps the red). The judge marker keeps its own severity colour regardless.
	stateC := tok.color
	if terminal {
		stateC = m.pal.faintC
	}
	// Title colour by band: NEEDS YOU rows are amber (the one lit thing), the floor is default
	// ink, DONE is faint.
	var titleC color.Color
	switch band {
	case bandNeedsYou:
		titleC = m.pal.amber
	case bandDone:
		titleC = m.pal.faintC
	default: // floor band
		// Unselected floor rows keep nil (default ink, the intended quiet look). A SELECTED
		// floor row needs an explicit foreground or its default-fg title vanishes into the
		// warm selection bar on a light terminal (dark-on-dark). tungsten (ld(#7c5200,#c9a061))
		// resolves dark-on-light-selBg and light-on-dark-selBg, matching the selected id ink.
		if sel {
			titleC = m.pal.tungsten
		}
	}
	idC := m.pal.faintC
	if sel {
		idC = m.pal.tungsten
	}

	cursor := paintSeg(nil, bg, false, " ")
	if sel {
		cursor = paintSeg(m.pal.tungsten, bg, true, "▸")
	}
	gap := paintSeg(nil, bg, false, "  ")
	row := cursor +
		paintSeg(stateC, bg, false, "▌") +
		paintSeg(stateC, bg, false, tok.glyph) +
		paintSeg(nil, bg, false, " ") +
		paintSeg(idC, bg, sel, padCell(shortRunID(r.ID), boardIDWidth)) +
		gap

	if m.board.admin {
		// D7 (B1): OwnerEmail is forge-authored untrusted text — route it through renderer.Plain
		// (= capCell(cellText(...))); a bare capCell would not strip control bytes. OwnerEmail is
		// in d7UntrustedFields.
		row += paintSeg(m.pal.faintC, bg, false, padCell(m.renderer.Plain(strOr(r.OwnerEmail, ""), boardOwnerWidth), boardOwnerWidth)) + gap
	}

	row += paintSeg(stateC, bg, false, padCell(tok.word, boardStatusWordWidth)) + gap +
		paintSeg(m.pal.faintC, bg, false, padCell(relAge(r.CreatedAt), boardAgeWidth)) + gap

	if m.boardShowMile() {
		row += padSeg(m.milestoneMarker(r, terminal, bg), boardMileWidth, bg) + gap
	}

	// WHICH Anthropic credential this run spent (PRD #111 / #295), gated by boardShowCred:
	// drawn after the milestone micro-bar and before the COST column, so the row reads
	// account → cost → title.
	if showCred {
		row += m.boardCredSeg(r, bg) + gap
	}

	// WHAT this run cost (PRD #650), gated by boardShowCost: a right-aligned whole-dollar cell,
	// own board only. Drawn after the credential column and before the title.
	if m.boardShowCost() {
		row += m.boardCostSeg(r, bg) + gap
	}

	// Title: capped to the remaining width AND to boardTitleMax. The marker column allowance is
	// reserved for every row so the title keeps one width down the board; markers then align by
	// flushing the whole row to width-fullW below.
	markerAllow := 0
	if mc.fullW > 0 {
		markerAllow = mc.fullW + 2
	}
	avail := m.width - boardRowPrefixWidth(m.board.admin, m.boardShowMile(), showCred, m.boardShowCost()) - markerAllow
	if avail < 10 {
		avail = 10
	}
	if avail > boardTitleMax {
		avail = boardTitleMax
	}
	// Clickable #<iid> immediately before the title (issue-less runs render nothing).
	// Its visual width comes off the title budget; the OSC-8 escape is zero-width to
	// lipgloss so padding stays correct. Variable-width, so it is deliberately NOT in
	// boardRowPrefixWidth's fixed column set.
	titlePrefix := ""
	if r.IssueIID != nil {
		styledIID := paintSeg(idC, bg, sel, "#"+itoa(int(*r.IssueIID)))
		titlePrefix = m.issueLink(r.RunDTO, styledIID) + paintSeg(nil, bg, false, " ")
		avail -= visualWidth(titlePrefix)
		if avail < 10 {
			avail = 10
		}
	}
	row += titlePrefix + paintSeg(titleC, bg, false, clampVisual(m.renderer.Plain(runTitle(r.RunDTO), avail), avail))

	// Judge marker (own board only; AdminListRuns carries no JudgeVerdict), flushed to the right
	// edge so the ⚖ icon and count align down the board.
	if mc.fullW > 0 && !m.board.admin && r.JudgeVerdict != nil {
		row = padSeg(row, m.width-mc.fullW, bg) + m.verdictMarker(*r.JudgeVerdict, r.JudgeTodoCount, mc.verdictW, mc.countW, bg)
	} else if sel {
		// Keep the warm selection bar spanning the full width even with no trailing marker.
		row = padSeg(row, m.width, bg)
	}
	return row
}

// boardCredSeg renders WHICH Anthropic credential a run spent (PRD #111 / #295): just the token
// LABEL, drawn muted (faint) — deliberately no dot and no select-reason colour, so the column is
// a quiet "which account" scan rather than an attention signal (the reason/mode lives on the run
// detail and in `uzi run <id>`). An empty cell is drawn when no credential was recorded (a run
// claimed before PRD #111 M1, or one not yet claimed) — a guessed placeholder would assert
// something nothing knows. The label is USER-AUTHORED and drawn through renderer.Plain (D7);
// AnthropicSecretLabel is in d7UntrustedFields.
func (m tuiModel) boardCredSeg(r apitypes.RunListItemDTO, bg color.Color) string {
	if r.AnthropicSecretLabel == nil || *r.AnthropicSecretLabel == "" {
		return padSeg("", boardCredWidth, bg)
	}
	label := m.renderer.Plain(strOr(r.AnthropicSecretLabel, ""), boardCredWidth)
	return paintSeg(m.pal.faintC, bg, false, padCell(label, boardCredWidth))
}

// boardCostSeg renders WHAT a run cost (PRD #650), right-aligned in a fixed cell, whole dollars
// only (no decimals, to keep the column width stable). Three distinct states: a nil Usage (a
// pre-#40 or unclaimed run) draws a BLANK cell, matching boardCredSeg's empty convention; a $0
// cost is a subscription-auth run the SDK prices at $0 and draws "—" (never "$0"); a real cost
// draws fmtCostWhole (which itself renders 0<usd<0.5 as "<$1" so a real cost never shows as $0).
func (m tuiModel) boardCostSeg(r apitypes.RunListItemDTO, bg color.Color) string {
	if r.Usage == nil {
		return padSeg("", boardCostWidth, bg)
	}
	s := "—"
	if r.Usage.CostUSD != 0 {
		s = fmtCostBoard(r.Usage.CostUSD) // width-capped so a pathological cost can't blow the cell
	}
	if pad := boardCostWidth - visualWidth(s); pad > 0 {
		s = strings.Repeat(" ", pad) + s // right-align within the cell
	}
	return paintSeg(m.pal.faintC, bg, false, s)
}

// boardRowPrefixWidth is the visual width of every column before TITLE, so the title can be
// sized to what remains. cursor(1)+strip(1)+glyph(1)+space(1)+id, then two-space gaps around
// the status-word and AGE cells, the micro-bar cell when shown, the credential cell when
// shown, the cost cell when shown, plus the admin owner cell.
func boardRowPrefixWidth(admin, mile, cred, cost bool) int {
	w := 4 + boardIDWidth + 2 + boardStatusWordWidth + 2 + boardAgeWidth + 2
	if mile {
		w += boardMileWidth + 2
	}
	if cred {
		w += boardCredWidth + 2
	}
	if cost {
		w += boardCostWidth + 2
	}
	if admin {
		w += 2 + boardOwnerWidth
	}
	return w
}

// boardMarkerCols are the fixed sub-column widths of the judge marker across the visible rows:
// verdictW for the variable-length verdict WORD (ideal/ok/issues) and countW for the "· N"
// recommendation count. fullW is the whole marker's width. Splitting the marker into fixed
// sub-columns is what keeps the ⚖ icon and the count in stable columns while only the verdict
// word moves within its own slot. All zero on the admin board (no verdicts) or when no visible
// row carries a verdict.
type boardMarkerCols struct{ verdictW, countW, fullW int }

func (m tuiModel) boardMarkerCols(rows []apitypes.RunListItemDTO) boardMarkerCols {
	var mc boardMarkerCols
	if m.board.admin {
		return mc
	}
	for _, r := range rows {
		if r.JudgeVerdict == nil {
			continue
		}
		if w := visualWidth(m.renderer.Plain(*r.JudgeVerdict, 8)); w > mc.verdictW {
			mc.verdictW = w
		}
		if r.JudgeTodoCount > 0 {
			if w := visualWidth("· " + itoa(r.JudgeTodoCount)); w > mc.countW {
				mc.countW = w
			}
		}
	}
	if mc.verdictW > 0 {
		// The empty-verdict/zero-count marker has the full fixed width, and visualWidth accounts
		// for the ⚖ glyph's own cell count.
		mc.fullW = visualWidth(m.verdictMarker("", 0, mc.verdictW, mc.countW, nil))
	}
	return mc
}

// verdictMarker is the own-board ⚖ judge badge, coloured by severity (issues → alarm red,
// everything else → faint) via the shared verdictColor. It lays the verdict WORD out in a fixed
// verdictW slot and the "· N" count in a fixed countW slot after it, so down the board the ⚖
// icon and the count align and only the verdict word shifts. The verdict is a closed enum but
// rendered through Plain so an unrecognised value from a newer server cannot inject control
// bytes. bg carries the selection bar behind the marker on a selected row.
func (m tuiModel) verdictMarker(verdict string, todo, verdictW, countW int, bg color.Color) string {
	label := "⚖ " + padVisual(m.renderer.Plain(verdict, 8), verdictW)
	if countW > 0 {
		count := ""
		if todo > 0 {
			count = "· " + itoa(todo)
		}
		label += " " + padVisual(count, countW)
	}
	return paintSeg(m.pal.verdictColor(verdict), bg, false, label)
}

// milestoneMarker is the board's compact milestone micro-bar — one cell per frozen milestone,
// done ▰ (tungsten, or faint on a DONE row) ahead of remaining ▱ (faint). A run that has reported
// nothing yet has done=0, so it draws an all-empty ▱ bar (the graphical `0/N`) rather than the
// web MilestoneBadge's `–/N` text — the board favours the at-a-glance bar over the never/zero
// distinction the wider detail rail keeps. When the list is longer than boardMileCap the bar
// would overflow the column, so it falls back to text: `done/total` once reported, and the
// neutral `–/N` while nothing is reported (a text cell has no bar to read as `0`, so the
// cross-surface `–/N` convention wins there). Empty for a run with no frozen list, so a
// non-milestone row draws nothing. Carries only counts, so it needs no D7 sanitizing. bg carries
// the selection bar behind it on a selected row.
func (m tuiModel) milestoneMarker(r apitypes.RunListItemDTO, dim bool, bg color.Color) string {
	done, total, reported := milestoneProgress(r.RunDTO)
	if total == 0 {
		return ""
	}
	if total > boardMileCap {
		return paintSeg(m.pal.faintC, bg, false, milestoneCount(done, total, reported))
	}
	fillC := m.pal.tungsten
	if dim {
		fillC = m.pal.faintC
	}
	// The in-progress cell (first frozen id in progress, D4) takes the cell right after the
	// done fill and blinks ▰/▱ in the wait colour. Not on a DONE (terminal) row — its
	// in-progress snapshot is stale and the row is faint end to end.
	ipID, _ := milestoneInProgress(r.RunDTO)
	blink := ipID != "" && done < total && !dim
	out := paintSeg(fillC, bg, false, strings.Repeat("▰", done))
	empty := total - done
	if blink {
		out += m.milestoneCell(bg)
		empty--
	}
	return out + paintSeg(m.pal.faintC, bg, false, strings.Repeat("▱", empty))
}

// boardShowSecondLine reports whether the SELECTED row gets its second "now" line (PRD #1064
// D4): a non-terminal run with a server-derived current_activity. It has no milestone
// precondition (D5) — the board reads current_activity directly. Used both by the row window
// math (which must reserve a physical line for it) and by renderBoard.
func (m tuiModel) boardShowSecondLine(r apitypes.RunListItemDTO) bool {
	return r.CurrentActivity != nil && !terminalRunStatuses[r.Status]
}

// boardSecondLine is the selected row's second line (PRD #1064 D4): `▸ <id> <title> · <role>
// <label> · <age>` from RunDTO.CurrentActivity, riding the warm selection bar like the crew
// rail's lane-label line. The `<id> <title>` prefix is the first in-progress milestone (D4) and
// is dropped when nothing is declared in progress; role/label/age come from the activity. Role
// and label are UNTRUSTED, model-authored text and go through renderer.Plain (D7). Clamped and
// padded to the full width so the selection bar spans the row.
func (m tuiModel) boardSecondLine(r apitypes.RunListItemDTO) string {
	act := r.CurrentActivity
	if act == nil || terminalRunStatuses[r.Status] {
		return ""
	}
	bg := m.pal.selBg
	var b strings.Builder
	b.WriteString(paintSeg(m.pal.tungsten, bg, false, "  ▸ "))
	if id, title := milestoneInProgress(r.RunDTO); id != "" {
		b.WriteString(paintSeg(m.pal.wait, bg, false, m.renderer.Plain(id, 12)))
		if title != "" {
			b.WriteString(paintSeg(nil, bg, false, " "+m.renderer.Plain(title, 40)))
		}
		b.WriteString(paintSeg(m.pal.faintC, bg, false, " · "))
	}
	b.WriteString(paintSeg(m.pal.sage, bg, false, m.renderer.Plain(act.Agent, 16)))
	if lbl := activityLabel(act); lbl != "" {
		b.WriteString(paintSeg(nil, bg, false, "  "+m.renderer.Plain(lbl, 48)))
	}
	b.WriteString(paintSeg(m.pal.faintC, bg, false, " · "+relAge(act.At)))
	return padSeg(clampVisual(b.String(), m.width), m.width, bg)
}
