package main

// tui_pr.go — the forge PR drill-in (viewPR, PRD #1255 M5, D1/D3/D4/D7/D8/D9/D13): CHECKS,
// REVIEWS and MERGE for one open PR, re-polled live every 5s on its OWN poll-guard tick chain (a
// copy of the board's, with its own reqSeq/waitID/tickGen/errStreak — see prState). It is a peer
// of the run-detail drill-in, opened from the pulls list (enter/→) or the run view (m), and
// cross-linked back to the run view (u ↳ run). Every forge-authored string is drawn through
// m.renderer.Plain (D7); every check/review/merge state carries a glyph AND a word so colour is
// never the only cue (D8); a forge URL reaches the frame only through oscLink, https-gated
// (D7/D9); every rendered line is clamped to m.width (the m4b width lesson).

import (
	"context"
	"image/color"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/pipelinestatus"
)

// ---- messages -------------------------------------------------------------

// prMsg carries a GetPull reply for the PR drill-in, the PR analogue of pullsMsg. reqID is the
// request-generation id this reply belongs to; the model honours it only when reqID ==
// m.pr.waitID, so an older poll that resolves after a newer request was minted is dropped.
type prMsg struct {
	detail apitypes.PullDetailDTO
	err    error
	reqID  uint64
}

// prTickMsg drives the PR drill-in's own 5s live re-poll chain, carrying the tick-chain generation
// it was scheduled under (a tick whose gen != m.pr.tickGen is from a superseded chain and is
// dropped), mirroring pullsTickMsg / ciTickMsg.
type prTickMsg struct{ gen uint64 }

// prActionMsg carries the result of a w (rework) / f (fix ci) action fired from the PR view or a
// pulls list row (PRD #1255 D1/D12). kind is "rework" | "fixci"; on success run is the queued run,
// and on a typed 4xx/409 err carries the server's reason — drawn inline (never a crash).
type prActionMsg struct {
	kind string
	run  apitypes.RunDTO
	err  error
}

// ---- poll cadence ---------------------------------------------------------

// prPollInterval is the PR drill-in's live re-poll cadence (PRD #1255 D4: a drill-in polls every
// 5s). A var (not const) so a test can shrink it.
var prPollInterval = 5 * time.Second

// prBackoffCap caps the PR error-backoff reschedule interval, the PR twin of pullsBackoffCap. A var
// so a test can shrink it.
var prBackoffCap = 60 * time.Second

// prTickAfter arms the PR tick after delay d, stamping the produced prTickMsg with the tick-chain
// generation gen — the PR twin of pullsTickAfter.
func prTickAfter(d time.Duration, gen uint64) tea.Cmd {
	return tea.Tick(d, func(time.Time) tea.Msg { return prTickMsg{gen: gen} })
}

// prTickInterval maps the consecutive PR-poll error streak to the reschedule interval (base at
// streak 0, doubling per consecutive failure, clamped at prBackoffCap), the PR twin of
// pullsTickInterval — a pure function so the backoff is unit-assertable.
func prTickInterval(streak int) time.Duration {
	if streak <= 0 {
		return prPollInterval
	}
	if streak > 16 {
		streak = 16
	}
	d := prPollInterval << uint(streak)
	if d <= 0 || d > prBackoffCap {
		return prBackoffCap
	}
	return d
}

// ---- state ----------------------------------------------------------------

type prState struct {
	repoID string
	iid    int64
	detail apitypes.PullDetailDTO
	cursor int // over the sorted CHECKS list
	err    error

	// reqSeq / waitID / tickGen are the request-generation guard, a copy of the board's (see
	// boardState): waitID == 0 is idle and a tick polls ONLY while idle (the in-flight guard); a
	// prMsg is honoured only when reqID == waitID (the out-of-order defence); tickGen is the
	// tick-chain generation (a mismatched tick is from a superseded chain).
	reqSeq    uint64
	waitID    uint64
	tickGen   uint64
	errStreak int

	// loaded is true once a SUCCESSFUL reply has landed, so the view can tell "still loading the
	// first page" from a genuinely empty PR. lastPollAt is the clock of the last SUCCESSFUL poll,
	// rendered as `re-polled Ns ago` in the CHECKS heading (D4).
	loaded     bool
	lastPollAt time.Time

	// rateLimited / retryAfter render the header's `~ rate-limited · retry in Ns` state (D4): a
	// forge 429 maps to ExitUnreachable carrying a Retry-After, which backs off the poll and shows
	// the strip instead of an error line.
	rateLimited bool
	retryAfter  time.Duration
}

func newPRState(repoID string, iid int64) prState {
	// tickGen starts at 1 so the Init-armed PR tick is honoured; reqSeq / waitID start at 0 (idle)
	// because the first fetch is the immediate startPRReq the open path issues — not an Init fetch.
	return prState{repoID: repoID, iid: iid, tickGen: 1}
}

func (p *prState) apply(msg prMsg) {
	if msg.err != nil {
		p.err = msg.err
		p.errStreak++
		if ra, ok := rateLimitRetry(msg.err); ok {
			p.rateLimited = true
			p.retryAfter = ra
		}
		return
	}
	p.err = nil
	p.errStreak = 0
	p.rateLimited = false
	p.retryAfter = 0
	p.detail = msg.detail
	p.loaded = true
	p.lastPollAt = time.Now()
	p.clampCursor()
}

func (p *prState) clampCursor() {
	n := len(p.detail.Checks)
	if p.cursor >= n {
		p.cursor = n - 1
	}
	if p.cursor < 0 {
		p.cursor = 0
	}
}

// ---- commands -------------------------------------------------------------

// fetchPRCmd reads one PR's detail (checks/reviews/merge), tagged with the request-generation id so
// a stale reply is dropped (PRD #1255 D4; the #1130 poll-guard pattern).
func (m tuiModel) fetchPRCmd(repoID string, iid int64, reqID uint64) tea.Cmd {
	c, parent := m.client, m.ctx
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, boardPollTimeout)
		defer cancel()
		detail, err := c.GetPull(ctx, repoID, iid)
		return prMsg{detail: detail, err: err, reqID: reqID}
	}
}

// startPRReq mints the next PR request id, records it as the one the model is waiting on, and
// returns the tagged fetch — the PR twin of startPullsReq. It returns nil when no PR is resolved.
func (m *tuiModel) startPRReq() tea.Cmd {
	if m.pr.repoID == "" {
		return nil
	}
	m.pr.reqSeq++
	m.pr.waitID = m.pr.reqSeq
	return m.fetchPRCmd(m.pr.repoID, m.pr.iid, m.pr.waitID)
}

// reworkCmd queues a rework of the PR's linked run (the w key, PRD #1255 D1/D12). Guidance is empty
// — the server enforces the completed / open-MR / reworkable preconditions and answers a typed 409
// reason, drawn inline (prActionNotice). RECONCILIATION (PRD #1255 M5): the PRD says w is "shown
// only when the PR has a completed linked run", but the list/detail DTO carries only RunID, not the
// run's status, so the caller gates on RunID != nil and lets the server 409 reason show rather than
// pre-gating on a status the DTO does not have.
func (m tuiModel) reworkCmd(runID string) tea.Cmd {
	c, parent := m.client, m.ctx
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, boardPollTimeout)
		defer cancel()
		run, err := c.RunRework(ctx, runID, "")
		return prActionMsg{kind: "rework", run: run, err: err}
	}
}

// fixCICmd queues a CI-fix run for the PR's head branch (the f key, PRD #1255 D12). The server
// refuses (409) unless the ref is a watched failed ref (R6); that reason is drawn inline.
func (m tuiModel) fixCICmd(repoID, ref string) tea.Cmd {
	c, parent := m.client, m.ctx
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, boardPollTimeout)
		defer cancel()
		run, err := c.CreateCIFixRun(ctx, repoID, ref)
		return prActionMsg{kind: "fixci", run: run, err: err}
	}
}

// prActionNotice renders a w/f action result as one transient status line (PRD #1255 D1): a success
// confirmation naming the queued run, or the server's reason on a typed 4xx/409 — sanitized through
// fmtErr so a rotted server message cannot drive the terminal.
func prActionNotice(msg prActionMsg) string {
	verb := "rework"
	if msg.kind == "fixci" {
		verb = "fix ci"
	}
	if msg.err != nil {
		return "✗ " + verb + ": " + fmtErr(msg.err)
	}
	return "↳ queued " + verb + " " + shortRunID(msg.run.ID)
}

// openLinkedRun opens the run-detail view for a PR's linked uzi run (the u ↳ run cross-link, D1),
// using the SAME drill-in sequence as the board (tui_board.go): newDetailState + a fresh session
// generation + the batched load/stream commands. `from` is recorded as m.detailReturn so the run
// view's esc returns to the screen the jump came from (viewPulls or viewPR). A nil/empty run id is
// a no-op (the key is hidden from the legend then).
func (m tuiModel) openLinkedRun(runID *string, from tuiView) (tea.Model, tea.Cmd) {
	if runID == nil || *runID == "" {
		return m, nil
	}
	m.view = viewDetail
	m.detail = newDetailState(*runID)
	m.detailGen++
	m.detail.gen = m.detailGen
	m.detailReturn = from
	return m, tea.Batch(m.loadRunCmd(*runID), m.loadTailCmd(*runID), m.openStreamCmd(*runID))
}

// ---- keys -----------------------------------------------------------------

func (m tuiModel) prKey(k string) (tea.Model, tea.Cmd) {
	// ↑↓/jk move the cursor over the sorted CHECKS list (the selected check's URL is drawn faint
	// beneath it).
	if d := motionDelta(k); d != 0 {
		m.pr.cursor += d
		m.pr.clampCursor()
		return m, nil
	}
	switch k {
	case keyEsc:
		// esc returns to where the drill-in was opened from (D1): the pulls list, or the run view on
		// the detail→m→PR path. The state returned to persists on the model (m.pulls / m.detail are
		// never clobbered), so it is still loaded. Reset to the default for the next open.
		m.view = m.prReturn
		m.prReturn = viewPulls
		return m, nil
	case keyRefresh:
		// A keypress is intent: never gated on the in-flight guard, mirroring the list screens' r.
		if m.pr.repoID != "" {
			return m, (&m).startPRReq()
		}
		return m, nil
	case keyRunLink:
		// u (↳ run): open the PR's linked uzi run; a nil RunID is a no-op (hidden from the legend).
		return m.openLinkedRun(m.pr.detail.RunID, viewPR)
	case keyRework:
		// w: rework the linked run. Shown only when RunID != nil; the server enforces the rest.
		if rid := m.pr.detail.RunID; rid != nil && *rid != "" {
			return m, m.reworkCmd(*rid)
		}
		return m, nil
	case keyFixCI:
		// f: fix ci for the PR's head branch. The server 409s an unwatched/green ref (R6); drawn inline.
		if m.pr.repoID != "" {
			return m, m.fixCICmd(m.pr.repoID, m.pr.detail.SourceBranch)
		}
		return m, nil
	}
	return m, nil
}

// ---- check projection + classification (D7/D8) ----------------------------

// prCheckText is a CheckDTO's forge-authored text projected into DISTINCTLY-NAMED fields so the D7
// AST guard can require each to be drawn through a sanitizer. CheckDTO.Name / .Description share
// bare selector names with unrelated pre-existing draws in other tui_*.go files (e.g. the tool
// payload .Name in tui_detail_transcript.go, drawn raw), so those bare names cannot go in
// d7UntrustedFields. The distinct names (checkName, checkDesc) are added there instead, and the copy
// below is plumbing — a struct-field copy, not a draw — so it is not a guard hit.
type prCheckText struct {
	checkName string // CheckDTO.Name
	checkDesc string // CheckDTO.Description
}

func prCheckTextOf(ck apitypes.CheckDTO) prCheckText {
	return prCheckText{checkName: ck.Name, checkDesc: ck.Description}
}

// prReviewText projects PullReviewDTO's untrusted text into distinct fields (reviewerLogin from
// .Author, reviewState from .State), for the same D7 reason: .State collides with an unrelated
// pre-existing draw (pendingJudge.State, tui_review.go), so it cannot be a bare guard entry.
type prReviewText struct {
	reviewerLogin string // PullReviewDTO.Author
	reviewState   string // PullReviewDTO.State
}

func prReviewTextOf(rv apitypes.PullReviewDTO) prReviewText {
	return prReviewText{reviewerLogin: rv.Author, reviewState: rv.State}
}

// prMergeText projects MergeStateDTO's untrusted text into distinct fields (mergeBlocked from
// .BlockedReason, mergeableState from .MergeableState), the same D7 pattern — the bare names are
// generic and kept out of the guard list.
type prMergeText struct {
	mergeBlocked   string // MergeStateDTO.BlockedReason
	mergeableState string // MergeStateDTO.MergeableState
}

func prMergeTextOf(ms apitypes.MergeStateDTO) prMergeText {
	return prMergeText{mergeBlocked: ms.BlockedReason, mergeableState: ms.MergeableState}
}

// prCheckEffective is the raw forge status to classify/colour a check by: the Conclusion once the
// forge split it out (completed checks), else the Status (a check still in flight has no conclusion).
func prCheckEffective(ck apitypes.CheckDTO) string {
	if ck.Conclusion != "" {
		return ck.Conclusion
	}
	return ck.Status
}

const (
	// Check sort/count buckets: failing → pending → passed → skipped (D3). pending folds the
	// "running" and "attention" tones (a check waiting on a human is still not settled); skipped is
	// the neutral tone (skipped/cancelled/neutral/unknown).
	prCkFailing = iota
	prCkPending
	prCkPassed
	prCkSkipped
)

func prCheckCategory(ck apitypes.CheckDTO) int {
	switch pipelinestatus.Tone(prCheckEffective(ck)) {
	case "failed":
		return prCkFailing
	case "running", "attention":
		return prCkPending
	case "passed":
		return prCkPassed
	default:
		return prCkSkipped
	}
}

// prSortedChecks returns the checks ordered failing → pending → passed → skipped (D3), stable
// within each bucket so the forge's own order is preserved there. A copy, so the model's slice is
// never reordered under it.
func prSortedChecks(checks []apitypes.CheckDTO) []apitypes.CheckDTO {
	out := make([]apitypes.CheckDTO, len(checks))
	copy(out, checks)
	sort.SliceStable(out, func(i, j int) bool {
		return prCheckCategory(out[i]) < prCheckCategory(out[j])
	})
	return out
}

func (m tuiModel) prCheckCounts() (failing, pending, passed, skipped int) {
	for _, ck := range m.pr.detail.Checks {
		switch prCheckCategory(ck) {
		case prCkFailing:
			failing++
		case prCkPending:
			pending++
		case prCkPassed:
			passed++
		default:
			skipped++
		}
	}
	return
}

func (m tuiModel) prSelectedCheck() (apitypes.CheckDTO, bool) {
	sorted := prSortedChecks(m.pr.detail.Checks)
	if m.pr.cursor < 0 || m.pr.cursor >= len(sorted) {
		return apitypes.CheckDTO{}, false
	}
	return sorted[m.pr.cursor], true
}

// checkGlyph is a check's spine glyph + colour by Tone of its effective status (D3/D8): ✗ failed, ●
// running, ✓ passed, ⚠ attention, · neutral/skipped. Every state has a distinct glyph so colour is
// never the only carrier.
func (m tuiModel) checkGlyph(ck apitypes.CheckDTO) (string, color.Color) {
	switch pipelinestatus.Tone(prCheckEffective(ck)) {
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

// reviewGlyph is a reviewer's glyph + colour by their raw review state (D8).
func (m tuiModel) reviewGlyph(state string) (string, color.Color) {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "approved":
		return "✓", m.pal.sage
	case "changes_requested":
		return "✎", m.pal.amber
	default: // commented / dismissed / pending / unknown
		return "·", m.pal.faintC
	}
}

// reviewStateWord maps a raw per-reviewer state to a human word (mirroring the CLI's reviewLabel
// vocabulary). An unrecognised value passes through verbatim and is sanitised at the draw site.
func reviewStateWord(state string) string {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "approved":
		return "approved"
	case "changes_requested":
		return "changes requested"
	case "commented":
		return "commented"
	case "dismissed":
		return "dismissed"
	case "pending":
		return "review requested"
	default:
		return state
	}
}

// ---- render ---------------------------------------------------------------

// Fixed column widths for a CHECKS row. The description takes what remains; the elapsed cell is
// reserved on the right. Below the narrowest tier the final clampVisual is the backstop.
const (
	prCheckNameWidth = 28
	prCheckStateW    = 8 // forgeState word: running / queued / passed / failed / done
	prElapsedWidth   = 7 // checkElapsed (e.g. 3m00s)
	prCheckDescMin   = 6
)

func (m tuiModel) renderPR() string {
	var sb strings.Builder

	sb.WriteString(clampVisual(m.prHeaderLine1(), m.width) + "\n")
	sb.WriteString(clampVisual(m.prHeaderLine2(), m.width) + "\n")

	if note := m.prHeaderNote(); note != "" {
		sb.WriteString(clampVisual(note, m.width) + "\n")
	}

	if !m.pr.loaded {
		sb.WriteString("\n" + m.pal.faint.Render(" loading…"))
		return sb.String()
	}
	sb.WriteString("\n")

	d := m.pr.detail

	// CHECKS: a summarizing heading, then one row per check (sorted failing → pending → passed →
	// skipped), windowed to the space that fits between the chrome and the REVIEWS/MERGE sections so
	// a long check list never pushes them off the frame.
	sb.WriteString(clampVisual(m.prChecksHeading(), m.width) + "\n")
	sorted := prSortedChecks(d.Checks)
	if len(sorted) == 0 {
		sb.WriteString(m.pal.faint.Render("  · no checks reported") + "\n")
	} else {
		start, end := prChecksWindow(m.pr.cursor, len(sorted), m.prChecksCapacity())
		for i := start; i < end; i++ {
			ck := sorted[i]
			sel := i == m.pr.cursor
			sb.WriteString(clampVisual(m.prCheckRow(ck, sel), m.width) + "\n")
			if sel {
				if url := m.prCheckURLLine(ck); url != "" {
					sb.WriteString(clampVisual(url, m.width) + "\n")
				}
			}
		}
	}

	// REVIEWS: one line per reviewer, the backend already folded to the latest per reviewer (D6).
	sb.WriteString("\n" + m.pal.title.Render(" REVIEWS") + "\n")
	if len(d.Reviews) == 0 {
		sb.WriteString(m.pal.faint.Render("  · no reviews yet") + "\n")
	} else {
		for _, rv := range d.Reviews {
			sb.WriteString(clampVisual(m.prReviewLine(rv), m.width) + "\n")
		}
	}

	// MERGE: conflicts / required-checks / blocked-reason — informational only, uzi never merges.
	sb.WriteString("\n" + m.pal.title.Render(" MERGE") + "\n")
	for _, line := range m.prMergeLines(d) {
		sb.WriteString(clampVisual(line, m.width) + "\n")
	}

	sb.WriteString(clampVisual(m.prFooter(), m.width))
	return sb.String()
}

// prChecksCapacity is how many CHECKS rows fit between the header chrome and the REVIEWS/MERGE
// sections at the current height, so the drill-in never overflows (the windowing twin of the list
// screens' capacity calc).
func (m tuiModel) prChecksCapacity() int {
	used := 2 // the two header lines
	if m.prHeaderNote() != "" {
		used++
	}
	used += 2 // the blank line + the CHECKS heading
	if ck, ok := m.prSelectedCheck(); ok && ck.WebURL != "" {
		used++ // the selected check's ↗ url line
	}
	used += 2 // the blank line + the REVIEWS heading
	if n := len(m.pr.detail.Reviews); n > 0 {
		used += n
	} else {
		used++ // the "no reviews yet" line
	}
	used += 2 // the blank line + the MERGE heading
	used += len(m.prMergeLines(m.pr.detail))
	used++ // the footer
	c := m.height - used
	if c < 1 {
		c = 1
	}
	return c
}

// prChecksWindow keeps the cursor visible within capacity rows (cursor-centred), so ↑↓ over a long
// check list always shows the selected check and its url line.
func prChecksWindow(cursor, total, capacity int) (start, end int) {
	if capacity < 1 {
		capacity = 1
	}
	if total <= capacity {
		return 0, total
	}
	start = cursor - capacity/2
	if start < 0 {
		start = 0
	}
	if start+capacity > total {
		start = total - capacity
	}
	return start, start + capacity
}

// prHeaderLine1 is the breadcrumb + `PR #<iid> · <head> → <base>` with a right-aligned checks+review
// rollup and the `● live · 5s` cadence tag.
func (m tuiModel) prHeaderLine1() string {
	d := m.pr.detail
	back := "pulls"
	if m.prReturn == viewDetail {
		back = "run"
	}
	left := m.pal.faint.Render("‹ "+back+"  ") +
		m.pal.title.Render("PR #"+itoa(int(m.pr.iid))) +
		m.pal.faint.Render(" · ") +
		m.pal.faint.Render(m.renderer.Plain(d.SourceBranch, 28)) +
		m.pal.faint.Render(" → ") +
		m.pal.faint.Render(m.renderer.Plain(d.TargetBranch, 20))
	right := m.prRollup() +
		m.pal.faint.Render("   ") +
		paintSeg(m.pal.wait, nil, false, "● live") +
		m.pal.faint.Render(" · "+shortDuration(prPollInterval))
	target := m.width - visualWidth(right) - 1
	if target < 1 {
		return left
	}
	return padVisual(clampVisual(left, target), target) + " " + right
}

// prHeaderLine2 is the title (untrusted, clipped) · author · run <id> (only when linked) · age ·
// +adds −dels.
func (m tuiModel) prHeaderLine2() string {
	d := m.pr.detail
	bold := lipgloss.NewStyle().Bold(true)
	seg := m.pal.faint.Render(" ") + bold.Render(m.renderer.Plain(d.Title, 80))
	seg += m.pal.faint.Render(" · ") + paintSeg(m.pal.sage, nil, false, m.renderer.Plain(d.Author, 24))
	if d.RunID != nil && *d.RunID != "" {
		seg += m.pal.faint.Render(" · run " + shortRunID(*d.RunID))
	}
	seg += m.pal.faint.Render(" · " + relAge(d.CreatedAt))
	seg += m.pal.faint.Render(" · +" + itoa(d.Additions) + " −" + itoa(d.Deletions))
	return seg
}

// prRollup is the header's top-right checks+review rollup (D3): `✗ F failing`, or `● N pending · ✓
// P/T`, or `✓ T/T · ✎ changes requested`, or `✓ T/T`. Every variant carries words, so it survives
// a colour-stripped profile (D8).
func (m tuiModel) prRollup() string {
	failing, pending, passed, _ := m.prCheckCounts()
	total := len(m.pr.detail.Checks)
	switch {
	case failing > 0:
		return paintSeg(m.pal.alarm, nil, false, "✗ "+itoa(failing)+" failing")
	case pending > 0:
		return paintSeg(m.pal.wait, nil, false, "● "+itoa(pending)+" pending") +
			m.pal.faint.Render(" · ") +
			paintSeg(m.pal.sage, nil, false, "✓ "+itoa(passed)+"/"+itoa(total))
	case total == 0:
		return m.pal.faint.Render("· no checks")
	case m.pr.detail.ReviewDecision == "changes_requested":
		return paintSeg(m.pal.sage, nil, false, "✓ "+itoa(total)+"/"+itoa(total)) +
			m.pal.faint.Render(" · ") +
			paintSeg(m.pal.amber, nil, false, "✎ changes requested")
	default:
		return paintSeg(m.pal.sage, nil, false, "✓ "+itoa(total)+"/"+itoa(total))
	}
}

// prHeaderNote is the sub-header line: a transient w/f action notice (the user just acted, so it is
// the most relevant thing) takes priority over the forge rate-limit state (D4), which takes
// priority over a generic refresh error. forgeNotice is already sanitized where it is set.
func (m tuiModel) prHeaderNote() string {
	if m.forgeNotice != "" {
		return m.pal.faint.Render(" " + m.forgeNotice)
	}
	if m.pr.rateLimited {
		retry := "soon"
		if m.pr.retryAfter > 0 {
			retry = shortDuration(m.pr.retryAfter)
		}
		return m.pal.faint.Render(" ~ rate-limited · retry in " + retry)
	}
	if m.pr.err != nil {
		return m.pal.faint.Render(" could not refresh: " + fmtErr(m.pr.err))
	}
	return ""
}

// prChecksHeading summarizes the CHECKS section and carries the `re-polled Ns ago` cadence proof
// (D4). The aggregate words (in progress / passed / failing / successful / skipped) are the D8
// carrier for the whole section.
func (m tuiModel) prChecksHeading() string {
	failing, pending, passed, skipped := m.prCheckCounts()
	total := len(m.pr.detail.Checks)
	head := m.pal.title.Render(" CHECKS") + "  "
	var summary string
	switch {
	case total == 0:
		summary = m.pal.faint.Render("· no checks reported")
	case failing > 0:
		summary = paintSeg(m.pal.alarm, nil, false, "✗ "+itoa(failing)+" failing") +
			m.pal.faint.Render(" · "+itoa(passed)+" passed · "+itoa(pending)+" pending")
	case pending > 0:
		summary = paintSeg(m.pal.wait, nil, false, "● "+itoa(pending)+" in progress") +
			m.pal.faint.Render(" · "+itoa(passed)+" passed · "+itoa(failing)+" failing")
	default:
		summary = paintSeg(m.pal.sage, nil, false, "✓ all checks passed") +
			m.pal.faint.Render(" · "+itoa(total)+" successful · "+itoa(skipped)+" skipped")
	}
	repolled := ""
	if m.pr.loaded {
		repolled = m.pal.faint.Render(" · re-polled " + relAge(m.pr.lastPollAt) + " ago")
	}
	return head + summary + repolled
}

// prCheckRow renders one check: the ▌ spine + state glyph, the name, a state WORD (forgeState — the
// D8 word beside the glyph), the description (clipped to one cell line) and the elapsed. Name and
// description are forge-authored → drawn through renderer.Plain (D7).
func (m tuiModel) prCheckRow(ck apitypes.CheckDTO, sel bool) string {
	t := prCheckTextOf(ck)
	glyph, c := m.checkGlyph(ck)

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
		paintSeg(nameC, bg, false, padCell(m.renderer.Plain(t.checkName, prCheckNameWidth), prCheckNameWidth)) + gap +
		// State WORD beside the glyph (D8). forgeState can echo an unknown forge status verbatim, so
		// it is sanitized through renderer.Plain before drawing.
		paintSeg(c, bg, false, padCell(m.renderer.Plain(forgeState(ck.Status, ck.Conclusion), prCheckStateW), prCheckStateW)) + gap

	descW := m.width - (4 + prCheckNameWidth + 2 + prCheckStateW + 2) - (prElapsedWidth + 2)
	if descW < prCheckDescMin {
		descW = prCheckDescMin
	}
	row += paintSeg(m.pal.faintC, bg, false, clampVisual(m.renderer.Plain(t.checkDesc, descW), descW))

	// Elapsed flushed to a fixed right cell so the column stays stable down the list.
	row = padSeg(row, m.width-prElapsedWidth, bg)
	row += padSeg(paintSeg(m.pal.faintC, bg, false, clampVisual(checkElapsed(ck), prElapsedWidth)), prElapsedWidth, bg)
	return row
}

// prCheckURLLine is the selected check's URL, drawn faint beneath its row as `↗ <url>` so a user on
// a terminal without hyperlink support can copy it (D9). The url reaches the frame only through
// oscLink (the sanitizing sink that strips every control/format rune from the OSC-8 target), and an
// OSC-8 envelope is emitted ONLY when the url parses as https AND the terminal supports links
// (D7/D9); the visible `↗ url` text goes through renderer.Plain regardless. It takes the whole
// CheckDTO (like pullLink takes the PullDTO) so the WebURL access stays inside here.
func (m tuiModel) prCheckURLLine(ck apitypes.CheckDTO) string {
	if ck.WebURL == "" {
		return ""
	}
	text := m.pal.faint.Render("   ↗ " + m.renderer.Plain(ck.WebURL, m.width-6))
	if m.linksEnabled() && isHTTPSURL(ck.WebURL) {
		return oscLink(ck.WebURL, text)
	}
	return text
}

// prReviewLine is one reviewer's line: glyph · login · state word · age (D6/D8). Login and the
// state word are forge-authored → renderer.Plain.
func (m tuiModel) prReviewLine(rv apitypes.PullReviewDTO) string {
	t := prReviewTextOf(rv)
	glyph, c := m.reviewGlyph(t.reviewState)
	var b strings.Builder
	b.WriteString(paintSeg(c, nil, false, " "+glyph+" "))
	b.WriteString(paintSeg(m.pal.sage, nil, false, m.renderer.Plain(t.reviewerLogin, 24)))
	b.WriteString("  " + m.renderer.Plain(reviewStateWord(t.reviewState), 20))
	b.WriteString(m.pal.faint.Render("  " + relAge(rv.SubmittedAt)))
	return b.String()
}

// prMergeLines is the MERGE section from MergeStateDTO (D3), informational only: a conflicts line, a
// required-checks line, and the blocked reason (or, when none, the raw coarse mergeable state). The
// blocked reason and mergeable state are forge-authored → renderer.Plain.
func (m tuiModel) prMergeLines(d apitypes.PullDetailDTO) []string {
	ms := d.Merge
	t := prMergeTextOf(ms)
	target := m.renderer.Plain(d.TargetBranch, 24)
	var lines []string
	switch {
	case ms.Conflicts == nil:
		lines = append(lines, paintSeg(m.pal.faintC, nil, false, " · conflict state unknown"))
	case *ms.Conflicts:
		lines = append(lines, paintSeg(m.pal.amber, nil, false, " ⚠ conflicts with ")+m.pal.faint.Render(target))
	default:
		lines = append(lines, paintSeg(m.pal.sage, nil, false, " ✓ no conflicts with ")+m.pal.faint.Render(target))
	}
	if ms.RequiredChecksPassed {
		lines = append(lines, paintSeg(m.pal.sage, nil, false, " ✓ required checks passed"))
	} else {
		lines = append(lines, paintSeg(m.pal.wait, nil, false, " ● waiting on checks"))
	}
	if t.mergeBlocked != "" {
		lines = append(lines, paintSeg(m.pal.amber, nil, false, " ✎ blocked: ")+
			m.pal.faint.Render(m.renderer.Plain(t.mergeBlocked, m.width-14)))
	} else if t.mergeableState != "" {
		lines = append(lines, m.pal.faint.Render(" · "+m.renderer.Plain(t.mergeableState, m.width-6)))
	}
	return lines
}

// prFooter is the one-line key legend for the PR view (D13). u/w appear only when the PR has a
// linked run (RunID != nil); ↗ appears only when the selected check carries a URL.
func (m tuiModel) prFooter() string {
	parts := []string{m.keyHint("↑↓", "move")}
	if ck, ok := m.prSelectedCheck(); ok && ck.WebURL != "" {
		parts = append(parts, m.keyHint("↗", "open check"))
	}
	if rid := m.pr.detail.RunID; rid != nil && *rid != "" {
		parts = append(parts, m.keyHint("u", "run"), m.keyHint("w", "rework"))
	}
	parts = append(parts, m.keyHint("f", "fix ci"), m.keyHint("esc", "back"), m.keyHint("?", "keys"))
	return " " + strings.Join(parts, m.pal.faint.Render(" · "))
}
