package main

// Transcript build/render and tool-frame helpers for the run detail view (PRD #1009 M3):
// turning a lane's frames into windowed display lines, the viewport/scroll math, and the
// JSON tool-use / tool-result payload extractors the transcript reads.

import (
	"encoding/json"
	"strings"
	"time"

	lipgloss "charm.land/lipgloss/v2"
)

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
	// The transcript pane waits on its own tail page while the header/rail are already up
	// (PRD #1137 M4). With no frame to show yet, the pane alone reports its state: a failed
	// tail page shows the error HERE (the header stays up — a page error never collapses the
	// whole view), otherwise the loading placeholder until the newest page lands. A live
	// frame that beat the tail (len(frames) > 0) renders instead of hiding.
	if len(m.detail.frames) == 0 {
		var placeholder string
		switch {
		case m.detail.pageErr != nil:
			placeholder = "could not load transcript: " + fmtErr(m.detail.pageErr) + " · r to retry"
		case !m.detail.tailLoaded:
			placeholder = "loading…"
		}
		if placeholder != "" {
			return m.padPaneTitle(title, "") + "\n" +
				padLinesToViewport([]string{m.pal.faint.Render(placeholder)}, m.transcriptViewport())
		}
		// tailLoaded with no frames and no error: a genuinely empty run — fall through to
		// the normal (empty) transcript render below.
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
