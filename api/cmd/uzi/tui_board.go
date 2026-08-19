package main

import (
	"fmt"
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

// visible applies the `/` filter over the fields a human would search by. The filter
// is the user's own text, matched against sanitized cell text so a control byte in a
// title cannot affect what matches.
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
	if strings.TrimSpace(b.filter) == "" {
		return base
	}
	q := strings.ToLower(strings.TrimSpace(b.filter))
	out := make([]apitypes.RunListItemDTO, 0, len(base))
	for _, r := range base {
		hay := strings.ToLower(strings.Join([]string{
			r.ID, r.Kind, effectiveRunStatus(r.Status, r.IsPlanning), cellText(runTitle(r.RunDTO)),
		}, " "))
		if strings.Contains(hay, q) {
			out = append(out, r)
		}
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
		return m, m.fetchRunsCmd(m.board.admin)
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
	case keyEnter:
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

	title := "factory floor"
	if m.board.admin {
		// The admin view is NOT symmetric with own-runs: AdminListRuns returns
		// non-terminal runs only, capped, with no judge/usage columns. Labelling it
		// "active runs (factory-wide)" is the honest header — promising completed rows,
		// or calling it "all crews", is a claim the API cannot satisfy.
		title = "active runs (factory-wide)"
	}
	// Wordmark + a summary bar that answers "does anything need me?" before the eye
	// reaches a row (PRD #325 M2). The summary is over the WHOLE board, not the filtered
	// view, so it stays a stable factory read while you filter.
	brand := m.pal.title.Render("▚ uzi") + m.pal.faint.Render("  "+title)
	if m.board.hideDone && !m.board.admin {
		brand += m.pal.faint.Render("   active only")
	}
	if m.board.filter != "" || m.board.filtering {
		brand += m.pal.faint.Render("   /" + cellText(m.board.filter))
		if m.board.filtering {
			brand += m.pal.faint.Render("▌")
		}
	}
	// Window the run list to the terminal height so the header and footer stay on screen
	// no matter how many runs there are. capacity is how many run rows fit; start is the
	// first visible row, scrolled to keep the cursor in view.
	capacity := m.boardCapacity()
	start, end := boardWindow(m.board.cursor, m.board.scroll, len(rows), capacity)

	summary := m.boardSummary()
	if len(rows) > capacity {
		// Position readout so a windowed board says where you are in it (top-right, beside
		// the run counts), since the list no longer shows every row at once.
		summary = m.pal.faint.Render(fmt.Sprintf("%d–%d of %d   ", start+1, end, len(rows))) + summary
	}
	sb.WriteString(padVisual(brand, m.width-visualWidth(summary)-1) + summary + "\n")
	sb.WriteString(m.pal.faint.Render(strings.Repeat("─", boardRuleWidth(m.width))) + "\n")

	if m.board.adminDenied {
		sb.WriteString(m.pal.faint.Render("the factory-wide board needs an admin (uza_) token — showing your runs") + "\n")
	}
	if m.board.err != nil {
		sb.WriteString(m.pal.faint.Render("could not refresh: "+fmtErr(m.board.err)) + "\n")
	}

	// Column header. The 3-space prefix aligns RUN with the data rows, whose spine (1) +
	// gutter (1) + space (1) also occupy 3 cols; every column below uses two-space gaps.
	ownerHdr := ""
	if m.board.admin {
		ownerHdr = "  " + padCell("OWNER", boardOwnerWidth)
	}
	mileHdr := ""
	if m.boardShowMile() {
		mileHdr = "  " + padCell("MILE", boardMileWidth)
	}
	sb.WriteString("   " + m.pal.faint.Render(padCell("RUN", 9)+ownerHdr+"  "+padCell("STATUS", boardStatusWidth)+"  "+padCell("HEALTH", boardHealthWidth)+"  "+padCell("AGE", 5)+mileHdr+"  TITLE") + "\n")

	if len(rows) == 0 {
		sb.WriteString(m.boardEmptyState())
	} else {
		// The judge marker's sub-column widths are sized to the WHOLE visible list (not just the
		// window) so the columns do not jitter as you scroll. Every row flushes its marker to the
		// board's right edge.
		mc := m.boardMarkerCols(rows)
		for i := start; i < end; i++ {
			sb.WriteString(m.boardRow(rows[i], i == m.board.cursor, mc) + "\n")
		}
	}

	sb.WriteString(m.pal.faint.Render(strings.Repeat("─", boardRuleWidth(m.width))) + "\n")
	// The hide-done toggle is meaningless on the admin board (non-terminal runs only), so its
	// key hint is dropped there rather than offering a no-op.
	hideHint := ""
	if !m.board.admin {
		if m.board.hideDone {
			hideHint = "h show done · "
		} else {
			hideHint = "h hide done · "
		}
	}
	sb.WriteString(m.pal.faint.Render("enter open · / filter · a admin · " + hideHint + "r refresh · ? keys · q quit"))
	return sb.String()
}

const (
	boardStatusWidth = 19 // status chip cell (fits the longest status, "awaiting_approval" + chip padding)
	boardHealthWidth = 10 // HEALTH cell (stalled ▲, or a non-stalled health word, truncated)
	boardOwnerWidth  = 18 // admin OWNER cell
	boardMileWidth   = 6  // MILE cell — fits the "MILE" header and M{done}/{total} up to two digits each (e.g. M12/34)
	boardTitleMax    = 60 // TITLE cap: long titles are trimmed to a tidy column instead of running the width of a wide terminal
	// boardMileMinWidth is the narrowest board that shows the MILE column. The column adds 8
	// cols to the fixed prefix (6 + a 2-space gap); below this the prefix would squeeze the
	// title so hard that a marker row overflows the terminal edge and the judge marker clips
	// with no ellipsis (issue #379 tui-ux finding). Below it the column is dropped and the
	// board reverts to the pre-#379 layout — milestone progress is still on the run detail
	// view. 90 clears the marker-row overflow point (prefix 62 + widest marker 14 + a 10-col
	// title floor = 86, so width ≥ 87 fits; 90 is a small safety margin).
	boardMileMinWidth = 90
)

// boardShowMile reports whether the terminal is wide enough for the MILE column (issue #379).
//
// boardMileMinWidth is calibrated against the NON-admin row prefix. The admin board carries an
// extra OWNER column, so its prefix is boardOwnerWidth+2 cols wider and the same MILE tax
// overflows the floored title at widths 90-91 (#380 review). Shift the admin threshold up by
// exactly that extra prefix so the column drops on a narrow admin board instead of clipping —
// milestone progress is still on the run-detail view.
func (m tuiModel) boardShowMile() bool {
	min := boardMileMinWidth
	if m.board.admin {
		min += boardRowPrefixWidth(true, true) - boardRowPrefixWidth(false, true)
	}
	return m.width >= min
}

func boardRuleWidth(w int) int {
	if w < 2 {
		return 1
	}
	return w - 1
}

// boardCapacity is how many run rows fit between the header block and the footer at the
// current terminal height. It must count the same chrome lines renderBoard draws: brand +
// rule + column header (3) and the closing rule + footer (2), plus the optional adminDenied
// and error lines. At least one row is always shown.
func (m tuiModel) boardCapacity() int {
	chrome := 5
	if m.board.adminDenied {
		chrome++
	}
	if m.board.err != nil {
		chrome++
	}
	c := m.height - chrome
	if c < 1 {
		c = 1
	}
	return c
}

// boardWindow is the [start, end) slice of the visible run list to draw, scrolled so the
// cursor stays on screen. Stateless: it takes the last scroll offset and returns a corrected
// one via start, so it is safe to call from both the key handler (to persist) and the
// renderer (to self-correct after a height or list change).
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

// syncedScroll returns the scroll offset that keeps the cursor visible, for the key handler
// to persist so in-window navigation does not shift the viewport on every keystroke.
func (m tuiModel) syncedScroll() int {
	start, _ := boardWindow(m.board.cursor, m.board.scroll, len(m.board.visible()), m.boardCapacity())
	return start
}

// boardSummary is the "N runs · N needs you · N stalled" bar. Predicates (PRD #325 M2):
// needs you = awaiting_approval + awaiting_input; stalled = health "stalled"; N runs =
// the whole board. Computed over m.board.runs so it does not shrink under a filter.
func (m tuiModel) boardSummary() string {
	needs, stalled := 0, 0
	for _, r := range m.board.runs {
		if r.Status == "awaiting_approval" || r.Status == "awaiting_input" {
			needs++
		}
		if r.Health == "stalled" {
			stalled++
		}
	}
	s := m.pal.faint.Render(fmt.Sprintf("%d runs", len(m.board.runs)))
	if needs > 0 {
		s += m.pal.faint.Render(" · ") + m.pal.chip(fmt.Sprintf("%d needs you", needs), m.pal.statusColor("awaiting_approval", ""))
	}
	if stalled > 0 {
		s += m.pal.faint.Render(" · ") + lipgloss.NewStyle().Foreground(m.pal.statusStalled).Render(fmt.Sprintf("%d stalled", stalled))
	}
	return s
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
				return m.pal.faint.Render("No active runs — finished runs are hidden (h to show).") + "\n"
			}
		}
	}
	if m.board.admin {
		return m.pal.faint.Render("No active runs across the factory right now.") + "\n"
	}
	return m.pal.faint.Render("No runs yet. Start one from the web board or the command line.") + "\n"
}

// boardRow renders one run: a status-coloured spine (with a NO_COLOR-safe glyph), the id,
// OWNER on the admin board, a status chip, a health marker, AGE, and the title with an
// own-board-only judge marker.
func (m tuiModel) boardRow(r apitypes.RunListItemDTO, sel bool, mc boardMarkerCols) string {
	es := effectiveRunStatus(r.Status, r.IsPlanning)
	sc := m.pal.statusColor(es, r.Health)

	// The spine carries the status GLYPH in the chip foreground on the status-colour
	// background. Under NO_COLOR / an Ascii profile the fill is stripped but the glyph
	// survives, so the spine's signal (D3) does not depend on colour.
	spine := lipgloss.NewStyle().Background(sc).Foreground(m.pal.chipFg).Render(statusGlyph(es))
	gutter := " "
	id := m.pal.faint.Render(padCell(shortRunID(r.ID), 9))
	if sel {
		gutter = m.pal.sel.Render("▸")
		id = m.pal.sel.Render(padCell(shortRunID(r.ID), 9))
	}

	owner := ""
	if m.board.admin {
		// D7 (PRD #325 M2, B1): OwnerEmail is forge-authored untrusted text — route it
		// through renderer.Plain (= capCell(cellText(...))); bare capCell would not strip
		// control bytes. OwnerEmail is in d7UntrustedFields. F1: pad to the column so the
		// admin rows and header align (Plain truncates but does not pad).
		owner = "  " + m.pal.faint.Render(padCell(m.renderer.Plain(strOr(r.OwnerEmail, ""), boardOwnerWidth), boardOwnerWidth))
	}

	statusCell := padVisual(m.pal.chip(m.renderer.Plain(es, boardStatusWidth-2), sc), boardStatusWidth)

	// MILE: the compact milestone badge (M{done}/{total}) in its own fixed column between AGE
	// and TITLE, blank for a run with no frozen list. A fixed column (not a float after the
	// title) keeps the badge in one place down BOTH boards — milestones are informational,
	// unlike the own-board judge marker that trails the title. padVisual holds the column
	// width even when the badge is empty, so TITLE stays aligned across rows. Dropped entirely
	// on a narrow terminal (boardShowMile), where its 8-col tax would clip the judge marker.
	mileCell := ""
	if m.boardShowMile() {
		mileCell = padVisual(m.milestoneMarker(r), boardMileWidth) + "  "
	}

	// F2 + title trim: cap the title to the remaining width (so no row exceeds boardRuleWidth
	// and a narrow terminal does not wrap) AND to boardTitleMax (so a very long title is
	// trimmed to a tidy column rather than running the full width of a wide terminal). The
	// marker column allowance (its full width + a 2-col minimum gap) is reserved for EVERY row
	// so the title column keeps a single width down the board.
	markerAllow := 0
	if mc.fullW > 0 {
		markerAllow = mc.fullW + 2
	}
	avail := boardRuleWidth(m.width) - boardRowPrefixWidth(m.board.admin, m.boardShowMile()) - markerAllow
	if avail < 10 {
		avail = 10
	}
	if avail > boardTitleMax {
		avail = boardTitleMax
	}
	titleStr := padVisual(m.renderer.Plain(runTitle(r.RunDTO), avail), avail)

	row := spine + gutter + " " + id + owner + "  " + statusCell + "  " + m.boardHealthCell(r) + "  " +
		m.pal.faint.Render(padCell(relAge(r.CreatedAt), 5)) + "  " + mileCell + titleStr

	// F4: the judge marker (own board only; AdminListRuns carries no JudgeVerdict) is a
	// fixed-width block flushed to the board's RIGHT EDGE, so the ⚖ icon and the count align
	// down the board, in a column with the summary bar above. Every marker is mc.fullW wide, so
	// flushing the row to boardRuleWidth-fullW lands them all in the same columns.
	if mc.fullW > 0 && !m.board.admin && r.JudgeVerdict != nil {
		marker := m.verdictMarker(*r.JudgeVerdict, r.JudgeTodoCount, mc.verdictW, mc.countW)
		row = padVisual(row, boardRuleWidth(m.width)-mc.fullW) + marker
	}
	return row
}

// boardRowPrefixWidth is the visual width of every column before TITLE, so F2 can size the
// title to what remains. spine(1)+gutter(1)+space(1)+id(9), then two-space gaps around the
// STATUS, HEALTH and AGE cells, the MILE cell when shown, plus the admin OWNER cell.
func boardRowPrefixWidth(admin, mile bool) int {
	w := 3 + 9 + 2 + boardStatusWidth + 2 + boardHealthWidth + 2 + 5 + 2
	if mile {
		w += boardMileWidth + 2
	}
	if admin {
		w += 2 + boardOwnerWidth
	}
	return w
}

// boardHealthCell is the HEALTH column. "stalled" (which also turns the spine/chip orange
// via statusColor and is counted in the summary) keeps its ▲ glyph; every other non-ok
// health (slow / looping / waiting_worker) shows its WORD, as the pre-redesign board did,
// truncated to the column. ok/empty is blank. The cell is always boardHealthWidth wide so
// AGE and TITLE stay aligned.
func (m tuiModel) boardHealthCell(r apitypes.RunListItemDTO) string {
	switch h := boardHealth(r); h {
	case "":
		return strings.Repeat(" ", boardHealthWidth)
	case "stalled":
		return padVisual(lipgloss.NewStyle().Foreground(m.pal.statusStalled).Render("▲"), boardHealthWidth)
	default:
		// The word, sanitized + capped (Health is server-controlled, but drawn defensively
		// like every other cell), in the default colour — visible, not faint.
		return padCell(m.renderer.Plain(h, boardHealthWidth), boardHealthWidth)
	}
}

// boardMarkerCols are the fixed sub-column widths of the judge marker across the visible rows:
// verdictW for the variable-length verdict WORD (ideal/ok/issues) and countW for the "· N"
// recommendation count. fullW is the whole marker's width. Splitting the marker into fixed
// sub-columns is what keeps the ⚖ icon and the count in stable columns while only the verdict
// word moves within its own slot (the count is short and fixed, the verdict is not). All zero
// on the admin board (no verdicts) or when no visible row carries a verdict.
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
		mc.fullW = visualWidth(m.verdictMarker("", 0, mc.verdictW, mc.countW))
	}
	return mc
}

// verdictMarker is the own-board ⚖ judge badge, coloured by severity (issues → red, ideal/ok →
// teal). It lays the verdict WORD out LEFT-aligned in a fixed verdictW slot and the "· N"
// recommendation count in a fixed countW slot after it, so down the board the ⚖ icon and the
// count align and only the verdict word shifts within its slot. The verdict is a closed enum
// but rendered through Plain so an unrecognised value from a newer server cannot inject control
// bytes. A run with no open recommendations (JudgeTodoCount 0) leaves the count slot blank.
func (m tuiModel) verdictMarker(verdict string, todo, verdictW, countW int) string {
	c := m.pal.verdictColor(verdict)
	label := "⚖ " + padVisual(m.renderer.Plain(verdict, 8), verdictW)
	if countW > 0 {
		count := ""
		if todo > 0 {
			count = "· " + itoa(todo)
		}
		label += " " + padVisual(count, countW)
	}
	return lipgloss.NewStyle().Foreground(c).Render(label)
}

// milestoneMarker is the board's compact milestone badge — `M{done}/{total}` (or `M–/{total}`
// when nothing was reported), the TUI twin of the web's MilestoneBadge. Empty for a run with
// no frozen milestone list, so a non-milestone row is byte-for-byte unchanged. It carries no
// untrusted text (only counts), so it needs no D7 sanitizing. Drawn faint: milestones are an
// at-a-glance progress read, not an attention signal like the judge marker or a stalled row.
func (m tuiModel) milestoneMarker(r apitypes.RunListItemDTO) string {
	done, total, reported := milestoneProgress(r.RunDTO)
	if total == 0 {
		return ""
	}
	return m.pal.faint.Render("M" + milestoneCount(done, total, reported))
}

// statusGlyph is the per-status spine marker. It is the NO_COLOR fallback (D3), so it must
// read without colour and be a single terminal cell wide.
func statusGlyph(status string) string {
	switch status {
	case "running":
		return "●"
	case "planning":
		return "○"
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
	default: // queued, claimed, cancelled, unknown
		return "·"
	}
}

// boardHealth renders the PRD #47 health flag, blank when healthy so the column is
// quiet in the normal case and an anomaly stands out.
func boardHealth(r apitypes.RunListItemDTO) string {
	if r.Health == "" || r.Health == "ok" {
		return ""
	}
	return r.Health
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
