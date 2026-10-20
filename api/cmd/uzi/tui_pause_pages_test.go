package main

import (
	tea "charm.land/bubbletea/v2"
	"errors"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
	"strings"
	"testing"
	"time"
)

func pausePagesSeed(t *testing.T, seq int32) (tuiModel, *uzicli.FakeClient) {
	t.Helper()
	at := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	f := &uzicli.FakeClient{LogsByID: map[string][]apitypes.MessageDTO{"r1": {pauseMarker(seq, at, "owner")}}}
	m := tuiTestModel(t, f, "r1")
	m.detail.applyRun(apitypes.RunDTO{ID: "r1", Kind: "prompt", Status: "paused", StatusSince: &at, UpdatedAt: at}, nil)
	return m, f
}
func pausePagesUpdate(m *tuiModel, msg tea.Msg) tea.Cmd {
	n, c := m.Update(msg)
	*m = n.(tuiModel)
	return c
}
func pausePagesCheck(t *testing.T, m tuiModel, label, want string) {
	t.Helper()
	got := m.detail.pauseOrigin()
	rendered := strings.Contains(stripANSI(m.View().Content), "paused by you")
	t.Logf("%s: origin=%q ownerRendered=%v tailReady=%v suffixSet=%v suffixAfter=%d high=%d wait=%d failed=%v metaReady=%v gen=%d epoch=%d pageEpoch=%d", label, got, rendered, m.detail.pauseTailReady, m.detail.pauseSuffixSet, m.detail.pauseSuffixAfter, m.detail.highSeq, m.detail.catchupWaitID, m.detail.pauseCatchupFailed, m.detail.pauseMetaReady, m.detail.gen, m.detail.pauseEpoch, m.detail.pausePageEpoch)
	if got != want {
		t.Errorf("%s: origin=%q want=%q", label, got, want)
	}
	if rendered != (got == "owner") {
		t.Errorf("%s: rendered owner=%v inconsistent with origin=%q", label, rendered, got)
	}
}

func TestTUIPauseOriginPagesFullRecovery(t *testing.T) {
	m, f := pausePagesSeed(t, 1)
	pausePagesCheck(t, m, "initial", "")
	f.Err = errors.New("initial tail offline")
	pausePagesUpdate(&m, m.loadTailCmd("r1")())
	pausePagesCheck(t, m, "tail failed", "")
	f.Err = nil
	m.detail.catchupWaitID = 1
	page := m.catchupCmd("r1", 0, 1)().(detailPageMsg)
	if page.err != nil || len(page.msgs) != 1 || page.msgs[0].Seq != 1 || page.reqID != 1 || page.gen != m.detail.gen {
		t.Fatalf("invalid catchup fixture: %+v", page)
	}
	next := pausePagesUpdate(&m, page)
	pausePagesCheck(t, m, "catchup in flight", "")
	if next == nil {
		t.Fatal("missing chained EOF command")
	}
	eof := next().(detailPageMsg)
	if eof.err != nil || len(eof.msgs) != 0 || eof.reqID != 1 || eof.gen != m.detail.gen {
		t.Fatalf("invalid EOF: %+v", eof)
	}
	pausePagesUpdate(&m, eof)
	pausePagesCheck(t, m, "complete current catchup from zero", "owner")
}

func TestTUIPauseOriginPagesOldTailAfterFailure(t *testing.T) {
	m, f := pausePagesSeed(t, 100)
	pausePagesUpdate(&m, m.loadTailCmd("r1")())
	pausePagesCheck(t, m, "valid tail", "owner")
	oldTail := m.loadTailCmd("r1")()
	m.detail.catchupWaitID = 1
	f.Err = errors.New("current catchup offline")
	failed := m.catchupCmd("r1", 100, 1)().(detailPageMsg)
	if failed.err == nil || failed.reqID != 1 || failed.gen != m.detail.gen {
		t.Fatalf("invalid failed response: %+v", failed)
	}
	pausePagesUpdate(&m, failed)
	pausePagesCheck(t, m, "current catchup failed", "")
	pausePagesUpdate(&m, oldTail)
	pausePagesCheck(t, m, "pre-failure tail delivered late", "")
	f.Err = nil
	pausePagesUpdate(&m, m.loadTailCmd("r1")())
	pausePagesCheck(t, m, "fresh post-failure tail", "owner")
}

func TestTUIPauseOriginPagesOldEOFThenFreshEOF(t *testing.T) {
	m, f := pausePagesSeed(t, 100)
	pausePagesUpdate(&m, m.loadTailCmd("r1")())
	pausePagesCheck(t, m, "valid tail", "owner")
	m.detail.catchupWaitID = 1
	oldEOF := m.catchupCmd("r1", 100, 1)().(detailPageMsg)
	if oldEOF.err != nil || len(oldEOF.msgs) != 0 || oldEOF.reqID != 1 || oldEOF.gen != m.detail.gen {
		t.Fatalf("invalid old EOF: %+v", oldEOF)
	}
	f.Err = errors.New("new tail offline")
	pausePagesUpdate(&m, m.loadTailCmd("r1")())
	pausePagesCheck(t, m, "new tail failed", "")
	pausePagesUpdate(&m, oldEOF)
	pausePagesCheck(t, m, "pre-failure EOF delivered late", "")
	f.Err = nil
	m.detail.catchupWaitID = 2
	freshEOF := m.catchupCmd("r1", 100, 2)().(detailPageMsg)
	if freshEOF.err != nil || len(freshEOF.msgs) != 0 || freshEOF.reqID != 2 || freshEOF.gen != m.detail.gen {
		t.Fatalf("invalid fresh EOF: %+v", freshEOF)
	}
	pausePagesUpdate(&m, freshEOF)
	pausePagesCheck(t, m, "fresh complete suffix catchup", "owner")
}

func TestTUIPauseOriginPagesInvalidation(t *testing.T) {
	for _, failure := range []string{"nonadvancing", "openFailure", "closed"} {
		t.Run(failure, func(t *testing.T) {
			m, f := pausePagesSeed(t, 100)
			pausePagesUpdate(&m, m.loadTailCmd("r1")())
			oldTail := m.loadTailCmd("r1")
			m.detail.catchupWaitID = 1
			oldEOF := m.catchupCmd("r1", 100, 1)
			switch failure {
			case "nonadvancing":
				// A successful HTTP read returning only a duplicate is not EOF proof.
				pausePagesUpdate(&m, m.catchupCmd("r1", 99, 1)())
				m.detail.catchupWaitID = 1
			case "openFailure":
				pausePagesUpdate(&m, streamReadyMsg{runID: "r1", gen: m.detail.gen, err: errors.New("offline")})
			case "closed":
				pausePagesUpdate(&m, streamEventsMsg{runID: "r1", gen: m.detail.gen, closed: true})
			}
			// Refresh metadata independently so it cannot mask a stale page's restoration.
			m.detail.applyRun(m.detail.run, nil)
			pausePagesUpdate(&m, oldTail())
			pausePagesUpdate(&m, oldEOF())
			pausePagesCheck(t, m, "queued pages after invalidation", "")
			// Old history failure is only a transcript badge and cannot retire fresh proof.
			m.detail.catchupWaitID = 2
			pausePagesUpdate(&m, m.catchupCmd("r1", 100, 2)())
			pausePagesCheck(t, m, "fresh EOF", "owner")
			f.Err = errors.New("old history offline")
			pausePagesUpdate(&m, m.backfillCmd("r1", 100)())
			pausePagesCheck(t, m, "older backfill error", "owner")
		})
	}
}

func TestTUIPauseOriginPagesEmptyEOF(t *testing.T) {
	m, f := pausePagesSeed(t, 1)
	f.LogsByID["r1"] = nil
	f.Err = errors.New("tail offline")
	pausePagesUpdate(&m, m.loadTailCmd("r1")())
	f.Err = nil
	m.detail.catchupWaitID = 1
	pausePagesUpdate(&m, m.catchupCmd("r1", 0, 1)())
	if !m.detail.pauseTailReady || !m.detail.pauseSuffixSet || m.detail.pauseSuffixAfter != 0 {
		t.Fatal("current empty EOF did not establish the zero baseline")
	}
	pausePagesCheck(t, m, "empty current EOF", "")
	// A live marker at seq 2 must retain the missing seq 1 in the empty baseline.
	since := *m.detail.run.StatusSince
	pausePagesUpdate(&m, streamEventsMsg{runID: "r1", gen: m.detail.gen, events: []apitypes.RunEventDTO{{Type: uzicli.RunEventTypeMessage, Seq: 2, Kind: "paused", Payload: pauseMarker(2, since, "owner").Payload, CreatedAt: &since}}})
	pausePagesCheck(t, m, "empty EOF with later gap", "")
}
