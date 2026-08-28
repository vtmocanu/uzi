package main

import (
	"encoding/json"
	"image/color"
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

// railRateBarWidth is the full-rail account meter's bar. The rail row is width-locked to
// laneRailWidth (26); its budget is: "5h " label+space (3) + bar + " NN%" percent field (5) +
// 2-col gap (2) + 6-col right-aligned reset countdown (6). The bar takes what is left (issue #588
// shrank it from 18 to 10 to make room for the inline reset). A window with no reset time renders
// no countdown and the row is simply shorter (still <= 26).
const railRateBarWidth = laneRailWidth - 3 - 5 - 2 - 6 // 10

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
	if d.seen == nil {
		// Total on a zero-value detailState: newDetailState seeds this map, but the
		// handler guard is the load-bearing fix — this keeps a nil map from panicking
		// the program if any future path reaches addFrame without the constructor.
		d.seen = map[int32]bool{}
	}
	if f.Seq > 0 {
		if d.seen[f.Seq] {
			return
		}
		d.seen[f.Seq] = true
	}
	d.frames = append(d.frames, f)
}

func (d *detailState) rebuild() {
	// The user's selection is tracked by lane KEY, not index: prepending the aggregated lane at
	// index 0 (below) shifts every real lane down one the instant a run grows from 1 to ≥2 lanes,
	// and re-pinning the same index would silently swap the transcript the user is reading for the
	// firehose. Capture the selected key before the rebuild and restore it after.
	var selKey string
	if d.laneIdx >= 0 && d.laneIdx < len(d.lanes) {
		selKey = d.lanes[d.laneIdx].Key
	}
	d.lanes = buildLanes(d.frames)
	// Prepend the aggregated "all agents" lane once a run has ≥2 real lanes, so index 0 is the
	// firehose and the individual lanes follow for isolating one. On the FIRST build there is no
	// prior selection (selKey ""), so the restore below is skipped and the default index 0 lands on
	// the firehose — the intended opening view for a multi-lane run.
	if len(d.lanes) >= 2 {
		d.lanes = append([]agentLane{allLane(d.frames)}, d.lanes...)
	}
	if selKey != "" {
		for i, l := range d.lanes {
			if l.Key == selKey {
				d.laneIdx = i
				break
			}
		}
	}
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

// exitToBoard leaves the run detail and returns to the factory-floor board: it closes the live
// stream, drops the detail state, and refetches the board. Shared by esc and by ← at the left
// pane boundary so the two cannot drift.
func (m tuiModel) exitToBoard() (tea.Model, tea.Cmd) {
	if m.detail.stream != nil {
		m.detail.stream.Close()
	}
	m.view = viewBoard
	m.detail = detailState{}
	return m, m.fetchRunsCmd(m.board.admin)
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
		return m.exitToBoard()
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
		// ← from the transcript moves focus to the crew rail; ← again from the rail (the leftmost
		// pane) backs out of the run and returns to the factory floor — the natural "back out" at
		// the left boundary, the mirror of the board's → drill-in, and alongside esc.
		if m.detail.focus == focusRail {
			return m.exitToBoard()
		}
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

// joinTags joins the non-empty parts with sep, so an absent tag (no credential, no transport)
// leaves no stray separator.
func joinTags(sep string, parts ...string) string {
	out := ""
	for _, p := range parts {
		if p == "" {
			continue
		}
		if out != "" {
			out += sep
		}
		out += p
	}
	return out
}

// detailHeaderLines builds the priority header (PRD #325 M3): the breadcrumb + run id + kind, the
// run TITLE as the bold headline, WHICH Anthropic credential the run spent (PRD #295), the run
// status token + duration, and the transport tag ("● live"). It ALWAYS returns exactly one line
// (issue #666): the breadcrumb (left) and the status + transport block (right) keep their full width
// and the TITLE takes the middle, truncating with "…" when it does not fit — so the title is the
// field cut before the status, which renders in full at any width that can hold it at all (below
// ~28 cols even the right block cannot fit and is itself clamped — inherent to one row). The
// returned line is clamped to exactly one physical row (the #379 invariant); transcriptViewport
// uses len() of this to keep its budget in step.
func (m tuiModel) detailHeaderLines() []string {
	d := &m.detail

	crumb := m.pal.faint.Render("‹ floor") + m.pal.faint.Render("  run ") + m.pal.title.Render(shortRunID(d.runID))
	if d.run.ID != "" && d.run.Kind != "" {
		crumb += m.pal.faint.Render(" · " + m.renderer.Plain(d.run.Kind, 12))
	}
	if d.run.IssueIID != nil {
		styledIID := m.pal.title.Render("#" + itoa(int(*d.run.IssueIID)))
		crumb += m.pal.faint.Render(" · ") + m.issueLink(d.run, styledIID)
	}

	// The healthy/transient transport tag; a transport DEGRADATION still takes its own line
	// below. The credential label (PRD #295) was dropped from this row in PRD #623 — the
	// account the run is spending is now called out in the rail ACCOUNTS block instead,
	// freeing this row so the title flows into the reclaimed width.
	transportTag := m.transportHeaderTag()
	// The status token (glyph + human word, its state colour; the glyph is the NO_COLOR twin) plus
	// elapsed time — a live run's runtime or a terminal run's total wall time.
	statusTag := ""
	if d.run.ID != "" {
		// RunDTO carries no is_revising (issue #750): the detail header keeps its own
		// derivePlanRevision panel, so revising is not surfaced through this token here.
		tok := m.pal.stateToken(d.run.Status, d.run.Health, d.run.IsPlanning, false)
		statusTag = lipgloss.NewStyle().Foreground(tok.color).Render(tok.glyph + " " + tok.word)
		if dur := runDuration(d.run, time.Now()); dur != "" {
			statusTag += m.pal.faint.Render(" · " + dur)
		}
	}
	if d.run.Usage != nil {
		cost := "—" // subscription-auth $0 renders "—", never "$0.00" (web money() convention)
		if d.run.Usage.CostUSD > 0 {
			cost = fmtCostCents(d.run.Usage.CostUSD)
		}
		statusTag += m.pal.faint.Render(" · " + cost)
	}

	sep := m.pal.faint.Render("   ")
	combinedRight := joinTags(sep, statusTag, transportTag) // status, transport — pinned right, never truncated
	title := m.renderer.Plain(runTitle(d.run), 80)          // plain (D7-guarded), capped at the 80-rune upper bound

	// One physical row, always (issue #666). The combined right block (status + transport) keeps its
	// full width and the STATUS is never the field cut; the TITLE takes the middle and truncates with
	// "…" when it does not fit. When the terminal is so narrow that the full crumb + right block would
	// leave the title below a readable floor, the BREADCRUMB yields next — its run-id/issue tail
	// truncates with "…" — so a title prefix always survives alongside the intact status (issue #666
	// CodeRabbit follow-up: the split path's old floor, honoured on one row). Below ~14 cols even that
	// cannot hold and the final clampVisual clamps whatever is left; realistic terminals never reach it.
	sepW := visualWidth(sep) // 3 — the "   " gap after the crumb
	titleW := visualWidth(title)
	// target is the width the left block is padded to before the right block appends; gap keeps a
	// column between a truncated title's "…" and the status glyph, and the -1 is the trailing edge gap.
	target := m.width
	gap := 0
	if combinedRight != "" {
		gap = 1
		target = m.width - visualWidth(combinedRight) - 1
	}
	leftBudget := target - gap // crumb + sep + title must fit within this
	left := clampVisual(crumb, leftBudget)
	if titleW > 0 {
		floor := 10 // minimum readable title width
		if titleW < floor {
			floor = titleW
		}
		bold := lipgloss.NewStyle().Bold(true)
		if titleAvail := leftBudget - visualWidth(crumb) - sepW; titleAvail >= floor {
			// The crumb keeps its full width; the title fills whatever remains after it.
			left = crumb + sep + bold.Render(clampVisual(title, titleAvail))
		} else {
			// Too narrow for both: the crumb yields (its tail truncates with "…") so the title keeps
			// its floor and the status still renders in full.
			crumbCap := leftBudget - sepW - floor
			if crumbCap < 0 {
				crumbCap = 0
			}
			left = clampVisual(crumb, crumbCap) + sep + bold.Render(clampVisual(title, floor))
		}
	}
	if combinedRight != "" {
		left = padVisual(clampVisual(left, target), target) + combinedRight
	}
	return []string{clampVisual(left, m.width)}
}

func (m tuiModel) renderDetail() string {
	d := &m.detail
	var sb strings.Builder

	// The priority header (PRD #325 M3), rendered as a single physical row (issue #666)
	// (see detailHeaderLines). Each row is clamped to exactly one physical line: a wrap would make
	// transcriptViewport under-count and push the footer off the bottom (the #379 invariant).
	for _, hl := range m.detailHeaderLines() {
		sb.WriteString(hl + "\n")
	}

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
	sb.WriteString(m.joinColumns(rail, body, laneRailWidth))
	// The attention band (PRD #325 M3 redesigned) shows regardless of ownership — it is
	// informational. At a plan gate the OWNER's action keys ride inline at the band's right
	// edge, and detailFooter drops them so they are not duplicated.
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
		return m.pal.faint.Render("live stream unavailable (" + fmtErr(d.streamErr) + "), falling back to a 2s refresh")
	case d.polling:
		return m.pal.faint.Render("reconnecting — refreshing every 2s")
	default:
		return ""
	}
}

// detailFooter is the single-line keymap (PRD #325 M4, redesigned): pane/scroll navigation
// combined with the owner's steer actions. At a plan gate the owner's y/n live inline in the
// attention band, so the footer does NOT repeat them (no duplication). The steer bar's
// interactive modes draw their own hints, so this is only emitted when idle.
func (m tuiModel) detailFooter() string {
	owner := m.detail.steer.access == steerAllowed
	parts := []string{m.keyHint("←→", "pane"), m.keyHint("↑↓", "move")}
	if len(m.detail.lanes) > 0 {
		parts = append(parts, m.keyHint("c", "crew"))
	}
	if isLiveRunStatus(m.detail.run.Status) {
		// g re-attaches the transcript follow (M5); it is a view affordance, so it shows for
		// owner and non-owner alike, but only when there is live output to follow.
		parts = append(parts, m.keyHint("g", "live"))
	}
	if owner {
		// v review is meaningless at a plan gate (no verdict yet); everywhere else the owner
		// gets it. y/n are inline in the band, so they are never here.
		if !atPlanGate(m.detail.run) {
			parts = append(parts, m.keyHint("v", "review"))
		}
		parts = append(parts, m.keyHint("f", "follow-up"), m.keyHint("x", "cancel"))
	}
	parts = append(parts, m.keyHint("esc", "back"), m.keyHint("?", "keys"))
	return strings.Join(parts, m.pal.faint.Render(" · "))
}

// keyHint is a compact tungsten-key / faint-label footer hint.
func (m tuiModel) keyHint(k, label string) string {
	return m.pal.title.Render(k) + m.pal.faint.Render(" "+label)
}

// paneTitle renders a detail pane's eyebrow with a focus indicator: a tungsten ▎ focus bar +
// bold CAPS when focused, faint CAPS otherwise.
func (m tuiModel) paneTitle(title string, focused bool) string {
	if focused {
		return m.pal.title.Render("▎" + strings.ToUpper(title))
	}
	return " " + m.pal.faint.Render(strings.ToUpper(title))
}

// detailBanner is the S3 attention-band treatment: awaiting_approval gets the PLAN GATE band with
// the OWNER's approve/reject keys inline (dropped from the footer so they are not duplicated);
// awaiting_input gets a DISTINCT needs-input band that never offers y/n — those keys do nothing
// at a clarification park, which is answered off-TUI (run answer / web / Slack); awaiting_followup
// (PRD #517) gets its OWN band, distinct from needs-input: an interactive task parked for the
// user's next follow-up, which is NOT a y/n prompt — the owner sends a follow-up (the `f` key) or
// stops the run. All bands show for owner and non-owner alike; the inline y/n keys are
// ownership-gated, and the follow-up band's "with f" hint is too (the `f` key is owner-only, so a
// read-only viewer is pointed at web/Slack instead of an inert key).
func (m tuiModel) detailBanner() string {
	owner := m.detail.steer.access == steerAllowed
	switch m.detail.run.Status {
	case "awaiting_approval":
		return m.attentionBanner("⚑ PLAN GATE", "the crew is waiting on your approval", owner)
	case "awaiting_input":
		return m.attentionBanner("✎ NEEDS INPUT", "the agent asked a question; answer it from another terminal, the web, or Slack", false)
	case "awaiting_followup":
		// The "with f" hint is owner-only: the `f` steer key is gated to the run owner
		// (tui_steer.go), so a read-only viewer is pointed at the web/Slack surfaces
		// instead of an inert key. Body kept under the awaiting_input banner's width so
		// the amber band never truncates at the 100-col reference frame. Owners still see
		// `f` in the footer regardless; withKeys stays false (a follow-up park is not a
		// y/n prompt).
		body := "parked for your follow-up — send one from the web or Slack"
		if owner {
			body = "parked for your follow-up — send one with f, web, or Slack"
		}
		return m.attentionBanner("➤ AWAITING FOLLOW-UP", body, false)
	}
	return ""
}

// attentionBanner draws the ONE filled surface on the detail screen: a full-width amber band
// with a ▌ cap, the head in bold caps, the body, and (when withKeys) the owner's approve/reject
// keys pinned right. NO_COLOR fallback (D3): under an Ascii profile the amber fill and colour
// are stripped, but the ▌ cap and the bold "⚑ PLAN GATE" caps survive, so the gate stays
// structurally unmissable without colour.
func (m tuiModel) attentionBanner(head, body string, withKeys bool) string {
	amber, fg := m.pal.amber, m.pal.bandFg
	seg := func(bold bool, s string) string { return paintSeg(fg, amber, bold, s) }
	left := seg(true, "▌"+head) + seg(false, "  "+body)
	if withKeys {
		right := seg(true, "y") + seg(false, " approve") + seg(false, " · ") + seg(true, "n") + seg(false, " reject") + seg(false, " ")
		return clampVisual(padSeg(left, m.width-visualWidth(right), amber)+right, m.width)
	}
	return clampVisual(padSeg(left, m.width, amber), m.width)
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
		if sp := m.renderSpend(strings.Count(sb.String(), "\n") + 1); sp != "" {
			sb.WriteString("\n\n" + sp)
		}
		if rb := m.railRateMeters(now, strings.Count(sb.String(), "\n")+1); rb != "" {
			sb.WriteString("\n\n" + rb)
		}
		return sb.String()
	}

	suffixes := laneSuffixes(d.lanes)
	// The ALL row wears a fixed neutral ◉ (laneRow ignores the state it is handed), so short-circuit
	// it here rather than computing a crew state nothing reads. Every real row is a plain per-key
	// ladder — no rollup.
	st := func(l agentLane) crewState {
		if l.Key == laneAllKey {
			return crewIdle
		}
		return crewStateFor(d.run.Status, d.run.Health, l.Key, active, l.LastActivity, now)
	}
	// The lead's context-window meter (#565): latest-wins across LEAD frames only, computed once
	// and shown ONLY on the lead lane. A subagent lane and the synthetic ALL lane never get it.
	fill, hasCtx := leadContextFill(d.lanes)
	if d.railCollapsed {
		// Just the selected lane, so the reader still knows whose transcript is on screen while
		// the milestones get the rest of the column.
		if l, ok := d.selectedLane(); ok {
			sb.WriteString(m.laneRow(l, true, st(l), suffixes[l.Key], fill, hasCtx && l.Key == laneLead))
		}
	} else {
		for i, l := range d.lanes {
			sb.WriteString(m.laneRow(l, i == d.laneIdx, st(l), suffixes[l.Key], fill, hasCtx && l.Key == laneLead))
		}
	}
	if block := m.renderMilestones(); block != "" {
		sb.WriteString("\n" + block)
	}
	if sp := m.renderSpend(strings.Count(sb.String(), "\n") + 1); sp != "" {
		sb.WriteString("\n\n" + sp)
	}
	if rb := m.railRateMeters(now, strings.Count(sb.String(), "\n")+1); rb != "" {
		sb.WriteString("\n\n" + rb)
	}
	return sb.String()
}

// laneIdentities maps each real lane's key to the identity string the crew rail shows for it,
// so the aggregated transcript can tag every frame with the SAME token its lane row carries:
// the role, plus a ·N ordinal suffix (laneSuffixes) only when the run has two lanes of one
// role. The ALL lane itself is skipped. The role goes through Plain (D7) at the same cap
// laneRow uses, so the tag and the rail row are byte-identical.
func (m tuiModel) laneIdentities() map[string]string {
	suffixes := laneSuffixes(m.detail.lanes)
	out := make(map[string]string, len(m.detail.lanes))
	for _, l := range m.detail.lanes {
		if l.Key == laneAllKey {
			continue
		}
		out[l.Key] = m.renderer.Plain(l.Role, 14) + suffixes[l.Key]
	}
	return out
}

// laneRow renders one crew-rail row: the ▸ cursor, the status dot, the model-authored role and
// an optional ·N disambiguating ordinal, and the optional italic label line beneath it. A
// selected lane rides the warm selection bar (like the board). The role and label are UNTRUSTED
// and go through renderer.Plain (D7); the ordinal suffix is derived, not untrusted. Keeping
// every cell's sanitizing in this one helper is why the collapsed and expanded paths share it.
//
// When showMeter is set (the lead lane, and only when leadContextFill found a valid reading) the
// row carries the inline context-window meter AFTER the role/suffix. The meter is DERIVED, not
// untrusted (no Plain needed), and is built here so it rides this row's own selection bg.
func (m tuiModel) laneRow(l agentLane, selected bool, st crewState, suffix string, fill contextFill, showMeter bool) string {
	var bg color.Color
	if selected {
		bg = m.pal.selBg
	}
	dotC := m.pal.state(st).GetForeground()
	dot := laneDot(st)
	if l.Key == laneAllKey {
		// The aggregated row is a META lane, not an actor, so it wears a neutral tungsten ◉ (a
		// circle-family glyph, distinct from the plain ● state dot) rather than a crew state
		// dot — it never flashes the alarming ▲ a worst-state rollup would put on the whole run.
		dot, dotC = "◉", m.pal.tungsten
	}
	cursor := paintSeg(nil, bg, false, " ")
	if selected {
		cursor = paintSeg(m.pal.tungsten, bg, true, "▸")
	}
	line := cursor + paintSeg(dotC, bg, false, dot) + paintSeg(nil, bg, false, " "+m.renderer.Plain(l.Role, 14))
	if suffix != "" {
		// A ·N ordinal disambiguates two lanes of one role (laneSuffixes). It is DERIVED, not
		// the opaque SDK invocation id, so the id tail never reaches the rail — a lone role
		// shows no suffix at all — and there is nothing untrusted here to sanitize.
		line += paintSeg(m.pal.faintC, bg, false, suffix)
	}
	if showMeter {
		// The bar fills whatever the rail has left after the ▸● <role><suffix> prefix,
		// the meter's own leading space, its separator space, and the 4-col percent (=6).
		barW := laneRailWidth - visualWidth(line) - 6
		if barW < 0 {
			barW = 0
		}
		line += m.contextMeterCell(bg, fill, barW)
	}
	var sb strings.Builder
	sb.WriteString(padSeg(line, laneRailWidth, bg) + "\n")
	if l.Label != "" {
		// The machine's own words about its task: italic (degrading to plain under NO_COLOR),
		// faint. Still UNTRUSTED → renderer.Plain.
		lst := lipgloss.NewStyle().Foreground(m.pal.faintC).Italic(true)
		if bg != nil {
			lst = lst.Background(bg)
		}
		label := paintSeg(nil, bg, false, "   ") + lst.Render(m.renderer.Plain(l.Label, laneLabelCap))
		sb.WriteString(padSeg(label, laneRailWidth, bg) + "\n")
	}
	return sb.String()
}

// milestoneTitleCap is the per-row title budget in the crew rail (laneRailWidth 26 minus
// the " ✓ " glyph prefix). joinColumns clamps the whole left column anyway, but Plain must
// cap the UNTRUSTED title itself (D7), so it caps at the width the rail can actually show.
const milestoneTitleCap = 22

// milestoneProgress folds a run's frozen milestone list into (done, total, reported), shared by
// the crew-rail block and the board badge so their COUNTS cannot disagree. `done` counts frozen
// MEMBERS present in the completed set (immune to a duplicate id and to a completed id naming a
// milestone dropped after it was ticked). `reported` is whether ANY completion was ever reported:
// a nil completed slice (JSON null) means never. The crew rail and the board's text fallback use
// it to read `–/N` rather than a `0/N` that looks like failure; the board's own micro-bar instead
// draws an all-empty ▱ bar for an unreported run (see milestoneMarker), trading the never/zero
// distinction for an at-a-glance bar. total is 0 for a run with no frozen list — caller renders
// nothing.
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
	// The eyebrow gets a milestone micro-bar (▰ done / ▱ remaining) beside the count, the rail
	// twin of the board's micro-bar. Dropped for a very long list, where the per-milestone rows
	// below carry the detail anyway.
	bar := ""
	if reported && total <= boardMileCap {
		bar = lipgloss.NewStyle().Foreground(m.pal.tungsten).Render(strings.Repeat("▰", done)) +
			m.pal.faint.Render(strings.Repeat("▱", total-done)) + " "
	}
	sb.WriteString(m.pal.faint.Render("MILESTONES") + " " + bar + m.pal.faint.Render(milestoneCount(done, total, reported)) + "\n")
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

// renderSpend is the crew-rail SPEND block (PRD #650): a run's rolled-up cost headline over an
// in/out/cache token breakdown, sitting directly above the ACCOUNTS block (railRateMeters) — the
// "what it cost" beside the "which account paid it" (PRD #623). Omitted entirely for a nil Usage
// (pre-#40 / unclaimed run). Budgeted whole-block-or-nothing against the remaining rail height,
// exactly like railRateMeters' account entries, because joinColumns clamps the rail by dropping
// its BOTTOM lines — a half-drawn SPEND (header, no cache line) must never render.
//
// The token split mirrors the web usage panel's aggregates (web/src/lib/runUsage.ts): "in" is the
// web's `fresh` (InputTokens + CacheCreationTokens), "cache" is `cached` (CacheReadTokens), so
// in + cache == the web "Tokens in" figure, and the cache% is cache's share of that — the exact
// cacheDisplayPct semantics (clamped [1,99]; 100 only when fresh==0, 0 only when no cache reads).
func (m tuiModel) renderSpend(usedRows int) string {
	u := m.detail.run.Usage
	if u == nil {
		return ""
	}
	total := "—" // subscription-auth $0
	if u.CostUSD > 0 {
		total = fmtCostCents(u.CostUSD)
	}
	fresh := u.InputTokens + u.CacheCreationTokens
	pct := cacheDisplayPct(u.InputTokens, u.CacheReadTokens, u.CacheCreationTokens)
	head := m.pal.faint.Render("SPEND") + "  " + lipgloss.NewStyle().Foreground(m.pal.tungsten).Render(total)
	inOut := m.pal.faint.Render("in " + fmtTokens(fresh) + "  out " + fmtTokens(u.OutputTokens))
	cache := m.pal.faint.Render("cache " + fmtTokens(u.CacheReadTokens) + " " + itoa(pct) + "%")
	lines := []string{head, inOut, cache}
	// Whole-block-or-nothing: the -1 is the blank "\n\n" separator the caller prepends (same budget
	// arithmetic railRateMeters uses).
	if len(lines) > m.transcriptViewport()-usedRows-1 {
		return ""
	}
	return strings.Join(lines, "\n")
}

// railRateMeters renders the stacked per-account rate-limit block for the crew rail, or ""
// when the selection is empty. It appends WHOLE account entries only while they fit within
// the remaining rail height (transcriptViewport() minus usedRows minus the blank separator
// the caller adds), because joinColumns clamps the rail to the transcript height by dropping
// its BOTTOM lines one at a time — an uncapped block would leave a half-drawn entry (label +
// 5h, no 7d). Dropping whole entries keeps every visible entry complete. Reuses rateWindowCell
// so the bar/percent/tone and the nil-window "-" are identical to the board strip.
//
// Deploy-ordering note (#519): the meters populate only when GET /api/me/settings and
// /api/me/rate-limits answer over the CLI uzc_ Bearer token. /me/settings GET moved to
// RequireUser in #519; against a server that predates that the settings fetch 401s (error
// swallowed) and this falls back to default-token-only, exactly as the board strip does. No
// server change here.
func (m tuiModel) railRateMeters(now time.Time, usedRows int) string {
	shown, showLabel := m.selectedRateMeters()

	// PRD #623: force-show + highlight the account THIS run is spending, as the first
	// ACCOUNTS entry, even when it is deselected in settings. This fold runs BEFORE the
	// empty-check below (the M1 trap): when the run's account is deselected and nothing
	// else is selected, selectedRateMeters returns (nil, false) and an early return here
	// would drop the ACCOUNTS block entirely — exactly the deselected-account case this
	// PRD exists to fix. Detail-only: railRateMeters is not shared with the board strip,
	// so selectedRateMeters/boardRateLimitStrip stay untouched.
	runID := m.detail.run.AnthropicSecretID
	if runID != nil {
		// Move-to-front if already shown (the common path — the run's account is often
		// IsDefault, which is always in shown; a bare prepend without the remove would
		// double-list it).
		foundIdx := -1
		for i, t := range shown {
			if t.SecretID == *runID {
				foundIdx = i
				break
			}
		}
		if foundIdx >= 0 {
			runTok := shown[foundIdx]
			shown = append(shown[:foundIdx], shown[foundIdx+1:]...)
			shown = append([]apitypes.TokenRateLimitDTO{runTok}, shown...)
		} else {
			// Not selected — force-show it. Prefer a real rate-limit row (any Status, even
			// non-"ok") so its windows render; else synthesize a label-only entry so the
			// account name still shows with "5h -"/"7d -".
			var runTok apitypes.TokenRateLimitDTO
			hasTok := false
			for _, t := range m.rateLimits {
				if t.SecretID == *runID {
					runTok = t
					hasTok = true
					break
				}
			}
			if !hasTok {
				runTok = apitypes.TokenRateLimitDTO{SecretID: *runID, Label: strOr(m.detail.run.AnthropicSecretLabel, "")}
			}
			shown = append([]apitypes.TokenRateLimitDTO{runTok}, shown...)
		}
	}

	if len(shown) == 0 {
		return ""
	}
	// budget is the rail height left below the content already built; the -1 is the blank
	// separator the caller prepends via "\n\n" before this block.
	budget := m.transcriptViewport() - usedRows - 1

	// Each account entry is a \n-joined string of an optional faint label eyebrow + the two
	// window cells; entries are added whole while they fit under the ACCOUNTS header.
	const headerRow = 1
	var fitted []string
	accumulated := 0
	for _, t := range shown {
		var lines []string
		// The run's account label renders UNCONDITIONALLY (even when showLabel is false —
		// a single-account run must still show its highlighted name) in tungsten normal
		// weight; siblings keep faint and still obey showLabel. rows is computed from the
		// lines actually built, so the always-present run label is already in the budget.
		isRun := runID != nil && t.SecretID == *runID
		if isRun {
			lines = append(lines, lipgloss.NewStyle().Foreground(m.pal.tungsten).Render(m.renderer.Plain(t.Label, laneRailWidth)))
		} else if showLabel {
			lines = append(lines, m.pal.faint.Render(m.renderer.Plain(t.Label, laneRailWidth)))
		}
		lines = append(lines,
			m.rateWindowCell("5h", t.Limits.FiveHour, railRateBarWidth, 4, now),
			m.rateWindowCell("7d", t.Limits.SevenDay, railRateBarWidth, 4, now),
		)
		entry := strings.Join(lines, "\n")
		rows := len(lines)
		if headerRow+accumulated+rows > budget {
			break
		}
		fitted = append(fitted, entry)
		accumulated += rows
	}
	if len(fitted) == 0 {
		return ""
	}
	return m.pal.faint.Render("ACCOUNTS") + "\n" + strings.Join(fitted, "\n")
}

// buildTranscriptLines renders the selected lane's frames to display lines (no windowing),
// so the follow/scroll windowing and the line-count extent share one layout. A speaker frame is
// a ▪ header (tungsten) over its markdown body; a tool frame compresses to a single faint
// `⚙ <tool> #seq` line. f.Kind and the tool name are drawn through renderer.Plain (D7); the body
// through renderer.Markdown.
// buildTranscriptLines renders a lane's frames to human-readable display lines (no windowing),
// so the follow/scroll windowing and the line-count extent share one layout. The presentation
// is written for a person, not a log reader:
//   - text           the message body (markdown), with NO "text #123" header — the body is the
//     message. In the aggregated lane it gets a tungsten "▪ <who>" speaker line so interleaved
//     turns stay attributable; a single-lane view needs none (the pane title names the lane).
//   - thinking       the same, but marked "thinking" (faint) so the model's INTERNAL reasoning is
//     never read as its output.
//   - tool_use       "⚙ <Tool>  <what it ran>" — the tool plus a compact arg preview (the
//     command / path / pattern), never the internal seq.
//   - tool_result    a faint "  ↳ <summary>" folded UNDER its own call (paired by id), flattened
//     to one readable line (resultSummary). A failed call (is_error) gets a ✗; an ORPHAN result —
//     a parallel call's result seq-interleaved away from its use — names its own tool + actor.
//   - anything else  a humanized "▪ <kind>" header (no seq) over its body.
//
// Every model-authored string is UNTRUSTED and passes through Plain/Markdown (D7); the whole
// palette stays ANDON's two intensities (tungsten accent over faint body), no per-frame colour.
func (m tuiModel) buildTranscriptLines(lane agentLane) []string {
	aggregated := lane.Key == laneAllKey
	var ids map[string]string
	if aggregated {
		ids = m.laneIdentities()
	}
	tungsten := lipgloss.NewStyle().Foreground(m.pal.tungsten)
	tungstenB := tungsten.Bold(true)
	width := m.transcriptWidth()
	// who returns the aggregated lane's per-frame identity tag (empty in a single-lane view).
	who := func(f laneFrame) string {
		if !aggregated {
			return ""
		}
		id := ids[laneKeyOf(f)]
		if id == "" {
			id = m.renderer.Plain(frameAgentTag(f), 16)
		}
		return id
	}
	// tightNext[i] is true when frame i is a tool_use IMMEDIATELY followed by ITS OWN result
	// (matched by id, not merely by the next frame being a tool_result). A single assistant turn
	// with parallel tool calls persists as [use A, use B, result A, result B]; keying the pair off
	// the next frame's kind alone would fold result A under call B and mis-title it. names resolves
	// a result's tool by its use id, so an ORPHAN result (its call not directly above it) can still
	// name the tool it belongs to.
	tightNext := make([]bool, len(lane.Frames))
	names := map[string]string{}
	for i, f := range lane.Frames {
		if f.Kind != "tool_use" {
			continue
		}
		id := toolUseID(f)
		if id == "" {
			continue
		}
		if n, ok := toolFrameName(f); ok {
			names[id] = n
		}
		if i+1 < len(lane.Frames) {
			if nx := lane.Frames[i+1]; nx.Kind == "tool_result" && resultUseID(nx) == id {
				tightNext[i] = true
			}
		}
	}
	var sb strings.Builder
	for i, f := range lane.Frames {
		var block string
		switch f.Kind {
		case "tool_use":
			name, ok := toolFrameName(f)
			if !ok {
				name = "tool"
			}
			// The tool NAME is the scannable anchor of a busy tool run (many ⚙ Bash lines in a
			// row), so it gets the bold tungsten accent; the ⚙ glyph and the arg preview stay
			// faint. The ·<who> comes BEFORE the command, not after: a long command is the
			// truncatable tail, and in the aggregated view the attribution must never be the
			// thing clampVisual cuts off (it did, on long greps).
			line := m.pal.faint.Render("⚙ ") + tungstenB.Render(m.renderer.Plain(name, 24))
			if w := who(f); w != "" {
				line += m.pal.faint.Render(" · ") + tungsten.Render(w)
			}
			if arg := toolArgPreview(f.Payload); arg != "" {
				line += m.pal.faint.Render("  " + m.renderer.Plain(arg, 200))
			}
			block = clampVisual(line, width)
		case "tool_result":
			sum := resultSummary(f.Payload)
			if sum == "" {
				sum = "(no output)"
			}
			// ✗ marks a FAILED tool call (is_error) — a glyph, not just a colour, so the failure
			// survives a NO_COLOR/Ascii profile the way the header's ▲ does; the alarm colour rides
			// on top for a colour terminal. The web shows the same signal as a red "✗ error" chip
			// (PRD #116 classifyResultState), and dropping it made a failing run read like a passing
			// one.
			line := m.pal.faint.Render("  ↳ ")
			if resultIsError(f.Payload) {
				line += lipgloss.NewStyle().Foreground(m.pal.alarm).Render("✗ ")
			}
			// A result folded UNDER its own call (tightNext[i-1]) inherits that call's
			// "⚙ <Tool> · <who>" line, so it repeats nothing. An ORPHAN result — a parallel call's
			// result seq-separated from its use, or a result whose use is missing — carries its own
			// tool name + actor so it stays attributable (the aggregated lane's whole purpose).
			if i == 0 || !tightNext[i-1] {
				seg := ""
				if name := names[resultUseID(f)]; name != "" {
					seg = tungstenB.Render(m.renderer.Plain(name, 24))
				}
				if w := who(f); w != "" {
					if seg != "" {
						seg += m.pal.faint.Render(" · ")
					}
					seg += tungsten.Render(w)
				}
				if seg != "" {
					line += seg + m.pal.faint.Render("  ")
				}
			}
			line += m.pal.faint.Render(m.renderer.Plain(sum, 200))
			block = clampVisual(line, width)
		case "text", "thinking":
			// TrimLeft drops Glamour's document top-margin so the body sits directly under the
			// "▪ <who>" speaker line instead of a blank line below it.
			body := strings.TrimLeft(m.renderer.Markdown(transcriptText(f)), "\n")
			head := ""
			if w := who(f); w != "" {
				head = tungsten.Render("▪ " + w)
			}
			if f.Kind == "thinking" {
				// The model's INTERNAL reasoning, not its output: mark it so a reader never mistakes
				// a private deliberation for something the agent "said". Faint, riding the speaker
				// header when aggregated, else a lone faint eyebrow (text keeps none — the pane title
				// names the lane).
				if head != "" {
					head += m.pal.faint.Render(" · thinking")
				} else {
					head = m.pal.faint.Render("▪ thinking")
				}
			}
			if head != "" {
				block = head + "\n"
			}
			block += body
		default:
			head := tungsten.Render("▪ " + m.renderer.Plain(f.Kind, 16))
			if w := who(f); w != "" {
				head += m.pal.faint.Render("  · ") + tungsten.Render(w)
			}
			block = clampVisual(head, width) + "\n" + strings.TrimLeft(m.renderer.Markdown(transcriptText(f)), "\n")
		}
		// Blocks are blank-line separated, EXCEPT a tool_use and ITS OWN result (tightNext, matched
		// by id) pair tight (a single newline), so a call and its output read as one unit.
		sep := "\n\n"
		if tightNext[i] {
			sep = "\n"
		}
		sb.WriteString(block + sep)
	}
	return strings.Split(strings.TrimRight(sb.String(), "\n"), "\n")
}

// toolFrameName pulls a tool-use frame's tool name from its payload, so the transcript can
// compress it to one line. Returns ok=false for a non-tool frame or one with no name.
func toolFrameName(f laneFrame) (string, bool) {
	if f.Kind != "tool_use" || len(f.Payload) == 0 {
		return "", false
	}
	var p struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(f.Payload, &p) == nil && p.Name != "" {
		return p.Name, true
	}
	return "", false
}

// toolUseID pulls a tool_use frame's SDK id ("id"), the key a tool_result references back via
// "tool_use_id". Empty for a non-tool_use frame or one carrying no id — in which case the pairing
// falls back to unpaired (blank-line separated) rather than guessing.
func toolUseID(f laneFrame) string {
	if f.Kind != "tool_use" || len(f.Payload) == 0 {
		return ""
	}
	var p struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(f.Payload, &p) == nil {
		return p.ID
	}
	return ""
}

// toolArgPreview renders a tool_use's arguments as a compact one-liner for the transcript, so a
// reader sees WHAT ran, not just the tool name: the command for Bash, the path for a file tool,
// the pattern/url otherwise, else the whole input folded. The value is model-authored and
// UNTRUSTED — compactText folds newlines/tabs, strips control bytes and caps the length.
func toolArgPreview(payload json.RawMessage) string {
	var p struct {
		Input map[string]any `json:"input"`
	}
	if json.Unmarshal(payload, &p) != nil || len(p.Input) == 0 {
		return ""
	}
	for _, k := range []string{"command", "file_path", "path", "pattern", "query", "url", "description"} {
		if v, ok := p.Input[k].(string); ok && strings.TrimSpace(v) != "" {
			return compactText(v)
		}
	}
	b, err := json.Marshal(p.Input)
	if err != nil {
		return ""
	}
	return compactText(string(b))
}

// resultSummary flattens a tool_result payload's content into ONE readable line — the TUI twin
// of the web's resultToText (RunEvent.tsx). Content is a string or an SDK array of
// {type:"text",text:…} blocks; compactText turns the escaped newlines that made the raw JSON
// dump unreadable into spaces and caps the length. Non-text blocks (images) are dropped, and an
// empty/unknown shape yields "" so the caller can show "(no output)".
func resultSummary(payload json.RawMessage) string {
	var p struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(payload, &p) != nil || len(p.Content) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(p.Content, &s) == nil {
		return compactText(s)
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(p.Content, &blocks) == nil {
		parts := make([]string, 0, len(blocks))
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return compactText(strings.Join(parts, " "))
	}
	return ""
}

// resultUseID pulls a tool_result frame's "tool_use_id" — the id of the tool_use it answers, used
// to pair a result with its own call (buildTranscriptLines) instead of with whatever frame happens
// to sit above it in seq order.
func resultUseID(f laneFrame) string {
	if f.Kind != "tool_result" || len(f.Payload) == 0 {
		return ""
	}
	var p struct {
		ToolUseID string `json:"tool_use_id"`
	}
	if json.Unmarshal(f.Payload, &p) == nil {
		return p.ToolUseID
	}
	return ""
}

// resultIsError reports whether a tool_result payload carries is_error:true — a failed (or
// guardrail-denied) tool call. The flag is passed through verbatim from the upstream SDK
// (agent/src/sdk-messages.ts); a malformed or absent flag reads as not-an-error.
func resultIsError(payload json.RawMessage) bool {
	if len(payload) == 0 {
		return false
	}
	var p struct {
		IsError bool `json:"is_error"`
	}
	return json.Unmarshal(payload, &p) == nil && p.IsError
}

// transcriptViewport is the transcript's visible line budget (the pane title is a fixed
// header above it, so it is one row shorter than the pane).
//
// The budget is the terminal height minus every OTHER row the detail view draws, so the
// two-pane body fills the screen down to the footer (issue #379) rather than stopping at the
// last line of content and leaving a tall terminal with dead space below the footer. It
// counts the SAME chrome renderDetail emits — header (always 1 row, detailHeaderLines), optional
// park line, an optional degraded
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
	chrome := len(m.detailHeaderLines()) // the priority header: always 1 row (detailHeaderLines)
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
	// The pane title names WHOSE lane is on screen: TRANSCRIPT · <role>.
	title := m.paneTitle("transcript", focused)
	if ok && lane.Role != "" {
		title += m.pal.faint.Render(" · " + m.renderer.Plain(lane.Role, 16))
	}
	if !ok {
		return m.padPaneTitle(title, "") + "\n" +
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

	return m.padPaneTitle(title, m.followBadge(top, maxTop)) + "\n" + window
}

// followBadge is the transcript's live-follow affordance (M5) — distinct from the transport
// line, which is about THIS CLIENT's connection. Only a LIVE run tails: a terminal run's
// transcript is complete, so it carries no badge. `⇣ following` while auto-tailing; when paused,
// `⏸ N new · g ⇣` — the count of lines below the fold, plus the key that teaches its own remedy.
func (m tuiModel) followBadge(top, maxTop int) string {
	if !isLiveRunStatus(m.detail.run.Status) {
		return ""
	}
	if m.detail.follow {
		return lipgloss.NewStyle().Foreground(m.pal.sage).Render("⇣ following")
	}
	badge := lipgloss.NewStyle().Foreground(m.pal.amber).Bold(true).Render("⏸")
	if below := maxTop - top; below > 0 {
		badge += m.pal.faint.Render(" " + itoa(below) + " new")
	}
	return badge + m.pal.faint.Render(" · ") + m.keyHint("g", "⇣")
}

// padPaneTitle right-aligns a badge (the follow indicator) against a pre-rendered pane title,
// padded to the transcript column width.
func (m tuiModel) padPaneTitle(title, badge string) string {
	if badge == "" {
		return title
	}
	return padVisual(title, m.transcriptWidth()-visualWidth(badge)) + badge
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
func (m tuiModel) joinColumns(left, right string, width int) string {
	l := strings.Split(left, "\n")
	r := strings.Split(right, "\n")
	n := len(r)
	div := " " + m.pal.faint.Render("▏") + " " // a faint hairline replaces the old │ rule
	var sb strings.Builder
	for i := 0; i < n; i++ {
		var lv, rv string
		if i < len(l) {
			lv = l[i]
		}
		if i < len(r) {
			rv = r[i]
		}
		sb.WriteString(padVisual(clampVisual(lv, width), width) + div + rv + "\n")
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
