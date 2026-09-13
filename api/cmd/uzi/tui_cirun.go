package main

// tui_cirun.go — the forge CI-run drill-in (viewCIRun, PRD #1255 M6, D3/D7/D8/D9/D13): the JOBS
// (and, for GitHub Actions, the selected job's STEPS) of one CI run, re-polled live every 5s on
// its OWN poll-guard tick chain (a copy of the board's, with its own reqSeq/waitID/tickGen/
// errStreak — see ciRunState). It is the sibling of the PR drill-in (tui_pr.go), opened from the
// ci list (enter/→), and is the m6 twin of viewPR in every structural respect: the same live
// header (`● live · 5s` + `re-polled Ns ago`), the same ↑↓ cursor over a section, the same inline
// `f fix ci` action surfacing the server's reason, and — critically — a model-level ciRunGen
// SESSION generation so a stale GetCIRun reply from a previously-open run can never apply to a
// newly-open run (the cross-entity contamination class the PR view's prGen fixed). Every forge-
// authored string is drawn through m.renderer.Plain (D7); every job/step state carries a glyph AND
// a word so colour is never the only cue (D8); a job URL reaches the frame only through oscLink,
// https-gated (D7/D9); every width-derived cap is floored and every rendered line is clamped to
// m.width so the view never panics or overflows at any terminal width (the m4b/m5 width lesson).

import (
	"context"
	"image/color"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/pipelinestatus"
)

// ---- messages -------------------------------------------------------------

// ciRunMsg carries a GetCIRun reply for the CI-run drill-in, the ci-run analogue of prMsg. reqID is
// the request-generation id this reply belongs to; the model honours it only when reqID ==
// m.cirun.waitID, so an older poll that resolves after a newer request was minted is dropped. gen
// is the CI-run SESSION generation (ciRunState.gen) the request was issued under: a reopen resets
// reqSeq so a prior run's reply can collide on reqID, and the gen check is what rejects it (see
// ciRunState.gen).
type ciRunMsg struct {
	detail apitypes.CIRunDetailDTO
	err    error
	reqID  uint64
	gen    uint64
}

// ciRunTickMsg drives the CI-run drill-in's own 5s live re-poll chain, carrying the tick-chain
// generation it was scheduled under (a tick whose gen != m.cirun.tickGen is from a superseded chain
// and is dropped), mirroring prTickMsg.
type ciRunTickMsg struct{ gen uint64 }

// ---- poll cadence ---------------------------------------------------------

// ciRunPollInterval is the CI-run drill-in's live re-poll cadence (PRD #1255 D4: a drill-in polls
// every 5s). A var (not const) so a test can shrink it.
var ciRunPollInterval = 5 * time.Second

// ciRunBackoffCap caps the CI-run error-backoff reschedule interval, the ci-run twin of
// prBackoffCap. A var so a test can shrink it.
var ciRunBackoffCap = 60 * time.Second

// ciRunTickAfter arms the CI-run tick after delay d, stamping the produced ciRunTickMsg with the
// tick-chain generation gen — the ci-run twin of prTickAfter.
func ciRunTickAfter(d time.Duration, gen uint64) tea.Cmd {
	return tea.Tick(d, func(time.Time) tea.Msg { return ciRunTickMsg{gen: gen} })
}

// ciRunTickInterval maps the consecutive CI-run-poll error streak to the reschedule interval (base
// at streak 0, doubling per consecutive failure, clamped at ciRunBackoffCap), the ci-run twin of
// prTickInterval — a pure function so the backoff is unit-assertable.
func ciRunTickInterval(streak int) time.Duration {
	if streak <= 0 {
		return ciRunPollInterval
	}
	if streak > 16 {
		streak = 16
	}
	d := ciRunPollInterval << uint(streak)
	if d <= 0 || d > ciRunBackoffCap {
		return ciRunBackoffCap
	}
	return d
}

// ---- state ----------------------------------------------------------------

type ciRunState struct {
	repoID string
	runID  int64
	detail apitypes.CIRunDetailDTO
	cursor int // over detail.Jobs
	err    error

	// gen is this CI-run session's generation, stamped from tuiModel.ciRunGen by startCIRunReq on
	// the first fetch of each drill-in (from the ci row open). newCIRunState resets reqSeq/waitID to
	// 0 on every open, so a prior run's in-flight GetCIRun mints the SAME reqID as this one and
	// passes the reqID==waitID guard; the monotonic gen is the session check that rejects it,
	// mirroring prState.gen. esc cannot cancel a command in flight, and reopening the SAME run also
	// advances gen, so a stale reply is dropped on gen before its detail can be applied to the wrong
	// run.
	gen uint64

	// reqSeq / waitID / tickGen are the request-generation guard, a copy of the board's (see
	// boardState): waitID == 0 is idle and a tick polls ONLY while idle (the in-flight guard); a
	// ciRunMsg is honoured only when reqID == waitID (the out-of-order defence); tickGen is the
	// tick-chain generation (a mismatched tick is from a superseded chain).
	reqSeq    uint64
	waitID    uint64
	tickGen   uint64
	errStreak int

	// loaded is true once a SUCCESSFUL reply has landed, so the view can tell "still loading the
	// first page" from a genuinely empty run. lastPollAt is the clock of the last SUCCESSFUL poll,
	// rendered as `re-polled Ns ago` in the JOBS heading (D4).
	loaded     bool
	lastPollAt time.Time

	// rateLimited / retryAfter render the header's `~ rate-limited · retry in Ns` state (D4): a
	// forge 429 maps to ExitUnreachable carrying a Retry-After, which backs off the poll and shows
	// the strip instead of an error line.
	rateLimited bool
	retryAfter  time.Duration
}

func newCIRunState(repoID string, runID int64) ciRunState {
	// tickGen starts at 1 so the Init-armed CI-run tick is honoured; reqSeq / waitID start at 0
	// (idle) because the first fetch is the immediate startCIRunReq the open path issues — not an
	// Init fetch.
	return ciRunState{repoID: repoID, runID: runID, tickGen: 1}
}

func (c *ciRunState) apply(msg ciRunMsg) {
	if msg.err != nil {
		c.err = msg.err
		c.errStreak++
		if ra, ok := rateLimitRetry(msg.err); ok {
			c.rateLimited = true
			c.retryAfter = ra
		}
		return
	}
	c.err = nil
	c.errStreak = 0
	c.rateLimited = false
	c.retryAfter = 0
	c.detail = msg.detail
	c.loaded = true
	c.lastPollAt = time.Now()
	c.clampCursor()
}

func (c *ciRunState) clampCursor() {
	n := len(c.detail.Jobs)
	if c.cursor >= n {
		c.cursor = n - 1
	}
	if c.cursor < 0 {
		c.cursor = 0
	}
}

// ---- commands -------------------------------------------------------------

// fetchCIRunCmd reads one CI run's detail (jobs + steps), tagged with the request-generation id so
// a stale reply is dropped (PRD #1255 D4; the #1130 poll-guard pattern).
func (m tuiModel) fetchCIRunCmd(repoID string, runID int64, reqID uint64) tea.Cmd {
	c, parent, gen := m.client, m.ctx, m.cirun.gen
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, boardPollTimeout)
		defer cancel()
		detail, err := c.GetCIRun(ctx, repoID, runID)
		return ciRunMsg{detail: detail, err: err, reqID: reqID, gen: gen}
	}
}

// startCIRunReq mints the next CI-run request id, records it as the one the model is waiting on, and
// returns the tagged fetch — the ci-run twin of startPRReq. It returns nil when no run is resolved.
//
// It also advances the model-level CI-run SESSION generation on the FIRST request of each drill-in.
// newCIRunState resets reqSeq to 0 on every open, so reqSeq == 0 here is true exactly once per open
// (the immediate fetch the open path issues, never a re-poll or a refresh, whose reqSeq is already
// > 0). Stamping ciRunGen there — rather than at the ci-row open site — keeps the guard
// self-contained while giving each session a unique, never-reset gen, so a previous run's in-flight
// reply (whose reset reqSeq minted the SAME reqID as this session) is rejected on gen by the
// ciRunMsg handler.
func (m *tuiModel) startCIRunReq() tea.Cmd {
	if m.cirun.repoID == "" {
		return nil
	}
	if m.cirun.reqSeq == 0 {
		m.ciRunGen++
		m.cirun.gen = m.ciRunGen
	}
	m.cirun.reqSeq++
	m.cirun.waitID = m.cirun.reqSeq
	return m.fetchCIRunCmd(m.cirun.repoID, m.cirun.runID, m.cirun.waitID)
}

// ---- keys -----------------------------------------------------------------

func (m tuiModel) ciRunKey(k string) (tea.Model, tea.Cmd) {
	// ↑↓/jk move the cursor over the jobs; the selected job expands its steps beneath it.
	if d := motionDelta(k); d != 0 {
		m.cirun.cursor += d
		m.cirun.clampCursor()
		return m, nil
	}
	switch k {
	case keyEsc:
		// esc returns to the ci list — the only entry point — which is never clobbered (m.ci persists
		// on the model), so it is still loaded on return (D1).
		m.view = viewCI
		return m, nil
	case keyRefresh:
		// A keypress is intent: never gated on the in-flight guard, mirroring the PR view's r.
		if m.cirun.repoID != "" {
			return m, (&m).startCIRunReq()
		}
		return m, nil
	case keyFixCI:
		// f: queue a CI-fix run for this run's branch. The server 409s an unwatched/green ref (R6);
		// that reason is drawn inline (prActionNotice, shared with the PR view).
		if m.cirun.repoID != "" {
			return m, m.fixCICmd(m.cirun.repoID, m.cirun.detail.Branch)
		}
		return m, nil
	}
	return m, nil
}

// ---- forge text projection (D7) -------------------------------------------

// ciJobText / ciStepText project a job's / step's forge-authored Name into a DISTINCTLY-NAMED
// internal field so the D7 AST guard can require each to be drawn through a sanitizer. CIJobDTO.Name
// and CIStepDTO.Name share the bare selector name `.Name` with unrelated pre-existing draws in other
// tui_*.go files (e.g. the tool payload .Name in tui_detail_transcript.go, drawn raw), so that bare
// name cannot go in d7UntrustedFields. The distinct names (jobName, stepName) are added there
// instead, and the copy below is plumbing — a struct-field copy, not a draw — so it is not a guard
// hit. The embedded CIRunDTO header fields (workflow Name / Event / Branch / SHA / Title / Actor)
// reuse the ci-list projection ciTextOf (tui_ci.go), whose distinct names are already registered.
type ciJobText struct {
	jobName string // CIJobDTO.Name
}

func ciJobTextOf(j apitypes.CIJobDTO) ciJobText { return ciJobText{jobName: j.Name} }

type ciStepText struct {
	stepName string // CIStepDTO.Name
}

func ciStepTextOf(s apitypes.CIStepDTO) ciStepText { return ciStepText{stepName: s.Name} }

// ciJobEffective / ciStepEffective are the raw forge status to classify/colour a job / step by: the
// Conclusion once the forge split it out (completed units), else the Status (a unit still in flight
// has no conclusion).
func ciJobEffective(j apitypes.CIJobDTO) string {
	if j.Conclusion != "" {
		return j.Conclusion
	}
	return j.Status
}

func ciStepEffective(s apitypes.CIStepDTO) string {
	if s.Conclusion != "" {
		return s.Conclusion
	}
	return s.Status
}

// toneGlyph is a unit's spine glyph + colour by the five-tone class of its effective status
// (D3/D8): ✗ failed, ● running, ✓ passed, ⚠ attention, · neutral/skipped. Every state has a
// distinct glyph so colour is never the only carrier. Shared by the job and step rows.
func (m tuiModel) toneGlyph(status string) (string, color.Color) {
	switch pipelinestatus.Tone(status) {
	case "failed":
		return "✗", m.pal.alarm
	case "running":
		return "●", m.pal.wait
	case "passed":
		return "✓", m.pal.sage
	case "attention":
		return "⚠", m.pal.amber
	default:
		return "·", m.pal.faintC
	}
}

// ciRunJobCounts tallies the jobs by five-tone class (D3): the carriers of the header rollup and
// the JOBS heading.
func (m tuiModel) ciRunJobCounts() (running, failing, passed, total int) {
	for _, j := range m.cirun.detail.Jobs {
		total++
		switch pipelinestatus.Tone(ciJobEffective(j)) {
		case "failed":
			failing++
		case "running":
			running++
		case "passed":
			passed++
		}
	}
	return
}

func (m tuiModel) ciRunSelectedJob() (apitypes.CIJobDTO, bool) {
	jobs := m.cirun.detail.Jobs
	if m.cirun.cursor < 0 || m.cirun.cursor >= len(jobs) {
		return apitypes.CIJobDTO{}, false
	}
	return jobs[m.cirun.cursor], true
}

// ---- render ---------------------------------------------------------------

// Fixed column widths for a JOBS row. Narrow panes are handled by the final clampVisual to m.width
// (the backstop) plus capCell's ≤0 guard, so these are layout hints, not floors.
const (
	ciRunJobNameWidth  = 36
	ciRunStepNameWidth = 32
	ciRunStateW        = 8 // forgeState word: running / queued / passed / failed / done
	ciRunElapsedW      = 7 // elapsedBetween → shortDuration (coarse, e.g. 3m / 3h4m)
)

// shortSHA is the first 7 runes of a forge-authored commit SHA for the `sha7` header cell. The SHA
// is sliced through its sanitized (cellText) form so the count is over sanitized runes, and the
// ≤7-rune result is still drawn through renderer.Plain by the caller (the D7 sink) — a SHA is hex so
// no control rune survives the slice, but the Plain draw keeps the guard honest. Slicing first is
// what makes `sha7` honest: Plain(sha, 7) on a full 40-char SHA would append an ellipsis (6 hex + …),
// whereas Plain on an already-≤7-rune value adds none (e.g. `deadbee`, not `deadbe…`).
func shortSHA(sha string) string {
	r := []rune(cellText(sha))
	if len(r) > 7 {
		r = r[:7]
	}
	return string(r)
}

// ciRunPlainWidth floors a width-derived renderer.Plain / capCell cap so a narrow pane TRUNCATES
// gracefully instead of underflowing the rune slice; every m.width-N cap in this file passes through
// it, and renderCIRun's clampVisual to m.width is the backstop for the whole line (the m5 lesson).
func ciRunPlainWidth(n int) int {
	if n < 1 {
		return 1
	}
	return n
}

func (m tuiModel) renderCIRun() string {
	var sb strings.Builder

	sb.WriteString(clampVisual(m.ciRunHeaderLine1(), m.width) + "\n")
	sb.WriteString(clampVisual(m.ciRunHeaderLine2(), m.width) + "\n")
	if note := m.ciRunHeaderNote(); note != "" {
		sb.WriteString(clampVisual(note, m.width) + "\n")
	}

	if !m.cirun.loaded {
		sb.WriteString("\n" + clampVisual(m.pal.faint.Render(" loading…"), m.width))
		return sb.String()
	}
	sb.WriteString("\n")

	d := m.cirun.detail
	sb.WriteString(clampVisual(m.ciRunJobsHeading(), m.width) + "\n")

	switch {
	case d.Unsupported != "":
		// The forge degraded (ErrForgeVersionUnsupported); render the server's sentence verbatim
		// rather than guessing at the reason (D5/R2).
		sb.WriteString(clampVisual(m.pal.faint.Render(" "+m.renderer.Plain(d.Unsupported, 80)), m.width) + "\n")
	case len(d.Jobs) == 0:
		// A resolved run with no jobs (some forges never populate them), or a run that has not
		// scheduled any yet: a left-aligned, sentence-case guiding line like the list empty states.
		sb.WriteString(clampVisual(m.pal.faint.Render(" No jobs reported for this run yet."), m.width) + "\n")
	default:
		lines, anchor := m.ciRunBodyLines()
		start, end := ciRunWindow(anchor, len(lines), m.ciRunCapacity())
		for i := start; i < end; i++ {
			sb.WriteString(clampVisual(lines[i], m.width) + "\n")
		}
	}

	sb.WriteString(clampVisual(m.ciRunFooter(), m.width))
	return sb.String()
}

// ciRunBodyLines renders every JOBS line into a flat slice — one row per job, and for the SELECTED
// job its steps indented beneath it (a GitLab/Forgejo job has no steps, so it shows just its row —
// the honest degrade) plus a faint copyable URL line — and returns the index of the selected job's
// row so the window can keep it visible. Flattening first is what keeps the variable-height step
// expansion from ever overflowing the frame: the window is applied to the finished slice.
func (m tuiModel) ciRunBodyLines() (lines []string, anchor int) {
	for i, j := range m.cirun.detail.Jobs {
		sel := i == m.cirun.cursor
		if sel {
			anchor = len(lines)
		}
		lines = append(lines, m.ciRunJobRow(j, sel))
		if sel {
			// The selected job expands its STEPS (GitHub Actions only; empty on GitLab/Forgejo, which
			// then shows just the job row above — the documented no-steps degrade).
			for _, s := range j.Steps {
				lines = append(lines, m.ciRunStepRow(s))
			}
			if url := m.ciRunJobURLLine(j); url != "" {
				lines = append(lines, url)
			}
		}
	}
	return lines, anchor
}

// ciRunCapacity is how many JOBS body lines fit between the header chrome and the footer, the
// ci-run twin of prChecksCapacity, so the drill-in never overflows at the current height.
func (m tuiModel) ciRunCapacity() int {
	used := 2 // the two header lines
	if m.ciRunHeaderNote() != "" {
		used++
	}
	used += 2 // the blank line + the JOBS heading
	used++    // the footer
	c := m.height - used
	if c < 1 {
		c = 1
	}
	return c
}

// ciRunWindow keeps the selected job's row (anchor) visible within capacity lines, cursor-centred,
// so ↑↓ over a long jobs list always shows the selected job and its expanded steps — the ci-run
// twin of prChecksWindow.
func ciRunWindow(anchor, total, capacity int) (start, end int) {
	if capacity < 1 {
		capacity = 1
	}
	if total <= capacity {
		return 0, total
	}
	start = anchor - capacity/2
	if start < 0 {
		start = 0
	}
	if start+capacity > total {
		start = total - capacity
	}
	return start, start + capacity
}

// ciRunHeaderLine1 is the breadcrumb + `<workflow> #<number> · <event> · <branch> · <sha7>` with a
// right-aligned rollup and the `● live · 5s` cadence tag. The workflow/event/branch/sha are
// forge-authored → renderer.Plain via the shared ciTextOf projection (D7).
func (m tuiModel) ciRunHeaderLine1() string {
	t := ciTextOf(m.cirun.detail.CIRunDTO)
	left := m.pal.faint.Render("‹ ci  ") +
		m.pal.title.Render(m.renderer.Plain(t.runName, 24)+" #"+itoa(int(m.cirun.detail.Number))) +
		m.pal.faint.Render(" · ") +
		m.pal.faint.Render(m.renderer.Plain(t.eventName, 14)) +
		m.pal.faint.Render(" · ") +
		m.pal.faint.Render(m.renderer.Plain(t.runBranch, 22)) +
		m.pal.faint.Render(" · ") +
		m.pal.faint.Render(m.renderer.Plain(shortSHA(t.runSHA), 7))
	right := m.ciRunRollup() +
		m.pal.faint.Render("   ") +
		paintSeg(m.pal.wait, nil, false, "● live") +
		m.pal.faint.Render(" · "+shortDuration(ciRunPollInterval))
	target := m.width - visualWidth(right) - 1
	if target < 1 {
		return left
	}
	return padVisual(clampVisual(left, target), target) + " " + right
}

// ciRunHeaderLine2 is the run's title (untrusted, clipped) · actor · age. Title and actor are
// forge-authored → renderer.Plain via ciTextOf (D7).
func (m tuiModel) ciRunHeaderLine2() string {
	t := ciTextOf(m.cirun.detail.CIRunDTO)
	bold := lipgloss.NewStyle().Bold(true)
	left := m.pal.faint.Render(" ") + bold.Render(m.renderer.Plain(t.runTitle, 80))
	left += m.pal.faint.Render(" · ") + paintSeg(m.pal.sage, nil, false, m.renderer.Plain(t.actorLogin, 24))
	left += m.pal.faint.Render(" · " + relAge(m.cirun.detail.CreatedAt))
	return left
}

// ciRunRollup is the header's top-right rollup (D3): `✗ N failing`, or `● N running · ✓ done/total`,
// or `✓ all jobs passed` / `✓ P/T passed`, computed from the jobs; with no jobs it falls back to the
// run's own status word (the unsupported / not-yet-scheduled case). Every variant carries words, so
// it survives a colour-stripped profile (D8).
func (m tuiModel) ciRunRollup() string {
	running, failing, passed, total := m.ciRunJobCounts()
	switch {
	case total == 0:
		return m.ciRunStatusWord()
	case failing > 0:
		return paintSeg(m.pal.alarm, nil, false, "✗ "+itoa(failing)+" failing")
	case running > 0:
		done := total - running
		return paintSeg(m.pal.wait, nil, false, "● "+itoa(running)+" running") +
			m.pal.faint.Render(" · ") +
			paintSeg(m.pal.sage, nil, false, "✓ "+itoa(done)+"/"+itoa(total))
	case passed == total:
		return paintSeg(m.pal.sage, nil, false, "✓ all jobs passed")
	default:
		return paintSeg(m.pal.sage, nil, false, "✓ "+itoa(passed)+"/"+itoa(total)+" passed")
	}
}

// ciRunStatusWord is the rollup fallback when the run carries no jobs (a forge-version degrade or a
// run that has scheduled nothing yet): the run's own five-tone status as a glyph + word (D8).
func (m tuiModel) ciRunStatusWord() string {
	switch pipelinestatus.Tone(ciRunEffective(m.cirun.detail.CIRunDTO)) {
	case "running":
		return paintSeg(m.pal.wait, nil, false, "● running")
	case "failed":
		return paintSeg(m.pal.alarm, nil, false, "✗ failing")
	case "passed":
		return paintSeg(m.pal.sage, nil, false, "✓ passed")
	case "attention":
		return paintSeg(m.pal.amber, nil, false, "⚠ attention")
	default:
		return m.pal.faint.Render("· no jobs")
	}
}

// ciRunHeaderNote is the sub-header line: a transient f (fix ci) notice (the user just acted, so it
// is the most relevant thing) takes priority over the forge rate-limit state (D4), which takes
// priority over a generic refresh error. forgeNotice is already sanitized where it is set.
func (m tuiModel) ciRunHeaderNote() string {
	if m.forgeNotice != "" {
		return m.pal.faint.Render(" " + m.forgeNotice)
	}
	if m.cirun.rateLimited {
		retry := "soon"
		if m.cirun.retryAfter > 0 {
			retry = shortDuration(m.cirun.retryAfter)
		}
		return m.pal.faint.Render(" ~ rate-limited · retry in " + retry)
	}
	if m.cirun.err != nil {
		return m.pal.faint.Render(" could not refresh: " + fmtErr(m.cirun.err))
	}
	return ""
}

// ciRunJobsHeading summarizes the JOBS section and carries the `re-polled Ns ago` cadence proof
// (D4). The aggregate words (failing / running / passed) are the D8 carrier for the whole section.
func (m tuiModel) ciRunJobsHeading() string {
	running, failing, passed, total := m.ciRunJobCounts()
	head := m.pal.title.Render(" JOBS") + "  "
	var summary string
	switch {
	case total == 0:
		summary = m.pal.faint.Render("· no jobs reported")
	case failing > 0:
		summary = paintSeg(m.pal.alarm, nil, false, "✗ "+itoa(failing)+" failing") +
			m.pal.faint.Render(" · "+itoa(passed)+" passed · "+itoa(running)+" running")
	case running > 0:
		summary = paintSeg(m.pal.wait, nil, false, "● "+itoa(running)+" running") +
			m.pal.faint.Render(" · "+itoa(passed)+" passed · "+itoa(failing)+" failing")
	case passed == total:
		summary = paintSeg(m.pal.sage, nil, false, "✓ all jobs passed") +
			m.pal.faint.Render(" · "+itoa(total)+" jobs")
	default:
		// failing == 0 && running == 0, but some jobs sit in the attention (action_required /
		// manual / warning → Tone "attention") or neutral/skipped tone — counted in total, never in
		// passed. Mirror ciRunRollup: show an honest count, never a false "all jobs passed" (an
		// all-skipped run would otherwise read "all jobs passed · N jobs" with 0 actually passed).
		summary = paintSeg(m.pal.sage, nil, false, "✓ "+itoa(passed)+"/"+itoa(total)+" passed") +
			m.pal.faint.Render(" · "+itoa(total)+" jobs")
	}
	repolled := ""
	if m.cirun.loaded {
		repolled = m.pal.faint.Render(" · re-polled " + relAge(m.cirun.lastPollAt) + " ago")
	}
	return head + summary + repolled
}

// ciRunJobRow renders one job: the ▌ spine + state glyph, the name, a state WORD (forgeState — the
// D8 word beside the glyph) and the elapsed (StartedAt→FinishedAt, or →now while running). Name and
// the state word are forge-authored → renderer.Plain (D7).
func (m tuiModel) ciRunJobRow(j apitypes.CIJobDTO, sel bool) string {
	t := ciJobTextOf(j)
	glyph, c := m.toneGlyph(ciJobEffective(j))

	var bg color.Color
	if sel {
		bg = m.pal.selBg
	}
	nameC := color.Color(nil)
	if sel {
		nameC = m.pal.tungsten
	}
	cursor := paintSeg(nil, bg, false, " ")
	if sel {
		cursor = paintSeg(m.pal.tungsten, bg, true, "▸")
	}
	gap := paintSeg(nil, bg, false, "  ")

	row := cursor +
		paintSeg(c, bg, false, "▌") +
		paintSeg(c, bg, false, glyph) +
		paintSeg(nil, bg, false, " ") +
		paintSeg(nameC, bg, false, padCell(m.renderer.Plain(t.jobName, ciRunJobNameWidth), ciRunJobNameWidth)) + gap +
		// State WORD beside the glyph (D8). forgeState can echo an unknown forge status verbatim, so
		// it is sanitized through renderer.Plain before drawing.
		paintSeg(c, bg, false, padCell(m.renderer.Plain(forgeState(j.Status, j.Conclusion), ciRunStateW), ciRunStateW))

	// Elapsed flushed to a fixed right cell so the column stays stable down the list; padSeg never
	// truncates and clampVisual (whole row) is the width backstop.
	row = padSeg(row, m.width-ciRunElapsedW, bg)
	row += padSeg(paintSeg(m.pal.faintC, bg, false, clampVisual(elapsedBetween(j.StartedAt, j.FinishedAt, j.Status == "completed"), ciRunElapsedW)), ciRunElapsedW, bg)
	return clampVisual(row, m.width)
}

// ciRunStepRow renders one step of the selected job, indented beneath it: `  <glyph> <name> · <word>
// · <elapsed>` (D3). A FAILING step draws its name in the alarm tone (D8) but still carries its
// glyph and word. Name and the state word are forge-authored → renderer.Plain (D7).
func (m tuiModel) ciRunStepRow(s apitypes.CIStepDTO) string {
	ts := ciStepTextOf(s)
	eff := ciStepEffective(s)
	glyph, c := m.toneGlyph(eff)
	nameC := m.pal.faintC
	if pipelinestatus.Tone(eff) == "failed" {
		nameC = m.pal.alarm
	}
	line := paintSeg(c, nil, false, "    "+glyph+" ") +
		paintSeg(nameC, nil, false, m.renderer.Plain(ts.stepName, ciRunStepNameWidth)) +
		m.pal.faint.Render(" · ") +
		paintSeg(m.pal.faintC, nil, false, m.renderer.Plain(forgeState(s.Status, s.Conclusion), ciRunStateW)) +
		m.pal.faint.Render(" · "+elapsedBetween(s.StartedAt, s.CompletedAt, s.Status == "completed"))
	return clampVisual(line, m.width)
}

// ciRunJobURLLine is the selected job's URL, drawn faint beneath its steps as `↗ <url>` so a user on
// a terminal without hyperlink support can copy it (D9). The url reaches the frame only through
// oscLink (the sanitizing sink that strips every control/format rune from the OSC-8 target), and an
// OSC-8 envelope is emitted ONLY when the url parses as https AND the terminal supports links
// (D7/D9); the visible `↗ url` text goes through renderer.Plain regardless. It takes the whole
// CIJobDTO (like prCheckURLLine takes the CheckDTO) so the WebURL access stays inside here.
func (m tuiModel) ciRunJobURLLine(j apitypes.CIJobDTO) string {
	if j.WebURL == "" {
		return ""
	}
	text := m.pal.faint.Render("    ↗ " + m.renderer.Plain(j.WebURL, ciRunPlainWidth(m.width-7)))
	if m.linksEnabled() && isHTTPSURL(j.WebURL) {
		return oscLink(j.WebURL, text)
	}
	return text
}

// ciRunFooter is the one-line key legend for the CI-run view (D13). ↗ appears only when the selected
// job carries a URL.
func (m tuiModel) ciRunFooter() string {
	parts := []string{m.keyHint("↑↓", "move")}
	if j, ok := m.ciRunSelectedJob(); ok && j.WebURL != "" {
		parts = append(parts, m.keyHint("↗", "open job"))
	}
	parts = append(parts, m.keyHint("f", "fix ci"), m.keyHint("esc", "back"), m.keyHint("?", "keys"))
	return " " + strings.Join(parts, m.pal.faint.Render(" · "))
}
