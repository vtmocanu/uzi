package main

// tui_ci.go — the forge `ci` list screen (PRD #1255 M4b, D1/D2/D3/D7/D8/D13), the sibling of
// the `pulls` list (tui_pulls.go) built on the SAME navigation scaffold: the shared repo scope
// (m.repos / m.repoIdx / m.repoChosen), the board's banded layout, and a poll-guard tick chain
// copied from the board (its own reqSeq / waitID / tickGen, a 10s tick with backoff, in-flight
// and generation guards). It shows the repo's workflow/pipeline runs banded RUNNING / FAILED /
// RECENT (D3). Every forge-authored string is drawn through m.renderer.Plain (D7); every band
// and state carries a glyph AND a word so colour is never the only cue (D8).

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

// ciMsg carries a ListCIRuns reply for the `ci` screen, the ci analogue of pullsMsg. reqID is
// the request-generation id this reply belongs to; the model honours it only when reqID ==
// m.ci.waitID, so an older poll that resolves after a newer request was minted is dropped
// instead of clearing the newer request's guard. unsupported is the non-empty degrade sentence
// ListCIRuns returns on the ErrForgeVersionUnsupported path (an old forge without the runs
// endpoint), which the screen renders verbatim instead of an error line (D5/R2).
type ciMsg struct {
	runs        []apitypes.CIRunDTO
	unsupported string
	err         error
	reqID       uint64
}

// ciTickMsg drives the `ci` list's own 10s poll chain, carrying the tick-chain generation it was
// scheduled under (a tick whose gen != m.ci.tickGen is from a superseded chain and is dropped),
// mirroring pullsTickMsg.
type ciTickMsg struct{ gen uint64 }

// ---- poll cadence ---------------------------------------------------------

// ciPollInterval is the `ci` list's poll cadence (PRD #1255 D4: a list polls every 10s). A var
// (not const) so a test can shrink it.
var ciPollInterval = 10 * time.Second

// ciBackoffCap caps the ci error-backoff reschedule interval, the ci twin of pullsBackoffCap. A
// var so a test can shrink it.
var ciBackoffCap = 60 * time.Second

// ciTickAfter arms the ci tick after delay d, stamping the produced ciTickMsg with the tick-chain
// generation gen (the reply owns rescheduling at the backed-off cadence, and gen lets the model
// drop a tick from a superseded chain) — the ci twin of pullsTickAfter.
func ciTickAfter(d time.Duration, gen uint64) tea.Cmd {
	return tea.Tick(d, func(time.Time) tea.Msg { return ciTickMsg{gen: gen} })
}

// ciTickInterval maps the consecutive ci-poll error streak to the reschedule interval (base at
// streak 0, doubling per consecutive failure, clamped at ciBackoffCap), the ci twin of
// pullsTickInterval — a pure function so the backoff is unit-assertable.
func ciTickInterval(streak int) time.Duration {
	if streak <= 0 {
		return ciPollInterval
	}
	if streak > 16 {
		streak = 16
	}
	d := ciPollInterval << uint(streak)
	if d <= 0 || d > ciBackoffCap {
		return ciBackoffCap
	}
	return d
}

// ---- state ----------------------------------------------------------------

type ciState struct {
	runs   []apitypes.CIRunDTO
	cursor int
	scroll int
	err    error

	// reqSeq / waitID / tickGen are the request-generation guard, a copy of the board's (see
	// boardState): waitID == 0 is idle and a tick polls ONLY while idle (the in-flight guard); a
	// ciMsg is honoured only when reqID == waitID (the out-of-order defence); tickGen is the
	// tick-chain generation (a mismatched tick is from a superseded chain).
	reqSeq    uint64
	waitID    uint64
	tickGen   uint64
	errStreak int

	filtering bool
	filter    string

	// loaded is true once a SUCCESSFUL reply has landed, so the view can tell "still loading the
	// first page" from "loaded, and there are no runs".
	loaded bool

	// unsupported is the verbatim degrade sentence ListCIRuns returns when the forge version has
	// no runs endpoint (ErrForgeVersionUnsupported); non-empty only on that path, where runs is
	// empty and the screen renders this sentence instead of the no-runs empty state (D5/R2).
	unsupported string

	// rateLimited / retryAfter render the header's `~ rate-limited · retry in Ns` state (D4): a
	// forge 429 maps to ExitUnreachable carrying a Retry-After, which backs off the poll and shows
	// the strip instead of an error line.
	rateLimited bool
	retryAfter  time.Duration
}

func newCIState() ciState {
	// The tick chain starts at generation 1 (the Init-armed ci tick is honoured); reqSeq / waitID
	// start at 0 (idle) because, like the pulls list, NO request is in flight at Init — the repo
	// the `ci` route needs is not known until ListRepos returns.
	return ciState{tickGen: 1}
}

func (c *ciState) apply(msg ciMsg) {
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
	c.runs = msg.runs
	c.unsupported = msg.unsupported
	c.loaded = true
	c.clampCursor()
}

// resetForRepoChange drops the cached rows, cursor, scroll, loaded flag and unsupported sentence
// so the screen reads "loading…" under the new repo rather than flashing the prior repo's runs.
// It is called on BOTH list states when the SHARED repoIdx cycles (R), since the ci and pulls
// screens scope to the same repo and both caches go stale the instant it changes.
func (c *ciState) resetForRepoChange() {
	c.cursor, c.scroll = 0, 0
	c.runs, c.loaded, c.unsupported = nil, false, ""
}

func (c *ciState) clampCursor() {
	n := len(c.visible())
	if c.cursor >= n {
		c.cursor = n - 1
	}
	if c.cursor < 0 {
		c.cursor = 0
	}
}

func (c *ciState) selected() (apitypes.CIRunDTO, bool) {
	v := c.visible()
	if c.cursor < 0 || c.cursor >= len(v) {
		return apitypes.CIRunDTO{}, false
	}
	return v[c.cursor], true
}

// visible applies the `/` filter, then orders the survivors into the three bands (RUNNING →
// FAILED → RECENT). The cursor indexes this list, so the selectable order IS the visual order.
// The filter matches sanitized cell text (name / event / branch / title / actor) plus the
// status word, so a control byte in a forge field cannot affect what matches.
func (c *ciState) visible() []apitypes.CIRunDTO {
	base := c.runs
	if strings.TrimSpace(c.filter) != "" {
		q := strings.ToLower(strings.TrimSpace(c.filter))
		out := make([]apitypes.CIRunDTO, 0, len(base))
		for _, r := range base {
			hay := strings.ToLower(strings.Join([]string{
				"#" + itoa(int(r.Number)),
				cellText(r.Name), cellText(r.Event), cellText(r.Branch),
				cellText(r.Title), cellText(r.Actor),
				forgeState(r.Status, r.Conclusion),
			}, " "))
			if strings.Contains(hay, q) {
				out = append(out, r)
			}
		}
		base = out
	}
	return ciBandOrder(base)
}

// ---- bands (D3) -----------------------------------------------------------

const (
	// RUNNING: the run is still in flight (Tone(effective) == running — queued/in_progress/
	// waiting on GitHub, running/pending/… on GitLab/Forgejo). FAILED (amber eyebrow): a terminal
	// failure (pipelinestatus.IsFailed — failure/timed_out/…) or action_required (a human must
	// approve — attention, grouped with FAILED per D3). RECENT: everything else (success /
	// cancelled / skipped / neutral), newest first.
	ciBandRunning = iota
	ciBandFailed
	ciBandRecent
	numCIBands
)

var ciBandNames = [numCIBands]string{"RUNNING", "FAILED", "RECENT"}

// ciRunEffective is the raw forge status to classify/colour a run by: the Conclusion once the
// forge split it out (GitHub's completed runs), else the Status (GitLab/Forgejo fold everything
// into Status, and a GitHub run still in flight has no conclusion yet).
func ciRunEffective(r apitypes.CIRunDTO) string {
	if r.Conclusion != "" {
		return r.Conclusion
	}
	return r.Status
}

func ciBand(r apitypes.CIRunDTO) int {
	eff := ciRunEffective(r)
	if pipelinestatus.Tone(eff) == "running" {
		return ciBandRunning
	}
	if pipelinestatus.IsFailed(eff) || eff == "action_required" {
		return ciBandFailed
	}
	return ciBandRecent
}

// ciBandOrder partitions runs into the three bands and, inside each band, sorts by newest
// activity (UpdatedAt) first (D3), then concatenates RUNNING → FAILED → RECENT.
func ciBandOrder(runs []apitypes.CIRunDTO) []apitypes.CIRunDTO {
	var buckets [numCIBands][]apitypes.CIRunDTO
	for _, r := range runs {
		buckets[ciBand(r)] = append(buckets[ciBand(r)], r)
	}
	out := make([]apitypes.CIRunDTO, 0, len(runs))
	for b := 0; b < numCIBands; b++ {
		sort.SliceStable(buckets[b], func(i, j int) bool {
			return buckets[b][i].UpdatedAt.After(buckets[b][j].UpdatedAt)
		})
		out = append(out, buckets[b]...)
	}
	return out
}

// ---- forge text projection (D7) -------------------------------------------

// ciRowText is the forge-authored text of a CIRunDTO projected into DISTINCTLY-NAMED fields so
// the D7 AST guard can require each to be drawn through a sanitizer. CIRunDTO.Name / .Event /
// .Branch / .SHA / .Actor share bare selector names with unrelated pre-existing `.Name`/… draws
// in other tui_*.go files (e.g. tui_detail_transcript.go's payload .Name), so those bare names
// cannot go in d7UntrustedFields without reddening the guard on code this screen does not own.
// The distinct names below (runName, eventName, …) are added to d7UntrustedFields instead, and
// the copy in ciTextOf is plumbing — a struct-field copy, not a draw — so it is not a guard hit.
type ciRowText struct {
	runName    string // CIRunDTO.Name (workflow / pipeline name)
	eventName  string // CIRunDTO.Event (push / pull_request / schedule / …)
	runBranch  string // CIRunDTO.Branch (a PR-triggered GitHub run shows its refs/pull/N/head ref as-is)
	runSHA     string // CIRunDTO.SHA (drawn short, sanitized)
	runTitle   string // CIRunDTO.Title (commit / display title)
	actorLogin string // CIRunDTO.Actor (the login that triggered the run)
}

func ciTextOf(r apitypes.CIRunDTO) ciRowText {
	return ciRowText{
		runName:    r.Name,
		eventName:  r.Event,
		runBranch:  r.Branch,
		runSHA:     r.SHA,
		runTitle:   r.Title,
		actorLogin: r.Actor,
	}
}

// ---- render ---------------------------------------------------------------

// Fixed column widths for a ci row. The title takes what remains; narrow terminals drop the
// optional columns right-to-left before the title, like the board.
const (
	ciNameWidth    = 14 // `<Name> #<Number>`
	ciEventWidth   = 12 // event
	ciBranchWidth  = 16 // branch
	ciStatusWidth  = 8  // forgeState word (running / queued / passed / failed / done)
	ciElapsedWidth = 6  // ciRunElapsed duration
	ciTitleMin     = 10 // narrowest title a row ever draws; below this the row clampVisual's
	ciTitleMax     = 44 // title cap, so a long title does not run a wide terminal
	ciRightWidth   = 11 // the `▰▱ done/total` jobs micro-bar, or the run age
	ciSHAWidth     = 12 // short sha on the selected row's second line
	ciJobBarCells  = 5  // glyph cells in the jobs micro-bar (above this it scales; the count text carries the numbers)
)

// ciCols is the set of OPTIONAL columns a ci row draws at a given terminal width. As the
// terminal narrows the columns are shed right-to-left before the title — branch first, then
// event, then the status+elapsed pair, then the right cell last — so a narrow terminal degrades
// gracefully (drops columns) instead of overflowing m.width and wrapping, which would corrupt
// every row below it. The name cell and the title are never dropped (the title clamps instead),
// and ciRow's final clampVisual is the ultimate backstop below the narrowest tier.
type ciCols struct {
	branch        bool
	event         bool
	statusElapsed bool
	right         bool
}

// ciColumnsFor picks the richest column set that still leaves room for the title floor (and the
// reserved right cell) at width w, dropping columns in the order above. It mirrors the board /
// pulls pattern of dropping columns before squeezing the title.
func ciColumnsFor(w int) ciCols {
	c := ciCols{branch: true, event: true, statusElapsed: true, right: true}
	for _, drop := range []*bool{&c.branch, &c.event, &c.statusElapsed, &c.right} {
		if ciRowLayoutWidth(c) <= w {
			return c
		}
		*drop = false
	}
	return c
}

// ciRowLayoutWidth is the width a row with column set c needs to hold its prefix, the title
// floor, and the reserved right cell (with its 2-col gap) when shown — the fit test ciColumnsFor
// degrades against.
func ciRowLayoutWidth(c ciCols) int {
	w := ciRowPrefixWidth(c) + ciTitleMin
	if c.right {
		w += ciRightWidth + 2
	}
	return w
}

func ciRowPrefixWidth(c ciCols) int {
	// cursor(1)+spine(1)+glyph(1)+space(1), the name cell + its trailing gap, then the event,
	// branch and status+elapsed cells + their trailing gaps when shown.
	w := 4 + ciNameWidth + 2
	if c.event {
		w += ciEventWidth + 2
	}
	if c.branch {
		w += ciBranchWidth + 2
	}
	if c.statusElapsed {
		w += ciStatusWidth + 2 + ciElapsedWidth + 2
	}
	return w
}

func (m tuiModel) renderCI() string {
	var sb strings.Builder
	rows := m.ci.visible()

	brand := m.tabStrip()
	if m.ci.filter != "" || m.ci.filtering {
		brand += m.pal.faint.Render("   /" + cellText(m.ci.filter))
		if m.ci.filtering {
			brand += m.pal.title.Render("▌")
		}
	}

	items := buildCIItems(rows)
	capacity := m.ciCapacity()
	selItem := selectedBoardItem(items, m.ci.cursor)
	start, end := boardWindow(selItem, m.ci.scroll, len(items), capacity)

	summary := m.ciSummary()
	if len(rows) > 0 {
		lo, hi := windowRunSpan(items, start, end)
		summary += m.pal.faint.Render(" · " + itoa(lo) + "–" + itoa(hi))
	}
	sb.WriteString(clampVisual(padVisual(" "+brand, m.width-visualWidth(summary)-1)+summary, m.width) + "\n")

	// Sub-header: the forge rate-limit / error state (D4), else nothing.
	if note := m.ciHeaderNote(); note != "" {
		sb.WriteString(clampVisual(note, m.width) + "\n")
	}
	sb.WriteString("\n")

	switch {
	case !m.pullsRepoReady():
		// The repo-ready check is screen-agnostic (tui_pulls.go): the ci screen shows the SAME
		// scoped repo as the pulls screen, so it reuses that precondition rather than re-deriving it.
		sb.WriteString(m.ciScopeState())
	case len(rows) == 0:
		sb.WriteString(m.ciEmptyState())
	default:
		for i := start; i < end; i++ {
			switch it := items[i]; it.kind {
			case biEyebrow:
				sb.WriteString(clampVisual(m.ciEyebrow(it), m.width) + "\n")
			case biRow:
				r := rows[it.runIdx]
				sel := it.runIdx == m.ci.cursor
				sb.WriteString(m.ciRow(r, sel) + "\n")
				if sel {
					if sl := m.ciSecondLine(r); sl != "" {
						sb.WriteString(sl + "\n")
					}
				}
			default:
				sb.WriteString("\n")
			}
		}
	}

	sb.WriteString(clampVisual(m.ciFooter(), m.width))
	return sb.String()
}

// buildCIItems turns the band-ordered visible list into the flat display list: an eyebrow at
// each band boundary, a spacer between bands, one row per run — the ci twin of buildPullItems,
// reusing boardItem so the shared windowing helpers apply.
func buildCIItems(runs []apitypes.CIRunDTO) []boardItem {
	var counts [numCIBands]int
	for _, r := range runs {
		counts[ciBand(r)]++
	}
	items := make([]boardItem, 0, len(runs)+numCIBands*2)
	prevBand := -1
	for i, r := range runs {
		if b := ciBand(r); b != prevBand {
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

// ciCapacity is how many display lines fit between the header block and the footer, the ci twin
// of pullsCapacity: tab strip + blank + footer (3), plus the optional sub-header note, plus the
// selected row's reserved second line.
func (m tuiModel) ciCapacity() int {
	chrome := 3
	if m.ciHeaderNote() != "" {
		chrome++
	}
	if _, ok := m.ci.selected(); ok {
		chrome++
	}
	c := m.height - chrome
	if c < 1 {
		c = 1
	}
	return c
}

func (m tuiModel) ciSyncedScroll() int {
	items := buildCIItems(m.ci.visible())
	sel := selectedBoardItem(items, m.ci.cursor)
	start, _ := boardWindow(sel, m.ci.scroll, len(items), m.ciCapacity())
	return start
}

// ciEyebrow is a faint CAPS band label + count; FAILED is amber (the one band a human must act
// on), the ci twin of pullEyebrow.
func (m tuiModel) ciEyebrow(it boardItem) string {
	name := ciBandNames[it.band]
	if it.band == ciBandFailed {
		return " " + lipgloss.NewStyle().Foreground(m.pal.amber).Bold(true).Render(name) + m.pal.faint.Render(" · "+itoa(it.count))
	}
	return " " + m.pal.faint.Render(name+" · "+itoa(it.count))
}

// ciRow renders one CI run: the ▌ andon spine + state glyph, `<Name> #<Number>` (an OSC-8 link
// to the run's web URL, https only — D7/D9), the event, the branch, a status word (forgeState),
// the elapsed, the title, and a right cell: the `▰▱ done/total` jobs micro-bar for a RUNNING row
// with jobs, else the run's age.
func (m tuiModel) ciRow(r apitypes.CIRunDTO, sel bool) string {
	band := ciBand(r)
	t := ciTextOf(r)
	glyph, glyphC := m.ciGlyph(r)
	cols := ciColumnsFor(m.width)

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

	// `<Name> #<Number>`: the name (forge text → renderer.Plain) capped so the #<number> suffix
	// (a plain int, not forge text) fits in the fixed cell.
	num := " #" + itoa(int(r.Number))
	nameCap := ciNameWidth - len([]rune(num))
	if nameCap < 3 {
		nameCap = 3
	}
	nameCell := paintSeg(idC, bg, sel, padCell(m.renderer.Plain(t.runName, nameCap)+num, ciNameWidth))

	row := cursor +
		paintSeg(glyphC, bg, false, "▌") +
		paintSeg(glyphC, bg, false, glyph) +
		paintSeg(nil, bg, false, " ") +
		m.ciLink(r, nameCell) + gap

	if cols.event {
		row += paintSeg(m.pal.faintC, bg, false, padCell(m.renderer.Plain(t.eventName, ciEventWidth), ciEventWidth)) + gap
	}
	if cols.branch {
		row += paintSeg(m.pal.faintC, bg, false, padCell(m.renderer.Plain(t.runBranch, ciBranchWidth), ciBranchWidth)) + gap
	}
	if cols.statusElapsed {
		// Status word (mirror the CLI's forgeState vocabulary). forgeState can echo an unknown forge
		// status verbatim, so it is sanitized through renderer.Plain before drawing.
		row += paintSeg(m.pal.faintC, bg, false, padCell(m.renderer.Plain(forgeState(r.Status, r.Conclusion), ciStatusWidth), ciStatusWidth)) + gap
		row += paintSeg(m.pal.faintC, bg, false, padCell(ciRunElapsed(r), ciElapsedWidth)) + gap
	}

	rightReserve := 0
	if cols.right {
		rightReserve = ciRightWidth + 2
	}
	avail := m.width - ciRowPrefixWidth(cols) - rightReserve
	if avail < ciTitleMin {
		avail = ciTitleMin
	}
	if avail > ciTitleMax {
		avail = ciTitleMax
	}
	row += paintSeg(m.ciTitleColor(band, sel), bg, false, clampVisual(m.renderer.Plain(t.runTitle, avail), avail))

	if cols.right {
		// The right cell (jobs micro-bar or age) is flushed to the right edge, aligned in a fixed
		// cell so the title width stays stable down the list (the board pattern). clampVisual caps
		// it to its reserved width so a wide cell (e.g. a ≥100-job `done/total`) can never overrun.
		row = padSeg(row, m.width-ciRightWidth, bg)
		row += padSeg(clampVisual(m.ciRightCell(r, band, bg), ciRightWidth), ciRightWidth, bg)
	} else if sel {
		// No right cell at this width: still span the warm selection bar to the full width.
		row = padSeg(row, m.width, bg)
	}
	// Final backstop: no rendered row may exceed the terminal width at ANY width — below the
	// narrowest column tier the name cell + title floor alone can overrun, so clamp unconditionally
	// rather than let the terminal wrap the row and corrupt every row beneath it.
	return clampVisual(row, m.width)
}

// ciGlyph is the row's spine glyph + colour, by Tone of the effective status (D3/D8): ● running,
// ✗ failed, ✓ passed, ⚠ attention (e.g. action_required), · neutral/skipped. Every state has a
// distinct glyph so colour is never the only carrier.
func (m tuiModel) ciGlyph(r apitypes.CIRunDTO) (string, color.Color) {
	switch pipelinestatus.Tone(ciRunEffective(r)) {
	case "failed":
		return "✗", m.pal.alarm
	case "running":
		return "●", m.pal.wait
	case "passed":
		return "✓", m.pal.sage
	case "attention":
		return "⚠", m.pal.amber
	default: // neutral (success's opposite here is cancelled/skipped/unknown)
		return "·", m.pal.faintC
	}
}

// ciTitleColor follows the board's "one lit thing" rule: FAILED titles are amber (the one band a
// human must act on), the rest are quiet default ink (tungsten when the row is selected so it
// survives the warm bar).
func (m tuiModel) ciTitleColor(band int, sel bool) color.Color {
	if band == ciBandFailed {
		return m.pal.amber
	}
	if sel {
		return m.pal.tungsten
	}
	return nil
}

// ciRightCell is the row's right-edge cell (D3): the `▰▱ done/total` jobs micro-bar for a RUNNING
// row whose route filled JobsTotal (best-effort, running rows only — D5), else the run's age. A
// FAILED row does NOT get a `✗ <failed job>` cell: the LIST DTO carries no per-job array and no
// failed-job name (those live on CIRunDetailDTO, the m6 drill-in), so fabricating one would be a
// guess — the glyph + the FAILED band already carry "this run failed".
func (m tuiModel) ciRightCell(r apitypes.CIRunDTO, band int, bg color.Color) string {
	if band == ciBandRunning && r.JobsTotal > 0 {
		return m.ciJobBar(r.JobsDone, r.JobsTotal, bg)
	}
	return paintSeg(m.pal.faintC, bg, false, relAge(r.CreatedAt))
}

// ciJobBar renders the running-row jobs progress as a ▰/▱ micro-bar (the milestoneMarker
// vocabulary, tui_board_rows.go) that ALWAYS keeps its `done/total` count text (D8) — unlike the
// board's milestoneMarker, which drops the count below boardMileCap. The count carries the real
// numbers and is reserved FIRST so it is never the field cut; the glyph cells then take whatever
// of ciRightWidth remains (capped at ciJobBarCells, proportionally filled), so a run with many
// jobs — e.g. `▰▱ 100/200` — cannot blow the fixed right cell (padSeg never truncates).
func (m tuiModel) ciJobBar(done, total int, bg color.Color) string {
	if done < 0 {
		done = 0
	}
	if done > total {
		done = total
	}
	count := " " + itoa(done) + "/" + itoa(total)
	// Fit the glyph cells into what the count leaves of the reserved right-cell width.
	cells := total
	if cells > ciJobBarCells {
		cells = ciJobBarCells
	}
	if budget := ciRightWidth - visualWidth(count); cells > budget {
		cells = budget
	}
	if cells < 0 {
		cells = 0
	}
	filled := done
	if total > cells {
		filled = done * cells / total
	}
	bar := paintSeg(m.pal.tungsten, bg, false, strings.Repeat("▰", filled)) +
		paintSeg(m.pal.faintC, bg, false, strings.Repeat("▱", cells-filled))
	return bar + paintSeg(m.pal.faintC, bg, false, count)
}

// ciSecondLine is the selected row's faint detail line (D3): only what the LIST CIRunDTO carries
// — the short sha, the actor, the full (clipped) title, and the jobs count for a running run.
// NO per-job breakdown: the jobs/steps array is on CIRunDetailDTO (the m6 drill-in), not here, so
// this line is honest by omission rather than fabricating a job list.
func (m tuiModel) ciSecondLine(r apitypes.CIRunDTO) string {
	t := ciTextOf(r)
	bg := m.pal.selBg
	var b strings.Builder
	b.WriteString(paintSeg(m.pal.tungsten, bg, false, "  ▸ "))
	b.WriteString(paintSeg(m.pal.faintC, bg, false, m.renderer.Plain(t.runSHA, ciSHAWidth)))
	b.WriteString(paintSeg(m.pal.faintC, bg, false, " · "))
	b.WriteString(paintSeg(m.pal.sage, bg, false, m.renderer.Plain(t.actorLogin, 20)))
	b.WriteString(paintSeg(nil, bg, false, " · "+m.renderer.Plain(t.runTitle, 48)))
	if ciBand(r) == ciBandRunning && r.JobsTotal > 0 {
		b.WriteString(paintSeg(m.pal.faintC, bg, false, " · "+itoa(r.JobsDone)+"/"+itoa(r.JobsTotal)+" jobs"))
	}
	return padSeg(clampVisual(b.String(), m.width), m.width, bg)
}

// ciSummary is the header's top-right glyph cluster: ● N (running) · ✗ N (failed) · ✓ N
// (succeeded) · N runs. Computed over ALL runs so it does not shrink under a filter; zero-count
// segments drop. The ✓ count is the RECENT-band runs that actually succeeded (cancelled/skipped
// rows stay out of it), so the cluster never reads a cancelled run as green.
func (m tuiModel) ciSummary() string {
	running, failed, passed := 0, 0, 0
	for _, r := range m.ci.runs {
		switch ciBand(r) {
		case ciBandRunning:
			running++
		case ciBandFailed:
			failed++
		case ciBandRecent:
			if pipelinestatus.IsSuccess(ciRunEffective(r)) {
				passed++
			}
		}
	}
	var segs []string
	if running > 0 {
		segs = append(segs, paintSeg(m.pal.wait, nil, false, "● "+itoa(running)))
	}
	if failed > 0 {
		segs = append(segs, paintSeg(m.pal.alarm, nil, false, "✗ "+itoa(failed)))
	}
	if passed > 0 {
		segs = append(segs, paintSeg(m.pal.sage, nil, false, "✓ "+itoa(passed)))
	}
	segs = append(segs, m.pal.faint.Render(itoa(len(m.ci.runs))+" runs"))
	return strings.Join(segs, m.pal.faint.Render(" · "))
}

// ciHeaderNote is the sub-header line: the forge rate-limit state (D4) takes priority over a
// generic refresh error, so a 429 reads as a backoff rather than a failure.
func (m tuiModel) ciHeaderNote() string {
	if m.ci.rateLimited {
		retry := "soon"
		if m.ci.retryAfter > 0 {
			retry = shortDuration(m.ci.retryAfter)
		}
		return m.pal.faint.Render(" ~ rate-limited · retry in " + retry)
	}
	if m.ci.err != nil {
		return m.pal.faint.Render(" could not refresh: " + fmtErr(m.ci.err))
	}
	return ""
}

// ciScopeState explains why there are no rows to show yet, before any repo is resolved: a repos
// load error, repos not loaded, or no enabled repos — the ci twin of pullsScopeState.
func (m tuiModel) ciScopeState() string {
	switch {
	case m.reposErr != nil:
		return m.pal.faint.Render(" could not load repositories: "+fmtErr(m.reposErr)) + "\n"
	case !m.reposLoaded:
		return m.pal.faint.Render(" loading…") + "\n"
	case len(m.repos) == 0:
		return m.pal.faint.Render(" No enabled repositories. Enable one from the web board to see its CI runs.") + "\n"
	default:
		return m.pal.faint.Render(" loading…") + "\n"
	}
}

// ciEmptyState covers a repo that is resolved but has no rows to show: a forge version without
// the runs endpoint (the verbatim unsupported sentence — D5/R2), still loading the first page, a
// filter that matched nothing, or genuinely no CI runs. The no-runs line is a left-aligned,
// sentence-case guiding line consistent with the sibling empty states and names the scoped repo
// through renderer.Plain (PathWithNamespace is forge-authored — D7).
func (m tuiModel) ciEmptyState() string {
	if m.ci.unsupported != "" {
		// The forge degraded (ErrForgeVersionUnsupported); render the server's sentence verbatim
		// rather than guessing at the reason.
		return m.pal.faint.Render(" "+m.renderer.Plain(m.ci.unsupported, 80)) + "\n"
	}
	if !m.ci.loaded {
		return m.pal.faint.Render(" loading…") + "\n"
	}
	if strings.TrimSpace(m.ci.filter) != "" {
		return m.pal.faint.Render(" No CI runs match the filter.") + "\n"
	}
	msg := "No CI runs yet."
	if repo, ok := m.currentRepo(); ok {
		msg = "No CI runs yet on " + m.renderer.Plain(repo.PathWithNamespace, 40) + "."
	}
	return m.pal.faint.Render(" "+msg) + "\n"
}

// ciFooter is the one-line key legend for the ci screen (D13: only the keys M4b binds — enter/→
// opening the CI run drill-in lands in M6, so it is deliberately NOT promised here). R is dropped
// when only one repo is enabled.
func (m tuiModel) ciFooter() string {
	parts := []string{m.keyHint("↑↓", "move"), m.keyHint("/", "filter")}
	if len(m.repos) > 1 {
		parts = append(parts, m.keyHint("R", "repo"))
	}
	parts = append(parts, m.keyHint("tab", "views"), m.keyHint("r", "refresh"),
		m.keyHint("?", "keys"), m.keyHint("q", "quit"))
	return " " + strings.Join(parts, m.pal.faint.Render(" · "))
}

// ciLink wraps an already-styled `<Name> #<Number>` segment in an OSC-8 hyperlink to the run's
// web URL when it is https and the terminal supports links; otherwise the plain styled segment.
// oscLink is the sanitizing sink for the URL (it strips every control/format rune from the OSC-8
// target), so a hostile URL cannot forge its own terminator — the https check (isHTTPSURL,
// tui_pulls.go) is a filter on top of it, never a substitute (D7). It takes the whole CIRunDTO
// (like pullLink takes the PullDTO) so the WebURL access stays INSIDE here — the D7 AST guard does
// not gate the oscLink path (oscLink is not a recognised writer); WebURL stays in
// d7UntrustedFields as a tripwire and the hostile-URL render test is the real defence.
func (m tuiModel) ciLink(r apitypes.CIRunDTO, styled string) string {
	if m.linksEnabled() && isHTTPSURL(r.WebURL) {
		return oscLink(r.WebURL, styled)
	}
	return styled
}

// ---- keys -----------------------------------------------------------------

// gotoCI leaves the current screen for the `ci` list (PRD #1255 D1): it resolves the default repo
// (once) and, when a repo is ready and no poll is in flight, issues an immediate fetch so the
// screen is not stuck on "loading…" until the next tick — the ci twin of gotoPulls.
func (m tuiModel) gotoCI() (tea.Model, tea.Cmd) {
	m.view = viewCI
	(&m).resolveDefaultRepo()
	if m.ci.waitID == 0 && m.pullsRepoReady() {
		return m, (&m).startCIReq()
	}
	return m, nil
}

func (m tuiModel) ciKey(k string) (tea.Model, tea.Cmd) {
	// Filter input mode swallows ordinary keys so typing "q" / "R" filters rather than
	// quitting / cycling the repo (D13).
	if m.ci.filtering {
		switch k {
		case keyEnter, keyEsc:
			m.ci.filtering = false
			if k == keyEsc {
				m.ci.filter = ""
			}
			m.ci.clampCursor()
		case "backspace":
			if n := len([]rune(m.ci.filter)); n > 0 {
				m.ci.filter = string([]rune(m.ci.filter)[:n-1])
			}
			m.ci.clampCursor()
		default:
			if k == keySpaceName {
				k = " "
			}
			if len([]rune(k)) == 1 {
				m.ci.filter += k
				m.ci.clampCursor()
			}
		}
		m.ci.scroll = m.ciSyncedScroll()
		return m, nil
	}

	if d := motionDelta(k); d != 0 {
		m.ci.cursor += d
		m.ci.clampCursor()
		m.ci.scroll = m.ciSyncedScroll()
		return m, nil
	}

	switch k {
	case keyFilter:
		m.ci.filtering = true
		return m, nil
	case keyRefresh:
		// User-initiated: never gated on the in-flight guard (a keypress is intent), mirroring the
		// pulls screen's r. startCIReq mints a fresh id so the next tick does not stack on it.
		if m.pullsRepoReady() {
			return m, (&m).startCIReq()
		}
		// No repo resolved yet: when the shared repo scope is not ready (still loading, or a
		// transient ListRepos failure), r retries fetchReposCmd — the precondition for any ci fetch
		// — so the user can recover the screen without restarting the TUI. The reposInFlight guard
		// keeps it from stacking a second outstanding repos fetch.
		if !m.reposReady() && !m.reposInFlight {
			m.reposInFlight = true
			return m, m.fetchReposCmd()
		}
		return m, nil
	case keyRepoCycle:
		// R cycles the enabled repos (D2); hidden from the legend when only one is enabled, so this
		// is a no-op then. The choice persists for the session (repoChosen) and is SHARED with the
		// pulls screen — so BOTH caches go stale the instant repoIdx changes. Clear them both (the
		// pulls screen would otherwise render repo A's PRs under repo B's header on its next visit,
		// until its own poll lands), then refetch only the CURRENT (ci) screen.
		if len(m.repos) > 1 {
			m.repoIdx = (m.repoIdx + 1) % len(m.repos)
			m.repoChosen = true
			m.ci.resetForRepoChange()
			m.pulls.resetForRepoChange()
			return m, (&m).startCIReq()
		}
		return m, nil
	case keyTab, keyViewFloor:
		// tab advances the cycle ci → floor; 1 jumps to the floor directly (D1).
		m.view = viewBoard
		return m, nil
	case keyViewPulls:
		return m.gotoPulls()
	case keyViewCI:
		return m, nil // already here
	case keyEsc:
		// esc on a list returns to the floor (D1).
		m.view = viewBoard
		return m, nil
	}
	return m, nil
}

// ---- commands -------------------------------------------------------------

// fetchCIRunsCmd reads one repo's CI runs, tagged with the request-generation id so a stale reply
// is dropped (PRD #1255 D4; the #1130 poll-guard pattern). A zero limit sends no query param, so
// the server applies its own default (forgeViewCIRunsDefault), matching `uzi ci list`.
func (m tuiModel) fetchCIRunsCmd(repoID string, reqID uint64) tea.Cmd {
	c, parent := m.client, m.ctx
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, boardPollTimeout)
		defer cancel()
		runs, unsupported, err := c.ListCIRuns(ctx, repoID, 0)
		return ciMsg{runs: runs, unsupported: unsupported, err: err, reqID: reqID}
	}
}

// startCIReq mints the next ci request id, records it as the one the model is waiting on, and
// returns the tagged fetch — the ci twin of startPullsReq. It returns nil when no repo is resolved
// (nothing to fetch).
func (m *tuiModel) startCIReq() tea.Cmd {
	repo, ok := m.currentRepo()
	if !ok {
		return nil
	}
	m.ci.reqSeq++
	m.ci.waitID = m.ci.reqSeq
	return m.fetchCIRunsCmd(repo.ID, m.ci.waitID)
}
