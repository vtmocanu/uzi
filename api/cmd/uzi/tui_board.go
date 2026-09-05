package main

import (
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
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

	// boardInFlight marks a periodic board ListRuns poll as pending (PRD #1130 M1 D1):
	// a boardTickMsg skips issuing a new fetch while it is set, so a slow link cannot
	// stack overlapping requests. It is cleared in the boardRunsMsg case on EVERY reply
	// (D2 — not behind apply's admin-mismatch early return, so a mid-flight admin toggle
	// cannot latch it). User-initiated r/a fetches set it but are never gated on it.
	boardInFlight bool

	// errStreak is the consecutive-failure counter driving the tick backoff (PRD #1130 M3 D4):
	// each consecutive failed board poll widens the reschedule interval via boardTickInterval,
	// and the first success resets it to 0 so the cadence snaps back to the 2s base. Updated
	// inside apply, so it honours the same admin-mismatch early return (a stale reply leaves it
	// unchanged).
	errStreak int

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
		// A genuine failed poll for the board this reply belongs to: widen the backoff
		// (PRD #1130 M3 D4). Placed after the admin-mismatch early return, so a stale reply
		// leaves the streak untouched. The admin-refusal sub-branch above is still an error
		// reply for the own board that follows, so incrementing here is correct for it too.
		b.errStreak++
		return
	}
	b.err = nil
	b.errStreak = 0
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
			_, word := stateGlyphWord(r.Status, r.Health, r.IsPlanning, r.IsRevising)
			hay := strings.ToLower(strings.Join([]string{
				r.ID, r.Kind, effectiveRunStatus(r.Status, r.IsPlanning, r.IsRevising),
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
	bandNeedsYou = iota // awaiting_approval + awaiting_input + awaiting_followup — the only rows a human must act on
	bandFloor           // everything non-terminal not in NEEDS YOU (running/claimed/queued/planning/limit_wait/pool_wait, stalled)
	bandDone            // terminal: completed/failed/cancelled
	numBands
)

var bandNames = [numBands]string{"NEEDS YOU", "ON THE FLOOR", "DONE"}

// runBand places a run in its triage band from its status (plus is_revising).
//
// issue #750: a run mid-"revise" replan keeps status == awaiting_approval but is NOT the
// user's turn — the agent is re-planning (~90s) and will re-gate itself — so it drops to
// ON THE FLOOR instead of sitting in NEEDS YOU. is_revising is NOT status-gated
// server-side, so the awaiting_approval check is applied here (mirroring effectiveRunStatus).
func runBand(status string, isRevising bool) int {
	if status == "awaiting_approval" && isRevising {
		return bandFloor
	}
	switch status {
	case "awaiting_approval", "awaiting_input", "awaiting_followup":
		// awaiting_followup (PRD #517) is the user's turn — an interactive task parked for
		// its next follow-up — so it belongs in NEEDS YOU alongside the other two parks.
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
		b := runBand(r.Status, r.IsRevising)
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
		// User-initiated fetch (PRD #1130 M1 D1): NEVER gated on the in-flight guard — a
		// keypress is intent and always issues a fetch. It sets the marker so the next
		// periodic tick does not stack a second poll on top of this one; the reply clears it.
		m.board.boardInFlight = true
		return m, tea.Batch(m.fetchRunsCmd(m.board.admin), m.fetchRateLimitsCmd(), m.fetchSettingsCmd())
	case keyAdmin:
		m.board.admin = !m.board.admin
		m.board.adminDenied = false
		m.board.cursor = 0
		m.board.scroll = 0
		// Same as keyRefresh (D1): always fetch, and set the marker so the next tick does not
		// stack. The subsequent boardRunsMsg for the new admin value clears it in that case.
		m.board.boardInFlight = true
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
	if strip := m.boardRateLimitStrip(time.Now()); strip != "" {
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
				r := rows[it.runIdx]
				sel := it.runIdx == m.board.cursor
				sb.WriteString(m.boardRow(r, sel, mc) + "\n")
				// The selected row gains a variable-height second "now" line (D4). The window
				// math reserves its physical line via boardCapacity, so this never overflows.
				if sel {
					if sl := m.boardSecondLine(r); sl != "" {
						sb.WriteString(sl + "\n")
					}
				}
			default:
				sb.WriteString("\n")
			}
		}
	}

	sb.WriteString(clampVisual(m.boardFooterLine(), m.width))
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
		counts[runBand(r.Status, r.IsRevising)]++
	}
	items := make([]boardItem, 0, len(rows)+numBands*2)
	prevBand := -1
	for i, r := range rows {
		if b := runBand(r.Status, r.IsRevising); b != prevBand {
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

// boardFooterLine is the board's footer: the help legend, plus an always-on compact
// CLI-vs-server version readout right-aligned at the far edge (issue #687, superseding
// #681's conditional skew sentence). m.showVersion gates the whole readout (the off
// switches: --quiet / UZI_VERSION_CHECK=0 / the CheckServerVersion seam). The single
// outer clampVisual(..., m.width) at the renderBoard call site is the only wrap.
func (m tuiModel) boardFooterLine() string {
	help := m.boardFooter()
	if !m.showVersion {
		return help
	}
	const gap = 1
	helpW := visualWidth(help)
	readout := m.versionReadout()
	if helpW+gap+visualWidth(readout) <= m.width {
		return padVisual(help, m.width-visualWidth(readout)) + readout
	}
	// Too narrow for the full readout: drop the "<arrow> <server>" suffix and show the
	// client version alone (still red when behind, so the alarm survives the drop).
	client := m.versionClientOnly()
	cw := visualWidth(client)
	if helpW+gap+cw <= m.width {
		return padVisual(help, m.width-cw) + client
	}
	// Still too narrow: give the client version the right edge, let help truncate.
	left := clampVisual(help, m.width-cw-gap)
	return padVisual(left, m.width-cw) + client
}

// versionReadout is the compact CLI-vs-server version line. The client version renders
// verbatim as this binary was stamped (v0.63.0, or `dev`); the server renders verbatim
// as it reports (bare 0.63.0), sanitized through cellText — it is attacker-controlled
// (GET /api/version) and serverVersion is in d7UntrustedFields. Steady colour only, no
// blink (SGR 5 is an accessibility problem, WCAG 2.2.2); red is reinforcement, the arrow
// and server number carry "behind" on their own — which is what survives NO_COLOR/Ascii.
func (m tuiModel) versionReadout() string {
	client := version
	srv := cellText(m.serverVersion)
	cmp, ok := uzicli.CompareServerVersion(client, srv)
	if !ok {
		// dev build, or no/invalid server version: client alone, neutral.
		return m.pal.faint.Render(client)
	}
	ascii := m.profile == colorprofile.Ascii
	switch {
	case cmp == 0:
		return m.pal.faint.Render(client) // in sync: one neutral version
	case cmp < 0: // CLI behind server: client red, faint arrow to server
		arrow, marker := " ⇢ ", ""
		if ascii {
			arrow, marker = " -> ", " (behind)"
		}
		red := lipgloss.NewStyle().Foreground(m.pal.alarm)
		return red.Render(client) + m.pal.faint.Render(arrow+srv+marker)
	default: // CLI ahead of server: neutral, never alarm
		arrow := " ⇠ "
		if ascii {
			arrow = " <- "
		}
		return m.pal.faint.Render(client + arrow + srv)
	}
}

// versionClientOnly is the narrow-width fallback: the client version with no server
// suffix, still red when the CLI is behind so the alarm is not silently dropped.
func (m tuiModel) versionClientOnly() string {
	client := version
	if cmp, ok := uzicli.CompareServerVersion(client, cellText(m.serverVersion)); ok && cmp < 0 {
		return lipgloss.NewStyle().Foreground(m.pal.alarm).Render(client)
	}
	return m.pal.faint.Render(client)
}

const (
	boardIDWidth         = 8  // short run id (first 8 of the UUID)
	boardStatusWordWidth = 12 // status word cell (fits the longest word, "rate-limited")
	boardAgeWidth        = 4  // AGE cell (relAge, single-unit)
	boardMileWidth       = 9  // milestone micro-bar cell (up to boardMileCap ▰/▱ cells, or done/total | –/N text above that)
	boardMileCap         = 9  // above this many milestones the micro-bar falls back to N/M text
	boardOwnerWidth      = 20 // admin owner-email cell
	boardCredWidth       = 10 // credential cell: a 2-col tone-dot slot + up to 8 cols of token label
	boardCostWidth       = 6  // COST cell width (right-aligned; fits "$9999", "<$1", "—")
	boardTitleMax        = 60 // TITLE cap: long titles trim to a tidy column instead of running a wide terminal
	// boardMileMinWidth is the narrowest own board that shows the milestone micro-bar. Below it
	// the column is dropped (milestone progress is still on the run-detail view) so the fixed
	// prefix does not squeeze the title into an overflowing marker row (issue #379).
	boardMileMinWidth = 91
	// boardCostMinWidth is the narrowest own board that keeps the COST column. It sits BELOW
	// boardMileMinWidth so COST (the higher-priority cost signal) is retained after the milestone
	// micro-bar drops on a narrowing terminal (PRD #650 M2; issue #379 invariant).
	boardCostMinWidth = 83
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
	min += boardRowPrefixWidth(m.board.admin, false, m.boardShowCred(), m.boardShowCost()) - boardRowPrefixWidth(false, false, false, false)
	return m.width >= min
}

// boardShowCost gates the own-board COST column on terminal width. The admin board has no
// per-run Usage on the wire (AdminListRuns attaches none), so it never shows the column —
// the same way it carries no judge marker. COST is retained down to boardCostMinWidth, BELOW
// boardMileMinWidth, so on a narrowing terminal the milestone micro-bar drops FIRST and COST
// (the higher-priority cost signal) drops only after it — the title is never the column cut.
func (m tuiModel) boardShowCost() bool {
	if m.board.admin {
		return false
	}
	min := boardCostMinWidth
	// The extra columns before TITLE (the credential cell when shown) push the threshold up by
	// exactly their width; cost is false in both terms so it cancels, only the cred delta survives.
	min += boardRowPrefixWidth(false, false, m.boardShowCred(), false) - boardRowPrefixWidth(false, false, false, false)
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
	if m.boardRateLimitStrip(time.Now()) != "" {
		chrome++
	}
	// The selected row's variable-height second "now" line (D4) reserves one physical line, so
	// the row window shows one fewer item and the extra line never pushes the footer off screen.
	// This is the ONE variable-height row the board's selection/scroll math must tolerate.
	if r, ok := m.board.selected(); ok && m.boardShowSecondLine(r) {
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

// boardSummary is the top-right glyph cluster: ⚑ N · ✎ N · ➤ N · ▲ N · <total> runs.
// Zero-count segments are dropped, so a healthy factory reads simply "N runs". Computed
// over m.board.runs so it does not shrink under a filter. All three parks in the NEEDS YOU
// band get a segment — awaiting_followup (PRD #517) alongside awaiting_approval and
// awaiting_input — so a follow-up park is never invisible in the summary line.
func (m tuiModel) boardSummary() string {
	approvals, inputs, followups, warn := 0, 0, 0, 0
	for _, r := range m.board.runs {
		switch r.Status {
		case "awaiting_approval":
			// issue #750: a run mid-"revise" replan keeps status == awaiting_approval but is
			// NOT the user's turn (the agent is re-planning and will re-gate itself), so it must
			// not inflate the ⚑ counter — mirror runBand's !isRevising gate so the cluster count
			// matches the NEEDS YOU band header.
			if !r.IsRevising {
				approvals++
			}
		case "awaiting_input":
			inputs++
		case "awaiting_followup":
			followups++
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
	if followups > 0 {
		segs = append(segs, paintSeg(m.pal.amber, nil, false, "➤ "+itoa(followups)))
	}
	if warn > 0 {
		segs = append(segs, paintSeg(m.pal.stall, nil, false, "▲ "+itoa(warn)))
	}
	// Floor total (PRD #650): the rounded raw-CostUSD sum over usage-bearing runs, own board only
	// (the admin board attaches no Usage, so its total would always be 0 — the guard makes the
	// "admin shows no total" invariant explicit). boardCostTotal drops the segment when 0. It wears
	// the tungsten accent (not the faint chrome of the ` · ` dividers and `N runs`) so the floor's
	// headline spend figure is findable in the cluster and matches the detail SPEND total's weight.
	if !m.board.admin {
		if total, ok := boardCostTotal(m.board.runs); ok {
			segs = append(segs, lipgloss.NewStyle().Foreground(m.pal.tungsten).Render(total))
		}
	}
	segs = append(segs, m.pal.faint.Render(itoa(len(m.board.runs))+" runs"))
	return strings.Join(segs, m.pal.faint.Render(" · "))
}
