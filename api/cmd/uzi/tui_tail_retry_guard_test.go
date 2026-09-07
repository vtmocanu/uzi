package main

import (
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// tailQueries returns the Tail-window RunLogsPage queries recorded since index `from` — the
// initial-tail (re)load's signature.
func tailQueries(fake *uzicli.FakeClient, from int) []uzicli.LogsPageQuery {
	var out []uzicli.LogsPageQuery
	for _, q := range fake.RunLogsPageCalls[from:] {
		if q.Tail > 0 {
			out = append(out, q)
		}
	}
	return out
}

// PR #1150 review (CodeRabbit, tui.go pollFallbackMsg): the initial-tail RETRY in the stuck state
// (tail errored, nothing held, socket down) had no in-flight guard, so every 2s fallback tick — and
// every `r` — stacked another tail request on a slow link, the #1130 pile-up on the one page that
// was not otherwise guarded. tailInFlight closes it: two consecutive ticks with NO reply issue
// EXACTLY ONE Tail (a positive count, not a vacuous "not two"), an `r` while it is in flight issues
// none, and a pageTail reply (here an error, the case that must NOT wedge) releases the guard so
// the next tick retries again.
func TestTUIDetailTailRetryIsGuardedInFlight(t *testing.T) {
	shrinkPollInterval(t, time.Millisecond)
	now := time.Now()
	runID := "tail-retry-guard"
	fake := &uzicli.FakeClient{
		LogsByID: map[string][]apitypes.MessageDTO{runID: {
			msgDTO(1, "text", "lead", "", "", "body-1", now),
		}},
	}
	fake.GetRunHook = func(id string) (apitypes.RunDTO, error) { return apitypes.RunDTO{ID: id, Status: "running"}, nil }

	m := tuiTestModel(t, fake, runID)
	next, _ := m.Update(detailRunMsg{runID: runID, run: apitypes.RunDTO{ID: runID, Status: "running"}})
	m = next.(tuiModel)
	next, _ = m.Update(detailPageMsg{runID: runID, kind: pageTail, err: errFake("tail boom")})
	m = next.(tuiModel)
	next, _ = m.Update(streamReadyMsg{runID: runID, err: errFake("stream down")})
	m = next.(tuiModel)
	if m.detail.highSeq != 0 || m.detail.pageErr == nil || !m.detail.polling || m.detail.tailInFlight {
		t.Fatalf("setup: want the stuck state with no tail in flight; got highSeq=%d pageErr=%v polling=%v tailInFlight=%v",
			m.detail.highSeq, m.detail.pageErr, m.detail.polling, m.detail.tailInFlight)
	}

	// Two fallback ticks, no reply in between: exactly one Tail request.
	from := len(fake.RunLogsPageCalls)
	next, cmd := m.Update(pollFallbackMsg{})
	m = next.(tuiModel)
	drainCmd(cmd)
	next, cmd = m.Update(pollFallbackMsg{})
	m = next.(tuiModel)
	drainCmd(cmd)
	if got := len(tailQueries(fake, from)); got != 1 {
		t.Fatalf("two fallback ticks with no reply issued %d Tail requests, want exactly 1; calls = %+v", got, fake.RunLogsPageCalls[from:])
	}
	if !m.detail.tailInFlight {
		t.Fatalf("tailInFlight not set after dispatching the retry")
	}

	// `r` while the retry is in flight: no second Tail.
	from = len(fake.RunLogsPageCalls)
	nm, cmd := m.detailKey(keyRefresh)
	m = nm.(tuiModel)
	drainCmd(cmd)
	if got := len(tailQueries(fake, from)); got != 0 {
		t.Fatalf("r while a tail retry is in flight issued %d Tail requests, want 0", got)
	}

	// The retry's reply lands as an ERROR: the guard must release (not wedge), so the next tick
	// retries again — exactly one more Tail.
	next, _ = m.Update(detailPageMsg{runID: runID, kind: pageTail, err: errFake("tail boom again")})
	m = next.(tuiModel)
	if m.detail.tailInFlight {
		t.Fatalf("a pageTail error reply did not release tailInFlight")
	}
	from = len(fake.RunLogsPageCalls)
	_, cmd = m.Update(pollFallbackMsg{})
	drainCmd(cmd)
	if got := len(tailQueries(fake, from)); got != 1 {
		t.Fatalf("after the failed retry's reply the next tick issued %d Tail requests, want exactly 1", got)
	}
}
