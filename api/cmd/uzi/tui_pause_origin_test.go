package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func TestTUIPauseOriginEpochAndCoverage(t *testing.T) {
	at := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	run := apitypes.RunDTO{ID: "r1", Kind: "prompt", Status: "paused", StatusSince: &at, UpdatedAt: at}
	m := tuiTestModel(t, &uzicli.FakeClient{}, "r1")
	update := func(msg any) { t.Helper(); next, _ := m.Update(msg); m = next.(tuiModel) }
	check := func(want string) {
		t.Helper()
		if got := m.detail.pauseOrigin(); got != want {
			t.Fatalf("origin %q want %q", got, want)
		}
		out := stripANSI(m.View().Content)
		label := "paused"
		if want == "owner" {
			label = "paused by you"
		}
		if want == "lead_park" {
			label = "paused by the run's lead"
		}
		if !strings.Contains(out, "‖ "+label) {
			t.Fatalf("missing %q: %s", label, out)
		}
		if want == "" && strings.Contains(out, "paused by") {
			t.Fatalf("unsupported attribution: %s", out)
		}
	}
	update(detailRunMsg{runID: "r1", run: run})
	check("")
	update(detailPageMsg{runID: "r1", kind: pageTail, msgs: []apitypes.MessageDTO{pauseMarker(100, at, "lead_park")}})
	check("lead_park") // older history is intentionally absent
	// A missing current sequence blocks even a newer valid marker.
	update(streamEventsMsg{runID: "r1", events: []apitypes.RunEventDTO{{Type: uzicli.RunEventTypeMessage, Seq: 102, Kind: "paused", Payload: pauseMarker(102, at, "owner").Payload, CreatedAt: &at}}})
	check("")
	update(detailPageMsg{runID: "r1", kind: pageBackfill, msgs: []apitypes.MessageDTO{{Seq: 101, Kind: "text", CreatedAt: at}}})
	check("owner")
	// The newest invalid marker hides both valid older markers.
	update(detailPageMsg{runID: "r1", kind: pageBackfill, msgs: []apitypes.MessageDTO{pauseMarker(103, at, "hostile")}})
	check("")
	update(detailPageMsg{runID: "r1", kind: pageBackfill, msgs: []apitypes.MessageDTO{pauseMarker(104, at, "owner")}})
	check("owner")
	// Read failure/inflight/nonadvancing termination remain neutral.
	m.detail.catchupWaitID = 1
	check("")
	update(detailPageMsg{runID: "r1", kind: pageCatchup, reqID: 1, err: errors.New("offline")})
	check("")
	m.detail.catchupWaitID = 2
	update(detailPageMsg{runID: "r1", kind: pageCatchup, reqID: 2, msgs: []apitypes.MessageDTO{pauseMarker(104, at, "owner")}})
	check("")
	m.detail.catchupWaitID = 3
	update(detailPageMsg{runID: "r1", kind: pageCatchup, reqID: 3, pageEpoch: m.detail.pausePageEpoch})
	check("owner")
	// State-only events invalidate the timestamp until matching fresh metadata.
	update(streamEventsMsg{runID: "r1", events: []apitypes.RunEventDTO{{Type: uzicli.RunEventTypeState, Status: "paused"}}})
	check("")
	m.detail.metaWaitID = 1
	mismatch := run
	mismatch.Status = "running"
	update(detailMetaMsg{runID: "r1", reqID: 1, run: mismatch, pauseEpoch: m.detail.pauseEpoch})
	check("")
	m.detail.metaWaitID = 2
	update(detailMetaMsg{runID: "r1", reqID: 2, run: run, pauseEpoch: m.detail.pauseEpoch})
	check("owner")
	update(streamEventsMsg{runID: "r1", events: []apitypes.RunEventDTO{{Type: uzicli.RunEventTypeState, Status: "running"}, {Type: uzicli.RunEventTypeState, Status: "paused"}}})
	check("")
	// An earlier pause timestamp cannot identify a new pause, even with matching status.
	m.detail.metaWaitID = 3
	update(detailMetaMsg{runID: "r1", reqID: 3, run: run, pauseEpoch: m.detail.pauseEpoch})
	check("")
	later := at.Add(time.Minute)
	run.StatusSince = &later
	m.detail.metaWaitID = 4
	update(detailMetaMsg{runID: "r1", reqID: 4, run: run, pauseEpoch: m.detail.pauseEpoch - 1})
	check("")
	m.detail.metaWaitID = 5
	update(detailMetaMsg{runID: "r1", reqID: 5, run: run, pauseEpoch: m.detail.pauseEpoch})
	check("") // all held markers are stale
	update(detailPageMsg{runID: "r1", kind: pageBackfill, msgs: []apitypes.MessageDTO{pauseMarker(105, later, "lead_park")}})
	check("lead_park")
}

// TestTUIPauseOriginHiddenReplayFailure crosses the real stream pump and Update
// boundary. A state reconciliation must not hide a failed message replay.
func TestTUIPauseOriginHiddenReplayFailure(t *testing.T) {
	at := time.Now().UTC()
	run := apitypes.RunDTO{ID: "r1", Kind: "prompt", Status: "paused", StatusSince: &at}
	connections := make(chan *websocket.Conn, 4)
	var fail atomic.Bool
	var replayed atomic.Int32
	fail.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/ws":
			connection, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			connections <- connection
		case "/api/runs/r1/messages":
			if fail.Load() {
				http.Error(w, "offline", http.StatusServiceUnavailable)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"messages": []apitypes.MessageDTO{}})
			replayed.Add(1)
		case "/api/runs/r1":
			_ = json.NewEncoder(w).Encode(map[string]any{"run": run})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	client := uzicli.NewHTTPClient(uzicli.Settings{URL: server.URL, Token: "test"})
	stream, err := client.StreamRun(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	conn := <-connections
	m := tuiTestModel(t, &uzicli.FakeClient{}, "r1")
	m = applyDetail(m, run, []apitypes.MessageDTO{pauseMarker(100, at, "owner")})
	m.detail.stream = stream
	stream.NoteSeen(100)
	if got := m.detail.pauseOrigin(); got != "owner" {
		t.Fatal(got)
	}
	staleCatchup := m.catchupCmd("r1", 100, 7)
	staleTail := m.loadTailCmd("r1")
	_ = conn.CloseNow()
	select {
	case conn = <-connections:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case event := <-stream.Events():
		next, _ := m.Update(streamEventsMsg{runID: "r1", events: []apitypes.RunEventDTO{event}})
		m = next.(tuiModel)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	wait := func(predicate func() bool) {
		t.Helper()
		tick := time.NewTicker(time.Millisecond)
		defer tick.Stop()
		for !predicate() {
			select {
			case <-tick.C:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}
	}
	wait(func() bool { return !stream.Coverage().Busy && stream.Coverage().Failed })
	m.detail.applyMeta(run)
	if got := m.detail.pauseOrigin(); got != "" {
		t.Fatalf("hidden replay error attributed %q", got)
	}
	// Successful OLD history must not acknowledge the failed current replay.
	next, _ := m.Update(detailPageMsg{runID: "r1", kind: pageBackfill, msgs: []apitypes.MessageDTO{{Seq: 99, Kind: "text", CreatedAt: at}}})
	m = next.(tuiModel)
	if got := m.detail.pauseOrigin(); got != "" {
		t.Fatalf("old backfill acknowledged replay failure: %q", got)
	}
	// A current read dispatched before the failure cannot acknowledge it either.
	m.detail.catchupWaitID = 7
	next, _ = m.Update(staleCatchup())
	m = next.(tuiModel)
	if got := m.detail.pauseOrigin(); got != "" {
		t.Fatalf("stale current read acknowledged replay failure: %q", got)
	}
	next, _ = m.Update(staleTail())
	m = next.(tuiModel)
	if got := m.detail.pauseOrigin(); got != "" {
		t.Fatalf("stale tail acknowledged replay failure: %q", got)
	}
	// The existing reconnect replay at the SAME floor proves current coverage.
	fail.Store(false)
	_ = conn.CloseNow()
	select {
	case conn = <-connections:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	wait(func() bool { return replayed.Load() > 0 && !stream.Coverage().Busy })
	if got := m.detail.pauseOrigin(); got != "" {
		t.Fatalf("replay without current metadata attributed %q", got)
	}
	m.detail.applyMeta(run)
	if got := m.detail.pauseOrigin(); got != "owner" {
		t.Fatalf("same-floor replay plus current metadata did not restore readiness: %q", got)
	}
	replayed.Store(0)
	// Live seq 102 raises the transport cursor across missing 101.
	event := apitypes.RunEventDTO{Type: uzicli.RunEventTypeMessage, Seq: 102, Kind: "paused", Payload: pauseMarker(102, at, "lead_park").Payload, CreatedAt: &at}
	data, _ := json.Marshal(event)
	if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatal(err)
	}
	select {
	case event = <-stream.Events():
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	next, _ = m.Update(streamEventsMsg{runID: "r1", events: []apitypes.RunEventDTO{event}})
	m = next.(tuiModel)
	fail.Store(false)
	_ = conn.CloseNow()
	select {
	case conn = <-connections:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	defer func() { _ = conn.CloseNow() }()
	wait(func() bool { return replayed.Load() > 0 && !stream.Coverage().Busy })
	m.detail.applyMeta(run) // current generation metadata still cannot cover the hole
	if got := m.detail.pauseOrigin(); got != "" {
		t.Fatalf("replay above hole attributed %q", got)
	}
	// A current empty catchup above that hole also cannot prove coverage.
	m.detail.catchupWaitID = 1
	next, _ = m.Update(m.catchupCmd("r1", 102, 1)())
	m = next.(tuiModel)
	if got := m.detail.pauseOrigin(); got != "" {
		t.Fatalf("catchup above hole attributed %q", got)
	}
	// An existing backfill can supply the missing frame without an added read.
	next, _ = m.Update(detailPageMsg{runID: "r1", kind: pageBackfill, msgs: []apitypes.MessageDTO{{Seq: 101, Kind: "text", CreatedAt: at}}})
	m = next.(tuiModel)
	if got := m.detail.pauseOrigin(); got != "lead_park" {
		t.Fatalf("filled gap origin %q", got)
	}
}

func TestTUIPauseOriginShippedScenes(t *testing.T) {
	now := time.Now()
	for _, dark := range []bool{false, true} {
		for _, origin := range []string{"lead_park", "owner", ""} {
			for _, width := range []int{40, 60, 120} {
				for _, plain := range []bool{false, true} {
					out := detailPauseOrigin(dark, now, origin, width, plain)
					want := "‖ paused"
					if origin == "owner" {
						want += " by you"
					}
					if origin == "lead_park" {
						if width < 60 {
							want += " by lead"
						} else {
							want += " by the run's lead"
						}
					}
					if !strings.Contains(stripANSI(out), want) {
						t.Fatalf("scene dark=%v origin=%q width=%d plain=%v missing %q: %s", dark, origin, width, plain, want, out)
					}
					text := stripANSI(out)
					since := "since " + now.Add(-5*time.Minute).UTC().Format("15:04") + " (5m)"
					if !strings.Contains(text, since) {
						t.Fatalf("width=%d missing pause clock %q: %s", width, since, text)
					}
					if !strings.Contains(strings.Split(text, "\n")[0], " · 10m") {
						t.Fatalf("width=%d header lost elapsed time: %s", width, text)
					}
					if plain && strings.Contains(out, "\x1b[") {
						t.Fatalf("no-color scene retained SGR: %q", out)
					}
				}
			}
		}
	}
}

func TestTUIPauseOriginEmptyTailGap(t *testing.T) {
	at := time.Now()
	m := tuiTestModel(t, &uzicli.FakeClient{}, "r1")
	m = applyDetail(m, apitypes.RunDTO{ID: "r1", Status: "paused", StatusSince: &at}, nil)
	event := apitypes.RunEventDTO{Type: uzicli.RunEventTypeMessage, Seq: 2, Kind: "paused", Payload: pauseMarker(2, at, "owner").Payload, CreatedAt: &at}
	next, _ := m.Update(streamEventsMsg{runID: "r1", events: []apitypes.RunEventDTO{event}})
	m = next.(tuiModel)
	if got := m.detail.pauseOrigin(); got != "" {
		t.Fatalf("empty baseline lost gap: %q", got)
	}
	m.detail.applyCatchupPage([]apitypes.MessageDTO{{Seq: 1}})
	if got := m.detail.pauseOrigin(); got != "owner" {
		t.Fatalf("filled initial gap: %q", got)
	}
}

// A message observed by the stream is already part of current coverage, even
// before Update consumes it. This exercises a contiguous message, not a gap.
func TestTUIPauseOriginStreamAhead(t *testing.T) {
	at := time.Now().UTC()
	connections := make(chan *websocket.Conn, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err == nil {
			connections <- conn
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client := uzicli.NewHTTPClient(uzicli.Settings{URL: server.URL, Token: "test"})
	stream, err := client.StreamRun(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	conn := <-connections
	defer func() { _ = conn.CloseNow() }()
	m := tuiTestModel(t, &uzicli.FakeClient{}, "r1")
	m = applyDetail(m, apitypes.RunDTO{ID: "r1", Status: "paused", StatusSince: &at}, []apitypes.MessageDTO{pauseMarker(100, at, "owner")})
	m.detail.stream = stream
	stream.NoteSeen(100)
	ev := apitypes.RunEventDTO{Type: uzicli.RunEventTypeMessage, Seq: 101, Kind: "paused", Payload: pauseMarker(101, at, "lead_park").Payload, CreatedAt: &at}
	data, _ := json.Marshal(ev)
	if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatal(err)
	}
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for stream.Coverage().Through < 101 {
		select {
		case <-tick.C:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if got := m.detail.pauseOrigin(); got != "" {
		t.Fatalf("unloaded current frame attributed %q", got)
	}
	select {
	case ev = <-stream.Events():
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	next, _ := m.Update(streamEventsMsg{runID: "r1", events: []apitypes.RunEventDTO{ev}})
	m = next.(tuiModel)
	if got := m.detail.pauseOrigin(); got != "lead_park" {
		t.Fatalf("loaded suffix did not restore: %q", got)
	}
}

func TestTUIPauseOriginTailFailure(t *testing.T) {
	at := time.Now()
	run := apitypes.RunDTO{ID: "r1", Status: "paused", StatusSince: &at}
	m := tuiTestModel(t, &uzicli.FakeClient{}, "r1")
	m = applyDetail(m, run, []apitypes.MessageDTO{pauseMarker(100, at, "owner")})
	m.detail.applyTailPage(nil, errors.New("offline"))
	if got := m.detail.pauseOrigin(); got != "" {
		t.Fatal(got)
	}
	m.detail.applyTailPage([]apitypes.MessageDTO{pauseMarker(102, at, "lead_park")}, nil)
	if got := m.detail.pauseOrigin(); got != "" {
		t.Fatalf("tail above hole restored origin %q", got)
	}
	m.detail.applyCatchupPage([]apitypes.MessageDTO{{Seq: 101}})
	if got := m.detail.pauseOrigin(); got != "lead_park" {
		t.Fatal(got)
	}
}

func TestTUIPauseOriginEmptyTailAfterLive(t *testing.T) {
	at := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		seq  int32
		want string
	}{
		{"complete", 1, "lead_park"},
		{"gap2", 2, ""},
		{"gap100", 100, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := tuiTestModel(t, &uzicli.FakeClient{}, "r1")
			update := func(msg any) { next, _ := m.Update(msg); m = next.(tuiModel) }
			update(detailRunMsg{runID: "r1", run: apitypes.RunDTO{ID: "r1", Status: "paused", StatusSince: &at}})
			update(streamEventsMsg{runID: "r1", events: []apitypes.RunEventDTO{{Type: uzicli.RunEventTypeMessage, Seq: tc.seq, Kind: "paused", Payload: pauseMarker(tc.seq, at, "lead_park").Payload, CreatedAt: &at}}})
			// The first tail read was empty; its response arrives after the live marker.
			update(detailPageMsg{runID: "r1", kind: pageTail})
			if got := m.detail.pauseOrigin(); got != tc.want {
				t.Fatalf("origin %q want %q", got, tc.want)
			}
		})
	}
}

func TestTUIPauseOriginInitialReplayFailureThenCurrentTail(t *testing.T) {
	at := time.Now().UTC()
	run := apitypes.RunDTO{ID: "r1", Kind: "prompt", Status: "paused", StatusSince: &at}
	connections := make(chan *websocket.Conn, 4)
	var fail atomic.Bool
	var replayed atomic.Int32
	fail.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/ws":
			connection, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			connections <- connection
		case "/api/runs/r1/messages":
			if fail.Load() {
				http.Error(w, "offline", http.StatusServiceUnavailable)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"messages": []apitypes.MessageDTO{}})
			replayed.Add(1)
		case "/api/runs/r1":
			_ = json.NewEncoder(w).Encode(map[string]any{"run": run})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	client := uzicli.NewHTTPClient(uzicli.Settings{URL: server.URL, Token: "test"})
	stream, err := client.StreamRun(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	var conn *websocket.Conn
	select {
	case conn = <-connections:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	m := tuiTestModel(t, &uzicli.FakeClient{}, "r1")
	m.detail.applyRun(run, nil)
	m.detail.stream = stream

	_ = conn.CloseNow()
	select {
	case conn = <-connections:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case event := <-stream.Events():
		next, _ := m.Update(streamEventsMsg{runID: "r1", events: []apitypes.RunEventDTO{event}})
		m = next.(tuiModel)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	wait := func(predicate func() bool) {
		t.Helper()
		tick := time.NewTicker(time.Millisecond)
		defer tick.Stop()
		for !predicate() {
			select {
			case <-tick.C:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}
	}
	wait(func() bool { return !stream.Coverage().Busy && stream.Coverage().Failed })
	defer func() { _ = conn.CloseNow() }()
	m.detail.applyMeta(run)
	tail := make([]apitypes.MessageDTO, 0, detailPageSize)
	for seq := int32(801); seq <= 1000; seq++ {
		tail = append(tail, apitypes.MessageDTO{Seq: seq, Kind: "text", CreatedAt: at})
	}
	tail[len(tail)-1] = pauseMarker(1000, at, "owner")
	m.detail.applyTailPage(tail, nil, stream.Coverage().Revision)
	if got := m.detail.pauseOrigin(); got != "owner" {
		t.Fatalf("successful current tail with older history absent: origin=%q coverage=%+v", got, stream.Coverage())
	}
}
