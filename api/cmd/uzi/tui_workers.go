package main

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
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
	pressureText                                                           []string
}

func workerTextOf(r workerRow) workerRowText {
	w := r.w
	return workerRowText{
		workerName: w.Name, workerVersion: workerString(w.Version),
		templateDeclared: workerString(w.TemplateDeclared), templateReported: workerString(w.TemplateReported),
		upgradeDetail: workerString(w.UpgradeDetail), upgradeTarget: w.UpgradeTarget,
		blockingContainer: workerString(w.UpgradeBlockingContainer), blockingReason: workerString(w.UpgradeBlockingReason),
		outboxBlockedText: workerString(w.OutboxBlocked), tokenLabel: workerString(w.AnthropicSecretLabel),
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

func workersTickInterval(failures int) time.Duration {
	return min(60*time.Second, 5*time.Second<<uint(min(4, max(0, failures))))
}
func workersTickAfter(d time.Duration, gen uint64) tea.Cmd {
	return tea.Tick(d, func(time.Time) tea.Msg { return workersTickMsg{gen} })
}
func (m tuiModel) workersVisible() bool {
	if m.showHelp || m.quitting || m.updatePrompt.showing {
		return false
	}
	return m.view == viewBoard || m.view == viewWorkers || m.splitDrawn()
}

// Every update and key path reconciles visibility. Departure invalidates pending
// ticks; entry supersedes any request left outstanding while hidden.
func (m tuiModel) reconcileWorkers(cmd tea.Cmd) (tea.Model, tea.Cmd) {
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
	if m.workers.admin != m.board.admin {
		m.workers.rows, m.workers.loaded = nil, false
		m.workers.admin = m.board.admin
		m.workers.cursor, m.workers.scroll, m.workers.errStreak = 0, 0, 0
		m.workers.selectedID = ""
		m.workers.err = nil
	}
	if !m.workersVisible() {
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
	if !m.workersVisible() {
		return m, nil
	}
	m.workers.tickGen++
	return m, workersTickAfter(workersTickInterval(m.workers.errStreak), m.workers.tickGen)
}
func workerState(r workerRow) string {
	w := r.w
	switch {
	case w.Status != "online":
		return "offline"
	case w.RetainingUnpublishedWork:
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
	if w.RetainingUnpublishedWork {
		add(1, "⚑ unpublished work", "⚑ holding unpublished work; not spare capacity")
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
		add(2, text, text+" · idle, held for follow-up")
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
	rows := make([]workerRow, 0, len(s.rows))
	for _, r := range s.rows {
		if strings.Contains(strings.ToLower(cellText(r.w.Name)), strings.ToLower(s.filter)) {
			rows = append(rows, r)
		}
	}
	slices.SortStableFunc(rows, func(a, b workerRow) int {
		if d := workerSeverity(a, now) - workerSeverity(b, now); d != 0 {
			return d
		}
		if d := strings.Compare(cellText(a.w.Name), cellText(b.w.Name)); d != 0 {
			return d
		}
		return strings.Compare(a.w.ID, b.w.ID)
	})
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
func (m tuiModel) workersSummary(width int, floor bool) string {
	if m.workers.admin != m.board.admin || len(m.workers.rows) == 0 {
		return ""
	}
	n, online, used, cap, unknown, attention, holding, draining, cordoned := len(m.workers.rows), 0, 0, 0, 0, 0, 0, 0, 0
	now := time.Now()
	for _, r := range m.workers.rows {
		w := r.w
		if w.Status == "online" {
			online++
			used += w.ActiveRuns
			if w.MaxConcurrentRuns == nil {
				unknown++
			} else {
				cap += *w.MaxConcurrentRuns
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
	slots := fmt.Sprintf("%d/%d slots in use", used, cap)
	if floor {
		summary := fmt.Sprintf("workers %s · %d/%d online · %d need attention · 2 for detail", slots, online, n, attention)
		if visualWidth(summary) > width {
			summary = fmt.Sprintf("workers %s · %d need attention · 2 for detail", slots, attention)
		}
		return clampVisual(summary, width)
	}
	scope := "your workers"
	if m.board.admin {
		scope = "factory workers"
	}
	build := func(unk, admission, on bool) string {
		seg := []string{scope, itoa(n)}
		if on {
			seg = append(seg, fmt.Sprintf("%d online", online))
		}
		occ := slots
		if unk && unknown > 0 {
			occ += fmt.Sprintf(" +%d ?cap", unknown)
		}
		seg = append(seg, occ)
		if admission {
			for _, c := range []struct {
				n     int
				label string
			}{{holding, "holding"}, {draining, "draining"}, {cordoned, "cordoned"}} {
				if c.n > 0 {
					seg = append(seg, fmt.Sprintf("%d %s", c.n, c.label))
				}
			}
		}
		seg = append(seg, fmt.Sprintf("%d need attention", attention))
		return strings.Join(seg, " · ")
	}
	for _, opts := range [][3]bool{{true, true, true}, {false, true, true}, {false, false, true}, {false, false, false}} {
		s := build(opts[0], opts[1], opts[2])
		if visualWidth(s) <= width {
			return s
		}
	}
	return clampVisual(build(false, false, false), width)
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
	switch t.hostedSize {
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
func (m tuiModel) workerRowLine(r workerRow, selected bool, width int) string {
	t := workerTextOf(r)
	now := time.Now()
	pre := " "
	if selected {
		pre = "▌"
	}
	state := workerState(r)
	fields := []string{pre, padVisual(clampVisual(m.renderer.Plain(t.workerName, 13), 13), 13)}
	if width >= 120 && m.board.admin {
		fields = append(fields, padVisual(clampVisual(m.renderer.Plain(t.workerOwner, 10), 10), 10))
	}
	fields = append(fields, padVisual(workerStateGlyph(state)+" "+state, 10), padVisual(m.workerKind(r), 9), padVisual(workerSlots(r.w), 5))
	if width >= 120 {
		cpu, mem, disk := "?", "?", "?"
		if r.w.StatsCPUPct != nil {
			cpu = fmt.Sprintf("%.0f%%", *r.w.StatsCPUPct)
		}
		if r.w.StatsMemBytes != nil {
			mem = fmt.Sprintf("%.1fG", float64(*r.w.StatsMemBytes)/(1<<30))
		}
		worst := -1.0
		for _, d := range workerDisks(r.w) {
			if d.pct > worst {
				worst = d.pct
				disk = fmt.Sprintf("%s %.0f%%", d.label, d.pct)
			}
		}
		if state == "offline" {
			cpu = "~" + cpu
			mem = "~" + mem
			disk = "~" + disk
			cpu = m.pal.faint.Render(cpu)
			mem = m.pal.faint.Render(mem)
			disk = m.pal.faint.Render(disk)
		}
		ver := m.renderer.Plain(t.workerVersion, 8)
		switch r.w.UpgradeStatus {
		case "outdated":
			ver += "↑"
		case "upgrade_failed":
			ver += "✕"
		}
		fields = append(fields, padVisual(cpu, 5), padVisual(mem, 6), padVisual(disk, 15), padVisual(ver, 9), padVisual(workerAge(r.w.LastHeartbeatAt, now), 4))
	}
	items := workerAttention(r, now)
	att := "—"
	if len(items) > 0 {
		suffix := ""
		if len(items) > 1 {
			suffix = fmt.Sprintf(" +%d", len(items)-1)
		}
		budget := max(0, width-visualWidth(strings.Join(fields, " "))-1-visualWidth(suffix))
		att = clampVisual(m.renderer.Plain(items[0].attnShort, budget), budget) + suffix
	}
	fields = append(fields, att)
	return clampVisual(strings.Join(fields, " "), width)
}
func (m tuiModel) workerReadout(r workerRow, width int) []string {
	t := workerTextOf(r)
	lines := []string{clampVisual("selected "+m.renderer.Plain(t.workerName, width-9), width)}
	if m.board.admin {
		lines = append(lines, clampVisual("owner "+m.renderer.Plain(t.workerOwner, width-6), width))
	}
	for _, item := range workerAttention(r, time.Now()) {
		lines = append(lines, clampVisual(m.renderer.Plain(item.attnDetail, width), width))
	}
	return lines
}
func (m tuiModel) renderWorkers() string { return m.renderWorkersBody(m.height, true) }
func (m tuiModel) renderWorkersBody(height int, full bool) string {
	if height <= 0 {
		return ""
	}
	width := m.width
	if !full {
		width = min(80, width)
	}
	var lines []string
	if full {
		lines = append(lines, clampVisual(" "+m.tabStrip(m.board.admin, viewWorkers, false), width))
		if summary := m.workersSummary(width, false); summary != "" {
			lines = append(lines, summary)
		}
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
	header := "  NAME          STATE      KIND      RUNS  ATTENTION"
	if width >= 120 {
		header = "  NAME          STATE      KIND      RUNS  CPU   MEM    DISK (worst)    VERSION   HB   ATTENTION"
		if m.board.admin {
			header = "  NAME          OWNER      STATE      KIND      RUNS  CPU   MEM    DISK (worst)    VERSION   HB   ATTENTION"
		}
	}
	lines = append(lines, clampVisual(header, width))
	rows := m.workers.visible(time.Now())
	if m.workers.admin != m.board.admin {
		rows = nil
	}
	cursor := m.workers.selectedIndex(rows)
	var readout []string
	if len(rows) > 0 {
		readout = m.workerReadout(rows[cursor], width)
	}
	footer := "j/k move · / filter · a scope · r refresh · 1-4 tabs · ? keys · q quit"
	reserve := 0
	if full {
		reserve = 1
	}
	// Bound the readout before allocating list rows, retaining the selected row
	// and footer even at tiny heights. Each failure is independent of other rows.
	readout = readout[:min(len(readout), max(0, height-len(lines)-reserve-1))]
	capacity := max(0, height-len(lines)-len(readout)-reserve)
	start := min(m.workers.scroll, max(0, len(rows)-capacity))
	if cursor < start {
		start = cursor
	}
	if cursor >= start+capacity {
		start = max(0, cursor-capacity+1)
	}
	for i := start; i < min(len(rows), start+capacity); i++ {
		lines = append(lines, m.workerRowLine(rows[i], m.view == viewWorkers && i == cursor, width))
	}
	if len(rows) == 0 && capacity > 0 {
		empty := "no workers"
		if !m.workers.loaded {
			empty = "loading workers…"
		}
		lines = append(lines, empty)
	}
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
