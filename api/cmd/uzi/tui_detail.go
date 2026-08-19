package main

import (
	"encoding/json"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// laneRailWidth is the left rail's fixed column budget.
const laneRailWidth = 26

// The detail view has two focusable panes (PRD #325 M4). ←/→ (and tab) move focus between
// them; ↑/↓ act WITHIN the focused pane — between agents on the rail, scrolling the
// transcript. The zero value is focusRail, so a run opens focused on the crew rail.
const (
	focusRail = iota
	focusTranscript
)

// detailState is one run's live view: the lane rail plus the selected lane's
// transcript, fed by the REST replay for history and StreamRun for live frames.
type detailState struct {
	runID string
	run   apitypes.RunDTO
	// frames is the merged, seq-ordered message log — replay and live frames land in
	// the same slice, deduped by seq, because the lane logic must not care which
	// transport a frame arrived on.
	frames []laneFrame
	seen   map[int32]bool

	lanes   []agentLane
	laneIdx int
	// scroll is the transcript window's TOP line index (M5, bottom-anchored). When follow
	// is true the window is pinned to the bottom (auto-tail) and scroll is recomputed on
	// render; when paused, scroll is the fixed top so the view does not jump as frames
	// arrive below it.
	scroll int
	focus  int  // focusRail | focusTranscript — which pane ↑/↓ drives (M4)
	follow bool // M5: auto-tail the transcript (tail -f). Reset true on open / lane switch.
	// railCollapsed folds the crew list to a one-line summary + the selected lane (`c`), so a
	// tall roster cannot push the milestone block below the fold — the rail does not scroll,
	// it is clamped to the transcript height (issue #379).
	railCollapsed bool
	loaded        bool
	loadErr       error
	stream        *uzicli.RunStream
	streamErr     error
	// polling is the D8 fallback: the socket is unusable, so the view re-reads over
	// REST on the same 2s cadence `uzi run logs --follow` uses.
	polling bool

	// M4 surfaces. steer is gated on OWNERSHIP, not visibility (see steerAccessFor);
	// review is the [v] overlay.
	steer  steerState2
	review reviewState
}

func newDetailState(runID string) detailState {
	return detailState{runID: runID, seen: map[int32]bool{}, follow: true}
}

func (d *detailState) applyLoaded(msg detailLoadedMsg) {
	if msg.err != nil {
		d.loadErr = msg.err
		d.loaded = true
		return
	}
	d.loadErr = nil
	d.run = msg.run
	for _, m := range msg.msgs {
		d.addFrame(laneFrameFromMessage(m))
	}
	d.loaded = true
	d.rebuild()
}

// applyMeta refreshes the run DTO from a periodic GetRun poll WITHOUT touching the frame
// log — the transcript is fed by the stream/replay, this only refreshes the non-streamed
// fields (milestones, health, kind, title, lifecycle stamps). Ignored until the initial
// load has set the baseline, so a poll racing the first full load cannot flip `loaded`.
//
// Status is DELIBERATELY preserved rather than overwritten: the live stream owns it
// (applyEvents sets it from authoritative `state` frames, including StreamRun's reconcile),
// and applyMeta only runs while the stream is healthy (the dispatch guards on !polling).
// Overwriting it would let a GetRun response that was in flight across a status transition
// revert the status for up to one 2s tick — e.g. dropping the plan-gate banner and its owner
// keys the instant a run enters awaiting_approval, or flipping a just-finished run back to
// running. When the stream is down the poll-fallback path (loadDetailCmd/applyLoaded) owns
// status instead, so nothing goes stale.
func (d *detailState) applyMeta(run apitypes.RunDTO) {
	if !d.loaded {
		return
	}
	status := d.run.Status
	d.run = run
	d.run.Status = status
}

// applyEvents folds a batch in and reports whether the steer queue was signalled
// changed, so the caller can re-read it.
func (d *detailState) applyEvents(evs []apitypes.RunEventDTO) bool {
	inputChanged := false
	for _, ev := range evs {
		switch ev.Type {
		case uzicli.RunEventTypeMessage:
			d.addFrame(laneFrameFromEvent(ev))
		case uzicli.RunEventTypeState:
			// The authoritative status. A synthetic frame from StreamRun's reconcile is
			// indistinguishable from a live one here, which is the point — the recovery
			// path must not need its own branch.
			if ev.Status != "" {
				d.run.Status = ev.Status
			}
		case uzicli.RunEventTypeInput:
			// Carries no data by design (the steer text is owner-gated and never rides
			// the socket): it is a prompt to re-read GET /runs/{id}/inputs.
			inputChanged = true
		}
		// A health frame likewise carries no data; the next reconcile/poll picks it up.
	}
	d.rebuild()
	return inputChanged
}

// addFrame appends a message frame, deduped by seq. Dedup is required, not defensive:
// a reconnect replays from the last seq seen and the live socket may deliver the same
// frame, and a duplicate would double a lane's contribution.
func (d *detailState) addFrame(f laneFrame) {
	if f.Seq > 0 {
		if d.seen[f.Seq] {
			return
		}
		d.seen[f.Seq] = true
	}
	d.frames = append(d.frames, f)
}

func (d *detailState) rebuild() {
	d.lanes = buildLanes(d.frames)
	if d.laneIdx >= len(d.lanes) {
		d.laneIdx = len(d.lanes) - 1
	}
	if d.laneIdx < 0 {
		d.laneIdx = 0
	}
}

func (d *detailState) selectedLane() (agentLane, bool) {
	if d.laneIdx < 0 || d.laneIdx >= len(d.lanes) {
		return agentLane{}, false
	}
	return d.lanes[d.laneIdx], true
}

func (m tuiModel) detailKey(k string) (tea.Model, tea.Cmd) {
	// The overlay and the steer bar get first refusal, in that order: while either is
	// in an input/confirm mode it must swallow keys that would otherwise be pane
	// navigation, or typing "l" into a follow-up would move pane focus underneath it.
	if nm, cmd, handled := m.reviewKey(k); handled {
		return nm, cmd
	}
	if nm, cmd, handled := m.steerKey(k); handled {
		return nm, cmd
	}
	if k == "v" {
		m.detail.review.open = true
		if m.detail.review.review == nil && !m.detail.review.loading {
			m.detail.review.loading = true
			return m, m.loadReviewCmd(m.detail.runID)
		}
		return m, nil
	}
	switch k {
	case keyEsc:
		if m.detail.stream != nil {
			m.detail.stream.Close()
		}
		m.view = viewBoard
		m.detail = detailState{}
		return m, m.fetchRunsCmd(m.board.admin)
	case keyRefresh:
		return m, m.loadDetailCmd(m.detail.runID)
	case keyCollapseCrew:
		// Fold / unfold the crew list so the milestone block below it is always reachable
		// (the rail is height-clamped and does not scroll). No-op with no lanes: there is
		// nothing to fold and the caret/hint are hidden, so toggling would only leave a run
		// that later gains lanes silently opening collapsed.
		if len(m.detail.lanes) > 0 {
			m.detail.railCollapsed = !m.detail.railCollapsed
		}
		return m, nil
	case keyTab:
		// tab cycles focus between the two panes.
		m.detail.focus = 1 - m.detail.focus
		return m, nil
	case "h", keyLeft:
		m.detail.focus = focusRail
		return m, nil
	case "l", keyRight:
		m.detail.focus = focusTranscript
		return m, nil
	case keyGoLive:
		// g: re-attach follow and jump to the newest output (M5). f is already follow-up.
		m.detail.follow = true
		return m, nil
	}
	if d := motionDelta(k); d != 0 {
		if m.detail.focus == focusRail {
			// Move between agents. One step per press regardless of the motion size
			// (page keys are meaningless for a short lane list). Switching lanes re-arms
			// follow so the new lane opens tailing its newest output.
			if n := len(m.detail.lanes); n > 0 {
				step := 1
				if d < 0 {
					step = -1
				}
				m.detail.laneIdx = (m.detail.laneIdx + step + n) % n
				m.detail.scroll, m.detail.follow = 0, true
			}
			return m, nil
		}
		// Transcript focused: bottom-anchored scroll (M5). Scrolling UP detaches follow and
		// moves the window's top up; scrolling DOWN moves it toward the bottom and re-arms
		// follow on a LIVE run once it reaches the newest line.
		total, vp := m.transcriptExtent()
		maxTop := total - vp
		if maxTop < 0 {
			maxTop = 0
		}
		// F-M5a: reclamp the stored top against the CURRENT extent BEFORE applying the
		// delta. A resize (WindowSizeMsg) since it was set can leave scroll above the new
		// maxTop; applying the delta to that stale value would push it past the bottom clamp
		// below and wrongly re-arm follow on the next key instead of scrolling to older
		// output.
		if m.detail.scroll > maxTop {
			m.detail.scroll = maxTop
		}
		if m.detail.scroll < 0 {
			m.detail.scroll = 0
		}
		if d < 0 { // up, toward older
			if m.detail.follow {
				m.detail.follow = false
				m.detail.scroll = maxTop
			}
			m.detail.scroll += d // d is negative
		} else { // down, toward newest
			if m.detail.follow {
				return m, nil // already at the bottom
			}
			m.detail.scroll += d
		}
		if m.detail.scroll < 0 {
			m.detail.scroll = 0
		}
		if m.detail.scroll >= maxTop {
			m.detail.scroll = maxTop
			if isLiveRunStatus(m.detail.run.Status) {
				m.detail.follow = true
			}
		}
		return m, nil
	}
	return m, nil
}

func (m tuiModel) renderDetail() string {
	d := &m.detail
	var sb strings.Builder

	// Header: id + a kind chip + a semantic STATUS chip (PRD #325 M3, reading M2's
	// statusColor/chip seam). "stalled" already turns the status chip orange via the
	// precedence rule; because that colour vanishes under NO_COLOR, M4 appends a
	// NO_COLOR-safe cue for it (▲ + "stalled") as well as the word for any other non-ok
	// health, so no health state is lost when colour is stripped.
	head := m.pal.faint.Render("run ") + m.pal.title.Render(shortRunID(d.runID))
	if d.run.ID != "" {
		if d.run.Kind != "" {
			head += "  " + m.pal.chip(m.renderer.Plain(d.run.Kind, 10), m.pal.title.GetForeground())
		}
		es := effectiveRunStatus(d.run.Status, d.run.IsPlanning)
		head += "  " + m.pal.chip(m.renderer.Plain(es, 18), m.pal.statusColor(es, d.run.Health))
		if h := d.run.Health; h != "" && h != "ok" {
			// A NO_COLOR-safe health cue (M4 review nit): without it a stalled run's only
			// header signal is the orange chip colour, which vanishes under an Ascii
			// profile. "stalled" gets a ▲ glyph + word (the glyph survives the colour
			// strip); any other non-ok health shows its word, as the board does.
			if h == "stalled" {
				head += "  " + lipgloss.NewStyle().Foreground(m.pal.statusStalled).Render("▲ "+m.renderer.Plain(h, 14))
			} else {
				head += "  " + m.renderer.Plain(h, 14)
			}
		}
		// Elapsed time: how long a live run has been going, or a terminal run's total wall
		// time. Sits before the title so a narrow terminal that truncates the (capped) title
		// still shows the duration.
		if dur := runDuration(d.run, time.Now()); dur != "" {
			head += "  " + m.pal.faint.Render(dur)
		}
		head += "  " + m.pal.faint.Render(m.renderer.Plain(runTitle(d.run), 60))
	}
	// The healthy/transient transport state folds into the header (see transportHeaderTag) so
	// it does not cost its own row; a degradation gets a full line below instead.
	if tag := m.transportHeaderTag(); tag != "" {
		head += "  " + tag
	}
	// Clamp to the terminal width so the header is always exactly ONE physical row. It is
	// otherwise unbounded (a long title, plus the duration and the folded transport tag), and
	// a wrap would make transcriptViewport — which counts the header as one row — under-count
	// and push the footer off the bottom, the #379 invariant this view protects.
	sb.WriteString(clampVisual(head, m.width) + "\n")

	// The park line (PRD #35). The status word alone is already in the header, and it
	// is not enough: "limit_wait" tells a user their run stopped and nothing about
	// whether it is coming back. This is the countdown, drawn in the crewWaiting colour
	// so it matches the dot every lane in the rail below is about to show.
	//
	// It sits ABOVE the transport line rather than beside it because the two describe
	// different things and would be read as one if adjacent: the transport line is
	// about THIS CLIENT's connection ("live", "reconnecting"), and a park is a fact
	// about the run that a perfectly healthy socket reports. Same reason the countdown
	// is not appended to the header — a run that is both parked and being polled has
	// two independent things to say.
	if line := limitWaitLine(d.run, time.Now()); line != "" {
		sb.WriteString(m.pal.state(crewWaiting).Render(m.renderer.Plain(line, 120)) + "\n")
	}

	// The transport line is never silent about a degradation: a user watching a stale pane
	// must see WHY it is stale. The healthy/transient states ("live", "connecting…") are the
	// header tag above; only a degradation takes a full row here, where its longer text fits.
	if tline := m.transportLine(); tline != "" {
		sb.WriteString(clampVisual(tline, m.width) + "\n") // one physical row (a long streamErr must not wrap)
	}
	sb.WriteString("\n")

	if d.loadErr != nil {
		sb.WriteString(m.pal.faint.Render("could not load this run: " + fmtErr(d.loadErr)))
		return sb.String()
	}
	if !d.loaded {
		sb.WriteString(m.pal.faint.Render("loading…"))
		return sb.String()
	}

	if m.detail.review.open {
		return sb.String() + m.renderReviewOverlay()
	}

	rail := m.renderLaneRail()
	body := m.renderTranscript()
	sb.WriteString(joinColumns(rail, body, laneRailWidth))
	// The attention banner (PRD #325 M3) shows regardless of ownership — it is
	// informational. The owner-gated action keys live in the footer below it.
	if b := m.detailBanner(); b != "" {
		sb.WriteString(b + "\n")
	}
	// Steer status (queue indicator, notice, typing input, confirm box, or the read-only
	// reason) renders above the footer when present. When the bar is mid-input it owns the
	// key hints, so the single combined footer is drawn only when the bar is idle (M4).
	if steer := m.renderSteerBar(); steer != "" {
		sb.WriteString(steer + "\n")
	}
	if m.detail.steer.mode == steerIdle {
		sb.WriteString(clampVisual(m.detailFooter(), m.width)) // one physical row on a narrow terminal
	}
	return sb.String()
}

// transportHeaderTag is the compact connection tag folded into the header for the healthy
// and transient states ("● live", "connecting…"), so the steady case does not cost a whole
// row. Empty in a degraded state — transportLine draws that on its own line instead, and the
// two are mutually exclusive, so exactly one of them is non-empty for any transport state.
func (m tuiModel) transportHeaderTag() string {
	d := &m.detail
	// A terminal run will never produce more output, so it carries no transport chrome at all
	// (neither this tag nor transportLine): "connecting…"/"reconnecting…" on a completed run is
	// noise. The stream still opens to replay history, then closes — that close must not read as
	// a degradation here.
	if isTerminalRunStatus(d.run.Status) {
		return ""
	}
	switch {
	case d.streamErr != nil && d.polling, d.polling:
		return "" // degraded: shown on its own line by transportLine
	case d.stream != nil:
		return m.pal.state(crewWorking).Render("● live")
	default:
		return m.pal.faint.Render("connecting…")
	}
}

// transportLine is the full-width transport line, drawn ONLY for a degradation (the socket
// is unusable and the view fell back to REST polling): the healthy/transient states fold
// into transportHeaderTag. Kept as its own line because the explanation is long and a user
// watching a stale pane must be able to read WHY it is stale. Pure — transcriptViewport
// calls it to keep the chrome accounting and the render in lockstep.
func (m tuiModel) transportLine() string {
	d := &m.detail
	// A terminal run shows no transport chrome (see transportHeaderTag): its stream closing is
	// expected, not a degradation to reconnect from.
	if isTerminalRunStatus(d.run.Status) {
		return ""
	}
	switch {
	case d.streamErr != nil && d.polling:
		return m.pal.faint.Render("live stream unavailable (" + fmtErr(d.streamErr) + ") — falling back to a 2s refresh")
	case d.polling:
		return m.pal.faint.Render("reconnecting — refreshing every 2s")
	default:
		return ""
	}
}

// detailFooter is the single-line keymap (PRD #325 M4): pane/scroll navigation combined
// with the owner's steer actions, with approve/reject leading at a plan gate. The steer
// bar's interactive modes draw their own hints, so this is only emitted when idle.
func (m tuiModel) detailFooter() string {
	owner := m.detail.steer.access == steerAllowed
	var parts []string
	if owner && atPlanGate(m.detail.run) {
		parts = append(parts, m.keyHint("y", "approve"), m.keyHint("n", "reject"),
			m.keyHint("f", "follow-up"), m.keyHint("x", "cancel"),
			m.keyHint("←→", "pane"), m.keyHint("↑↓", "move"))
	} else {
		parts = append(parts, m.keyHint("←→", "pane"), m.keyHint("↑↓", "move"))
		if len(m.detail.lanes) > 0 {
			parts = append(parts, m.keyHint("c", "crew"))
		}
		if isLiveRunStatus(m.detail.run.Status) {
			// g re-attaches the transcript follow (M5); it is a view affordance, so it shows
			// for owner and non-owner alike, but only when there is live output to follow.
			parts = append(parts, m.keyHint("g", "live"))
		}
		if owner {
			parts = append(parts, m.keyHint("f", "follow-up"), m.keyHint("v", "review"), m.keyHint("x", "cancel"))
		}
	}
	parts = append(parts, m.keyHint("esc", "back"), m.keyHint("?", "keys"))
	return strings.Join(parts, m.pal.faint.Render(" · "))
}

// keyHint is a compact bright-key / muted-label footer hint.
func (m tuiModel) keyHint(k, label string) string {
	return m.pal.title.Render(k) + m.pal.faint.Render(" "+label)
}

// paneTitle renders a detail pane's title with a focus indicator: a bright brand bar + bold
// title when focused, a dim title otherwise (M4).
func (m tuiModel) paneTitle(title string, focused bool) string {
	if focused {
		return m.pal.title.Render("▎" + strings.ToUpper(title))
	}
	return " " + m.pal.faint.Render(strings.ToUpper(title))
}

// detailBanner is the S3 two-banner treatment: awaiting_approval gets the PLAN GATE banner
// (approve/reject, owner-gated keys in the steer bar); awaiting_input gets a DISTINCT
// needs-input banner that does NOT offer y/n — those keys do nothing at a clarification
// park, which is answered off-TUI (run answer / web / Slack). It shows for owner and
// non-owner alike; only the promoted keys below are gated.
func (m tuiModel) detailBanner() string {
	switch m.detail.run.Status {
	case "awaiting_approval":
		return m.attentionBanner("⚑  PLAN GATE", "this run is waiting on your approval")
	case "awaiting_input":
		return m.attentionBanner("✎  NEEDS INPUT", "the agent asked a question; answer it from another terminal, the web, or Slack")
	}
	return ""
}

// attentionBanner draws a BORDERED amber banner. The border is the NO_COLOR fallback (D3):
// under an Ascii profile the amber fill/foreground is stripped but the box and its bold
// text survive, so the gate stays structurally unmissable (SC2) without colour.
func (m tuiModel) attentionBanner(head, body string) string {
	c := m.pal.statusColor("awaiting_approval", "") // the needs-you colour (amber)
	inner := m.width - 4
	if inner < 20 {
		inner = 20
	}
	text := lipgloss.NewStyle().Bold(true).Foreground(c).Render(head) + m.pal.faint.Render("  ·  ") + body
	return lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(c).Padding(0, 1).Width(inner).Render(text)
}

func (m tuiModel) renderLaneRail() string {
	d := &m.detail
	now := time.Now()
	active := activeLaneKey(d.run.Status, d.frames)

	var sb strings.Builder
	// A caret advertises the collapse state (`c`) once there is a roster to collapse: ▾ open,
	// ▸ with the lane count when folded to a summary + the selected lane.
	title := m.paneTitle("crew", d.focus == focusRail)
	if len(d.lanes) > 0 {
		if d.railCollapsed {
			title += m.pal.faint.Render("  " + itoa(len(d.lanes)) + " ▸")
		} else {
			title += m.pal.faint.Render("  ▾")
		}
	}
	sb.WriteString(title + "\n")

	if len(d.lanes) == 0 {
		sb.WriteString(m.pal.faint.Render("(no activity yet)"))
		// A milestone-structured run has a frozen list before any frame arrives (queued /
		// just-claimed), so the block must show even with no lanes yet.
		if block := m.renderMilestones(); block != "" {
			sb.WriteString("\n\n" + block)
		}
		return sb.String()
	}

	if d.railCollapsed {
		// Just the selected lane, so the reader still knows whose transcript is on screen while
		// the milestones get the rest of the column.
		if l, ok := d.selectedLane(); ok {
			st := crewStateFor(d.run.Status, d.run.Health, l.Key, active, l.LastActivity, now)
			sb.WriteString(m.laneRow(l, true, st))
		}
	} else {
		for i, l := range d.lanes {
			st := crewStateFor(d.run.Status, d.run.Health, l.Key, active, l.LastActivity, now)
			sb.WriteString(m.laneRow(l, i == d.laneIdx, st))
		}
	}
	if block := m.renderMilestones(); block != "" {
		sb.WriteString("\n" + block)
	}
	return sb.String()
}

// laneRow renders one crew-rail row: the status dot, the model-authored role and (untrusted)
// instance id, and the optional label line beneath it. `selected` adds the ▸ marker. The
// role, id and label are all UNTRUSTED and go through renderer.Plain (D7) — keeping every
// cell's sanitizing in this one helper is why the collapsed and expanded paths share it.
func (m tuiModel) laneRow(l agentLane, selected bool, st crewState) string {
	name := m.renderer.Plain(l.Role, 14)
	if l.Key != l.Role {
		// The instance id is UNTRUSTED like everything else on this rail: it is the SDK's
		// parent_tool_use_id, forwarded verbatim. shortInstanceID only takes a tail — it does
		// not sanitize — so the result goes through Plain like every other cell. Rendering it
		// raw was a real hole, caught by TestTUIViewsStripControlBytesFromUntrustedText.
		name += m.pal.faint.Render("·" + m.renderer.Plain(shortInstanceID(l.Key), 8))
	}
	row := m.pal.state(st).Render(laneDot(st)) + " " + name
	var sb strings.Builder
	if selected {
		sb.WriteString(m.pal.sel.Render("▸" + row))
	} else {
		sb.WriteString(" " + row)
	}
	sb.WriteString("\n")
	if l.Label != "" {
		sb.WriteString("   " + m.pal.faint.Render(m.renderer.Plain(l.Label, laneLabelCap)) + "\n")
	}
	return sb.String()
}

// milestoneTitleCap is the per-row title budget in the crew rail (laneRailWidth 26 minus
// the " ✓ " glyph prefix). joinColumns clamps the whole left column anyway, but Plain must
// cap the UNTRUSTED title itself (D7), so it caps at the width the rail can actually show.
const milestoneTitleCap = 22

// milestoneProgress folds a run's frozen milestone list into (done, total, reported) — the
// TUI twin of the web's milestoneBadge, shared by the crew-rail block and the board badge so
// the two surfaces cannot disagree. `done` counts frozen MEMBERS present in the completed
// set (immune to a duplicate id and to a completed id naming a milestone dropped after it
// was ticked). `reported` is whether ANY completion was ever reported: a nil completed slice
// (JSON null) means never, so an unreported run reads `–/N` rather than a `0/N` that looks
// like failure. total is 0 for a run with no frozen list — the caller renders nothing.
func milestoneProgress(run apitypes.RunDTO) (done, total int, reported bool) {
	total = len(run.Milestones)
	if total == 0 {
		return 0, 0, false
	}
	completed := make(map[string]bool, len(run.MilestonesCompleted))
	for _, id := range run.MilestonesCompleted {
		completed[id] = true
	}
	for _, mi := range run.Milestones {
		if completed[mi.ID] {
			done++
		}
	}
	return done, total, run.MilestonesCompleted != nil
}

// milestoneCount is the compact `2/4` (or `–/4` when nothing was reported) shared by the
// crew-rail block's summary and the board's `M…` badge.
func milestoneCount(done, total int, reported bool) string {
	if !reported {
		return "–/" + itoa(total)
	}
	return itoa(done) + "/" + itoa(total)
}

// renderMilestones is the crew rail's milestone progress block (the TUI twin of the web's
// MilestoneChecklist and the CLI `uzi run get` milestoneRows): a compact `{done}/{total}`
// summary and one glyph-marked row per milestone in FROZEN order — done ✓, in progress ◐,
// not started ○.
//
// Empty for a run with no frozen milestone list, so a pre-#122 (or non-milestone) run's
// rail is byte-for-byte unchanged — the same back-compat contract the nil Milestones slice
// carries elsewhere.
//
// The count says neither "verified" nor "complete" (PRD #122 Decision 6): the worker only
// REPORTS a milestone done and nothing in uzi has verified it, so the bare N/M and the ✓
// glyph must not imply verification. A run that has reported nothing shows `–/N`, not a
// `0/N` that reads as failure (matching the web's `–/N` treatment). `done` counts frozen
// members present in the completed set, immune to a duplicate id and to a completed id
// that names a milestone dropped after it was ticked — the same rule milestoneBadge uses.
//
// Titles are UNTRUSTED repo/agent-authored text (apitypes.Milestone.Title), so each goes
// through renderer.Plain — the D7 obligation the lane rail's Role/Label already carry, and
// why "Title" is in d7UntrustedFields.
func (m tuiModel) renderMilestones() string {
	ms := m.detail.run.Milestones
	if len(ms) == 0 {
		return ""
	}
	completed := make(map[string]bool, len(m.detail.run.MilestonesCompleted))
	for _, id := range m.detail.run.MilestonesCompleted {
		completed[id] = true
	}
	inProgress := make(map[string]bool, len(m.detail.run.MilestonesInProgress))
	for _, id := range m.detail.run.MilestonesInProgress {
		inProgress[id] = true
	}
	done, total, reported := milestoneProgress(m.detail.run)

	var sb strings.Builder
	sb.WriteString(m.pal.title.Render("MILESTONES") + " " + m.pal.faint.Render(milestoneCount(done, total, reported)) + "\n")
	for _, mi := range ms {
		glyph := m.pal.faint.Render("○") // not started
		style := m.pal.faint
		switch {
		case completed[mi.ID]:
			glyph = m.pal.state(crewWorking).Render("✓")
			// done → muted, like the web's text-muted; the ✓ glyph carries completion, so no
			// strikethrough (which lipgloss emits per-rune, bloating the frame for no signal).
			style = m.pal.faint
		case inProgress[mi.ID]:
			glyph = m.pal.state(crewWaiting).Render("◐")
			style = lipgloss.NewStyle() // current — plain terminal fg, like the web's text-fg
		}
		sb.WriteString(" " + glyph + " " + style.Render(m.renderer.Plain(mi.Title, milestoneTitleCap)) + "\n")
	}
	return strings.TrimRight(sb.String(), "\n")
}

// buildTranscriptLines renders the selected lane's frames to display lines (no windowing),
// so the follow/scroll windowing and the line-count extent share one layout.
func (m tuiModel) buildTranscriptLines(lane agentLane) []string {
	var sb strings.Builder
	for _, f := range lane.Frames {
		sb.WriteString(m.pal.faint.Render("#"+itoa(int(f.Seq))+" "+m.renderer.Plain(f.Kind, 16)) + "\n")
		sb.WriteString(m.renderer.Markdown(transcriptText(f)) + "\n\n")
	}
	return strings.Split(strings.TrimRight(sb.String(), "\n"), "\n")
}

// transcriptViewport is the transcript's visible line budget (the pane title is a fixed
// header above it, so it is one row shorter than the pane).
//
// The budget is the terminal height minus every OTHER row the detail view draws, so the
// two-pane body fills the screen down to the footer (issue #379) rather than stopping at the
// last line of content and leaving a tall terminal with dead space below the footer. It
// counts the SAME chrome renderDetail emits — header, optional park line, an optional degraded
// transport line (a healthy "live"/"connecting…" folds into the header), the blank separator,
// the pane title row, an optional attention banner, an optional steer bar, and the footer — so
// this and the render cannot disagree; both the window render and the scroll clamp read it.
// renderSteerBar/detailBanner/transportLine are pure and never call back here (no recursion).
func (m tuiModel) transcriptViewport() int {
	lineCount := func(s string) int {
		if s == "" {
			return 0
		}
		return strings.Count(s, "\n") + 1
	}
	chrome := 1 // header line (run id + chips)
	if limitWaitLine(m.detail.run, time.Now()) != "" {
		chrome++ // the park / limit-wait line
	}
	if m.transportLine() != "" {
		chrome++ // the degraded transport line (a healthy "live"/"connecting…" folds into the header)
	}
	chrome++ // the blank separator line before the body
	chrome++ // the transcript pane's own title row (the first line of the body column)
	chrome += lineCount(m.detailBanner())
	chrome += lineCount(m.renderSteerBar())
	if m.detail.steer.mode == steerIdle {
		chrome++ // the footer (suppressed while the steer bar owns the key hints)
	}
	if h := m.height - chrome; h > 3 {
		return h
	}
	return 3
}

// padLinesToViewport joins lines and pads with blank lines to exactly vp rows, so the
// transcript column always fills the viewport height and the pane divider joinColumns draws
// beside it reaches the footer even when the run has fewer lines than the viewport (#379).
func padLinesToViewport(lines []string, vp int) string {
	out := make([]string, 0, vp)
	out = append(out, lines...)
	for len(out) < vp {
		out = append(out, "")
	}
	return strings.Join(out, "\n")
}

// transcriptExtent gives the selected lane's total line count and the viewport, so the key
// handler can clamp the scroll offset against the same layout renderTranscript uses.
func (m tuiModel) transcriptExtent() (total, viewport int) {
	viewport = m.transcriptViewport()
	if lane, ok := m.detail.selectedLane(); ok {
		total = len(m.buildTranscriptLines(lane))
	}
	return total, viewport
}

func (m tuiModel) renderTranscript() string {
	focused := m.detail.focus == focusTranscript
	lane, ok := m.detail.selectedLane()
	if !ok {
		return m.paneTitleBadge("transcript", focused, "") + "\n" +
			padLinesToViewport([]string{m.pal.faint.Render("no lane selected")}, m.transcriptViewport())
	}
	lines := m.buildTranscriptLines(lane)
	vp := m.transcriptViewport()

	// Bottom-anchored window (PRD #325 M5). Following pins the window to the bottom
	// (auto-tail); paused holds a fixed top line so the view does not jump as frames land
	// below it, and the count of lines below the fold is what the badge reports.
	maxTop := len(lines) - vp
	if maxTop < 0 {
		maxTop = 0
	}
	top := m.detail.scroll
	if m.detail.follow || top > maxTop {
		top = maxTop
	}
	if top < 0 {
		top = 0
	}
	end := top + vp
	if end > len(lines) {
		end = len(lines)
	}
	// Pad to the full viewport so the transcript column, and the pane divider beside it,
	// reach the bottom of the screen even when the content is shorter than the viewport (#379).
	window := padLinesToViewport(lines[top:end], vp)

	return m.paneTitleBadge("transcript", focused, m.followBadge(top, maxTop)) + "\n" + window
}

// followBadge is the transcript's live-follow affordance (M5) — distinct from the transport
// line, which is about THIS CLIENT's connection. Only a LIVE run tails: a terminal run's
// transcript is complete, so it carries no badge. FOLLOWING while auto-tailing; PAUSED with
// a "↓N new" count (N = lines below the fold) once the reader has scrolled back.
func (m tuiModel) followBadge(top, maxTop int) string {
	if !isLiveRunStatus(m.detail.run.Status) {
		return ""
	}
	if m.detail.follow {
		return lipgloss.NewStyle().Foreground(m.pal.statusColor("running", "")).Bold(true).Render("● FOLLOWING")
	}
	badge := lipgloss.NewStyle().Foreground(m.pal.statusColor("awaiting_approval", "")).Bold(true).Render("⏸ PAUSED")
	if below := maxTop - top; below > 0 {
		badge += m.pal.faint.Render(" ↓" + itoa(below) + " new")
	}
	return badge
}

// paneTitleBadge is paneTitle with a right-aligned badge (the follow indicator) padded to
// the transcript column width.
func (m tuiModel) paneTitleBadge(title string, focused bool, badge string) string {
	t := m.paneTitle(title, focused)
	if badge == "" {
		return t
	}
	return padVisual(t, m.transcriptWidth()-visualWidth(badge)) + badge
}

// isLiveRunStatus reports whether a run is actively producing output, so follow-live
// applies. `claimed` is live like `running` (a worker has it and is about to speak).
func isLiveRunStatus(status string) bool {
	return status == "running" || status == "claimed"
}

// transcriptText pulls the human-readable body out of a frame's payload. The payload
// is server-forwarded run content and is DATA: it is rendered, never interpreted, and
// an unrecognised shape falls back to the compacted raw JSON rather than being hidden.
func transcriptText(f laneFrame) string {
	if len(f.Payload) == 0 {
		return ""
	}
	var p struct {
		Text  string `json:"text"`
		Name  string `json:"name"`
		Input any    `json:"input"`
	}
	if err := json.Unmarshal(f.Payload, &p); err == nil {
		if p.Text != "" {
			return p.Text
		}
		if p.Name != "" {
			return "`" + p.Name + "`"
		}
	}
	return compactText(string(f.Payload))
}

// joinColumns places the rail beside the body at a fixed gutter. The row count is the RIGHT
// (transcript) column's height, NOT max(left, right): the transcript is padded to exactly the
// viewport (padLinesToViewport), so it is the height budget for the whole two-pane body. A
// rail TALLER than that — a run with many lanes plus a long milestone block — is truncated to
// it rather than pushing the total past the terminal height and clipping the footer below the
// body (issue #379: the footer carries pane nav / esc / ? and must always render). A shorter
// rail is padded, which is what fills the divider to the bottom.
func joinColumns(left, right string, width int) string {
	l := strings.Split(left, "\n")
	r := strings.Split(right, "\n")
	n := len(r)
	var sb strings.Builder
	for i := 0; i < n; i++ {
		var lv, rv string
		if i < len(l) {
			lv = l[i]
		}
		if i < len(r) {
			rv = r[i]
		}
		sb.WriteString(padVisual(clampVisual(lv, width), width) + " │ " + rv + "\n")
	}
	return sb.String()
}

// clampVisual truncates s to n visual columns (ANSI- and wide-rune-aware),
// appending an ellipsis when it cuts. It is padVisual's dual: together they hold
// a column to exactly n columns regardless of content, so joinColumns' divider
// sits at one fixed column on every row.
func clampVisual(s string, n int) string {
	if n < 1 {
		return ""
	}
	return ansi.Truncate(s, n, "…")
}

// padVisual pads to n columns ignoring ANSI escapes, which lipgloss has already added
// by this point — counting them as width would shove every row right.
func padVisual(s string, n int) string {
	w := visualWidth(s)
	if w >= n {
		return s
	}
	return s + strings.Repeat(" ", n-w)
}

// visualWidth is lipgloss's own width measurement.
//
// It was a hand-rolled rune count that skipped ANSI sequences, and that was wrong in a
// way a CLI table can tolerate but a two-column TUI cannot: a CJK or emoji rune occupies
// TWO terminal columns and was counted as one, so every such rune in a transcript walked
// the split pane's right column one place left. lipgloss.Width delegates to
// ansi.StringWidth, which is both ANSI-aware and wide-rune-aware, and lipgloss v2 is
// already a direct dependency — the correct implementation was linked into the binary
// the whole time.
func visualWidth(s string) int { return lipgloss.Width(s) }

// runDuration is the run's elapsed WORK time for the header: a live run's time since it
// started, or a terminal run's total from start to finish. Start is StartedAt, or ClaimedAt
// during the brief claimed-but-not-started window; end is FinishedAt when set, else now.
//
// CreatedAt is deliberately NOT a fallback: it would turn a queued run's header into its
// queue-WAIT age dressed up as run-elapsed time (and a just-created run into a literal "0s"),
// conflating two different clocks. A run with no start stamp yet (queued) shows nothing.
func runDuration(run apitypes.RunDTO, now time.Time) string {
	var start time.Time
	switch {
	case run.StartedAt != nil && !run.StartedAt.IsZero():
		start = *run.StartedAt
	case run.ClaimedAt != nil && !run.ClaimedAt.IsZero():
		start = *run.ClaimedAt
	default:
		return "" // not started yet: no work-elapsed time to show
	}
	end := now
	if run.FinishedAt != nil && !run.FinishedAt.IsZero() {
		end = *run.FinishedAt
	}
	return shortDuration(end.Sub(start))
}

// shortDuration formats an elapsed duration compactly for the header: "45s", "12m", "3h4m",
// "2d5h". Negative clamps to "0s". It carries the second unit (unlike relAge, which is
// single-unit for the queue-age column) because a run's total reads better as "3h4m" than "3h".
func shortDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return itoa(int(d.Seconds())) + "s"
	case d < time.Hour:
		return itoa(int(d.Minutes())) + "m"
	case d < 24*time.Hour:
		h := int(d.Hours())
		if mins := int(d.Minutes()) % 60; mins != 0 {
			return itoa(h) + "h" + itoa(mins) + "m"
		}
		return itoa(h) + "h"
	default:
		days := int(d.Hours()) / 24
		if h := int(d.Hours()) % 24; h != 0 {
			return itoa(days) + "d" + itoa(h) + "h"
		}
		return itoa(days) + "d"
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}
