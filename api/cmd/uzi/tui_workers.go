package main

import (
	"context"
	"fmt"
	"image/color"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

type workerRow struct {
	w            apitypes.WorkerDTO
	workerOwner  string
	pressureText []string
}
type workerRowText struct {
	workerName, workerVersion, templateDeclared, templateReported          string
	upgradeDetail, upgradeTarget, blockingContainer, blockingReason        string
	outboxBlockedText, tokenLabel, hostedSize, capabilityText, workerOwner string
	quarantineText                                                         string
	pressureText                                                           []string
}

func workerTextOf(r workerRow) workerRowText {
	w := r.w
	return workerRowText{
		workerName: w.Name, workerVersion: workerString(w.Version),
		templateDeclared: workerString(w.TemplateDeclared), templateReported: workerString(w.TemplateReported),
		upgradeDetail: workerString(w.UpgradeDetail), upgradeTarget: w.UpgradeTarget,
		blockingContainer: workerString(w.UpgradeBlockingContainer), blockingReason: workerString(w.UpgradeBlockingReason),
		outboxBlockedText: workerString(w.OutboxBlocked), quarantineText: workerString(w.ResidueQuarantineCause), tokenLabel: workerString(w.AnthropicSecretLabel),
		hostedSize: workerString(w.HostedSize), capabilityText: strings.Join(w.Capabilities, " "),
		workerOwner: r.workerOwner, pressureText: r.pressureText,
	}
}
func workerString(s *string) string {
	if s == nil {
		return "?"
	}
	return *s
}

type attnItem struct {
	severity              int
	attnShort, attnDetail string
}
type workersState struct {
	rows                             []workerRow
	admin, loaded, active, filtering bool
	filter                           string
	selectedID                       string
	cursor, scroll, errStreak        int
	err                              error
	reqSeq, waitID, tickGen          uint64
}
type workersMsg struct {
	rows  []workerRow
	admin bool
	err   error
	reqID uint64
}
type workersTickMsg struct{ gen uint64 }

var workersPollTimeout = 10 * time.Second
var workersPollInterval = 5 * time.Second

func workersTickInterval(failures int) time.Duration {
	return min(60*time.Second, workersPollInterval<<uint(min(4, max(0, failures))))
}
func workersTickAfter(d time.Duration, gen uint64) tea.Cmd {
	return tea.Tick(d, func(time.Time) tea.Msg { return workersTickMsg{gen} })
}
func (m tuiModel) workersVisible() bool {
	if m.showHelp || m.quitting || m.updatePrompt.showing {
		return false
	}
	return m.view == viewBoard || m.view == viewWorkers || m.view == viewWorker || m.splitDrawn()
}

// Every update and key path reconciles visibility. Departure invalidates pending
// ticks; entry supersedes any request left outstanding while hidden.
func (m tuiModel) reconcileWorkers(cmd tea.Cmd) (tea.Model, tea.Cmd) {
	if m.workerNav.runID != "" && (m.view != viewDetail || m.detail.runID != m.workerNav.runID || m.detail.gen != m.workerNav.gen || m.board.admin != m.workerNav.admin) {
		m.workerNav.req++
		m.workerNav.runID = ""
	}
	if m.view == viewWorker && m.workerDetail.admin != m.board.admin {
		next, exit := m.leaveWorker()
		n := next.(tuiModel)
		if n.view == viewDetail {
			n.detail.steer.notice = "worker not in your list"
		} else {
			n.splitNote = "worker not in your list"
		}
		return n.reconcileWorkers(tea.Batch(cmd, exit))
	}
	active := m.workersVisible()
	if active != m.workers.active {
		m.workers.active = active
		m.workers.tickGen++
		if active {
			cmd = tea.Batch(cmd, (&m).startWorkersReq())
		}
	}
	return m, cmd
}
func (m *tuiModel) startWorkersReq() tea.Cmd {
	return m.startWorkersReqMode(false)
}
func (m *tuiModel) startWorkersReqMode(finite bool) tea.Cmd {
	scopeChanged := m.workers.admin != m.board.admin
	if scopeChanged {
		m.workers.rows, m.workers.loaded = nil, false
		m.workers.admin = m.board.admin
		m.workers.cursor, m.workers.scroll, m.workers.errStreak = 0, 0, 0
		m.workers.selectedID = ""
		m.workers.err = nil
	}
	if !finite && !scopeChanged && !m.workersVisible() {
		m.workers.waitID = 0
		m.workers.tickGen++
		return nil
	}
	m.workers.reqSeq++
	m.workers.waitID = m.workers.reqSeq
	m.workers.tickGen++
	return m.fetchWorkersCmd(m.board.admin, m.workers.waitID)
}
func (m tuiModel) fetchWorkersCmd(admin bool, id uint64) tea.Cmd {
	c, parent := m.client, m.ctx
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, workersPollTimeout)
		defer cancel()
		msg := workersMsg{admin: admin, reqID: id}
		if admin {
			ws, err := c.AdminListWorkers(ctx)
			msg.err = err
			for _, w := range ws {
				msg.rows = append(msg.rows, workerRow{w.WorkerDTO, w.OwnerEmail, w.DiskPressureVolumes})
			}
		} else {
			ws, err := c.ListWorkers(ctx)
			msg.err = err
			for _, w := range ws {
				msg.rows = append(msg.rows, workerRow{w: w})
			}
		}
		return msg
	}
}
func (m tuiModel) applyWorkers(msg workersMsg) (tea.Model, tea.Cmd) {
	if msg.reqID == 0 || msg.reqID != m.workers.waitID || msg.admin != m.board.admin {
		return m, nil
	}
	m.workers.waitID = 0
	if msg.err != nil {
		if msg.admin && uzicli.ExitCodeFor(msg.err) == uzicli.ExitAuth {
			m.board.admin, m.board.adminDenied = false, true
			m.workerNav.req++
			m.workerNav.runID = ""
			return m, tea.Batch((&m).startWorkersReq(), (&m).startBoardReq())
		}
		m.workers.err = msg.err
		m.workers.errStreak++
	} else {
		old := m.workers.visible(time.Now())
		m.workers.cursor = m.workers.selectedIndex(old)
		id := m.workers.selectedID
		if id == "" && len(old) > 0 {
			id = old[min(m.workers.cursor, len(old)-1)].w.ID
		}
		m.workers.rows, m.workers.admin, m.workers.loaded = msg.rows, msg.admin, true
		m.workers.err, m.workers.errStreak = nil, 0
		v := m.workers.visible(time.Now())
		for i, r := range v {
			if r.w.ID == id {
				m.workers.cursor = i
				break
			}
		}
		m.workers.clamp(len(v))
		m.workers.rememberSelection(v)
	}
	var departure tea.Cmd
	if msg.err == nil && m.view == viewWorker {
		if _, ok := m.scopedWorker(m.workerDetail.workerID); !ok {
			m.workerDetail.notice = "worker not in your list"
			next, cmd := m.leaveWorker()
			m = next.(tuiModel)
			if m.view == viewDetail {
				m.detail.steer.notice = "worker not in your list"
			} else {
				m.splitNote = "worker not in your list"
			}
			departure = cmd
		} else {
			m.reconcileReportedRun()
		}
	}
	if !m.workersVisible() {
		return m, departure
	}
	m.workers.tickGen++
	return m, tea.Batch(departure, workersTickAfter(workersTickInterval(m.workers.errStreak), m.workers.tickGen))
}
func workerState(r workerRow) string {
	w := r.w
	switch {
	case w.Status != "online":
		return "offline"
	case w.CustodyDecisionsNeeded != nil && *w.CustodyDecisionsNeeded > 0:
		return "holding"
	case w.DrainingSince != nil && w.ActiveRuns > 0:
		return "draining"
	case w.DrainingSince != nil:
		return "cordoned"
	case w.Busy:
		return "busy"
	default:
		return "idle"
	}
}
func workerStateGlyph(state string) string {
	switch state {
	case "offline":
		return "○"
	case "holding":
		return "⚑"
	case "draining":
		return "◐"
	case "cordoned":
		return "◌"
	case "busy":
		return "◉"
	default:
		return "●"
	}
}

type workerDisk struct {
	label       string
	pct         float64
	displayOnly bool
}

func workerDisks(w apitypes.WorkerDTO) []workerDisk {
	var ds []workerDisk
	add := func(label string, u, t *int64, display bool) {
		if u != nil && t != nil && *t > 0 {
			ds = append(ds, workerDisk{label, 100 * float64(*u) / float64(*t), display})
		}
	}
	add("data", w.StatsDiskDataBytes, w.StatsDiskDataTotalBytes, false)
	add("nix", w.StatsDiskNixBytes, w.StatsDiskNixTotalBytes, false)
	add("dind", w.StatsDiskDindBytes, w.StatsDiskDindTotalBytes, true)
	add("data ino", w.StatsDiskDataInodes, w.StatsDiskDataTotalInodes, true)
	add("dind ino", w.StatsDiskDindInodes, w.StatsDiskDindTotalInodes, true)
	return ds
}
func workerDuration(d time.Duration) string {
	d = max(0, d)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
func workerAge(at *time.Time, now time.Time) string {
	if at == nil {
		return "?"
	}
	return workerDuration(now.Sub(*at))
}
func workerAttention(r workerRow, now time.Time) []attnItem {
	w, t := r.w, workerTextOf(r)
	renderer := &tuiRenderer{}
	var items []attnItem
	add := func(sev int, short, detail string) { items = append(items, attnItem{sev, short, detail}) }
	if w.UpgradeStatus == "upgrade_failed" {
		detail := "✕ upgrade failed: " + renderer.Plain(t.blockingContainer, 200) + " · " + renderer.Plain(t.blockingReason, 200) + " · " + renderer.Plain(t.upgradeDetail, 200) + " · target " + renderer.Plain(t.upgradeTarget, 200) + ", running " + renderer.Plain(t.workerVersion, 200)
		if w.UpgradeLastExitCode != nil {
			detail += fmt.Sprintf(" · exit %d", *w.UpgradeLastExitCode)
		}
		add(0, "✕ upgrade failed", detail)
	}
	if w.OutboxBlocked != nil {
		add(0, "✕ outbox blocked", "✕ outbox blocked: "+renderer.Plain(t.outboxBlockedText, 200))
	}
	// Residue quarantine (issue #2213): the worker found an unreadable, unattributed
	// runner-uid process and claims nothing until its container restarts. The cause is the
	// worker's own self-report (the api sanitizes it, but it is still untrusted), so it is
	// drawn only through renderer.Plain.
	if w.ResidueQuarantinedAt != nil {
		add(0, "✕ quarantined", "✕ quarantined "+workerAge(w.ResidueQuarantinedAt, now)+" ago · claims nothing until the worker container restarts · cause: "+renderer.Plain(t.quarantineText, 200))
	}
	for _, volume := range t.pressureText {
		text := "✕ pressure: " + renderer.Plain(volume, 200)
		if volume == "dind" {
			text += " (display-only)"
		}
		add(0, text, text+" · server-reported (factory scope)")
	}
	for _, d := range workerDisks(w) {
		if d.pct < 90 || d.displayOnly {
			continue
		}
		sev := 0
		text := fmt.Sprintf("✕ %s %.0f%%", d.label, d.pct)
		add(sev, text, text+" · visual cue, not a server admission verdict")
	}
	if w.CustodyDecisionsNeeded != nil && *w.CustodyDecisionsNeeded > 0 {
		add(1, "⚑ unpublished work", "⚑ unpublished work needs an owner decision")
	}
	for _, run := range w.ReportedRuns {
		if run.TerminalPending {
			text := "◷ outcome pending " + workerAge(run.TerminalPendingSince, now)
			add(1, text, text+" · not yet delivered/acknowledged")
		}
	}
	if w.OutboxPendingMessages != nil && *w.OutboxPendingMessages > 0 {
		text := fmt.Sprintf("⇡ outbox %d queued", *w.OutboxPendingMessages)
		add(1, text, text)
	}
	if w.Status != "online" {
		text := "○ offline " + workerAge(w.LastHeartbeatAt, now)
		add(1, text, text+" since last heartbeat")
	}
	if w.DrainingSince != nil {
		text := "◌ cordoned"
		if w.ActiveRuns > 0 {
			text = fmt.Sprintf("◐ draining, %d run left", w.ActiveRuns)
		}
		add(1, text, text+" · claims nothing new")
	}
	if w.UpgradeStatus == "outdated" {
		add(1, "↑ outdated", "↑ outdated "+renderer.Plain(t.workerVersion, 200)+" · target "+renderer.Plain(t.upgradeTarget, 200))
	}
	if w.TemplateDeclared != nil && w.TemplateReported != nil && *w.TemplateDeclared != *w.TemplateReported {
		add(1, "▲ template drift", "▲ template drift: declared "+renderer.Plain(t.templateDeclared, 200)+" · reported "+renderer.Plain(t.templateReported, 200))
	}
	for _, d := range workerDisks(w) {
		if d.pct >= 90 && d.displayOnly {
			text := fmt.Sprintf("▲ %s %.0f%% (display-only)", d.label, d.pct)
			add(1, text, text+" · fixed visual cue, not a server admission verdict")
		}
	}
	if w.Ephemeral && w.EphemeralLeaseExpiresAt != nil && w.EphemeralLeaseExpiresAt.After(now) {
		text := "· lease " + workerDuration(w.EphemeralLeaseExpiresAt.Sub(now)) + " left"
		add(2, text, text+" · ephemeral lease held for follow-up")
	}
	if w.Busy && w.ActiveRuns == 0 {
		add(2, "· chat active", "· chat active (run lane occupancy is zero)")
	}
	slices.SortStableFunc(items, func(a, b attnItem) int { return a.severity - b.severity })
	return items
}
func workerSeverity(r workerRow, now time.Time) int {
	items := workerAttention(r, now)
	if len(items) == 0 {
		return 3
	}
	return items[0].severity
}
func (s workersState) visible(now time.Time) []workerRow {
	type rankedWorker struct {
		row      workerRow
		severity int
		name     string
	}
	ranked := make([]rankedWorker, 0, len(s.rows))
	filter := strings.ToLower(s.filter)
	for _, r := range s.rows {
		name := cellText(r.w.Name)
		if strings.Contains(strings.ToLower(name), filter) {
			ranked = append(ranked, rankedWorker{r, workerSeverity(r, now), name})
		}
	}
	slices.SortStableFunc(ranked, func(a, b rankedWorker) int {
		if d := a.severity - b.severity; d != 0 {
			return d
		}
		if d := strings.Compare(a.name, b.name); d != 0 {
			return d
		}
		return strings.Compare(a.row.w.ID, b.row.w.ID)
	})
	rows := make([]workerRow, len(ranked))
	for i, r := range ranked {
		rows[i] = r.row
	}
	return rows
}

func (s *workersState) clamp(n int) {
	s.cursor = min(max(0, s.cursor), max(0, n-1))
	s.scroll = min(s.scroll, max(0, n-1))
}
func (s workersState) selectedIndex(rows []workerRow) int {
	for i, r := range rows {
		if s.selectedID != "" && r.w.ID == s.selectedID {
			return i
		}
	}
	return min(max(0, s.cursor), max(0, len(rows)-1))
}
func (s *workersState) rememberSelection(rows []workerRow) {
	s.selectedID = ""
	if len(rows) > 0 {
		s.selectedID = rows[s.cursor].w.ID
	}
}
func stripDestination(v tuiView, k string) (tuiView, bool) {
	order := []tuiView{viewBoard, viewWorkers, viewPulls, viewCI}
	switch k {
	case keyViewFloor:
		return viewBoard, true
	case keyViewWorkers:
		return viewWorkers, true
	case keyViewPulls:
		return viewPulls, true
	case keyViewCI:
		return viewCI, true
	case keyTab, "shift+tab":
		i := slices.Index(order, v)
		if i < 0 {
			return v, false
		}
		delta := 1
		if k == "shift+tab" {
			delta = 3
		}
		return order[(i+delta)%4], true
	}
	return v, false
}
func (m tuiModel) gotoList(v tuiView) (tea.Model, tea.Cmd) {
	switch v {
	case viewCI:
		return m.gotoCI()
	case viewPulls:
		return m.gotoPulls()
	}
	oldView := m.view
	m.setListView(v)
	if m.workers.active && oldView != v {
		return m, (&m).startWorkersReq()
	}
	return m, nil
}
func (m tuiModel) workersKey(k string) (tea.Model, tea.Cmd) {
	if m.workers.filtering {
		switch k {
		case keyEsc, keyEnter:
			m.workers.filtering = false
		case "backspace":
			rs := []rune(m.workers.filter)
			if len(rs) > 0 {
				m.workers.filter = string(rs[:len(rs)-1])
			}
		default:
			if k == keySpaceName {
				k = " "
			}
			if len([]rune(k)) == 1 {
				m.workers.filter += k
			}
		}
		m.workers.cursor, m.workers.scroll = 0, 0
		m.workers.rememberSelection(m.workers.visible(time.Now()))
		return m, nil
	}
	rows := m.workers.visible(time.Now())
	m.workers.cursor = m.workers.selectedIndex(rows)
	if d := motionDelta(k); d != 0 {
		m.workers.cursor += d
		m.workers.clamp(len(rows))
		m.workers.rememberSelection(rows)
		return m, nil
	}
	switch k {
	case keyEnter, keyRight:
		if len(rows) > 0 {
			return m.openWorker(rows[m.workers.cursor].w.ID)
		}
		return m, nil
	case keyHome:
		m.workers.cursor = 0
	case keyEnd:
		m.workers.cursor = max(0, len(rows)-1)
	case keyFilter:
		m.workers.filtering = true
	case keyRefresh:
		return m, (&m).startWorkersReq()
	case keyAdmin:
		m.board.admin = !m.board.admin
		m.board.adminDenied = false
		return m, tea.Batch((&m).startBoardReq(), (&m).startWorkersReq())
	case keyEsc:
		return m.gotoList(viewBoard)
	}
	m.workers.rememberSelection(rows)
	return m, nil
}
func workerSlots(w apitypes.WorkerDTO) string {
	cap := "?"
	if w.MaxConcurrentRuns != nil {
		cap = itoa(*w.MaxConcurrentRuns)
	}
	return itoa(w.ActiveRuns) + "/" + cap
}
func (m tuiModel) workersSummary(width int) string {
	if m.workers.admin != m.board.admin || len(m.workers.rows) == 0 {
		return ""
	}
	n, online, used, cap, unknown, attention, holding, draining, cordoned := len(m.workers.rows), 0, 0, 0, 0, 0, 0, 0, 0
	now := time.Now()
	for _, r := range m.workers.rows {
		if r.w.Status == "online" {
			online++
			used += r.w.ActiveRuns
			if r.w.MaxConcurrentRuns == nil {
				unknown++
			} else {
				cap += *r.w.MaxConcurrentRuns
			}
		}
		if workerSeverity(r, now) < 2 {
			attention++
		}
		switch workerState(r) {
		case "holding":
			holding++
		case "draining":
			draining++
		case "cordoned":
			cordoned++
		}
	}
	scope := "workers"
	if m.board.admin {
		scope = "factory workers"
	}
	build := func(showUnknown, showAdmission, showOnline, showCount bool) string {
		parts := []string{paintSeg(nil, nil, true, scope)}
		if showCount {
			parts = append(parts, itoa(n))
		}
		if showOnline && online > 0 {
			parts = append(parts, fmt.Sprintf("%d online", online))
		}
		slots := paintSeg(m.pal.tungsten, nil, false, itoa(used)) + fmt.Sprintf("/%d slots in use", cap)
		if showUnknown && unknown > 0 {
			slots += m.pal.faint.Render(fmt.Sprintf(" +%d ?cap", unknown))
		}
		parts = append(parts, slots)
		if showAdmission {
			if holding > 0 {
				parts = append(parts, paintSeg(m.pal.amber, nil, false, fmt.Sprintf("%d holding", holding)))
			}
			if draining > 0 {
				parts = append(parts, paintSeg(m.pal.wait, nil, false, fmt.Sprintf("%d draining", draining)))
			}
			if cordoned > 0 {
				parts = append(parts, paintSeg(m.pal.wait, nil, false, fmt.Sprintf("%d cordoned", cordoned)))
			}
		}
		if attention > 0 {
			parts = append(parts, paintSeg(m.pal.amber, nil, false, fmt.Sprintf("%d need attention", attention)))
		}
		return strings.Join(parts, " · ")
	}
	for _, options := range [][4]bool{{true, true, true, true}, {false, true, true, true}, {false, false, true, true}, {false, false, false, false}} {
		text := build(options[0], options[1], options[2], options[3])
		if visualWidth(text) <= width {
			return text
		}
	}
	return build(false, false, false, false)
}

func (m tuiModel) workerKind(r workerRow) string {
	if r.w.Kind == "external" {
		return "ext"
	}
	if r.w.Kind != "hosted" {
		return "?"
	}
	t := workerTextOf(r)
	kind := "host"
	if r.w.Ephemeral {
		kind = "eph"
	}
	size := "?"
	switch strings.ToUpper(t.hostedSize) {
	case "S":
		size = "S"
	case "M":
		size = "M"
	case "L":
		size = "L"
	}
	kind += "·" + size
	if r.w.Docker != nil && *r.w.Docker {
		kind += "+dk"
	}
	return kind
}
func (m tuiModel) workerStateColor(state string) color.Color {
	switch state {
	case "busy":
		return m.pal.sage
	case "holding":
		return m.pal.amber
	case "offline":
		return m.pal.stall
	case "draining", "cordoned":
		return m.pal.wait
	default:
		return m.pal.faintC
	}
}
func (m tuiModel) workerAttentionColor(a attnItem) color.Color {
	if a.severity == 0 {
		return m.pal.alarm
	}
	if a.severity == 2 {
		return m.pal.faintC
	}
	if strings.HasPrefix(a.attnShort, "⚑") {
		return m.pal.amber
	}
	if strings.HasPrefix(a.attnShort, "◐") || strings.HasPrefix(a.attnShort, "◌") {
		return m.pal.wait
	}
	return m.pal.stall
}
func (m tuiModel) workerUsageColor(pct float64) color.Color {
	if pct >= 90 {
		return m.pal.alarm
	}
	if pct >= 75 {
		return m.pal.stall
	}
	return nil
}
func (m tuiModel) workerUsage(pct float64, cells int) string {
	n := min(cells, max(0, int(pct*float64(cells)/100+0.5)))
	return paintSeg(m.workerUsageColor(pct), nil, false, strings.Repeat("▮", n)) + paintSeg(m.pal.faintC, nil, false, strings.Repeat("▯", cells-n)) + paintSeg(m.workerUsageColor(pct), nil, false, fmt.Sprintf(" %.0f%%", pct))
}
func (m tuiModel) workerFleetTitleLines(title string) []string {
	status := m.workersSummary(m.width)
	if status == "" {
		return []string{clampVisual(title, m.width)}
	}
	// Fit the fleet's optional segments into the title's remaining space first.
	compact := m.workersSummary(max(0, m.width-visualWidth(title)-2))
	if gap := m.width - visualWidth(title) - visualWidth(compact); gap >= 2 {
		return []string{title + strings.Repeat(" ", gap) + compact}
	}
	return []string{clampVisual(title, m.width), clampVisual(status, m.width)}
}

const (
	workerCPUWidth       = 5  // includes the stale prefix in ~100%
	workerDiskWidth      = 19 // stale prefix, eight-cell label, four-cell bar, 100%
	workerMemoryMinWidth = 9  // includes ~12.0/16G
)

func workerMemoryText(w apitypes.WorkerDTO) string {
	if w.StatsMemBytes == nil {
		return "?"
	}
	used := float64(*w.StatsMemBytes) / (1 << 30)
	if w.StatsMemLimitBytes != nil && *w.StatsMemLimitBytes > 0 {
		return fmt.Sprintf("%.1f/%.0fG", used, float64(*w.StatsMemLimitBytes)/(1<<30))
	}
	return fmt.Sprintf("%.1fG", used)
}

type workerTableWidths struct{ memory, version int }

// Measure the filtered fleet once; the header and every row share these widths.
func (m tuiModel) workerTableWidths(rows []workerRow, width int) workerTableWidths {
	columns := workerTableWidths{memory: workerMemoryMinWidth, version: 8}
	for _, row := range rows {
		text := workerMemoryText(row.w)
		if workerState(row) == "offline" {
			text = "~" + text
		}
		columns.memory = max(columns.memory, visualWidth(text))
		marker := 0
		if row.w.UpgradeStatus == "outdated" || row.w.UpgradeStatus == "upgrade_failed" {
			marker = 1
		}
		columns.version = max(columns.version, visualWidth(m.renderer.Plain(workerTextOf(row).workerVersion, 18))+marker)
	}
	// Cursor, name, state, kind, runs, resource cells, heartbeat, ten separators,
	// and one attention glyph; factory scope adds owner plus its separator.
	fixed := 1 + 13 + 10 + 9 + 4 + workerCPUWidth + columns.memory + workerDiskWidth + 3 + 11
	if m.board.admin {
		fixed += 8
	}
	columns.version = max(1, min(columns.version, 18, max(1, width-fixed-4)))
	return columns
}
func (m tuiModel) workerRowLine(r workerRow, selected bool, width int, columns workerTableWidths, now time.Time) string {
	t := workerTextOf(r)
	var bg color.Color
	if selected {
		bg = m.pal.selBg
	}
	cell := func(text string, n int, fg color.Color) string {
		return padSeg(paintSeg(fg, bg, false, clampVisual(text, n)), n, bg)
	}
	pre := " "
	if selected {
		pre = "▌"
	}
	fields := []string{paintSeg(m.pal.tungsten, bg, true, pre), cell(m.renderer.Plain(t.workerName, 13), 13, nil)}
	if width >= 120 && m.board.admin {
		fields = append(fields, cell(m.workerOwnerCell(r), 7, m.pal.faintC))
	}
	state := workerState(r)
	fields = append(fields, cell(workerStateGlyph(state)+" "+state, 10, m.workerStateColor(state)), cell(m.workerKind(r), 9, m.pal.faintC), cell(workerSlots(r.w), 4, nil))
	if width >= 120 {
		cpu, disk := "?", "?"
		var cpuC, diskC color.Color
		if r.w.StatsCPUPct != nil {
			cpu = fmt.Sprintf("%3.0f%%", *r.w.StatsCPUPct)
			cpuC = m.workerUsageColor(*r.w.StatsCPUPct)
		}
		mem := workerMemoryText(r.w)
		worst := -1.0
		for _, d := range workerDisks(r.w) {
			if d.pct > worst {
				worst = d.pct
				filled := min(4, max(0, int(d.pct*4/100+0.5)))
				disk = paintSeg(nil, bg, false, padVisual(d.label, 8)+" ") + paintSeg(m.workerUsageColor(d.pct), bg, false, strings.Repeat("▮", filled)) + paintSeg(m.pal.faintC, bg, false, strings.Repeat("▯", 4-filled)) + paintSeg(m.workerUsageColor(d.pct), bg, false, fmt.Sprintf(" %.0f%%", d.pct))
			}
		}
		var memC color.Color
		if state == "offline" {
			cpu = "~" + strings.TrimSpace(cpu)
			mem = "~" + mem
			disk = "~" + ansi.Strip(disk)
			cpuC = m.pal.faintC
			memC = m.pal.faintC
			diskC = m.pal.faintC
		}
		versionWidth := columns.version
		marker, verC := "", color.Color(nil)
		switch r.w.UpgradeStatus {
		case "outdated":
			marker = "↑"
			verC = m.pal.stall
		case "upgrade_failed":
			marker = "✕"
			verC = m.pal.alarm
		}
		ver := clampVisual(m.renderer.Plain(t.workerVersion, versionWidth), max(0, versionWidth-visualWidth(marker))) + marker
		fields = append(fields, cell(cpu, workerCPUWidth, cpuC), cell(mem, columns.memory, memC), cell(disk, workerDiskWidth, diskC), cell(ver, versionWidth, verC), cell(workerAge(r.w.LastHeartbeatAt, now), 3, m.pal.faintC))
	}
	items := workerAttention(r, now)
	att := paintSeg(m.pal.faintC, bg, false, "—")
	if len(items) > 0 {
		suffix := ""
		if len(items) > 1 {
			suffix = fmt.Sprintf(" +%d", len(items)-1)
		}
		budget := max(0, width-visualWidth(strings.Join(fields, " "))-1-visualWidth(suffix))
		att = paintSeg(m.workerAttentionColor(items[0]), bg, false, m.renderer.Plain(items[0].attnShort, budget)) + paintSeg(m.pal.faintC, bg, false, suffix)
	}
	fields = append(fields, att)
	line := clampVisual(strings.Join(fields, paintSeg(nil, bg, false, " ")), width)
	return line + paintSeg(nil, bg, false, strings.Repeat(" ", max(0, width-visualWidth(line))))
}
func (m tuiModel) workerReadout(r workerRow, width int) []string {
	t := workerTextOf(r)
	selected := m.pal.faint.Render("selected ") + m.pal.title.Render(m.renderer.Plain(t.workerName, max(0, width-9)))
	if m.board.admin {
		selected += m.pal.faint.Render(" · owner ") + m.renderer.Plain(t.workerOwner, width)
	}
	items := workerAttention(r, time.Now())
	if width >= 120 {
		for _, item := range items {
			selected += m.pal.faint.Render(" · ") + paintSeg(m.workerAttentionColor(item), nil, false, m.renderer.Plain(item.attnShort, width))
		}
		return []string{clampVisual(selected, width)}
	}
	lines := []string{clampVisual(selected, width)}
	for _, item := range items {
		lines = append(lines, clampVisual("  "+paintSeg(m.workerAttentionColor(item), nil, false, m.renderer.Plain(item.attnShort, max(0, width-2))), width))
	}
	return lines
}

func (m tuiModel) renderWorkers() string { return m.renderWorkersBody(m.height, true) }
func (m tuiModel) renderWorkersBody(height int, full bool) string {
	if height <= 0 {
		return ""
	}
	width := m.width
	now := time.Now()
	var rows []workerRow
	if m.workers.admin == m.board.admin {
		rows = m.workers.visible(now)
	}
	columns := m.workerTableWidths(rows, width)
	var lines []string
	if full {
		lines = append(lines, m.workerFleetTitleLines(" "+m.tabStrip(m.board.admin, viewWorkers, false))...)
	}
	if m.board.adminDenied {
		lines = append(lines, clampVisual("factory workers need an admin (uza_) token — showing your workers", width))
	}
	if m.workers.err != nil {
		lines = append(lines, clampVisual("could not refresh: "+fmtErr(m.workers.err), width))
	}
	if m.workers.filter != "" || m.workers.filtering {
		lines = append(lines, clampVisual("/"+cellText(m.workers.filter), width))
	}
	cols := []string{padVisual("NAME", 13)}
	if width >= 120 && m.board.admin {
		cols = append(cols, padVisual("OWNER", 7))
	}
	cols = append(cols, padVisual("STATE", 10), padVisual("KIND", 9), padVisual("RUNS", 4))
	if width >= 120 {
		cols = append(cols, padVisual("CPU", workerCPUWidth), padVisual("MEM", columns.memory), padVisual("DISK (worst)", workerDiskWidth), padVisual("VERSION", columns.version), padVisual("HB", 3))
	}
	cols = append(cols, "ATTENTION")
	header := "  " + strings.Join(cols, " ")
	lines = append(lines, "", m.pal.faint.Render(clampVisual(header, width)))
	cursor := m.workers.selectedIndex(rows)
	var readout []string
	if len(rows) > 0 {
		readout = m.workerReadout(rows[cursor], width)
	}
	footer := "enter/→ worker · j/k move · / filter · a scope · r refresh · ? keys · q quit"
	reserve := 0
	if full {
		reserve = 1
	}
	// Bound the readout before allocating list rows, retaining the selected row
	// and footer even at tiny heights. Each failure is independent of other rows.
	readout = readout[:min(len(readout), max(0, height-len(lines)-reserve-2))]
	capacity := max(0, height-len(lines)-len(readout)-reserve-1)
	start := min(m.workers.scroll, max(0, len(rows)-capacity))
	if cursor < start {
		start = cursor
	}
	if cursor >= start+capacity {
		start = max(0, cursor-capacity+1)
	}
	for i := start; i < min(len(rows), start+capacity); i++ {
		lines = append(lines, m.workerRowLine(rows[i], m.view == viewWorkers && i == cursor, width, columns, now))
	}
	if len(rows) == 0 && capacity > 0 {
		empty := "no workers"
		if !m.workers.loaded {
			empty = "loading workers…"
		}
		lines = append(lines, empty)
	}
	lines = append(lines, "")
	lines = append(lines, readout...)
	if full {
		for len(lines) < height-1 {
			lines = append(lines, "")
		}
		lines = lines[:min(len(lines), max(0, height-1))]
		lines = append(lines, clampVisual(m.withSplitNote(footer), width))
	}
	lines = lines[:min(len(lines), height)]
	return strings.Join(lines, "\n")
}

func (m tuiModel) workerOwnerCell(r workerRow) string {
	if m.selfEmail != "" && r.workerOwner == m.selfEmail {
		return "you"
	}
	owner := m.renderer.Plain(r.workerOwner, 200)
	local, _, _ := strings.Cut(owner, "@")
	return clampVisual(local, 7)
}
