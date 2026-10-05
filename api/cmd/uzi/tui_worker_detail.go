package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
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
	if m.view == viewDetail && m.detailReturn != viewWorker {
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
		m.workerDetail.scroll = max(0, m.workerDetail.scroll+motionDelta(k))
		return m, nil
	}
	if d := motionDelta(k); d != 0 {
		m.workerNav.req++
		m.workerDetail.cursor += d
		m.workerDetail.selectedRunID = ""
		m.reconcileReportedRun()
		if r, ok := m.scopedWorker(m.workerDetail.workerID); ok {
			line := 5 + len(workerAttention(r, time.Now())) + m.workerDetail.cursor
			capacity := max(1, m.height-3)
			if line < m.workerDetail.scroll {
				m.workerDetail.scroll = line
			}
			if line >= m.workerDetail.scroll+capacity {
				m.workerDetail.scroll = line - capacity + 1
			}
		}
		return m, nil
	}
	if k == keyRefresh {
		return m, (&m).startWorkersReq()
	}
	if k != keyEnter || m.workerDetail.selectedRunID == "" {
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
func (m tuiModel) renderWorker() string {
	r, ok := m.scopedWorker(m.workerDetail.workerID)
	if !ok {
		return "worker not in your list\n esc back"
	}
	t := workerTextOf(r)
	lines := []string{m.pal.title.Render(m.renderer.Plain(t.workerName, max(1, m.width))), "", "Attention"}
	for _, a := range workerAttention(r, time.Now()) {
		lines = append(lines, m.renderer.Plain(a.attnDetail, max(1, m.width)))
	}
	lines = append(lines, "", "Reported runs")
	for i, run := range r.w.ReportedRuns {
		mark := "  "
		if i == m.workerDetail.cursor {
			mark = "› "
		}
		pending := ""
		if run.TerminalPending {
			pending = " · outcome pending"
		}
		lines = append(lines, fmt.Sprintf("%s%s · %s · generation %d%s", mark, m.renderer.Plain(run.RunID, 36), m.renderer.Plain(run.Phase, 24), run.ClaimGeneration, pending))
	}
	capacity := max(1, m.height-3)
	m.workerDetail.scroll = min(m.workerDetail.scroll, max(0, len(lines)-capacity))
	lines = lines[m.workerDetail.scroll:min(len(lines), m.workerDetail.scroll+capacity)]
	lines = append(lines, m.workerDetail.notice, "enter run · esc back · pgup/pgdn scroll · r refresh · ? keys")
	for i, line := range lines {
		lines[i] = clampVisual(line, max(1, m.width))
	}
	return strings.Join(lines, "\n")
}
