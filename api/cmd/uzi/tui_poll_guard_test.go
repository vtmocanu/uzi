package main

import (
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// PRD #1130 M1: the periodic board / detail-meta polls carry an in-flight guard so a slow
// or flaky link cannot stack overlapping concurrent requests (bubbletea runs each Cmd in its
// own goroutine). These tests assert the guard with POSITIVE fake-client call counts, never a
// bare "not two", per the repo's vacuous-negative-assertion guidance.

// drainCmd executes a returned tea.Cmd and every inner cmd of a tea.BatchMsg it yields,
// returning the produced messages. Executing fetchRunsCmd / refreshRunMetaCmd this way is what
// makes the FakeClient count its ListRuns / GetRun invocations. tickCmd is executed too, so
// callers that drain a boardTickMsg batch must shrink boardPollInterval first (below) to keep
// the 1ms tick from blocking on the real 2s cadence.
func drainCmd(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		return []tea.Msg{msg}
	}
	var out []tea.Msg
	for _, inner := range batch {
		if inner != nil {
			out = append(out, inner())
		}
	}
	return out
}

func hasMsg[T tea.Msg](msgs []tea.Msg) bool {
	for _, m := range msgs {
		if _, ok := m.(T); ok {
			return true
		}
	}
	return false
}

// Two consecutive periodic board ticks with NO intervening reply must issue EXACTLY ONE real
// ListRuns: the first tick fetches and latches the guard, the second is skipped while the poll
// is in flight. A boardRunsMsg then clears the guard so the next tick fetches again (count → 2).
func TestTUIBoardTickInFlightGuard(t *testing.T) {
	origInterval := boardPollInterval
	boardPollInterval = time.Millisecond
	t.Cleanup(func() { boardPollInterval = origInterval })

	runs := []apitypes.RunListItemDTO{
		{RunDTO: apitypes.RunDTO{ID: "aaaaaaaa-1111-2222-3333-444444444444", Kind: "issue", Status: "running"}},
	}
	fake := &uzicli.FakeClient{Runs: runs}
	m := tuiTestModel(t, fake, "")

	// First periodic tick: issues one board fetch and latches the guard.
	next, cmd := m.Update(boardTickMsg{})
	m = next.(tuiModel)
	if !m.board.boardInFlight {
		t.Fatal("first boardTickMsg did not set boardInFlight; the in-flight guard can never engage")
	}
	drainCmd(cmd) // runs the fetch closure → ListRunsCalls == 1
	if fake.ListRunsCalls != 1 {
		t.Fatalf("first tick issued %d ListRuns, want exactly 1", fake.ListRunsCalls)
	}

	// Second tick with the guard still latched (no reply yet): must NOT issue a fetch.
	next, cmd = m.Update(boardTickMsg{})
	m = next.(tuiModel)
	drainCmd(cmd)
	if fake.ListRunsCalls != 1 {
		t.Fatalf("a second tick while a poll was in flight issued another ListRuns (total %d); the guard did not hold", fake.ListRunsCalls)
	}

	// A board reply clears the guard, so the next tick fetches again.
	next, _ = m.Update(boardRunsMsg{runs: runs})
	m = next.(tuiModel)
	if m.board.boardInFlight {
		t.Fatal("boardRunsMsg did not clear boardInFlight; the periodic poll would wedge after one fetch")
	}
	next, cmd = m.Update(boardTickMsg{})
	m = next.(tuiModel)
	drainCmd(cmd)
	if fake.ListRunsCalls != 2 {
		t.Fatalf("after a reply cleared the guard, the next tick brought the total to %d ListRuns, want 2", fake.ListRunsCalls)
	}
}

// D2's board half: the guard must clear on EVERY board reply, including an admin-mismatch reply
// that apply ignores via its early return. A clear placed behind apply would latch the guard on
// a mid-flight admin toggle and freeze the poll.
func TestTUIBoardTickGuardClearsOnAdminMismatchReply(t *testing.T) {
	origInterval := boardPollInterval
	boardPollInterval = time.Millisecond
	t.Cleanup(func() { boardPollInterval = origInterval })

	runs := []apitypes.RunListItemDTO{
		{RunDTO: apitypes.RunDTO{ID: "aaaaaaaa-1111-2222-3333-444444444444", Kind: "issue", Status: "running"}},
	}
	fake := &uzicli.FakeClient{Runs: runs}
	m := tuiTestModel(t, fake, "")

	// A tick latches the guard (own-runs poll in flight).
	next, cmd := m.Update(boardTickMsg{})
	m = next.(tuiModel)
	drainCmd(cmd)
	if !m.board.boardInFlight {
		t.Fatal("boardTickMsg did not latch boardInFlight")
	}

	// An admin-mismatch reply arrives (b.admin == false, msg.admin == true): apply ignores it,
	// but the guard must still clear in the message case.
	next, _ = m.Update(boardRunsMsg{admin: true, runs: runs})
	m = next.(tuiModel)
	if m.board.boardInFlight {
		t.Fatal("an admin-mismatch boardRunsMsg left boardInFlight latched; the poll would wedge after a mid-flight admin toggle")
	}

	// Proof it is unwedged: the next tick fetches again.
	next, cmd = m.Update(boardTickMsg{})
	m = next.(tuiModel)
	drainCmd(cmd)
	if fake.ListRunsCalls != 2 {
		t.Fatalf("after the ignored reply cleared the guard, total ListRuns is %d, want 2", fake.ListRunsCalls)
	}
}

// D2's detail half — the single non-obvious correctness point of the PRD. The detailMetaMsg case
// early-returns on err != nil (the flaky-connection path), so the meta guard must clear at the TOP
// of the case, above that guard. A failed meta poll must still unlatch metaInFlight and the next
// tick must re-issue the meta refresh.
func TestTUIDetailMetaGuardClearsOnError(t *testing.T) {
	origInterval := boardPollInterval
	boardPollInterval = time.Millisecond
	t.Cleanup(func() { boardPollInterval = origInterval })

	runID := "aaaaaaaa-1111-2222-3333-444444444444"
	run := apitypes.RunDTO{ID: runID, Kind: "issue", Status: "running"}
	fake := &uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{runID: run}}
	m := tuiTestModel(t, fake, runID) // startRun → view == viewDetail

	// Establish the loaded baseline so applyMeta runs and run.ID is set.
	next, _ := m.Update(detailLoadedMsg{runID: runID, run: run})
	m = next.(tuiModel)
	if m.view != viewDetail || m.detail.run.ID == "" || m.detail.polling {
		t.Fatalf("detail precondition not met: view=%v runID=%q polling=%v", m.view, m.detail.run.ID, m.detail.polling)
	}

	// A tick issues the meta refresh and latches its guard.
	next, cmd := m.Update(boardTickMsg{})
	m = next.(tuiModel)
	if !m.detail.metaInFlight {
		t.Fatal("boardTickMsg did not set metaInFlight; the detail-meta poll is unguarded")
	}
	if msgs := drainCmd(cmd); !hasMsg[detailMetaMsg](msgs) {
		t.Fatal("the tick batch did not issue a refreshRunMetaCmd (no detailMetaMsg produced)")
	}

	// The flaky-connection path: a detailMetaMsg carrying err != nil. The marker must clear
	// despite the case's err early-return.
	next, _ = m.Update(detailMetaMsg{runID: runID, err: uzicli.Exitf(uzicli.ExitGeneric, "context deadline exceeded")})
	m = next.(tuiModel)
	if m.detail.metaInFlight {
		t.Fatal("a detailMetaMsg with err != nil did not clear metaInFlight; the detail-meta poll would wedge forever on a flaky link")
	}

	// Proof it is unwedged: the next tick re-issues the meta refresh.
	next, cmd = m.Update(boardTickMsg{})
	m = next.(tuiModel)
	if !m.detail.metaInFlight {
		t.Fatal("after a failed meta poll cleared the guard, the next tick did not re-issue the meta refresh")
	}
	if msgs := drainCmd(cmd); !hasMsg[detailMetaMsg](msgs) {
		t.Fatal("the follow-up tick did not issue a refreshRunMetaCmd after the guard cleared")
	}
}

// D1: a user-initiated manual refresh (r) is NEVER blocked by the in-flight guard, even while a
// periodic poll is pending. It issues its fetch and (so the next tick does not stack on it) keeps
// the marker latched.
func TestTUIManualRefreshNotBlockedByInFlight(t *testing.T) {
	runs := []apitypes.RunListItemDTO{
		{RunDTO: apitypes.RunDTO{ID: "aaaaaaaa-1111-2222-3333-444444444444", Kind: "issue", Status: "running"}},
	}
	fake := &uzicli.FakeClient{Runs: runs}
	m := tuiTestModel(t, fake, "")

	// Simulate a periodic poll already in flight.
	m.board.boardInFlight = true

	next, cmd := m.handleKey(keyRefresh)
	m = next.(tuiModel)
	drainCmd(cmd) // executes the fetch closure → ListRunsCalls increments
	if fake.ListRunsCalls != 1 {
		t.Fatalf("manual r refresh issued %d ListRuns while a poll was in flight, want 1 (never blocked)", fake.ListRunsCalls)
	}
	if !m.board.boardInFlight {
		t.Error("manual r refresh cleared the in-flight marker; it should latch it so the next tick does not stack a second poll")
	}
}
