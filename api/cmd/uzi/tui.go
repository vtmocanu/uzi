package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
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

// boardPollTimeout bounds a single periodic board / detail-meta poll independently of the
// shared 30s http.Client.Timeout (client.go), which is right for one-shot CLI calls but far
// too long for a 2s-cadence poll (PRD #1130 D3). A var (not const) so a test can shrink it.
var boardPollTimeout = 10 * time.Second

// rateLimitPollInterval is the strip's own cadence: re-fetch the per-token meters and
// settings on ~60s, matching the web sidebar's useMyRateLimits(60_000). The server
// recomputes meters only every ~5m (UZI_USAGE_POLL_INTERVAL), so polling faster
// re-serves the same value. A var (not const) so a test can shrink it.
var rateLimitPollInterval = 60 * time.Second

// skewPollInterval is the version-skew banner's own cadence: re-probe the server's build
// version every few minutes so a server rolling forward while the TUI is open lights the
// banner within one interval, without probing on the 2s board tick. A var (not const) so a
// test can shrink it.
var skewPollInterval = 5 * time.Minute

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

// stripTickMsg fires on the 60s rateLimitPollInterval to refresh the rate-limit strip's
// meters + settings, independently of the 2s boardTickMsg runs cadence.
type stripTickMsg struct{}

// skewTickMsg fires on the skewPollInterval to re-probe the server's build version so the
// footer skew banner lights within one interval of a server rolling forward, independently
// of the 2s boardTickMsg runs cadence.
type skewTickMsg struct{}

// blinkTickMsg fires on the 500ms blinkInterval to toggle the in-progress milestone cell
// (PRD #1064 D4). Unlike the board/strip/skew tickers it is NOT armed at Init: it is started
// only when the board (or the drilled-in run) first holds a visible, non-terminal run with a
// non-empty MilestonesInProgress (maybeArmBlink), and re-arms itself from this message only
// while such a run remains — so an idle board wakes on nothing.
type blinkTickMsg struct{}

// secretsMsg carries the viewer's Anthropic token count (from ListSecrets), fetched once
// at Init to gate the board credential column on PRD #295's more-than-one-token rule.
type secretsMsg struct {
	count int
	err   error
}

// rateLimitsMsg carries the viewer's own per-token rate-limit meters (from SelfRateLimits),
// which drive the factory-floor rate-limit strip. Fetched at Init, on the 60s strip ticker
// (stripTickMsg), and on manual refresh.
type rateLimitsMsg struct {
	tokens []apitypes.TokenRateLimitDTO
	err    error
}

// settingsMsg carries the viewer's own non-secret settings (from GetMySettings); only
// SidebarTokenIds is used, to mirror the web sidebar's non-default-token selection.
type settingsMsg struct {
	settings apitypes.UserSettingsDTO
	err      error
}

// buildInfoMsg carries the connected server's build version (from BuildInfo), which drives
// the board footer's CLI-vs-server skew banner. Fetched at Init and on the skewTickMsg
// ticker when the session is allowed to probe (see tuiModel.skewCheck).
type buildInfoMsg struct {
	version string
	err     error
}

type detailLoadedMsg struct {
	runID string
	run   apitypes.RunDTO
	msgs  []apitypes.MessageDTO
	err   error
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

// detailMetaMsg carries a fresh run DTO for the drilled-in run, so the detail view's
// non-streamed fields (milestones, health, kind, title, duration) stay current while the
// live socket is connected. The socket only carries transcript frames and status, so
// without this the milestone checklist is frozen at open-time — the board badge polls, the
// detail did not.
type detailMetaMsg struct {
	runID string
	run   apitypes.RunDTO
	err   error
}

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

	// quitting is the ctrl+c confirm modal (q quits immediately and does NOT route through
	// it); ctrlCSeen makes a second ctrl+c quit immediately, which is the escape hatch a user
	// reaches for when the modal itself is what is wrong.
	quitting  bool
	ctrlCSeen bool
	showHelp  bool

	// tokenCount is how many Anthropic tokens the viewer holds (from ListSecrets, fetched
	// once at Init). It gates the board's credential column exactly as the web RunsList
	// does (PRD #295): the own board shows WHICH token a run spent only when there is more
	// than one to disambiguate. 0 until the probe returns, so the column stays hidden until
	// then rather than flashing in.
	tokenCount int

	// profile is the terminal's colour profile (tea.ColorProfileMsg, set at program
	// start). It gates OSC-8 hyperlink emission: links are emitted only at ANSI or
	// richer, so a NO_COLOR/Ascii terminal gets plain #<iid> text (the colorprofile
	// Writer strips SGR under Ascii but passes OSC-8 through unchanged, so links must
	// self-gate). Defaults to TrueColor so the first frame and untouched test models
	// emit links, mirroring the dark:true default.
	profile colorprofile.Profile

	// rateLimits and sidebarTokenIds drive the factory-floor rate-limit strip
	// (mirrors the web sidebar selection: default token + sidebar_token_ids,
	// status=="ok"). Fetched at Init, on the 60s strip ticker (stripTickMsg), and on
	// manual refresh (r) — NOT on the 2s board tick: a meter changes at most once per
	// ~5m server poll, so 60s already re-serves the same value and the 2s runs cadence
	// would only multiply the API load. A fetch failure is swallowed: the strip just hides.
	rateLimits      []apitypes.TokenRateLimitDTO
	sidebarTokenIds []string

	// serverVersion holds the connected server's build version for the footer version readout
	// (issue #687). Stored raw (unsanitized, last-known-good); sanitized at draw time via
	// cellText before it reaches the readout (versionReadout / versionClientOnly). Empty until
	// the first probe returns.
	serverVersion string

	// skewCheck is whether this session may auto-probe the server version (fetchBuildInfoCmd +
	// the skew ticker). It mirrors the CLI's versionCheckEnabled + stamped-build gate, and is
	// set only on the real run path — --demo and direct newTUIModel test construction leave it
	// false, so neither auto-probes. Whether the footer readout RENDERS is a separate gate
	// (showVersion below): a stamped build both probes and shows; a dev build shows `dev`
	// without probing.
	skewCheck bool

	// showVersion is the off-switch gate ALONE (= versionCheckEnabled: the injection seam,
	// --quiet, and UZI_VERSION_CHECK=0) and governs whether the footer version readout
	// renders at all. It is deliberately split from skewCheck, which additionally requires a
	// stamped build: a `go build` binary must still SHOW its `dev` version in the footer even
	// though it never auto-probes. The auto-probe stays gated by skewCheck (= showVersion &&
	// stamped build). --demo and direct newTUIModel test construction leave this false.
	showVersion bool

	// blinkOn is the current phase of the 500ms in-progress-cell blink (PRD #1064 D4): the
	// board micro-bar's and the crew rail's in-progress cell render ▰ when true, ▱ when false.
	// It starts false so a single non-tty/offline render (the tui-ux renderer, the #1061 sketch
	// harness) shows the STATIC frame, and it only ever toggles while a blink tick is armed.
	blinkOn bool
	// blinkArmed is whether a 500ms blinkTickMsg is currently scheduled. It guards against a 2s
	// board refresh that reveals an in-progress run stacking a second tick on top of the one
	// already running (double renders): the tick re-arms itself only from its own message, and
	// maybeArmBlink starts one only when none is armed.
	blinkArmed bool
	// noBlink pins the static frame (UZI_TUI_NO_BLINK=1, the reduced-motion opt-out), read
	// ONCE at model init with the CLI's os.Getenv idiom — never per render. When set the blink
	// tick is never armed and blinkOn stays false.
	noBlink bool
}

func newTUIModel(ctx context.Context, c uzicli.Client, startRun string) tuiModel {
	m := tuiModel{
		client: c, ctx: ctx,
		width: 100, height: 30, dark: true,
		profile: colorprofile.TrueColor,
		pal:     newPalette(true),
		view:    viewBoard,
	}
	m.renderer, _ = newTUIRenderer(m.width, m.dark)
	m.board = newBoardState()
	if startRun != "" {
		m.view = viewDetail
		m.detail = newDetailState(startRun)
	}
	return m
}

// initCmds is the pre-batch Cmd slice, extracted from Init so a test can inspect the
// startup commands without executing any (some members block on tea.Tick).
// tea.RequestBackgroundColor is the bare Cmd value (a func() Msg): issuing it from
// startup makes the terminal report its background colour, which drives the
// tea.BackgroundColorMsg handler in Update that flips m.dark and rebuilds the palette —
// so a light terminal actually gets the light theme instead of the dark default.
func (m tuiModel) initCmds() []tea.Cmd {
	cmds := []tea.Cmd{m.fetchRunsCmd(m.board.admin), m.fetchSecretsCmd(),
		m.fetchRateLimitsCmd(), m.fetchSettingsCmd(), tickCmd(), stripTickCmd(),
		tea.RequestBackgroundColor}
	if m.skewCheck {
		cmds = append(cmds, m.fetchBuildInfoCmd(), skewTickCmd())
	}
	if m.view == viewDetail {
		cmds = append(cmds, m.loadDetailCmd(m.detail.runID), m.openStreamCmd(m.detail.runID))
	}
	return cmds
}

func (m tuiModel) Init() tea.Cmd { return tea.Batch(m.initCmds()...) }

func tickCmd() tea.Cmd {
	return tea.Tick(boardPollInterval, func(time.Time) tea.Msg { return boardTickMsg{} })
}

func stripTickCmd() tea.Cmd {
	return tea.Tick(rateLimitPollInterval, func(time.Time) tea.Msg { return stripTickMsg{} })
}

func skewTickCmd() tea.Cmd {
	return tea.Tick(skewPollInterval, func(time.Time) tea.Msg { return skewTickMsg{} })
}

// blinkInterval is the in-progress-cell blink cadence (PRD #1064 D4). A var (not const) so a
// test can shrink it.
var blinkInterval = 500 * time.Millisecond

func blinkCmd() tea.Cmd {
	return tea.Tick(blinkInterval, func(time.Time) tea.Msg { return blinkTickMsg{} })
}

// blinkWanted reports whether the in-progress cell should be animating: at least one VISIBLE,
// non-terminal run — on the board, or the drilled-in run — carries a non-empty
// MilestonesInProgress. Always false under UZI_TUI_NO_BLINK, which pins the static frame.
func (m tuiModel) blinkWanted() bool {
	if m.noBlink {
		return false
	}
	for _, r := range m.board.visible() {
		if !terminalRunStatuses[r.Status] && len(r.MilestonesInProgress) > 0 {
			return true
		}
	}
	if m.view == viewDetail && !terminalRunStatuses[m.detail.run.Status] &&
		len(m.detail.run.MilestonesInProgress) > 0 {
		return true
	}
	return false
}

// maybeArmBlink starts the 500ms blink tick when the model now holds an in-progress run and no
// tick is already scheduled, marking the model armed. It is the ONLY place a tick is started
// from outside the tick's own message; blinkArmed is what stops a 2s board refresh from
// stacking a second tick.
func (m *tuiModel) maybeArmBlink() tea.Cmd {
	if m.blinkArmed || !m.blinkWanted() {
		return nil
	}
	m.blinkArmed = true
	return blinkCmd()
}

func (m tuiModel) fetchRunsCmd(admin bool) tea.Cmd {
	c, parent := m.client, m.ctx
	return func() tea.Msg {
		// Per-poll deadline (PRD #1130 D3): a stalled poll fails within boardPollTimeout
		// instead of the shared 30s http.Client.Timeout. WithTimeout + defer cancel() live
		// INSIDE the closure so the cancel fires when the poll returns, not immediately.
		ctx, cancel := context.WithTimeout(parent, boardPollTimeout)
		defer cancel()
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

// fetchSecretsCmd reads the viewer's Anthropic tokens once so the board can gate the
// credential column on holding more than one (PRD #295). A failure is swallowed — the
// column just stays hidden, and never blocks the board.
func (m tuiModel) fetchSecretsCmd() tea.Cmd {
	c, ctx := m.client, m.ctx
	return func() tea.Msg {
		secrets, err := c.ListSecrets(ctx)
		return secretsMsg{count: len(secrets), err: err}
	}
}

// fetchRateLimitsCmd reads the viewer's own per-token rate-limit meters so the board can
// draw the factory-floor rate-limit strip. A failure is swallowed — the strip just hides,
// and never blocks the board.
func (m tuiModel) fetchRateLimitsCmd() tea.Cmd {
	c, ctx := m.client, m.ctx
	return func() tea.Msg {
		tokens, err := c.SelfRateLimits(ctx)
		return rateLimitsMsg{tokens: tokens, err: err}
	}
}

// fetchSettingsCmd reads the viewer's own settings so the strip mirrors the web sidebar's
// non-default-token selection (sidebar_token_ids). Swallowed on failure like the meters.
func (m tuiModel) fetchSettingsCmd() tea.Cmd {
	c, ctx := m.client, m.ctx
	return func() tea.Msg {
		s, err := c.GetMySettings(ctx)
		return settingsMsg{settings: s, err: err}
	}
}

// fetchBuildInfoCmd reads the connected server's build version so the board footer can
// show the CLI-vs-server skew banner. A failure is swallowed — the banner just keeps its
// last value (or stays hidden), and never blocks the board.
func (m tuiModel) fetchBuildInfoCmd() tea.Cmd {
	c, ctx := m.client, m.ctx
	return func() tea.Msg {
		info, err := c.BuildInfo(ctx)
		return buildInfoMsg{version: info.Version, err: err}
	}
}

func (m tuiModel) loadDetailCmd(runID string) tea.Cmd {
	c, ctx := m.client, m.ctx
	return func() tea.Msg {
		run, err := c.GetRun(ctx, runID)
		if err != nil {
			return detailLoadedMsg{runID: runID, err: err}
		}
		msgs, err := c.RunLogs(ctx, runID, 0)
		return detailLoadedMsg{runID: runID, run: run, msgs: msgs, err: err}
	}
}

// refreshRunMetaCmd re-reads only the run DTO (no transcript replay), so the periodic
// detail refresh is cheap: the socket already carries the frames, this just refreshes the
// milestone / health / duration fields the stream does not send.
func (m tuiModel) refreshRunMetaCmd(runID string) tea.Cmd {
	c, parent := m.client, m.ctx
	return func() tea.Msg {
		// Per-poll deadline (PRD #1130 D3): same short bound as the board poll, derived
		// inside the closure so defer cancel() fires on return rather than immediately.
		ctx, cancel := context.WithTimeout(parent, boardPollTimeout)
		defer cancel()
		run, err := c.GetRun(ctx, runID)
		return detailMetaMsg{runID: runID, run: run, err: err}
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

	case tea.ColorProfileMsg:
		m.profile = msg.Profile
		return m, nil

	case tea.KeyPressMsg:
		return m.handleKey(keyString(msg))

	case boardTickMsg:
		if m.quitting {
			return m, tickCmd()
		}
		// The in-flight guard (PRD #1130 M1 D1): a periodic tick never starts a new board
		// poll while one is still pending, so a link where a ListRuns takes longer than the
		// 2s cadence cannot pile up overlapping concurrent requests. The pending poll's reply
		// clears the marker (boardRunsMsg case). The tick is ALWAYS re-armed regardless.
		cmds := []tea.Cmd{tickCmd()}
		if !m.board.boardInFlight {
			m.board.boardInFlight = true
			cmds = append(cmds, m.fetchRunsCmd(m.board.admin))
		}
		// Keep the drilled-in run's non-streamed fields (milestones, health, duration) fresh
		// on the same 2s cadence the board polls at: the live socket carries transcript frames
		// and status only. Skipped while the D8 fallback (pollFallbackMsg) is already reloading
		// the whole DTO every 2s, so the two never double up — and guarded by its own in-flight
		// marker so a slow meta poll does not stack either.
		if m.view == viewDetail && !m.detail.polling && m.detail.run.ID != "" && !m.detail.metaInFlight {
			m.detail.metaInFlight = true
			cmds = append(cmds, m.refreshRunMetaCmd(m.detail.runID))
		}
		return m, tea.Batch(cmds...)

	case stripTickMsg:
		if m.quitting {
			return m, stripTickCmd()
		}
		return m, tea.Batch(m.fetchRateLimitsCmd(), m.fetchSettingsCmd(), stripTickCmd())

	case skewTickMsg:
		if m.quitting {
			return m, skewTickCmd()
		}
		return m, tea.Batch(m.fetchBuildInfoCmd(), skewTickCmd())

	case boardRunsMsg:
		// Clear the in-flight guard on EVERY board reply (PRD #1130 M1 D2), independently of
		// apply — which early-returns on an admin/own-runs mismatch (tui_board.go). A clear
		// placed behind that early return would leave the marker latched on a mid-flight admin
		// toggle, wedging the periodic poll.
		m.board.boardInFlight = false
		m.board.apply(msg)
		// A refresh that first reveals a visible in-progress run arms the blink; blinkArmed
		// keeps a later refresh from stacking a second tick.
		return m, m.maybeArmBlink()

	case blinkTickMsg:
		if !m.blinkWanted() {
			// Nothing in progress any more (or the blink is disabled): drop to the static
			// frame and let the tick lapse — it re-arms only when an in-progress run reappears.
			m.blinkArmed = false
			m.blinkOn = false
			return m, nil
		}
		m.blinkOn = !m.blinkOn
		return m, blinkCmd()

	case secretsMsg:
		if msg.err == nil {
			m.tokenCount = msg.count
		}
		return m, nil

	case rateLimitsMsg:
		if msg.err == nil {
			m.rateLimits = msg.tokens
		}
		return m, nil

	case settingsMsg:
		if msg.err == nil {
			m.sidebarTokenIds = msg.settings.SidebarTokenIds
		}
		return m, nil

	case buildInfoMsg:
		// Store ONLY on a successful, non-empty probe: preserve the last-known-good server
		// version on an empty-but-successful reply, mirroring RecordServerVersion.
		if msg.err == nil && msg.version != "" {
			m.serverVersion = msg.version
		}
		return m, nil

	case detailLoadedMsg:
		// Drop a load that resolved for a run the user has since navigated away from:
		// esc/exitToBoard resets m.detail to its zero value (runID "", nil `seen` map), and
		// drilling into another run swaps runID — a late applyLoaded from the previous run
		// would splice the wrong transcript in, and against the zero value it would write the
		// nil map and panic. Same runID guard every sibling detail message carries. runID is
		// set by loadDetailCmd on both the success and error paths; run.ID is the fallback for
		// a message that carries only the run (the error path leaves run zero, which is why the
		// explicit field exists).
		id := msg.runID
		if id == "" {
			id = msg.run.ID
		}
		if id != m.detail.runID {
			return m, nil
		}
		m.detail.applyLoaded(msg)
		// The ownership probe rides the same call the queue indicator needs; a milestone run
		// that opens with work in progress arms the blink too.
		return m, tea.Batch(m.fetchInputsCmd(m.detail.runID), m.maybeArmBlink())

	case detailMetaMsg:
		// Clear the in-flight guard at the TOP of the case, ABOVE the guard (PRD #1130 M1 D2):
		// this case early-returns on runID-mismatch OR err != nil, and the err branch is exactly
		// the flaky-connection path this milestone targets. A clear placed after the guard would
		// leave the marker latched on a failed poll, wedging the detail-meta poll forever.
		m.detail.metaInFlight = false
		if msg.runID != m.detail.runID || msg.err != nil {
			return m, nil
		}
		m.detail.applyMeta(msg.run)
		// A poll that first reveals the drilled-in run's in-progress milestone arms the blink.
		return m, m.maybeArmBlink()

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
	// q quits immediately (user preference). ctrl+c still routes through a confirm modal so a
	// stray ctrl+c cannot drop a watched run; a second ctrl+c quits at once.
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
		return m, tea.Quit
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
	var sketch string
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
			// --sketch previews a throwaway TUI sketch by name with no server (PRD #1061),
			// a hidden discovery-friendly flag parallel to --demo. Changed() so a bare or
			// valued --sketch triggers it while a plain `uzi tui` is unaffected.
			if cmd.Flags().Changed("sketch") {
				return runTUISketch(cmd.Context(), env, sketch)
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
			// Gate the footer version readout and its auto-probe. showVersion is the off-switch
			// gate alone (versionCheckEnabled: the injection/quiet/env switches) and governs
			// whether the readout renders — a `dev` build still shows its version. The auto-probe
			// additionally needs a stamped build, exactly as the CLI's PersistentPreRun hook gates
			// its own version check: a `go build` binary carries `dev`, so it never probes.
			enabled := versionCheckEnabled(env, gf)
			m.showVersion = enabled
			m.skewCheck = enabled && uzicli.IsStampedVersion(version)
			// The reduced-motion opt-out is read ONCE here, with the CLI's os.Getenv idiom
			// (root.go), never per render (PRD #1064 D4).
			m.noBlink = os.Getenv("UZI_TUI_NO_BLINK") == "1"
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
	cmd.Flags().StringVar(&sketch, "sketch", "", "preview a throwaway TUI sketch by name (no server); bare --sketch lists them")
	cmd.Flags().Lookup("sketch").NoOptDefVal = "list" // bare --sketch => the "list" sentinel, not a cobra "flag needs an argument" error
	_ = cmd.Flags().MarkHidden("sketch")
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
