package uzicli

import (
	"context"
	"encoding/json"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"gitlab.example.com/vtmocanu/uzi/api/internal/apitypes"
)

// The CLOSED frame-type set the hub emits (apitypes.RunEventDTO.Type), plus the
// sentinel an unrecognised type is normalized to.
const (
	RunEventTypeMessage = "message"
	RunEventTypeState   = "state"
	RunEventTypeHealth  = "health"
	RunEventTypeInput   = "input"

	// RunEventTypeUnknown replaces a Type this binary does not recognise. It is a
	// value no consumer branches on, so an unknown frame renders neutrally and can
	// never be mistaken for a state/health/input signal.
	RunEventTypeUnknown = "unknown"

	// RunStatusUnknown replaces a state frame's unrecognised Status. This is the
	// sentinel that matters most: Status is what decides whether a run reads as
	// still live, so an unrecognised one must never reach a consumer as-is.
	RunStatusUnknown = "unknown"
)

// knownRunEventTypes is closed because the hub that emits these frames ships in the
// same binary as the API (internal/hub).
var knownRunEventTypes = map[string]struct{}{
	RunEventTypeMessage: {}, RunEventTypeState: {},
	RunEventTypeHealth: {}, RunEventTypeInput: {},
}

// knownRunStatuses is closed because the database enforces it: runs.status carries a
// CHECK constraint over exactly these eight values (runs_status_check, declared
// inline in 00020_workers_runs.sql and widened with 'limit_wait' by PRD #35's
// migration). A value outside the set cannot be stored, so one arriving on the wire
// means the server is newer than this binary — which is precisely when it must not be
// trusted to mean "active".
//
// THIS MAP MUST BE WIDENED IN THE SAME COMMIT AS THE CHECK. It is not a display
// concern: NormalizeRunEvent rewrites any status outside it to RunStatusUnknown at the
// DECODE boundary, so a status the DB accepts and this map omits reaches every CLI and
// TUI consumer as "unknown" — silently, and with the opposite meaning to the one
// intended, since the comment below turns an unrecognised status into "do not trust
// this to be active".
//
// Two tests enforce this, and they cover different halves:
//
//   - TestKnownRunStatusesMatchTheDocumentedCount pins the map against the COUNT in the
//     first sentence, so editing one without the other fails.
//   - TestKnownRunStatusesMatchTheMigrationCheck pins it against runs_status_check
//     ITSELF, parsed out of the migration that last declares it. That is the one that
//     matters: it is what makes "widen the CHECK, forget this map" a red test instead of
//     a silent downgrade of every run in the new status.
//
// An earlier version of this comment claimed the second test was impossible here,
// because uzicli is a leaf package that cannot import the store. That was wrong twice
// over: the package already imports apitypes and coder/websocket, and reading a file
// needs no import at all — internal/uzicli sits at the same depth as internal/workersvc,
// whose auto_select_test.go established the `../store/migrations/` path literal.
//
// What is still NOT covered is one narrow case, measured rather than assumed: a future
// migration that widens the domain WITHOUT ever naming runs_status_check passes
// silently, because the scan selects its file by that name. A plain rename does not
// slip through — it fails loudly instead — and a DROP-then-ADD-under-a-new-name is
// caught, because the DROP still names it. See the test for the full table.
var knownRunStatuses = map[string]struct{}{
	"queued": {}, "claimed": {}, "running": {}, "awaiting_approval": {},
	// limit_wait (PRD #35): parked until the owner's Anthropic usage window reopens.
	// NON-terminal — it is deliberately absent from terminalRunStatuses below.
	"limit_wait": {},
	"completed":  {}, "failed": {}, "cancelled": {},
}

// terminalRunStatuses are the three a run never leaves. Verified against the requeue
// path, which is scoped WHERE status IN ('claimed','running','awaiting_approval')
// (store/queries/runtime.sql RequeueRunsOfStaleWorkers / RequeueWorkerRuns) — so a
// terminal run is never returned to 'queued' and the reconcile below can stop.
var terminalRunStatuses = map[string]struct{}{
	"completed": {}, "failed": {}, "cancelled": {},
}

// NormalizeRunEvent classifies a decoded frame's CLOSED enums, replacing an
// unrecognised value with an inert sentinel. It runs at the DECODE boundary rather
// than at render, because a value that reaches a consumer intact can be branched on
// by any render path added later — including ones written after this rule is
// forgotten.
//
// It normalizes Type and Status and deliberately leaves Kind and Agent ALONE. Those
// two are open sets written by the worker container, which versions independently of
// this binary: a `uzi` built today routinely sees kinds a newer agent emits, and
// rewriting them to a sentinel would silently discard information the TUI could
// otherwise render. Nothing branches on Kind for liveness — its safety requirement is
// terminal-control-byte stripping at render (PRD #112 D7), a different mechanism.
func NormalizeRunEvent(ev apitypes.RunEventDTO) apitypes.RunEventDTO {
	if _, ok := knownRunEventTypes[ev.Type]; !ok {
		ev.Type = RunEventTypeUnknown
		// An unknown-typed frame must not carry a readable Status either: Status is
		// only meaningful on a "state" frame, and leaving it set would let an
		// unrecognised frame drive the run chip through a consumer that checks
		// Status before Type.
		ev.Status = ""
		return ev
	}
	if ev.Type == RunEventTypeState {
		if _, ok := knownRunStatuses[ev.Status]; !ok {
			ev.Status = RunStatusUnknown
		}
	}
	return ev
}

// IsTerminalRunStatus reports whether a run status is one a run never leaves.
func IsTerminalRunStatus(status string) bool {
	_, ok := terminalRunStatuses[status]
	return ok
}

// Stream tuning. All three are overridable per-client for tests (same package).
const (
	// defaultStreamReconcile is the periodic GetRun re-read that closes the hub's
	// seq-less drop hole. state/health/input frames carry no seq, so a dropped one
	// leaves no gap for the client to detect and nothing to trigger a replay: a
	// dropped TERMINAL state frame would leave a finished run rendering as live
	// forever. Only a TIME-based re-read can recover it, because no event is coming.
	defaultStreamReconcile = 12 * time.Second
	// Reconnect backoff. Exponential with jitter so a restarting api does not take a
	// synchronised retry storm from every open TUI.
	defaultStreamBackoffBase = 500 * time.Millisecond
	defaultStreamBackoffMax  = 30 * time.Second
)

// RunStream is one run's live event feed, with the recovery contract folded in so
// every consumer inherits it rather than reimplementing it.
//
// What it guarantees beyond a raw socket:
//   - reconnects with exponential backoff, and on EVERY reconnect (not merely on a
//     detected seq gap) replays the missed messages via RunLogs and re-reads the
//     authoritative run state via GetRun
//   - periodically re-reads run state, which is the only way to recover a dropped
//     seq-less frame (see internal/hub.broadcast)
//   - normalizes the closed enums at decode, so an unrecognised frame is inert
//
// Events closes exactly once, when the stream is finished; Err reports why.
type RunStream struct {
	events chan apitypes.RunEventDTO
	cancel context.CancelFunc

	mu   sync.Mutex
	err  error
	done bool

	// wg tracks the pump and its per-connection frame reader. It exists so a test
	// can prove BOTH have exited rather than sampling runtime.NumGoroutine(), which
	// is a process-wide number with unrelated traffic in it: a slack threshold there
	// would pass while leaking one goroutine per stream, which over a TUI session is
	// exactly the leak worth catching.
	wg sync.WaitGroup

	// lastSeq is the highest message seq emitted, and is what a reconnect replays
	// from. Only message frames carry one.
	lastSeq int32
	// lastStatus is the last run status emitted, so the reconcile emits only on a
	// real change rather than every tick.
	lastStatus string
}

// Events is the receive side. It is closed when the stream ends (context cancelled,
// Close called, or the stream gave up); check Err afterwards.
func (s *RunStream) Events() <-chan apitypes.RunEventDTO { return s.events }

// Err returns the terminal error, or nil for a clean shutdown. Meaningful only once
// Events is closed.
func (s *RunStream) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Close stops the stream and releases its goroutine and socket. Safe to call more
// than once, and safe to call without draining Events.
func (s *RunStream) Close() { s.cancel() }

func (s *RunStream) setErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.done {
		s.err = err
		s.done = true
	}
}

// emit delivers one event, or reports false if the stream is shutting down. Every
// send goes through here so no goroutine can block forever on an undrained consumer.
func (s *RunStream) emit(ctx context.Context, ev apitypes.RunEventDTO) bool {
	select {
	case s.events <- ev:
		return true
	case <-ctx.Done():
		return false
	}
}

func (c *HTTPClient) reconcileInterval() time.Duration {
	if c.streamReconcile > 0 {
		return c.streamReconcile
	}
	return defaultStreamReconcile
}

func (c *HTTPClient) backoff(attempt int) time.Duration {
	base := c.streamBackoffBase
	if base <= 0 {
		base = defaultStreamBackoffBase
	}
	ceiling := c.streamBackoffMax
	if ceiling <= 0 {
		ceiling = defaultStreamBackoffMax
	}
	d := base << attempt
	if d > ceiling || d <= 0 { // <=0 guards the shift overflowing on a long outage
		d = ceiling
	}
	// Full jitter over [d/2, d): spreads a fleet's reconnects without ever waiting
	// longer than the computed ceiling.
	return d/2 + time.Duration(rand.Int63n(int64(d/2)+1))
}

// wsURL maps the validated API base onto the /api/ws endpoint for one run. It goes
// through credentialSafeBase, the SAME gate newRequest uses, because this socket
// carries the same Authorization: Bearer credential — an https-only rule enforced in
// one place and not two.
func (c *HTTPClient) wsURL(runID string) (string, error) {
	u, err := c.credentialSafeBase()
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	}
	base := strings.TrimRight(u.String(), "/")
	return base + "/api/ws?run=" + url.QueryEscape(runID), nil
}

// dialWS opens the run's socket with the CLI credential on an Authorization header.
//
// The credential rides a real header, never Sec-WebSocket-Protocol: the subprotocol
// is the one handshake header a BROWSER can set, so a subprotocol-borne token would
// be reachable from a cross-site page and reintroduce exactly the hijacking that
// PRD #112 M1's origin argument rules out. It is not in the query string either,
// which lands in access logs and proxy history.
//
// No Origin header is sent, deliberately: coder/websocket's Accept passes an empty
// Origin (accept.go:228-232), which is what lets a browser-less client through the
// unchanged same-origin check.
func (c *HTTPClient) dialWS(ctx context.Context, runID string) (*websocket.Conn, error) {
	endpoint, err := c.wsURL(runID)
	if err != nil {
		return nil, err
	}
	hdr := http.Header{}
	if c.Token != "" {
		hdr.Set("Authorization", "Bearer "+c.Token)
	}
	// Passing c.HTTP is deliberate and safe on both counts: coder/websocket applies
	// Client.Timeout to the HANDSHAKE only (it clones the client with Timeout=0 for
	// the live socket, dial.go:78-83), and it CALLS our CheckRedirect (dial.go:88-101),
	// so refuseRedirect still stops the bearer token being replayed across a hop.
	conn, resp, err := websocket.Dial(ctx, endpoint, &websocket.DialOptions{
		HTTPClient: c.HTTP,
		HTTPHeader: hdr,
	})
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		switch status {
		case http.StatusUnauthorized, http.StatusForbidden:
			return nil, Exitf(ExitAuth, "not authorized to follow run %s over /api/ws (status %d): %v", runID, status, err)
		case http.StatusNotFound:
			return nil, Exitf(ExitNotFound, "run %s not found", runID)
		default:
			return nil, Exitf(ExitUnreachable, "cannot open the run stream at %s: %v", c.BaseURL, err)
		}
	}
	return conn, nil
}

// StreamRun subscribes to one run's live events over /api/ws.
//
// The initial dial is synchronous so a caller learns immediately that the socket is
// unusable and can fall back to REST polling (PRD #112 D8) rather than waiting on a
// channel that will never produce. Everything after that is handled internally:
// reconnects, replay, and the periodic state reconcile.
//
// It does NOT replay history on the FIRST connect — the caller loads that itself
// (GetRun + RunLogs) and would otherwise receive it twice. Replay happens on
// reconnects, where the caller has no other way to know what it missed.
func (c *HTTPClient) StreamRun(ctx context.Context, runID string) (*RunStream, error) {
	conn, err := c.dialWS(ctx, runID)
	if err != nil {
		return nil, err
	}
	streamCtx, cancel := context.WithCancel(ctx)
	s := &RunStream{
		events: make(chan apitypes.RunEventDTO),
		cancel: cancel,
	}
	s.wg.Add(1)
	go s.pump(streamCtx, c, runID, conn)
	return s, nil
}

// pump owns the socket, the reconnect loop and the reconcile ticker, and is the sole
// closer of s.events.
func (s *RunStream) pump(ctx context.Context, c *HTTPClient, runID string, conn *websocket.Conn) {
	defer s.wg.Done()
	defer close(s.events)

	reconcile := time.NewTicker(c.reconcileInterval())
	defer reconcile.Stop()
	reconcileStopped := false

	attempt := 0
	for {
		if conn == nil {
			// No socket: wait out the backoff and try again. The reconcile ticker is
			// in this select on purpose — an outage is exactly when the periodic state
			// re-read matters most, so it must not be starved by a 30s backoff.
			waiting := true
			for waiting {
				select {
				case <-ctx.Done():
					s.setErr(ctx.Err())
					return
				case <-reconcile.C:
					terminal := s.reconcile(ctx, c, runID)
					if terminal && !reconcileStopped {
						reconcile.Stop()
						reconcileStopped = true
					}
				case <-time.After(c.backoff(attempt)):
					waiting = false
				}
			}
			attempt++
			next, err := c.dialWS(ctx, runID)
			if err != nil {
				if ctx.Err() != nil {
					s.setErr(ctx.Err())
					return
				}
				continue // still no socket; back off and retry
			}
			conn = next
			if !s.replay(ctx, c, runID) {
				s.setErr(ctx.Err())
				return
			}
		}

		connCtx, connCancel := context.WithCancel(ctx)
		frames := make(chan []byte, 32)
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			readFrames(connCtx, conn, frames)
		}()

		live := true
		for live {
			select {
			case <-ctx.Done():
				connCancel()
				_ = conn.CloseNow()
				s.setErr(ctx.Err())
				return

			case <-reconcile.C:
				terminal := s.reconcile(ctx, c, runID)
				if terminal && !reconcileStopped {
					// The run reached a status it never leaves, so there is nothing
					// left for a re-read to discover. Stopping is not just thrift: a
					// user can leave a finished run open for a long time.
					reconcile.Stop()
					reconcileStopped = true
				}

			case raw, ok := <-frames:
				if !ok {
					live = false // socket died; reconnect below
					break
				}
				attempt = 0 // a delivered frame proves the connection healthy
				var ev apitypes.RunEventDTO
				if err := json.Unmarshal(raw, &ev); err != nil {
					// A frame we cannot parse is dropped, not fatal: one malformed
					// frame must not tear down a live run's view.
					continue
				}
				ev = NormalizeRunEvent(ev)
				if ev.Type == RunEventTypeMessage && ev.Seq > s.lastSeq {
					s.lastSeq = ev.Seq
				}
				if ev.Type == RunEventTypeState && ev.Status != "" {
					s.lastStatus = ev.Status
				}
				if !s.emit(ctx, ev) {
					connCancel()
					_ = conn.CloseNow()
					s.setErr(ctx.Err())
					return
				}
			}
		}

		// Socket died. Drop it and let the top of the loop back off, redial and
		// replay — one reconnect path, whether this is the first failure or the
		// twentieth.
		connCancel()
		_ = conn.CloseNow()
		conn = nil
	}
}

// readFrames pumps raw frames off conn until it fails or connCtx ends, closing out
// on the way so the pump sees the connection die.
func readFrames(connCtx context.Context, conn *websocket.Conn, out chan<- []byte) {
	defer close(out)
	for {
		_, data, err := conn.Read(connCtx)
		if err != nil {
			return
		}
		select {
		case out <- data:
		case <-connCtx.Done():
			return
		}
	}
}

// replay runs the post-reconnect recovery: every message after the last seq seen,
// then the authoritative run state.
//
// Both halves are required and neither substitutes for the other. RunLogs recovers
// the message log, which is the only seq-carrying frame type. GetRun recovers state,
// which carries NO seq — nothing in the message replay would reveal that the run
// finished while the socket was down.
func (s *RunStream) replay(ctx context.Context, c *HTTPClient, runID string) bool {
	msgs, err := c.RunLogs(ctx, runID, s.lastSeq)
	if err == nil {
		for _, m := range msgs {
			if m.Seq <= s.lastSeq {
				continue
			}
			created := m.CreatedAt
			if !s.emit(ctx, apitypes.RunEventDTO{
				Type:          RunEventTypeMessage,
				Seq:           m.Seq,
				Kind:          m.Kind,
				Agent:         m.Agent,
				AgentInstance: m.AgentInstance,
				AgentLabel:    m.AgentLabel,
				Payload:       m.Payload,
				CreatedAt:     &created,
			}) {
				return false
			}
			s.lastSeq = m.Seq
		}
	}
	// A failed replay is not fatal — the next reconcile tick and the live socket both
	// still work — so state is re-read regardless.
	s.reconcile(ctx, c, runID)
	return ctx.Err() == nil
}

// reconcile re-reads authoritative run state and emits a synthetic state frame when
// it differs from what the consumer was last told. It returns whether the run is
// terminal.
//
// Emitting only on CHANGE is what keeps this quiet in the steady state: a run that is
// still running produces no traffic, and the consumer cannot tell a recovered frame
// from a live one — which is the point, since it must handle both identically.
func (s *RunStream) reconcile(ctx context.Context, c *HTTPClient, runID string) bool {
	run, err := c.GetRun(ctx, runID)
	if err != nil {
		return false // best effort: the next tick retries
	}
	status := run.Status
	if _, ok := knownRunStatuses[status]; !ok {
		status = RunStatusUnknown
	}
	if status != s.lastStatus {
		s.lastStatus = status
		if !s.emit(ctx, apitypes.RunEventDTO{Type: RunEventTypeState, Status: status}) {
			return false
		}
	}
	return IsTerminalRunStatus(status)
}

// NewRunStream builds a stream that emits events in order and then stays OPEN until
// ctx is cancelled or Close is called. It backs FakeClient and any consumer test.
//
// Staying open is the faithful part: a live socket that has stopped producing is not
// a closed one, and a consumer that treats "no more events" as "run finished" is
// exactly the bug this package's reconcile exists to prevent. A fake that closed
// here would hide it.
func NewRunStream(ctx context.Context, events []apitypes.RunEventDTO) *RunStream {
	streamCtx, cancel := context.WithCancel(ctx)
	s := &RunStream{
		events: make(chan apitypes.RunEventDTO),
		cancel: cancel,
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer close(s.events)
		for _, ev := range events {
			if !s.emit(streamCtx, NormalizeRunEvent(ev)) {
				s.setErr(streamCtx.Err())
				return
			}
		}
		<-streamCtx.Done()
		s.setErr(streamCtx.Err())
	}()
	return s
}

// waitStopped blocks until the pump and its frame reader have both exited, or the
// timeout elapses. Test-only: it is the exact instrument for "no goroutine left
// behind", where a runtime.NumGoroutine() delta is only a rough one.
func (s *RunStream) waitStopped(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}
