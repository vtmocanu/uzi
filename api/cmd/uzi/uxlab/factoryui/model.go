package factoryui

import (
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

type view int

const (
	viewBoard view = iota
	viewDetail
)

// liveTick is how often the demo appends a synthetic frame to a live run, so follow-live is
// actually drivable (the transcript grows under you).
const liveTick = 1200 * time.Millisecond

type tickMsg struct{}

func tickCmd() tea.Cmd {
	return tea.Tick(liveTick, func(time.Time) tea.Msg { return tickMsg{} })
}

// Model is the demo's bubbletea model. It drives the redesigned views over seeded fixtures
// — no API, no DB, no network. View() renders through the same pure funcs the static
// screenshots use, so the live demo and the mock PNGs are the same design.
type Model struct {
	runs   []Run
	cursor int // index into the CURRENTLY VISIBLE (filtered) rows
	view   view

	// detail is the opened run (its own copy, so the demo's live ticker can grow its
	// transcript without touching the board rows).
	detail  Run
	focus   int // FocusRail | FocusTranscript — which detail pane up/down drives
	selLane int
	scroll  int  // transcript lines scrolled UP from the bottom; 0 == bottom
	follow  bool // auto-tail the transcript as new frames arrive
	liveIdx int  // rotates the synthetic live frames

	admin     bool
	filtering bool
	filter    string

	reviewOpen   bool
	reviewCursor int
	showHelp     bool

	dark   bool
	pal    Palette
	width  int
	height int
}

// New builds the demo model with seeded runs and a default size (bubbletea sends the real
// size and the terminal background immediately on start).
func New() Model {
	return Model{
		runs:   SeedRuns(),
		view:   viewBoard,
		dark:   true,
		pal:    NewPalette(true),
		width:  100,
		height: 34,
	}
}

// Init starts the live-output ticker so follow-live has something to follow.
func (m Model) Init() tea.Cmd { return tickCmd() }

func (m Model) visible() []Run { return filterRows(m.runs, m.filter) }

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case tea.BackgroundColorMsg:
		m.dark = msg.IsDark()
		m.pal = NewPalette(m.dark)
		return m, nil
	case tickMsg:
		if m.view == viewDetail && !m.reviewOpen && isLive(m.detail.Status) {
			m.appendLiveFrame()
		}
		return m, tickCmd()
	case tea.KeyPressMsg:
		return m.handleKey(msg.String())
	}
	return m, nil
}

func (m Model) handleKey(k string) (tea.Model, tea.Cmd) {
	if k == "ctrl+c" || (k == "q" && !m.filtering) {
		return m, tea.Quit
	}
	if m.showHelp {
		m.showHelp = false
		return m, nil
	}
	if k == "?" && !m.filtering {
		m.showHelp = true
		return m, nil
	}
	if m.view == viewDetail {
		return m.detailKey(k)
	}
	return m.boardKey(k)
}

func (m Model) boardKey(k string) (tea.Model, tea.Cmd) {
	if m.filtering {
		switch k {
		case "enter", "esc":
			m.filtering = false
			if k == "esc" {
				m.filter = ""
			}
			m.clampCursor()
		case "backspace":
			if n := len([]rune(m.filter)); n > 0 {
				m.filter = string([]rune(m.filter)[:n-1])
			}
			m.clampCursor()
		default:
			if k == "space" {
				k = " "
			}
			if len([]rune(k)) == 1 {
				m.filter += k
				m.clampCursor()
			}
		}
		return m, nil
	}

	switch k {
	case "j", "down":
		m.cursor++
		m.clampCursor()
	case "k", "up":
		m.cursor--
		m.clampCursor()
	case "/":
		m.filtering = true
	case "a":
		m.admin = !m.admin
		m.cursor = 0
	case "enter":
		vis := m.visible()
		if m.cursor >= 0 && m.cursor < len(vis) {
			m.detail = vis[m.cursor]
			m.view = viewDetail
			// Open focused on the CREW rail — the first, leftmost pane — so ↑/↓ move
			// between agents by default; → moves focus to the transcript. The transcript
			// still follows live in the background (the FOLLOWING badge stays on).
			m.focus, m.selLane, m.scroll, m.follow = FocusRail, 0, 0, true
			m.reviewOpen, m.reviewCursor, m.liveIdx = false, 0, 0
		}
	}
	return m, nil
}

func (m Model) detailKey(k string) (tea.Model, tea.Cmd) {
	if m.reviewOpen {
		switch k {
		case "esc", "v":
			m.reviewOpen = false
		case "j", "down":
			m.reviewCursor++
			if n := len(SeedReview().Recommendations); m.reviewCursor >= n {
				m.reviewCursor = n - 1
			}
		case "k", "up":
			if m.reviewCursor > 0 {
				m.reviewCursor--
			}
		}
		return m, nil
	}
	switch k {
	case "esc":
		m.view = viewBoard
	case "v":
		m.reviewOpen = true
	case "g":
		// go live: re-attach follow and jump to the newest output.
		m.follow, m.scroll = true, 0
	case "tab":
		m.focus = 1 - m.focus
	case "left", "h":
		m.focus = FocusRail
	case "right", "l":
		m.focus = FocusTranscript
	case "j", "down":
		if m.focus == FocusRail {
			m.moveLane(1)
		} else {
			m.scrollDown()
		}
	case "k", "up":
		if m.focus == FocusRail {
			m.moveLane(-1)
		} else {
			m.scrollUp()
		}
	}
	return m, nil
}

// moveLane cycles the selected agent and resets the transcript to follow that lane's tail.
func (m *Model) moveLane(d int) {
	n := len(m.detail.Lanes)
	if n == 0 {
		return
	}
	m.selLane = (m.selLane + d + n) % n
	m.scroll, m.follow = 0, true
}

// scrollUp detaches follow (a reader who scrolls back is no longer tailing) and moves the
// window up one line.
func (m *Model) scrollUp() {
	if m.follow {
		m.follow, m.scroll = false, 1
	} else {
		m.scroll++
	}
	m.clampScroll()
}

// scrollDown moves the window toward the newest output, re-attaching follow on a live run
// once it reaches the bottom.
func (m *Model) scrollDown() {
	if m.follow {
		return
	}
	if m.scroll > 0 {
		m.scroll--
	}
	if m.scroll == 0 && isLive(m.detail.Status) {
		m.follow = true
	}
}

func (m *Model) clampScroll() {
	total, vp := TranscriptExtent(m.pal, m.detail, m.selLane, m.height)
	maxScroll := total - vp
	if maxScroll < 0 {
		maxScroll = 0
	}
	if m.scroll > maxScroll {
		m.scroll = maxScroll
	}
	if m.scroll < 0 {
		m.scroll = 0
	}
}

// appendLiveFrame simulates one new frame of live output on the selected lane. When paused,
// it nudges the scroll offset so the reader's window stays anchored and the "new below"
// counter grows, exactly as a real tail buffer behaves.
func (m *Model) appendLiveFrame() {
	role := "lead"
	if m.selLane >= 0 && m.selLane < len(m.detail.Lanes) {
		role = m.detail.Lanes[m.selLane].Role
	}
	before, _ := TranscriptExtent(m.pal, m.detail, m.selLane, m.height)
	ln := liveLines[m.liveIdx%len(liveLines)]
	m.liveIdx++
	m.detail.Transcript = append(m.detail.Transcript, Frame{Role: role, Kind: ln.kind, Text: ln.text})
	if !m.follow {
		after, _ := TranscriptExtent(m.pal, m.detail, m.selLane, m.height)
		m.scroll += after - before
	}
}

type liveLine struct{ kind, text string }

var liveLines = []liveLine{
	{"tool_use", "Reading api/internal/poller/scheduler.go"},
	{"text", "Backoff applied; the near-cap branch now clamps at 10%."},
	{"tool_use", "Running go test ./internal/poller/..."},
	{"text", "Tests green. Preparing the diff for review."},
	{"tool_use", "Editing scheduler_test.go, adding the near-cap case."},
}

func (m *Model) clampCursor() {
	n := len(m.visible())
	if m.cursor >= n {
		m.cursor = n - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
}

func (m Model) View() tea.View {
	var v tea.View
	v.AltScreen = true
	v.WindowTitle = "uzi · demo"
	v.SetContent(m.Body())
	return v
}

// Body is View's content as a plain string — the same render funcs the static screenshots
// use, so the live demo and the mock PNGs draw the identical design.
func (m Model) Body() string {
	switch {
	case m.showHelp:
		return m.renderHelp()
	case m.view == viewDetail && m.reviewOpen:
		return RenderReview(m.pal, m.detail, SeedReview(), m.reviewCursor, m.width)
	case m.view == viewDetail:
		return RenderDetail(m.pal, m.detail, m.focus, m.selLane, m.scroll, m.follow, m.width, m.height)
	default:
		return RenderBoard(m.pal, m.runs, m.cursor, m.admin, m.filtering, m.filter, m.width)
	}
}

func (m Model) renderHelp() string {
	p := m.pal
	lines := []string{
		p.keycap("j / ↓", "down") + "        move down (board rows · detail: within the focused pane)",
		p.keycap("k / ↑", "up") + "          move up",
		p.keycap("enter", "open") + "        open the selected run",
		p.keycap("← / →", "focus") + "       focus the crew rail / the transcript (detail)",
		p.keycap("tab", "cycle") + "         cycle the focused pane (detail)",
		p.keycap("g", "go live") + "         re-attach follow and jump to newest (detail)",
		p.keycap("v", "review") + "          judge review overlay (detail)",
		p.keycap("/", "filter") + "          filter the board",
		p.keycap("a", "all crews") + "       factory-wide board",
		p.keycap("esc", "back") + "          back out / close",
		p.keycap("q", "quit") + "            quit",
	}
	return p.fg(p.brand).Bold(true).Render("uzi · keybindings") + "\n\n" +
		strings.Join(lines, "\n") + "\n\n" + p.fg(p.faint).Render("any key returns")
}

var _ tea.Model = Model{}
