package main

import (
	"encoding/json"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// PR #1150 review (CodeRabbit, tui.go detailPageMsg): exitToBoard resets m.detail but cannot cancel
// a page command already in flight, and reopening the SAME run passes the runID guard — so a tail
// reply from the previous session used to land in the new one: it cleared tailInFlight, marked the
// pane loaded, and could start a second backfill chain from an obsolete cursor. Every run/page
// command now captures the session generation (detailState.gen, stamped from tuiModel.detailGen on
// each drill-in) and the handlers reject a mismatch before touching state. Exercised with the real
// exit → board → reopen path and a delayed reply from the first session.
func TestTUIDetailStaleSessionPageReplyRejected(t *testing.T) {
	now := time.Now()
	runID := "cccccccc-1111-2222-3333-444444444444"
	fake := &uzicli.FakeClient{
		Runs: []apitypes.RunListItemDTO{{RunDTO: apitypes.RunDTO{ID: runID, Kind: "issue", Status: "running", IssueTitle: "reopened"}}},
		LogsByID: map[string][]apitypes.MessageDTO{runID: {
			msgDTO(1, "text", "lead", "", "", "body-1", now),
			msgDTO(2, "text", "lead", "", "", "body-2", now),
		}},
	}
	fake.GetRunHook = func(id string) (apitypes.RunDTO, error) { return apitypes.RunDTO{ID: id, Status: "running"}, nil }

	// Session A: the `--run` start session (gen 0). Capture its tail command BEFORE leaving, as
	// the in-flight request exitToBoard cannot cancel; execute it later as the "delayed reply".
	m := tuiTestModel(t, fake, runID)
	staleTail := m.loadTailCmd(runID)
	staleRun := m.loadRunCmd(runID)

	// Leave, then reopen the SAME run from the board: session B.
	m = press(t, m, keyEsc)
	if m.view != viewBoard {
		t.Fatalf("esc did not return to the board")
	}
	next, _ := m.Update(boardRunsMsg{reqID: m.board.waitID, runs: fake.Runs})
	m = next.(tuiModel)
	m = press(t, m, keyEnter)
	if m.view != viewDetail || m.detail.runID != runID {
		t.Fatalf("enter did not reopen run %s (view=%v runID=%q)", runID, m.view, m.detail.runID)
	}
	if m.detail.gen == 0 {
		t.Fatalf("reopening from the board did not mint a new session generation")
	}
	genB := m.detail.gen

	// Session A's delayed replies arrive: same runID, older gen. They must be rejected whole —
	// no frames, no tailLoaded, no run applied, no backfill chain started, no command returned.
	calls := len(fake.RunLogsPageCalls)
	next, cmd := m.Update(staleTail())
	m = next.(tuiModel)
	if cmd != nil {
		t.Fatalf("a stale-session tail reply returned a command (a backfill chain from an obsolete cursor)")
	}
	if m.detail.tailLoaded || len(m.detail.frames) != 0 || m.detail.backfilling || m.detail.lowSeq != 0 {
		t.Fatalf("a stale-session tail reply mutated the new session: tailLoaded=%v frames=%d backfilling=%v lowSeq=%d",
			m.detail.tailLoaded, len(m.detail.frames), m.detail.backfilling, m.detail.lowSeq)
	}
	next, _ = m.Update(staleRun())
	m = next.(tuiModel)
	if m.detail.runLoaded {
		t.Fatalf("a stale-session run reply marked the new session runLoaded")
	}
	if got := len(fake.RunLogsPageCalls) - calls; got != 1 { // exactly the stale closure's own request, nothing chained
		t.Fatalf("stale replies caused %d RunLogsPage calls, want exactly 1 (the delayed request itself)", got)
	}
	if m.detail.gen != genB {
		t.Fatalf("session generation changed while rejecting stale replies: %d → %d", genB, m.detail.gen)
	}

	// Session B's own replies (current gen) apply normally.
	next, _ = m.Update(m.loadRunCmd(runID)())
	m = next.(tuiModel)
	next, _ = m.Update(m.loadTailCmd(runID)())
	m = next.(tuiModel)
	if !m.detail.runLoaded || !m.detail.tailLoaded || len(m.detail.frames) != 2 {
		t.Fatalf("session B's own replies did not apply: runLoaded=%v tailLoaded=%v frames=%d",
			m.detail.runLoaded, m.detail.tailLoaded, len(m.detail.frames))
	}
}

// ---- #1151: stream and meta replies carry the session generation too ------------------------

// reopenFromBoard leaves the current detail session and reopens the SAME run from the board, the
// real esc → board → enter path, and returns the new session's model (session B).
func reopenFromBoard(t *testing.T, m tuiModel, fake *uzicli.FakeClient, runID string) tuiModel {
	t.Helper()
	genA := m.detail.gen
	m = press(t, m, keyEsc)
	if m.view != viewBoard {
		t.Fatalf("esc did not return to the board (view=%v)", m.view)
	}
	next, _ := m.Update(boardRunsMsg{reqID: m.board.waitID, runs: fake.Runs})
	m = next.(tuiModel)
	m = press(t, m, keyEnter)
	if m.view != viewDetail || m.detail.runID != runID {
		t.Fatalf("enter did not reopen run %s (view=%v runID=%q)", runID, m.view, m.detail.runID)
	}
	if m.detail.gen == genA {
		t.Fatalf("reopening did not mint a new session generation (still %d)", genA)
	}
	return m
}

// sessionGenFake is a one-run fake whose GetRun reports the title held in *title at call time,
// so a meta reply's origin (stale session vs current) is visible in the applied DTO.
func sessionGenFake(runID string, title *string) *uzicli.FakeClient {
	fake := &uzicli.FakeClient{
		Runs: []apitypes.RunListItemDTO{{RunDTO: apitypes.RunDTO{ID: runID, Kind: "issue", Status: "running", IssueTitle: "board"}}},
	}
	fake.GetRunHook = func(id string) (apitypes.RunDTO, error) {
		return apitypes.RunDTO{ID: id, Status: "running", IssueTitle: *title}, nil
	}
	return fake
}

// streamOf runs an openStreamCmd and returns its streamReadyMsg, registering the stream for
// cleanup so no pump goroutine outlives the test.
func streamOf(t *testing.T, open tea.Cmd) streamReadyMsg {
	t.Helper()
	msg, ok := open().(streamReadyMsg)
	if !ok || msg.err != nil || msg.stream == nil {
		t.Fatalf("openStreamCmd did not yield a usable stream: %#v", msg)
	}
	t.Cleanup(msg.stream.Close)
	return msg
}

// requireStreamClosed observes a stream's close through its Events channel. The receive is
// bounded only so a regression fails instead of hanging; no timing is asserted.
func requireStreamClosed(t *testing.T, s *uzicli.RunStream, what string) {
	t.Helper()
	select {
	case _, ok := <-s.Events():
		if ok {
			t.Fatalf("%s delivered a frame instead of closing", what)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("%s was not closed", what)
	}
}

// requireStreamOpen asserts nothing closed the stream (a fake stream with no events never
// delivers a frame, so a ready receive can only be the close).
func requireStreamOpen(t *testing.T, s *uzicli.RunStream, what string) {
	t.Helper()
	select {
	case _, ok := <-s.Events():
		t.Fatalf("%s is no longer open (receive ok=%v)", what, ok)
	default:
	}
}

// (a) A socket that session A opened lands after the user reopened the same run from the board
// and session B already adopted its own. It must be closed, not adopted: before #1151 it passed
// the runID guard, replaced B's stream (orphaning it) and started a second read chain.
func TestTUIDetailStaleSessionStreamReadyRejected(t *testing.T) {
	runID := "cccccccc-5555-2222-3333-444444444444"
	title := "run"
	fake := sessionGenFake(runID, &title)

	m := tuiTestModel(t, fake, runID) // session A, gen 0
	genA := m.detail.gen
	staleOpen := m.openStreamCmd(runID)

	m = reopenFromBoard(t, m, fake, runID)
	own := streamOf(t, m.openStreamCmd(runID))
	next, cmd := m.Update(own)
	m = next.(tuiModel)
	if cmd == nil || m.detail.stream != own.stream {
		t.Fatalf("session B did not adopt its own stream (cmd=%v)", cmd)
	}

	stale := streamOf(t, staleOpen)
	if stale.gen != genA || stale.runID != runID {
		t.Fatalf("session A's socket reply = (run %q, gen %d), want (%q, %d)", stale.runID, stale.gen, runID, genA)
	}
	next, cmd = m.Update(stale)
	m = next.(tuiModel)
	if cmd != nil {
		t.Fatalf("a stale-session streamReadyMsg returned a command (a second read chain)")
	}
	if m.detail.stream != own.stream {
		t.Fatalf("a stale-session streamReadyMsg replaced session B's stream")
	}
	if m.detail.polling || m.detail.streamErr != nil {
		t.Fatalf("a stale-session streamReadyMsg touched B's transport state: polling=%v streamErr=%v", m.detail.polling, m.detail.streamErr)
	}
	requireStreamClosed(t, stale.stream, "session A's late socket")
	requireStreamOpen(t, own.stream, "session B's stream")
}

// (b) exitToBoard closes session A's stream, so A's in-flight read delivers a closed batch for the
// same runID. Before #1151 that nil-ed the reopened session's stream and flipped it to polling.
func TestTUIDetailStaleSessionStreamEventsRejected(t *testing.T) {
	runID := "cccccccc-6666-2222-3333-444444444444"
	title := "run"
	fake := sessionGenFake(runID, &title)

	m := tuiTestModel(t, fake, runID) // session A, gen 0
	genA := m.detail.gen
	next, staleRead := m.Update(streamOf(t, m.openStreamCmd(runID)))
	m = next.(tuiModel)
	if staleRead == nil {
		t.Fatalf("session A did not start its read chain")
	}

	m = reopenFromBoard(t, m, fake, runID) // esc closes A's stream
	own := streamOf(t, m.openStreamCmd(runID))
	next, _ = m.Update(own)
	m = next.(tuiModel)

	// A's read chain now returns the closed batch (its stream was closed by exitToBoard).
	closedMsg, ok := staleRead().(streamEventsMsg)
	if !ok || !closedMsg.closed || closedMsg.gen != genA || closedMsg.runID != runID {
		t.Fatalf("session A's read did not end in a closed batch for gen %d: %#v", genA, closedMsg)
	}
	next, cmd := m.Update(closedMsg)
	m = next.(tuiModel)
	if cmd != nil {
		t.Fatalf("a stale-session closed batch returned a command (poll fallback or a re-read)")
	}
	if m.detail.stream != own.stream || m.detail.polling || m.detail.streamErr != nil {
		t.Fatalf("a stale-session closed batch hit session B: stream replaced=%v polling=%v streamErr=%v",
			m.detail.stream != own.stream, m.detail.polling, m.detail.streamErr)
	}

	// A non-closed stale batch is dropped too: no frames applied, no re-read of B's stream.
	agent := "lead"
	at := time.Now()
	next, cmd = m.Update(streamEventsMsg{runID: runID, gen: genA, events: []apitypes.RunEventDTO{{
		Type: uzicli.RunEventTypeMessage, Seq: 7, Kind: "text", Agent: &agent, CreatedAt: &at,
		Payload: json.RawMessage(`{"text":"stale frame"}`),
	}}})
	m = next.(tuiModel)
	if cmd != nil {
		t.Fatalf("a stale-session batch returned a command (a second read chain on B's stream)")
	}
	if len(m.detail.frames) != 0 || m.detail.highSeq != 0 {
		t.Fatalf("a stale-session batch applied frames: frames=%d highSeq=%d", len(m.detail.frames), m.detail.highSeq)
	}
	requireStreamOpen(t, own.stream, "session B's stream")
}

// (c) metaSeq restarts per session, so session A's in-flight meta poll (reqID 1) collides with
// session B's first poll (also reqID 1). It must be rejected on gen before the id comparison:
// before #1151 it cleared B's guard and applied A's DTO.
func TestTUIDetailStaleSessionMetaReplyRejected(t *testing.T) {
	runID := "cccccccc-7777-2222-3333-444444444444"
	title := "run"
	fake := sessionGenFake(runID, &title)

	m := tuiTestModel(t, fake, runID) // session A, gen 0
	genA := m.detail.gen
	staleMeta := (&m).startDetailMetaReq()
	if m.detail.metaWaitID != 1 {
		t.Fatalf("session A's first meta poll id = %d, want 1", m.detail.metaWaitID)
	}

	m = reopenFromBoard(t, m, fake, runID)
	title = "fresh"
	next, _ := m.Update(m.loadRunCmd(runID)()) // B's run lands, so applyMeta is live
	m = next.(tuiModel)
	ownMeta := (&m).startDetailMetaReq()
	if m.detail.metaWaitID != 1 {
		t.Fatalf("session B's first meta poll id = %d, want 1 (metaSeq restarts per session)", m.detail.metaWaitID)
	}

	title = "stale session A"
	stale, ok := staleMeta().(detailMetaMsg)
	if !ok || stale.reqID != 1 || stale.gen != genA {
		t.Fatalf("session A's meta reply = %#v, want reqID 1 gen %d", stale, genA)
	}
	next, cmd := m.Update(stale)
	m = next.(tuiModel)
	if cmd != nil {
		t.Fatalf("a stale-session meta reply returned a command")
	}
	if m.detail.metaWaitID != 1 {
		t.Fatalf("a stale-session meta reply cleared session B's guard (metaWaitID=%d)", m.detail.metaWaitID)
	}
	if m.detail.run.IssueTitle != "fresh" {
		t.Fatalf("a stale-session meta reply was applied (IssueTitle=%q)", m.detail.run.IssueTitle)
	}

	// B's own reply applies and clears the guard.
	title = "fresh meta"
	next, _ = m.Update(ownMeta())
	m = next.(tuiModel)
	if m.detail.metaWaitID != 0 || m.detail.run.IssueTitle != "fresh meta" {
		t.Fatalf("session B's own meta reply did not apply: metaWaitID=%d IssueTitle=%q", m.detail.metaWaitID, m.detail.run.IssueTitle)
	}
}

// (d) The same stale-socket race through the PR entry path (PRD #1255 M5): run detail → m (PR
// view) → u (↳ run) reopens the same run in a new session via openLinkedRun.
func TestTUIDetailStaleSessionStreamReadyRejectedViaPR(t *testing.T) {
	runID := prLinkedRunID
	run := apitypes.RunDTO{ID: runID, Status: "running", RepoID: sp("r1"), MrIID: ip(1254), IssueTitle: "a run"}
	detail := prDetail(1254, "approved", []apitypes.CheckDTO{ckPassed("a")}, nil, apitypes.MergeStateDTO{})
	fake := &uzicli.FakeClient{Repos: []apitypes.RepoDTO{oneRepo()}, PullDetailResult: detail}

	m := tuiTestModel(t, fake, runID) // session A, gen 0
	genA := m.detail.gen
	staleOpen := m.openStreamCmd(runID)
	m = applyDetail(m, run, nil)

	m = press(t, m, keyPRView)
	if m.view != viewPR {
		t.Fatalf("m did not open the PR view (view=%v)", m.view)
	}
	next, _ := m.Update(prMsg{reqID: m.pr.waitID, gen: m.pr.gen, detail: detail})
	m = next.(tuiModel)
	m = press(t, m, keyRunLink)
	if m.view != viewDetail || m.detail.runID != runID || m.detail.gen == genA {
		t.Fatalf("u did not reopen run %s in a new session (view=%v runID=%q gen=%d)", runID, m.view, m.detail.runID, m.detail.gen)
	}

	own := streamOf(t, m.openStreamCmd(runID))
	next, cmd := m.Update(own)
	m = next.(tuiModel)
	if cmd == nil || m.detail.stream != own.stream {
		t.Fatalf("the PR-opened session did not adopt its own stream")
	}

	stale := streamOf(t, staleOpen)
	next, cmd = m.Update(stale)
	m = next.(tuiModel)
	if cmd != nil {
		t.Fatalf("a stale-session streamReadyMsg returned a command (a second read chain)")
	}
	if m.detail.stream != own.stream {
		t.Fatalf("a stale-session streamReadyMsg replaced the PR-opened session's stream")
	}
	requireStreamClosed(t, stale.stream, "session A's late socket")
	requireStreamOpen(t, own.stream, "the PR-opened session's stream")
}

// (e) The current session's OWN read chain must keep its generation. Every other test here starts
// session A on the `--run` path at gen 0, where a dropped stamp (the zero value) still equals the
// session's gen, so a slip in readStreamCmd's returns or in a handler's re-read would go unseen.
// A board-opened session has gen >= 1: there such a slip makes the live session discard its own
// frames. Drives the model's own openStreamCmd over a fake stream with message frames, feeds every
// read back through Update, then closes the socket so the chain's closed batch (readStreamCmd's
// first return, a close with no frames queued) is exercised too, and finally round-trips a meta
// poll from the same session. It covers only the handler's default re-read; the two steer re-reads
// are covered by TestTUIDetailBoardOpenedSessionSteerReReadKeepsGen. NOT covered: readStreamCmd's
// "closed while draining a batch" return (events plus closed: true), which needs the close to land
// between two drained frames and cannot be hit deterministically with the fake stream.
func TestTUIDetailBoardOpenedSessionOwnReadChainApplies(t *testing.T) {
	runID := "cccccccc-8888-2222-3333-444444444444"
	title := "run"
	fake := sessionGenFake(runID, &title)
	agent := "lead"
	at := time.Now()
	for seq := int32(1); seq <= 3; seq++ {
		fake.StreamEvents = append(fake.StreamEvents, apitypes.RunEventDTO{
			Type: uzicli.RunEventTypeMessage, Seq: seq, Kind: "text", Agent: &agent, CreatedAt: &at,
			Payload: json.RawMessage(`{"text":"own frame"}`),
		})
	}

	m := tuiTestModel(t, fake, runID) // `--run` session, gen 0
	m = reopenFromBoard(t, m, fake, runID)
	if m.detail.gen == 0 {
		t.Fatalf("the board-opened session has gen 0; this test needs a non-zero generation")
	}
	gen := m.detail.gen
	next, _ := m.Update(m.loadRunCmd(runID)())
	m = next.(tuiModel)

	own := streamOf(t, m.openStreamCmd(runID))
	if own.gen != gen {
		t.Fatalf("openStreamCmd stamped gen %d, want the board-opened session's %d", own.gen, gen)
	}
	next, read := m.Update(own)
	m = next.(tuiModel)
	if read == nil || m.detail.stream != own.stream {
		t.Fatalf("the board-opened session did not adopt its own stream (cmd=%v)", read)
	}

	// Frame reads: a read drains whatever the pump has queued, so the three frames arrive over one
	// or more reads. Every one must be applied and must re-arm the chain; the stream stays open
	// after the last frame, so the final re-read is held, not executed, until the socket closes.
	reads := 0
	for len(m.detail.frames) < len(fake.StreamEvents) {
		if reads == len(fake.StreamEvents) {
			t.Fatalf("%d reads applied only %d of %d frames", reads, len(m.detail.frames), len(fake.StreamEvents))
		}
		batch, ok := read().(streamEventsMsg)
		if !ok || batch.closed || len(batch.events) == 0 {
			t.Fatalf("read %d of the board-opened session's stream = %#v, want a frame batch", reads+1, batch)
		}
		if batch.gen != gen {
			t.Fatalf("read %d stamped gen %d, want %d", reads+1, batch.gen, gen)
		}
		before := len(m.detail.frames)
		next, read = m.Update(batch)
		m = next.(tuiModel)
		reads++
		if got := len(m.detail.frames) - before; got != len(batch.events) {
			t.Fatalf("read %d: %d of its %d frames applied (the session discarded its own batch)", reads, got, len(batch.events))
		}
		if read == nil {
			t.Fatalf("read %d: the session's own batch returned no re-read (the read chain ended)", reads)
		}
	}
	if m.detail.highSeq != 3 || m.detail.stream != own.stream || m.detail.polling {
		t.Fatalf("after the frame reads: highSeq=%d stream kept=%v polling=%v", m.detail.highSeq, m.detail.stream == own.stream, m.detail.polling)
	}

	// The next read, after the socket closes, is the chain's closed batch: it must carry the same
	// gen and be applied (stream dropped, REST poll fallback armed), not discarded.
	own.stream.Close()
	closed, ok := read().(streamEventsMsg)
	if !ok || !closed.closed || closed.gen != gen {
		t.Fatalf("the read after close = %#v, want a closed batch for gen %d", closed, gen)
	}
	next, cmd := m.Update(closed)
	m = next.(tuiModel)
	if cmd == nil || m.detail.stream != nil || !m.detail.polling {
		t.Fatalf("the session's own closed batch was not applied: cmd=%v stream nil=%v polling=%v",
			cmd, m.detail.stream == nil, m.detail.polling)
	}
	if len(m.detail.frames) != len(fake.StreamEvents) {
		t.Fatalf("frames after the closed batch = %d, want %d", len(m.detail.frames), len(fake.StreamEvents))
	}

	// A meta poll from the board-opened session applies and clears its guard.
	metaCmd := (&m).startDetailMetaReq()
	if m.detail.metaWaitID == 0 {
		t.Fatalf("startDetailMetaReq did not arm the meta guard")
	}
	title = "own meta"
	meta, ok := metaCmd().(detailMetaMsg)
	if !ok || meta.gen != gen {
		t.Fatalf("the session's meta reply = %#v, want gen %d", meta, gen)
	}
	next, _ = m.Update(meta)
	m = next.(tuiModel)
	if m.detail.metaWaitID != 0 || m.detail.run.IssueTitle != "own meta" {
		t.Fatalf("the session's own meta reply was not applied: metaWaitID=%d IssueTitle=%q", m.detail.metaWaitID, m.detail.run.IssueTitle)
	}
}

// (f) The streamEventsMsg handler's two steer re-reads (a state frame while the ownership probe is
// unresolved, an input frame while the session may steer) return their own readStreamCmd alongside
// fetchInputsCmd, and each must carry the session generation too. (e) only reaches the default
// re-read, so a zero stamp in either steer branch would pass it; here, in a board-opened session
// (gen >= 1), the batch is executed, its re-read must be stamped with the session's gen, and fed
// back through Update it must be applied. Deterministic on the unbuffered fake stream: the re-read
// runs only while a frame remains; if the trigger's read already drained the frame after it, the
// socket is closed first so the re-read returns the chain's closed batch instead of blocking.
func TestTUIDetailBoardOpenedSessionSteerReReadKeepsGen(t *testing.T) {
	agent := "lead"
	at := time.Now()
	after := apitypes.RunEventDTO{
		Type: uzicli.RunEventTypeMessage, Seq: 1, Kind: "text", Agent: &agent, CreatedAt: &at,
		Payload: json.RawMessage(`{"text":"after the trigger"}`),
	}
	cases := []struct {
		name    string
		trigger apitypes.RunEventDTO
		access  steerAccess
	}{
		{"state frame while access is unknown", apitypes.RunEventDTO{Type: uzicli.RunEventTypeState, Status: "running"}, steerUnknown},
		{"input frame while access is allowed", apitypes.RunEventDTO{Type: uzicli.RunEventTypeInput}, steerAllowed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runID := "cccccccc-9999-2222-3333-444444444444"
			title := "run"
			fake := sessionGenFake(runID, &title)
			fake.InputsByID = map[string][]apitypes.SteerInputDTO{runID: nil} // the caller's own run
			fake.StreamEvents = []apitypes.RunEventDTO{tc.trigger, after}

			m := tuiTestModel(t, fake, runID) // `--run` session, gen 0
			m = reopenFromBoard(t, m, fake, runID)
			if m.detail.gen == 0 {
				t.Fatalf("the board-opened session has gen 0; this test needs a non-zero generation")
			}
			gen := m.detail.gen
			next, _ := m.Update(m.loadRunCmd(runID)())
			m = next.(tuiModel)
			if m.detail.steer.access != steerUnknown {
				t.Fatalf("fixture: steer access = %v before the probe answered, want steerUnknown", m.detail.steer.access)
			}
			m.detail.steer.access = tc.access

			own := streamOf(t, m.openStreamCmd(runID))
			next, read := m.Update(own)
			m = next.(tuiModel)
			if read == nil || m.detail.stream != own.stream {
				t.Fatalf("the board-opened session did not adopt its own stream (cmd=%v)", read)
			}

			trig, ok := read().(streamEventsMsg)
			if !ok || trig.closed || len(trig.events) == 0 || trig.events[0].Type != tc.trigger.Type || trig.gen != gen {
				t.Fatalf("the first read = %#v, want an open %s batch for gen %d", trig, tc.trigger.Type, gen)
			}
			next, cmd := m.Update(trig)
			m = next.(tuiModel)
			if cmd == nil {
				t.Fatalf("the %s batch returned no command", tc.trigger.Type)
			}
			batch, ok := cmd().(tea.BatchMsg)
			if !ok || len(batch) != 2 {
				t.Fatalf("the %s batch returned %#v, want a two-command tea.BatchMsg (re-read + inputs fetch)", tc.trigger.Type, batch)
			}

			drained := len(trig.events) == len(fake.StreamEvents)
			if drained {
				own.stream.Close() // nothing left to read: the re-read must return the closed batch, not block
			}
			var reread *streamEventsMsg
			fetched := 0
			for _, c := range batch {
				switch r := c().(type) {
				case streamEventsMsg:
					reread = &r
				case runInputsMsg:
					if r.runID != runID || r.err != nil {
						t.Fatalf("the inputs fetch = %#v, want the run's own queue", r)
					}
					fetched++
				default:
					t.Fatalf("the steer batch yielded an unexpected %T", r)
				}
			}
			if reread == nil || fetched != 1 {
				t.Fatalf("the steer batch yielded re-read=%v and %d inputs fetches, want one of each", reread != nil, fetched)
			}
			if reread.gen != gen {
				t.Fatalf("the %s branch's re-read stamped gen %d, want the session's %d", tc.trigger.Type, reread.gen, gen)
			}

			next, cmd = m.Update(*reread)
			m = next.(tuiModel)
			if cmd == nil {
				t.Fatalf("the session's own re-read was discarded (no command returned)")
			}
			if drained {
				if !reread.closed || m.detail.stream != nil || !m.detail.polling {
					t.Fatalf("the session's own closed re-read was not applied: closed=%v stream nil=%v polling=%v",
						reread.closed, m.detail.stream == nil, m.detail.polling)
				}
			} else if reread.closed || m.detail.highSeq != 1 || m.detail.stream != own.stream {
				t.Fatalf("the session's own re-read was not applied: closed=%v highSeq=%d stream kept=%v",
					reread.closed, m.detail.highSeq, m.detail.stream == own.stream)
			}
			if len(m.detail.frames) != 1 {
				t.Fatalf("frames = %d, want the one message frame after the trigger", len(m.detail.frames))
			}
		})
	}
}
