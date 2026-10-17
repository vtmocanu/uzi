package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func TestTUIPauseOriginReconnect(t *testing.T) {
	for _, mode := range []string{"run503", "sameStatus", "changedSince", "ackFailureThenGap"} {
		t.Run(mode, func(t *testing.T) {
			at := time.Now().UTC()
			later := at.Add(time.Minute)
			run := apitypes.RunDTO{ID: "r1", Kind: "prompt", Status: "paused", StatusSince: &at}
			connections := make(chan *websocket.Conn, 4)
			var reads atomic.Int32
			var failMessages atomic.Bool
			var failRun atomic.Bool
			var changed atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/ws":
					c, err := websocket.Accept(w, r, nil)
					if err == nil {
						connections <- c
					}
				case "/api/runs/r1/messages":
					if failMessages.Load() {
						http.Error(w, "offline", http.StatusServiceUnavailable)
						return
					}
					messages := []apitypes.MessageDTO{}
					if r.URL.Query().Get("tail") != "" {
						messages = []apitypes.MessageDTO{pauseMarker(100, at, "owner")}
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"messages": messages})
				case "/api/runs/r1":
					defer reads.Add(1)
					if failRun.Load() {
						http.Error(w, "offline", http.StatusServiceUnavailable)
						return
					}
					dto := run
					if changed.Load() {
						dto.StatusSince = &later
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"run": dto})
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
			receiveConn := func() *websocket.Conn {
				t.Helper()
				select {
				case c := <-connections:
					return c
				case <-ctx.Done():
					t.Fatal(ctx.Err())
					return nil
				}
			}
			conn := receiveConn()
			defer func() { _ = conn.CloseNow() }()
			m := tuiTestModel(t, &uzicli.FakeClient{}, "r1")
			m = applyDetail(m, run, []apitypes.MessageDTO{pauseMarker(100, at, "owner")})
			m.detail.stream = stream
			stream.NoteSeen(100)
			update := func(msg any) { t.Helper(); n, _ := m.Update(msg); m = n.(tuiModel) }
			send := func(ev apitypes.RunEventDTO) {
				t.Helper()
				b, _ := json.Marshal(ev)
				if err := conn.Write(ctx, websocket.MessageText, b); err != nil {
					t.Fatal(err)
				}
				select {
				case ev = <-stream.Events():
					update(streamEventsMsg{runID: "r1", events: []apitypes.RunEventDTO{ev}})
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			wait := func(p func() bool) {
				t.Helper()
				ticker := time.NewTicker(time.Millisecond)
				defer ticker.Stop()
				for !p() {
					select {
					case <-ticker.C:
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					}
				}
			}
			// A real live state frame sets lastStatus in the pump: reconnect must not
			// synthesize an initial paused event and accidentally invalidate metadata.
			send(apitypes.RunEventDTO{Type: uzicli.RunEventTypeState, Status: "paused"})
			m.detail.applyMeta(run)
			if got := m.detail.pauseOrigin(); got != "owner" {
				t.Fatalf("seed origin=%q", got)
			}
			oldEpoch := m.detail.pauseEpoch
			m.client = client
			staleMeta := m.refreshRunMetaCmd("r1", 1, m.detail.gen)()
			staleRun := m.loadRunCmd("r1")()
			reads.Store(0)
			if mode == "run503" {
				failRun.Store(true)
			}
			if mode == "changedSince" {
				changed.Store(true)
			}
			if mode == "ackFailureThenGap" {
				failMessages.Store(true)
			}
			_ = conn.CloseNow()
			conn = receiveConn()
			wait(func() bool { return reads.Load() > 0 && !stream.Coverage().Busy })
			// Drain any unexpected synthetic event so the probe cannot ignore one.
			select {
			case ev := <-stream.Events():
				update(streamEventsMsg{runID: "r1", events: []apitypes.RunEventDTO{ev}})
				t.Fatalf("unexpected same-status reconcile event: %+v", ev)
			default:
			}
			t.Logf("after reconnect coverage=%+v epoch=%d (before=%d)", stream.Coverage(), m.detail.pauseEpoch, oldEpoch)
			if mode != "ackFailureThenGap" {
				if got := m.detail.pauseOrigin(); got != "" {
					t.Errorf("reconnect without current metadata attributed %q", got)
				}
				update(staleRun)
				if got := m.detail.pauseOrigin(); got != "" {
					t.Errorf("queued pre-reconnect run attributed %q", got)
				}
				m.detail.metaWaitID = 1
				update(staleMeta)
				if got := m.detail.pauseOrigin(); got != "" {
					t.Errorf("queued pre-reconnect metadata attributed %q", got)
				}
				failRun.Store(false)
				m.client = client
				m.detail.metaWaitID = 2
				update(m.refreshRunMetaCmd("r1", 2, m.detail.gen)())
				want := "owner"
				if mode == "changedSince" {
					want = ""
				}
				if got := m.detail.pauseOrigin(); got != want {
					t.Errorf("fresh metadata origin=%q want=%q", got, want)
				}
				if mode == "changedSince" {
					update(detailPageMsg{runID: "r1", kind: pageBackfill, msgs: []apitypes.MessageDTO{pauseMarker(101, later, "owner")}})
					if got := m.detail.pauseOrigin(); got != "owner" {
						t.Errorf("current pause marker origin=%q", got)
					}
				}
				t.Logf("fresh metadata origin=%q", m.detail.pauseOrigin())
				return
			}
			if got := m.detail.pauseOrigin(); got != "" {
				t.Fatalf("failed replay origin=%q", got)
			}
			m.detail.metaWaitID = 2
			update(m.refreshRunMetaCmd("r1", 2, m.detail.gen)())
			// Successful current tail at floor 100 acknowledges revision 1. No
			// further reconnect is allowed to clear the pump's sticky Failed flag.
			failMessages.Store(false)
			update(m.loadTailCmd("r1")())
			if got := m.detail.pauseOrigin(); got != "owner" {
				t.Fatalf("successful tail did not acknowledge failure: %q", got)
			}
			send(apitypes.RunEventDTO{Type: uzicli.RunEventTypeMessage, Seq: 102, Kind: "paused", Payload: pauseMarker(102, at, "lead_park").Payload, CreatedAt: &at})
			if got := m.detail.pauseOrigin(); got != "" {
				t.Fatalf("actual missing 101 origin=%q", got)
			}
			update(detailPageMsg{runID: "r1", kind: pageBackfill, msgs: []apitypes.MessageDTO{{Seq: 101, Kind: "text", CreatedAt: at}}})
			t.Logf("filled real gap coverage=%+v readRevision=%d metaReady=%t", stream.Coverage(), m.detail.pauseReadRevision, m.detail.pauseMetaReady)
			if got := m.detail.pauseOrigin(); got != "lead_park" {
				t.Errorf("loaded gap recovery origin=%q want=lead_park", got)
			}
		})
	}
}

func TestTUIPauseOriginReconnectFallbackQueuedDTO(t *testing.T) {
	for _, closed := range []bool{false, true} {
		t.Run(map[bool]string{false: "openFailure", true: "closed"}[closed], func(t *testing.T) {
			at := time.Now().UTC()
			run := apitypes.RunDTO{ID: "r1", Kind: "prompt", Status: "paused", StatusSince: &at}
			m := tuiTestModel(t, &uzicli.FakeClient{}, "r1")
			m = applyDetail(m, run, []apitypes.MessageDTO{pauseMarker(100, at, "owner")})
			update := func(msg any) { n, _ := m.Update(msg); m = n.(tuiModel) }
			epoch := m.detail.pauseEpoch
			staleRun := detailRunMsg{runID: "r1", run: run, gen: m.detail.gen, pauseEpoch: epoch}
			staleMeta := detailMetaMsg{runID: "r1", run: run, gen: m.detail.gen, reqID: 1, pauseEpoch: epoch}
			if closed {
				update(streamEventsMsg{runID: "r1", gen: m.detail.gen, closed: true})
			} else {
				update(streamReadyMsg{runID: "r1", gen: m.detail.gen, err: errors.New("offline")})
			}
			// Complete current transcript proof first: metadata alone must remain blocked.
			m.detail.catchupWaitID = 1
			update(detailPageMsg{runID: "r1", gen: m.detail.gen, kind: pageCatchup, reqID: 1, pageEpoch: m.detail.pausePageEpoch})
			update(staleRun)
			m.detail.metaWaitID = 1
			update(staleMeta)
			if got := m.detail.pauseOrigin(); got != "" {
				t.Fatalf("queued fallback DTO attributed %q", got)
			}
			m.detail.metaWaitID = 2
			update(detailMetaMsg{runID: "r1", run: run, gen: m.detail.gen, reqID: 2, pauseEpoch: m.detail.pauseEpoch})
			if got := m.detail.pauseOrigin(); got != "owner" {
				t.Fatalf("current fallback metadata origin=%q", got)
			}
		})
	}
}
