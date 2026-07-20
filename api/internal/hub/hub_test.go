package hub

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
)

// readFrame decodes one Event from a subscription within a short deadline.
func readFrame(t *testing.T, s *Subscription) Event {
	t.Helper()
	select {
	case raw, ok := <-s.Events():
		if !ok {
			t.Fatal("subscription channel closed unexpectedly")
		}
		var ev Event
		if err := json.Unmarshal(raw, &ev); err != nil {
			t.Fatalf("unmarshal frame: %v", err)
		}
		return ev
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for a frame")
		return Event{}
	}
}

func TestPublishMessageReachesOnlyItsRunSubscribers(t *testing.T) {
	h := New()
	runA, runB := uuid.New(), uuid.New()
	subA := h.Subscribe(runA)
	defer subA.Close()
	subB := h.Subscribe(runB)
	defer subB.Close()

	h.PublishMessage(runA, 7, "text", "coder", "", "", json.RawMessage(`{"t":"hi"}`), time.Now())

	ev := readFrame(t, subA)
	if ev.Type != "message" || ev.Seq != 7 || ev.Kind != "text" {
		t.Fatalf("unexpected event: %+v", ev)
	}
	if ev.Agent == nil || *ev.Agent != "coder" {
		t.Fatalf("expected agent coder, got %+v", ev.Agent)
	}
	// runB must not have received A's message.
	select {
	case <-subB.Events():
		t.Fatal("a message for run A leaked to a run B subscriber")
	case <-time.After(50 * time.Millisecond):
	}
}

// TestPublishMessageCarriesInstanceAndLabel pins the live-frame half of PRD #99:
// the browser lanes a message off the WS frame itself (useRunStream builds its
// RunMessage from the frame, no REST re-read), so a subagent frame that loses its
// instance id lands in the wrong lane until the next replay. Empty must stay
// ABSENT from the JSON (omitempty on a nil pointer), matching how `agent` behaves
// — the web reads `?? null` and falls back to the role name.
func TestPublishMessageCarriesInstanceAndLabel(t *testing.T) {
	h := New()
	run := uuid.New()
	sub := h.Subscribe(run)
	defer sub.Close()

	h.PublishMessage(run, 3, "tool_use", "coder", "toolu_A", "web gate UX", json.RawMessage(`{}`), time.Now())
	ev := readFrame(t, sub)
	if ev.AgentInstance == nil || *ev.AgentInstance != "toolu_A" {
		t.Fatalf("frame should carry agent_instance, got %+v", ev.AgentInstance)
	}
	if ev.AgentLabel == nil || *ev.AgentLabel != "web gate UX" {
		t.Fatalf("frame should carry agent_label, got %+v", ev.AgentLabel)
	}

	// A lead frame carries neither; the keys are omitted, not emitted as "".
	h.PublishMessage(run, 4, "text", "lead", "", "", json.RawMessage(`{}`), time.Now())
	raw, ok := <-sub.Events()
	if !ok {
		t.Fatal("subscription closed")
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, present := wire["agent_instance"]; present {
		t.Fatalf("a lead frame must omit agent_instance entirely, got %s", raw)
	}
	if _, present := wire["agent_label"]; present {
		t.Fatalf("a lead frame must omit agent_label entirely, got %s", raw)
	}
}

func TestPublishStateCarriesStatus(t *testing.T) {
	h := New()
	run := uuid.New()
	sub := h.Subscribe(run)
	defer sub.Close()

	h.PublishState(run, "awaiting_approval")
	ev := readFrame(t, sub)
	if ev.Type != "state" || ev.Status != "awaiting_approval" {
		t.Fatalf("unexpected state event: %+v", ev)
	}
}

func TestPublishHealthEmitsHealthFrameWithoutReason(t *testing.T) {
	h := New()
	run := uuid.New()
	sub := h.Subscribe(run)
	defer sub.Close()

	// The reason is passed for the shared Broadcaster contract but must NOT ride the
	// socket — the browser re-reads the run (owner-gated) instead.
	h.PublishHealth(run, "stalled", "the agent stopped sending updates", true)
	ev := readFrame(t, sub)
	if ev.Type != "health" {
		t.Fatalf("type = %q, want health", ev.Type)
	}
	if ev.Status != "" {
		t.Fatalf("health frame must carry no status/reason, got status=%q", ev.Status)
	}
}

func TestUnsubscribeStopsDelivery(t *testing.T) {
	h := New()
	run := uuid.New()
	sub := h.Subscribe(run)
	sub.Close()
	// Publishing to a run with no live subscribers must not panic or block.
	h.PublishMessage(run, 1, "text", "", "", "", json.RawMessage(`{}`), time.Now())
	if _, ok := <-sub.Events(); ok {
		t.Fatal("closed subscription should yield a closed channel")
	}
}

func TestBroadcastNeverBlocksOnAFullBuffer(t *testing.T) {
	h := New()
	run := uuid.New()
	sub := h.Subscribe(run)
	defer sub.Close()
	// Overfill the buffer well past capacity: a slow browser must not wedge the
	// persistence path — excess frames are dropped, not blocked on.
	done := make(chan struct{})
	go func() {
		for i := 0; i < subBuffer*3; i++ {
			h.PublishMessage(run, int32(i+1), "text", "", "", "", json.RawMessage(`{}`), time.Now())
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("broadcast blocked on a full subscriber buffer")
	}
}
