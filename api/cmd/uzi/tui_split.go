package main

import (
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
)

// The bounds include all three band headings and two inter-band spacers even
// when the current list has fewer bands.
const (
	splitSharedChrome = 1 + 1 + 2 + 1 + 1 + 1 + 1 // wordmark, fleet fallback, meters, summary fallback, vault, admin, error
	splitBandHeadings = 3
	splitBandSpacers  = splitBandHeadings - 1
	splitFloorChrome  = 1 + 1 + splitBandHeadings + splitBandSpacers
	splitForgeChrome  = 1 + 1 + 1 + splitBandHeadings + splitBandSpacers
	splitSeparator    = 1
	splitFooter       = 1
	splitRows         = 8
	splitMinHeight    = splitSharedChrome + splitSeparator + splitFooter + splitFloorChrome + splitForgeChrome + 2*splitRows
)

func (m tuiModel) listView() bool {
	return m.view == viewBoard || m.view == viewWorkers || m.view == viewCI || m.view == viewPulls
}
func (m *tuiModel) setListView(v tuiView) {
	m.workerOrigin = workerOrigin{}
	m.view = v
	if v == viewBoard || v == viewWorkers {
		m.topTab = v
	}
	if v == viewCI || v == viewPulls {
		m.bottomTab = v
	}
}
func (m tuiModel) top() tuiView {
	if m.topTab == viewWorkers {
		return viewWorkers
	}
	return viewBoard
}
func (m tuiModel) bottom() tuiView {
	if m.bottomTab == viewPulls {
		return viewPulls
	}
	return viewCI
}
func (m tuiModel) splitEligible() bool {
	return m.splitMode != "off" && !m.splitOff && m.splitLatch
}
func (m tuiModel) splitDrawn() bool {
	return m.splitEligible() && m.listView() && !m.quitting && !m.showHelp && !m.updatePrompt.showing
}
func (m tuiModel) displayedForge() tuiView {
	if m.quitting || m.showHelp || m.updatePrompt.showing {
		return viewBoard
	}
	if m.splitDrawn() {
		return m.bottom()
	}
	return m.view
}
func (m *tuiModel) collapseSplit() {
	if m.view == viewCI {
		m.ci.filtering = false
	}
	if m.view == viewPulls {
		m.pulls.filtering = false
	}
	m.setListView(m.top())
}
func (m tuiModel) focusBottom() (tea.Model, tea.Cmd) {
	if m.bottom() == viewPulls {
		return m.gotoPulls()
	}
	return m.gotoCI()
}
func (m *tuiModel) activateSplit() tea.Cmd {
	if !m.splitDrawn() {
		return nil
	}
	if !m.reposLoaded {
		if !m.reposInFlight {
			m.reposInFlight = true
			return m.fetchReposCmd()
		}
		return nil
	}
	if !m.boardReplied && !m.repoChosen {
		return nil
	}
	m.resolveDefaultRepo()
	if !m.pullsRepoReady() {
		return nil
	}
	if m.bottom() == viewPulls {
		if m.pulls.waitID == 0 {
			return m.startPullsReq()
		}
	} else if m.ci.waitID == 0 {
		return m.startCIReq()
	}
	return nil
}
func (m tuiModel) splitHeights() (int, int) {
	return m.splitHeightsWithHeader(len(m.splitHeader(time.Now())))
}

// The resize threshold stays worst-case; pane sizes charge only the drawn header.
func (m tuiModel) splitHeightsWithHeader(headerRows int) (int, int) {
	avail := max(2, m.height-headerRows-splitSeparator-splitFooter)
	baseFloor := splitFloorChrome + splitRows
	baseForge := splitForgeChrome + splitRows
	extra := avail - baseFloor - baseForge
	// If future header chrome exceeds the threshold allowance, shrink the panes
	// rather than letting their base allocations push the footer off screen.
	if extra < 0 {
		return (avail + 1) / 2, avail / 2
	}
	return baseFloor + (extra+1)/2, baseForge + extra/2
}
func (m tuiModel) boardScrollCapacity() int {
	if m.splitDrawn() {
		h, _ := m.splitHeights()
		return m.boardCapacityAt(h, 0, false)
	}
	return m.boardCapacity()
}
func (m tuiModel) ciScrollCapacity() int {
	if m.splitDrawn() {
		_, h := m.splitHeights()
		return m.ciCapacityAt(h, false)
	}
	return m.ciCapacity()
}
func (m tuiModel) pullsScrollCapacity() int {
	if m.splitDrawn() {
		_, h := m.splitHeights()
		return m.pullsCapacityAt(h, false)
	}
	return m.pullsCapacity()
}
func splitPane(body string, height int) string {
	lines := strings.Split(strings.TrimSuffix(body, "\n"), "\n")
	if len(lines) > height {
		lines = lines[:height]
	}
	for len(lines) < height {
		lines = append(lines, "")
	}
	return strings.Join(lines, "\n")
}
func (m tuiModel) splitSeparatorLine() string {
	_, paneHeight := m.splitHeights()
	return m.splitSeparatorAt(paneHeight)
}

func (m tuiModel) splitSeparatorAt(paneHeight int) string {
	tab := m.bottom()
	focus := m.view == tab
	var tabs string
	if focus {
		if tab == viewPulls {
			tabs = m.pal.title.Render("[pulls]") + m.pal.faint.Render(" · ci")
		} else {
			tabs = m.pal.faint.Render("pulls · ") + m.pal.title.Render("[ci]")
		}
	} else if tab == viewPulls {
		tabs = m.pal.title.Render("pulls") + m.pal.faint.Render(" · ci")
	} else {
		tabs = m.pal.faint.Render("pulls · ") + m.pal.title.Render("ci")
	}
	line := " " + tabs
	filter, summary, filtering := m.ci.filter, m.ciSummary(), m.ci.filtering
	if tab == viewPulls {
		filter, summary, filtering = m.pulls.filter, m.pullsSummary(), m.pulls.filtering
		rows := m.pulls.visible()
		if len(rows) > 0 {
			items := buildPullItems(rows)
			start, end := boardWindow(selectedBoardItem(items, m.pulls.cursor), m.pulls.scroll, len(items), m.pullsCapacityAt(paneHeight, false))
			lo, hi := windowRunSpan(items, start, end)
			summary += m.pal.faint.Render(" · " + itoa(lo) + "–" + itoa(hi))
		}
	} else {
		rows := m.ci.visible()
		if len(rows) > 0 {
			items := buildCIItems(rows)
			start, end := boardWindow(selectedBoardItem(items, m.ci.cursor), m.ci.scroll, len(items), m.ciCapacityAt(paneHeight, false))
			lo, hi := windowRunSpan(items, start, end)
			summary += m.pal.faint.Render(" · " + itoa(lo) + "–" + itoa(hi))
		}
	}
	if filter != "" || filtering {
		line += "   /" + cellText(filter)
		if filtering {
			line += m.pal.title.Render("▌")
		}
	}
	line += "   " + summary
	if repo, ok := m.currentRepo(); ok {
		if width := m.width - visualWidth(line) - 3; width >= 16 {
			line += "   " + clampVisual(m.renderer.Plain(repo.PathWithNamespace, width), width)
		}
	}
	line = clampVisual(line, m.width)
	if rest := m.width - visualWidth(line); rest > 0 {
		line += m.pal.faint.Render(strings.Repeat("─", rest))
	}
	return line
}

// A split note yields to the supplied footer; callers reserve any separate readout.
func (m tuiModel) withSplitNote(footer string) string {
	if m.splitMode == "off" || !m.listView() || m.splitDrawn() {
		return footer
	}
	note := m.splitNote
	if note == "" && m.splitOff && m.splitLatch {
		note = "s split"
	}
	if note != "" {
		candidate := " " + m.pal.faint.Render(note) + " · " + strings.TrimPrefix(footer, " ")
		if visualWidth(candidate) <= m.width {
			return candidate
		}
	}
	return footer
}

func (m tuiModel) splitFooterLine() string {
	var hints []string
	if m.view == viewBoard {
		hints = []string{"enter/→ open", "tab pane", "/ filter", "a scope", "h fold done", "r refresh", "? keys", "q quit"}
	}
	if m.view == viewWorkers {
		hints = []string{"enter/→ open", "j/k move", "ctrl+w focus", "/ filter", "a scope", "r refresh", "? keys", "q quit"}
	}
	if m.view == viewCI {
		hints = []string{"enter/→ open", "tab pane", "R repo", "/ filter", "r refresh", "? keys", "q quit"}
	}
	if m.view == viewPulls {
		hints = []string{"enter/→ open", "tab pane", "R repo", "/ filter", "r refresh", "? keys", "q quit"}
	}
	if m.showVersion && m.updatePrompt.installedVersion != "" {
		hint := m.restartHintText()
		if m.profile != colorprofile.Ascii {
			hint = m.pal.faint.Render(hint)
		}
		hw := visualWidth(hint)
		// One column separates the key hints from the right-aligned restart hint.
		if left, ok := fitSplitHints(hints, "", m.width-hw-1); ok {
			return padVisual(left, m.width-hw) + hint
		}
		if line, ok := m.restartFooter(" ? keys · q quit"); ok {
			return line
		}
	}
	suffix := ""
	if m.showVersion {
		suffix = " " + m.versionReadout()
	}
	left, fits := fitSplitHints(hints, suffix, m.width)
	if fits {
		return padVisual(left, m.width)
	}
	return clampVisual(left, m.width)
}

// fitSplitHints drops optional key hints in a fixed order until the joined
// hints plus suffix fit within width. fits reports whether the returned line
// does; when false the line is the narrowest candidate reached.
func fitSplitHints(hints []string, suffix string, width int) (line string, fits bool) {
	hints = append([]string(nil), hints...)
	for len(hints) > 2 {
		line = " " + strings.Join(hints, " · ") + suffix
		if visualWidth(line) <= width {
			return line, true
		}
		removed := false
		for _, key := range []string{"r refresh", "h fold done", "a scope", "/ filter"} {
			for i, h := range hints {
				if h == key {
					hints = append(hints[:i], hints[i+1:]...)
					removed = true
					break
				}
			}
			if removed {
				break
			}
		}
		if !removed {
			return line, false
		}
	}
	return " " + strings.Join(hints, " · ") + suffix, false
}

// splitHeader is the sole source of shared header lines and their row count.
// Build it once per render: time-dependent meter widths can change its height.
func (m tuiModel) splitHeader(now time.Time) []string {
	return m.splitHeaderSummary(now, m.boardSummary())
}
func (m tuiModel) splitHeaderSummary(now time.Time, summary string) []string {
	var lines []string
	floor := "floor"
	if m.board.admin {
		floor = "active runs"
	}
	labels := []struct {
		name string
		tab  tuiView
	}{{floor, viewBoard}, {"workers", viewWorkers}}
	title := " " + m.pal.title.Render("▚▚ uzi")
	for _, label := range labels {
		title += m.pal.faint.Render(" · ")
		if label.tab == m.top() {
			name := label.name
			if m.view == m.top() {
				name = "[" + name + "]"
			}
			title += m.pal.title.Render(name)
		} else {
			title += m.pal.faint.Render(label.name)
		}
	}
	lines = append(lines, m.workerFleetTitleLines(title)...)
	if m.top() == viewBoard {
		lines = append(lines, m.boardMeterSummaryLines(m.boardMeterLayout(now).lines, summary)...)
	}
	if vault := m.vaultIndicatorLine(); vault != "" {
		lines = append(lines, vault)
	}
	if m.board.adminDenied {
		lines = append(lines, clampVisual(m.pal.faint.Render(" the factory-wide board needs an admin (uza_) token — showing your runs"), m.width))
	}
	if m.board.err != nil {
		lines = append(lines, clampVisual(m.pal.faint.Render(" could not refresh: "+fmtErr(m.board.err)), m.width))
	}
	// Retain one row per pane, separator and footer even if future header
	// additions outgrow the worst-case allowance used by the resize latch.
	return lines[:min(len(lines), max(0, m.height-splitSeparator-splitFooter-2))]
}

func (m tuiModel) renderSplit() string {
	now := time.Now()
	lines := m.splitHeader(now)
	top, bottom := m.splitHeightsWithHeader(len(lines))
	if m.top() == viewBoard {
		lines = m.splitHeaderSummary(now, m.boardWindowSummary(m.boardCapacityAt(top, 0, false)))
	}
	if m.top() == viewWorkers {
		lines = append(lines, splitPane(m.renderWorkersBody(top, false), top))
	} else {
		lines = append(lines, splitPane(m.renderBoardBody(top, false), top))
	}
	lines = append(lines, m.splitSeparatorAt(bottom))
	if m.bottom() == viewPulls {
		lines = append(lines, splitPane(m.renderPullsBody(bottom, false), bottom))
	} else {
		lines = append(lines, splitPane(m.renderCIBody(bottom, false), bottom))
	}
	lines = append(lines, m.splitFooterLine())
	return strings.Join(lines, "\n")
}
