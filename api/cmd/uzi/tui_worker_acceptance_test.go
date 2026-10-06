package main

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// Drain only a finite entry batch; never drain commands returned by stream replies.
// Entry batches have a fixed depth and contain no stream readers or recurring polls.
func workerAcceptanceMessages(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		var out []tea.Msg
		for _, child := range batch {
			out = append(out, workerAcceptanceMessages(child)...)
		}
		return out
	}
	return []tea.Msg{msg}
}

func TestWorkerAcceptanceReportedRunPreflight(t *testing.T) {
	for _, cached := range []bool{false, true} {
		t.Run(fmt.Sprintf("cached=%t", cached), func(t *testing.T) {
			m, f := workerNavFixture(t)
			b := f.RunByID["B"]
			b.IssueTitle = "fresh preflight title"
			b.MilestonesInProgress = []string{"m2"}
			f.RunByID["B"] = b
			f.InputsByID = map[string][]apitypes.SteerInputDTO{"B": {{Kind: kindFollowUp, Body: sp("current input")}}}
			if cached {
				m.board.runs = []apitypes.RunListItemDTO{{RunDTO: apitypes.RunDTO{ID: "B", IssueIID: i64(1), IssueTitle: "old cached title"}}}
			}
			m.blinkArmed = false
			m = press(t, m, "W")
			if cached {
				requireWorkerText(t, stripANSI(m.View().Content), "old cached title")
			} else {
				requireWorkerText(t, stripANSI(m.View().Content), "B", "not cached")
			}
			next, cmd := m.handleKey(keyEnter)
			m = next.(tuiModel)
			reply := cmd().(workerRunMsg)
			if f.gets != 1 || m.detail.runLoaded {
				t.Fatal("preflight must fetch once before entering run detail")
			}
			next, cmd = m.Update(reply)
			m = next.(tuiModel)
			if !reflect.DeepEqual(m.detail.run, b) || !m.detail.runLoaded || !m.blinkArmed || m.detailReturn != viewWorker {
				t.Fatal("preflight did not apply the fresh DTO and normal detail side effects")
			}
			var input, blink, tail, stream bool
			for _, msg := range workerAcceptanceMessages(cmd) {
				switch msg := msg.(type) {
				case runInputsMsg:
					input = true
					next, _ = m.Update(msg)
					m = next.(tuiModel)
				case blinkTickMsg:
					blink = true
				case detailPageMsg:
					tail = msg.kind == pageTail && msg.runID == "B" && msg.gen == m.detail.gen
				case streamReadyMsg:
					stream = msg.runID == "B" && msg.gen == m.detail.gen && msg.stream != nil
					if msg.stream != nil {
						msg.stream.Close()
					}
				}
			}
			if f.gets != 1 || f.inputs != 1 || !input || !blink || !tail || !stream || m.detail.steer.access != steerAllowed || len(m.detail.steer.queue) != 1 {
				t.Fatalf("initial lifecycle: gets=%d inputs=%d input=%t blink=%t tail=%t stream=%t", f.gets, f.inputs, input, blink, tail, stream)
			}
		})
	}
}

func TestWorkerAcceptanceBothStreamsFreshTailAndBackfill(t *testing.T) {
	shrinkPageSize(t, 2)
	m, f := workerNavFixture(t)
	m.detailReturn = viewPulls
	f.LogsByID = map[string][]apitypes.MessageDTO{"A": {
		msgDTO(1, "text", "lead", "", "", "oldest", time.Now()),
		msgDTO(2, "text", "lead", "", "", "older", time.Now()),
		msgDTO(3, "text", "lead", "", "", "middle", time.Now()),
		msgDTO(4, "text", "lead", "", "", "newest tail", time.Now()),
		msgDTO(5, "text", "lead", "", "", "latest output", time.Now()),
	}}
	oldGen := m.detail.gen
	oldTail := m.loadTailCmd("A")()
	a := streamOf(t, m.openStreamCmd("A"))
	next, _ := m.Update(a)
	m = next.(tuiModel)
	m = press(t, m, "W")
	requireStreamClosed(t, a.stream, "A leaving for worker")
	next, cmd := m.handleKey(keyEnter)
	m = next.(tuiModel)
	next, cmd = m.Update(cmd())
	m = workerNavFinite(t, next.(tuiModel), cmd)
	b := m.detail.stream
	if b == nil {
		t.Fatal("B lifecycle did not open its stream")
	}
	t.Cleanup(b.Close)
	m = press(t, m, keyEsc)
	requireStreamClosed(t, b, "B leaving for worker")
	if m.view != viewWorker || m.workerOrigin.runID != "A" {
		t.Fatal("B replaced original A")
	}
	next, cmd = m.handleKey(keyEsc)
	m = next.(tuiModel)
	if m.detail.gen == oldGen || m.detail.runLoaded || len(m.detail.frames) != 0 || m.detailReturn != viewPulls {
		t.Fatal("A was not reopened as an empty fresh session with its original target")
	}
	// Exercise the real retry guard before delivering the departed session's tail.
	next, _ = m.Update(detailPageMsg{runID: "A", gen: m.detail.gen, kind: pageTail, err: errFake("retry needed")})
	m = next.(tuiModel)
	next, retry := m.handleKey(keyRefresh)
	m = next.(tuiModel)
	if retry == nil || !m.detail.tailInFlight {
		t.Fatal("fresh session did not issue guarded retry")
	}
	wait := m.detail.tailInFlight
	next, ignored := m.Update(oldTail)
	m = next.(tuiModel)
	if ignored != nil || m.detail.tailInFlight != wait || len(m.detail.frames) != 0 {
		t.Fatal("old A tail cleared current tail request")
	}
	var backfill tea.Cmd
	for _, msg := range workerAcceptanceMessages(cmd) {
		if ready, ok := msg.(streamReadyMsg); ok && ready.stream != nil {
			t.Cleanup(ready.stream.Close)
		}
		next, follow := m.Update(msg)
		m = next.(tuiModel)
		if page, ok := msg.(detailPageMsg); ok && page.kind == pageTail {
			backfill = follow
		}
	}
	if !reflect.DeepEqual(frameSeqs(m), []int32{4, 5}) || m.detail.tailInFlight || !m.detail.backfilling || backfill == nil || !strings.Contains(stripANSI(m.View().Content), "latest output") {
		t.Fatal("fresh return did not paint newest tail before background history")
	}
	if q := f.RunLogsPageCalls[len(f.RunLogsPageCalls)-1]; q.Tail != 2 || q.Before != 0 {
		t.Fatal("fresh return did not request newest tail")
	}
	next, ignored = m.Update(detailPageMsg{runID: "A", gen: oldGen, kind: pageBackfill})
	m = next.(tuiModel)
	if ignored != nil || !m.detail.backfilling || !reflect.DeepEqual(frameSeqs(m), []int32{4, 5}) {
		t.Fatal("stale backfill disturbed current walk")
	}
	page := backfill().(detailPageMsg)
	next, backfill = m.Update(page)
	m = next.(tuiModel)
	if !reflect.DeepEqual(frameSeqs(m), []int32{2, 3, 4, 5}) || backfill == nil {
		t.Fatal("first older page did not continue background walk")
	}
	next, backfill = m.Update(backfill())
	m = next.(tuiModel)
	if !reflect.DeepEqual(frameSeqs(m), []int32{1, 2, 3, 4, 5}) || !m.detail.historyComplete || m.detail.backfilling || backfill != nil || f.gets != 2 {
		t.Fatal("fresh A history did not finish without duplicate DTO fetch")
	}
	freshStream := m.detail.stream
	m = press(t, m, keyEsc)
	requireStreamClosed(t, freshStream, "fresh A leaving for original list")
	if m.workerOrigin != (workerOrigin{}) {
		t.Fatal("origin survived final list return")
	}
}

func TestWorkerAcceptanceSameWorkerReentryRejectsOldNavigation(t *testing.T) {
	m, _ := workerNavFixture(t)
	m = press(t, m, "W")
	firstGen := m.workerDetail.gen
	next, cmd := m.handleKey(keyEnter)
	m = next.(tuiModel)
	old := cmd().(workerRunMsg)
	next, cmd = m.handleKey(keyEsc)
	m = workerNavFinite(t, next.(tuiModel), cmd)
	m = press(t, m, "W")
	next, cmd = m.handleKey(keyEnter)
	m = next.(tuiModel)
	nav, notice := m.workerNav, m.workerDetail.notice
	if m.workerDetail.gen == firstGen || nav.workerID != old.nav.workerID || nav.selectedRunID != old.nav.selectedRunID {
		t.Fatal("fixture did not reenter same worker/run under a new generation")
	}
	next, ignored := m.Update(old)
	m = next.(tuiModel)
	if ignored != nil || m.view != viewWorker || m.workerNav != nav || m.workerDetail.notice != notice {
		t.Fatal("departed worker preflight cleared current navigation wait")
	}
	next, _ = m.Update(cmd())
	m = next.(tuiModel)
	if m.view != viewDetail || m.detail.runID != "B" || !m.detail.runLoaded {
		t.Fatal("current reentry preflight did not navigate")
	}
}
