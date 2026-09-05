package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// msgEvent builds a message-kind RunEventDTO for the live-stream path (streamEventsMsg), the
// event twin of msgDTO.
func msgEvent(seq int32, kind, agent, text string, at time.Time) apitypes.RunEventDTO {
	a, when := agent, at
	ev := apitypes.RunEventDTO{
		Type: uzicli.RunEventTypeMessage, Seq: seq, Kind: kind, CreatedAt: &when,
		Payload: json.RawMessage(`{"text":` + quoteJSON(text) + `}`),
	}
	if agent != "" {
		ev.Agent = &a
	}
	return ev
}

// shrinkPageSize sets detailPageSize to n for the duration of a test and restores it after, so
// a small seed spans multiple tail + backfill pages (the package var is the test seam, PRD #1137).
func shrinkPageSize(t *testing.T, n int32) {
	t.Helper()
	prev := detailPageSize
	detailPageSize = n
	t.Cleanup(func() { detailPageSize = prev })
}

// frameSeqs returns the held frames' seqs in order, so a test asserts the seq-sorted invariant.
func frameSeqs(m tuiModel) []int32 {
	out := make([]int32, 0, len(m.detail.frames))
	for _, f := range m.detail.frames {
		out = append(out, f.Seq)
	}
	return out
}

func ascending(seqs []int32) bool {
	for i := 1; i < len(seqs); i++ {
		if seqs[i] <= seqs[i-1] {
			return false
		}
	}
	return true
}

// M5 regression: a FAILED initial tail page must NOT latch historyComplete. A tail error adds
// no frames (lowSeq stays 0), and if the completion set were ungated the run would be marked
// "complete" with zero history — and a later successful `r` retry would then never start the
// backfill (the start guard requires !historyComplete). Here the tail errors, then a successful
// tail lands (as `r` would deliver) and the backfill chain starts.
func TestTUIDetailBackfillTailErrorDoesNotCompleteHistory(t *testing.T) {
	shrinkPageSize(t, 2)
	now := time.Now()
	runID := "bf-tailerr"
	m := tuiTestModel(t, &uzicli.FakeClient{}, runID)
	next, _ := m.Update(detailRunMsg{runID: runID, run: apitypes.RunDTO{ID: runID, Kind: "issue", Status: "running"}})
	m = next.(tuiModel)

	// The initial tail page fails.
	next, _ = m.Update(detailPageMsg{runID: runID, kind: pageTail, err: errFake("tail boom")})
	m = next.(tuiModel)
	if m.detail.historyComplete {
		t.Fatal("a failed tail page must not latch historyComplete (zero history was loaded)")
	}

	// A successful tail retry lands the newest page (seqs 4,5) with older history below it; the
	// backfill must now start rather than be blocked by a falsely-latched historyComplete.
	nextM, cmd := m.Update(detailPageMsg{runID: runID, kind: pageTail, msgs: []apitypes.MessageDTO{
		msgDTO(4, "text", "lead", "", "", "body-4", now),
		msgDTO(5, "text", "lead", "", "", "body-5", now),
	}})
	m = nextM.(tuiModel)
	if !m.detail.backfilling {
		t.Fatal("after a successful tail retry with older history, the backfill must start")
	}
	if cmd == nil {
		t.Fatal("the successful tail retry must return the first backfillCmd")
	}
	page, ok := cmd().(detailPageMsg)
	if !ok || page.kind != pageBackfill {
		t.Fatalf("the retry chained %T, want a pageBackfill", cmd())
	}
}

// M5 regression: pressing `r` mid-walk re-issues loadTailCmd; its reply must NOT start a SECOND
// backfill chain over the shared lowSeq cursor while one is already in flight (the !backfilling
// start guard).
func TestTUIDetailBackfillTailWhileBackfillingDoesNotDoubleStart(t *testing.T) {
	shrinkPageSize(t, 2)
	now := time.Now()
	runID := "bf-double"
	m := tuiTestModel(t, &uzicli.FakeClient{}, runID)
	next, _ := m.Update(detailRunMsg{runID: runID, run: apitypes.RunDTO{ID: runID, Kind: "issue", Status: "running"}})
	m = next.(tuiModel)

	// First tail starts the walk (lowSeq 4 > 1).
	next, _ = m.Update(detailPageMsg{runID: runID, kind: pageTail, msgs: []apitypes.MessageDTO{
		msgDTO(4, "text", "lead", "", "", "body-4", now),
		msgDTO(5, "text", "lead", "", "", "body-5", now),
	}})
	m = next.(tuiModel)
	if !m.detail.backfilling {
		t.Fatal("the first tail should have started the backfill")
	}

	// A second tail reply arrives (as a mid-walk `r` would) while backfilling is still true: it
	// must NOT return a second backfillCmd.
	_, cmd := m.Update(detailPageMsg{runID: runID, kind: pageTail, msgs: []apitypes.MessageDTO{
		msgDTO(4, "text", "lead", "", "", "body-4", now),
		msgDTO(5, "text", "lead", "", "", "body-5", now),
	}})
	if cmd != nil {
		t.Fatalf("a tail reply while backfilling must not start a second chain, got %T", cmd())
	}
}

// M5: the tail page starts the background backfill, which walks older history one page at a
// time — Tail{2} → Before{4,2} → Before{2,2} — until it reaches the start, after which the badge
// is gone and historyComplete is set. The exact RunLogsPage query sequence is asserted.
func TestTUIDetailBackfillChainWalksToComplete(t *testing.T) {
	shrinkPageSize(t, 2)
	now := time.Now()
	runID := "bf-chain"
	fake := &uzicli.FakeClient{
		RunByID: map[string]apitypes.RunDTO{runID: {ID: runID, Status: "running"}},
		LogsByID: map[string][]apitypes.MessageDTO{runID: {
			msgDTO(1, "text", "lead", "", "", "body-1", now),
			msgDTO(2, "text", "lead", "", "", "body-2", now),
			msgDTO(3, "text", "lead", "", "", "body-3", now),
			msgDTO(4, "text", "lead", "", "", "body-4", now),
			msgDTO(5, "text", "lead", "", "", "body-5", now),
		}},
	}
	m := tuiTestModel(t, fake, runID)
	next, _ := m.Update(detailRunMsg{runID: runID, run: apitypes.RunDTO{ID: runID, Kind: "issue", Status: "running"}})
	m = next.(tuiModel)

	// The tail command's reply drives the chain: each pageBackfill reply is fed back through
	// Update, which returns the Cmd for the next page (nil once the walk stops).
	cmd := m.loadTailCmd(runID)
	for cmd != nil {
		msg := cmd()
		next, cmd = m.Update(msg)
		m = next.(tuiModel)
	}

	want := []uzicli.LogsPageQuery{
		{Tail: 2, PayloadMax: detailPayloadMax},
		{Before: 4, Limit: 2, PayloadMax: detailPayloadMax},
		{Before: 2, Limit: 2, PayloadMax: detailPayloadMax},
	}
	if len(fake.RunLogsPageCalls) != len(want) {
		t.Fatalf("backfill chain issued %d RunLogsPage calls, want %d: %+v", len(fake.RunLogsPageCalls), len(want), fake.RunLogsPageCalls)
	}
	for i := range want {
		if fake.RunLogsPageCalls[i] != want[i] {
			t.Errorf("call[%d] = %+v, want %+v", i, fake.RunLogsPageCalls[i], want[i])
		}
	}
	if !m.detail.historyComplete {
		t.Error("the walk reached the start of history but historyComplete is still false")
	}
	if m.detail.backfilling {
		t.Error("backfilling must be false once the walk is complete")
	}
	if got := frameSeqs(m); len(got) != 5 || !ascending(got) {
		t.Errorf("held frames = %v, want the 5 seeded frames ascending", got)
	}
	if bf := stripANSI(m.backfillBadge()); bf != "" {
		t.Errorf("the backfill badge must be empty once history is complete, got %q", bf)
	}
}

// M5: an empty backfill page marks the walk complete (the start-of-history terminal), clearing
// backfilling and the badge.
func TestTUIDetailBackfillEmptyPageCompletes(t *testing.T) {
	shrinkPageSize(t, 2)
	now := time.Now()
	runID := "bf-empty"
	fake := &uzicli.FakeClient{LogsByID: map[string][]apitypes.MessageDTO{runID: {
		msgDTO(4, "text", "lead", "", "", "body-4", now),
		msgDTO(5, "text", "lead", "", "", "body-5", now),
	}}}
	m := tuiTestModel(t, fake, runID)
	next, _ := m.Update(detailRunMsg{runID: runID, run: apitypes.RunDTO{ID: runID, Status: "running"}})
	m = next.(tuiModel)
	// The tail page lands and starts the backfill (lowSeq=4>1).
	next, _ = m.Update(detailPageMsg{runID: runID, kind: pageTail, msgs: fake.LogsByID[runID]})
	m = next.(tuiModel)
	if !m.detail.backfilling {
		t.Fatal("the tail page should have started the backfill")
	}
	// An empty backfill page: the start of history.
	next, _ = m.Update(detailPageMsg{runID: runID, kind: pageBackfill, msgs: nil})
	m = next.(tuiModel)
	if !m.detail.historyComplete || m.detail.backfilling {
		t.Fatalf("an empty backfill page must complete the walk: historyComplete=%v backfilling=%v", m.detail.historyComplete, m.detail.backfilling)
	}
	if bf := stripANSI(m.backfillBadge()); bf != "" {
		t.Errorf("badge must be empty after completion, got %q", bf)
	}
}

// M5: a backfill page prepends older history — frames stay seq-sorted, the older agent's lane
// appears, and the selection KEY is unchanged across the prepend (selection is by key, not index,
// so the aggregated lane inserted at index 0 does not swap the transcript the user is reading).
func TestTUIDetailBackfillPrependKeepsSelectionAndSort(t *testing.T) {
	shrinkPageSize(t, 2)
	now := time.Now()
	runID := "bf-prepend"
	fake := &uzicli.FakeClient{LogsByID: map[string][]apitypes.MessageDTO{runID: {
		msgDTO(1, "text", "tester", "", "", "old-1", now),
		msgDTO(2, "text", "tester", "", "", "old-2", now),
		msgDTO(3, "text", "tester", "", "", "old-3", now),
		msgDTO(4, "text", "lead", "", "", "new-4", now),
		msgDTO(5, "text", "lead", "", "", "new-5", now),
	}}}
	m := tuiTestModel(t, fake, runID)
	next, _ := m.Update(detailRunMsg{runID: runID, run: apitypes.RunDTO{ID: runID, Status: "running"}})
	m = next.(tuiModel)

	// Tail: only the lead's newest two frames → a single lead lane.
	next, _ = m.Update(detailPageMsg{runID: runID, kind: pageTail, msgs: fake.LogsByID[runID][3:]})
	m = next.(tuiModel)
	lanesBefore := len(m.detail.lanes)
	selKeyBefore := m.detail.lanes[m.detail.laneIdx].Key

	// A backfill page brings the older tester frames.
	next, _ = m.Update(detailPageMsg{runID: runID, kind: pageBackfill, msgs: fake.LogsByID[runID][:3]})
	m = next.(tuiModel)

	if got := frameSeqs(m); len(got) != 5 || !ascending(got) {
		t.Errorf("frames after prepend = %v, want 5 ascending", got)
	}
	if len(m.detail.lanes) <= lanesBefore {
		t.Errorf("the older agent's lane did not appear: %d lanes before, %d after", lanesBefore, len(m.detail.lanes))
	}
	// The tester lane is now present.
	tester := false
	for _, l := range m.detail.lanes {
		if l.Role == "tester" {
			tester = true
		}
	}
	if !tester {
		t.Errorf("the older tester lane is missing after the prepend: %+v", m.detail.lanes)
	}
	if got := m.detail.lanes[m.detail.laneIdx].Key; got != selKeyBefore {
		t.Errorf("the selection key jumped across the prepend: was %q, now %q", selKeyBefore, got)
	}
}

// M5: a live frame (newest seq) interleaved between backfill pages merges in seq order with no
// duplicate — the seq-sorted, seen-deduped log does not care which transport delivered a frame.
func TestTUIDetailBackfillInterleavesLiveFrame(t *testing.T) {
	shrinkPageSize(t, 2)
	now := time.Now()
	runID := "bf-interleave"
	fake := &uzicli.FakeClient{LogsByID: map[string][]apitypes.MessageDTO{runID: {
		msgDTO(2, "text", "lead", "", "", "body-2", now),
		msgDTO(3, "text", "lead", "", "", "body-3", now),
		msgDTO(4, "text", "lead", "", "", "body-4", now),
		msgDTO(5, "text", "lead", "", "", "body-5", now),
	}}}
	m := tuiTestModel(t, fake, runID)
	next, _ := m.Update(detailRunMsg{runID: runID, run: apitypes.RunDTO{ID: runID, Status: "running"}})
	m = next.(tuiModel)

	// Tail: [4,5].
	next, _ = m.Update(detailPageMsg{runID: runID, kind: pageTail, msgs: fake.LogsByID[runID][2:]})
	m = next.(tuiModel)

	// A live frame (newest seq 6) arrives before the next backfill page.
	next, _ = m.Update(streamEventsMsg{runID: runID, events: []apitypes.RunEventDTO{
		msgEvent(6, "text", "lead", "live-6", now),
	}})
	m = next.(tuiModel)

	// A backfill page brings the older [2,3].
	next, _ = m.Update(detailPageMsg{runID: runID, kind: pageBackfill, msgs: fake.LogsByID[runID][:2]})
	m = next.(tuiModel)

	if got := frameSeqs(m); len(got) != 5 || !ascending(got) {
		t.Fatalf("frames = %v, want [2 3 4 5 6] ascending", got)
	}
	want := []int32{2, 3, 4, 5, 6}
	for i := range want {
		if frameSeqs(m)[i] != want[i] {
			t.Fatalf("frames = %v, want %v", frameSeqs(m), want)
		}
	}
	// A duplicate live frame (seq 4, already held) is deduped: the length does not grow.
	next, _ = m.Update(streamEventsMsg{runID: runID, events: []apitypes.RunEventDTO{
		msgEvent(4, "text", "lead", "dup-4", now),
	}})
	m = next.(tuiModel)
	if got := frameSeqs(m); len(got) != 5 {
		t.Errorf("a duplicate live frame was not deduped: frames = %v", got)
	}
}

// M5: a paused viewport keeps its top line across a prepend — the scroll anchor pushes scroll down
// by the prepended line-count delta so the line under the cursor stays put (§ Scroll anchoring).
func TestTUIDetailBackfillAnchorsPausedViewport(t *testing.T) {
	shrinkPageSize(t, 4)
	now := time.Now()
	runID := "bf-anchor"
	fake := &uzicli.FakeClient{LogsByID: map[string][]apitypes.MessageDTO{runID: {
		msgDTO(1, "text", "lead", "", "", "body-1", now),
		msgDTO(2, "text", "lead", "", "", "body-2", now),
		msgDTO(3, "text", "lead", "", "", "body-3", now),
		msgDTO(4, "text", "lead", "", "", "body-4", now),
		msgDTO(5, "text", "lead", "", "", "body-5", now),
		msgDTO(6, "text", "lead", "", "", "body-6", now),
		msgDTO(7, "text", "lead", "", "", "body-7", now),
		msgDTO(8, "text", "lead", "", "", "body-8", now),
	}}}
	m := tuiTestModel(t, fake, runID)
	// A small terminal so the viewport is shorter than the content and the window actually scrolls.
	m.height = 7
	next, _ := m.Update(detailRunMsg{runID: runID, run: apitypes.RunDTO{ID: runID, Status: "running"}})
	m = next.(tuiModel)

	// Tail: the newest 4 frames [5,6,7,8].
	next, _ = m.Update(detailPageMsg{runID: runID, kind: pageTail, msgs: fake.LogsByID[runID][4:]})
	m = next.(tuiModel)

	// Pause and pick a top line that is not clamped to the bottom.
	m.detail.follow = false
	m.detail.scroll = 2
	topBefore := topVisibleLine(t, m)

	// A backfill page prepends the older [1,2,3,4].
	next, _ = m.Update(detailPageMsg{runID: runID, kind: pageBackfill, msgs: fake.LogsByID[runID][:4]})
	m = next.(tuiModel)

	if m.detail.scroll <= 2 {
		t.Errorf("scroll was not pushed down by the prepend delta: still %d", m.detail.scroll)
	}
	topAfter := topVisibleLine(t, m)
	if topBefore != topAfter {
		t.Errorf("the paused viewport's top line jumped across the prepend:\nbefore: %q\nafter:  %q", stripANSI(topBefore), stripANSI(topAfter))
	}
}

// M5: a failed backfill page shows the unavailable badge (not a full-pane error) and leaves the
// walk incomplete; `r` then resumes it from lowSeq with a Before page.
func TestTUIDetailBackfillFailedPageBadgeAndResume(t *testing.T) {
	shrinkPageSize(t, 2)
	now := time.Now()
	runID := "bf-failed"
	fake := &uzicli.FakeClient{
		RunByID: map[string]apitypes.RunDTO{runID: {ID: runID, Status: "running"}},
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
	// Tail lands, starts the backfill (lowSeq=4).
	next, _ = m.Update(detailPageMsg{runID: runID, kind: pageTail, msgs: fake.LogsByID[runID][3:]})
	m = next.(tuiModel)
	// The backfill page fails.
	next, _ = m.Update(detailPageMsg{runID: runID, kind: pageBackfill, err: errFake("backfill boom")})
	m = next.(tuiModel)

	if m.detail.backfilling {
		t.Error("a failed backfill page must clear backfilling")
	}
	if !m.detail.backfillFailed {
		t.Error("a failed backfill page must set backfillFailed")
	}
	if m.detail.historyComplete {
		t.Error("a failed backfill page must NOT complete the walk")
	}
	if m.detail.pageErr != nil {
		t.Error("a backfill failure is a badge, not a full-pane page error")
	}
	if bf := stripANSI(m.backfillBadge()); !strings.Contains(bf, "earlier history unavailable") {
		t.Errorf("the failed-backfill badge must invite retry, got %q", bf)
	}

	// `r` resumes from lowSeq: a Before{lowSeq} page is issued.
	lowSeq := m.detail.lowSeq
	callsBefore := len(fake.RunLogsPageCalls)
	nm, cmd := m.handleKey(keyRefresh)
	m = nm.(tuiModel)
	if !m.detail.backfilling || m.detail.backfillFailed {
		t.Errorf("r must re-arm the backfill: backfilling=%v backfillFailed=%v", m.detail.backfilling, m.detail.backfillFailed)
	}
	drainCmd(cmd)
	found := false
	for _, q := range fake.RunLogsPageCalls[callsBefore:] {
		if q.Before == lowSeq && q.Limit == detailPageSize {
			found = true
		}
	}
	if !found {
		t.Errorf("r did not issue a Before{%d} backfill page; calls since = %+v", lowSeq, fake.RunLogsPageCalls[callsBefore:])
	}
}

// M5: a page whose frames do not go below `before` (a hostile/broken server, or an all-duplicate
// page) stops the chain — the mirror of RunLogs' "did not advance" guard.
func TestTUIDetailBackfillDidNotAdvanceStops(t *testing.T) {
	shrinkPageSize(t, 2)
	now := time.Now()
	runID := "bf-stall"
	fake := &uzicli.FakeClient{LogsByID: map[string][]apitypes.MessageDTO{runID: {
		msgDTO(4, "text", "lead", "", "", "body-4", now),
		msgDTO(5, "text", "lead", "", "", "body-5", now),
	}}}
	m := tuiTestModel(t, fake, runID)
	next, _ := m.Update(detailRunMsg{runID: runID, run: apitypes.RunDTO{ID: runID, Status: "running"}})
	m = next.(tuiModel)
	next, _ = m.Update(detailPageMsg{runID: runID, kind: pageTail, msgs: fake.LogsByID[runID]})
	m = next.(tuiModel)
	if !m.detail.backfilling {
		t.Fatal("the tail page should have started the backfill")
	}
	// A page that returns only already-held (duplicate) frames does not lower the cursor.
	next, _ = m.Update(detailPageMsg{runID: runID, kind: pageBackfill, msgs: fake.LogsByID[runID]})
	m = next.(tuiModel)

	if m.detail.backfilling {
		t.Error("a did-not-advance page must stop the chain")
	}
	if !m.detail.backfillFailed {
		t.Error("a did-not-advance page must set backfillFailed")
	}
	if m.detail.historyComplete {
		t.Error("a did-not-advance page must NOT mark history complete")
	}
}

// topVisibleLine mirrors renderTranscript's bottom-anchored window computation to return the
// selected lane's first visible line, so a test can assert it is unchanged across a prepend.
func topVisibleLine(t *testing.T, m tuiModel) string {
	t.Helper()
	lane, ok := m.detail.selectedLane()
	if !ok {
		t.Fatal("no lane selected")
	}
	lines := m.buildTranscriptLines(lane)
	vp := m.transcriptViewport()
	maxTop := len(lines) - vp
	if maxTop < 0 {
		maxTop = 0
	}
	top := m.detail.scroll
	if m.detail.follow || top > maxTop {
		top = maxTop
	}
	if top < 0 {
		top = 0
	}
	if top >= len(lines) {
		t.Fatalf("computed top %d out of %d lines", top, len(lines))
	}
	return lines[top]
}
