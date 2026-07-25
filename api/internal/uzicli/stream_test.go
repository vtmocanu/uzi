package uzicli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"gitlab.example.com/vtmocanu/uzi/api/internal/apitypes"
)

// PRD #112 M2: StreamRun's decode boundary and recovery contract, against a real
// WebSocket server on loopback.
//
// The server is real rather than mocked because the properties under test are wire
// properties: which HEADER the credential rides on, what happens when the socket
// DIES mid-stream, and whether a frame the server never re-sends is recovered. A
// hand-rolled fake channel proves none of those.

const testRunID = "11111111-2222-3333-4444-555555555555"

// streamServer is a fake uzi API serving the three endpoints StreamRun touches:
// the socket itself, GetRun (the reconcile + post-reconnect state re-read) and
// RunLogs (the post-reconnect message replay).
type streamServer struct {
	*httptest.Server

	mu sync.Mutex
	// authHeaders records the Authorization header of EVERY /api/ws dial, so a test
	// can assert the credential rode a real header — server-side, which is the only
	// place that claim can be checked.
	authHeaders []string
	// subprotocols records Sec-WebSocket-Protocol per dial: the credential must NOT
	// be there (a browser can set it, unlike Authorization).
	subprotocols []string
	dials        int
	getRuns      int
	logCalls     []int32 // the `after` of each RunLogs call
	status       string
	msgs         []apitypes.MessageDTO

	// conns hands each accepted socket to the test so it can push frames or kill it.
	conns chan *websocket.Conn
}

func newStreamServer(t *testing.T) *streamServer {
	t.Helper()
	s := &streamServer{
		status: "running",
		conns:  make(chan *websocket.Conn, 8),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/ws", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.dials++
		s.authHeaders = append(s.authHeaders, r.Header.Get("Authorization"))
		s.subprotocols = append(s.subprotocols, r.Header.Get("Sec-WebSocket-Protocol"))
		s.mu.Unlock()
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{})
		if err != nil {
			return
		}
		select {
		case s.conns <- c:
		default:
			_ = c.CloseNow()
		}
	})
	mux.HandleFunc("/api/runs/"+testRunID+"/messages", func(w http.ResponseWriter, r *http.Request) {
		var after int32
		_, _ = fmt.Sscanf(r.URL.Query().Get("after"), "%d", &after)
		s.mu.Lock()
		s.logCalls = append(s.logCalls, after)
		out := make([]apitypes.MessageDTO, 0, len(s.msgs))
		for _, m := range s.msgs {
			if m.Seq > after {
				out = append(out, m)
			}
		}
		s.mu.Unlock()
		writeJSON(w, map[string]any{"messages": out})
	})
	mux.HandleFunc("/api/runs/"+testRunID, func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.getRuns++
		st := s.status
		s.mu.Unlock()
		writeJSON(w, map[string]any{"run": apitypes.RunDTO{ID: testRunID, Status: st}})
	})
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (s *streamServer) setStatus(status string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = status
}

func (s *streamServer) setMessages(msgs []apitypes.MessageDTO) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.msgs = msgs
}

func (s *streamServer) snapshot() (dials, getRuns int, auth, subproto []string, logs []int32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dials, s.getRuns, append([]string(nil), s.authHeaders...),
		append([]string(nil), s.subprotocols...), append([]int32(nil), s.logCalls...)
}

// nextConn waits for the server to accept a socket.
func (s *streamServer) nextConn(t *testing.T) *websocket.Conn {
	t.Helper()
	select {
	case c := <-s.conns:
		return c
	case <-time.After(5 * time.Second):
		t.Fatal("server never accepted a WebSocket connection")
		return nil
	}
}

// client builds an HTTPClient pointed at the fake server with test-speed timings.
func (s *streamServer) client(token string) *HTTPClient {
	c := NewHTTPClient(Settings{URL: s.URL, Token: token})
	c.streamReconcile = 60 * time.Millisecond
	c.streamBackoffBase = 5 * time.Millisecond
	c.streamBackoffMax = 20 * time.Millisecond
	return c
}

// pushFrame marshals ev and sends it down conn.
func pushFrame(t *testing.T, conn *websocket.Conn, ev apitypes.RunEventDTO) {
	t.Helper()
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal frame: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, b); err != nil {
		t.Fatalf("server write frame: %v", err)
	}
}

// nextEvent reads one event or fails with why it was waiting.
func nextEvent(t *testing.T, s *RunStream, why string) apitypes.RunEventDTO {
	t.Helper()
	select {
	case ev, ok := <-s.Events():
		if !ok {
			t.Fatalf("stream closed while waiting for %s (Err: %v)", why, s.Err())
		}
		return ev
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", why)
		return apitypes.RunEventDTO{}
	}
}

// -------------------------------------------------------------------------
// Decode: all four frame types, and the PRD #99 pointer fields in BOTH states.
// Absent AgentInstance is NOT the same as Agent == "lead" (hub.go says so), and
// M3's lane keying depends on telling them apart, so both are pinned.
// -------------------------------------------------------------------------

func TestStreamRunDecodesEveryFrameType(t *testing.T) {
	srv := newStreamServer(t)
	c := srv.client("uzc_test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream, err := c.StreamRun(ctx, testRunID)
	if err != nil {
		t.Fatalf("StreamRun: %v", err)
	}
	defer stream.Close()
	conn := srv.nextConn(t)

	agent, instance, label := "coder", "toolu_01", "write the tests"
	now := time.Unix(1700000000, 0).UTC()
	sent := []apitypes.RunEventDTO{
		{Type: "message", Seq: 1, Kind: "text", Agent: &agent, AgentInstance: &instance, AgentLabel: &label,
			Payload: json.RawMessage(`{"text":"a"}`), CreatedAt: &now},
		{Type: "message", Seq: 2, Kind: "thinking", Agent: &agent,
			Payload: json.RawMessage(`{"text":"b"}`), CreatedAt: &now},
		{Type: "state", Status: "awaiting_approval"},
		{Type: "health"},
		{Type: "input"},
	}
	for _, ev := range sent {
		pushFrame(t, conn, ev)
	}

	got := make([]apitypes.RunEventDTO, 0, len(sent))
	for range sent {
		got = append(got, nextEvent(t, stream, "a decoded frame"))
	}

	if got[0].Type != "message" || got[0].Seq != 1 || got[0].Kind != "text" {
		t.Errorf("frame 0 = %+v, want a message seq=1 kind=text", got[0])
	}
	if got[0].AgentInstance == nil || *got[0].AgentInstance != instance {
		t.Errorf("frame 0 agent_instance = %v, want %q — the PRD #99 invocation id must survive decode, it is M3's lane key",
			got[0].AgentInstance, instance)
	}
	if got[0].AgentLabel == nil || *got[0].AgentLabel != label {
		t.Errorf("frame 0 agent_label = %v, want %q", got[0].AgentLabel, label)
	}
	// The ABSENT case: a frame that carried no parent_tool_use_id must decode to nil,
	// NOT to an empty string, or a consumer cannot distinguish "no invocation id"
	// from "the empty invocation id" and every such frame collapses into one lane.
	if got[1].AgentInstance != nil {
		t.Errorf("frame 1 agent_instance = %q, want nil — an absent invocation id must stay absent, not become \"\"", *got[1].AgentInstance)
	}
	if got[1].AgentLabel != nil {
		t.Errorf("frame 1 agent_label = %q, want nil", *got[1].AgentLabel)
	}
	if got[2].Type != "state" || got[2].Status != "awaiting_approval" {
		t.Errorf("frame 2 = %+v, want a state frame with status awaiting_approval", got[2])
	}
	if got[3].Type != "health" {
		t.Errorf("frame 3 type = %q, want health", got[3].Type)
	}
	if got[4].Type != "input" {
		t.Errorf("frame 4 type = %q, want input", got[4].Type)
	}
}

// -------------------------------------------------------------------------
// The credential rides an Authorization HEADER — asserted SERVER-side, since a
// client-side check would only prove the code compiled.
//
// Sec-WebSocket-Protocol is asserted empty in the same breath, and that is the
// security half: the subprotocol is the one handshake header a browser CAN set, so
// a credential carried there would be reachable from a cross-site page and would
// undo M1's whole origin argument.
// -------------------------------------------------------------------------

func TestStreamRunSendsBearerOnTheAuthorizationHeader(t *testing.T) {
	const token = "uzc_secret-token"
	srv := newStreamServer(t)
	c := srv.client(token)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream, err := c.StreamRun(ctx, testRunID)
	if err != nil {
		t.Fatalf("StreamRun: %v", err)
	}
	defer stream.Close()
	srv.nextConn(t)

	_, _, auth, subproto, _ := srv.snapshot()
	if len(auth) != 1 {
		t.Fatalf("server saw %d dials, want 1", len(auth))
	}
	wantAuth := "Bearer " + token
	if auth[0] != wantAuth {
		t.Errorf("dial Authorization = %q, want %q", auth[0], wantAuth)
	}
	if subproto[0] != "" {
		t.Errorf("dial Sec-WebSocket-Protocol = %q, want empty — the credential must never ride the subprotocol, which is the one handshake header a cross-site browser page can set", subproto[0])
	}
}

// The same https guard the REST calls use covers the socket, because it carries the
// same token. Without this, `--url http://evil.example` would leak the credential
// over the socket while the REST path refused it.
func TestStreamRunRefusesPlaintextNonLoopbackURL(t *testing.T) {
	c := NewHTTPClient(Settings{URL: "http://evil.example", Token: "uzc_secret-token"})
	_, err := c.StreamRun(context.Background(), testRunID)
	if err == nil {
		t.Fatal("StreamRun dialled a plaintext non-loopback URL with a bearer token attached")
	}
	if !strings.Contains(err.Error(), "refusing to send credentials") {
		t.Errorf("error = %v, want the credential-leak guard's refusal (the same one newRequest raises)", err)
	}
}

// -------------------------------------------------------------------------
// Unknown enum values are inert AT DECODE.
//
// The Status half is the one that matters: Status decides whether a run reads as
// live, so an unrecognised one reaching a consumer intact is the "unknown state
// masquerading as active" hole. Kind is asserted to survive VERBATIM — it is an
// open set written by the separately-versioned worker, and normalising it would
// silently discard message types a newer agent emits.
// -------------------------------------------------------------------------

func TestStreamRunClassifiesUnknownEnumsInert(t *testing.T) {
	srv := newStreamServer(t)
	c := srv.client("uzc_test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream, err := c.StreamRun(ctx, testRunID)
	if err != nil {
		t.Fatalf("StreamRun: %v", err)
	}
	defer stream.Close()
	conn := srv.nextConn(t)

	pushFrame(t, conn, apitypes.RunEventDTO{Type: "teleport", Status: "running", Seq: 9})
	ev := nextEvent(t, stream, "the unknown-type frame")
	if ev.Type != RunEventTypeUnknown {
		t.Errorf("unknown frame type decoded as %q, want %q — it must not reach a consumer under a name they might branch on", ev.Type, RunEventTypeUnknown)
	}
	if ev.Status != "" {
		t.Errorf("unknown frame type kept status %q; a frame whose TYPE is unrecognised must not carry a readable status, or a consumer checking Status before Type reads it as live state", ev.Status)
	}

	pushFrame(t, conn, apitypes.RunEventDTO{Type: "state", Status: "ascended"})
	ev = nextEvent(t, stream, "the unknown-status state frame")
	if ev.Status != RunStatusUnknown {
		t.Errorf("unknown run status decoded as %q, want %q — an unrecognised status must never read as an active state", ev.Status, RunStatusUnknown)
	}

	// A kind this binary does not know is PRESERVED: the worker container versions
	// independently, so unknown kinds are routine, and rewriting them would lose
	// data the TUI could render.
	pushFrame(t, conn, apitypes.RunEventDTO{Type: "message", Seq: 1, Kind: "some_future_kind"})
	ev = nextEvent(t, stream, "the unknown-kind message frame")
	if ev.Kind != "some_future_kind" {
		t.Errorf("unknown message kind decoded as %q, want it preserved verbatim — Kind is an open set written by a separately-deployed worker and nothing branches on it for liveness", ev.Kind)
	}
}

// -------------------------------------------------------------------------
// THE POINT OF THE RECONCILE. A dropped state frame carries no seq, so it leaves no
// gap, so nothing triggers a replay — the run would read as live forever. Here the
// server NEVER sends the terminal state frame; only the periodic GetRun can find it.
// -------------------------------------------------------------------------

func TestStreamRunRecoversADroppedTerminalStateFrame(t *testing.T) {
	srv := newStreamServer(t)
	c := srv.client("uzc_test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream, err := c.StreamRun(ctx, testRunID)
	if err != nil {
		t.Fatalf("StreamRun: %v", err)
	}
	defer stream.Close()
	conn := srv.nextConn(t)

	// The socket is healthy and delivers a message, so nothing here is a reconnect:
	// the ONLY path to the terminal status below is the time-based re-read.
	pushFrame(t, conn, apitypes.RunEventDTO{Type: "message", Seq: 1, Kind: "text"})
	if ev := nextEvent(t, stream, "the live message frame"); ev.Seq != 1 {
		t.Fatalf("first event = %+v, want the seq=1 message", ev)
	}

	// The run finishes server-side and its state frame is "dropped" — never sent.
	srv.setStatus("completed")

	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-stream.Events():
			if !ok {
				t.Fatalf("stream closed before the dropped state frame was recovered (Err: %v)", stream.Err())
			}
			if ev.Type == RunEventTypeState && ev.Status == "completed" {
				return // recovered
			}
		case <-deadline:
			t.Fatal("a terminal state frame that was never sent on the socket was NEVER recovered: " +
				"state/health/input carry no seq, so no gap exists to trigger a replay and only the periodic GetRun reconcile can find it. " +
				"Without it a finished run renders as still running forever.")
		}
	}
}

// The reconcile emits only on CHANGE, so a steady-state run produces no synthetic
// traffic. Without this the consumer would get a state frame every tick and could
// not tell a real transition from a heartbeat.
func TestStreamRunReconcileIsQuietWhileStatusIsUnchanged(t *testing.T) {
	srv := newStreamServer(t)
	c := srv.client("uzc_test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream, err := c.StreamRun(ctx, testRunID)
	if err != nil {
		t.Fatalf("StreamRun: %v", err)
	}
	defer stream.Close()
	srv.nextConn(t)

	// First reconcile seeds the status (one frame). Everything after is silence.
	if ev := nextEvent(t, stream, "the seeding state frame"); ev.Status != "running" {
		t.Fatalf("seed frame = %+v, want status running", ev)
	}
	select {
	case ev, ok := <-stream.Events():
		if ok {
			t.Fatalf("reconcile emitted %+v while the status was unchanged; it must emit only on a real transition", ev)
		}
	case <-time.After(300 * time.Millisecond): // several reconcile intervals
	}
}

// -------------------------------------------------------------------------
// Reconnect → replay. The socket dies mid-run; while it is down the run produces
// messages the client never saw. On reconnect they must arrive via RunLogs, replayed
// from the last seq SEEN (not from zero, which would duplicate the whole history).
// -------------------------------------------------------------------------

func TestStreamRunReplaysMissedMessagesOnReconnect(t *testing.T) {
	srv := newStreamServer(t)
	c := srv.client("uzc_test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream, err := c.StreamRun(ctx, testRunID)
	if err != nil {
		t.Fatalf("StreamRun: %v", err)
	}
	defer stream.Close()
	conn := srv.nextConn(t)

	pushFrame(t, conn, apitypes.RunEventDTO{Type: "message", Seq: 1, Kind: "text"})
	if ev := nextEvent(t, stream, "the pre-drop message"); ev.Seq != 1 {
		t.Fatalf("first event = %+v, want seq=1", ev)
	}

	// While the socket is down the run emits seq 2 and 3.
	srv.setMessages([]apitypes.MessageDTO{
		{Seq: 1, Kind: "text"}, {Seq: 2, Kind: "tool_use"}, {Seq: 3, Kind: "text"},
	})
	_ = conn.CloseNow() // kill it

	var seqs []int32
	deadline := time.After(5 * time.Second)
	for len(seqs) < 2 {
		select {
		case ev, ok := <-stream.Events():
			if !ok {
				t.Fatalf("stream closed during reconnect (Err: %v)", stream.Err())
			}
			if ev.Type == RunEventTypeMessage {
				seqs = append(seqs, ev.Seq)
			}
		case <-deadline:
			t.Fatalf("after the socket died, the missed messages were never replayed (got seqs %v, want 2 then 3)", seqs)
		}
	}
	if seqs[0] != 2 || seqs[1] != 3 {
		t.Errorf("replayed seqs = %v, want [2 3]", seqs)
	}

	// The replay asked for messages AFTER the last seq seen. Replaying from 0 would
	// re-deliver the entire history on every blip.
	_, _, _, _, logs := srv.snapshot()
	if len(logs) == 0 {
		t.Fatal("no RunLogs call was made on reconnect; the replay half of the recovery contract is missing")
	}
	if logs[0] != 1 {
		t.Errorf("reconnect replayed from after=%d, want after=1 (the last seq seen) — replaying from 0 duplicates the whole run", logs[0])
	}
}

// A reconnect also re-reads GetRun, because the message replay cannot reveal a state
// change: state frames carry no seq and are not in run_messages at all.
func TestStreamRunReReadsStateOnReconnect(t *testing.T) {
	srv := newStreamServer(t)
	c := srv.client("uzc_test")
	// Long reconcile so the state below can ONLY come from the reconnect path.
	c.streamReconcile = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream, err := c.StreamRun(ctx, testRunID)
	if err != nil {
		t.Fatalf("StreamRun: %v", err)
	}
	defer stream.Close()
	conn := srv.nextConn(t)

	srv.setStatus("completed")
	_ = conn.CloseNow()

	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-stream.Events():
			if !ok {
				t.Fatalf("stream closed during reconnect (Err: %v)", stream.Err())
			}
			if ev.Type == RunEventTypeState && ev.Status == "completed" {
				return
			}
		case <-deadline:
			t.Fatal("reconnect did not re-read run state: with the reconcile disabled, GetRun on reconnect is the only way to learn the run finished while the socket was down")
		}
	}
}

// -------------------------------------------------------------------------
// Shutdown: cancelling the context closes Events and leaves no goroutine behind.
// -------------------------------------------------------------------------

func TestStreamRunShutsDownCleanlyOnContextCancel(t *testing.T) {
	srv := newStreamServer(t)
	c := srv.client("uzc_test")
	ctx, cancel := context.WithCancel(context.Background())

	stream, err := c.StreamRun(ctx, testRunID)
	if err != nil {
		t.Fatalf("StreamRun: %v", err)
	}
	conn := srv.nextConn(t)
	pushFrame(t, conn, apitypes.RunEventDTO{Type: "message", Seq: 1, Kind: "text"})
	nextEvent(t, stream, "one frame before cancelling")

	cancel()

	select {
	case _, ok := <-stream.Events():
		if ok {
			// Drain whatever was in flight, then require the close.
			select {
			case _, ok2 := <-stream.Events():
				if ok2 {
					t.Fatal("Events still delivering after context cancel")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Events never closed after context cancel")
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Events never closed after context cancel")
	}
	if err := stream.Err(); err == nil {
		t.Error("Err() is nil after a cancelled context; a consumer cannot tell a cancelled stream from one that ended on its own")
	}

	// The pump and its per-connection frame reader must BOTH have exited. This waits
	// on the stream's own WaitGroup rather than sampling runtime.NumGoroutine(),
	// because a process-wide count carries httptest's traffic and would need a slack
	// threshold — one that passes while leaking a goroutine per stream, which is the
	// leak a long TUI session actually accumulates.
	if !stream.waitStopped(5 * time.Second) {
		t.Fatal("the pump or its frame reader was still running after the context was cancelled")
	}
}

// Close() is the same shutdown without touching the caller's context, which is how a
// TUI closes one run's stream while staying alive.
func TestStreamRunCloseStopsTheStream(t *testing.T) {
	srv := newStreamServer(t)
	c := srv.client("uzc_test")

	stream, err := c.StreamRun(context.Background(), testRunID)
	if err != nil {
		t.Fatalf("StreamRun: %v", err)
	}
	srv.nextConn(t)
	stream.Close()

	deadline := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-stream.Events():
			if !ok {
				if !stream.waitStopped(5 * time.Second) {
					t.Fatal("Events closed but the pump or its frame reader is still running after Close()")
				}
				return
			}
		case <-deadline:
			t.Fatal("Events never closed after Close()")
		}
	}
}

// -------------------------------------------------------------------------
// Backoff is exponential and capped, not a fixed retry.
// -------------------------------------------------------------------------

func TestStreamBackoffGrowsAndIsCapped(t *testing.T) {
	c := &HTTPClient{streamBackoffBase: 100 * time.Millisecond, streamBackoffMax: time.Second}

	// Jitter is over [d/2, d], so compare CEILINGS: attempt n can never exceed
	// base<<n, and must exceed half of it.
	for attempt, ceiling := range []time.Duration{
		100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond, 800 * time.Millisecond,
	} {
		for i := 0; i < 50; i++ {
			d := c.backoff(attempt)
			if d > ceiling || d < ceiling/2 {
				t.Fatalf("backoff(%d) = %v, want within [%v, %v]", attempt, d, ceiling/2, ceiling)
			}
		}
	}
	// Far past the cap, including where the shift would overflow.
	for _, attempt := range []int{4, 10, 62, 63, 64, 100} {
		for i := 0; i < 50; i++ {
			if d := c.backoff(attempt); d > time.Second || d <= 0 {
				t.Fatalf("backoff(%d) = %v, want capped within (0, 1s] — a shift overflow must not produce a zero or negative wait", attempt, d)
			}
		}
	}
}

// -------------------------------------------------------------------------
// NormalizeRunEvent as a pure function, so the classification table is pinned
// independently of any socket.
// -------------------------------------------------------------------------

func TestNormalizeRunEventTable(t *testing.T) {
	for _, tc := range []struct {
		name               string
		in                 apitypes.RunEventDTO
		wantType, wantStat string
	}{
		{"known message passes through", apitypes.RunEventDTO{Type: "message", Seq: 1}, "message", ""},
		{"known state keeps a known status", apitypes.RunEventDTO{Type: "state", Status: "running"}, "state", "running"},
		{"terminal status is known", apitypes.RunEventDTO{Type: "state", Status: "cancelled"}, "state", "cancelled"},
		{"unknown status is made inert", apitypes.RunEventDTO{Type: "state", Status: "ascended"}, "state", RunStatusUnknown},
		{"empty status on a state frame is inert too", apitypes.RunEventDTO{Type: "state"}, "state", RunStatusUnknown},
		{"unknown type is made inert and loses its status", apitypes.RunEventDTO{Type: "teleport", Status: "running"}, RunEventTypeUnknown, ""},
		{"empty type is unknown", apitypes.RunEventDTO{}, RunEventTypeUnknown, ""},
		{"health passes through", apitypes.RunEventDTO{Type: "health"}, "health", ""},
		{"input passes through", apitypes.RunEventDTO{Type: "input"}, "input", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := NormalizeRunEvent(tc.in)
			if got.Type != tc.wantType {
				t.Errorf("Type = %q, want %q", got.Type, tc.wantType)
			}
			if got.Status != tc.wantStat {
				t.Errorf("Status = %q, want %q", got.Status, tc.wantStat)
			}
		})
	}

	// Kind is never touched, for any type.
	in := apitypes.RunEventDTO{Type: "message", Kind: "a_kind_from_a_newer_worker"}
	if got := NormalizeRunEvent(in); got.Kind != in.Kind {
		t.Errorf("Kind = %q, want it preserved as %q", got.Kind, in.Kind)
	}
}

func TestIsTerminalRunStatus(t *testing.T) {
	for _, s := range []string{"completed", "failed", "cancelled"} {
		if !IsTerminalRunStatus(s) {
			t.Errorf("IsTerminalRunStatus(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"queued", "claimed", "running", "awaiting_approval", RunStatusUnknown, ""} {
		if IsTerminalRunStatus(s) {
			t.Errorf("IsTerminalRunStatus(%q) = true, want false — treating a live run as terminal stops the reconcile that would have corrected it", s)
		}
	}
}

// FakeClient satisfies the same contract, including the normalization, so a consumer
// test cannot pass against a frame shape the live client would have made inert.
func TestFakeClientStreamRunNormalizes(t *testing.T) {
	f := &FakeClient{StreamEvents: []apitypes.RunEventDTO{
		{Type: "state", Status: "ascended"},
		{Type: "message", Seq: 4, Kind: "text"},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream, err := f.StreamRun(ctx, testRunID)
	if err != nil {
		t.Fatalf("FakeClient.StreamRun: %v", err)
	}
	defer stream.Close()

	if ev := nextEvent(t, stream, "the fake's state frame"); ev.Status != RunStatusUnknown {
		t.Errorf("fake delivered status %q un-normalized; a fake that skips the decode boundary lets a consumer test pass on a frame the real client would have made inert", ev.Status)
	}
	if ev := nextEvent(t, stream, "the fake's message frame"); ev.Seq != 4 {
		t.Errorf("fake message seq = %d, want 4", ev.Seq)
	}
	if f.LastStreamRunID != testRunID {
		t.Errorf("LastStreamRunID = %q, want %q", f.LastStreamRunID, testRunID)
	}
}

// A fake whose socket cannot open models the D8 degradation: the REST reads still
// work, only the stream fails.
func TestFakeClientStreamErrIsIndependentOfErr(t *testing.T) {
	f := &FakeClient{
		StreamErr: Exitf(ExitUnreachable, "no socket"),
		RunByID:   map[string]apitypes.RunDTO{testRunID: {ID: testRunID, Status: "running"}},
	}
	if _, err := f.StreamRun(context.Background(), testRunID); err == nil {
		t.Fatal("StreamErr did not make StreamRun fail")
	}
	if _, err := f.GetRun(context.Background(), testRunID); err != nil {
		t.Errorf("GetRun failed too (%v); StreamErr must fail ONLY the stream, or a test cannot model the fall-back-to-REST degradation", err)
	}
}

// Close must work on an UNDRAINED stream, which is the case the two shutdown tests
// above cannot see: they both keep reading until Events closes, so the pump is never
// blocked trying to hand over a frame.
//
// This is the shape a TUI actually produces — the user closes a run view and stops
// reading while frames are still arriving — and it is where the pump leaks if its
// send is not cancellable. Found by mutation: dropping the ctx case from emit() left
// both other tests green.
func TestStreamRunCloseWhileConsumerIsNotReading(t *testing.T) {
	srv := newStreamServer(t)
	c := srv.client("uzc_test")

	stream, err := c.StreamRun(context.Background(), testRunID)
	if err != nil {
		t.Fatalf("StreamRun: %v", err)
	}
	conn := srv.nextConn(t)

	// Events is unbuffered, so this frame parks the pump in emit with nobody reading.
	pushFrame(t, conn, apitypes.RunEventDTO{Type: "message", Seq: 1, Kind: "text"})

	// Give the pump time to actually reach the blocked send before closing, so the
	// test exercises the blocked path rather than racing past it.
	time.Sleep(100 * time.Millisecond)
	stream.Close()

	if !stream.waitStopped(5 * time.Second) {
		t.Fatal("Close() on an undrained stream left the pump running: it is parked handing a frame to a consumer that will never read it. " +
			"Every send must be cancellable, or a TUI leaks one goroutine and one socket per run view the user closes.")
	}
}
