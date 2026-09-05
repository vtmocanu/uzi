package main

import (
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"

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
	// leadCtx is the lead context-window meter reading, memoized on rebuild so a
	// render does not re-scan+unmarshal lead frames each View() (mirrors web's
	// leadContext memoization in deriveRunUsage).
	leadCtx   contextFill
	leadCtxOK bool
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
	runLoaded     bool // the first GetRun has landed: header/milestones/accounts can render
	tailLoaded    bool // the newest transcript page has landed
	// lowSeq / highSeq bound the seq-carrying frames held (0 = none). lowSeq is the backfill
	// cursor: the background walk requests the newest page strictly below it and the reply
	// lowers it, until the start of history is reached. highSeq is the total, since seq is
	// gapless from 1 (D5), so the progress badge reads `<held> of <highSeq>`.
	lowSeq  int32
	highSeq int32
	// backfilling / historyComplete / backfillFailed drive the background history walk (M5).
	// backfilling is true while a page is in flight; historyComplete is set once the walk
	// reaches the start (an empty page, or a page holding seq 1); backfillFailed is set when a
	// page errored or did not advance the cursor, and the badge then invites `r` to resume.
	backfilling     bool
	historyComplete bool
	backfillFailed  bool
	// loadErr is the RUN-load error only (GetRun) — fatal to the whole view, since the
	// header/rail have no DTO to render from. pageErr is the transcript-page error,
	// scoped to the pane so a failed tail leaves the header up (PRD #1137: the header is
	// independent of the history). The two are separate fields precisely because the two
	// commands are dispatched concurrently and share nothing else — folding both into one
	// field let a tail success clear a run error (or a tail error erase a live header).
	loadErr   error
	pageErr   error
	stream    *uzicli.RunStream
	streamErr error
	// polling is the D8 fallback: the socket is unusable, so the view re-reads over
	// REST on the same 2s cadence `uzi run logs --follow` uses.
	polling bool

	// metaSeq / metaWaitID are the detail-meta analogue of the board's reqSeq / waitID (PRD
	// #1130 M1 D2). metaSeq is the monotonic id minted for each meta GetRun poll
	// (startDetailMetaReq bumps it); metaWaitID is the id the detail is waiting on, with
	// metaWaitID == 0 meaning idle — a boardTickMsg issues a new meta refresh only while idle.
	// A detailMetaMsg is honoured only when its reqID == metaWaitID (and its runID matches),
	// and a failed poll clears metaWaitID so the next tick retries (the D2 anti-wedge property).
	// metaSeq restarts per run (newDetailState), which is safe because the detailMetaMsg case
	// checks runID BEFORE comparing the id, so a reply for an old run can never match.
	metaSeq    uint64
	metaWaitID uint64

	// M4 surfaces. steer is gated on OWNERSHIP, not visibility (see steerAccessFor);
	// review is the [v] overlay.
	steer  steerState2
	review reviewState
}

func newDetailState(runID string) detailState {
	return detailState{runID: runID, seen: map[int32]bool{}, follow: true}
}

// applyRun folds the first GetRun in: the header, the crew-rail milestones/accounts and
// the "now" line render from this alone, one round trip before the transcript. Status is
// taken wholesale (nothing to preserve yet, unlike applyMeta).
func (d *detailState) applyRun(run apitypes.RunDTO, err error) {
	if err != nil {
		d.loadErr = err
		d.runLoaded = true
		return
	}
	d.loadErr = nil
	d.run = run
	d.runLoaded = true
}

// applyTailPage folds the newest transcript page in and marks the pane loaded. Frames are
// deduped by seq (addFrame), so a live frame that beat the tail is not doubled. A page
// error is recorded in pageErr — NOT loadErr — so it stays scoped to the transcript pane
// and never swallows (or is swallowed by) the concurrent run-load result.
func (d *detailState) applyTailPage(msgs []apitypes.MessageDTO, err error) {
	if err != nil {
		d.pageErr = err
		d.tailLoaded = true
		return
	}
	d.pageErr = nil
	d.addFrames(framesFromMessages(msgs))
	d.tailLoaded = true
	d.rebuild()
}

// applyBackfillPage folds one background history page in and returns the Cmd that chains the
// next page (or nil when the walk has stopped). A backfill failure is a badge, not a full-pane
// error: it never touches pageErr. The chain stops on an error, an empty page (start reached),
// a page that did not lower the cursor (a hostile/broken server or an all-duplicate page — the
// mirror of RunLogs' "did not advance" guard), or a page reaching seq 1. It is a *tuiModel
// method, not a *detailState one, because the scroll anchoring below reads the rendered
// line-count through the model's buildTranscriptLines.
func (m *tuiModel) applyBackfillPage(msgs []apitypes.MessageDTO, err error) tea.Cmd {
	if err != nil {
		m.detail.backfilling = false
		m.detail.backfillFailed = true
		return nil
	}
	before := m.detail.lowSeq // the page's `before`; the reply must lower it
	// Anchor the paused viewport: measure the selected lane's rendered line count before and
	// after the prepend, and push scroll down by the delta so the user's top line does not
	// jump. With follow the window is bottom-anchored and nothing moves.
	var linesBefore int
	if !m.detail.follow {
		if lane, ok := m.detail.selectedLane(); ok {
			linesBefore = len(m.buildTranscriptLines(lane))
		}
	}
	m.detail.addFrames(framesFromMessages(msgs))
	m.detail.rebuild()
	if !m.detail.follow {
		if lane, ok := m.detail.selectedLane(); ok {
			if delta := len(m.buildTranscriptLines(lane)) - linesBefore; delta > 0 {
				m.detail.scroll += delta
			}
		}
	}
	if len(msgs) == 0 { // the start of history: nothing older
		m.detail.historyComplete = true
		m.detail.backfilling = false
		return nil
	}
	if m.detail.lowSeq >= before { // did not advance: stop, invite `r`
		m.detail.backfilling = false
		m.detail.backfillFailed = true
		return nil
	}
	if m.detail.lowSeq <= 1 { // reached seq 1: the whole history is held
		m.detail.historyComplete = true
		m.detail.backfilling = false
		return nil
	}
	m.detail.backfillFailed = false
	return m.backfillCmd(m.detail.runID, m.detail.lowSeq)
}

// applyMeta refreshes the run DTO from a periodic GetRun poll WITHOUT touching the frame
// log — the transcript is fed by the stream/replay, this only refreshes the non-streamed
// fields (milestones, health, kind, title, lifecycle stamps). Ignored until the initial
// load has set the baseline, so a poll racing the first run load cannot flip `runLoaded`.
//
// Status is DELIBERATELY preserved rather than overwritten: the live stream owns it
// (applyEvents sets it from authoritative `state` frames, including StreamRun's reconcile),
// and applyMeta only runs while the stream is healthy (the dispatch guards on !polling).
// Overwriting it would let a GetRun response that was in flight across a status transition
// revert the status for up to one 2s tick — e.g. dropping the plan-gate banner and its owner
// keys the instant a run enters awaiting_approval, or flipping a just-finished run back to
// running. When the stream is down the poll-fallback path (loadRunCmd/applyRun) owns
// status instead, so nothing goes stale.
func (d *detailState) applyMeta(run apitypes.RunDTO) {
	if !d.runLoaded {
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

// addFrame appends a single (live) message frame, deduped by seq, keeping frames seq-sorted
// and the seq bounds current. Dedup is required, not defensive: a reconnect replays from the
// last seq seen and the live socket may deliver the same frame, and a duplicate would double a
// lane's contribution. It routes through addFrames so the sort + bounds invariant has one owner.
func (d *detailState) addFrame(f laneFrame) {
	d.addFrames([]laneFrame{f})
}

// framesFromMessages maps a page of MessageDTOs to laneFrames, the shape addFrames folds in.
func framesFromMessages(msgs []apitypes.MessageDTO) []laneFrame {
	out := make([]laneFrame, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, laneFrameFromMessage(m))
	}
	return out
}

// addFrames folds a page of frames into the seq-sorted log, deduped by seq. frames stays
// ascending by seq so the transcript renders oldest→newest regardless of which transport
// (tail, backfill, live) delivered a frame. A page below the held range prepends, above
// appends, overlapping merges — all handled by a stable sort after the dedup append (M5 is not
// the perf milestone; M6 memoizes rendering). The backfill "did not advance" guard reads the
// seq bounds after this returns, not a count, so nothing here returns one.
func (d *detailState) addFrames(page []laneFrame) {
	if d.seen == nil {
		// Total on a zero-value detailState: newDetailState seeds this map, but the handler
		// guard is the load-bearing fix — this keeps a nil map from panicking the program if
		// any future path reaches addFrames without the constructor.
		d.seen = map[int32]bool{}
	}
	added := 0
	for _, f := range page {
		if f.Seq > 0 {
			if d.seen[f.Seq] {
				continue
			}
			d.seen[f.Seq] = true
		}
		d.frames = append(d.frames, f)
		added++
	}
	if added > 0 {
		sort.SliceStable(d.frames, func(i, j int) bool { return d.frames[i].Seq < d.frames[j].Seq })
	}
	d.recomputeSeqBounds()
}

// recomputeSeqBounds sets lowSeq/highSeq from the seq-carrying frames (seq>0), ignoring any
// seq-less infra frame.
func (d *detailState) recomputeSeqBounds() {
	d.lowSeq, d.highSeq = 0, 0
	for _, f := range d.frames {
		if f.Seq <= 0 {
			continue
		}
		if d.lowSeq == 0 || f.Seq < d.lowSeq {
			d.lowSeq = f.Seq
		}
		if f.Seq > d.highSeq {
			d.highSeq = f.Seq
		}
	}
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
	// Memoize the lead context-window meter reading now that d.lanes is final, so each
	// render reads the cached value instead of re-scanning+unmarshalling lead frames.
	d.leadCtx, d.leadCtxOK = leadContextFill(d.lanes)
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
	// Mint a new id via startBoardReq so this refetch can't clear a periodic poll's guard: a
	// board poll may be outstanding when the user backs out, and reusing its id would let this
	// reply (or that stale one) be mistaken for the other (PRD #1130 M1 D2).
	return m, (&m).startBoardReq()
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
		cmds := []tea.Cmd{m.loadRunCmd(m.detail.runID), m.loadTailCmd(m.detail.runID)}
		// Resume a stalled or failed background backfill (M5): clear the failed flag and
		// re-arm the walk from the current cursor. M7 reworks `r` to be fully incremental;
		// this is the M5 resume. Skipped when the walk is already complete or in flight so a
		// refresh never stacks a second backfill chain on the one already running.
		if !m.detail.historyComplete && !m.detail.backfilling && m.detail.lowSeq > 1 {
			m.detail.backfillFailed = false
			m.detail.backfilling = true
			cmds = append(cmds, m.backfillCmd(m.detail.runID, m.detail.lowSeq))
		}
		return m, tea.Batch(cmds...)
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
	// Nothing renders before the first GetRun lands — the header/rail/milestones all read
	// from d.run. Once runLoaded, the header and rail render even while the transcript pane
	// is still waiting on its tail page (gated separately in renderTranscript).
	if !d.runLoaded {
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
