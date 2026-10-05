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
	m := workersScene(true, "workers-list-120")
	want := "your workers · 9 · 8 online · 5/12 slots in use +1 ?cap · 1 holding · 1 draining · 6 need attention"
	if got := m.workersSummary(120, false); got != want {
		t.Fatalf("summary %q", got)
	}
	if got := m.workersSummary(120, true); got != "workers 5/12 slots in use · 8/9 online · 6 need attention · 2 for detail" {
		t.Fatal(got)
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
	r.w.Version = sp(strings.Repeat("界", 8))
	r.w.UpgradeStatus = "upgrade_failed"
	r.w.OutboxBlocked = sp("blocked")
	if line := m.workerRowLine(r, true, 120); !strings.Contains(stripANSI(line), "+") || visualWidth(line) > 120 {
		t.Fatalf("attention suffix lost: %s", line)
	}
	if !strings.Contains(stripANSI(m.renderWorkers()), "fixed visual cue") {
		t.Fatal("missing disk legend")
	}
	help := strings.Join(helpLines(viewWorkers), "\n")
	if !strings.Contains(help, "j / ↓") || !strings.Contains(help, "k / ↑") {
		t.Fatal("movement help omitted")
	}
}

func TestWorkerStatePrecedence(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name string
		w    apitypes.WorkerDTO
		want string
	}{
		{"offline wins", apitypes.WorkerDTO{Status: "offline", RetainingUnpublishedWork: true, DrainingSince: &now, Busy: true}, "offline"},
		{"holding wins", apitypes.WorkerDTO{Status: "online", RetainingUnpublishedWork: true, DrainingSince: &now, Busy: true}, "holding"},
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
				if name == "split-workers-top" && !strings.Contains(frame, "2 [workers]") {
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
	if !strings.Contains(m.workersSummary(120, false), "2 need attention") {
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

func TestWorkersZeroSummaryAndMaximumAttentionBounds(t *testing.T) {
	m := workersScene(true, "workers-list-80")
	r := m.workers.rows[0]
	r.w.OutboxBlocked = sp(strings.Repeat("blocked ", 100))
	r.w.UpgradeStatus = "upgrade_failed"
	r.w.RetainingUnpublishedWork = true
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
	if m.workersSummary(80, false) != "" || m.workersSummary(80, true) != "" {
		t.Fatal("empty summary charged")
	}
	m.height = 34
	for _, line := range strings.Split(m.renderHelp(), "\n") {
		if visualWidth(line) > 80 {
			t.Fatal("workers help overflow", line)
		}
	}
}
