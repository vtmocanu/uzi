package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/spf13/cobra"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// `uzi tui` — the full-screen board + run detail (PRD #112 M3).
//
// It lives in package main as tui_*.go rather than in a tui/ subpackage, and that is
// a structural decision, not a filing one: all of api/cmd/uzi is package main, Go
// forbids importing a main package, and D6/D7 mandate reusing ~25 unexported helpers
// from here (sanitizeTTY, compactText, cellText, capCell, shortInstanceID, runTitle,
// relAge, …). In-package they are simply reachable, which makes "the TUI and the plain
// commands cannot drift" structurally true rather than a rule to remember. Extracting
// them instead would refactor shipped CLI code this PRD has no other reason to touch,
// and would drop a working gate: TestPrintedInstructionsAreRegistered parses the
// CURRENT directory only, so a subpackage escapes it.

// boardPollInterval is the board's ListRuns cadence (D3: the board polls, only the
// drilled-in run opens a socket). A per-run WS for a list-level counter is
// disproportionate.
var boardPollInterval = 2 * time.Second

// tuiView is which screen has focus.
type tuiView int

const (
	viewBoard tuiView = iota
	viewDetail
)

// ---- messages -------------------------------------------------------------

type boardRunsMsg struct {
	runs  []apitypes.RunListItemDTO
	admin bool
	err   error
}

type boardTickMsg struct{}

type detailLoadedMsg struct {
	run  apitypes.RunDTO
	msgs []apitypes.MessageDTO
	err  error
}

type streamReadyMsg struct {
	runID  string
	stream *uzicli.RunStream
	err    error
}

// streamEventsMsg carries a BATCH. The stream reader drains everything already queued
// before handing over, so a burst of frames costs one re-render rather than one per
// frame — the stated use case is SSH, where per-frame redraws are what makes a TUI
// feel laggy.
type streamEventsMsg struct {
	runID  string
	events []apitypes.RunEventDTO
	closed bool
	err    error
}

// pollFallbackMsg drives the D8 degradation: when the socket is unreachable the detail
// view falls back to the same 2s REST poll `uzi run logs --follow` uses.
type pollFallbackMsg struct{}

// ---- model ----------------------------------------------------------------

type tuiModel struct {
	client uzicli.Client
	ctx    context.Context

	width, height int
	dark          bool
	pal           palette
	renderer      *tuiRenderer

	view   tuiView
	board  boardState
	detail detailState

	// quitting is the confirm modal (both q and ctrl+c route through it); ctrlCSeen
	// makes a second ctrl+c quit immediately, which is the escape hatch a user reaches
	// for when the modal itself is what is wrong.
	quitting  bool
	ctrlCSeen bool
	showHelp  bool
}

func newTUIModel(ctx context.Context, c uzicli.Client, startRun string) tuiModel {
	m := tuiModel{
		client: c, ctx: ctx,
		width: 100, height: 30, dark: true,
		pal:  newPalette(true),
		view: viewBoard,
	}
	m.renderer, _ = newTUIRenderer(m.width, m.dark)
	m.board = newBoardState()
	if startRun != "" {
		m.view = viewDetail
		m.detail = newDetailState(startRun)
	}
	return m
}

func (m tuiModel) Init() tea.Cmd {
	cmds := []tea.Cmd{m.fetchRunsCmd(m.board.admin), tickCmd()}
	if m.view == viewDetail {
		cmds = append(cmds, m.loadDetailCmd(m.detail.runID), m.openStreamCmd(m.detail.runID))
	}
	return tea.Batch(cmds...)
}

func tickCmd() tea.Cmd {
	return tea.Tick(boardPollInterval, func(time.Time) tea.Msg { return boardTickMsg{} })
}

func (m tuiModel) fetchRunsCmd(admin bool) tea.Cmd {
	c, ctx := m.client, m.ctx
	return func() tea.Msg {
		var runs []apitypes.RunListItemDTO
		var err error
		if admin {
			runs, err = c.AdminListRuns(ctx)
		} else {
			runs, err = c.ListRuns(ctx)
		}
		return boardRunsMsg{runs: runs, admin: admin, err: err}
	}
}

func (m tuiModel) loadDetailCmd(runID string) tea.Cmd {
	c, ctx := m.client, m.ctx
	return func() tea.Msg {
		run, err := c.GetRun(ctx, runID)
		if err != nil {
			return detailLoadedMsg{err: err}
		}
		msgs, err := c.RunLogs(ctx, runID, 0)
		return detailLoadedMsg{run: run, msgs: msgs, err: err}
	}
}

func (m tuiModel) openStreamCmd(runID string) tea.Cmd {
	c, ctx := m.client, m.ctx
	return func() tea.Msg {
		s, err := c.StreamRun(ctx, runID)
		return streamReadyMsg{runID: runID, stream: s, err: err}
	}
}

// readStreamCmd blocks for one event then DRAINS whatever else is already queued, so
// a burst costs a single re-render.
func readStreamCmd(runID string, s *uzicli.RunStream) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-s.Events()
		if !ok {
			return streamEventsMsg{runID: runID, closed: true, err: s.Err()}
		}
		batch := []apitypes.RunEventDTO{ev}
		for {
			select {
			case next, ok := <-s.Events():
				if !ok {
					return streamEventsMsg{runID: runID, events: batch, closed: true, err: s.Err()}
				}
				batch = append(batch, next)
				continue
			default:
			}
			break
		}
		return streamEventsMsg{runID: runID, events: batch}
	}
}

func pollFallbackCmd() tea.Cmd {
	return tea.Tick(2*time.Second, func(time.Time) tea.Msg { return pollFallbackMsg{} })
}

func (m tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.renderer, _ = newTUIRenderer(m.transcriptWidth(), m.dark)
		return m, nil

	case tea.BackgroundColorMsg:
		// The theme comes from what the terminal reports, not from a package-level
		// probe at import time (which is what lipgloss v2's compat AdaptiveColor does,
		// and it fires even without a TTY).
		m.dark = msg.IsDark()
		m.pal = newPalette(m.dark)
		m.renderer, _ = newTUIRenderer(m.transcriptWidth(), m.dark)
		return m, nil

	case tea.KeyPressMsg:
		return m.handleKey(keyString(msg))

	case boardTickMsg:
		if m.quitting {
			return m, tickCmd()
		}
		return m, tea.Batch(m.fetchRunsCmd(m.board.admin), tickCmd())

	case boardRunsMsg:
		m.board.apply(msg)
		return m, nil

	case detailLoadedMsg:
		m.detail.applyLoaded(msg)
		// The ownership probe rides the same call the queue indicator needs.
		return m, m.fetchInputsCmd(m.detail.runID)

	case runInputsMsg:
		if msg.runID != m.detail.runID {
			return m, nil
		}
		m.detail.steer.access = steerAccessFor(m.detail.run, msg.err)
		if msg.err == nil {
			m.detail.steer.queue = msg.inputs
		}
		return m, nil

	case steerResultMsg:
		if msg.runID != m.detail.runID {
			return m, nil
		}
		m.applySteerResult(msg)
		// Re-read the queue so the indicator reflects the write immediately rather
		// than waiting for the run's next `input` frame.
		return m, m.fetchInputsCmd(m.detail.runID)

	case reviewLoadedMsg:
		if msg.runID != m.detail.runID {
			return m, nil
		}
		m.detail.review.loading = false
		m.detail.review.review, m.detail.review.err = msg.review, msg.err
		// Assigned on EVERY load, including the nil case: a judge that finished (or
		// died) between two loads must clear the pending copy, not leave a stale "a
		// judge is in progress" over a verdict that already landed.
		m.detail.review.pendingJudge = msg.pendingJudge
		return m, nil

	case dispositionDoneMsg:
		if msg.runID != m.detail.runID {
			return m, nil
		}
		if msg.err != nil {
			m.detail.review.notice = "could not record that: " + fmtErr(msg.err)
			return m, nil
		}
		m.detail.review.notice = "triage recorded"
		m.detail.review.loading = true
		return m, m.loadReviewCmd(m.detail.runID)

	case streamReadyMsg:
		if msg.runID != m.detail.runID {
			if msg.stream != nil {
				msg.stream.Close()
			}
			return m, nil
		}
		if msg.err != nil {
			// D8: a socket we cannot open is a degradation, not a crash — fall back to
			// the same 2s REST poll `uzi run logs --follow` uses and say so on screen.
			m.detail.streamErr = msg.err
			m.detail.polling = true
			return m, pollFallbackCmd()
		}
		m.detail.stream = msg.stream
		m.detail.streamErr = nil
		m.detail.polling = false
		return m, readStreamCmd(msg.runID, msg.stream)

	case streamEventsMsg:
		if msg.runID != m.detail.runID {
			return m, nil
		}
		inputChanged := m.detail.applyEvents(msg.events)
		// A `state` frame is the retry trigger for a stuck ownership probe: if the
		// first probe failed for a reason that was not a 404, access is steerUnknown
		// and nothing else would ever ask again.
		if m.detail.steer.access == steerUnknown && hasStateFrame(msg.events) {
			return m, tea.Batch(readStreamCmd(msg.runID, m.detail.stream), m.fetchInputsCmd(m.detail.runID))
		}
		if inputChanged && m.detail.steer.access == steerAllowed {
			// PRD #95: an `input` frame says the steer queue changed (a follow-up was
			// consumed). It carries no data — it is a prompt to re-read — so the
			// indicator refreshes off it rather than guessing.
			return m, tea.Batch(readStreamCmd(msg.runID, m.detail.stream), m.fetchInputsCmd(m.detail.runID))
		}
		if msg.closed {
			m.detail.stream = nil
			m.detail.streamErr = msg.err
			m.detail.polling = true
			return m, pollFallbackCmd()
		}
		return m, readStreamCmd(msg.runID, m.detail.stream)

	case pollFallbackMsg:
		if m.view != viewDetail || !m.detail.polling {
			return m, nil
		}
		return m, tea.Batch(m.loadDetailCmd(m.detail.runID), pollFallbackCmd())
	}
	return m, nil
}

func (m tuiModel) handleKey(k string) (tea.Model, tea.Cmd) {
	// Quit routes through a confirm modal for BOTH q and ctrl+c, so a stray keystroke
	// cannot drop a watched run. A second ctrl+c quits at once — the escape hatch for
	// when the modal is what is broken.
	if k == keyCtrlC {
		if m.ctrlCSeen || m.quitting {
			return m, tea.Quit
		}
		m.ctrlCSeen = true
		m.quitting = true
		return m, nil
	}
	if m.quitting {
		switch k {
		case keyConfirmY, keyEnter:
			return m, tea.Quit
		default:
			m.quitting, m.ctrlCSeen = false, false
			return m, nil
		}
	}
	if m.showHelp {
		m.showHelp = false
		return m, nil
	}
	if k == keyQuit && !m.filtering() {
		m.quitting = true
		return m, nil
	}
	if k == keyHelp && !m.filtering() {
		m.showHelp = true
		return m, nil
	}

	switch m.view {
	case viewBoard:
		return m.boardKey(k)
	default:
		return m.detailKey(k)
	}
}

func (m tuiModel) filtering() bool {
	return m.view == viewBoard && m.board.filtering
}

func (m tuiModel) transcriptWidth() int {
	w := m.width - laneRailWidth - 6
	if w < 20 {
		w = 20
	}
	return w
}

func (m tuiModel) View() tea.View {
	var v tea.View
	// AltScreen is a VIEW field in bubbletea v2, not a program option.
	v.AltScreen = true
	v.WindowTitle = "uzi"

	var body string
	switch {
	case m.quitting:
		body = m.pal.box.Render("Quit uzi tui?  [y] quit   [any other key] stay")
	case m.showHelp:
		body = m.renderHelp()
	case m.view == viewDetail:
		body = m.renderDetail()
	default:
		body = m.renderBoard()
	}
	v.SetContent(body)
	return v
}

func (m tuiModel) renderHelp() string {
	lines := helpLines(m.view == viewDetail)
	return m.pal.title.Render("keybindings") + "\n\n" +
		strings.Join(lines, "\n") + "\n\n" +
		m.pal.faint.Render("any key returns")
}

// newTUICmd wires `uzi tui [run-id]`.
func newTUICmd(env Env, gf *globalFlags) *cobra.Command {
	var demo bool
	cmd := &cobra.Command{
		Use:   "tui [run-id]",
		Short: "Watch runs live in a full-screen terminal UI",
		Long: "Open a full-screen board of runs, drill into one to watch its agents work, " +
			"and follow the transcript live over the run stream. Read-only.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// A TUI needs a terminal. Degrade to a CLEAR MESSAGE naming the scriptable
			// alternatives rather than crashing or drawing escape codes into a pipe
			// (D8) — this is the path an agent hits when it runs `uzi tui` by mistake.
			if !env.StdoutTTY {
				return uzicli.Exitf(uzicli.ExitUsage,
					"this command needs an interactive terminal (stdout is not a TTY). "+
						"For scripting use `uzi run list --json` and `uzi run logs <id> --follow`.")
			}
			// --demo runs the interactive TUI over seeded fixtures with no server (PRD #325
			// M7): a hidden showcase that drives the SHIPPED views, so it cannot drift.
			if demo {
				return runTUIDemo(cmd.Context(), env)
			}
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			runID := ""
			if len(args) == 1 {
				runID = args[0]
			}
			m := newTUIModel(cmd.Context(), c, runID)
			p := tea.NewProgram(m, tea.WithContext(cmd.Context()),
				tea.WithInput(env.Stdin), tea.WithOutput(env.Stdout))
			if _, err := p.Run(); err != nil {
				return uzicli.Exitf(uzicli.ExitGeneric, "tui: %v", err)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&demo, "demo", false, "run a self-contained demo over seeded fixtures (no server)")
	_ = cmd.Flags().MarkHidden("demo")
	return cmd
}

// fmtErr renders an error as one screen line, sanitized like any other untrusted
// string — a server-supplied message can carry control bytes too.
func fmtErr(err error) string {
	if err == nil {
		return ""
	}
	return cellText(fmt.Sprintf("%v", err))
}

// hasStateFrame reports whether a batch carries an authoritative run-state frame.
func hasStateFrame(evs []apitypes.RunEventDTO) bool {
	for _, ev := range evs {
		if ev.Type == uzicli.RunEventTypeState {
			return true
		}
	}
	return false
}
