package main

import (
	"testing"
	"time"

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
