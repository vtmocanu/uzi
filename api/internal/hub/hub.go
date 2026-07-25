// Package hub is the browser live-event fan-out for agent runs. It is a pure
// in-process pub/sub keyed by run id: the worker-protocol handlers persist every
// message and state change to the DB first (the source of truth) and then poke
// the hub, which forwards a small JSON frame to any browser currently subscribed
// to that run.
//
// The hub is deliberately a LIVE/cache-invalidation channel, never authoritative:
// a frame may be dropped for a slow subscriber (bounded buffer). The lossless
// guarantee lives in the persisted run_messages log, not here — the client replays
// from its last-seen seq over REST on any gap or reconnect.
//
// That replay covers "message" frames and ONLY those: they are the only type
// carrying a seq, so they are the only type whose loss a client can detect. A
// dropped state/health/input frame is undetectable and unrecovered by replay, and
// needs a periodic re-read instead. broadcast documents the consequence in full;
// it is the single most misread property of this package.
package hub

import (
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"gitlab.example.com/vtmocanu/uzi/api/internal/apitypes"
)

// subBuffer is the per-subscriber frame buffer. A subscriber that falls this far
// behind has a frame dropped. For a "message" that costs only the WS fast-path (its
// seq gap drives a REST catch-up); for the seq-less frame types the signal is gone,
// which is why every consumer needs a time-based reconcile. See broadcast.
const subBuffer = 256

// Event is one WS frame. "message" carries a persisted run message (the client
// renders it directly, deduped by seq); "state" signals a run state change,
// "health" a run-health flag change (PRD #47), and "input" a steer-queue change
// (PRD #95) — for all three the client re-reads over REST, since WS never carries
// authoritative run state.
//
// It is an ALIAS, not a copy: the shape lives in apitypes because PRD #112 M1 put
// /api/ws on the RequireUser routes, and the uzi CLI now decodes these frames too.
// An alias (not a defined type) is what makes the server and the CLI provably the
// same shape — a second definition could drift a tag and nothing would fail. The
// field documentation lives with the type; see apitypes.RunEventDTO.
type Event = apitypes.RunEventDTO

// Subscription is one browser's view of a single run's live events.
type Subscription struct {
	runID uuid.UUID
	ch    chan []byte
	hub   *Hub
}

// Events is the receive side; each value is a marshaled Event JSON frame.
func (s *Subscription) Events() <-chan []byte { return s.ch }

// Close removes the subscription from its hub. Safe to call once.
func (s *Subscription) Close() { s.hub.unsubscribe(s) }

// Hub fans run events out to subscribers keyed by run id.
type Hub struct {
	mu   sync.Mutex
	subs map[uuid.UUID]map[*Subscription]struct{}
}

// New constructs an empty Hub.
func New() *Hub {
	return &Hub{subs: make(map[uuid.UUID]map[*Subscription]struct{})}
}

// Subscribe registers a new subscriber for a run and returns it. The caller must
// Close it when the connection ends.
func (h *Hub) Subscribe(runID uuid.UUID) *Subscription {
	s := &Subscription{runID: runID, ch: make(chan []byte, subBuffer), hub: h}
	h.mu.Lock()
	defer h.mu.Unlock()
	m := h.subs[runID]
	if m == nil {
		m = make(map[*Subscription]struct{})
		h.subs[runID] = m
	}
	m[s] = struct{}{}
	return s
}

func (h *Hub) unsubscribe(s *Subscription) {
	h.mu.Lock()
	defer h.mu.Unlock()
	m := h.subs[s.runID]
	if m == nil {
		return
	}
	if _, ok := m[s]; !ok {
		return
	}
	delete(m, s)
	close(s.ch)
	if len(m) == 0 {
		delete(h.subs, s.runID)
	}
}

// PublishMessage forwards a persisted run message to a run's subscribers. It
// satisfies the workersvc broadcaster contract.
func (h *Hub) PublishMessage(runID uuid.UUID, seq int32, kind, agent, agentInstance, agentLabel string, payload []byte, createdAt time.Time) {
	ev := Event{Type: "message", Seq: seq, Kind: kind, Payload: json.RawMessage(payload), CreatedAt: &createdAt}
	if agent != "" {
		ev.Agent = &agent
	}
	if agentInstance != "" {
		ev.AgentInstance = &agentInstance
	}
	if agentLabel != "" {
		ev.AgentLabel = &agentLabel
	}
	h.broadcast(runID, ev)
}

// PublishState signals a run state change to its subscribers.
func (h *Hub) PublishState(runID uuid.UUID, status string) {
	h.broadcast(runID, Event{Type: "state", Status: status})
}

// PublishHealth signals a run-health flag change (PRD #47) to its subscribers. Like
// a "state" frame it carries NO authoritative data — the browser re-reads the run
// over REST, which applies the owner-gating on health_reason, so the flag reason
// never rides the socket. The health/reason/nudge args are part of the shared
// Broadcaster contract (the Slack notifier consumes them); the hub needs only to
// prompt the repaint, so it ignores them.
func (h *Hub) PublishHealth(runID uuid.UUID, health, reason string, nudge bool) {
	h.broadcast(runID, Event{Type: "health"})
}

// PublishInput signals that a run's steer queue changed (PRD #95) — a follow-up was
// consumed, stamping consumed_at. Like a "state" or "health" frame it carries NO
// authoritative data: the browser re-reads GET /runs/{id}/inputs, which owner-gates
// the steer text, so it never rides the socket. This frame is a fast-path only — the
// client also refetches inputs on mount/reconnect/state/health, so a hub-dropped
// frame self-heals (Decision 5).
func (h *Hub) PublishInput(runID uuid.UUID) {
	h.broadcast(runID, Event{Type: "input"})
}

// broadcast marshals ev once and sends it to every subscriber of runID,
// non-blocking: a subscriber whose buffer is full has this frame dropped. Marshal
// failures are logged and skipped rather than propagated — a broadcast must never
// break the write path that persisted the underlying event.
//
// WHAT A DROP COSTS DEPENDS ON THE FRAME TYPE, and only one of the four self-heals.
// This comment used to say a dropped frame "is not lost — only its WS fast-path",
// which is true for "message" and FALSE for the other three:
//
//   - "message" frames carry a per-run gapless Seq, so a drop leaves a hole the
//     client sees and repairs with a REST replay from its last-seen seq.
//   - "state", "health" and "input" carry NO Seq. A drop produces no gap, so there
//     is nothing to detect and nothing triggers a re-read: the signal is simply
//     gone until the next frame of that type, which for a TERMINAL state frame
//     never comes. A completed run then reads as still running for as long as the
//     consumer trusts the socket.
//
// The recovery for the second bullet cannot live here — it is the consumer's, and
// it must be time-based rather than event-based, because no event will arrive to
// trigger it. uzicli.StreamRun closes it with a periodic GetRun reconcile; the web
// re-reads on its own signals. A new consumer of this hub MUST provide one of the
// two, or it will render terminal runs as live.
func (h *Hub) broadcast(runID uuid.UUID, ev Event) {
	frame, err := json.Marshal(ev)
	if err != nil {
		slog.Error("hub marshal event", "error", err, "run", runID)
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs[runID] {
		select {
		case s.ch <- frame:
		default:
			// Buffer full: drop. Recoverable for a "message" (its seq gap triggers a
			// REST catch-up); NOT self-healing for state/health/input, which carry no
			// seq — see the note above this function before assuming otherwise.
		}
	}
}
