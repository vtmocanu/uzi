package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// M4 seam 1: after the first GetRun alone (detailRunMsg), the header renders the title and
// status — one round trip before the transcript — while the transcript pane, still waiting on
// its tail page, shows the loading placeholder.
func TestTUIDetailRunFirstHeaderThenLoadingPane(t *testing.T) {
	runID := "prog-1"
	m := tuiTestModel(t, &uzicli.FakeClient{}, runID)

	next, _ := m.Update(detailRunMsg{runID: runID, run: apitypes.RunDTO{
		ID: runID, Kind: "issue", Status: "running", IssueTitle: "progressive load",
	}})
	m = next.(tuiModel)

	if !m.detail.runLoaded {
		t.Fatal("detailRunMsg did not set runLoaded")
	}
	if m.detail.tailLoaded {
		t.Fatal("the tail page has not landed yet; tailLoaded must still be false")
	}
	out := m.View().Content
	// Header is up from the run DTO alone: the title and the status word both render.
	if !strings.Contains(out, "progressive load") {
		t.Errorf("the header title did not render after detailRunMsg alone\n%s", out)
	}
	if !strings.Contains(out, "running") {
		t.Errorf("the header status did not render after detailRunMsg alone\n%s", out)
	}
	// The transcript pane, gated on its own tail page, shows the placeholder.
	if !strings.Contains(out, "loading…") {
		t.Errorf("the transcript pane should show loading… while the tail page is outstanding\n%s", out)
	}
}

// M4 seam 2: after the newest transcript page (detailPageMsg pageTail) the transcript shows the
// newest frames and the pane leaves the loading placeholder.
func TestTUIDetailTailPageRendersNewestFrames(t *testing.T) {
	now := time.Now()
	runID := "prog-2"
	m := tuiTestModel(t, &uzicli.FakeClient{}, runID)

	next, _ := m.Update(detailRunMsg{runID: runID, run: apitypes.RunDTO{
		ID: runID, Kind: "issue", Status: "running", IssueTitle: "tail page",
	}})
	m = next.(tuiModel)

	next, _ = m.Update(detailPageMsg{runID: runID, kind: pageTail, msgs: []apitypes.MessageDTO{
		msgDTO(1, "text", "lead", "", "", "newest frame body", now),
	}})
	m = next.(tuiModel)

	if !m.detail.tailLoaded {
		t.Fatal("the tail page did not set tailLoaded")
	}
	out := m.View().Content
	if !strings.Contains(stripANSI(out), "newest frame body") {
		t.Errorf("the transcript did not render the newest frame after the tail page\n%s", out)
	}
	if strings.Contains(out, "loading…") {
		t.Errorf("the transcript pane still shows the loading placeholder after the tail page\n%s", out)
	}
}

// M4 seam 3: the tail command asks for exactly one page — Tail=detailPageSize,
// PayloadMax=detailPayloadMax — and never a RunLogs/After:0 whole-transcript fetch.
func TestTUIDetailLoadTailUsesOneTailPage(t *testing.T) {
	now := time.Now()
	runID := "prog-3"
	fake := &uzicli.FakeClient{
		RunByID:  map[string]apitypes.RunDTO{runID: {ID: runID, Status: "running"}},
		LogsByID: map[string][]apitypes.MessageDTO{runID: {msgDTO(1, "text", "lead", "", "", "hi", now)}},
	}
	m := tuiTestModel(t, fake, runID)

	// Drive the load command's closure the way the poll-loop tests do, then feed its reply back.
	cmd := m.loadTailCmd(runID)
	msg := cmd()

	if len(fake.RunLogsPageCalls) != 1 {
		t.Fatalf("loadTailCmd issued %d RunLogsPage calls, want exactly 1: %+v", len(fake.RunLogsPageCalls), fake.RunLogsPageCalls)
	}
	want := uzicli.LogsPageQuery{Tail: detailPageSize, PayloadMax: detailPayloadMax}
	if fake.RunLogsPageCalls[0] != want {
		t.Errorf("tail query = %+v, want %+v", fake.RunLogsPageCalls[0], want)
	}
	// The one call is a tail page, not an After:0 whole-transcript fetch.
	if got := fake.RunLogsPageCalls[0]; got.After != 0 || got.Before != 0 || got.Limit != 0 {
		t.Errorf("the tail query carries a non-tail window %+v; it must be Tail-only", got)
	}
	page, ok := msg.(detailPageMsg)
	if !ok {
		t.Fatalf("loadTailCmd produced %T, want detailPageMsg", msg)
	}
	if page.kind != pageTail || page.runID != runID || page.err != nil {
		t.Errorf("detailPageMsg = %+v, want a pageTail for %q with no error", page, runID)
	}
	if len(page.msgs) != 1 {
		t.Errorf("the tail page carried %d messages, want the one seeded", len(page.msgs))
	}
}

// M4: the run-load and tail-page commands are concurrent and share nothing but the run. A
// GetRun error must NOT be swallowed by a tail-page success that lands last — the view shows the
// load error, never a header painted from a zero RunDTO.
func TestTUIDetailRunErrorSurvivesLateTailSuccess(t *testing.T) {
	now := time.Now()
	runID := "prog-err-1"
	m := tuiTestModel(t, &uzicli.FakeClient{}, runID)

	// GetRun fails.
	next, _ := m.Update(detailRunMsg{runID: runID, err: errFake("boom")})
	m = next.(tuiModel)
	// The tail page succeeds and lands AFTER the run error.
	next, _ = m.Update(detailPageMsg{runID: runID, kind: pageTail, msgs: []apitypes.MessageDTO{
		msgDTO(1, "text", "lead", "", "", "some frame", now),
	}})
	m = next.(tuiModel)

	if m.detail.loadErr == nil {
		t.Fatal("the run-load error was swallowed by the tail-page success")
	}
	out := m.View().Content
	if !strings.Contains(out, "could not load this run") {
		t.Errorf("a run-load error must show the load-error message, not a header from a zero run\n%s", out)
	}
}

// M4: conversely, a tail-page error must stay scoped to the transcript pane — the header that a
// successful GetRun already painted stays up, and the pane reports the page failure locally.
func TestTUIDetailTailErrorLeavesHeaderUp(t *testing.T) {
	runID := "prog-err-2"
	m := tuiTestModel(t, &uzicli.FakeClient{}, runID)

	// GetRun succeeds: the header paints.
	next, _ := m.Update(detailRunMsg{runID: runID, run: apitypes.RunDTO{
		ID: runID, Kind: "issue", Status: "running", IssueTitle: "header stays",
	}})
	m = next.(tuiModel)
	// The tail page fails.
	next, _ = m.Update(detailPageMsg{runID: runID, kind: pageTail, err: errFake("tail boom")})
	m = next.(tuiModel)

	if m.detail.loadErr != nil {
		t.Fatal("a tail-page error must not set loadErr (that would collapse the whole view)")
	}
	if m.detail.pageErr == nil {
		t.Fatal("a tail-page error must be recorded in pageErr")
	}
	out := m.View().Content
	if !strings.Contains(out, "header stays") {
		t.Errorf("the header must stay up after a tail-page error\n%s", out)
	}
	if strings.Contains(out, "could not load this run") {
		t.Errorf("a tail-page error must not show the full-view run-load error\n%s", out)
	}
	if !strings.Contains(out, "could not load transcript") {
		t.Errorf("the transcript pane should report the page failure locally\n%s", out)
	}
}

// M4 seam 4: a live frame that beats the tail page renders instead of hiding — the transcript
// gate is tailLoaded || len(frames) > 0, so an early streamEventsMsg is shown, not held.
func TestTUIDetailLiveFrameBeforeTailRenders(t *testing.T) {
	now := time.Now()
	runID := "prog-4"
	m := tuiTestModel(t, &uzicli.FakeClient{}, runID)

	next, _ := m.Update(detailRunMsg{runID: runID, run: apitypes.RunDTO{
		ID: runID, Kind: "issue", Status: "running", IssueTitle: "live first",
	}})
	m = next.(tuiModel)

	// A live frame arrives BEFORE the tail page (tailLoaded still false).
	agent, at := "lead", now
	next, _ = m.Update(streamEventsMsg{runID: runID, events: []apitypes.RunEventDTO{{
		Type: uzicli.RunEventTypeMessage, Seq: 7, Kind: "text", Agent: &agent, CreatedAt: &at,
		Payload: json.RawMessage(`{"text":"beat the tail"}`),
	}}})
	m = next.(tuiModel)

	if m.detail.tailLoaded {
		t.Fatal("the tail page has not landed; tailLoaded must still be false")
	}
	out := m.View().Content
	if !strings.Contains(stripANSI(out), "beat the tail") {
		t.Errorf("a live frame that arrived before the tail page was hidden instead of shown\n%s", out)
	}
	if strings.Contains(out, "loading…") {
		t.Errorf("the transcript still shows loading… even though a live frame is held\n%s", out)
	}
}
