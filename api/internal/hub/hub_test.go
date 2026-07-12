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

	h.PublishMessage(runA, 7, "text", "coder", json.RawMessage(`{"t":"hi"}`), time.Now())

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
	h.PublishHealth(run, "stalled", "the agent stopped sending updates")
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
	h.PublishMessage(run, 1, "text", "", json.RawMessage(`{}`), time.Now())
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
			h.PublishMessage(run, int32(i+1), "text", "", json.RawMessage(`{}`), time.Now())
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("broadcast blocked on a full subscriber buffer")
	}
}
