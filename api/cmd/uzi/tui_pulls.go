package main

// tui_pulls.go — the forge `pulls` list screen (PRD #1255 M4a, D1/D2/D3/D7/D8/D13) and
// the shared navigation scaffold the later forge screens (ci list, PR/CI drill-ins) build
// on. It mirrors the board's banded layout and poll-guard tick chain, scoped to ONE repo at
// a time (D2). Every forge-authored string is drawn through m.renderer.Plain (D7); every
// band/state carries a glyph AND a word so colour is never the only cue (D8).

import (
	"context"
	"errors"
	"image/color"
	"net/url"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// ---- messages -------------------------------------------------------------

// pullsMsg carries a ListPulls reply for the `pulls` screen, the pulls analogue of
// boardRunsMsg. reqID is the request-generation id this reply belongs to; the model honours
// it only when reqID == m.pulls.waitID, so an older poll that resolves after a newer request
// was minted is dropped instead of clearing the newer request's guard.
type pullsMsg struct {
	pulls []apitypes.PullDTO
	err   error
	reqID uint64
}

// pullsTickMsg drives the `pulls` list's own 10s poll chain, carrying the tick-chain
// generation it was scheduled under (a tick whose gen != m.pulls.tickGen is from a superseded
// chain and is dropped), mirroring boardTickMsg.
type pullsTickMsg struct{ gen uint64 }

// reposMsg carries the viewer's repos (from ListRepos), fetched once at Init so the forge
// views can scope to one ENABLED repo (PRD #1255 D2).
type reposMsg struct {
	repos []apitypes.RepoDTO
	err   error
}

// ---- poll cadence ---------------------------------------------------------

// pullsPollInterval is the `pulls` list's poll cadence (PRD #1255 D4: a list polls every
// 10s, a drill-in every 5s). A var (not const) so a test can shrink it.
var pullsPollInterval = 10 * time.Second

// pullsBackoffCap caps the pulls error-backoff reschedule interval, the pulls twin of
// boardBackoffCap. A var so a test can shrink it.
var pullsBackoffCap = 60 * time.Second

// pullsTickAfter arms the pulls tick after delay d, stamping the produced pullsTickMsg with
// the tick-chain generation gen (the reply owns rescheduling at the backed-off cadence, and
// gen lets the model drop a tick from a superseded chain) — the pulls twin of tickAfter.
func pullsTickAfter(d time.Duration, gen uint64) tea.Cmd {
	return tea.Tick(d, func(time.Time) tea.Msg { return pullsTickMsg{gen: gen} })
}

// pullsTickInterval maps the consecutive pulls-poll error streak to the reschedule interval
// (base at streak 0, doubling per consecutive failure, clamped at pullsBackoffCap), the pulls
// twin of boardTickInterval — a pure function so the backoff is unit-assertable.
func pullsTickInterval(streak int) time.Duration {
	if streak <= 0 {
		return pullsPollInterval
	}
	if streak > 16 {
		streak = 16
	}
	d := pullsPollInterval << uint(streak)
	if d <= 0 || d > pullsBackoffCap {
		return pullsBackoffCap
	}
	return d
}

// ---- state ----------------------------------------------------------------

type pullsState struct {
	pulls  []apitypes.PullDTO
	cursor int
	scroll int
	err    error

	// reqSeq / waitID / tickGen are the request-generation guard, a copy of the board's (see
	// boardState): waitID == 0 is idle and a tick polls ONLY while idle (the in-flight guard);
	// a pullsMsg is honoured only when reqID == waitID (the out-of-order defence); tickGen is
	// the tick-chain generation (a mismatched tick is from a superseded chain).
	reqSeq    uint64
	waitID    uint64
	tickGen   uint64
	errStreak int

	filtering bool
	filter    string

	// loaded is true once a SUCCESSFUL reply has landed, so the view can tell "still loading
	// the first page" from "loaded, and there are no open PRs".
	loaded bool

	// rateLimited / retryAfter render the header's `~ rate-limited · retry in Ns` state (D4):
	// a forge 429 maps to ExitUnreachable carrying a Retry-After, which backs off the poll and
	// shows the strip instead of an error line.
	rateLimited bool
	retryAfter  time.Duration
}

func newPullsState() pullsState {
	// The tick chain starts at generation 1 (the Init-armed pulls tick is honoured); reqSeq /
	// waitID start at 0 (idle) because, unlike the board, NO request is in flight at Init — the
	// repo the `pulls` route needs is not known until ListRepos returns.
	return pullsState{tickGen: 1}
}

func (p *pullsState) apply(msg pullsMsg) {
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
	p.pulls = msg.pulls
	p.loaded = true
	p.clampCursor()
}

// resetForRepoChange drops the cached rows, cursor, scroll and loaded flag so the screen reads
// "loading…" under the new repo rather than flashing the prior repo's PRs. It is called on BOTH
// list states when the SHARED repoIdx cycles (R), since the pulls and ci screens scope to the
// same repo and both caches go stale the instant it changes.
func (p *pullsState) resetForRepoChange() {
	p.cursor, p.scroll = 0, 0
	p.pulls, p.loaded = nil, false
}

func (p *pullsState) clampCursor() {
	n := len(p.visible())
	if p.cursor >= n {
		p.cursor = n - 1
	}
	if p.cursor < 0 {
		p.cursor = 0
	}
}

func (p *pullsState) selected() (apitypes.PullDTO, bool) {
	v := p.visible()
	if p.cursor < 0 || p.cursor >= len(v) {
		return apitypes.PullDTO{}, false
	}
	return v[p.cursor], true
}

// visible applies the `/` filter, then orders the survivors into the three bands (NEEDS YOU →
// IN FLIGHT → READY). The cursor indexes this list, so the selectable order IS the visual
// order. The filter matches sanitized cell text (title / branch / author) plus the review
// decision, so a control byte in a title cannot affect what matches.
func (p *pullsState) visible() []apitypes.PullDTO {
	base := p.pulls
	if strings.TrimSpace(p.filter) != "" {
		q := strings.ToLower(strings.TrimSpace(p.filter))
		out := make([]apitypes.PullDTO, 0, len(base))
		for _, pr := range base {
			hay := strings.ToLower(strings.Join([]string{
				"#" + itoa(int(pr.IID)),
				cellText(pr.Title), cellText(pr.SourceBranch), cellText(pr.Author),
				pr.ReviewDecision,
			}, " "))
			if strings.Contains(hay, q) {
				out = append(out, pr)
			}
		}
		base = out
	}
	return pullBandOrder(base)
}

// ---- bands (D3) -----------------------------------------------------------

const (
	// NEEDS YOU: changes requested, or a conflict. IN FLIGHT: a draft, or a review is pending.
	// READY: approved, or no review required, with no conflict. A failing-check signal is NOT
	// available on the list DTO (D4: the list route carries no per-check array — that becomes
	// precise only in the PR drill-in, M5), so NEEDS YOU here is review + conflict only.
	pullBandNeedsYou = iota
	pullBandInFlight
	pullBandReady
	numPullBands
)

var pullBandNames = [numPullBands]string{"NEEDS YOU", "IN FLIGHT", "READY"}

func pullBand(pr apitypes.PullDTO) int {
	if pr.ReviewDecision == "changes_requested" || (pr.Conflicts != nil && *pr.Conflicts) {
		return pullBandNeedsYou
	}
	if pr.Draft || pr.ReviewDecision == "review_required" {
		return pullBandInFlight
	}
	return pullBandReady
}

// pullBandOrder partitions pulls into the three bands and, inside each band, sorts by newest
// activity (UpdatedAt) first (D3), then concatenates NEEDS YOU → IN FLIGHT → READY.
func pullBandOrder(pulls []apitypes.PullDTO) []apitypes.PullDTO {
	var buckets [numPullBands][]apitypes.PullDTO
	for _, pr := range pulls {
		buckets[pullBand(pr)] = append(buckets[pullBand(pr)], pr)
	}
	out := make([]apitypes.PullDTO, 0, len(pulls))
	for b := 0; b < numPullBands; b++ {
		sort.SliceStable(buckets[b], func(i, j int) bool {
			return buckets[b][i].UpdatedAt.After(buckets[b][j].UpdatedAt)
		})
		out = append(out, buckets[b]...)
	}
	return out
}

// ---- repo scope (D2) ------------------------------------------------------

// enabledRepos keeps only the repos the user has enabled — the scope set the forge views
// cycle with R.
func enabledRepos(repos []apitypes.RepoDTO) []apitypes.RepoDTO {
	out := make([]apitypes.RepoDTO, 0, len(repos))
	for _, r := range repos {
		if r.Enabled {
			out = append(out, r)
		}
	}
	return out
}

func (m tuiModel) currentRepo() (apitypes.RepoDTO, bool) {
	if m.repoIdx >= 0 && m.repoIdx < len(m.repos) {
		return m.repos[m.repoIdx], true
	}
	return apitypes.RepoDTO{}, false
}

// pullsRepoReady reports whether a concrete repo has been resolved for the pulls route — the
// precondition for issuing a fetch or rendering rows.
func (m tuiModel) pullsRepoReady() bool {
	return m.repoChosen && m.repoIdx >= 0 && m.repoIdx < len(m.repos)
}

// reposReady reports whether the shared forge repo scope has resolved: a successful ListRepos
// has landed with no error. It is false while still loading the first page AND after a failure
// (reposErr set), which is exactly the "re-issue fetchReposCmd" condition the pulls-tick / r
// self-heal keys off (PRD #1255). An empty-but-loaded set (no enabled repos) is READY — a
// resolved state, not a failure to retry.
func (m tuiModel) reposReady() bool {
	return m.reposLoaded && m.reposErr == nil
}

// defaultRepoIdx implements D2's default: the repo of the newest non-chat board run when that
// repo is in the enabled set (a chat run has a nil RepoID; the admin board lists other users'
// runs, whose repos are not enabled for this viewer), else the first enabled repo.
func (m tuiModel) defaultRepoIdx() int {
	best := -1
	var bestAt time.Time
	for _, r := range m.board.runs {
		if r.RepoID == nil || *r.RepoID == "" {
			continue // a chat run has no repo
		}
		idx := -1
		for i, repo := range m.repos {
			if repo.ID == *r.RepoID {
				idx = i
				break
			}
		}
		if idx < 0 {
			continue // the run's repo is not in this viewer's enabled set
		}
		if best < 0 || r.CreatedAt.After(bestAt) {
			best, bestAt = idx, r.CreatedAt
		}
	}
	if best < 0 {
		return 0 // no board run names an enabled repo: the first enabled repo
	}
	return best
}

// resolveDefaultRepo applies the default-repo rule once (D2): the first time the pulls screen
// is entered with repos loaded, or when a repos reply lands while the screen is open. A user
// who has cycled with R has repoChosen set, so the default never overrides their choice.
func (m *tuiModel) resolveDefaultRepo() {
	if m.repoChosen || len(m.repos) == 0 {
		return
	}
	m.repoIdx = m.defaultRepoIdx()
	m.repoChosen = true
}

// ---- commands -------------------------------------------------------------

// fetchReposCmd reads the viewer's repos so the forge views can scope to one enabled repo.
func (m tuiModel) fetchReposCmd() tea.Cmd {
	c, parent := m.client, m.ctx
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, boardPollTimeout)
		defer cancel()
		repos, err := c.ListRepos(ctx)
		return reposMsg{repos: repos, err: err}
	}
}

// fetchPullsCmd reads one repo's open pulls, tagged with the request-generation id so a
// stale reply is dropped (PRD #1255 D4; the #1130 poll-guard pattern).
func (m tuiModel) fetchPullsCmd(repoID string, reqID uint64) tea.Cmd {
	c, parent := m.client, m.ctx
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, boardPollTimeout)
		defer cancel()
		pulls, err := c.ListPulls(ctx, repoID)
		return pullsMsg{pulls: pulls, err: err, reqID: reqID}
	}
}

// startPullsReq mints the next pulls request id, records it as the one the model is waiting
// on, and returns the tagged fetch — the pulls twin of startBoardReq. It returns nil when no
// repo is resolved (nothing to fetch).
func (m *tuiModel) startPullsReq() tea.Cmd {
	repo, ok := m.currentRepo()
	if !ok {
		return nil
	}
	m.pulls.reqSeq++
	m.pulls.waitID = m.pulls.reqSeq
	return m.fetchPullsCmd(repo.ID, m.pulls.waitID)
}

// ---- keys -----------------------------------------------------------------

// gotoPulls leaves the current screen for the `pulls` list (PRD #1255 D1): it resolves the
// default repo (once) and, when a repo is ready and no poll is in flight, issues an immediate
// fetch so the screen is not stuck on "loading…" until the next tick.
func (m tuiModel) gotoPulls() (tea.Model, tea.Cmd) {
	m.view = viewPulls
	(&m).resolveDefaultRepo()
	if m.pulls.waitID == 0 && m.pullsRepoReady() {
		return m, (&m).startPullsReq()
	}
	return m, nil
}

func (m tuiModel) pullsKey(k string) (tea.Model, tea.Cmd) {
	// Filter input mode swallows ordinary keys so typing "q" / "R" filters rather than
	// quitting / cycling the repo (D13).
	if m.pulls.filtering {
		switch k {
		case keyEnter, keyEsc:
			m.pulls.filtering = false
			if k == keyEsc {
				m.pulls.filter = ""
			}
			m.pulls.clampCursor()
		case "backspace":
			if n := len([]rune(m.pulls.filter)); n > 0 {
				m.pulls.filter = string([]rune(m.pulls.filter)[:n-1])
			}
			m.pulls.clampCursor()
		default:
			if k == keySpaceName {
				k = " "
			}
			if len([]rune(k)) == 1 {
				m.pulls.filter += k
				m.pulls.clampCursor()
			}
		}
		m.pulls.scroll = m.pullsSyncedScroll()
		return m, nil
	}

	if d := motionDelta(k); d != 0 {
		m.pulls.cursor += d
		m.pulls.clampCursor()
		m.pulls.scroll = m.pullsSyncedScroll()
		return m, nil
	}

	switch k {
	case keyFilter:
		m.pulls.filtering = true
		return m, nil
	case keyRefresh:
		// User-initiated: never gated on the in-flight guard (a keypress is intent), mirroring
		// the board's r. startPullsReq mints a fresh id so the next tick does not stack on it.
		if m.pullsRepoReady() {
			return m, (&m).startPullsReq()
		}
		// No repo resolved yet: when the shared repo scope is not ready (still loading, or a
		// transient ListRepos failure), r retries fetchReposCmd — the precondition for any pulls
		// fetch — so the user can recover the screen without restarting the TUI (PRD #1255). The
		// reposInFlight guard keeps it from stacking a second outstanding repos fetch.
		if !m.reposReady() && !m.reposInFlight {
			m.reposInFlight = true
			return m, m.fetchReposCmd()
		}
		return m, nil
	case keyRepoCycle:
		// R cycles the enabled repos (D2); hidden from the legend when only one is enabled, so
		// this is a no-op then. The choice persists for the session (repoChosen) and is SHARED
		// with the ci screen — so BOTH caches go stale the instant repoIdx changes. Clear them
		// both (the ci screen would otherwise render repo A's runs under repo B's header on its
		// next visit, until its own poll lands), then refetch only the CURRENT (pulls) screen.
		if len(m.repos) > 1 {
			m.repoIdx = (m.repoIdx + 1) % len(m.repos)
			m.repoChosen = true
			m.pulls.resetForRepoChange()
			m.ci.resetForRepoChange()
			return m, (&m).startPullsReq()
		}
		return m, nil
	case keyTab:
		// tab cycles floor → pulls → ci → floor, so from the pulls list it advances to ci.
		return m.gotoCI()
	case keyViewFloor:
		// 1 jumps to the floor directly.
		m.view = viewBoard
		return m, nil
	case keyViewPulls:
		return m, nil // already here
	case keyViewCI:
		// 3 jumps to the ci list directly.
		return m.gotoCI()
	case keyEsc:
		// esc on a list returns to the floor (D1).
		m.view = viewBoard
		return m, nil
	}
	return m, nil
}

// ---- render ---------------------------------------------------------------

// Fixed column widths for a pulls row. The title takes what remains; narrow terminals drop
// the branch column (then the run-link cell), right-to-left before the title, like the board.
const (
	pullIIDWidth       = 7  // #<iid>
	pullChecksWidth    = 4  // neutral `·  —` placeholder (the list route carries no checks — see pullRow)
	pullReviewWidth    = 10 // `✎ changes` / `✓ approved`
	pullAgeWidth       = 4  // relAge(UpdatedAt)
	pullBranchWidth    = 20 // source branch
	pullTitleMax       = 60 // title cap, so a long title does not run a wide terminal
	pullRunLinkWidth   = 11 // `↳ ` + 8-char short run id + slack
	pullBranchMinWidth = 96 // below this the branch column drops so the title is never squeezed
)

func pullRowPrefixWidth(branch bool) int {
	// cursor(1)+spine(1)+glyph(1)+space(1), then two-space gaps around the iid/checks/review/age
	// cells, plus the branch cell when shown.
	w := 4 + pullIIDWidth + 2 + pullChecksWidth + 2 + pullReviewWidth + 2 + pullAgeWidth + 2
	if branch {
		w += pullBranchWidth + 2
	}
	return w
}

func (m tuiModel) renderPulls() string {
	var sb strings.Builder
	rows := m.pulls.visible()

	brand := m.tabStrip()
	if m.pulls.filter != "" || m.pulls.filtering {
		brand += m.pal.faint.Render("   /" + cellText(m.pulls.filter))
		if m.pulls.filtering {
			brand += m.pal.title.Render("▌")
		}
	}

	items := buildPullItems(rows)
	capacity := m.pullsCapacity()
	selItem := selectedBoardItem(items, m.pulls.cursor)
	start, end := boardWindow(selItem, m.pulls.scroll, len(items), capacity)

	summary := m.pullsSummary()
	if len(rows) > 0 {
		lo, hi := windowRunSpan(items, start, end)
		summary += m.pal.faint.Render(" · " + itoa(lo) + "–" + itoa(hi))
	}
	sb.WriteString(clampVisual(padVisual(" "+brand, m.width-visualWidth(summary)-1)+summary, m.width) + "\n")

	// Sub-header: the forge rate-limit / error state (D4), else nothing.
	if note := m.pullsHeaderNote(); note != "" {
		sb.WriteString(clampVisual(note, m.width) + "\n")
	}
	sb.WriteString("\n")

	switch {
	case !m.pullsRepoReady():
		sb.WriteString(m.pullsScopeState())
	case len(rows) == 0:
		sb.WriteString(m.pullsEmptyState())
	default:
		runLinkW := 0
		for _, pr := range rows {
			if pr.RunID != nil && *pr.RunID != "" {
				runLinkW = pullRunLinkWidth
				break
			}
		}
		for i := start; i < end; i++ {
			switch it := items[i]; it.kind {
			case biEyebrow:
				sb.WriteString(m.pullEyebrow(it) + "\n")
			case biRow:
				pr := rows[it.runIdx]
				sel := it.runIdx == m.pulls.cursor
				sb.WriteString(m.pullRow(pr, sel, runLinkW) + "\n")
				if sel {
					if sl := m.pullSecondLine(pr); sl != "" {
						sb.WriteString(sl + "\n")
					}
				}
			default:
				sb.WriteString("\n")
			}
		}
	}

	sb.WriteString(clampVisual(m.pullsFooter(), m.width))
	return sb.String()
}

// buildPullItems turns the band-ordered visible list into the flat display list: an eyebrow
// at each band boundary, a spacer between bands, one row per pull — the pulls twin of
// buildBoardItems, reusing boardItem so the shared windowing helpers apply.
func buildPullItems(pulls []apitypes.PullDTO) []boardItem {
	var counts [numPullBands]int
	for _, pr := range pulls {
		counts[pullBand(pr)]++
	}
	items := make([]boardItem, 0, len(pulls)+numPullBands*2)
	prevBand := -1
	for i, pr := range pulls {
		if b := pullBand(pr); b != prevBand {
			if prevBand != -1 {
				items = append(items, boardItem{kind: biSpacer})
			}
			items = append(items, boardItem{kind: biEyebrow, band: b, count: counts[b]})
			prevBand = b
		}
		items = append(items, boardItem{kind: biRow, runIdx: i})
	}
	return items
}

// pullsCapacity is how many display lines fit between the header block and the footer, the
// pulls twin of boardCapacity: tab strip + blank + footer (3), plus the optional sub-header
// note, plus the selected row's reserved second line.
func (m tuiModel) pullsCapacity() int {
	chrome := 3
	if m.pullsHeaderNote() != "" {
		chrome++
	}
	if _, ok := m.pulls.selected(); ok {
		chrome++
	}
	c := m.height - chrome
	if c < 1 {
		c = 1
	}
	return c
}

func (m tuiModel) pullsSyncedScroll() int {
	items := buildPullItems(m.pulls.visible())
	sel := selectedBoardItem(items, m.pulls.cursor)
	start, _ := boardWindow(sel, m.pulls.scroll, len(items), m.pullsCapacity())
	return start
}

// pullEyebrow is a faint CAPS band label + count; NEEDS YOU is amber (the one band a human
// must act on), the pulls twin of boardEyebrow.
func (m tuiModel) pullEyebrow(it boardItem) string {
	name := pullBandNames[it.band]
	if it.band == pullBandNeedsYou {
		return " " + lipgloss.NewStyle().Foreground(m.pal.amber).Bold(true).Render(name) + m.pal.faint.Render(" · "+itoa(it.count))
	}
	return " " + m.pal.faint.Render(name+" · "+itoa(it.count))
}

// pullRow renders one pull: the ▌ andon spine + state glyph, the #<iid> (an OSC-8 link to the
// PR's web URL, https only — D7/D9), a neutral checks placeholder, the review cell, the age,
// the source branch, the title, and a `↳ <run>` link when a uzi run opened it.
func (m tuiModel) pullRow(pr apitypes.PullDTO, sel bool, runLinkW int) string {
	band := pullBand(pr)
	glyph, glyphC := m.pullGlyph(pr)

	var bg color.Color
	if sel {
		bg = m.pal.selBg
	}
	idC := m.pal.faintC
	if sel {
		idC = m.pal.tungsten
	}

	cursor := paintSeg(nil, bg, false, " ")
	if sel {
		cursor = paintSeg(m.pal.tungsten, bg, true, "▸")
	}
	gap := paintSeg(nil, bg, false, "  ")

	styledIID := paintSeg(idC, bg, sel, padCell("#"+itoa(int(pr.IID)), pullIIDWidth))
	row := cursor +
		paintSeg(glyphC, bg, false, "▌") +
		paintSeg(glyphC, bg, false, glyph) +
		paintSeg(nil, bg, false, " ") +
		m.pullLink(pr, styledIID) + gap

	// Checks cell: the `pulls` LIST route carries NO per-check array — that is the PR drill-in's
	// job (D4, M5) — so this is a NEUTRAL placeholder, never a fabricated passed/total count. It
	// mirrors the CLI's renderPullList, which likewise shows no checks column for a list row.
	row += paintSeg(m.pal.faintC, bg, false, padCell("·  —", pullChecksWidth)) + gap

	rg, rw, rc := pullReviewCell(m.pal, pr)
	row += paintSeg(rc, bg, false, padCell(rg+" "+rw, pullReviewWidth)) + gap

	// Age = latest activity (UpdatedAt), so the freshest PR reads first within its band.
	row += paintSeg(m.pal.faintC, bg, false, padCell(relAge(pr.UpdatedAt), pullAgeWidth)) + gap

	showBranch := m.width >= pullBranchMinWidth
	if showBranch {
		row += paintSeg(m.pal.faintC, bg, false, padCell(m.renderer.Plain(pr.SourceBranch, pullBranchWidth), pullBranchWidth)) + gap
	}

	avail := m.width - pullRowPrefixWidth(showBranch)
	if runLinkW > 0 {
		avail -= runLinkW + 2
	}
	if avail < 10 {
		avail = 10
	}
	if avail > pullTitleMax {
		avail = pullTitleMax
	}
	row += paintSeg(m.pullTitleColor(band, sel), bg, false, clampVisual(m.renderer.Plain(pr.Title, avail), avail))

	// `↳ <short run id>` flushed right, aligned in a fixed cell so the title width stays stable
	// down the list. Faint, tungsten on the selected row. The run id is a uzi UUID (not forge
	// text), shown git-short.
	if runLinkW > 0 {
		row = padSeg(row, m.width-runLinkW, bg)
		linkC := m.pal.faintC
		if sel {
			linkC = m.pal.tungsten
		}
		link := ""
		if pr.RunID != nil && *pr.RunID != "" {
			link = "↳ " + shortRunID(*pr.RunID)
		}
		row += paintSeg(linkC, bg, false, padCell(link, runLinkW))
	} else if sel {
		row = padSeg(row, m.width, bg)
	}
	return row
}

// pullGlyph is the row's spine glyph + colour, by priority: changes requested (✎), conflict
// (⚠), draft (·), review pending (●), ready (✓). Every state has a distinct glyph so colour
// is never the only carrier (D8).
func (m tuiModel) pullGlyph(pr apitypes.PullDTO) (string, color.Color) {
	switch {
	case pr.ReviewDecision == "changes_requested":
		return "✎", m.pal.amber
	case pr.Conflicts != nil && *pr.Conflicts:
		return "⚠", m.pal.amber
	case pr.Draft:
		return "·", m.pal.faintC
	case pr.ReviewDecision == "review_required":
		return "●", m.pal.wait
	default: // approved or none (READY)
		return "✓", m.pal.sage
	}
}

// pullReviewCell is the review-decision cell (glyph + word + colour), mirroring the CLI's
// reviewLabel vocabulary. A draft with no concrete decision reads `· draft`; a concrete
// decision (changes / approved) reads even on a draft.
func pullReviewCell(pal palette, pr apitypes.PullDTO) (glyph, word string, c color.Color) {
	switch pr.ReviewDecision {
	case "changes_requested":
		return "✎", "changes", pal.amber
	case "approved":
		return "✓", "approved", pal.sage
	case "review_required":
		if pr.Draft {
			return "·", "draft", pal.faintC
		}
		return "·", "review", pal.faintC
	default: // none / ""
		if pr.Draft {
			return "·", "draft", pal.faintC
		}
		return "·", "—", pal.faintC
	}
}

// pullTitleColor follows the board's "one lit thing" rule: NEEDS YOU titles are amber, the
// rest are quiet default ink (tungsten when the row is selected so it survives the warm bar).
func (m tuiModel) pullTitleColor(band int, sel bool) color.Color {
	if band == pullBandNeedsYou {
		return m.pal.amber
	}
	if sel {
		return m.pal.tungsten
	}
	return nil
}

// pullSecondLine is the selected row's faint detail line (D3): the latest-activity age and,
// when the forge reported mergeability, the conflicts note against the target branch.
func (m tuiModel) pullSecondLine(pr apitypes.PullDTO) string {
	bg := m.pal.selBg
	var b strings.Builder
	b.WriteString(paintSeg(m.pal.tungsten, bg, false, "  ▸ "))
	b.WriteString(paintSeg(m.pal.faintC, bg, false, "updated "+relAge(pr.UpdatedAt)))
	if pr.Conflicts != nil {
		note := "no conflicts with "
		if *pr.Conflicts {
			note = "conflicts with "
		}
		b.WriteString(paintSeg(m.pal.faintC, bg, false, " · "+note))
		b.WriteString(paintSeg(m.pal.faintC, bg, false, m.renderer.Plain(pr.TargetBranch, 24)))
	}
	return padSeg(clampVisual(b.String(), m.width), m.width, bg)
}

// pullsSummary is the header's top-right glyph cluster: ✎ N (needs-you) · ● N (in flight) · ✓
// N (ready) · N open. Computed over ALL pulls so it does not shrink under a filter; zero-count
// segments drop.
func (m tuiModel) pullsSummary() string {
	changes, pending, ready := 0, 0, 0
	for _, pr := range m.pulls.pulls {
		switch pullBand(pr) {
		case pullBandNeedsYou:
			changes++
		case pullBandInFlight:
			pending++
		case pullBandReady:
			ready++
		}
	}
	var segs []string
	if changes > 0 {
		segs = append(segs, paintSeg(m.pal.amber, nil, false, "✎ "+itoa(changes)))
	}
	if pending > 0 {
		segs = append(segs, paintSeg(m.pal.wait, nil, false, "● "+itoa(pending)))
	}
	if ready > 0 {
		segs = append(segs, paintSeg(m.pal.sage, nil, false, "✓ "+itoa(ready)))
	}
	segs = append(segs, m.pal.faint.Render(itoa(len(m.pulls.pulls))+" open"))
	return strings.Join(segs, m.pal.faint.Render(" · "))
}

// pullsHeaderNote is the sub-header line: the forge rate-limit state (D4) takes priority over
// a generic refresh error, so a 429 reads as a backoff rather than a failure.
func (m tuiModel) pullsHeaderNote() string {
	if m.pulls.rateLimited {
		retry := "soon"
		if m.pulls.retryAfter > 0 {
			retry = shortDuration(m.pulls.retryAfter)
		}
		return m.pal.faint.Render(" ~ rate-limited · retry in " + retry)
	}
	if m.pulls.err != nil {
		return m.pal.faint.Render(" could not refresh: " + fmtErr(m.pulls.err))
	}
	return ""
}

// pullsScopeState explains why there are no rows to show yet, before any repo is resolved:
// a repos load error, repos not loaded, or no enabled repos.
func (m tuiModel) pullsScopeState() string {
	switch {
	case m.reposErr != nil:
		return m.pal.faint.Render(" could not load repositories: "+fmtErr(m.reposErr)) + "\n"
	case !m.reposLoaded:
		return m.pal.faint.Render(" loading…") + "\n"
	case len(m.repos) == 0:
		return m.pal.faint.Render(" No enabled repositories. Enable one from the web board to see its pull requests.") + "\n"
	default:
		return m.pal.faint.Render(" loading…") + "\n"
	}
}

// pullsEmptyState covers a repo that is resolved but has no rows to show: still loading the
// first page, a filter that matched nothing, or genuinely no open PRs. The no-open-PRs line is
// a left-aligned, sentence-case guiding line consistent with the sibling empty states (the
// board's "No runs yet…", and this file's "No enabled repositories…" / "No pull requests match
// the filter."), and names the scoped repo through renderer.Plain (PathWithNamespace is
// forge-authored — D7) so it reads as guidance, not a bare label.
func (m tuiModel) pullsEmptyState() string {
	if !m.pulls.loaded {
		return m.pal.faint.Render(" loading…") + "\n"
	}
	if strings.TrimSpace(m.pulls.filter) != "" {
		return m.pal.faint.Render(" No pull requests match the filter.") + "\n"
	}
	msg := "No open pull requests."
	if repo, ok := m.currentRepo(); ok {
		msg = "No open pull requests on " + m.renderer.Plain(repo.PathWithNamespace, 40) + "."
	}
	return m.pal.faint.Render(" "+msg) + "\n"
}

// pullsFooter is the one-line key legend for the pulls screen (D13: only the keys M4a binds —
// the u/w/f row actions and `enter open` land in M5). R is dropped when only one repo is
// enabled.
func (m tuiModel) pullsFooter() string {
	parts := []string{m.keyHint("↑↓", "move"), m.keyHint("/", "filter")}
	if len(m.repos) > 1 {
		parts = append(parts, m.keyHint("R", "repo"))
	}
	parts = append(parts, m.keyHint("tab", "views"), m.keyHint("r", "refresh"),
		m.keyHint("?", "keys"), m.keyHint("q", "quit"))
	return " " + strings.Join(parts, m.pal.faint.Render(" · "))
}

// ---- links + errors -------------------------------------------------------

// isHTTPSURL reports whether s parses as an absolute https URL — the gate (with linksEnabled)
// for emitting a forge URL as an OSC-8 hyperlink (D7/D9, the web's isHttpsUrl rule): a
// javascript: / http: / relative URL is never linked.
func isHTTPSURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && u.Scheme == "https" && u.Host != ""
}

// pullLink wraps an already-styled #<iid> segment in an OSC-8 hyperlink to the PR's web URL
// when it is https and the terminal supports links; otherwise the plain styled segment.
// oscLink is the sanitizing sink for the URL (it strips every control/format rune from the
// OSC-8 target), so a hostile URL cannot forge its own terminator — the https check is a
// filter on top of it, never a substitute (D7). It takes the whole PullDTO (like issueLink
// takes the RunDTO) so the WebURL access stays INSIDE here — the D7 AST guard does not gate
// the oscLink path (oscLink is not a recognised writer), and keeping the access out of the
// caller's string-concatenation avoids a false guard hit; WebURL stays in d7UntrustedFields
// as a tripwire and the hostile-URL render test is the real defence.
func (m tuiModel) pullLink(pr apitypes.PullDTO, styled string) string {
	if m.linksEnabled() && isHTTPSURL(pr.WebURL) {
		return oscLink(pr.WebURL, styled)
	}
	return styled
}

// rateLimitRetry reports the Retry-After a forge 429 carried (PRD #1255 D4): a rate-limit shed
// maps to an ExitError{ExitUnreachable} with RetryAfter set, which the header renders as
// `~ rate-limited · retry in Ns` rather than a generic error line.
func rateLimitRetry(err error) (time.Duration, bool) {
	var ee *uzicli.ExitError
	if errors.As(err, &ee) && ee.Code == uzicli.ExitUnreachable && ee.RetryAfter > 0 {
		return ee.RetryAfter, true
	}
	return 0, false
}
