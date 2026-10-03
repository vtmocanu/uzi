package main

import (
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

// The bounds include all three band headings and two inter-band spacers even
// when the current list has fewer bands.
const (
	splitSharedChrome = 1 + 2 + 1 + 1 + 1 // wordmark, meters, vault, admin, error
	splitBandHeadings = 3
	splitBandSpacers  = splitBandHeadings - 1
	splitFloorChrome  = 1 + 1 + splitBandHeadings + splitBandSpacers
	splitForgeChrome  = 1 + 1 + 1 + splitBandHeadings + splitBandSpacers
	splitSeparator    = 1
	splitFooter       = 1
	splitRows         = 8
	splitMinHeight    = splitSharedChrome + splitSeparator + splitFooter + 2*(splitForgeChrome+splitRows)
)

func (m tuiModel) listView() bool {
	return m.view == viewBoard || m.view == viewCI || m.view == viewPulls
}
func (m *tuiModel) setListView(v tuiView) {
	m.view = v
	if v == viewCI || v == viewPulls {
		m.bottomTab = v
	}
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
	m.setListView(viewBoard)
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
	avail := m.height - splitSharedChrome - splitSeparator - splitFooter
	baseFloor := splitFloorChrome + splitRows
	baseForge := splitForgeChrome + splitRows
	extra := avail - baseFloor - baseForge
	if extra < 0 {
		extra = 0
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
	_, paneHeight := m.splitHeights()
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
		hints = []string{"enter/→ open", "tab pane", "/ filter", "a factory", "h fold done", "r refresh", "? keys", "q quit"}
	}
	if m.view == viewCI {
		hints = []string{"enter/→ open", "tab pane", "R repo", "/ filter", "r refresh", "? keys", "q quit"}
	}
	if m.view == viewPulls {
		hints = []string{"enter/→ open", "tab pane", "R repo", "/ filter", "r refresh", "? keys", "q quit"}
	}
	suffix := ""
	if m.showVersion {
		suffix = " " + m.versionReadout()
	}
	for len(hints) > 2 {
		line := " " + strings.Join(hints, " · ") + suffix
		if visualWidth(line) <= m.width {
			return padVisual(line, m.width)
		}
		removed := false
		for _, key := range []string{"r refresh", "h fold done", "a factory", "/ filter"} {
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
			return clampVisual(line, m.width)
		}
	}
	return clampVisual(" "+strings.Join(hints, " · ")+suffix, m.width)
}
func (m tuiModel) renderSplit() string {
	var lines []string
	floor := "floor"
	if m.board.admin {
		floor = "active runs"
	}
	if m.view == viewBoard {
		floor = "[" + floor + "]"
	}
	lines = append(lines, clampVisual(" "+m.pal.title.Render("▚▚ uzi")+" · "+m.pal.title.Render(floor), m.width))
	lines = append(lines, m.boardMeterLayout(time.Now()).lines...)
	if vault := m.vaultIndicatorLine(); vault != "" {
		lines = append(lines, vault)
	}
	if m.board.adminDenied {
		lines = append(lines, clampVisual(m.pal.faint.Render(" the factory-wide board needs an admin (uza_) token — showing your runs"), m.width))
	}
	if m.board.err != nil {
		lines = append(lines, clampVisual(m.pal.faint.Render(" could not refresh: "+fmtErr(m.board.err)), m.width))
	}
	top, bottom := m.splitHeights()
	lines = append(lines, splitPane(m.renderBoardBody(top, false), top))
	lines = append(lines, m.splitSeparatorLine())
	if m.bottom() == viewPulls {
		lines = append(lines, splitPane(m.renderPullsBody(bottom, false), bottom))
	} else {
		lines = append(lines, splitPane(m.renderCIBody(bottom, false), bottom))
	}
	lines = append(lines, m.splitFooterLine())
	return strings.Join(lines, "\n")
}
