package main

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	"gitlab.example.com/vtmocanu/uzi/api/internal/apitypes"
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
			r.ID, r.Kind, r.Status, cellText(runTitle(r.RunDTO)),
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

	title := "runs"
	if m.board.admin {
		// The admin view is NOT symmetric with own-runs: AdminListRuns returns
		// non-terminal runs only, capped, with no judge/usage columns. Labelling it
		// "active runs" is the honest header — promising completed rows here would be
		// a lie the API cannot satisfy.
		title = "active runs (factory-wide)"
	}
	sb.WriteString(m.pal.title.Render("uzi · " + title))
	if m.board.filter != "" || m.board.filtering {
		sb.WriteString(m.pal.faint.Render("   /" + cellText(m.board.filter)))
		if m.board.filtering {
			sb.WriteString(m.pal.faint.Render("▌"))
		}
	}
	sb.WriteString("\n\n")

	if m.board.adminDenied {
		sb.WriteString(m.pal.faint.Render("the factory-wide board needs an admin (uza_) token — showing your runs\n\n"))
	}
	if m.board.err != nil {
		sb.WriteString(m.pal.faint.Render("could not refresh: "+fmtErr(m.board.err)) + "\n\n")
	}

	rows := m.board.visible()
	if len(rows) == 0 {
		sb.WriteString(m.pal.faint.Render("no runs to show"))
		return sb.String()
	}

	// Fixed-width cells: every untrusted value goes through the Plain path (cellText +
	// rune cap), never sanitizeTTY alone, or a newline in a title breaks the column.
	sb.WriteString(m.pal.faint.Render(padCell("RUN", 10)+"  "+padCell("KIND", 12)+"  "+
		padCell("STATUS", 18)+"  "+padCell("HEALTH", 10)+"  TITLE") + "\n")
	for i, r := range rows {
		line := padCell(shortRunID(r.ID), 10) + "  " +
			padCell(m.renderer.Plain(r.Kind, 12), 12) + "  " +
			padCell(m.renderer.Plain(r.Status, 18), 18) + "  " +
			padCell(m.renderer.Plain(boardHealth(r), 10), 10) + "  " +
			m.renderer.Plain(runTitle(r.RunDTO), 60)
		if i == m.board.cursor {
			sb.WriteString(m.pal.sel.Render("▸ " + line))
		} else {
			sb.WriteString("  " + line)
		}
		sb.WriteString("\n")
	}
	sb.WriteString("\n" + m.pal.faint.Render("enter open · / filter · a admin · r refresh · ? keys · q quit"))
	return sb.String()
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
