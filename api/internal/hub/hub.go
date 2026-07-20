// Package hub is the browser live-event fan-out for agent runs. It is a pure
// in-process pub/sub keyed by run id: the worker-protocol handlers persist every
// message and state change to the DB first (the source of truth) and then poke
// the hub, which forwards a small JSON frame to any browser currently subscribed
// to that run.
//
// The hub is deliberately a LIVE/cache-invalidation channel, never authoritative:
// a frame may be dropped for a slow subscriber (bounded buffer), and the client
// recovers by replaying from its last-seen seq over REST on any gap or reconnect
// (the lossless guarantee lives in the persisted run_messages log, not here).
package hub

import (
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
)

// subBuffer is the per-subscriber frame buffer. A subscriber that falls this far
// behind has a frame dropped; the client detects the resulting seq gap and does a
// REST catch-up, so no data is lost — only the WS fast-path is skipped.
const subBuffer = 256

// Event is one WS frame. "message" carries a persisted run message (the client
// renders it directly, deduped by seq); "state" signals a run state change,
// "health" a run-health flag change (PRD #47), and "input" a steer-queue change
// (PRD #95) — for all three the client re-reads over REST, since WS never carries
// authoritative run state.
type Event struct {
	Type  string  `json:"type"` // "message" | "state" | "health" | "input"
	Seq   int32   `json:"seq,omitempty"`
	Kind  string  `json:"kind,omitempty"`
	Agent *string `json:"agent,omitempty"`
	// AgentInstance/AgentLabel are the PRD #99 subagent invocation id + task
	// label. The browser lanes a live frame off these without a REST re-read, so
	// they must ride the frame exactly as Agent does. Absent when the frame
	// carried no parent_tool_use_id (which is NOT the same as Agent == "lead").
	AgentInstance *string         `json:"agent_instance,omitempty"`
	AgentLabel    *string         `json:"agent_label,omitempty"`
	Payload       json.RawMessage `json:"payload,omitempty"`
	CreatedAt     *time.Time      `json:"created_at,omitempty"`
	Status        string          `json:"status,omitempty"` // set on "state" frames
}

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
// non-blocking: a subscriber whose buffer is full has this frame dropped (it
// recovers via REST replay on the resulting gap). Marshal failures are logged and
// skipped rather than propagated — a broadcast must never break the write path
// that persisted the underlying event.
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
			// Buffer full: drop. The client's seq-gap detection triggers a REST
			// catch-up, so the dropped frame is not lost — only its WS fast-path.
		}
	}
}
