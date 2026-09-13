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

// boardBackoffCap caps the board's error-backoff reschedule interval (PRD #1130 M3 D4): the
// tick interval doubles from boardPollInterval per consecutive failed poll but never exceeds
// this. A var (not const) so a test can shrink it and so the cap is explicit.
var boardBackoffCap = 30 * time.Second

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
	// viewPulls is the forge `pulls` list screen (PRD #1255 M4a). viewCI is the forge
	// `ci` list screen (PRD #1255 M4b). viewPR is the PR drill-in (PRD #1255 M5) — a peer of
	// viewDetail, opened from the pulls list (enter/→) or the run view (m). viewCIRun is the
	// CI-run drill-in (PRD #1255 M6) — a peer of viewPR, opened from the ci list (enter/→).
	viewPulls
	viewCI
	viewPR
	viewCIRun
)

// ---- messages -------------------------------------------------------------

type boardRunsMsg struct {
	runs  []apitypes.RunListItemDTO
	admin bool
	err   error
	// reqID is the request-generation id this reply belongs to (PRD #1130 M1 D2). The board
	// honours it only when reqID == m.board.waitID, so an older reply that resolves after a
	// newer request was minted (bubbletea runs each Cmd in its own goroutine and delivers in
	// completion order) is dropped instead of clearing the newer request's guard.
	reqID uint64
}

// boardTickMsg carries the tick-chain generation it was scheduled under (PRD #1130 M1). A tick
// whose gen != m.board.tickGen belongs to a superseded chain (a manual/admin refresh or a
// reply-driven reschedule bumped the generation) and is dropped, so only one tick chain is ever
// live at once.
type boardTickMsg struct{ gen uint64 }

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

// detailRunMsg carries the first GetRun for the drilled-in run (PRD #1137). The header,
// crew-rail milestones/accounts and now-line render from it, before the transcript.
type detailRunMsg struct {
	runID string
	run   apitypes.RunDTO
	err   error
	// gen is the detail SESSION generation the request was issued under (detailState.gen).
	// exitToBoard cannot cancel an in-flight command, and reopening the SAME run passes the
	// runID guard, so a reply from the previous session is rejected on gen instead.
	gen uint64
}

// detailPageKind tags a transcript page. pageTail is the newest page; pageBackfill is the
// background walk toward older history (M5); pageCatchup is the incremental socket-down /
// manual-refresh recovery of frames after the highest seq held (M7).
type detailPageKind int

const (
	pageTail detailPageKind = iota
	pageBackfill
	pageCatchup
)

// detailPageMsg carries one page of transcript messages for the drilled-in run.
type detailPageMsg struct {
	runID string
	kind  detailPageKind
	msgs  []apitypes.MessageDTO
	err   error
	// reqID is the catch-up chain generation this page belongs to (M7), used only by pageCatchup
	// so a superseded chain's reply is dropped; pageTail/pageBackfill leave it zero.
	reqID uint64
	// gen is the detail SESSION generation (detailState.gen) the page was requested under. A
	// page from a session the user has since left — even for the same run, which passes the
	// runID guard — must not touch the new session's cursor, guards or pane state.
	gen uint64
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
	// reqID is the per-run request-generation id this reply belongs to (PRD #1130 M1 D2). The
	// detail honours it only when reqID == m.detail.metaWaitID AND runID matches the current
	// run, so a stale/superseded meta poll cannot clear a newer poll's guard. metaSeq restarts
	// per run (newDetailState), which is safe because the case checks runID first.
	reqID uint64
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
	// pulls is the forge `pulls` screen's state (PRD #1255 M4a). It carries its OWN
	// reqSeq/waitID/tickGen poll-guard counters (a copy of the board's chain, not a share)
	// so the two lists poll independently.
	pulls pullsState
	// ci is the forge `ci` screen's state (PRD #1255 M4b), with its OWN poll-guard counters
	// like pulls, so the two forge lists poll independently over the SAME scoped repo.
	ci ciState
	// pr is the PR drill-in's state (PRD #1255 M5), with its OWN reqSeq/waitID/tickGen poll-guard
	// counters and a 5s live re-poll chain, independent of the two list screens.
	pr prState
	// cirun is the CI-run drill-in's state (PRD #1255 M6), the ci-run twin of pr: its OWN
	// reqSeq/waitID/tickGen poll-guard counters and a 5s live re-poll chain, independent of the two
	// list screens and of the PR view.
	cirun ciRunState
	// prReturn / detailReturn record WHERE a drill-in was opened from, so its esc returns there
	// (D1): the PR view returns to prReturn (the pulls list, or the run view on detail→m→PR), and
	// the run view returns to detailReturn (the board by default, or pulls / PR on a u ↳ run jump).
	// prReturn defaults to viewPulls; detailReturn to viewBoard (the zero value), so the existing
	// board↔detail behaviour is unchanged. The state returned to is never clobbered — m.pulls /
	// m.detail persist on the model — so it is still loaded when esc lands back on it.
	prReturn     tuiView
	detailReturn tuiView
	// forgeNotice is the transient one-line confirmation / server-reason a w (rework) / f (fix ci)
	// action leaves, drawn in the PR view's and the pulls screen's header-note area (D1/D12). It is
	// set by prActionMsg, overwritten by the next action, and cleared when a PR view is opened fresh
	// or the repo cycles. The success text is static + a short run id; the error text is fmtErr-
	// sanitized, so drawing it is safe.
	forgeNotice string
	// repos / repoIdx / repoChosen scope the forge views to one repo at a time (PRD #1255
	// D2). repos is the viewer's ENABLED repos (from ListRepos); repoIdx is the current one;
	// R cycles it; repoChosen records whether the default-repo rule has resolved (or the user
	// has cycled), so the default is computed once rather than on every view entry. reposLoaded
	// / reposErr drive the "loading…" / can't-load scope states.
	repos       []apitypes.RepoDTO
	repoIdx     int
	repoChosen  bool
	reposLoaded bool
	reposErr    error
	// reposInFlight is the repos-fetch in-flight guard, the repos twin of pullsState.waitID: it
	// is true while a fetchReposCmd is outstanding, so the pulls-tick / r self-heal (PRD #1255)
	// cannot stack a second live repos fetch on one already running. Seeded true at Init (initCmds
	// issues the first fetchReposCmd) and cleared by every reposMsg, success or failure.
	reposInFlight bool
	// detailGen counts detail sessions opened from the board; each drill-in stamps the new
	// detailState.gen from it, so a reply issued under an earlier session (same run reopened)
	// is rejected by the gen check in the detailRunMsg / detailPageMsg cases.
	detailGen uint64
	// prGen counts PR drill-in sessions opened from the pulls row or the run view; startPRReq stamps
	// the new prState.gen from it on each open's first fetch, so a reply issued under an earlier
	// session — a different PR whose reset reqSeq minted the SAME reqID, or the same PR reopened — is
	// rejected by the gen check in the prMsg case. The PR twin of detailGen.
	prGen uint64
	// ciRunGen counts CI-run drill-in sessions opened from the ci row; startCIRunReq stamps the new
	// ciRunState.gen from it on each open's first fetch, so a reply issued under an earlier session —
	// a different run whose reset reqSeq minted the SAME reqID, or the same run reopened — is rejected
	// by the gen check in the ciRunMsg case. The ci-run twin of prGen.
	ciRunGen uint64

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
	// Seed the Init board request as already in flight (PRD #1130 M1 D2): initCmds issues the
	// first fetchRunsCmd tagged with reqID 1, so its reply carries reqID 1 and clears the guard.
	// The tick chain starts at generation 1 too, so the Init tick (armed with tickGen 1) is
	// honoured. Without this, the first periodic tick would stack a second board poll on top of
	// the Init fetch, and an out-of-order Init reply could clear a newer request's guard.
	m.board.reqSeq = 1
	m.board.waitID = 1
	m.board.tickGen = 1
	// The pulls list seeds its tick chain at generation 1 (the Init-armed pulls tick is
	// honoured) but, UNLIKE the board, no request is in flight at Init: the repo the `pulls`
	// route needs is not known until ListRepos returns, so reqSeq/waitID start at 0 (idle).
	// The first fetch is minted by startPullsReq when the user opens the screen, or by the
	// first tick once repos have loaded (newPullsState).
	m.pulls = newPullsState()
	// The ci list seeds its own tick chain the same way as pulls (generation 1, no request in
	// flight until the repo scope resolves); both forge lists share the repo scope below.
	m.ci = newCIState()
	// The PR drill-in seeds its tick chain at generation 1 (the Init-armed PR tick is honoured)
	// with no PR loaded (repoID ""), so the tick never polls until the user opens a PR view and
	// startPRReq mints the first fetch. prReturn defaults to viewPulls; detailReturn keeps the
	// viewBoard zero value so board↔detail is unchanged (PRD #1255 M5 D1).
	m.pr = newPRState("", 0)
	m.prReturn = viewPulls
	// The CI-run drill-in seeds its tick chain at generation 1 (the Init-armed CI-run tick is
	// honoured) with no run loaded (repoID ""), so the tick never polls until the user opens a CI-run
	// view and startCIRunReq mints the first fetch (PRD #1255 M6) — the ci-run twin of pr.
	m.cirun = newCIRunState("", 0)
	// The repos scope fetch IS in flight at Init (initCmds issues fetchReposCmd), so seed the
	// guard true — the reply clears it. Without this a pulls tick that fires before the Init
	// reposMsg lands could stack a second repos fetch on top of the Init one.
	m.reposInFlight = true
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
	cmds := []tea.Cmd{m.fetchRunsCmd(m.board.admin, m.board.waitID), m.fetchSecretsCmd(),
		m.fetchRateLimitsCmd(), m.fetchSettingsCmd(), tickAfter(boardPollInterval, m.board.tickGen), stripTickCmd(),
		// The forge views' repo scope (PRD #1255 D2) and the `pulls` list's own 10s tick chain.
		// The tick is armed now but polls the forge only while the pulls screen is in focus
		// (pullsTickMsg), so an idle board pays no forge cost for it.
		m.fetchReposCmd(), pullsTickAfter(pullsPollInterval, m.pulls.tickGen),
		// The `ci` list's own 10s tick chain (PRD #1255 M4b). Like the pulls tick it is armed
		// now but polls the forge only while the ci screen is in focus, so an idle board pays no
		// forge cost for it.
		ciTickAfter(ciPollInterval, m.ci.tickGen),
		// The PR drill-in's own 5s live re-poll chain (PRD #1255 M5). Armed now but polls the forge
		// only while the PR view is in focus and a PR is loaded; the open path's immediate startPRReq
		// is what actually begins the chain, the reply re-arms it, so an idle board pays nothing.
		prTickAfter(prPollInterval, m.pr.tickGen),
		// The CI-run drill-in's own 5s live re-poll chain (PRD #1255 M6). Like the PR tick it is armed
		// now but polls the forge only while the CI-run view is in focus and a run is loaded; the open
		// path's immediate startCIRunReq begins the chain, the reply re-arms it, so an idle board pays
		// nothing.
		ciRunTickAfter(ciRunPollInterval, m.cirun.tickGen),
		tea.RequestBackgroundColor}
	if m.skewCheck {
		cmds = append(cmds, m.fetchBuildInfoCmd(), skewTickCmd())
	}
	if m.view == viewDetail {
		cmds = append(cmds, m.loadRunCmd(m.detail.runID), m.loadTailCmd(m.detail.runID), m.openStreamCmd(m.detail.runID))
	}
	return cmds
}

func (m tuiModel) Init() tea.Cmd { return tea.Batch(m.initCmds()...) }

// tickAfter arms the board tick after delay d, stamping the produced boardTickMsg with the
// tick-chain generation gen. The reply owns rescheduling (boardRunsMsg re-arms with the
// post-reply streak via boardTickInterval), so a confirmed-bad link is polled at the backed-off
// cadence rather than the flat 2s one; gen lets the model drop a tick from a superseded chain
// (a manual/admin refresh or a reply-driven reschedule bumps tickGen).
func tickAfter(d time.Duration, gen uint64) tea.Cmd {
	return tea.Tick(d, func(time.Time) tea.Msg { return boardTickMsg{gen: gen} })
}

// boardTickInterval maps the consecutive board-poll error streak to the reschedule
// interval (PRD #1130 M3 D4): boardPollInterval at streak 0, doubling per consecutive
// failure, clamped at boardBackoffCap. A pure function so the backoff is unit-assertable
// without inspecting an opaque tea.Tick. Reset-on-success (streak→0) snaps back to base.
func boardTickInterval(streak int) time.Duration {
	if streak <= 0 {
		return boardPollInterval
	}
	// Clamp the shift so a large streak cannot overflow the Duration; the cap clamp below
	// governs the real ceiling, this only bounds the arithmetic.
	if streak > 16 {
		streak = 16
	}
	d := boardPollInterval << uint(streak)
	// A left shift can also overflow into a negative Duration on a large base; treat any
	// non-positive or over-cap result as the cap.
	if d <= 0 || d > boardBackoffCap {
		return boardBackoffCap
	}
	return d
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

func (m tuiModel) fetchRunsCmd(admin bool, reqID uint64) tea.Cmd {
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
		return boardRunsMsg{runs: runs, admin: admin, err: err, reqID: reqID}
	}
}

// startBoardReq mints the next board request id, records it as the one the model is waiting on,
// and returns the tagged fetch. It centralizes the "every board fetch is id-tagged and supersedes
// any older outstanding request" invariant (PRD #1130 M1 D2): the periodic tick, manual r, admin
// toggle and exit-to-board all go through it, so a reply is honoured only when its reqID matches
// the latest waitID and an out-of-order older reply can never clear a newer request's guard.
func (m *tuiModel) startBoardReq() tea.Cmd {
	m.board.reqSeq++
	m.board.waitID = m.board.reqSeq
	return m.fetchRunsCmd(m.board.admin, m.board.waitID)
}

// startDetailMetaReq is the detail-meta analogue of startBoardReq: it mints the next per-run
// meta request id, records it as the one the detail is waiting on, and returns the tagged
// refresh. Same invariant — every meta fetch is id-tagged and supersedes any older outstanding
// meta poll — so an out-of-order reply cannot clear a newer poll's guard.
func (m *tuiModel) startDetailMetaReq() tea.Cmd {
	m.detail.metaSeq++
	m.detail.metaWaitID = m.detail.metaSeq
	return m.refreshRunMetaCmd(m.detail.runID, m.detail.metaWaitID)
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

// detailPageSize is the transcript page size (matches uzicli's logsPageSize). detailPayloadMax
// is the ?payload_max the TUI asks for — the two bulky fields it never draws in full are
// trimmed on the wire (PRD #1137 D4). Both are vars so tests can shrink them.
var (
	detailPageSize   int32 = 200
	detailPayloadMax int32 = 2048
)

func (m tuiModel) loadRunCmd(runID string) tea.Cmd {
	c, ctx, gen := m.client, m.ctx, m.detail.gen
	return func() tea.Msg {
		run, err := c.GetRun(ctx, runID)
		return detailRunMsg{runID: runID, run: run, err: err, gen: gen}
	}
}

func (m tuiModel) loadTailCmd(runID string) tea.Cmd {
	c, ctx, gen := m.client, m.ctx, m.detail.gen
	return func() tea.Msg {
		msgs, err := c.RunLogsPage(ctx, runID, uzicli.LogsPageQuery{Tail: detailPageSize, PayloadMax: detailPayloadMax})
		return detailPageMsg{runID: runID, kind: pageTail, msgs: msgs, err: err, gen: gen}
	}
}

// backfillCmd fetches the newest page strictly below `before` (older history), for the
// background walk that fills the transcript after the tail (PRD #1137 D1). One page; the
// reply chains the next.
func (m tuiModel) backfillCmd(runID string, before int32) tea.Cmd {
	c, ctx, gen := m.client, m.ctx, m.detail.gen
	return func() tea.Msg {
		msgs, err := c.RunLogsPage(ctx, runID, uzicli.LogsPageQuery{Before: before, Limit: detailPageSize, PayloadMax: detailPayloadMax})
		return detailPageMsg{runID: runID, kind: pageBackfill, msgs: msgs, err: err, gen: gen}
	}
}

// catchupCmd fetches the page of messages AFTER `after` (seq the view already holds), the socket-
// down / manual-refresh path that recovers frames the dead stream would have carried. reqID tags
// the reply so a superseded chain is dropped.
func (m tuiModel) catchupCmd(runID string, after int32, reqID uint64) tea.Cmd {
	c, ctx, gen := m.client, m.ctx, m.detail.gen
	return func() tea.Msg {
		msgs, err := c.RunLogsPage(ctx, runID, uzicli.LogsPageQuery{After: after, Limit: detailPageSize, PayloadMax: detailPayloadMax})
		return detailPageMsg{runID: runID, kind: pageCatchup, reqID: reqID, msgs: msgs, err: err, gen: gen}
	}
}

// startDetailCatchupReq mints the next catch-up id and returns the first page from the highest seq
// held. The caller must gate on catchupWaitID == 0 (no chain in flight) AND highSeq > 0 (never an
// after=0 whole-transcript fetch — SC4).
func (m *tuiModel) startDetailCatchupReq() tea.Cmd {
	m.detail.catchupSeq++
	m.detail.catchupWaitID = m.detail.catchupSeq
	return m.catchupCmd(m.detail.runID, m.detail.highSeq, m.detail.catchupWaitID)
}

// refreshRunMetaCmd re-reads only the run DTO (no transcript replay), so the periodic
// detail refresh is cheap: the socket already carries the frames, this just refreshes the
// milestone / health / duration fields the stream does not send.
func (m tuiModel) refreshRunMetaCmd(runID string, reqID uint64) tea.Cmd {
	c, parent := m.client, m.ctx
	return func() tea.Msg {
		// Per-poll deadline (PRD #1130 D3): same short bound as the board poll, derived
		// inside the closure so defer cancel() fires on return rather than immediately.
		ctx, cancel := context.WithTimeout(parent, boardPollTimeout)
		defer cancel()
		run, err := c.GetRun(ctx, runID)
		return detailMetaMsg{runID: runID, run: run, err: err, reqID: reqID}
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

// pollFallbackInterval is the D8 REST-poll cadence (the same 2s `uzi run logs --follow` uses). A
// var so a test can shrink it and drain the re-armed tick without blocking on the real 2s.
var pollFallbackInterval = 2 * time.Second

func pollFallbackCmd() tea.Cmd {
	return tea.Tick(pollFallbackInterval, func(time.Time) tea.Msg { return pollFallbackMsg{} })
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
		// Drop a tick from a superseded chain (PRD #1130 M1): a manual/admin refresh or a
		// reply-driven reschedule bumps m.board.tickGen, leaving any tick already pending under
		// the old generation stale. Honouring it would run two overlapping tick chains.
		if msg.gen != m.board.tickGen {
			return m, nil
		}
		// The ctrl+c confirm modal (m.quitting) is CANCELLABLE — any non-confirming key clears it
		// and the model keeps running (handleKey), so this is NOT teardown. Keep the tick chain
		// alive across the modal by re-arming at the current generation while skipping the poll (no
		// work while the user is deciding to quit), mirroring the strip/skew ticks. Since the reply
		// is now the only OTHER re-arm site, dropping the tick here would wedge automatic polling
		// once the modal is dismissed.
		if m.quitting {
			return m, tickAfter(boardTickInterval(m.board.errStreak), m.board.tickGen)
		}
		// The in-flight guard (PRD #1130 M1 D1): a periodic tick starts a new board poll ONLY
		// when none is outstanding (waitID == 0), so a link where a ListRuns takes longer than
		// the 2s cadence cannot pile up overlapping concurrent requests. This case NO LONGER
		// re-arms the tick — rescheduling moved to boardRunsMsg, which re-arms using the
		// post-reply error streak (so the first retry after a failure uses the backed-off
		// interval, not the stale pre-failure one) and bumps tickGen to supersede any tick a
		// manual/admin refresh left pending.
		var cmds []tea.Cmd
		if m.board.waitID == 0 {
			cmds = append(cmds, (&m).startBoardReq())
		}
		// Keep the drilled-in run's non-streamed fields (milestones, health, duration) fresh
		// on the same cadence the board polls at: the live socket carries transcript frames and
		// status only. Skipped while the D8 fallback (pollFallbackMsg) is already reloading the
		// whole DTO every 2s, so the two never double up — and guarded by its own request id so
		// a slow meta poll does not stack either.
		if m.view == viewDetail && !m.detail.polling && m.detail.run.ID != "" && m.detail.metaWaitID == 0 {
			cmds = append(cmds, (&m).startDetailMetaReq())
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
		// Drop a stale/out-of-order reply (PRD #1130 M1 D2): bubbletea runs each Cmd in its own
		// goroutine and delivers in completion order, so an older board poll can resolve after a
		// newer request was minted. Honour only the reply whose reqID matches the request we are
		// waiting on — do not clear the guard, do not apply, do not reschedule for any other.
		if msg.reqID != m.board.waitID {
			return m, nil
		}
		m.board.waitID = 0
		m.board.apply(msg)
		// Reschedule the tick HERE, after apply updated errStreak (PRD #1130 M3): this is what
		// makes the first retry after a failed poll use the backed-off interval
		// (boardTickInterval of the fresh streak) rather than the stale pre-failure one. Bump
		// tickGen first so this new chain supersedes any tick a manual/admin refresh left
		// pending — only one tick chain stays live.
		m.board.tickGen++
		return m, tea.Batch(tickAfter(boardTickInterval(m.board.errStreak), m.board.tickGen), m.maybeArmBlink())

	case reposMsg:
		// The forge views' repo scope (PRD #1255 D2). A failure is recorded (the pulls scope
		// state explains it) rather than crashing. On success, keep only the ENABLED repos; if
		// the user is already on the pulls screen, resolve the default repo and fetch now so they
		// are not stuck on "loading…" until the next 10s tick.
		// Every reply — success or failure — releases the in-flight guard, so the pulls-tick /
		// r self-heal can re-issue the next repos fetch after a transient failure.
		m.reposInFlight = false
		m.reposLoaded = true
		if msg.err != nil {
			m.reposErr = msg.err
			return m, nil
		}
		m.reposErr = nil
		m.repos = enabledRepos(msg.repos)
		// If the user is already on a forge screen, resolve the default repo and fetch now so they
		// are not stuck on "loading…" until the next 10s tick. Both forge lists share the repo scope
		// (D2), so whichever is in focus kicks off its own fetch.
		switch m.view {
		case viewPulls:
			(&m).resolveDefaultRepo()
			if m.pulls.waitID == 0 && m.pullsRepoReady() {
				return m, (&m).startPullsReq()
			}
		case viewCI:
			(&m).resolveDefaultRepo()
			if m.ci.waitID == 0 && m.pullsRepoReady() {
				return m, (&m).startCIReq()
			}
		}
		return m, nil

	case pullsTickMsg:
		// Drop a tick from a superseded chain (mirrors the board's tickGen guard).
		if msg.gen != m.pulls.tickGen {
			return m, nil
		}
		// Keep the chain alive across the cancellable quit modal without polling.
		if m.quitting {
			return m, pullsTickAfter(pullsTickInterval(m.pulls.errStreak), m.pulls.tickGen)
		}
		// Self-heal the shared repo scope (PRD #1255): a transient ListRepos failure at Init has
		// no other retry path, so every forge screen would show "could not load repositories" for
		// the whole session. While the pulls screen is in focus and repos are NOT ready (still
		// loading, or a failure left reposErr set), re-issue fetchReposCmd — gated on the
		// reposInFlight guard so it never stacks a second live repos fetch — and re-arm this tick.
		// It is gated on view==viewPulls like the pulls fetch so an idle board pays no forge cost
		// (the D4 forge-budget guard). A later successful reposMsg clears reposErr and resolves the
		// default repo, bringing the screen alive.
		if m.view == viewPulls && !m.reposReady() {
			cmds := []tea.Cmd{pullsTickAfter(pullsTickInterval(m.pulls.errStreak), m.pulls.tickGen)}
			if !m.reposInFlight {
				m.reposInFlight = true
				cmds = append(cmds, m.fetchReposCmd())
			}
			return m, tea.Batch(cmds...)
		}
		// Poll the forge only while the pulls screen is in focus and a repo is selected (the
		// forge budget is shared with the board poller — D4 — so a background tick does no forge
		// work), and only when no poll is already outstanding (the in-flight guard). When it DOES
		// fetch, the reply re-arms the chain (as on the board). When it does NOT, this tick must
		// re-arm ITSELF — here the fetch is conditional, so the reply is the only OTHER re-arm
		// site and a non-fetching tick would otherwise let the chain lapse.
		if m.view == viewPulls && m.pulls.waitID == 0 && m.pullsRepoReady() {
			return m, (&m).startPullsReq()
		}
		return m, pullsTickAfter(pullsTickInterval(m.pulls.errStreak), m.pulls.tickGen)

	case pullsMsg:
		// Drop a stale/out-of-order reply (mirrors boardRunsMsg): honour only the reply whose
		// reqID matches the request we are waiting on.
		if msg.reqID != m.pulls.waitID {
			return m, nil
		}
		m.pulls.waitID = 0
		m.pulls.apply(msg)
		// Reschedule AFTER apply updated errStreak so the first retry after a failed poll uses
		// the backed-off interval; bump tickGen so this chain supersedes any pending tick.
		m.pulls.tickGen++
		return m, pullsTickAfter(pullsTickInterval(m.pulls.errStreak), m.pulls.tickGen)

	case ciTickMsg:
		// The ci tick chain mirrors the pulls tick exactly (PRD #1255 M4b): drop a superseded
		// chain, keep alive across the cancellable quit modal without polling, self-heal the shared
		// repo scope while the ci screen is in focus and repos are not ready, else poll the forge
		// only while the ci screen is in focus and a repo is selected.
		if msg.gen != m.ci.tickGen {
			return m, nil
		}
		if m.quitting {
			return m, ciTickAfter(ciTickInterval(m.ci.errStreak), m.ci.tickGen)
		}
		// Self-heal the shared repo scope (PRD #1255): a transient ListRepos failure at Init has no
		// other retry path, so every forge screen would show "could not load repositories" for the
		// whole session. While the ci screen is in focus and repos are NOT ready, re-issue
		// fetchReposCmd — gated on the reposInFlight guard so it never stacks a second live repos
		// fetch — and re-arm this tick. Gated on view==viewCI so an idle board pays no forge cost.
		if m.view == viewCI && !m.reposReady() {
			cmds := []tea.Cmd{ciTickAfter(ciTickInterval(m.ci.errStreak), m.ci.tickGen)}
			if !m.reposInFlight {
				m.reposInFlight = true
				cmds = append(cmds, m.fetchReposCmd())
			}
			return m, tea.Batch(cmds...)
		}
		// Poll the forge only while the ci screen is in focus and a repo is selected (the forge
		// budget is shared with the board poller — D4), and only when no poll is outstanding (the
		// in-flight guard). When it DOES fetch, the reply re-arms the chain; when it does NOT, this
		// tick re-arms ITSELF (the fetch is conditional, so the reply is the only OTHER re-arm site).
		if m.view == viewCI && m.ci.waitID == 0 && m.pullsRepoReady() {
			return m, (&m).startCIReq()
		}
		return m, ciTickAfter(ciTickInterval(m.ci.errStreak), m.ci.tickGen)

	case ciMsg:
		// Drop a stale/out-of-order reply (mirrors pullsMsg): honour only the reply whose reqID
		// matches the request we are waiting on.
		if msg.reqID != m.ci.waitID {
			return m, nil
		}
		m.ci.waitID = 0
		m.ci.apply(msg)
		// Reschedule AFTER apply updated errStreak so the first retry after a failed poll uses the
		// backed-off interval; bump tickGen so this chain supersedes any pending tick.
		m.ci.tickGen++
		return m, ciTickAfter(ciTickInterval(m.ci.errStreak), m.ci.tickGen)

	case prTickMsg:
		// The PR drill-in's 5s live re-poll (PRD #1255 M5). Drop a superseded chain, keep alive
		// across the cancellable quit modal without polling, else poll the forge only while the PR
		// view is in focus and a PR is loaded and no poll is outstanding. When it DOES fetch, the
		// reply re-arms the chain (as on the board); when it does NOT, this tick re-arms ITSELF.
		if msg.gen != m.pr.tickGen {
			return m, nil
		}
		if m.quitting {
			return m, prTickAfter(prTickInterval(m.pr.errStreak), m.pr.tickGen)
		}
		if m.view == viewPR && m.pr.waitID == 0 && m.pr.repoID != "" {
			return m, (&m).startPRReq()
		}
		return m, prTickAfter(prTickInterval(m.pr.errStreak), m.pr.tickGen)

	case prMsg:
		// Drop a stale/out-of-order reply (mirrors pullsMsg): honour only the reply whose reqID
		// matches the request we are waiting on AND whose PR session generation is the current one.
		// The gen check is load-bearing across a reopen: newPRState resets reqSeq, so a prior PR's
		// in-flight reply mints the SAME reqID and passes the reqID==waitID guard — only gen tells
		// them apart, so without it PR A's late detail (Title/Checks/RunID) would be applied to the
		// PR B view and w/u would then act on the wrong run.
		if msg.reqID != m.pr.waitID || msg.gen != m.pr.gen {
			return m, nil
		}
		m.pr.waitID = 0
		m.pr.apply(msg)
		// Reschedule AFTER apply updated errStreak so the first retry after a failed poll uses the
		// backed-off interval; bump tickGen so this chain supersedes any pending tick.
		m.pr.tickGen++
		return m, prTickAfter(prTickInterval(m.pr.errStreak), m.pr.tickGen)

	case ciRunTickMsg:
		// The CI-run drill-in's 5s live re-poll (PRD #1255 M6), mirroring the PR tick exactly: drop a
		// superseded chain, keep alive across the cancellable quit modal without polling, else poll
		// the forge only while the CI-run view is in focus and a run is loaded and no poll is
		// outstanding. When it DOES fetch, the reply re-arms the chain; when it does NOT, this tick
		// re-arms ITSELF.
		if msg.gen != m.cirun.tickGen {
			return m, nil
		}
		if m.quitting {
			return m, ciRunTickAfter(ciRunTickInterval(m.cirun.errStreak), m.cirun.tickGen)
		}
		if m.view == viewCIRun && m.cirun.waitID == 0 && m.cirun.repoID != "" {
			return m, (&m).startCIRunReq()
		}
		return m, ciRunTickAfter(ciRunTickInterval(m.cirun.errStreak), m.cirun.tickGen)

	case ciRunMsg:
		// Drop a stale/out-of-order reply (mirrors prMsg): honour only the reply whose reqID matches
		// the request we are waiting on AND whose CI-run session generation is the current one. The
		// gen check is load-bearing across a reopen: newCIRunState resets reqSeq, so a prior run's
		// in-flight reply mints the SAME reqID and passes the reqID==waitID guard — only gen tells them
		// apart, so without it run A's late jobs/steps would be applied to the run B view.
		if msg.reqID != m.cirun.waitID || msg.gen != m.cirun.gen {
			return m, nil
		}
		m.cirun.waitID = 0
		m.cirun.apply(msg)
		// Reschedule AFTER apply updated errStreak so the first retry after a failed poll uses the
		// backed-off interval; bump tickGen so this chain supersedes any pending tick.
		m.cirun.tickGen++
		return m, ciRunTickAfter(ciRunTickInterval(m.cirun.errStreak), m.cirun.tickGen)

	case prActionMsg:
		// A w (rework) / f (fix ci) result from the PR view or a pulls list row: a success
		// confirmation or the server's typed 4xx/409 reason, drawn inline (never a crash).
		m.forgeNotice = prActionNotice(msg)
		return m, nil

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

	case detailRunMsg:
		// Drop a load that resolved for a run the user has since navigated away from:
		// esc/exitToBoard resets m.detail to its zero value (runID "", nil `seen` map), and
		// drilling into another run swaps runID — a late applyRun from the previous run would
		// flip the wrong header in, and against the zero value it would write the nil map and
		// panic. Same runID guard every sibling detail message carries. runID is set by
		// loadRunCmd on both the success and error paths; run.ID is the fallback for a message
		// that carries only the run (the error path leaves run zero, which is why the explicit
		// field exists).
		id := msg.runID
		if id == "" {
			id = msg.run.ID
		}
		if id != m.detail.runID {
			return m, nil
		}
		if msg.gen != m.detail.gen {
			return m, nil // a reply from a detail session the user has since left (same run reopened)
		}
		m.detail.applyRun(msg.run, msg.err)
		// The ownership probe + queue indicator and the in-progress-milestone blink both
		// key off the run DTO, so they ride the run load (as the old full-load did).
		return m, tea.Batch(m.fetchInputsCmd(m.detail.runID), m.maybeArmBlink())

	case detailPageMsg:
		if msg.runID != m.detail.runID {
			return m, nil
		}
		// Session guard: exitToBoard cannot cancel an in-flight page command, and reopening the
		// SAME run passes the runID check above. A stale page would clear tailInFlight, start a
		// second backfill chain from an obsolete cursor, or paint an old error over the new pane
		// — so it is rejected on the session generation before touching any state.
		if msg.gen != m.detail.gen {
			return m, nil
		}
		switch msg.kind {
		case pageTail:
			// Any tail reply — success or error — releases the retry guard, so a failed retry
			// re-enables the next `r` / fallback tick instead of wedging the stuck state.
			m.detail.tailInFlight = false
			m.detail.applyTailPage(msg.msgs, msg.err)
			// Raise the stream's replay floor to the highest seq now held, so a reconnect replays
			// only frames after the tail rather than the whole history (PRD #1137 M7).
			if s := m.detail.stream; s != nil {
				s.NoteSeen(m.detail.highSeq)
			}
			// Start the background backfill once the tail is in, if there is older history to
			// walk (PRD #1137 D1). The reply chains the next page. The !backfilling guard keeps a
			// second tail page (e.g. an `r` retry of a failed initial tail) from starting a SECOND
			// parallel backfill chain over the shared lowSeq cursor.
			if msg.err == nil && !m.detail.historyComplete && !m.detail.backfilling && m.detail.lowSeq > 1 {
				m.detail.backfilling = true
				return m, m.backfillCmd(m.detail.runID, m.detail.lowSeq)
			}
			// Only a SUCCESSFUL tail with nothing older is complete. A tail ERROR adds no frames
			// (lowSeq stays 0), so it must NOT latch historyComplete — otherwise a later `r`
			// retry that loads real history would never start the backfill (the start guard
			// requires !historyComplete).
			if msg.err == nil && m.detail.lowSeq <= 1 { // 0 (empty run) or 1 (start already held)
				m.detail.historyComplete = true
			}
			return m, nil
		case pageBackfill:
			return m, (&m).applyBackfillPage(msg.msgs, msg.err)
		case pageCatchup:
			if msg.reqID != m.detail.catchupWaitID {
				return m, nil // a superseded / stale catch-up reply
			}
			if msg.err != nil {
				m.detail.catchupWaitID = 0 // chain ends; the next tick may retry
				return m, nil
			}
			before := m.detail.highSeq
			m.detail.applyCatchupPage(msg.msgs)
			if s := m.detail.stream; s != nil {
				s.NoteSeen(m.detail.highSeq)
			}
			// Stop on an empty page (caught up) OR a page that did not advance the cursor (a
			// hostile/broken server or an all-duplicate page — the mirror of the backfill
			// did-not-advance guard), so the chain can never spin without clearing its guard.
			if len(msg.msgs) == 0 || m.detail.highSeq <= before {
				m.detail.catchupWaitID = 0 // caught up (or cannot advance): chain done
				return m, nil
			}
			return m, m.catchupCmd(m.detail.runID, m.detail.highSeq, msg.reqID) // chain the next page (same id)
		}
		return m, nil

	case detailMetaMsg:
		// runID is checked FIRST (PRD #1130 M1 D2): metaSeq restarts per run (newDetailState),
		// so a reply for a run we have navigated away from could otherwise collide with the new
		// run's id. A run-mismatched reply must never touch the current run's guard.
		if msg.runID != m.detail.runID {
			return m, nil // a reply for a run we've navigated away from — never touches the current guard
		}
		if msg.reqID != m.detail.metaWaitID {
			return m, nil // a stale/superseded poll for this run
		}
		m.detail.metaWaitID = 0
		if msg.err != nil {
			return m, nil // failed poll: guard cleared above so the next tick retries (D2)
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
		// Seed the replay floor with the highest seq already held (from a tail/catch-up page that
		// landed before the socket opened), so this stream's first reconnect replays only newer
		// frames rather than the whole history (PRD #1137 M7).
		m.detail.stream.NoteSeen(m.detail.highSeq)
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
		cmds := []tea.Cmd{pollFallbackCmd()} // always re-arm the 2s tick while polling
		if m.detail.metaWaitID == 0 {
			cmds = append(cmds, (&m).startDetailMetaReq()) // one meta refresh, guarded (#1135)
		}
		switch {
		case m.detail.pageErr != nil && m.detail.highSeq == 0 && !m.detail.tailInFlight:
			// The initial tail failed and the socket is also down, so nothing ever loaded the
			// transcript: retry the tail. Bounded to this stuck state (a held page moves to the
			// guarded catch-up below), so it never reintroduces the per-tick whole-transcript
			// refetch M7 removed — and guarded by tailInFlight so a slow link cannot stack a
			// second tail request per tick on one still in flight (the #1130 anti-stack property).
			m.detail.tailInFlight = true
			cmds = append(cmds, m.loadTailCmd(m.detail.runID))
		case m.detail.catchupWaitID == 0 && m.detail.highSeq > 0:
			cmds = append(cmds, (&m).startDetailCatchupReq()) // one catch-up chain, guarded
		}
		return m, tea.Batch(cmds...)
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
	case viewPulls:
		return m.pullsKey(k)
	case viewCI:
		return m.ciKey(k)
	case viewPR:
		return m.prKey(k)
	case viewCIRun:
		return m.ciRunKey(k)
	default:
		return m.detailKey(k)
	}
}

func (m tuiModel) filtering() bool {
	return (m.view == viewBoard && m.board.filtering) ||
		(m.view == viewPulls && m.pulls.filtering) ||
		(m.view == viewCI && m.ci.filtering)
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
	case m.view == viewPulls:
		body = m.renderPulls()
	case m.view == viewCI:
		body = m.renderCI()
	case m.view == viewPR:
		body = m.renderPR()
	case m.view == viewCIRun:
		body = m.renderCIRun()
	default:
		body = m.renderBoard()
	}
	v.SetContent(body)
	return v
}

func (m tuiModel) renderHelp() string {
	lines := helpLines(m.view)
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
