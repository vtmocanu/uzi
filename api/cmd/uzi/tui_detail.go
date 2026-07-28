package main

import (
	"encoding/json"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"

	"gitlab.example.com/vtmocanu/uzi/api/internal/apitypes"
	"gitlab.example.com/vtmocanu/uzi/api/internal/uzicli"
)

// laneRailWidth is the left rail's fixed column budget.
const laneRailWidth = 26

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

	lanes     []agentLane
	laneIdx   int
	scroll    int
	loaded    bool
	loadErr   error
	stream    *uzicli.RunStream
	streamErr error
	// polling is the D8 fallback: the socket is unusable, so the view re-reads over
	// REST on the same 2s cadence `uzi run logs --follow` uses.
	polling bool

	// M4 surfaces. steer is gated on OWNERSHIP, not visibility (see steerAccessFor);
	// review is the [v] overlay.
	steer  steerState2
	review reviewState
}

func newDetailState(runID string) detailState {
	return detailState{runID: runID, seen: map[int32]bool{}}
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
	// in an input/confirm mode it must swallow keys that would otherwise be lane
	// navigation, or typing "l" into a follow-up would switch lanes underneath it.
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
	case keyTab, "l", keyRight:
		if n := len(m.detail.lanes); n > 0 {
			m.detail.laneIdx = (m.detail.laneIdx + 1) % n
			m.detail.scroll = 0
		}
		return m, nil
	case "h", keyLeft:
		if n := len(m.detail.lanes); n > 0 {
			m.detail.laneIdx = (m.detail.laneIdx - 1 + n) % n
			m.detail.scroll = 0
		}
		return m, nil
	}
	if d := motionDelta(k); d != 0 {
		m.detail.scroll += d
		if m.detail.scroll < 0 {
			m.detail.scroll = 0
		}
		return m, nil
	}
	return m, nil
}

func (m tuiModel) renderDetail() string {
	d := &m.detail
	var sb strings.Builder

	head := "run " + shortRunID(d.runID)
	if d.run.ID != "" {
		head += " · " + m.renderer.Plain(d.run.Status, 20)
		if h := d.run.Health; h != "" && h != "ok" {
			head += " · " + m.renderer.Plain(h, 20)
		}
	}
	sb.WriteString(m.pal.title.Render(head))
	if d.run.ID != "" {
		sb.WriteString("  " + m.pal.faint.Render(m.renderer.Plain(runTitle(d.run), 60)))
	}
	sb.WriteString("\n")

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

	// The transport line is never silent about a degradation: a user watching a stale
	// pane must be able to see WHY it is stale.
	switch {
	case d.streamErr != nil && d.polling:
		sb.WriteString(m.pal.faint.Render("live stream unavailable (" + fmtErr(d.streamErr) + ") — falling back to a 2s refresh"))
	case d.polling:
		sb.WriteString(m.pal.faint.Render("reconnecting — refreshing every 2s"))
	case d.stream != nil:
		sb.WriteString(m.pal.faint.Render("live"))
	default:
		sb.WriteString(m.pal.faint.Render("connecting…"))
	}
	sb.WriteString("\n\n")

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
	sb.WriteString(m.renderSteerBar() + "\n")
	sb.WriteString(m.pal.faint.Render("tab/h/l lane · j/k scroll · r refresh · esc back · ? keys"))
	return sb.String()
}

func (m tuiModel) renderLaneRail() string {
	d := &m.detail
	now := time.Now()
	active := activeLaneKey(d.run.Status, d.frames)

	var sb strings.Builder
	sb.WriteString(m.pal.faint.Render("AGENTS") + "\n")
	if len(d.lanes) == 0 {
		sb.WriteString(m.pal.faint.Render("(no activity yet)"))
		return sb.String()
	}
	for i, l := range d.lanes {
		st := crewStateFor(d.run.Status, d.run.Health, l.Key, active, l.LastActivity, now)
		// Role and label are model-authored: plain, clamped, never markdown.
		name := m.renderer.Plain(l.Role, 14)
		if l.Key != l.Role {
			// The instance id is UNTRUSTED like everything else on this rail: it is the
			// SDK's parent_tool_use_id, forwarded verbatim. shortInstanceID only takes a
			// tail — it does not sanitize — so the result goes through Plain like every
			// other cell. Rendering it raw was a real hole, caught by
			// TestTUIViewsStripControlBytesFromUntrustedText.
			name += m.pal.faint.Render("·" + m.renderer.Plain(shortInstanceID(l.Key), 8))
		}
		row := m.pal.state(st).Render(laneDot(st)) + " " + name
		if i == d.laneIdx {
			sb.WriteString(m.pal.sel.Render("▸" + row))
		} else {
			sb.WriteString(" " + row)
		}
		sb.WriteString("\n")
		if l.Label != "" {
			sb.WriteString("   " + m.pal.faint.Render(m.renderer.Plain(l.Label, laneLabelCap)) + "\n")
		}
	}
	return sb.String()
}

func (m tuiModel) renderTranscript() string {
	lane, ok := m.detail.selectedLane()
	if !ok {
		return m.pal.faint.Render("no lane selected")
	}
	var sb strings.Builder
	for _, f := range lane.Frames {
		sb.WriteString(m.pal.faint.Render("#"+itoa(int(f.Seq))+" "+m.renderer.Plain(f.Kind, 16)) + "\n")
		sb.WriteString(m.renderer.Markdown(transcriptText(f)) + "\n\n")
	}
	out := strings.Split(sb.String(), "\n")
	if m.detail.scroll < len(out) {
		out = out[m.detail.scroll:]
	}
	if h := m.height - 10; h > 0 && len(out) > h {
		out = out[:h]
	}
	return strings.Join(out, "\n")
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

// joinColumns places the rail beside the body at a fixed gutter.
func joinColumns(left, right string, width int) string {
	l := strings.Split(left, "\n")
	r := strings.Split(right, "\n")
	n := len(l)
	if len(r) > n {
		n = len(r)
	}
	var sb strings.Builder
	for i := 0; i < n; i++ {
		var lv, rv string
		if i < len(l) {
			lv = l[i]
		}
		if i < len(r) {
			rv = r[i]
		}
		sb.WriteString(padVisual(lv, width) + " │ " + rv + "\n")
	}
	return sb.String()
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
