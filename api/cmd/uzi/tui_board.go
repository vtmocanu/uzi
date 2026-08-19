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
	if strings.TrimSpace(b.filter) == "" {
		return b.runs
	}
	q := strings.ToLower(strings.TrimSpace(b.filter))
	out := make([]apitypes.RunListItemDTO, 0, len(b.runs))
	for _, r := range b.runs {
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
		return m, nil
	}

	if d := motionDelta(k); d != 0 {
		m.board.cursor += d
		m.board.clampCursor()
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
		return m, m.fetchRunsCmd(m.board.admin)
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
	if m.board.filter != "" || m.board.filtering {
		brand += m.pal.faint.Render("   /" + cellText(m.board.filter))
		if m.board.filtering {
			brand += m.pal.faint.Render("▌")
		}
	}
	summary := m.boardSummary()
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
		for i, r := range rows {
			sb.WriteString(m.boardRow(r, i == m.board.cursor) + "\n")
		}
	}

	sb.WriteString(m.pal.faint.Render(strings.Repeat("─", boardRuleWidth(m.width))) + "\n")
	sb.WriteString(m.pal.faint.Render("enter open · / filter · a admin · r refresh · ? keys · q quit"))
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
func (m tuiModel) boardShowMile() bool { return m.width >= boardMileMinWidth }

func boardRuleWidth(w int) int {
	if w < 2 {
		return 1
	}
	return w - 1
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
	if m.board.admin {
		return m.pal.faint.Render("No active runs across the factory right now.") + "\n"
	}
	return m.pal.faint.Render("No runs yet. Start one from the web board or the command line.") + "\n"
}

// boardRow renders one run: a status-coloured spine (with a NO_COLOR-safe glyph), the id,
// OWNER on the admin board, a status chip, a health marker, AGE, and the title with an
// own-board-only judge marker.
func (m tuiModel) boardRow(r apitypes.RunListItemDTO, sel bool) string {
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

	// F4: the judge marker carries the todo count when > 0 ("⚖ issues · N"), own board only
	// (AdminListRuns carries no JudgeVerdict). It trails the title.
	marker := ""
	if !m.board.admin && r.JudgeVerdict != nil {
		marker = "  " + m.verdictMarker(*r.JudgeVerdict, r.JudgeTodoCount)
	}
	// F2 + title trim: cap the title to the remaining width (so no row exceeds boardRuleWidth
	// and a narrow terminal does not wrap) AND to boardTitleMax (so a very long title is
	// trimmed to a tidy column rather than running the full width of a wide terminal). Pad to
	// that width so the trailing judge marker lines up into a column instead of floating after
	// each ragged title end.
	avail := boardRuleWidth(m.width) - boardRowPrefixWidth(m.board.admin, m.boardShowMile()) - visualWidth(marker)
	if avail < 10 {
		avail = 10
	}
	if avail > boardTitleMax {
		avail = boardTitleMax
	}
	title := padVisual(m.renderer.Plain(runTitle(r.RunDTO), avail), avail) + marker

	return spine + gutter + " " + id + owner + "  " + statusCell + "  " + m.boardHealthCell(r) + "  " +
		m.pal.faint.Render(padCell(relAge(r.CreatedAt), 5)) + "  " + mileCell + title
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

// verdictMarker is the own-board ⚖ judge badge, coloured by severity (issues → red,
// ideal/ok → teal). The verdict is a closed enum, but it is rendered through Plain so an
// unrecognised value from a newer server cannot inject control bytes. F4: it appends the
// still-to-triage recommendation count when > 0, the DTO's "⚖ issues · N" grammar
// (JudgeTodoCount, apitypes/run.go), so a healthy or fully-triaged run shows just the
// verdict.
func (m tuiModel) verdictMarker(verdict string, todo int) string {
	c := m.pal.verdictColor(verdict)
	label := "⚖ " + m.renderer.Plain(verdict, 8)
	if todo > 0 {
		label += " · " + itoa(todo)
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
