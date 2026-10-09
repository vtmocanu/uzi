package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// One origin survives worker/run crosslinks; run sessions themselves never survive.
type workerOrigin struct {
	runID                    string
	detailReturn             tuiView
	workerID                 string
	cursor, scroll           int
	topTab, bottomTab, focus tuiView
	fromSplit, splitOff      bool
	splitMode                string
}
type workerDetailState struct {
	workerID, selectedRunID string
	cursor, scroll          int
	gen                     uint64
	admin                   bool
	notice                  string
}
type workerNavigation struct {
	req                            uint64
	workerID, runID, selectedRunID string
	gen                            uint64
	admin                          bool
}
type workerRunMsg struct {
	nav workerNavigation
	run apitypes.RunDTO
	err error
}
type runWorkerMsg struct {
	nav     workerNavigation
	workers workersMsg
}

func (m *tuiModel) clearRunSession() {
	if m.detail.stream != nil {
		m.detail.stream.Close()
	}
	m.detailGen++
	m.detail = detailState{}
	m.workerNav.req++
}
func (m tuiModel) scopedWorker(id string) (workerRow, bool) {
	if m.workers.admin != m.board.admin {
		return workerRow{}, false
	}
	for _, r := range m.workers.rows {
		if r.w.ID == id {
			return r, true
		}
	}
	return workerRow{}, false
}
func (m tuiModel) openWorker(id string) (tea.Model, tea.Cmd) {
	if _, ok := m.scopedWorker(id); !ok {
		return m, nil
	}
	if m.view == viewDetail && m.workerOrigin.runID == "" && m.workerOrigin.workerID == "" {
		m.workerOrigin = workerOrigin{runID: m.detail.runID, detailReturn: m.detailReturn,
			topTab: m.topTab, bottomTab: m.bottomTab, focus: m.view, fromSplit: m.fromSplit, splitOff: m.splitOff, splitMode: m.splitMode}
	} else if m.view == viewWorkers {
		m.workerOrigin = workerOrigin{workerID: id, cursor: m.workers.cursor, scroll: m.workers.scroll,
			topTab: m.topTab, bottomTab: m.bottomTab, focus: m.view, fromSplit: m.fromSplit || m.splitDrawn(), splitOff: m.splitOff, splitMode: m.splitMode}
	}
	m.clearRunSession()
	m.workerGen++
	m.workerDetail = workerDetailState{workerID: id, gen: m.workerGen, admin: m.board.admin}
	m.view = viewWorker
	m.reconcileReportedRun()
	return m, nil
}
func (m *tuiModel) reconcileReportedRun() {
	r, ok := m.scopedWorker(m.workerDetail.workerID)
	if !ok {
		return
	}
	d := &m.workerDetail
	for i, run := range r.w.ReportedRuns {
		if run.RunID == d.selectedRunID {
			d.cursor = i
			return
		}
	}
	if d.selectedRunID != "" {
		m.workerNav.req++
	}
	d.cursor = min(max(0, d.cursor), max(0, len(r.w.ReportedRuns)-1))
	d.selectedRunID = ""
	if len(r.w.ReportedRuns) > 0 {
		d.selectedRunID = r.w.ReportedRuns[d.cursor].RunID
	}
}
func (m tuiModel) leaveWorker() (tea.Model, tea.Cmd) {
	m.workerNav.req++
	o := m.workerOrigin
	m.workerOrigin = workerOrigin{}
	m.topTab, m.bottomTab, m.fromSplit, m.splitOff, m.splitMode = o.topTab, o.bottomTab, o.fromSplit, o.splitOff, o.splitMode
	if o.runID != "" {
		return m.openLinkedRun(&o.runID, o.detailReturn)
	}
	m.setListView(viewWorkers)
	m.topTab = viewWorkers
	m.fromSplit = false
	m.workers.cursor, m.workers.scroll, m.workers.selectedID = o.cursor, o.scroll, o.workerID
	rows := m.workers.visible(time.Now())
	m.workers.cursor = m.workers.selectedIndex(rows)
	m.workers.clamp(len(rows))
	m.workers.rememberSelection(rows)
	return m, nil
}
func (m tuiModel) workerKey(k string) (tea.Model, tea.Cmd) {
	if k == keyEsc || k == keyLeft {
		return m.leaveWorker()
	}
	if k == keyPageUp || k == keyPageDown {
		lines, _ := m.workerDetailLines(time.Now())
		capacity := max(0, m.height-2)
		m.workerDetail.scroll = min(max(0, len(lines)-capacity), max(0, m.workerDetail.scroll+motionDelta(k)))
		return m, nil
	}
	if d := motionDelta(k); d != 0 {
		m.workerNav.req++
		m.workerDetail.cursor += d
		m.workerDetail.selectedRunID = ""
		m.reconcileReportedRun()
		lines, selected := m.workerDetailLines(time.Now())
		capacity := max(1, m.height-2)
		m.workerDetail.scroll = min(max(0, m.workerDetail.scroll), max(0, len(lines)-capacity))
		if selected < m.workerDetail.scroll {
			m.workerDetail.scroll = selected
		}
		if selected >= m.workerDetail.scroll+capacity {
			m.workerDetail.scroll = selected - capacity + 1
		}
		return m, nil
	}
	if k == keyRefresh {
		return m, (&m).startWorkersReq()
	}
	if (k != keyEnter && k != keyRight) || m.workerDetail.selectedRunID == "" {
		return m, nil
	}
	m.workerNav.req++
	m.workerNav = workerNavigation{req: m.workerNav.req, workerID: m.workerDetail.workerID, selectedRunID: m.workerDetail.selectedRunID, gen: m.workerDetail.gen, admin: m.board.admin}
	nav, c, parent := m.workerNav, m.client, m.ctx
	m.workerDetail.notice = "loading run…"
	return m, func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, boardPollTimeout)
		defer cancel()
		run, err := c.GetRun(ctx, nav.selectedRunID)
		return workerRunMsg{nav, run, err}
	}
}
func (m tuiModel) applyWorkerRun(msg workerRunMsg) (tea.Model, tea.Cmd) {
	n := msg.nav
	if m.view != viewWorker || n != m.workerNav || n.gen != m.workerDetail.gen || n.admin != m.board.admin || n.workerID != m.workerDetail.workerID || n.selectedRunID != m.workerDetail.selectedRunID {
		return m, nil
	}
	m.workerNav.req++
	if msg.err != nil {
		m.workerDetail.notice = "could not open run: " + fmtErr(msg.err)
		if uzicli.ExitCodeFor(msg.err) == uzicli.ExitNotFound {
			m.workerDetail.notice = "run not visible"
		}
		return m, nil
	}
	// Deliver the preflight DTO through the normal guarded handler, without another GetRun.
	m.beginRunSession(n.selectedRunID, viewWorker)
	next, cmd := m.update(detailRunMsg{runID: n.selectedRunID, run: msg.run, gen: m.detail.gen})
	m = next.(tuiModel)
	return m, tea.Batch(cmd, m.loadTailCmd(n.selectedRunID), m.openStreamCmd(n.selectedRunID))
}
func (m tuiModel) runWorkerKey() (tea.Model, tea.Cmd) {
	m.workerNav.req++
	m.detail.steer.notice = ""
	if m.detail.run.WorkerID == nil {
		m.detail.steer.notice = "no worker yet"
		return m, nil
	}
	id := *m.detail.run.WorkerID
	if _, ok := m.scopedWorker(id); ok {
		return m.openWorker(id)
	}
	m.workerNav = workerNavigation{req: m.workerNav.req, workerID: id, runID: m.detail.runID, gen: m.detail.gen, admin: m.board.admin}
	n, fetch := m.workerNav, (&m).startWorkersReqMode(true)
	m.detail.steer.notice = "loading worker…"
	// A single finite fetch works while the ordinary workers poll is inactive.
	return m, func() tea.Msg { return runWorkerMsg{n, fetch().(workersMsg)} }
}
func (m tuiModel) applyRunWorker(msg runWorkerMsg) (tea.Model, tea.Cmd) {
	n := msg.nav
	if m.view != viewDetail || n != m.workerNav || n.runID != m.detail.runID || n.gen != m.detail.gen || n.admin != m.board.admin || m.detail.run.WorkerID == nil || *m.detail.run.WorkerID != n.workerID {
		return m, nil
	}
	if msg.workers.reqID == 0 || msg.workers.reqID != m.workers.waitID || msg.workers.admin != m.board.admin {
		return m, nil
	}
	next, cmd := m.applyWorkers(msg.workers)
	m = next.(tuiModel)
	m.workerNav.req++
	if msg.workers.err != nil {
		m.detail.steer.notice = "could not load worker: " + fmtErr(msg.workers.err)
		return m, cmd
	}
	if _, ok := m.scopedWorker(n.workerID); !ok {
		m.detail.steer.notice = "worker not in your list"
		return m, nil
	}
	return m.openWorker(n.workerID)
}

// workerDetailLines shares the selected row's actual position with scrolling. Each
// reported run has a title, worker report and separately sourced cached board row.
func (m tuiModel) workerDetailLines(now time.Time) ([]string, int) {
	r, ok := m.scopedWorker(m.workerDetail.workerID)
	if !ok {
		return []string{"worker not in your list"}, 0
	}
	w, t := r.w, workerTextOf(r)
	width := max(1, m.width)
	state := workerState(r)
	header := m.pal.faint.Render("worker › ") + m.pal.title.Render(m.renderer.Plain(t.workerName, width)) + "  " + paintSeg(m.workerStateColor(state), nil, false, workerStateGlyph(state)+" "+state) + "  " + m.pal.faint.Render(m.workerKind(r))
	lines := []string{m.tabStrip(m.board.admin, viewWorkers, false)}
	var timing []string
	if w.Status == "online" {
		if w.OnlineSince != nil {
			timing = append(timing, "up "+workerUptime(w.OnlineSince, now))
		}
		if w.LastHeartbeatAt != nil {
			timing = append(timing, "heartbeat "+workerAge(w.LastHeartbeatAt, now)+" ago")
		}
	} else if w.LastHeartbeatAt != nil {
		timing = append(timing, "last heartbeat "+workerAge(w.LastHeartbeatAt, now)+" ago")
	}
	heartbeat := strings.Join(timing, " · ")
	if heartbeat == "" {
		lines = append(lines, header)
	} else if visualWidth(header)+2+visualWidth(heartbeat) <= width {
		lines = append(lines, header+m.pal.faint.Render("  "+heartbeat))
	} else {
		lines = append(lines, header, "  "+m.pal.faint.Render(heartbeat))
	}
	lines = append(lines, "", m.pal.title.Render("attention"))
	attention := workerAttention(r, now)
	if len(attention) == 0 {
		lines = append(lines, "  "+m.pal.faint.Render("no reported warnings"))
	}
	for _, item := range attention {
		lines = append(lines, "  "+paintSeg(m.workerAttentionColor(item), nil, false, m.renderer.Plain(item.attnDetail, width)))
	}
	lines = append(lines, "", m.pal.title.Render("capacity"), "  "+workerSlots(w)+" runs · "+workerCrossCheckSlots(w)+" cross-checks")
	lines = append(lines, "", m.pal.title.Render("reported runs")+m.pal.faint.Render("  api active_runs ")+itoa(w.ActiveRuns)+m.pal.faint.Render(" / cap ")+strings.Split(workerSlots(w), "/")[1]+m.pal.faint.Render(" (run lane)"))
	selected := 0
	if len(w.ReportedRuns) == 0 {
		lines = append(lines, "  "+m.pal.faint.Render("none"))
	}
	for i, run := range w.ReportedRuns {
		id, title, engine, stage := clampVisual(m.renderer.Plain(run.RunID, width), 8), "", "?", "? (not cached)"
		if m.board.runsAdmin == m.board.admin {
			for _, br := range m.board.runs {
				if br.ID == run.RunID {
					if br.IssueIID != nil {
						id = fmt.Sprintf("#%d", *br.IssueIID)
						title = m.renderer.Plain(runTitle(br.RunDTO), width)
					}
					engine = m.renderer.Plain(br.Harness, width)
					stage = m.renderer.Plain(m.pal.runStateToken(br.RunDTO, br.IsRevising).word, width)
					break
				}
			}
		}
		mark := "  "
		if i == m.workerDetail.cursor {
			mark = m.pal.sel.Render("▌ ")
			selected = len(lines)
		}
		lines = append(lines, mark+m.pal.faint.Render(itoa(i+1)+" ")+m.pal.sel.Render(id)+" "+title+"  "+m.pal.faint.Render(engine))
		phase := "unknown"
		switch run.Phase {
		case "running", "awaiting_approval", "awaiting_input", "awaiting_followup":
			phase = run.Phase
		}
		line := "      " + m.pal.faint.Render("worker phase ") + phase + m.pal.faint.Render(" · run stage ") + stage + m.pal.faint.Render(fmt.Sprintf(" · gen %d", run.ClaimGeneration))
		if run.TerminalPending {
			line += " · " + paintSeg(m.pal.stall, nil, false, "◷ outcome pending "+workerAge(run.TerminalPendingSince, now))
		}
		lines = append(lines, line)
	}
	if len(w.ReportedRuns) > 0 {
		lines = append(lines, "  "+m.pal.faint.Render("↵/→ open run (esc returns here)"))
	}
	lines = append(lines, "", m.pal.title.Render("resources"))
	stale := w.Status != "online"
	if stale {
		lines[len(lines)-1] += m.pal.faint.Render("  last-known, stale")
	}
	kv := func(key, value string) { lines = append(lines, "  "+m.pal.faint.Render(padVisual(key, 13))+" "+value) }
	resource := func(value string) string {
		if stale {
			return m.pal.faint.Render("~ " + ansi.Strip(value))
		}
		return value
	}
	cpu := "?"
	if w.StatsCPUPct != nil {
		cpu = paintSeg(m.workerUsageColor(*w.StatsCPUPct), nil, false, fmt.Sprintf("%.0f%%", *w.StatsCPUPct))
	}
	source := ""
	if w.StatsSource != nil && *w.StatsSource == "process" {
		source = m.pal.faint.Render("  process only")
	}
	kv("cpu", resource(cpu+source))
	kv("memory", resource(workerBytePair(w.StatsMemBytes, w.StatsMemLimitBytes)+source))
	disk := func(key string, used, total, inodes, inodeTotal *int64, display bool) {
		value := "?"
		if used != nil && total != nil && *total > 0 {
			value = m.workerUsage(100*float64(*used)/float64(*total), 12)
		}
		if inodes != nil && inodeTotal != nil && *inodeTotal > 0 {
			pct := 100 * float64(*inodes) / float64(*inodeTotal)
			value += m.pal.faint.Render("  inodes ") + paintSeg(m.workerUsageColor(pct), nil, false, fmt.Sprintf("%.0f%%", pct))
		}
		if display {
			value += m.pal.faint.Render("  display only")
		}
		kv(key, resource(value))
	}
	disk("data", w.StatsDiskDataBytes, w.StatsDiskDataTotalBytes, w.StatsDiskDataInodes, w.StatsDiskDataTotalInodes, false)
	if w.Kind == "hosted" || w.StatsDiskNixBytes != nil {
		disk("nix", w.StatsDiskNixBytes, w.StatsDiskNixTotalBytes, nil, nil, false)
	}
	if (w.Docker != nil && *w.Docker) || w.StatsDiskDindBytes != nil {
		disk("dind", w.StatsDiskDindBytes, w.StatsDiskDindTotalBytes, w.StatsDiskDindInodes, w.StatsDiskDindTotalInodes, true)
	}
	if len(w.RunDisk) > 0 {
		d := w.RunDisk[0]
		id := clampVisual(m.renderer.Plain(d.RunID, width), 8)
		if m.board.runsAdmin == m.board.admin {
			for _, br := range m.board.runs {
				if br.ID == d.RunID && br.IssueIID != nil {
					id = fmt.Sprintf("#%d", *br.IssueIID)
					break
				}
			}
		}
		bound := ""
		if d.Truncated {
			bound = "≥"
		}
		kv("largest HOME", resource(m.pal.sel.Render(id)+"  "+bound+humanBytes(d.HomeBytes)+m.pal.faint.Render(" (sampled "+workerAge(&d.SampledAt, now)+" ago)")))
	}
	lines = append(lines, "", m.pal.title.Render("configuration"))
	ver := m.renderer.Plain(t.workerVersion, width)
	if w.UpgradeStatus == "outdated" {
		ver += "  " + paintSeg(m.pal.stall, nil, false, "↑ outdated · target "+m.renderer.Plain(t.upgradeTarget, width))
	}
	if w.UpgradeStatus == "upgrade_failed" {
		ver += "  " + paintSeg(m.pal.alarm, nil, false, "✕ upgrade failed") + paintSeg(m.pal.stall, nil, false, " · target "+m.renderer.Plain(t.upgradeTarget, width))
	}
	kv("version", ver)
	capabilities := m.renderer.Plain(t.capabilityText, width)
	if capabilities == "" {
		capabilities = "none"
	}
	kv("capabilities", capabilities)
	template := m.renderer.Plain(t.templateDeclared, width)
	if w.TemplateDeclared != nil && w.TemplateReported != nil && *w.TemplateDeclared != *w.TemplateReported {
		template += "  " + paintSeg(m.pal.stall, nil, false, "≠ reported "+m.renderer.Plain(t.templateReported, width))
	}
	kv("template", template)
	mode := "unknown"
	switch w.AnthropicBindMode {
	case "default", "auto":
		mode = w.AnthropicBindMode
	case "pinned":
		mode = "pinned · " + m.renderer.Plain(t.tokenLabel, width)
	}
	kv("token", mode)
	kind := "unknown"
	switch w.Kind {
	case "external", "hosted":
		kind = w.Kind
	}
	if w.Kind == "hosted" {
		kind += " · size " + m.renderer.Plain(t.hostedSize, width)
	}
	if w.Ephemeral {
		lease := "?"
		if w.EphemeralLeaseExpiresAt != nil {
			lease = "expired"
			if w.EphemeralLeaseExpiresAt.After(now) {
				lease = workerDuration(w.EphemeralLeaseExpiresAt.Sub(now)) + " left"
			}
		}
		kind += " · ephemeral · lease " + lease
	}
	kv("kind", kind)
	if m.board.admin {
		kv("owner", m.renderer.Plain(t.workerOwner, width))
	}
	return lines, selected
}
func workerUptime(at *time.Time, now time.Time) string {
	if at == nil {
		return "?"
	}
	d := max(time.Duration(0), now.Sub(*at))
	if d >= 24*time.Hour {
		return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
	}
	if d >= time.Hour {
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return workerDuration(d)
}

func workerBytePair(used, total *int64) string {
	value := func(n *int64) string {
		if n == nil {
			return "?"
		}
		return humanBytes(*n)
	}
	return value(used) + " / " + value(total)
}
func (m tuiModel) renderWorker() string {
	lines, _ := m.workerDetailLines(time.Now())
	capacity := max(0, m.height-2)
	scroll := min(max(0, m.workerDetail.scroll), max(0, len(lines)-capacity))
	lines = lines[scroll:min(len(lines), scroll+capacity)]
	if m.height >= 2 {
		lines = append(lines, m.renderer.Plain(m.workerDetail.notice, max(1, m.width)))
	}
	if m.height >= 1 {
		lines = append(lines, "j/k select · enter/→ run · esc/← back · pgup/pgdn scroll · r refresh · ? keys")
	}
	for i, line := range lines {
		lines[i] = clampVisual(line, max(1, m.width))
	}
	return strings.Join(lines, "\n")
}
