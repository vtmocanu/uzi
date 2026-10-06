package main

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

type workerNavFake struct {
	*uzicli.FakeClient
	gets, lists, inputs int
}

func (f *workerNavFake) GetRun(ctx context.Context, id string) (apitypes.RunDTO, error) {
	f.gets++
	return f.FakeClient.GetRun(ctx, id)
}
func (f *workerNavFake) ListWorkers(ctx context.Context) ([]apitypes.WorkerDTO, error) {
	f.lists++
	return f.FakeClient.ListWorkers(ctx)
}
func (f *workerNavFake) RunInputs(ctx context.Context, id string) ([]apitypes.SteerInputDTO, error) {
	f.inputs++
	return f.FakeClient.RunInputs(ctx, id)
}

// Execute finite commands only; stream readers and scheduled ticks are excluded.
func workerNavFinite(t *testing.T, m tuiModel, cmd tea.Cmd) tuiModel {
	t.Helper()
	if cmd == nil {
		return m
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, child := range batch {
			m = workerNavFinite(t, m, child)
		}
		return m
	}
	switch msg.(type) {
	case streamReadyMsg, detailPageMsg, detailRunMsg, runInputsMsg:
		next, _ := m.Update(msg)
		return next.(tuiModel)
	}
	return m
}
func workerNavFixture(t *testing.T) (tuiModel, *workerNavFake) {
	t.Helper()
	id := "worker-1"
	f := &workerNavFake{FakeClient: &uzicli.FakeClient{
		Workers: []apitypes.WorkerDTO{{ID: id, Name: "forge", Status: "online", ReportedRuns: []apitypes.WorkerReportedRunDTO{{RunID: "B", Phase: "running", ClaimGeneration: 2}}}},
		RunByID: map[string]apitypes.RunDTO{"A": {ID: "A", Status: "running", WorkerID: &id}, "B": {ID: "B", Status: "running", WorkerID: &id}},
	}}
	m := tuiTestModel(t, f, "A")
	m = applyDetail(m, f.RunByID["A"], nil)
	m.workers.rows = []workerRow{{w: f.Workers[0]}}
	m.workers.loaded = true
	return m, f
}
func TestWorkerNavigationFreshSingleOrigin(t *testing.T) {
	m, f := workerNavFixture(t)
	m.detailReturn = viewPulls
	b := f.RunByID["B"]
	b.MilestonesInProgress = []string{"m2"}
	f.RunByID["B"] = b
	m.blinkArmed = false
	oldGen := m.detail.gen
	stream := uzicli.NewRunStream(context.Background(), nil)
	m.detail.stream = stream
	m.detail.frames = []laneFrame{{Seq: 99}}
	m = press(t, m, "W")
	if m.view != viewWorker || m.detail.runID != "" || len(m.detail.frames) != 0 || m.detail.seen != nil || m.detail.stream != nil {
		t.Fatal("departed run session retained")
	}
	select {
	case <-stream.Events():
	case <-time.After(time.Second):
		t.Fatal("stream not closed")
	}
	next, cmd := m.handleKey(keyEnter)
	m = next.(tuiModel)
	reply := cmd().(workerRunMsg)
	if f.gets != 1 {
		t.Fatal("preflight GetRun missing")
	}
	next, cmd = m.Update(reply)
	m = next.(tuiModel)
	if m.view != viewDetail || m.detail.runID != "B" || !m.detail.runLoaded || m.detailReturn != viewWorker || !m.blinkArmed || !m.detail.follow {
		t.Fatal("preflight DTO not applied through run entry")
	}
	m = workerNavFinite(t, m, cmd)
	if f.gets != 1 || f.inputs != 1 {
		t.Fatalf("duplicate initial fetch or missing inputs: gets=%d inputs=%d", f.gets, f.inputs)
	}
	m = press(t, m, keyEsc)
	if m.view != viewWorker || m.workerOrigin.runID != "A" {
		t.Fatal("B replaced origin")
	}
	next, cmd = m.handleKey(keyEsc)
	m = next.(tuiModel)
	if m.view != viewDetail || m.detail.runID != "A" || m.detail.gen == oldGen || m.detailReturn != viewPulls || m.detail.runLoaded {
		t.Fatal("origin run was not freshly reopened with original target")
	}
	m = workerNavFinite(t, m, cmd)
	if f.gets != 2 {
		t.Fatal("origin did not fetch fresh DTO")
	}
}
func TestWorkerNavigationListAndPollSelection(t *testing.T) {
	m, _ := workerNavFixture(t)
	m.view = viewWorkers
	m.workers.cursor = 0
	m.workers.scroll = 3
	m.workers.selectedID = "worker-1"
	m.topTab = viewWorkers
	m.bottomTab = viewPulls
	m.splitLatch = true
	m.height = 80
	m = press(t, m, keyEnter)
	if m.view != viewWorker || !m.workerOrigin.fromSplit || !m.workersVisible() {
		t.Fatal("split worker entry or polling")
	}
	m.workers.waitID = 71
	row := m.workers.rows[0]
	row.w.ReportedRuns = append([]apitypes.WorkerReportedRunDTO{{RunID: "C"}}, row.w.ReportedRuns...)
	next, _ := m.Update(workersMsg{reqID: 71, rows: []workerRow{row}})
	m = next.(tuiModel)
	if m.workerDetail.selectedRunID != "B" || m.workerDetail.cursor != 1 {
		t.Fatal("accepted poll changed run identity")
	}
	m = press(t, m, keyEsc)
	if m.view != viewWorkers || m.topTab != viewWorkers || m.bottomTab != viewPulls || m.workers.selectedID != "worker-1" || !m.splitDrawn() {
		t.Fatal("list origin not restored")
	}
}
func TestWorkerNavigationPreflightStaleAndNotVisible(t *testing.T) {
	m, f := workerNavFixture(t)
	m = press(t, m, "W")
	next, cmd := m.handleKey(keyEnter)
	m = next.(tuiModel)
	msg := cmd().(workerRunMsg)
	next, _ = m.Update(workerRunMsg{nav: msg.nav, err: uzicli.Exitf(uzicli.ExitNotFound, "hidden")})
	m = next.(tuiModel)
	if m.view != viewWorker || m.workerDetail.notice != "run not visible" {
		t.Fatal("404 navigated")
	}
	next, cmd = m.handleKey(keyEnter)
	m = next.(tuiModel)
	newer := m.workerNav
	next, _ = m.Update(msg)
	m = next.(tuiModel)
	if m.view != viewWorker || m.workerNav != newer {
		t.Fatal("old preflight cleared new wait")
	}
	late := cmd().(workerRunMsg)
	m = press(t, m, keyEsc)
	next, _ = m.Update(late)
	m = next.(tuiModel)
	if m.view != viewDetail || m.detail.runID != "A" || f.gets != 2 {
		t.Fatal("late reply navigated departed view")
	}
}
func TestWorkerNavigationFiniteRefetchAndMetadataGuard(t *testing.T) {
	m, f := workerNavFixture(t)
	m.workers.rows = nil
	next, cmd := m.handleKey("W")
	m = next.(tuiModel)
	if cmd == nil || m.workersVisible() {
		t.Fatal("missing finite hidden fetch")
	}
	first := cmd().(runWorkerMsg)
	next, cmd = m.handleKey("W")
	m = next.(tuiModel)
	wait := m.workerNav
	next, _ = m.Update(first)
	m = next.(tuiModel)
	if m.workerNav != wait || m.view != viewDetail {
		t.Fatal("superseded W navigated")
	}
	second := cmd().(runWorkerMsg)
	other := "worker-2"
	m.detail.run.WorkerID = &other
	next, _ = m.Update(second)
	m = next.(tuiModel)
	if m.view != viewDetail || f.lists != 2 {
		t.Fatal("metadata mismatch navigated or refetch repeated")
	}
	m.detail.run.WorkerID = nil
	m = press(t, m, "W")
	if m.detail.steer.notice != "no worker yet" || f.lists != 2 {
		t.Fatal("nil worker fetched")
	}
}
func TestWorkerNavigationMissingAndScopeUnwind(t *testing.T) {
	for _, scope := range []bool{false, true} {
		m, _ := workerNavFixture(t)
		m.detailReturn = viewPR
		m = press(t, m, "W")
		var next tea.Model
		if scope {
			m.board.admin = true
			next, _ = m.reconcileWorkers(nil)
		} else {
			m.workers.waitID = 8
			next, _ = m.Update(workersMsg{reqID: 8})
		}
		m = next.(tuiModel)
		if m.view != viewDetail || m.detail.runID != "A" || m.detail.runLoaded || m.detailReturn != viewPR {
			t.Fatal("missing worker did not reopen origin fresh")
		}
	}
	m, f := workerNavFixture(t)
	m.workers.rows = nil
	f.Workers = nil
	next, cmd := m.handleKey("W")
	m = next.(tuiModel)
	next, _ = m.Update(cmd())
	m = next.(tuiModel)
	if m.view != viewDetail || m.detail.steer.notice != "worker not in your list" || f.lists != 1 {
		t.Fatal("absent worker fallback")
	}
}
func TestWorkerNavigationLateRunMessagesAndWorkerReturnBeforeCollapse(t *testing.T) {
	m, _ := workerNavFixture(t)
	gen := m.detail.gen
	m = press(t, m, "W")
	next, _ := m.Update(detailRunMsg{runID: "A", gen: gen, run: apitypes.RunDTO{ID: "A"}})
	m = next.(tuiModel)
	if m.detail.runID != "" || m.detail.runLoaded {
		t.Fatal("departed DTO repopulated worker")
	}
	stream := uzicli.NewRunStream(context.Background(), nil)
	next, _ = m.Update(streamReadyMsg{runID: "A", gen: gen, stream: stream})
	m = next.(tuiModel)
	select {
	case <-stream.Events():
	case <-time.After(time.Second):
		t.Fatal("late socket was not closed")
	}
	next, cmd := m.handleKey(keyEnter)
	m = next.(tuiModel)
	next, _ = m.Update(cmd())
	m = next.(tuiModel)
	m.fromSplit = true
	m.splitLatch = false
	m.topTab = viewWorkers
	m = press(t, m, "W")
	if m.workerOrigin.runID != "A" {
		t.Fatal("B W replaced single origin")
	}
	next, cmd = m.handleKey(keyEnter)
	m = next.(tuiModel)
	next, _ = m.Update(cmd())
	m = next.(tuiModel)
	m = press(t, m, keyEsc)
	if m.view != viewWorker {
		t.Fatal("collapse overrode worker return target")
	}
	m = press(t, m, keyEsc)
	if m.view != viewDetail || m.detail.runID != "A" {
		t.Fatal("origin chain changed")
	}
}

func TestWorkerNavigationListFallbackAndConciseRenderer(t *testing.T) {
	m, _ := workerNavFixture(t)
	m.view = viewWorkers
	m.workers.selectedID = "worker-1"
	m = press(t, m, keyEnter)
	out := stripANSI(m.View().Content)
	for _, want := range []string{"forge", "attention", "reported runs", "B", "gen 2", "enter/→ run"} {
		if !strings.Contains(out, want) {
			t.Fatalf("worker renderer omitted %q", want)
		}
	}
	m.workers.waitID = 99
	next, _ := m.Update(workersMsg{reqID: 99, rows: []workerRow{{w: apitypes.WorkerDTO{ID: "replacement", Name: "other"}}}})
	m = next.(tuiModel)
	if m.view != viewWorkers || m.workers.selectedID != "replacement" || m.splitNote != "worker not in your list" {
		t.Fatal("missing list origin did not use index fallback")
	}
}

func TestWorkerNavigationScopeReplyCannotClearNewWait(t *testing.T) {
	m, _ := workerNavFixture(t)
	m.workers.rows = nil
	next, cmd := m.handleKey("W")
	m = next.(tuiModel)
	old := cmd().(runWorkerMsg)
	m.board.admin = true
	next, _ = m.reconcileWorkers(nil)
	m = next.(tuiModel)
	next, cmd = m.handleKey("W")
	m = next.(tuiModel)
	_ = cmd
	wait := m.workerNav
	next, _ = m.Update(old)
	m = next.(tuiModel)
	if m.view != viewDetail || m.workerNav != wait || m.workers.loaded && len(m.workers.rows) > 0 {
		t.Fatal("old scope reply navigated or cleared new wait")
	}
}

type workerNavAdminDeniedFake struct {
	*workerNavFake
	adminLists int
}

func (f *workerNavAdminDeniedFake) AdminListWorkers(context.Context) ([]apitypes.AdminWorkerDTO, error) {
	f.adminLists++
	return nil, uzicli.Exitf(uzicli.ExitAuth, "admin workers denied")
}

func TestWorkerNavigationFiniteAdminDenialRefetchesOwnScopes(t *testing.T) {
	m, f := workerNavFixture(t)
	denied := &workerNavAdminDeniedFake{workerNavFake: f}
	m.client = denied
	m.board.admin, m.workers.admin = true, true
	m.workers.rows[0].w.ID = "other-admin-worker"
	m.workers.reqSeq, m.workers.waitID = 41, 41
	next, cmd := m.handleKey("W")
	m = next.(tuiModel)
	if m.workers.waitID <= 41 {
		t.Fatal("finite lookup did not supersede outstanding request")
	}
	reply := cmd().(runWorkerMsg)
	if reply.workers.reqID != m.workers.waitID || denied.adminLists != 1 {
		t.Fatal("finite lookup bypassed shared request guard")
	}
	next, cmd = m.Update(reply)
	m = next.(tuiModel)
	if m.board.admin || !m.board.adminDenied || m.workers.admin || len(m.workers.rows) != 0 || m.workers.loaded || m.workerNav.runID != "" || m.workerNav == reply.nav || m.workers.waitID == 0 {
		t.Fatal("finite admin denial did not clear scope, snapshot and navigation")
	}
	wait := m.workers.waitID
	next, _ = m.Update(reply)
	m = next.(tuiModel)
	if m.workers.waitID != wait {
		t.Fatal("old admin lookup cleared own request")
	}
	msgs := drainCmd(cmd)
	if denied.adminLists != 1 || f.lists != 1 || f.ListRunsCalls != 1 || f.AdminListRunsCalls != 0 {
		t.Fatalf("fallback calls: admin workers=%d own workers=%d own runs=%d admin runs=%d",
			denied.adminLists, f.lists, f.ListRunsCalls, f.AdminListRunsCalls)
	}
	var accepted bool
	for _, msg := range msgs {
		if own, ok := msg.(workersMsg); ok {
			if own.err != nil || own.admin || own.reqID != wait {
				t.Fatal("own workers refetch inherited admin failure")
			}
			next, rearm := m.Update(own)
			m = next.(tuiModel)
			if rearm != nil {
				t.Fatal("hidden finite reply rearmed polling")
			}
			accepted = m.workers.loaded && len(m.workers.rows) == 1 && m.workers.waitID == 0
		}
	}
	if !accepted || m.view != viewDetail {
		t.Fatal("own snapshot not accepted in run detail")
	}
}

func TestWorkerNavigationBoardFallbackFetchesWhileHidden(t *testing.T) {
	m, f := workerNavFixture(t)
	m.boardReplied = true
	m.board.admin, m.workers.admin = true, true
	m.board.waitID = 7
	m.workers.waitID, m.workers.reqSeq = 9, 9
	next, cmd := m.Update(boardRunsMsg{reqID: 7, admin: true, err: errFake("admin board denied")})
	m = next.(tuiModel)
	if m.view != viewDetail || m.workersVisible() || m.board.admin || !m.board.adminDenied || m.workers.admin || m.workers.loaded || len(m.workers.rows) != 0 || m.workers.waitID <= 9 {
		t.Fatal("board fallback failed to mint finite hidden own request")
	}
	// The final batch is the immediate scope refresh; leave the board timer pending.
	batch := cmd().(tea.BatchMsg)
	msgs := drainCmd(batch[len(batch)-1])
	var accepted bool
	for _, msg := range msgs {
		if own, ok := msg.(workersMsg); ok {
			if own.admin || own.err != nil {
				t.Fatal("scope transition did not fetch own workers")
			}
			next, rearm := m.Update(own)
			m = next.(tuiModel)
			if rearm != nil {
				t.Fatal("hidden own reply rearmed workers")
			}
			accepted = m.workers.loaded && len(m.workers.rows) == 1 && m.workers.waitID == 0
		}
	}
	if !accepted || f.lists != 1 || f.ListRunsCalls != 1 {
		t.Fatal("scope transition did not immediately fetch and accept own snapshot")
	}
}

func TestWorkerNavigationFiniteStaleGuardsPreserveSharedWait(t *testing.T) {
	for _, guard := range []string{"request", "metadata", "view", "scope"} {
		t.Run(guard, func(t *testing.T) {
			m, f := workerNavFixture(t)
			m.workers.rows = nil
			next, cmd := m.handleKey("W")
			m = next.(tuiModel)
			old := cmd().(runWorkerMsg)
			switch guard {
			case "request":
				_ = (&m).startWorkersReqMode(true)
			case "metadata":
				id := "worker-new"
				m.detail.run.WorkerID = &id
			case "view":
				m.view = viewPulls
			case "scope":
				m.board.admin = true
			}
			wait, nav := m.workers.waitID, m.workerNav
			next, cmd = m.Update(old)
			m = next.(tuiModel)
			if m.workers.waitID != wait || len(m.workers.rows) != 0 || cmd != nil || f.lists != 1 {
				t.Fatal("stale finite lookup touched snapshot, wait or polling")
			}
			if guard == "request" || guard == "metadata" {
				if m.workerNav != nav {
					t.Fatal("stale lookup cleared newer navigation")
				}
			}
		})
	}
}

func TestWorkerNavigationFallbackTickFreshSameRunSession(t *testing.T) {
	shrinkPollInterval(t, time.Millisecond)
	m, _ := workerNavFixture(t)
	next, cmd := m.Update(streamReadyMsg{runID: "A", gen: m.detail.gen, err: errFake("socket down")})
	m = next.(tuiModel)
	old := cmd().(pollFallbackMsg)
	m.beginRunSession("A", viewBoard)
	next, cmd = m.Update(streamReadyMsg{runID: "A", gen: m.detail.gen, err: errFake("new socket down")})
	m = next.(tuiModel)
	fresh := cmd().(pollFallbackMsg)
	if fresh.runID != old.runID || fresh.gen == old.gen {
		t.Fatal("same-ID reentry retained fallback identity")
	}
	next, cmd = m.Update(old)
	m = next.(tuiModel)
	if cmd != nil || m.detail.metaWaitID != 0 || !m.detail.polling {
		t.Fatal("old tick rearmed or mutated fresh session")
	}
	next, cmd = m.Update(fresh)
	m = next.(tuiModel)
	if cmd == nil || m.detail.metaWaitID == 0 {
		t.Fatal("fresh tick did not request metadata")
	}
	msgs := drainCmd(cmd)
	var rearmed, fetched bool
	for _, msg := range msgs {
		switch msg := msg.(type) {
		case pollFallbackMsg:
			rearmed = msg.runID == "A" && msg.gen == m.detail.gen
		case detailMetaMsg:
			fetched = msg.runID == "A" && msg.gen == m.detail.gen
		}
	}
	if !rearmed || !fetched {
		t.Fatal("fresh tick did not rearm current session and fetch")
	}
}

func TestWorkerNavigationNoRetainedSessionReferences(t *testing.T) {
	for _, v := range []any{workerOrigin{}, workerDetailState{}, workerNavigation{}} {
		typ := reflect.TypeOf(v)
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			if field.Type == reflect.TypeOf(detailState{}) || field.Type == reflect.TypeOf((*uzicli.RunStream)(nil)) || strings.Contains(strings.ToLower(field.Name), "suspend") {
				t.Fatalf("retained run session: %s", field.Name)
			}
		}
	}
}

func TestWorkerNavigationAuxiliarySessionGuards(t *testing.T) {
	m, f := workerNavFixture(t)
	m.detail.steer.access = steerAllowed
	f.InputsByID = map[string][]apitypes.SteerInputDTO{"A": {{Kind: kindFollowUp, Body: sp("fresh input")}}}
	f.Reviews = map[string]*apitypes.ReviewDTO{"A": reviewFixture()}
	cmds := []tea.Cmd{
		m.fetchInputsCmd("A"), m.submitSteerCmd(kindCancel, "", nil),
		m.loadReviewCmd("A"), m.setDispositionCmd("aaaaaaaa", "done", ""),
		m.deleteDispositionCmd("aaaaaaaa"),
		// Resolution errors are also stamped.
		m.setDispositionCmd("missing", "done", ""), m.deleteDispositionCmd("missing"),
	}
	old := make([]tea.Msg, len(cmds))
	for i, cmd := range cmds {
		old[i] = cmd()
	}
	m = press(t, m, "W")
	next, cmd := m.handleKey(keyEsc)
	m = workerNavFinite(t, next.(tuiModel), cmd)
	m.detail.review.loading = true
	m.detail.steer.access, m.detail.steer.queue = steerUnknown, nil
	before := m.detail
	for _, msg := range old {
		next, cmd = m.Update(msg)
		m = next.(tuiModel)
		if cmd != nil || !reflect.DeepEqual(before, m.detail) {
			t.Fatalf("old auxiliary reply %T changed fresh same-ID session", msg)
		}
	}
	current := []tea.Msg{
		m.fetchInputsCmd("A")(), m.submitSteerCmd(kindCancel, "", nil)(),
		m.loadReviewCmd("A")(), m.setDispositionCmd("aaaaaaaa", "done", "")(), m.deleteDispositionCmd("aaaaaaaa")(),
	}
	for _, msg := range current {
		next, cmd = m.Update(msg)
		m = next.(tuiModel)
		switch msg.(type) {
		case runInputsMsg:
			if m.detail.steer.access != steerAllowed || len(m.detail.steer.queue) != 1 || (m.detail.steer.queue[0].Body == nil || *m.detail.steer.queue[0].Body != "fresh input") {
				t.Fatal("current input probe ignored")
			}
		case steerResultMsg:
			if cmd == nil {
				t.Fatal("current steer reply did not fetch inputs")
			}
			if reply := cmd().(runInputsMsg); reply.gen != m.detail.gen {
				t.Fatal("input follow-up lost generation")
			}
		case reviewLoadedMsg:
			if m.detail.review.loading || m.detail.review.review == nil {
				t.Fatal("current review ignored")
			}
		case dispositionDoneMsg:
			if cmd == nil || !m.detail.review.loading || m.detail.review.notice != "triage recorded" {
				t.Fatal("current disposition ignored")
			}
			if reply := cmd().(reviewLoadedMsg); reply.gen != m.detail.gen {
				t.Fatal("review follow-up lost generation")
			}
		}
	}
	for _, cmd := range []tea.Cmd{m.setDispositionCmd("missing", "done", ""), m.deleteDispositionCmd("missing")} {
		msg := cmd().(dispositionDoneMsg)
		if msg.gen != m.detail.gen || msg.err == nil {
			t.Fatal("current disposition resolution error lost generation")
		}
	}
}

func TestWorkerNavigationPRFreshReturnAndRoot(t *testing.T) {
	for _, viaLink := range []bool{false, true} {
		t.Run(fmt.Sprint(viaLink), func(t *testing.T) {
			m, f := workerNavFixture(t)
			m.detailReturn = viewPulls
			b := f.RunByID["B"]
			b.RepoID, b.MrIID = sp("repo"), ip(12)
			if viaLink {
				b.WorkerID = sp("worker-2")
				m.workers.rows = append(m.workers.rows, workerRow{w: apitypes.WorkerDTO{ID: "worker-2", Status: "online", ReportedRuns: []apitypes.WorkerReportedRunDTO{{RunID: "B"}}}})
			}
			f.RunByID["B"] = b
			m = press(t, m, "W")
			next, cmd := m.handleKey(keyEnter)
			m = next.(tuiModel)
			next, cmd = m.Update(cmd())
			m = workerNavFinite(t, next.(tuiModel), cmd)
			gen := m.detail.gen
			aux := []tea.Msg{m.fetchInputsCmd("B")(), m.submitSteerCmd(kindCancel, "", nil)(), m.loadReviewCmd("B")(), m.setDispositionCmd("missing", "done", "")()}
			own := streamOf(t, m.openStreamCmd("B"))
			next, _ = m.Update(own)
			m = next.(tuiModel)
			m = press(t, m, keyPRView)
			requireStreamClosed(t, own.stream, "run departing for PR")
			if !reflect.DeepEqual(m.detail, detailState{}) || m.prReturnRunID != "B" || m.prReturnRunTarget != viewWorker {
				t.Fatal("PR retained loaded run state or lost scalar return")
			}
			m.fromSplit, m.splitLatch = true, false
			if viaLink {
				m.pr.detail.RunID = sp("B")
				next, cmd = m.handleKey(keyRunLink)
				m = next.(tuiModel)
			} else {
				next, cmd = m.handleKey(keyEsc)
				m = next.(tuiModel)
				if m.detailReturn != viewWorker || m.detail.runLoaded || m.prReturnRunID != "" {
					t.Fatal("PR Esc lost fresh worker return")
				}
			}
			reopen := cmd
			if reopen == nil {
				t.Fatal("fresh PR return did not fetch run/tail/stream")
			}
			if m.view != viewDetail || m.detail.runID != "B" || m.detail.gen == gen {
				t.Fatal("PR return did not open fresh B")
			}
			before := m.detail
			for _, msg := range aux {
				next, cmd = m.Update(msg)
				m = next.(tuiModel)
				if cmd != nil || !reflect.DeepEqual(before, m.detail) {
					t.Fatalf("PR return accepted stale auxiliary %T", msg)
				}
			}
			next, cmd = m.Update(detailRunMsg{runID: "B", gen: gen, run: b})
			m = next.(tuiModel)
			if cmd != nil || m.detail.runLoaded {
				t.Fatal("old B DTO applied")
			}
			stale := uzicli.NewRunStream(context.Background(), nil)
			next, _ = m.Update(streamReadyMsg{runID: "B", gen: gen, stream: stale})
			m = next.(tuiModel)
			requireStreamClosed(t, stale, "old B socket")
			m = workerNavFinite(t, m, reopen)
			if !m.detail.runLoaded || m.detail.stream == nil || m.detail.tailInFlight || f.gets != 2 || f.inputs != 2 {
				t.Fatal("PR return did not execute fresh run/tail/stream/input lifecycle")
			}
			if viaLink {
				m = press(t, m, "W")
				if m.workerDetail.workerID != "worker-2" || m.detail.runID != "" {
					t.Fatal("new worker link retained old identity or loaded run")
				}
				next, cmd = m.handleKey(keyEnter)
				m = next.(tuiModel)
				next, _ = m.Update(cmd())
				m = next.(tuiModel)
				m = press(t, m, keyEsc)
			} else {
				m = press(t, m, keyEsc)
			}
			if m.view != viewWorker || m.workerOrigin.runID != "A" || m.workerOrigin.detailReturn != viewPulls {
				t.Fatal("PR link replaced root A")
			}
			m = press(t, m, keyEsc)
			if m.detail.runID != "A" || m.detailReturn != viewPulls {
				t.Fatal("worker Esc lost original A target")
			}
			m = press(t, m, keyEsc)
			if m.workerOrigin != (workerOrigin{}) {
				t.Fatal("list return retained origin")
			}
			m.beginRunSession("C", viewBoard)
			c := apitypes.RunDTO{ID: "C", Status: "running", WorkerID: sp("worker-1")}
			m = applyDetail(m, c, nil)
			m = press(t, m, "W")
			if m.workerOrigin.runID != "C" || m.workerOrigin.detailReturn != viewBoard {
				t.Fatal("fresh C inherited stale root")
			}
		})
	}
}

func TestWorkerNavigationMissingWorkerRearmsVisiblePollOnly(t *testing.T) {
	for _, list := range []bool{false, true} {
		t.Run(fmt.Sprint(list), func(t *testing.T) {
			m, f := workerNavFixture(t)
			if list {
				m.view, m.topTab = viewWorkers, viewWorkers
			}
			m = press(t, m, "W")
			if list {
				m.view = viewWorkers
				m = press(t, m, keyEnter)
			}
			m.workers.active, m.workers.waitID = true, 91
			next, cmd := m.Update(workersMsg{reqID: 91})
			m = next.(tuiModel)
			if !list {
				for _, msg := range drainCmd(cmd) {
					if _, ok := msg.(workersTickMsg); ok {
						t.Fatal("hidden run origin rearmed workers")
					}
				}
				return
			}
			msgs := drainCmd(cmd)
			if len(msgs) != 1 {
				t.Fatalf("visible disappearance commands: %v", msgs)
			}
			tick, ok := msgs[0].(workersTickMsg)
			if !ok {
				t.Fatalf("missing visible worker tick: %T", msgs[0])
			}
			next, cmd = m.Update(tick)
			m = next.(tuiModel)
			if cmd == nil {
				t.Fatal("tick discarded next poll")
			}
			reply := cmd().(workersMsg)
			if f.lists != 1 || reply.reqID != m.workers.waitID {
				t.Fatal("tick did not request exactly one next poll")
			}
			_, duplicate := m.Update(tick)
			if duplicate != nil {
				t.Fatal("tick duplicated pending poll")
			}
		})
	}
}
