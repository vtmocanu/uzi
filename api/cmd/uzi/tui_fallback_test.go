package main

import (
	"context"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// shrinkPollInterval sets pollFallbackInterval to d for the duration of a test and restores it
// after, so drainCmd can execute the re-armed 2s tick without blocking on the real cadence
// (PRD #1137 M7; the same test-seam shape as shrinkPageSize).
func shrinkPollInterval(t *testing.T, d time.Duration) {
	t.Helper()
	prev := pollFallbackInterval
	pollFallbackInterval = d
	t.Cleanup(func() { pollFallbackInterval = prev })
}

// afterQueries returns the After-window RunLogsPage queries recorded since index `from` — the
// catch-up path's signature (After > 0), never a tail or a whole-transcript After:0 fetch.
func afterQueries(fake *uzicli.FakeClient, from int) []uzicli.LogsPageQuery {
	var out []uzicli.LogsPageQuery
	for _, q := range fake.RunLogsPageCalls[from:] {
		if q.After > 0 {
			out = append(out, q)
		}
	}
	return out
}

// M7 / SC4: while the socket is down, two consecutive pollFallbackMsg ticks with NO reply issue
// EXACTLY ONE incremental catch-up page (After: highSeq, never a Tail or After:0) and EXACTLY ONE
// GetRun — the catchupWaitID / metaWaitID guards hold across the second tick so a slow link cannot
// stack a second chain.
func TestTUIDetailPollFallbackIncrementalAndGuarded(t *testing.T) {
	shrinkPollInterval(t, time.Millisecond)
	now := time.Now()
	runID := "fallback-1"
	fake := &uzicli.FakeClient{
		LogsByID: map[string][]apitypes.MessageDTO{runID: {
			msgDTO(1, "text", "lead", "", "", "body-1", now),
			msgDTO(2, "text", "lead", "", "", "body-2", now),
			msgDTO(3, "text", "lead", "", "", "body-3", now),
		}},
	}
	// GetRunHook counts the meta refreshes (there is no static GetRun counter on the fake).
	getRuns := 0
	fake.GetRunHook = func(id string) (apitypes.RunDTO, error) {
		getRuns++
		return apitypes.RunDTO{ID: id, Status: "running"}, nil
	}

	m := tuiTestModel(t, fake, runID)
	// Run + the whole transcript as the tail (seq 1..3): highSeq=3, lowSeq=1, so history is
	// complete and no background backfill is in flight to confuse the counts.
	m = applyDetail(m, apitypes.RunDTO{ID: runID, Status: "running"}, fake.LogsByID[runID])
	if m.detail.highSeq != 3 || !m.detail.historyComplete {
		t.Fatalf("setup: highSeq=%d historyComplete=%v, want 3 / true", m.detail.highSeq, m.detail.historyComplete)
	}
	// The socket cannot open: fall back to polling.
	next, _ := m.Update(streamReadyMsg{runID: runID, err: errFake("stream down")})
	m = next.(tuiModel)
	if !m.detail.polling {
		t.Fatal("setup: a failed stream did not enter polling")
	}

	callsBefore := len(fake.RunLogsPageCalls)

	// First tick: one catch-up (After:3) + one meta GetRun.
	next, cmd := m.Update(pollFallbackMsg{})
	m = next.(tuiModel)
	drainCmd(cmd)
	// Second tick with NO reply delivered: the guards hold, so nothing new is issued.
	next, cmd = m.Update(pollFallbackMsg{})
	m = next.(tuiModel)
	drainCmd(cmd)

	got := afterQueries(fake, callsBefore)
	if len(got) != 1 {
		t.Fatalf("two ticks issued %d catch-up pages, want exactly 1: %+v", len(got), got)
	}
	want := uzicli.LogsPageQuery{After: 3, Limit: detailPageSize, PayloadMax: detailPayloadMax}
	if got[0] != want {
		t.Errorf("catch-up query = %+v, want %+v", got[0], want)
	}
	// Never a tail or an After:0 whole-transcript fetch across the two ticks.
	for _, q := range fake.RunLogsPageCalls[callsBefore:] {
		if q.Tail != 0 {
			t.Errorf("the fallback issued a Tail page %+v; it must be incremental", q)
		}
		if q.After == 0 && q.Before == 0 && q.Tail == 0 {
			t.Errorf("the fallback issued an After:0 whole-transcript fetch %+v", q)
		}
	}
	if getRuns != 1 {
		t.Errorf("two ticks issued %d GetRun calls, want exactly 1 (the metaWaitID guard held)", getRuns)
	}
}

// M7: a detailMetaMsg while POLLING adopts the DTO status wholesale (the stream that normally owns
// status is gone), while STREAMING it preserves the stream's status (the existing applyMeta rule).
func TestTUIDetailMetaStatusAdoptWhilePolling(t *testing.T) {
	runID := "fallback-meta"

	t.Run("polling adopts the DTO status", func(t *testing.T) {
		m := tuiTestModel(t, &uzicli.FakeClient{}, runID)
		m = applyDetail(m, apitypes.RunDTO{ID: runID, Status: "running", IssueTitle: "meta"}, nil)
		m.detail.polling = true
		m.detail.metaWaitID = 1
		next, _ := m.Update(detailMetaMsg{runID: runID, reqID: 1,
			run: apitypes.RunDTO{ID: runID, Status: "completed", IssueTitle: "meta"}})
		m = next.(tuiModel)
		if m.detail.run.Status != "completed" {
			t.Errorf("while polling, status = %q, want the DTO's completed", m.detail.run.Status)
		}
	})

	t.Run("streaming preserves the stream's status", func(t *testing.T) {
		m := tuiTestModel(t, &uzicli.FakeClient{}, runID)
		m = applyDetail(m, apitypes.RunDTO{ID: runID, Status: "running", IssueTitle: "meta"}, nil)
		m.detail.polling = false
		m.detail.metaWaitID = 1
		next, _ := m.Update(detailMetaMsg{runID: runID, reqID: 1,
			run: apitypes.RunDTO{ID: runID, Status: "completed", IssueTitle: "meta refreshed"}})
		m = next.(tuiModel)
		if m.detail.run.Status != "running" {
			t.Errorf("while streaming, status = %q, want the preserved running", m.detail.run.Status)
		}
		if m.detail.run.IssueTitle != "meta refreshed" {
			t.Errorf("the non-status fields did not refresh: title = %q", m.detail.run.IssueTitle)
		}
	})
}

// M7: `r` is fully incremental — it issues a catch-up (After: highSeq) and, when history is
// incomplete, a backfill (Before: lowSeq) — and a second `r` while a catch-up is in flight does NOT
// stack a second catch-up.
func TestTUIDetailRefreshKeyIncremental(t *testing.T) {
	shrinkPageSize(t, 2)
	now := time.Now()
	runID := "fallback-r"
	fake := &uzicli.FakeClient{
		LogsByID: map[string][]apitypes.MessageDTO{runID: {
			msgDTO(1, "text", "lead", "", "", "body-1", now),
			msgDTO(2, "text", "lead", "", "", "body-2", now),
			msgDTO(3, "text", "lead", "", "", "body-3", now),
			msgDTO(4, "text", "lead", "", "", "body-4", now),
			msgDTO(5, "text", "lead", "", "", "body-5", now),
		}},
	}
	m := tuiTestModel(t, fake, runID)
	next, _ := m.Update(detailRunMsg{runID: runID, run: apitypes.RunDTO{ID: runID, Status: "running"}})
	m = next.(tuiModel)
	// Tail lands the newest 2 (seq 4,5): highSeq=5, lowSeq=4, backfill auto-starts.
	next, _ = m.Update(detailPageMsg{runID: runID, kind: pageTail, msgs: fake.LogsByID[runID][3:]})
	m = next.(tuiModel)
	// The backfill page fails, clearing backfilling so `r`'s resume can re-arm it.
	next, _ = m.Update(detailPageMsg{runID: runID, kind: pageBackfill, err: errFake("backfill boom")})
	m = next.(tuiModel)
	if m.detail.backfilling || m.detail.historyComplete {
		t.Fatalf("setup: backfilling=%v historyComplete=%v, want a stalled incomplete walk", m.detail.backfilling, m.detail.historyComplete)
	}

	callsBefore := len(fake.RunLogsPageCalls)
	nm, cmd := m.handleKey(keyRefresh)
	m = nm.(tuiModel)
	drainCmd(cmd)

	var sawCatchup, sawBackfill bool
	for _, q := range fake.RunLogsPageCalls[callsBefore:] {
		if q.After == 5 && q.Limit == detailPageSize {
			sawCatchup = true
		}
		if q.Before == 4 && q.Limit == detailPageSize {
			sawBackfill = true
		}
	}
	if !sawCatchup {
		t.Errorf("r did not issue a catch-up After:5; calls since = %+v", fake.RunLogsPageCalls[callsBefore:])
	}
	if !sawBackfill {
		t.Errorf("r did not issue a backfill Before:4; calls since = %+v", fake.RunLogsPageCalls[callsBefore:])
	}

	// A second `r` while the catch-up chain is in flight (catchupWaitID != 0, no reply delivered)
	// must NOT stack a second catch-up.
	if m.detail.catchupWaitID == 0 {
		t.Fatal("setup: the first r did not leave a catch-up chain in flight")
	}
	callsAfterFirst := len(fake.RunLogsPageCalls)
	nm, cmd = m.handleKey(keyRefresh)
	m = nm.(tuiModel)
	drainCmd(cmd)
	if extra := afterQueries(fake, callsAfterFirst); len(extra) != 0 {
		t.Errorf("a second r stacked %d more catch-up pages, want 0: %+v", len(extra), extra)
	}
}

// M7: streamReadyMsg after a tail page raises the stream's replay floor to the highest seq held,
// so the stream's first reconnect replays only newer frames rather than the whole history.
func TestTUIDetailStreamReadyNotesTailFloor(t *testing.T) {
	now := time.Now()
	runID := "fallback-floor"
	m := tuiTestModel(t, &uzicli.FakeClient{}, runID)
	m = applyDetail(m, apitypes.RunDTO{ID: runID, Status: "running"}, []apitypes.MessageDTO{
		msgDTO(1, "text", "lead", "", "", "body-1", now),
		msgDTO(2, "text", "lead", "", "", "body-2", now),
		msgDTO(3, "text", "lead", "", "", "body-3", now),
	})
	if m.detail.highSeq != 3 {
		t.Fatalf("setup: highSeq = %d, want 3", m.detail.highSeq)
	}
	s := uzicli.NewRunStream(context.Background(), nil)
	defer s.Close()
	next, _ := m.Update(streamReadyMsg{runID: runID, stream: s})
	m = next.(tuiModel)
	if got := s.LastSeen(); got != 3 {
		t.Errorf("streamReadyMsg after a tail page set the replay floor to %d, want highSeq 3", got)
	}
}

// M7: the catch-up chain — a non-empty page merges (seq order, dedup) and chains the next
// After:newHighSeq page under the same id; an empty page ends the chain (catchupWaitID → 0).
func TestTUIDetailCatchupChain(t *testing.T) {
	now := time.Now()
	runID := "fallback-chain"
	fake := &uzicli.FakeClient{}
	m := tuiTestModel(t, fake, runID)
	m = applyDetail(m, apitypes.RunDTO{ID: runID, Status: "running"}, []apitypes.MessageDTO{
		msgDTO(1, "text", "lead", "", "", "body-1", now),
		msgDTO(2, "text", "lead", "", "", "body-2", now),
		msgDTO(3, "text", "lead", "", "", "body-3", now),
	})
	// Mint a catch-up chain (catchupWaitID = 1).
	(&m).startDetailCatchupReq()
	if m.detail.catchupWaitID != 1 {
		t.Fatalf("setup: catchupWaitID = %d, want 1", m.detail.catchupWaitID)
	}

	// A stale/superseded reply (mismatched id) is dropped.
	next, _ := m.Update(detailPageMsg{runID: runID, kind: pageCatchup, reqID: 99,
		msgs: []apitypes.MessageDTO{msgDTO(9, "text", "lead", "", "", "stale", now)}})
	m = next.(tuiModel)
	if len(m.detail.frames) != 3 {
		t.Errorf("a superseded catch-up reply was applied; frames = %d, want 3", len(m.detail.frames))
	}

	// A non-empty page (overlapping seq 3, new 4,5) merges deduped and in seq order, and chains
	// the next page from the new highest seq.
	callsBefore := len(fake.RunLogsPageCalls)
	next, cmd := m.Update(detailPageMsg{runID: runID, kind: pageCatchup, reqID: 1, msgs: []apitypes.MessageDTO{
		msgDTO(3, "text", "lead", "", "", "body-3", now),
		msgDTO(4, "text", "lead", "", "", "body-4", now),
		msgDTO(5, "text", "lead", "", "", "body-5", now),
	}})
	m = next.(tuiModel)
	if got := frameSeqs(m); len(got) != 5 || got[0] != 1 || got[4] != 5 {
		t.Errorf("catch-up merge = %v, want deduped seq-ordered 1..5", got)
	}
	if m.detail.catchupWaitID != 1 {
		t.Errorf("a non-empty catch-up page ended the chain (catchupWaitID = %d), want it to continue", m.detail.catchupWaitID)
	}
	drainCmd(cmd)
	next2 := afterQueries(fake, callsBefore)
	if len(next2) != 1 || next2[0].After != 5 {
		t.Errorf("the chain's next page = %+v, want a single After:5", next2)
	}

	// An empty page ends the chain.
	next, cmd = m.Update(detailPageMsg{runID: runID, kind: pageCatchup, reqID: 1, msgs: nil})
	m = next.(tuiModel)
	if m.detail.catchupWaitID != 0 {
		t.Errorf("an empty catch-up page did not end the chain: catchupWaitID = %d, want 0", m.detail.catchupWaitID)
	}
	if cmd != nil {
		t.Error("an empty catch-up page must not chain another page")
	}
}
