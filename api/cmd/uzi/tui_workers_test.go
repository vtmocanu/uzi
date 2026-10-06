package main

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func TestWorkersDemoFleet(t *testing.T) {
	f := newDemoClient()
	held, healthy := f.Workers[6], f.Workers[2]
	if !held.RetainingUnpublishedWork || held.CustodyDecisionsNeeded == nil || *held.CustodyDecisionsNeeded != 1 {
		t.Fatal("demo held worker needs retained source and one owner decision")
	}
	if !healthy.RetainingUnpublishedWork || healthy.CustodyDecisionsNeeded == nil || *healthy.CustodyDecisionsNeeded != 0 || !healthy.Busy || healthy.ActiveRuns == 0 {
		t.Fatal("demo healthy busy worker needs retained source and no owner decisions")
	}
	m := workersScene(true, "workers-list-120")
	want := "workers · 9 · 8 online · 5/12 slots in use +1 ?cap · 1 holding · 1 draining · 6 need attention"
	if got := stripANSI(m.workersSummary(120)); got != want {
		t.Fatalf("summary %q", got)
	}
	if title := m.workerFleetTitleLines("▚▚ uzi · floor"); !strings.Contains(stripANSI(title[0]), "need attention") {
		t.Fatal("compact floor title omitted attention", title)
	}
	var names []string
	for _, r := range m.workers.visible(time.Now()) {
		names = append(names, r.w.Name)
	}
	if !reflect.DeepEqual(names, []string{"forge-large", "forge-small", "laptop", "forge-docker", "forge-m-2", "recovery", "chat-box", "eph-b20000", "eph-a10000"}) {
		t.Fatal(names)
	}
	if len(f.Workers) != 9 || len(f.AdminWorkers) != 10 || !reflect.DeepEqual(f.AdminRuns, f.Runs) {
		t.Fatal("own/factory seed mismatch")
	}
	states := map[string]bool{}
	for _, w := range f.AdminWorkers {
		states[workerState(workerRow{w: w.WorkerDTO})] = true
		for _, report := range w.ReportedRuns {
			r, ok := f.RunByID[report.RunID]
			if !ok || r.WorkerID == nil || *r.WorkerID != w.ID || r.WorkerName == nil || *r.WorkerName != w.Name {
				t.Fatalf("inconsistent report %+v", report)
			}
		}
	}
	for _, state := range []string{"offline", "holding", "draining", "cordoned", "busy", "idle"} {
		if !states[state] {
			t.Error("missing state", state)
		}
	}
	for _, r := range f.Runs {
		if r.WorkerID != nil && (r.WorkerName == nil || r.RunDTO.WorkerName == nil || *r.WorkerName != *r.RunDTO.WorkerName) {
			t.Fatal("worker name projection mismatch", r.ID)
		}
	}
	queued := 0
	for _, r := range f.Runs {
		if r.Status == "queued" {
			queued++
			if r.WorkerID != nil || r.WorkerName != nil || r.RunDTO.WorkerName != nil {
				t.Fatal("unclaimed fixture assigned")
			}
		}
	}
	if queued != 1 {
		t.Fatal("missing queued unclaimed fixture")
	}
	b := f.AdminWorkers[9]
	if b.ActiveRuns != 0 || b.DrainingSince == nil || b.StatsDiskDataBytes == nil || *b.StatsDiskDataBytes >= 90 || !reflect.DeepEqual(b.DiskPressureVolumes, []string{"data"}) {
		t.Fatal("admin pressure must be independent of percentage and cordoned")
	}
}

func TestWorkersLegendHelpAndVersionBudget(t *testing.T) {
	m := workersScene(true, "workers-factory")
	r := m.workers.rows[0]
	r.w.Version = sp(strings.Repeat("界", 30))
	r.w.UpgradeStatus = "upgrade_failed"
	r.w.OutboxBlocked = sp("blocked")
	m.workers.rows = []workerRow{r}
	if line := m.workerRowLine(r, true, 120, m.workerTableWidths(m.workers.rows, 120), time.Now()); !strings.Contains(stripANSI(line), "+") || visualWidth(line) > 120 {
		t.Fatalf("attention suffix lost: %s", line)
	}
	line := stripANSI(m.workerRowLine(r, true, 120, m.workerTableWidths(m.workers.rows, 120), time.Now()))
	// Wide glyphs must fit the capped eighteen-column version cell, including its marker.
	versionCell := strings.Repeat("界", 8) + "…✕"
	at := strings.Index(line, versionCell)
	if at < 0 || visualWidth(line[:at]) != 86 || strings.Contains(line, strings.Repeat("界", 9)) {
		t.Fatalf("version cell moved or exceeded its visual budget: %q", line)
	}
	if strings.Contains(stripANSI(m.renderWorkers()), "fixed visual cue") {
		t.Fatal("disk legend should live only in help")
	}
	if !strings.Contains(strings.Join(helpLines(viewWorkers), "\n"), "fixed visual cue") {
		t.Fatal("missing help disk legend")
	}
	help := strings.Join(helpLines(viewWorkers), "\n")
	if !strings.Contains(help, "j / ↓") || !strings.Contains(help, "k / ↑") {
		t.Fatal("movement help omitted")
	}
}

func TestWorkerBusyLeaseReadout(t *testing.T) {
	now := time.Now()
	expires := now.Add(10 * time.Minute)
	m := workersScene(true, "workers-list-80")
	m.workers.rows = []workerRow{{w: apitypes.WorkerDTO{
		ID: "busy-lease", Name: "busy-lease", Status: "online", Busy: true,
		ActiveRuns: 1, Ephemeral: true, EphemeralLeaseExpiresAt: &expires,
	}}}
	frame := stripANSI(m.View().Content)
	if !strings.Contains(frame, "busy") || !strings.Contains(frame, "lease 9m left") || strings.Contains(frame, "idle") {
		t.Fatalf("busy lease readout misstates occupancy: %s", frame)
	}
}

func TestWorkerStatePrecedence(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name string
		w    apitypes.WorkerDTO
		want string
	}{
		{"offline wins", apitypes.WorkerDTO{Status: "offline", RetainingUnpublishedWork: true, CustodyDecisionsNeeded: custodyCount(1), DrainingSince: &now, Busy: true}, "offline"},
		{"holding wins", apitypes.WorkerDTO{Status: "online", RetainingUnpublishedWork: true, CustodyDecisionsNeeded: custodyCount(1), DrainingSince: &now, Busy: true}, "holding"},
		{"draining", apitypes.WorkerDTO{Status: "online", DrainingSince: &now, ActiveRuns: 1, Busy: true}, "draining"},
		{"cordoned", apitypes.WorkerDTO{Status: "online", DrainingSince: &now, Busy: true}, "cordoned"},
		{"chat busy", apitypes.WorkerDTO{Status: "online", Busy: true}, "busy"},
		{"idle", apitypes.WorkerDTO{Status: "online"}, "idle"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := workerState(workerRow{w: tc.w}); got != tc.want {
				t.Fatal(got)
			}
		})
	}
	if got := workerSlots(apitypes.WorkerDTO{ActiveRuns: 2}); got != "2/?" {
		t.Fatal(got)
	}
}

func TestWorkerAttentionFacts(t *testing.T) {
	f := newDemoClient()
	now := time.Now()
	for _, tc := range []struct {
		index int
		wants []string
	}{
		{5, []string{"upgrade failed", "seed-nix", "ImagePullBackOff", "offline", "cordoned"}},
		{7, []string{"outbox blocked", "outdated", "template drift", "chat active"}},
		{1, []string{"outcome pending 12m", "not yet delivered/acknowledged", "14 queued", "dind ino 97% (display-only)"}},
	} {
		items := workerAttention(workerRow{w: f.Workers[tc.index]}, now)
		var text string
		prev := -1
		for _, a := range items {
			if a.severity < prev {
				t.Fatal("attention not worst first")
			}
			prev = a.severity
			text += a.attnDetail + "\n"
		}
		for _, want := range tc.wants {
			if !strings.Contains(text, want) {
				t.Errorf("worker %d lacks %q: %s", tc.index, want, text)
			}
		}
	}
	depth := 14
	w := apitypes.WorkerDTO{Status: "online", OutboxPendingMessages: &depth}
	if a := workerAttention(workerRow{w: w}, now); len(a) != 1 || a[0].severity != 1 || strings.Contains(a[0].attnDetail, "blocked") {
		t.Fatal(a)
	}
	w = apitypes.WorkerDTO{Status: "online", TemplateDeclared: sp("a")}
	if len(workerAttention(workerRow{w: w}, now)) != 0 {
		t.Fatal("one template is not drift")
	}
	w.TemplateReported = sp("a")
	if len(workerAttention(workerRow{w: w}, now)) != 0 {
		t.Fatal("matching template is not drift")
	}
	w.TemplateReported = sp("b")
	if a := workerAttention(workerRow{w: w}, now); len(a) != 1 || a[0].severity != 1 {
		t.Fatal(a)
	}
	w = apitypes.WorkerDTO{Status: "online", StatsDiskDindBytes: ip(91), StatsDiskDindTotalBytes: ip(100), StatsDiskDataInodes: ip(99), StatsDiskDataTotalInodes: ip(100)}
	for _, a := range workerAttention(workerRow{w: w}, now) {
		if a.severity != 1 || !strings.Contains(a.attnDetail, "display-only") {
			t.Fatal(a)
		}
	}
	w = apitypes.WorkerDTO{Status: "online", StatsDiskDataBytes: ip(9), StatsDiskDataTotalBytes: ip(100)}
	a := workerAttention(workerRow{w: w, pressureText: []string{"data"}}, now)
	if len(a) != 1 || a[0].severity != 0 || !strings.Contains(a[0].attnDetail, "server-reported") {
		t.Fatal(a)
	}
}

func TestWorkersScenesContentAndBounds(t *testing.T) {
	for _, name := range workerSceneNames {
		for _, dark := range []bool{true, false} {
			t.Run(name+map[bool]string{true: "/dark", false: "/light"}[dark], func(t *testing.T) {
				m := workersScene(dark, name)
				frame := stripANSI(m.View().Content)
				for _, want := range []string{"workers", "slots in use", "need attention"} {
					if !strings.Contains(frame, want) {
						t.Errorf("missing %q\n%s", want, frame)
					}
				}
				if strings.Contains(name, "factory") || strings.Contains(name, "cordoned") {
					for _, want := range []string{"factory workers", "b-runner", "OWNER"} {
						if !strings.Contains(frame, want) {
							t.Error("missing", want)
						}
					}
				}
				if name == "workers-cordoned" {
					for _, want := range []string{"selected b-runner", "cordoned", "pressure: data", "user-b@example.test"} {
						if !strings.Contains(frame, want) {
							t.Error("missing", want)
						}
					}
				}
				if name == "split-workers-top" && !strings.Contains(frame, "[workers]") {
					t.Error("workers top focus missing")
				}
				lines := strings.Split(frame, "\n")
				if len(lines) > m.height || (name != "floor-fleet" && len(lines) != m.height) {
					t.Fatalf("physical height=%d want %d", len(lines), m.height)
				}
				for i, line := range lines {
					if visualWidth(line) > m.width {
						t.Errorf("line %d width=%d > %d: %s", i, visualWidth(line), m.width, line)
					}
				}
			})
		}
	}
}

func TestWorkersNavigationFilterAndSplit(t *testing.T) {
	if splitMinHeight != 41 {
		t.Fatalf("split minimum %d want 41 (restore at 43)", splitMinHeight)
	}
	m := workersScene(true, "workers-list-80")
	m.splitMode = "off"
	for _, tc := range []struct {
		k string
		v tuiView
	}{{"1", viewBoard}, {"2", viewWorkers}, {"3", viewPulls}, {"4", viewCI}, {"tab", viewBoard}, {"tab", viewWorkers}, {"tab", viewPulls}, {"tab", viewCI}, {"shift+tab", viewPulls}, {"shift+tab", viewWorkers}, {"shift+tab", viewBoard}, {"shift+tab", viewCI}} {
		m = press(t, m, tc.k)
		if m.view != tc.v {
			t.Fatalf("%s view=%v want %v", tc.k, m.view, tc.v)
		}
	}
	m = press(t, m, "2")
	m = press(t, m, "/")
	for _, k := range []string{"F", "O", "R", "G", "E"} {
		m = press(t, m, k)
	}
	if len(m.workers.visible(time.Now())) != 4 {
		t.Fatal("case insensitive filter")
	}
	m = press(t, m, "3")
	if m.view != viewWorkers || m.workers.filter != "FORGE3" {
		t.Fatal("digit escaped filter")
	}
	m = press(t, m, "esc")
	before := m.board.hideDone
	m = press(t, m, "h")
	if m.board.hideDone != before {
		t.Fatal("h changed workers state")
	}
	m.workers.filter = ""
	m.splitMode = "auto"
	m = resizeSplit(m, 120, splitMinHeight+2)
	m = press(t, m, "2")
	m = press(t, m, "ctrl+w")
	if m.view != viewCI || m.top() != viewWorkers {
		t.Fatal("pane focus")
	}
	m = press(t, m, "tab")
	if m.view != viewBoard || m.top() != viewBoard {
		t.Fatal("ci wrap must floor")
	}
	m = press(t, m, "shift+tab")
	if m.view != viewCI {
		t.Fatal("floor backtab must ci")
	}
	m = press(t, m, "2")
	m = press(t, m, "3")
	m = press(t, m, "s")
	if m.view != viewWorkers || m.splitDrawn() {
		t.Fatal("collapse must top")
	}
	m = press(t, m, "s")
	if !m.splitDrawn() || m.top() != viewWorkers || m.bottom() != viewPulls {
		t.Fatal("restore panes")
	}
	for _, h := range []int{splitMinHeight - 1, splitMinHeight, splitMinHeight + 1, splitMinHeight + 2} {
		m = resizeSplit(m, 120, h)
		if m.top() != viewWorkers || m.bottom() != viewPulls {
			t.Fatal("resize lost tabs")
		}
		if h == splitMinHeight-1 && (m.splitDrawn() || m.view != viewWorkers) {
			t.Fatal("resize collapse")
		}
		if h == splitMinHeight+2 && !m.splitDrawn() {
			t.Fatal("resize restore")
		}
	}
}

func TestWorkersPollSelectionAndStaleReplies(t *testing.T) {
	m := workersScene(true, "workers-list-80")
	old := m.workers.visible(time.Now())
	m.workers.cursor = 2
	id := old[2].w.ID
	m.workers.selectedID = id
	rows := append([]workerRow(nil), m.workers.rows...)
	for i := range rows {
		if rows[i].w.ID == id {
			rows[i].w.OutboxBlocked = nil
			rows[i].w.UpgradeStatus = "up_to_date"
			rows[i].w.TemplateReported = rows[i].w.TemplateDeclared
			rows[i].w.Busy = false
		}
	}
	_ = (&m).startWorkersReq()
	m = step(m, workersMsg{reqID: m.workers.waitID, rows: rows})
	if m.workers.visible(time.Now())[m.workers.cursor].w.ID != id {
		t.Fatal("reorder lost selected ID")
	}
	index := m.workers.cursor
	for i := range rows {
		if rows[i].w.ID == id {
			rows = append(rows[:i], rows[i+1:]...)
			break
		}
	}
	_ = (&m).startWorkersReq()
	m = step(m, workersMsg{reqID: m.workers.waitID, rows: rows})
	if m.workers.cursor != min(index, len(rows)-1) {
		t.Fatal("disappearance must retain old index")
	}
	_ = (&m).startWorkersReq()
	ownID := m.workers.waitID
	m = press(t, m, "a")
	factoryID := m.workers.waitID
	m = press(t, m, "a")
	newID := m.workers.waitID
	for _, msg := range []workersMsg{{reqID: ownID}, {reqID: factoryID, admin: true}, {reqID: newID, admin: true}, {reqID: 0}} {
		m = step(m, msg)
		if m.workers.waitID != newID || m.board.admin {
			t.Fatal("stale reply cleared newer own wait")
		}
	}
	next, cmd := m.Update(workersTickMsg{gen: m.workers.tickGen - 1})
	m = next.(tuiModel)
	if cmd != nil || m.workers.waitID != newID {
		t.Fatal("stale tick")
	}
	before := m.workers.waitID
	m = press(t, m, "r")
	if m.workers.waitID <= before {
		t.Fatal("refresh did not supersede")
	}
}

type workersProbeClient struct {
	uzicli.Client
	own, factory int
	contexts     []context.Context
	err          error
	block        bool
}

func (c *workersProbeClient) ListWorkers(ctx context.Context) ([]apitypes.WorkerDTO, error) {
	c.own++
	c.contexts = append(c.contexts, ctx)
	if c.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return []apitypes.WorkerDTO{{ID: "probe", Name: "probe", Status: "online"}}, c.err
}
func (c *workersProbeClient) AdminListWorkers(ctx context.Context) ([]apitypes.AdminWorkerDTO, error) {
	c.factory++
	c.contexts = append(c.contexts, ctx)
	if c.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return nil, c.err
}

func TestWorkersFetchDeadlinesAndBackoff(t *testing.T) {
	c := &workersProbeClient{Client: &uzicli.FakeClient{}}
	m := tuiTestModel(t, c, "")
	for _, admin := range []bool{false, true} {
		before := time.Now()
		msg := m.fetchWorkersCmd(admin, 42)().(workersMsg)
		ctx := c.contexts[len(c.contexts)-1]
		deadline, ok := ctx.Deadline()
		if !ok || deadline.Before(before) || deadline.After(before.Add(workersPollTimeout+time.Second)) || time.Until(deadline) >= 30*time.Second {
			t.Fatal("missing short worker deadline")
		}
		if ctx.Err() != context.Canceled || msg.reqID != 42 || msg.admin != admin {
			t.Fatal("fetch scope, request or cancellation")
		}
	}
	if c.own != 1 || c.factory != 1 {
		t.Fatal("wrong endpoint")
	}
	orig := workersPollTimeout
	workersPollTimeout = 2 * time.Millisecond
	t.Cleanup(func() { workersPollTimeout = orig })
	c.block = true
	for _, admin := range []bool{false, true} {
		if msg := m.fetchWorkersCmd(admin, 43)().(workersMsg); !errors.Is(msg.err, context.DeadlineExceeded) {
			t.Fatal("fetch did not enforce timeout", msg.err)
		}
	}
	for i, want := range []time.Duration{5, 10, 20, 40, 60, 60} {
		if got := workersTickInterval(i); got != want*time.Second {
			t.Fatalf("backoff %d=%v", i, got)
		}
	}
}

func TestWorkersVisibilityAndHiddenReply(t *testing.T) {
	c := &workersProbeClient{Client: &uzicli.FakeClient{}}
	m := tuiTestModel(t, c, "")
	if !m.workers.active || m.workers.waitID == 0 {
		t.Fatal("initial floor did not request workers")
	}
	m = step(m, m.fetchWorkersCmd(false, m.workers.waitID)())
	if c.own != 1 {
		t.Fatal("initial floor fetch", c.own)
	}
	if start := tuiTestModel(t, c, "run"); start.workers.active || start.workers.waitID != 0 {
		t.Fatal("startRun workers active")
	}
	m.splitMode = "off"
	m = press(t, m, "4")
	if m.workers.active || m.workers.waitID != 0 {
		t.Fatal("fullscreen forge workers active")
	}
	m = resizeSplit(m, 120, 60)
	m.splitMode = "auto"
	m = step(m, tea.WindowSizeMsg{Width: 120, Height: 60})
	if !m.workersVisible() || !m.workers.active {
		t.Fatal("unfocused top must poll")
	}
	m = press(t, m, "2")
	for _, overlay := range []string{"help", "quit", "update"} {
		t.Run(overlay, func(t *testing.T) {
			local := m
			id := local.workers.waitID
			if id == 0 {
				_ = (&local).startWorkersReq()
				id = local.workers.waitID
			}
			switch overlay {
			case "help":
				local.showHelp = true
			case "quit":
				local.quitting = true
			case "update":
				local.updatePrompt.showing = true
			}
			next, cmd := local.Update(workersMsg{reqID: id, rows: []workerRow{{w: apitypes.WorkerDTO{ID: "hidden", Status: "online"}}}})
			local = next.(tuiModel)
			if cmd != nil || local.workers.active {
				t.Fatal("hidden reply rearmed")
			}
			local.showHelp, local.quitting, local.updatePrompt.showing = false, false, false
			local = step(local, tea.WindowSizeMsg{Width: 120, Height: 60})
			if !local.workers.active || local.workers.waitID <= id {
				t.Fatal("reentry did not fetch")
			}
		})
	}
}

func TestWorkersHelpEntryPreservesHiddenReply(t *testing.T) {
	for _, replyHidden := range []bool{true, false} {
		t.Run(map[bool]string{true: "reply while hidden", false: "dismiss before reply"}[replyHidden], func(t *testing.T) {
			m := tuiTestModel(t, &uzicli.FakeClient{}, "")
			m.splitMode = "off"
			m = press(t, m, "2")
			id, gen := m.workers.waitID, m.workers.tickGen
			m = press(t, m, "?")
			if !m.showHelp || m.workers.active || m.workers.waitID != id || m.workers.tickGen <= gen {
				t.Fatal("help entry did not preserve request and invalidate ticks")
			}
			reply := workersMsg{reqID: id, rows: []workerRow{{w: apitypes.WorkerDTO{ID: "hidden", Name: "hidden", Status: "online"}}}}
			if replyHidden {
				next, cmd := m.Update(reply)
				m = next.(tuiModel)
				if cmd != nil || m.workers.active || m.workers.waitID != 0 || !m.workers.loaded || len(m.workers.rows) != 1 || m.workers.rows[0].w.ID != "hidden" {
					t.Fatal("hidden snapshot was dropped or polling rearmed")
				}
			}
			next, cmd := m.Update(workersTickMsg{gen: gen})
			m = next.(tuiModel)
			if cmd != nil {
				t.Fatal("pre-help tick restarted polling")
			}
			m = press(t, m, "esc")
			newID := m.workers.waitID
			if m.showHelp || !m.workers.active || newID <= id {
				t.Fatal("help dismissal did not supersede request")
			}
			before := append([]workerRow(nil), m.workers.rows...)
			m = step(m, reply)
			if m.workers.waitID != newID || !reflect.DeepEqual(m.workers.rows, before) {
				t.Fatal("late hidden reply overwrote dismissal request")
			}
		})
	}
}

func TestWorkersSelectionSurvivesLeaseExpiryOrder(t *testing.T) {
	m := tuiTestModel(t, &uzicli.FakeClient{}, "")
	m.splitMode = "off"
	m = press(t, m, "2")
	expired := time.Now().Add(-time.Hour)
	rows := []workerRow{
		{w: apitypes.WorkerDTO{ID: "a", Name: "alpha", Status: "online"}},
		{w: apitypes.WorkerDTO{ID: "z", Name: "zulu", Status: "online", Ephemeral: true, EphemeralLeaseExpiresAt: &expired}},
	}
	m.workers.rows, m.workers.loaded = rows, true
	// Before the lease expired, zulu's info item placed it ahead of alpha.
	if earlier := m.workers.visible(expired.Add(-time.Minute)); earlier[0].w.ID != "z" {
		t.Fatal("fixture did not change order when its lease expired")
	}
	m.workers.cursor, m.workers.selectedID = 0, "z"
	frame := stripANSI(m.renderWorkers())
	if !strings.Contains(frame, "selected zulu") || !strings.Contains(frame, "▌ zulu") {
		t.Fatalf("time-dependent reorder moved rendered selection:\n%s", frame)
	}
	m = step(m, workersMsg{reqID: m.workers.waitID, rows: rows})
	if m.workers.cursor != 1 || m.workers.selectedID != "z" {
		t.Fatal("reply lost selection after lease expiry")
	}
	m = press(t, m, "k")
	if m.workers.selectedID != "a" || !strings.Contains(stripANSI(m.renderWorkers()), "selected alpha") {
		t.Fatal("motion used stale index after lease expiry")
	}
}

func TestWorkersUnicodeLineBounds(t *testing.T) {
	for _, scene := range []string{"workers-list-80", "workers-factory", "split-workers-top"} {
		t.Run(scene, func(t *testing.T) {
			m := workersScene(true, scene)
			text := strings.Repeat("界😀e\u0301", 100)
			m.workers.rows = []workerRow{{w: apitypes.WorkerDTO{
				ID: "unicode", Name: text, Status: "online", Version: sp(text),
				OutboxBlocked: sp(text), UpgradeStatus: "upgrade_failed",
				UpgradeBlockingContainer: sp(text), UpgradeBlockingReason: sp(text),
				UpgradeDetail: sp(text), UpgradeTarget: text,
			}, workerOwner: text, pressureText: []string{text}}}
			m.workers.cursor, m.workers.selectedID = 0, "unicode"
			frame := stripANSI(m.View().Content)
			if !strings.Contains(frame, "界😀e\u0301") || strings.ContainsRune(frame, '\uFFFD') {
				t.Fatalf("Unicode was lost or corrupted:\n%s", frame)
			}
			lines := strings.Split(frame, "\n")
			if len(lines) != m.height {
				t.Fatalf("height %d want %d", len(lines), m.height)
			}
			for i, line := range lines {
				if visualWidth(line) > m.width {
					t.Errorf("line %d width %d > %d: %s", i, visualWidth(line), m.width, line)
				}
			}
		})
	}
}

func TestWorkersAdminDenialFallsBackBothScopes(t *testing.T) {
	for _, source := range []string{"workers", "board"} {
		t.Run(source, func(t *testing.T) {
			c := &workersProbeClient{Client: &uzicli.FakeClient{}, err: uzicli.Exitf(uzicli.ExitAuth, "admin access required")}
			m := tuiTestModel(t, c, "")
			m = press(t, m, "2")
			m = press(t, m, "a")
			if !m.board.admin || !m.workers.admin || m.workers.waitID == 0 || m.board.waitID == 0 {
				t.Fatal("shared factory scope not requested")
			}
			deniedID := m.workers.waitID
			if source == "workers" {
				m = step(m, m.fetchWorkersCmd(true, deniedID)())
				if c.factory != 1 {
					t.Fatal("admin worker endpoint not used")
				}
			} else {
				m = step(m, boardRunsMsg{reqID: m.board.waitID, admin: true, err: c.err})
			}
			if m.board.admin || m.workers.admin || !m.board.adminDenied || m.workers.waitID <= deniedID || m.board.waitID == 0 {
				t.Fatal("denial did not refetch both own scopes")
			}
			c.err = nil
			m = step(m, m.fetchWorkersCmd(false, m.workers.waitID)())
			if c.own != 1 || len(m.workers.rows) != 1 || m.workers.rows[0].w.ID != "probe" {
				t.Fatal("own worker fallback failed")
			}
			if !strings.Contains(stripANSI(m.renderWorkers()), "admin (uza_) token") {
				t.Fatal("denial note missing")
			}
		})
	}
}

func TestWorkersPollingOneChain(t *testing.T) {
	c := &workersProbeClient{Client: &uzicli.FakeClient{}}
	m := tuiTestModel(t, c, "")
	id := m.workers.waitID
	next, cmd := m.Update(workersTickMsg{gen: m.workers.tickGen})
	m = next.(tuiModel)
	if cmd != nil || m.workers.waitID != id {
		t.Fatal("in-flight tick started duplicate")
	}
	m = step(m, m.fetchWorkersCmd(false, id)())
	next, cmd = m.Update(workersTickMsg{gen: m.workers.tickGen})
	m = next.(tuiModel)
	if cmd == nil || m.workers.waitID <= id {
		t.Fatal("accepted tick did not request")
	}
	m = step(m, cmd())
	if c.own != 2 {
		t.Fatal("tick did not call endpoint once", c.own)
	}
	id = m.workers.reqSeq
	m = press(t, m, "r")
	if m.workers.waitID <= id {
		t.Fatal("r failed to supersede chain")
	}
	m = step(m, workersMsg{reqID: m.workers.waitID, err: errors.New("temporarily unavailable")})
	if m.workers.errStreak != 1 || m.workers.err == nil || len(m.workers.rows) != 1 {
		t.Fatal("failure lost rows or backoff")
	}
	_ = (&m).startWorkersReq()
	m = step(m, m.fetchWorkersCmd(false, m.workers.waitID)())
	if m.workers.errStreak != 0 || m.workers.err != nil {
		t.Fatal("success did not reset backoff")
	}
}

func TestWorkerSortSeverityNameAndIDTies(t *testing.T) {
	m := tuiTestModel(t, &uzicli.FakeClient{}, "")
	m.workers.rows = []workerRow{
		{w: apitypes.WorkerDTO{ID: "b", Name: "equal", Status: "online"}},
		{w: apitypes.WorkerDTO{ID: "a", Name: "equal", Status: "online"}},
		{w: apitypes.WorkerDTO{ID: "info", Name: "aaa", Status: "online", Busy: true}},
		{w: apitypes.WorkerDTO{ID: "warn", Name: "zzz", Status: "offline"}},
		{w: apitypes.WorkerDTO{ID: "danger", Name: "zzz", Status: "online", OutboxBlocked: sp("refused")}},
	}
	var ids []string
	for _, r := range m.workers.visible(time.Now()) {
		ids = append(ids, r.w.ID)
	}
	if !reflect.DeepEqual(ids, []string{"danger", "warn", "info", "a", "b"}) {
		t.Fatal(ids)
	}
	if !strings.Contains(m.workersSummary(120), "2 need attention") {
		t.Fatal("info counted as attention")
	}
}

func TestWorkersHostileReadoutAndASCII(t *testing.T) {
	m := workersScene(true, "workers-factory")
	hostile := "safe\n\r\t" + "\x1b[31m" + "hostile" + "\x1b]0;title\x07"
	w := apitypes.WorkerDTO{ID: "hostile", Name: hostile, Status: "offline", Version: sp(hostile), UpgradeStatus: "upgrade_failed", UpgradeBlockingReason: sp(hostile), UpgradeBlockingContainer: sp(hostile), UpgradeDetail: sp(hostile), UpgradeTarget: hostile, OutboxBlocked: sp(hostile), AnthropicSecretLabel: sp(hostile), TemplateDeclared: sp(hostile), TemplateReported: sp("other")}
	r := workerRow{w: w, workerOwner: hostile, pressureText: []string{hostile}}
	m.workers.rows = []workerRow{r}
	m.workers.cursor = 0
	m = step(m, tea.ColorProfileMsg{Profile: colorprofile.Ascii})
	for _, width := range []int{80, 119, 120} {
		m.width = width
		frame := stripANSI(m.renderWorkers())
		if strings.Contains(frame, "\r") || strings.Contains(frame, "\t") || strings.Contains(frame, "\x1b") || strings.Contains(frame, "\x07") {
			t.Fatal("hostile control escaped")
		}
		if len(strings.Split(frame, "\n")) != m.height {
			t.Fatal("hostile newline escaped")
		}
		for _, line := range strings.Split(frame, "\n") {
			if visualWidth(line) > width {
				t.Fatal("hostile overflow")
			}
		}
		for _, want := range []string{"offline", "upgrade failed", "outbox blocked", "owner"} {
			if !strings.Contains(frame, want) {
				t.Error("ascii fallback missing", want)
			}
		}
		if width >= 120 && (!strings.Contains(frame, "~?") || !strings.Contains(frame, "✕")) {
			t.Fatal("stale/version markers missing")
		}
	}
}

func TestWorkersSummaryDropsSegmentsInPriorityOrder(t *testing.T) {
	for _, tc := range []struct {
		name      string
		used, cap int
		want      string
	}{
		{"unknown cap first", 1, 2, "workers · 2 · 2 online · 1/2 slots in use · 1 holding · 1 need attention"},
		{"admission next", 12345, 12345, "workers · 2 · 2 online · 12345/12345 slots in use · 1 need attention"},
		{"online last", 1234567890, 1234567890, "workers · 1234567890/1234567890 slots in use · 1 need attention"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := workersScene(true, "workers-list-80")
			m.workers.rows = []workerRow{
				{w: apitypes.WorkerDTO{ID: "held", Status: "online", ActiveRuns: tc.used, MaxConcurrentRuns: &tc.cap, RetainingUnpublishedWork: true, CustodyDecisionsNeeded: custodyCount(1)}},
				{w: apitypes.WorkerDTO{ID: "unknown", Status: "online"}},
			}
			if wide := m.workersSummary(160); !strings.Contains(wide, "+1 ?cap") || !strings.Contains(wide, "1 holding") || !strings.Contains(wide, "2 online") {
				t.Fatalf("fixture lacks optional segments: %q", wide)
			}
			if got := stripANSI(m.workersSummary(visualWidth(tc.want))); got != tc.want || visualWidth(got) > visualWidth(tc.want) {
				t.Fatalf("narrowed summary = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestWorkersHelpEveryListLayout(t *testing.T) {
	for _, split := range []bool{false, true} {
		for _, key := range []string{"1", "2", "3", "4"} {
			t.Run(map[bool]string{false: "full", true: "split"}[split]+"/"+key, func(t *testing.T) {
				m := workersScene(true, "workers-list-80")
				m.splitMode = "off"
				if split {
					m.splitMode = "auto"
					m = resizeSplit(m, 80, splitMinHeight+2)
				}
				m = press(t, m, key)
				m = press(t, m, "?")
				if !m.showHelp {
					t.Fatal("help key did not open overlay")
				}
				frame := stripANSI(m.View().Content)
				wants := []string{"j / ↓", "k / ↑", "/          filter", "r          refresh", "?          this help", "any key returns"}
				if split {
					wants = append(wants, "tab        cycle floor, workers, pulls, ci", "1/2 top floor/workers · 3/4 bottom pulls/ci",
						"shift+tab  cycle ci, pulls, workers, floor", "ctrl+w     switch pane focus", "s          collapse / restore split")
				} else {
					wants = append(wants, "tab / shift+tab  floor · workers · pulls · ci", "1 / 2 / 3 / 4  floor / workers / pulls / ci")
					if strings.Contains(frame, "ctrl+w") || strings.Contains(frame, "collapse / restore split") {
						t.Fatal("full-screen help advertises split keys")
					}
				}
				switch key {
				case "1":
					wants = append(wants, "h          hide finished runs")
				case "2":
					wants = append(wants, "a          toggle your workers / factory workers", "occupancy / advertised slots", "chat excluded")
				case "3":
					wants = append(wants, "open the selected PR", "u          open the PR's linked uzi run")
				case "4":
					wants = append(wants, "open the selected run's jobs")
				}
				for _, want := range wants {
					if !strings.Contains(frame, want) {
						t.Errorf("help lacks %q: %s", want, frame)
					}
				}
				for _, line := range strings.Split(frame, "\n") {
					if visualWidth(line) > 80 {
						t.Errorf("help line exceeds 80 columns: %q", line)
					}
				}
			})
		}
	}
}

func TestWorkersDrillReturnPollingAndTopTab(t *testing.T) {
	for _, origin := range []struct {
		name, key    string
		list, detail tuiView
	}{
		{"run", "1", viewBoard, viewDetail},
		{"pr", "3", viewPulls, viewPR},
		{"ci", "4", viewCI, viewCIRun},
	} {
		for _, collapse := range []bool{false, true} {
			t.Run(origin.name+map[bool]string{false: "/split", true: "/resize"}[collapse], func(t *testing.T) {
				c := &workersProbeClient{Client: &uzicli.FakeClient{}}
				m := tuiTestModel(t, c, "")
				m = resizeSplit(m, 120, splitMinHeight+2)
				m.repos, m.reposLoaded, m.repoChosen = []apitypes.RepoDTO{oneRepo()}, true, true
				m.board.runs = []apitypes.RunListItemDTO{
					{RunDTO: apitypes.RunDTO{ID: "first", Kind: "issue", Status: "running"}},
					{RunDTO: apitypes.RunDTO{ID: "second", Kind: "issue", Status: "running"}},
				}
				m.pulls.pulls = samplePulls(time.Now())
				m.ci.runs = sampleCIRuns(time.Now())
				m = press(t, m, "2")
				m = step(m, m.fetchWorkersCmd(false, m.workers.waitID)())
				m = press(t, m, origin.key)
				// The run originates on the floor; bottom drill-ins retain workers on top.
				top := viewWorkers
				if origin.list == viewBoard {
					top = viewBoard
				}
				m = press(t, m, "j")
				id, gen := m.workers.reqSeq, m.workers.tickGen
				m = press(t, m, "enter")
				if m.view != origin.detail || !m.fromSplit || m.workers.active || m.workers.tickGen <= gen || m.top() != top {
					t.Fatalf("drill-in did not suspend workers or retain top: view=%v active=%v top=%v", m.view, m.workers.active, m.top())
				}
				next, cmd := m.Update(workersTickMsg{gen: gen})
				m = next.(tuiModel)
				if cmd != nil || m.workers.reqSeq != id {
					t.Fatal("pre-drill tick fetched while hidden")
				}
				next, cmd = m.Update(workersTickMsg{gen: m.workers.tickGen})
				m = next.(tuiModel)
				if cmd != nil || m.workers.reqSeq != id {
					t.Fatal("hidden current tick fetched")
				}
				if collapse {
					m = resizeSplit(m, 120, splitMinHeight-1)
					if m.workers.active || m.top() != top {
						t.Fatal("resize in detail restarted polling or lost top")
					}
				}
				next, cmd = m.handleKey("esc")
				m = next.(tuiModel)
				want := origin.list
				if collapse {
					want = top
				}
				if m.view != want || m.top() != top || m.fromSplit || !m.workers.active || m.workers.waitID <= id || cmd == nil {
					t.Fatalf("return did not restore view and fresh request: view=%v top=%v active=%v wait=%d", m.view, m.top(), m.workers.active, m.workers.waitID)
				}
				if origin.list == viewPulls && (m.bottom() != viewPulls || m.pulls.cursor != 1) {
					t.Fatal("PR return lost bottom tab or selection")
				}
				if origin.list == viewCI && (m.bottom() != viewCI || m.ci.cursor != 1) {
					t.Fatal("CI return lost bottom tab or selection")
				}
				if origin.list == viewBoard && m.board.cursor != 1 {
					t.Fatal("run return lost floor selection")
				}
				// Execute only the finite return command batch (board refresh and workers
				// fetch); no tick or stream command is fed back into this queue.
				pending := []tea.Cmd{cmd}
				var reply *workersMsg
				before := c.own
				for attempts := 0; len(pending) > 0 && attempts < 8; attempts++ {
					current := pending[0]
					pending = pending[1:]
					if current == nil {
						continue
					}
					switch msg := current().(type) {
					case tea.BatchMsg:
						pending = append(pending, msg...)
					case workersMsg:
						if reply != nil {
							t.Fatal("return scheduled duplicate worker fetches")
						}
						reply = &msg
					}
				}
				if len(pending) != 0 || reply == nil || reply.reqID != m.workers.waitID || c.own != before+1 {
					t.Fatal("return command did not immediately fetch workers exactly once")
				}
				m = step(m, *reply)
				next, cmd = m.Update(workersTickMsg{gen: gen})
				m = next.(tuiModel)
				if cmd != nil || m.workers.waitID != 0 {
					t.Fatal("pre-drill tick survived return")
				}
				if collapse {
					m = resizeSplit(m, 120, splitMinHeight+2)
					if !m.splitDrawn() || m.view != top || m.top() != top || (origin.list != viewBoard && m.bottom() != origin.list) {
						t.Fatal("regrow lost restored panes or top focus")
					}
				} else if !m.splitDrawn() {
					t.Fatal("return failed to draw split")
				}
			})
		}
	}
}

func TestWorkersReplySchedulesBackoffDelay(t *testing.T) {
	original := workersPollInterval
	workersPollInterval = 5 * time.Millisecond
	t.Cleanup(func() { workersPollInterval = original })
	m := tuiTestModel(t, &uzicli.FakeClient{}, "")
	for _, tc := range []struct {
		name   string
		err    error
		delay  time.Duration
		streak int
	}{
		{"first failure", errors.New("unavailable"), 2 * workersPollInterval, 1},
		{"second failure", errors.New("unavailable"), 4 * workersPollInterval, 2},
		{"recovery", nil, workersPollInterval, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m = press(t, m, "r")
			start := time.Now()
			next, cmd := m.Update(workersMsg{reqID: m.workers.waitID, err: tc.err})
			m = next.(tuiModel)
			if cmd == nil || m.workers.waitID != 0 || m.workers.errStreak != tc.streak {
				t.Fatal("reply failed to schedule the next poll")
			}
			// Run the actual reply command, not the interval helper. Its timer is
			// bounded by the specified delay; failure here does not skip siblings.
			msg := cmd()
			elapsed := time.Since(start)
			tick, ok := msg.(workersTickMsg)
			if workersTickInterval(m.workers.errStreak) != tc.delay {
				t.Fatalf("backoff ratio changed: streak=%d delay=%v", m.workers.errStreak, workersTickInterval(m.workers.errStreak))
			}
			if !ok || tick.gen != m.workers.tickGen || elapsed < tc.delay-workersPollInterval/2 || elapsed > tc.delay+time.Second {
				t.Fatalf("reply scheduled %T after %v, want workers tick after %v (gen %d)", msg, elapsed, tc.delay, m.workers.tickGen)
			}
			next, fetch := m.Update(tick)
			m = next.(tuiModel)
			if fetch == nil || m.workers.waitID == 0 {
				t.Fatal("reply-scheduled tick did not request workers")
			}
		})
	}
}

func TestWorkersFilteredSelectionAcrossPolls(t *testing.T) {
	m := tuiTestModel(t, &uzicli.FakeClient{}, "")
	m.splitMode = "off"
	m = press(t, m, "2")
	rows := []workerRow{
		{w: apitypes.WorkerDTO{ID: "a", Name: "forge-alpha", Status: "online"}},
		{w: apitypes.WorkerDTO{ID: "b", Name: "forge-beta", Status: "online"}},
		{w: apitypes.WorkerDTO{ID: "excluded", Name: "laptop", Status: "offline"}},
	}
	m = step(m, workersMsg{reqID: m.workers.waitID, rows: rows})
	m = press(t, m, "/")
	for _, key := range []string{"F", "O", "R", "G", "E", "enter", "j"} {
		m = press(t, m, key)
	}
	if m.workers.selectedID != "b" || m.workers.cursor != 1 {
		t.Fatal("filter did not select beta")
	}
	rows[1].w.OutboxBlocked = sp("blocked")
	m = press(t, m, "r")
	m = step(m, workersMsg{reqID: m.workers.waitID, rows: rows})
	if m.workers.filter != "FORGE" || m.workers.selectedID != "b" || m.workers.cursor != 0 {
		t.Fatal("filtered reorder lost selection")
	}
	if frame := stripANSI(m.View().Content); !strings.Contains(frame, "selected forge-beta") || strings.Contains(frame, "laptop") {
		t.Fatalf("filtered selection rendered incorrectly: %s", frame)
	}
	rows = []workerRow{rows[0], rows[2]}
	m = press(t, m, "r")
	m = step(m, workersMsg{reqID: m.workers.waitID, rows: rows})
	if m.workers.filter != "FORGE" || m.workers.selectedID != "a" || m.workers.cursor != 0 {
		t.Fatal("disappearance did not select remaining filtered row")
	}
}

func TestWorkersZeroSummaryAndMaximumAttentionBounds(t *testing.T) {
	m := workersScene(true, "workers-list-80")
	r := m.workers.rows[0]
	r.w.OutboxBlocked = sp(strings.Repeat("blocked ", 100))
	r.w.UpgradeStatus = "upgrade_failed"
	r.w.RetainingUnpublishedWork, r.w.CustodyDecisionsNeeded = true, custodyCount(1)
	r.w.Status = "offline"
	r.w.DrainingSince = tp(time.Now())
	for i := 0; i < 100; i++ {
		r.w.ReportedRuns = append(r.w.ReportedRuns, apitypes.WorkerReportedRunDTO{TerminalPending: true})
	}
	m.workers.rows = []workerRow{r}
	for _, h := range []int{1, 2, 5, 34} {
		m.height = h
		frame := m.renderWorkers()
		if len(strings.Split(frame, "\n")) != h {
			t.Fatal("max attention height", h)
		}
		for _, line := range strings.Split(frame, "\n") {
			if visualWidth(line) > 80 {
				t.Fatal("max attention width")
			}
		}
	}
	m.workers.rows = nil
	if m.workersSummary(80) != "" {
		t.Fatal("empty summary charged")
	}
	if title := m.workerFleetTitleLines("workers"); len(title) != 1 || title[0] != "workers" {
		t.Fatal("empty fleet added chrome", title)
	}
	m.height = 34
	for _, line := range strings.Split(m.renderHelp(), "\n") {
		if visualWidth(line) > 80 {
			t.Fatal("workers help overflow", line)
		}
	}
}
