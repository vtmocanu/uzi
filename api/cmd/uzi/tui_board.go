package main

import (
	"image/color"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// The board: a live list of runs, refreshed on a ListRuns poll (D3 — the board is
// list-level, so a socket per run to keep a counter fresh is disproportionate).

type boardState struct {
	runs   []apitypes.RunListItemDTO
	cursor int
	err    error

	// admin is the `[a]` factory-wide toggle. adminDenied records that the token
	// lacks the scope, so the refusal renders as a line rather than being retried.
	admin       bool
	adminDenied bool

	filtering bool
	filter    string

	// hideDone is the `[h]` toggle: drop terminal runs (completed/failed/cancelled) from
	// the board, keeping the active + needs-you set. A pure client-side view over the runs
	// already fetched, so toggling it needs no refetch. On the admin board it is a no-op —
	// AdminListRuns already returns non-terminal runs only.
	hideDone bool

	// scroll is the index of the first visible run row: the board windows the run list to
	// the terminal height so the header and footer stay on screen even with hundreds of runs.
	scroll int
}

func newBoardState() boardState { return boardState{} }

func (b *boardState) apply(msg boardRunsMsg) {
	// Ignore a reply for a view the user has since toggled away from, or a stale
	// admin reply would overwrite the own-runs list.
	if msg.admin != b.admin {
		return
	}
	if msg.err != nil {
		if b.admin {
			// A uzc_ token has no admin scope; the toggle is refused, not retried
			// (D8: cleanly refused, never a crash).
			b.admin = false
			b.adminDenied = true
		}
		b.err = msg.err
		return
	}
	b.err = nil
	b.runs = msg.runs
	b.clampCursor()
}

func (b *boardState) clampCursor() {
	n := len(b.visible())
	if b.cursor >= n {
		b.cursor = n - 1
	}
	if b.cursor < 0 {
		b.cursor = 0
	}
}

// visible applies the `/` filter, then orders the survivors into the three triage bands
// (NEEDS YOU → ON THE FLOOR → DONE). The cursor indexes this list, so the selectable order
// IS the visual order and navigation walks the bands top to bottom. The filter is the
// user's own text, matched against sanitized cell text so a control byte in a title cannot
// affect what matches — and against BOTH the raw status and the human status word, so a
// user can filter by either "awaiting_approval" or "plan gate".
func (b *boardState) visible() []apitypes.RunListItemDTO {
	base := b.runs
	if b.hideDone {
		kept := make([]apitypes.RunListItemDTO, 0, len(base))
		for _, r := range base {
			if terminalRunStatuses[r.Status] {
				continue
			}
			kept = append(kept, r)
		}
		base = kept
	}
	if strings.TrimSpace(b.filter) != "" {
		q := strings.ToLower(strings.TrimSpace(b.filter))
		out := make([]apitypes.RunListItemDTO, 0, len(base))
		for _, r := range base {
			// The effective status (the enum the user sees — "planning", not the raw "running"
			// #321 hides) plus the human word, so a user can filter by either "awaiting_approval"
			// or "plan gate". The truly-raw r.Status is deliberately NOT included: it would let
			// "running" match a planning run again, the exact thing #321 fixed.
			_, word := stateGlyphWord(r.Status, r.Health, r.IsPlanning)
			hay := strings.ToLower(strings.Join([]string{
				r.ID, r.Kind, effectiveRunStatus(r.Status, r.IsPlanning),
				word, r.Health, cellText(runTitle(r.RunDTO)),
			}, " "))
			if strings.Contains(hay, q) {
				out = append(out, r)
			}
		}
		base = out
	}
	return bandOrder(base)
}

// The three triage bands, in fixed top-to-bottom order.
const (
	bandNeedsYou = iota // awaiting_approval + awaiting_input — the only rows a human must act on
	bandFloor           // everything non-terminal not in NEEDS YOU (running/claimed/queued/planning/limit_wait, stalled)
	bandDone            // terminal: completed/failed/cancelled
	numBands
)

var bandNames = [numBands]string{"NEEDS YOU", "ON THE FLOOR", "DONE"}

// runBand places a run in its triage band from its status alone.
func runBand(status string) int {
	switch status {
	case "awaiting_approval", "awaiting_input":
		return bandNeedsYou
	}
	if terminalRunStatuses[status] {
		return bandDone
	}
	return bandFloor
}

// bandOrder partitions runs into the three bands, preserving each band's internal order
// (the server's), and concatenates them NEEDS YOU → ON THE FLOOR → DONE.
func bandOrder(runs []apitypes.RunListItemDTO) []apitypes.RunListItemDTO {
	var buckets [numBands][]apitypes.RunListItemDTO
	for _, r := range runs {
		b := runBand(r.Status)
		buckets[b] = append(buckets[b], r)
	}
	out := make([]apitypes.RunListItemDTO, 0, len(runs))
	for b := 0; b < numBands; b++ {
		out = append(out, buckets[b]...)
	}
	return out
}

func (b *boardState) selected() (apitypes.RunListItemDTO, bool) {
	v := b.visible()
	if b.cursor < 0 || b.cursor >= len(v) {
		return apitypes.RunListItemDTO{}, false
	}
	return v[b.cursor], true
}

func (m tuiModel) boardKey(k string) (tea.Model, tea.Cmd) {
	// Filter input mode swallows ordinary keys so typing "a" filters rather than
	// toggling the admin board.
	if m.board.filtering {
		switch k {
		case keyEnter, keyEsc:
			m.board.filtering = false
			if k == keyEsc {
				m.board.filter = ""
			}
			m.board.clampCursor()
		case "backspace":
			if n := len([]rune(m.board.filter)); n > 0 {
				m.board.filter = string([]rune(m.board.filter)[:n-1])
			}
			m.board.clampCursor()
		default:
			if k == keySpaceName {
				k = " "
			}
			if len([]rune(k)) == 1 {
				m.board.filter += k
				m.board.clampCursor()
			}
		}
		m.board.scroll = m.syncedScroll()
		return m, nil
	}

	if d := motionDelta(k); d != 0 {
		m.board.cursor += d
		m.board.clampCursor()
		m.board.scroll = m.syncedScroll()
		return m, nil
	}

	switch k {
	case keyFilter:
		m.board.filtering = true
		return m, nil
	case keyRefresh:
		return m, tea.Batch(m.fetchRunsCmd(m.board.admin), m.fetchRateLimitsCmd(), m.fetchSettingsCmd())
	case keyAdmin:
		m.board.admin = !m.board.admin
		m.board.adminDenied = false
		m.board.cursor = 0
		m.board.scroll = 0
		return m, m.fetchRunsCmd(m.board.admin)
	case keyHideDone:
		// No-op on the admin board: AdminListRuns already returns non-terminal runs only, so
		// hiding "finished" runs would change no rows — flipping the label there reads as a
		// broken toggle. Ignore it rather than toggling a hidden state.
		if m.board.admin {
			return m, nil
		}
		// Client-side view over the already-fetched runs, so no refetch. Reset the cursor and
		// scroll since the visible list changes shape under it.
		m.board.hideDone = !m.board.hideDone
		m.board.cursor = 0
		m.board.scroll = 0
		m.board.clampCursor()
		return m, nil
	case keyEnter, keyRight:
		// enter or → (right) opens the selected run — → is the natural "drill in" that pairs
		// with ← (left) backing out of the run detail (detailKey).
		sel, ok := m.board.selected()
		if !ok {
			return m, nil
		}
		m.view = viewDetail
		m.detail = newDetailState(sel.ID)
		return m, tea.Batch(m.loadDetailCmd(sel.ID), m.openStreamCmd(sel.ID))
	}
	return m, nil
}

func (m tuiModel) renderBoard() string {
	var sb strings.Builder
	rows := m.board.visible()

	// The wordmark, dark and quiet: ▚▚ uzi · <where>. The admin board stays labelled "active
	// runs" — AdminListRuns returns non-terminal runs only, so promising completed rows would
	// be a claim the API cannot satisfy.
	where := "floor"
	if m.board.admin {
		where = "active runs"
	}
	brand := m.pal.title.Render("▚▚ uzi") + m.pal.faint.Render(" · "+where)
	if m.board.hideDone && !m.board.admin {
		brand += m.pal.faint.Render("   active only")
	}
	if m.board.filter != "" || m.board.filtering {
		brand += m.pal.faint.Render("   /" + cellText(m.board.filter))
		if m.board.filtering {
			brand += m.pal.title.Render("▌")
		}
	}

	// The display list injects non-selectable eyebrow + spacer lines around the run rows; the
	// cursor still indexes RUN rows only (via visible()), so selection/enter/clamp are unchanged.
	// The window keeps the selected run row on screen so the wordmark and footer never scroll off.
	items := m.buildBoardItems(rows)
	capacity := m.boardCapacity()
	selItem := selectedBoardItem(items, m.board.cursor)
	start, end := boardWindow(selItem, m.board.scroll, len(items), capacity)

	// Summary glyph cluster + position readout, pinned top-right. Over the WHOLE board (not the
	// filtered view) so it stays a stable factory read while you filter.
	summary := m.boardSummary()
	if len(rows) > 0 {
		lo, hi := windowRunSpan(items, start, end)
		summary += m.pal.faint.Render(" · " + itoa(lo) + "–" + itoa(hi))
	}
	sb.WriteString(clampVisual(padVisual(" "+brand, m.width-visualWidth(summary)-1)+summary, m.width) + "\n")
	// The viewer's own rate-limit meters, mirroring the web sidebar's selection. One line,
	// only when at least one token is readable AND shown; otherwise nothing (no strip).
	if strip := m.boardRateLimitStrip(); strip != "" {
		sb.WriteString(strip + "\n")
	}
	sb.WriteString("\n")

	if m.board.adminDenied {
		sb.WriteString(clampVisual(m.pal.faint.Render(" the factory-wide board needs an admin (uza_) token — showing your runs"), m.width) + "\n")
	}
	if m.board.err != nil {
		sb.WriteString(clampVisual(m.pal.faint.Render(" could not refresh: "+fmtErr(m.board.err)), m.width) + "\n")
	}

	if len(rows) == 0 {
		sb.WriteString(m.boardEmptyState())
	} else {
		// The judge marker's sub-column widths are sized to the WHOLE visible list (not just the
		// window) so the columns do not jitter as you scroll. Every row flushes its marker to the
		// board's right edge.
		mc := m.boardMarkerCols(rows)
		for i := start; i < end; i++ {
			switch it := items[i]; it.kind {
			case biEyebrow:
				sb.WriteString(m.boardEyebrow(it) + "\n")
			case biRow:
				sb.WriteString(m.boardRow(rows[it.runIdx], it.runIdx == m.board.cursor, mc) + "\n")
			default:
				sb.WriteString("\n")
			}
		}
	}

	sb.WriteString(clampVisual(m.boardFooter(), m.width))
	return sb.String()
}

// boardItem is one line of the board's display list: a band eyebrow, a run row, or a blank
// spacer between bands. Only biRow lines are selectable; runIdx indexes visible().
type boardItem struct {
	kind   int
	band   int // biEyebrow: which band
	count  int // biEyebrow: how many runs in it
	runIdx int // biRow: index into visible()
}

const (
	biEyebrow = iota
	biRow
	biSpacer
)

// buildBoardItems turns the (already band-ordered) visible run list into the flat display
// list: an eyebrow at each band boundary, a spacer between bands, one row per run. Cheap
// (no styling), so both the renderer and the key handler's syncedScroll can call it and agree
// on line positions.
func (m tuiModel) buildBoardItems(rows []apitypes.RunListItemDTO) []boardItem {
	var counts [numBands]int
	for _, r := range rows {
		counts[runBand(r.Status)]++
	}
	items := make([]boardItem, 0, len(rows)+numBands*2)
	prevBand := -1
	for i, r := range rows {
		if b := runBand(r.Status); b != prevBand {
			if prevBand != -1 {
				items = append(items, boardItem{kind: biSpacer})
			}
			items = append(items, boardItem{kind: biEyebrow, band: b, count: counts[b]})
			prevBand = b
		}
		items = append(items, boardItem{kind: biRow, runIdx: i})
	}
	return items
}

// selectedBoardItem is the display index of the row the cursor is on (0 if none).
func selectedBoardItem(items []boardItem, cursor int) int {
	for i, it := range items {
		if it.kind == biRow && it.runIdx == cursor {
			return i
		}
	}
	return 0
}

// windowRunSpan is the 1-based run position range [lo, hi] visible in items[start:end].
func windowRunSpan(items []boardItem, start, end int) (lo, hi int) {
	for i := start; i < end && i < len(items); i++ {
		if items[i].kind != biRow {
			continue
		}
		pos := items[i].runIdx + 1
		if lo == 0 || pos < lo {
			lo = pos
		}
		if pos > hi {
			hi = pos
		}
	}
	if lo == 0 {
		lo = 1
	}
	return lo, hi
}

// boardEyebrow is a faint CAPS band label + count; NEEDS YOU is amber, because it is the one
// band a human must act on.
func (m tuiModel) boardEyebrow(it boardItem) string {
	name := bandNames[it.band]
	if it.band == bandNeedsYou {
		return " " + lipgloss.NewStyle().Foreground(m.pal.amber).Bold(true).Render(name) + m.pal.faint.Render(" · "+itoa(it.count))
	}
	return " " + m.pal.faint.Render(name+" · "+itoa(it.count))
}

// boardFooter is the one-line key legend; key letters are tungsten (keyHint), labels faint.
func (m tuiModel) boardFooter() string {
	parts := []string{m.keyHint("enter/→", "open"), m.keyHint("/", "filter")}
	if m.board.admin {
		parts = append(parts, m.keyHint("a", "my runs"))
	} else {
		parts = append(parts, m.keyHint("a", "factory"))
		// The fold-done toggle is meaningless on the admin board (non-terminal runs only), so
		// its hint is dropped there rather than offering a no-op.
		if m.board.hideDone {
			parts = append(parts, m.keyHint("h", "show done"))
		} else {
			parts = append(parts, m.keyHint("h", "fold done"))
		}
	}
	parts = append(parts, m.keyHint("r", "refresh"), m.keyHint("?", "keys"), m.keyHint("q", "quit"))
	return " " + strings.Join(parts, m.pal.faint.Render(" · "))
}

const (
	boardIDWidth         = 8  // short run id (first 8 of the UUID)
	boardStatusWordWidth = 12 // status word cell (fits the longest word, "rate-limited")
	boardAgeWidth        = 4  // AGE cell (relAge, single-unit)
	boardMileWidth       = 9  // milestone micro-bar cell (up to boardMileCap ▰/▱ cells, or done/total | –/N text above that)
	boardMileCap         = 9  // above this many milestones the micro-bar falls back to N/M text
	boardOwnerWidth      = 20 // admin owner-email cell
	boardCredWidth       = 10 // credential cell: a 2-col tone-dot slot + up to 8 cols of token label
	boardTitleMax        = 60 // TITLE cap: long titles trim to a tidy column instead of running a wide terminal
	// boardMileMinWidth is the narrowest own board that shows the milestone micro-bar. Below it
	// the column is dropped (milestone progress is still on the run-detail view) so the fixed
	// prefix does not squeeze the title into an overflowing marker row (issue #379).
	boardMileMinWidth = 91
)

// boardShowMile reports whether the terminal is wide enough for the milestone micro-bar column.
// The admin board carries an extra owner cell, so its prefix is wider and the threshold shifts
// up by exactly that extra prefix — the column drops on a narrow admin board instead of clipping.
func (m tuiModel) boardShowMile() bool {
	min := boardMileMinWidth
	// The extra columns before TITLE (the admin owner cell, and the credential cell when
	// shown) push the mile threshold up by exactly their width, so a narrow board drops the
	// micro-bar instead of squeezing the title (issue #379). mile is false in both terms so
	// it cancels; only the owner + credential deltas survive.
	min += boardRowPrefixWidth(m.board.admin, false, m.boardShowCred()) - boardRowPrefixWidth(false, false, false)
	return m.width >= min
}

// boardShowCred gates the credential column exactly as the web RunsList does (PRD #295):
// the admin factory board always shows it (naming which account a run billed is the point
// of the factory view), the own board only when the viewer holds more than one Anthropic
// token — a single token has nothing to disambiguate.
func (m tuiModel) boardShowCred() bool {
	return m.board.admin || m.tokenCount > 1
}

// boardCapacity is how many display lines fit between the wordmark block and the footer at the
// current terminal height. It counts the same chrome renderBoard draws: the wordmark line, the
// blank below it, the footer (3), plus the optional adminDenied and error lines. At least one
// line is always shown.
func (m tuiModel) boardCapacity() int {
	chrome := 3
	if m.board.adminDenied {
		chrome++
	}
	if m.board.err != nil {
		chrome++
	}
	// The rate-limit strip, when present, adds one line between the wordmark and the blank
	// below it. Recomputed here (cheap) so the row-window math matches renderBoard's layout.
	if m.boardRateLimitStrip() != "" {
		chrome++
	}
	c := m.height - chrome
	if c < 1 {
		c = 1
	}
	return c
}

// boardWindow is the [start, end) slice of the display list to draw, scrolled so the selected
// line stays on screen. Stateless: it takes the last scroll offset and returns a corrected one
// via start, so it is safe to call from both the key handler (to persist) and the renderer.
func boardWindow(cursor, scroll, n, capacity int) (int, int) {
	if capacity < 1 {
		capacity = 1
	}
	if n <= capacity {
		return 0, n
	}
	start := scroll
	if start < 0 {
		start = 0
	}
	if cursor < start {
		start = cursor
	}
	if cursor >= start+capacity {
		start = cursor - capacity + 1
	}
	if maxStart := n - capacity; start > maxStart {
		start = maxStart
	}
	if start < 0 {
		start = 0
	}
	return start, start + capacity
}

// syncedScroll returns the scroll offset that keeps the selected row visible, for the key
// handler to persist so in-window navigation does not shift the viewport on every keystroke.
func (m tuiModel) syncedScroll() int {
	items := m.buildBoardItems(m.board.visible())
	sel := selectedBoardItem(items, m.board.cursor)
	start, _ := boardWindow(sel, m.board.scroll, len(items), m.boardCapacity())
	return start
}

// boardSummary is the top-right glyph cluster: ⚑ N · ✎ N · ▲ N · <total> runs. Zero-count
// segments are dropped, so a healthy factory reads simply "N runs". Computed over m.board.runs
// so it does not shrink under a filter.
func (m tuiModel) boardSummary() string {
	approvals, inputs, warn := 0, 0, 0
	for _, r := range m.board.runs {
		switch r.Status {
		case "awaiting_approval":
			approvals++
		case "awaiting_input":
			inputs++
		}
		if stalledHealth[r.Health] {
			warn++
		}
	}
	var segs []string
	if approvals > 0 {
		segs = append(segs, paintSeg(m.pal.amber, nil, false, "⚑ "+itoa(approvals)))
	}
	if inputs > 0 {
		segs = append(segs, paintSeg(m.pal.amber, nil, false, "✎ "+itoa(inputs)))
	}
	if warn > 0 {
		segs = append(segs, paintSeg(m.pal.stall, nil, false, "▲ "+itoa(warn)))
	}
	segs = append(segs, m.pal.faint.Render(itoa(len(m.board.runs))+" runs"))
	return strings.Join(segs, m.pal.faint.Render(" · "))
}

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
func (m tuiModel) boardRateLimitStrip() string {
	readable := make([]apitypes.TokenRateLimitDTO, 0, len(m.rateLimits))
	for _, t := range m.rateLimits {
		if t.Limits.Status == "ok" {
			readable = append(readable, t)
		}
	}
	if len(readable) == 0 {
		return ""
	}
	showLabel := len(readable) > 1
	shown := make([]apitypes.TokenRateLimitDTO, 0, len(readable))
	for _, t := range readable {
		if t.IsDefault || slices.Contains(m.sidebarTokenIds, t.SecretID) {
			shown = append(shown, t)
		}
	}
	if len(shown) == 0 {
		return ""
	}
	segs := make([]string, 0, len(shown))
	for _, t := range shown {
		seg := ""
		if showLabel {
			seg = paintSeg(m.pal.faintC, nil, false, m.renderer.Plain(t.Label, 16)+" ")
		}
		seg += m.rateWindowCell("5h", t.Limits.FiveHour) + "   " + m.rateWindowCell("7d", t.Limits.SevenDay)
		// Prefix a per-group accent bar TIGHT against the label: it both delimits the group and
		// doubles as a status light — alarm when the token's peak window pct ≥ rateDangerPct,
		// faint otherwise. Emitted unconditionally via paintSeg so the delimiter and the danger
		// cue survive colour stripping (NO_COLOR/Ascii).
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
func (m tuiModel) rateWindowCell(label string, w *apitypes.RateLimitWindow) string {
	if w == nil {
		return paintSeg(m.pal.faintC, nil, false, label+" -")
	}
	filled, empty := rateBarParts(w.Pct)
	return paintSeg(m.pal.faintC, nil, false, label+" ") +
		paintSeg(m.rateTone(w.Pct), nil, false, filled) +
		paintSeg(m.pal.faintC, nil, false, empty) +
		paintSeg(m.pal.faintC, nil, false, " "+windowPct(w)+"%")
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

// rateBarParts splits the rateBarWidth-cell mini bar into its filled (▰) and empty (▱) runs,
// filled proportional to pct (the same ▰/▱ glyph vocabulary the milestone micro-bar uses).
func rateBarParts(pct int) (filled, empty string) {
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	f := (pct*rateBarWidth + 50) / 100
	if f > rateBarWidth {
		f = rateBarWidth
	}
	return strings.Repeat("▰", f), strings.Repeat("▱", rateBarWidth-f)
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

// paintSeg styles one row segment with an optional foreground and (for a selected row) the warm
// selection background. Applying the bg to EVERY segment — text and padding alike — is what
// keeps it continuous across the row: wrapping pre-styled fg spans in an outer bg would be
// dropped by their inner resets (the documented lipgloss gotcha), so each span carries its own.
func paintSeg(fg, bg color.Color, bold bool, s string) string {
	st := lipgloss.NewStyle()
	if fg != nil {
		st = st.Foreground(fg)
	}
	if bg != nil {
		st = st.Background(bg)
	}
	if bold {
		st = st.Bold(true)
	}
	return st.Render(s)
}

// padSeg right-pads s to n visual columns with background-carrying spaces, so a selected row's
// warm bar reaches the padded width rather than stopping at the last glyph.
func padSeg(s string, n int, bg color.Color) string {
	if w := visualWidth(s); w < n {
		return s + paintSeg(nil, bg, false, strings.Repeat(" ", n-w))
	}
	return s
}

// boardRow renders one run: the per-row ▌ andon strip and state glyph (state colour), the id,
// the owner on the admin board, the status WORD (state colour, not a filled chip), AGE, the
// milestone micro-bar, and the title, with an own-board-only judge marker flushed right. A
// selected row rides the full-width warm selection bar with a ▸ cursor; a DONE-band row is
// faint end to end.
func (m tuiModel) boardRow(r apitypes.RunListItemDTO, sel bool, mc boardMarkerCols) string {
	band := runBand(r.Status)
	terminal := band == bandDone
	tok := m.pal.stateToken(r.Status, r.Health, r.IsPlanning)
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
	// the same column the web Runs list shows, in the same place — before the title.
	if showCred {
		row += m.boardCredSeg(r, bg) + gap
	}

	// Title: capped to the remaining width AND to boardTitleMax. The marker column allowance is
	// reserved for every row so the title keeps one width down the board; markers then align by
	// flushing the whole row to width-fullW below.
	markerAllow := 0
	if mc.fullW > 0 {
		markerAllow = mc.fullW + 2
	}
	avail := m.width - boardRowPrefixWidth(m.board.admin, m.boardShowMile(), showCred) - markerAllow
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

// boardRowPrefixWidth is the visual width of every column before TITLE, so the title can be
// sized to what remains. cursor(1)+strip(1)+glyph(1)+space(1)+id, then two-space gaps around
// the status-word and AGE cells, the micro-bar cell when shown, the credential cell when
// shown, plus the admin owner cell.
func boardRowPrefixWidth(admin, mile, cred bool) int {
	w := 4 + boardIDWidth + 2 + boardStatusWordWidth + 2 + boardAgeWidth + 2
	if mile {
		w += boardMileWidth + 2
	}
	if cred {
		w += boardCredWidth + 2
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
	return paintSeg(fillC, bg, false, strings.Repeat("▰", done)) +
		paintSeg(m.pal.faintC, bg, false, strings.Repeat("▱", total-done))
}

// shortRunID is the board's id cell: the first 8 of a UUID, which is the rule
// shortRecID already uses for random UUIDs (unlike an SDK tool-use id, a run id has no
// constant prefix, so a head is the right end to take).
func shortRunID(id string) string {
	r := []rune(id)
	if len(r) <= 8 {
		return id
	}
	return string(r[:8])
}

// padCell right-pads to n RUNES. Rune-based to match capCell, so a multibyte title
// does not shift the column.
func padCell(s string, n int) string {
	c := capCell(s, n)
	if pad := n - len([]rune(c)); pad > 0 {
		return c + strings.Repeat(" ", pad)
	}
	return c
}
