package main

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// PRD #1130 M1: the periodic board / detail-meta polls carry a request-generation guard so a slow
// or flaky link cannot stack overlapping concurrent requests, and an out-of-order reply (bubbletea
// runs each Cmd in its own goroutine and delivers in completion order) cannot clear a newer
// request's guard. These tests assert the guard with POSITIVE fake-client call counts, never a
// bare "not two", per the repo's vacuous-negative-assertion guidance.

// drainCmd executes a returned tea.Cmd and every inner cmd of a tea.BatchMsg it yields,
// returning the produced messages. Executing fetchRunsCmd / refreshRunMetaCmd this way is what
// makes the FakeClient count its ListRuns / GetRun invocations. A reply's re-armed tick is a
// tea.Tick, so a caller that drains a boardRunsMsg batch must shrink boardPollInterval first
// (below) to keep boardTickInterval from blocking on the real 2s cadence.
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

// idleBoard delivers the Init board reply (reqID 1) so the model leaves the seeded in-flight state
// (waitID 1 → 0, tickGen 1 → 2), ready for a periodic tick to issue a fresh board poll.
func idleBoard(t *testing.T, m tuiModel, runs []apitypes.RunListItemDTO) tuiModel {
	t.Helper()
	next, _ := m.Update(boardRunsMsg{reqID: 1, runs: runs})
	nm := next.(tuiModel)
	if nm.board.waitID != 0 {
		t.Fatalf("idleBoard: the Init reply did not clear waitID (got %d)", nm.board.waitID)
	}
	return nm
}

// tick drives one periodic board tick under the CURRENT tick-chain generation (a zero-gen or
// stale-gen tick is dropped), returning the updated model and the returned Cmd.
func tick(t *testing.T, m tuiModel) (tuiModel, tea.Cmd) {
	t.Helper()
	next, cmd := m.Update(boardTickMsg{gen: m.board.tickGen})
	return next.(tuiModel), cmd
}

// Two consecutive periodic board ticks with NO intervening reply must issue EXACTLY ONE real
// ListRuns: the first tick fetches and latches the guard (waitID != 0), the second is skipped
// while the poll is in flight. A matching boardRunsMsg then clears the guard so the next tick
// fetches again (count → 2).
func TestTUIBoardTickInFlightGuard(t *testing.T) {
	origInterval := boardPollInterval
	boardPollInterval = time.Millisecond
	t.Cleanup(func() { boardPollInterval = origInterval })

	runs := []apitypes.RunListItemDTO{
		{RunDTO: apitypes.RunDTO{ID: "aaaaaaaa-1111-2222-3333-444444444444", Kind: "issue", Status: "running"}},
	}
	fake := &uzicli.FakeClient{Runs: runs}
	m := tuiTestModel(t, fake, "")
	m = idleBoard(t, m, runs)

	// First periodic tick: issues one board fetch and latches the guard.
	m, cmd := tick(t, m)
	if m.board.waitID == 0 {
		t.Fatal("first boardTickMsg did not set waitID; the in-flight guard can never engage")
	}
	drainCmd(cmd) // runs the fetch closure → ListRunsCalls == 1
	if fake.ListRunsCalls != 1 {
		t.Fatalf("first tick issued %d ListRuns, want exactly 1", fake.ListRunsCalls)
	}
	waitID := m.board.waitID

	// Second tick with the guard still latched (no reply yet): must NOT issue a fetch.
	m, cmd = tick(t, m)
	drainCmd(cmd)
	if fake.ListRunsCalls != 1 {
		t.Fatalf("a second tick while a poll was in flight issued another ListRuns (total %d); the guard did not hold", fake.ListRunsCalls)
	}

	// The matching board reply clears the guard, so the next tick fetches again.
	next, _ := m.Update(boardRunsMsg{reqID: waitID, runs: runs})
	m = next.(tuiModel)
	if m.board.waitID != 0 {
		t.Fatal("boardRunsMsg did not clear waitID; the periodic poll would wedge after one fetch")
	}
	m, cmd = tick(t, m)
	drainCmd(cmd)
	if fake.ListRunsCalls != 2 {
		t.Fatalf("after a reply cleared the guard, the next tick brought the total to %d ListRuns, want 2", fake.ListRunsCalls)
	}
}

// D2's board half: a reply whose reqID matches waitID must clear the guard EVEN when apply ignores
// it via the admin/own-runs early return. A clear placed behind apply would latch the guard on a
// mid-flight admin toggle and freeze the poll.
func TestTUIBoardTickGuardClearsOnAdminMismatchReply(t *testing.T) {
	origInterval := boardPollInterval
	boardPollInterval = time.Millisecond
	t.Cleanup(func() { boardPollInterval = origInterval })

	runs := []apitypes.RunListItemDTO{
		{RunDTO: apitypes.RunDTO{ID: "aaaaaaaa-1111-2222-3333-444444444444", Kind: "issue", Status: "running"}},
	}
	fake := &uzicli.FakeClient{Runs: runs}
	m := tuiTestModel(t, fake, "")
	m = idleBoard(t, m, runs)

	// A tick latches the guard (own-runs poll in flight).
	m, cmd := tick(t, m)
	drainCmd(cmd)
	if m.board.waitID == 0 {
		t.Fatal("boardTickMsg did not latch waitID")
	}
	waitID := m.board.waitID

	// A CURRENT-reqID admin-mismatch reply arrives (b.admin == false, msg.admin == true): apply
	// ignores it via its early return, but because the reqID matches the case must still clear the
	// guard.
	next, _ := m.Update(boardRunsMsg{reqID: waitID, admin: true, runs: runs})
	m = next.(tuiModel)
	if m.board.waitID != 0 {
		t.Fatal("an admin-mismatch (but reqID-matching) boardRunsMsg left waitID latched; the poll would wedge after a mid-flight admin toggle")
	}

	// Proof it is unwedged: the next tick fetches again.
	m, cmd = tick(t, m)
	drainCmd(cmd)
	if fake.ListRunsCalls != 2 {
		t.Fatalf("after the ignored reply cleared the guard, total ListRuns is %d, want 2", fake.ListRunsCalls)
	}
}

// D2's detail half — the single non-obvious correctness point of the PRD. The detailMetaMsg case
// returns early on err != nil (the flaky-connection path), so the meta guard must be cleared BEFORE
// that early return. A failed meta poll must still unlatch metaWaitID and the next tick must
// re-issue the meta refresh.
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
	m = idleBoard(t, m, nil)

	// A tick issues the meta refresh and latches its guard.
	m, cmd := tick(t, m)
	if m.detail.metaWaitID == 0 {
		t.Fatal("boardTickMsg did not set metaWaitID; the detail-meta poll is unguarded")
	}
	metaWaitID := m.detail.metaWaitID
	if msgs := drainCmd(cmd); !hasMsg[detailMetaMsg](msgs) {
		t.Fatal("the tick batch did not issue a refreshRunMetaCmd (no detailMetaMsg produced)")
	}

	// The flaky-connection path: a detailMetaMsg for THIS run carrying err != nil. The marker must
	// clear despite the case's err early-return.
	next, _ = m.Update(detailMetaMsg{runID: runID, reqID: metaWaitID, err: uzicli.Exitf(uzicli.ExitGeneric, "context deadline exceeded")})
	m = next.(tuiModel)
	if m.detail.metaWaitID != 0 {
		t.Fatal("a detailMetaMsg with err != nil did not clear metaWaitID; the detail-meta poll would wedge forever on a flaky link")
	}

	// Proof it is unwedged: the next tick re-issues the meta refresh.
	m, cmd = tick(t, m)
	if m.detail.metaWaitID == 0 {
		t.Fatal("after a failed meta poll cleared the guard, the next tick did not re-issue the meta refresh")
	}
	if msgs := drainCmd(cmd); !hasMsg[detailMetaMsg](msgs) {
		t.Fatal("the follow-up tick did not issue a refreshRunMetaCmd after the guard cleared")
	}
}

// D1: a user-initiated manual refresh (r) is NEVER blocked by the in-flight guard, even while a
// periodic poll is pending. It issues its fetch and (so the next tick does not stack on it) mints a
// fresh id that becomes the new waitID.
func TestTUIManualRefreshNotBlockedByInFlight(t *testing.T) {
	runs := []apitypes.RunListItemDTO{
		{RunDTO: apitypes.RunDTO{ID: "aaaaaaaa-1111-2222-3333-444444444444", Kind: "issue", Status: "running"}},
	}
	fake := &uzicli.FakeClient{Runs: runs}
	m := tuiTestModel(t, fake, "") // the seeded Init board poll is already in flight (waitID == 1)

	next, cmd := m.handleKey(keyRefresh)
	m = next.(tuiModel)
	drainCmd(cmd) // executes the fetch closure → ListRunsCalls increments
	if fake.ListRunsCalls != 1 {
		t.Fatalf("manual r refresh issued %d ListRuns while a poll was in flight, want 1 (never blocked)", fake.ListRunsCalls)
	}
	if m.board.waitID == 0 {
		t.Error("manual r refresh cleared the in-flight marker; it should mint a fresh waitID so the next tick does not stack a second poll")
	}
}

// Finding 2a: the Init board fetch is counted as in flight, so the first periodic tick does not
// stack a second poll on top of it, and its reply (reqID 1) clears the guard.
func TestTUIInitCountsAsInFlight(t *testing.T) {
	runs := []apitypes.RunListItemDTO{
		{RunDTO: apitypes.RunDTO{ID: "aaaaaaaa-1111-2222-3333-444444444444", Kind: "issue", Status: "running"}},
	}
	fake := &uzicli.FakeClient{Runs: runs}
	m := newTUIModel(context.Background(), fake, "")
	if m.board.waitID != 1 || m.board.reqSeq != 1 || m.board.tickGen != 1 {
		t.Fatalf("Init did not seed the board request in flight: waitID=%d reqSeq=%d tickGen=%d, want 1/1/1",
			m.board.waitID, m.board.reqSeq, m.board.tickGen)
	}
	next, _ := m.Update(boardRunsMsg{reqID: 1, runs: runs})
	m = next.(tuiModel)
	if m.board.waitID != 0 {
		t.Fatalf("the Init reply (reqID 1) did not clear the guard: waitID=%d, want 0", m.board.waitID)
	}
	if m.board.tickGen != 2 {
		t.Fatalf("the Init reply did not bump tickGen: got %d, want 2", m.board.tickGen)
	}
}

// Finding 2 (out-of-order board replies), case (a): a stale reply whose reqID != waitID must NOT
// clear waitID and must NOT change errStreak / runs; the newer matching reply is still honored
// afterward.
func TestTUIBoardStaleReplyIgnored(t *testing.T) {
	origInterval := boardPollInterval
	boardPollInterval = time.Millisecond
	t.Cleanup(func() { boardPollInterval = origInterval })

	runsA := []apitypes.RunListItemDTO{
		{RunDTO: apitypes.RunDTO{ID: "aaaaaaaa-1111-2222-3333-444444444444", Kind: "issue", Status: "running"}},
	}
	runsB := []apitypes.RunListItemDTO{
		{RunDTO: apitypes.RunDTO{ID: "bbbbbbbb-1111-2222-3333-444444444444", Kind: "issue", Status: "running"}},
	}
	fake := &uzicli.FakeClient{Runs: runsA}
	m := tuiTestModel(t, fake, "")
	m = idleBoard(t, m, runsA)

	// A tick mints a fresh outstanding request.
	m, _ = tick(t, m)
	waitID := m.board.waitID
	if waitID == 0 {
		t.Fatal("the tick did not issue a board poll")
	}

	// A stale reply (a reqID that never matched the outstanding one): must be dropped whole.
	next, _ := m.Update(boardRunsMsg{reqID: waitID + 999, err: uzicli.Exitf(uzicli.ExitGeneric, "stale boom")})
	m = next.(tuiModel)
	if m.board.waitID != waitID {
		t.Fatalf("a stale reply cleared the guard: waitID=%d, want the still-outstanding %d", m.board.waitID, waitID)
	}
	if m.board.errStreak != 0 {
		t.Fatalf("a stale error reply inflated errStreak to %d; an out-of-order reply must not touch it", m.board.errStreak)
	}

	// The newer, matching reply is still honored: it clears the guard and applies its runs.
	next, _ = m.Update(boardRunsMsg{reqID: waitID, runs: runsB})
	m = next.(tuiModel)
	if m.board.waitID != 0 {
		t.Fatal("the matching reply did not clear the guard")
	}
	if len(m.board.runs) != 1 || m.board.runs[0].ID != runsB[0].ID {
		t.Fatalf("the matching reply did not apply its runs (got %+v)", m.board.runs)
	}
}

// Finding 2 (out-of-order board replies), case (b): a manual r while a periodic poll is outstanding
// — the periodic reply (older reqID) is ignored, the manual reply (current reqID) is honored.
func TestTUIBoardManualSupersedesPeriodicReply(t *testing.T) {
	runsPeriodic := []apitypes.RunListItemDTO{
		{RunDTO: apitypes.RunDTO{ID: "aaaaaaaa-1111-2222-3333-444444444444", Kind: "issue", Status: "running"}},
	}
	runsManual := []apitypes.RunListItemDTO{
		{RunDTO: apitypes.RunDTO{ID: "bbbbbbbb-1111-2222-3333-444444444444", Kind: "issue", Status: "running"}},
	}
	fake := &uzicli.FakeClient{Runs: runsPeriodic}
	m := tuiTestModel(t, fake, "")
	m = idleBoard(t, m, nil) // idle with an empty board so an applied reply is observable

	// A periodic poll goes out.
	m, _ = tick(t, m)
	periodicID := m.board.waitID
	if periodicID == 0 {
		t.Fatal("the periodic tick did not issue a poll")
	}

	// The user presses r before the periodic reply lands: the manual fetch mints a fresh id that
	// becomes the new waitID, superseding the periodic one.
	m = press(t, m, keyRefresh)
	manualID := m.board.waitID
	if manualID == periodicID {
		t.Fatal("manual r did not mint a fresh request id; it must supersede the outstanding periodic poll")
	}

	// The periodic reply (older reqID) now resolves: it must be ignored.
	next, _ := m.Update(boardRunsMsg{reqID: periodicID, runs: runsPeriodic})
	m = next.(tuiModel)
	if m.board.waitID != manualID {
		t.Fatalf("the stale periodic reply cleared the manual request's guard: waitID=%d, want %d", m.board.waitID, manualID)
	}
	if len(m.board.runs) != 0 {
		t.Fatalf("the stale periodic reply was applied (runs=%+v); it must be dropped", m.board.runs)
	}

	// The manual reply (current reqID) is honored.
	next, _ = m.Update(boardRunsMsg{reqID: manualID, runs: runsManual})
	m = next.(tuiModel)
	if m.board.waitID != 0 {
		t.Fatal("the manual reply did not clear the guard")
	}
	if len(m.board.runs) != 1 || m.board.runs[0].ID != runsManual[0].ID {
		t.Fatalf("the manual reply did not apply its runs (got %+v)", m.board.runs)
	}
}

// Finding 2 (out-of-order board replies), case (c): an admin toggle while a periodic poll is
// outstanding — the pre-toggle periodic reply (stale reqID) is ignored; the admin reply (current
// reqID) is honored, and an admin-denied error reply still renders / behaves as apply dictates.
func TestTUIBoardAdminToggleSupersedesPeriodicReply(t *testing.T) {
	runsPeriodic := []apitypes.RunListItemDTO{
		{RunDTO: apitypes.RunDTO{ID: "aaaaaaaa-1111-2222-3333-444444444444", Kind: "issue", Status: "running"}},
	}
	fake := &uzicli.FakeClient{Runs: runsPeriodic}
	m := tuiTestModel(t, fake, "")
	m = idleBoard(t, m, runsPeriodic)

	// A periodic own-board poll goes out.
	m, _ = tick(t, m)
	periodicID := m.board.waitID
	if periodicID == 0 {
		t.Fatal("the periodic tick did not issue a poll")
	}

	// The user toggles the admin board before the periodic reply lands.
	m = press(t, m, keyAdmin)
	if !m.board.admin {
		t.Fatal("[a] did not turn the admin view on")
	}
	adminID := m.board.waitID
	if adminID == periodicID {
		t.Fatal("the admin toggle did not mint a fresh request id; it must supersede the periodic poll")
	}

	// The pre-toggle periodic reply (stale reqID, own-runs) resolves: ignored.
	next, _ := m.Update(boardRunsMsg{reqID: periodicID, admin: false, runs: runsPeriodic})
	m = next.(tuiModel)
	if m.board.waitID != adminID {
		t.Fatalf("the stale periodic reply cleared the admin request's guard: waitID=%d, want %d", m.board.waitID, adminID)
	}

	// The admin reply (current reqID) is refused by the server: apply records the denial and falls
	// back to own runs, and the reqID-matching case still clears the guard.
	next, _ = m.Update(boardRunsMsg{reqID: adminID, admin: true, err: uzicli.Exitf(uzicli.ExitAuth, "admin access required")})
	m = next.(tuiModel)
	if m.board.waitID != 0 {
		t.Fatal("the admin reply did not clear the guard")
	}
	if m.board.admin {
		t.Error("a refused admin reply left the board in admin mode; apply must fall back to own runs")
	}
	if !m.board.adminDenied {
		t.Error("the admin refusal was not recorded, so it cannot be explained on screen")
	}
	if !strings.Contains(m.View().Content, "admin") {
		t.Errorf("the admin refusal is not rendered\n%s", m.View().Content)
	}
	if m.board.errStreak != 1 {
		t.Fatalf("the admin-refused error reply left errStreak = %d, want 1 (apply increments it)", m.board.errStreak)
	}
}

// Finding 2d: a meta reply for a run the user has navigated AWAY from must be ignored and must NOT
// clear the current run's guard — even when the two share an id (metaSeq restarts per run), which
// is why the case checks runID before comparing the id. A matching-run FAILED meta reply still
// clears the guard (the D2 anti-wedge property).
func TestTUIDetailMetaIgnoresReplyForNavigatedAwayRun(t *testing.T) {
	origInterval := boardPollInterval
	boardPollInterval = time.Millisecond
	t.Cleanup(func() { boardPollInterval = origInterval })

	runA := "aaaaaaaa-1111-2222-3333-444444444444"
	runB := "bbbbbbbb-1111-2222-3333-444444444444"
	fake := &uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{
		runA: {ID: runA, Kind: "issue", Status: "running"},
		runB: {ID: runB, Kind: "issue", Status: "running"},
	}}
	m := tuiTestModel(t, fake, runA)

	// Drill into A and load it, then get a meta poll for A outstanding.
	next, _ := m.Update(detailLoadedMsg{runID: runA, run: apitypes.RunDTO{ID: runA, Kind: "issue", Status: "running"}})
	m = next.(tuiModel)
	m = idleBoard(t, m, nil)
	m, _ = tick(t, m)
	if m.detail.metaWaitID == 0 {
		t.Fatal("no meta poll went out for run A")
	}

	// Navigate to run B: the detail state resets (metaSeq restarts at 0).
	m.detail = newDetailState(runB)
	next, _ = m.Update(detailLoadedMsg{runID: runB, run: apitypes.RunDTO{ID: runB, Kind: "issue", Status: "running"}})
	m = next.(tuiModel)

	// Get a meta poll for B outstanding. Its metaWaitID is 1 (metaSeq restarted) — the SAME id
	// run A's outstanding poll carried, which is exactly the cross-run collision the runID check
	// defends against.
	next, _ = m.Update(boardRunsMsg{reqID: m.board.waitID, runs: nil}) // idle the board again
	m = next.(tuiModel)
	m, _ = tick(t, m)
	if m.detail.metaWaitID != 1 {
		t.Fatalf("run B's meta poll id = %d, want 1 (metaSeq restarts per run)", m.detail.metaWaitID)
	}

	// The late reply for run A (its old runID, reqID 1 == run B's metaWaitID): must be ignored on
	// the runID check and must NOT clear run B's guard.
	next, _ = m.Update(detailMetaMsg{runID: runA, reqID: 1, run: apitypes.RunDTO{ID: runA, Status: "completed"}})
	m = next.(tuiModel)
	if m.detail.metaWaitID != 1 {
		t.Fatalf("a reply for the navigated-away run A cleared run B's guard: metaWaitID=%d, want 1", m.detail.metaWaitID)
	}
	if m.detail.runID != runB {
		t.Fatalf("the current run changed to %q; a reply for another run must not touch it", m.detail.runID)
	}

	// A matching-run FAILED meta reply clears the guard so the next tick retries (D2).
	next, _ = m.Update(detailMetaMsg{runID: runB, reqID: 1, err: uzicli.Exitf(uzicli.ExitGeneric, "context deadline exceeded")})
	m = next.(tuiModel)
	if m.detail.metaWaitID != 0 {
		t.Fatal("a matching-run failed meta reply did not clear metaWaitID; the detail poll would wedge on a flaky link")
	}
}
