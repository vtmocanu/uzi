package slacksvc

import (
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
)

// newTestSteerPendings builds a registry with a controllable clock the test advances.
func newTestSteerPendings(now *time.Time) *SteerPendings {
	s := NewSteerPendings()
	s.now = func() time.Time { return *now }
	return s
}

// Arm then Take by the arming user returns the target run and is a one-shot: a second
// Take finds nothing.
func TestSteerPendingsArmTakeOneShot(t *testing.T) {
	now := time.Now()
	s := newTestSteerPendings(&now)
	runID, userID := uuid.New(), uuid.New()

	s.Arm("D1", "root1", runID, userID)

	got, ok := s.Take("D1", "root1", userID)
	if !ok || got != runID {
		t.Fatalf("Take should return the armed run: got=%v ok=%v want=%v", got, ok, runID)
	}
	if _, ok := s.Take("D1", "root1", userID); ok {
		t.Fatalf("Take must be one-shot: the second Take must find nothing")
	}
}

// An expired pending is not taken — advancing the clock past the TTL prunes it.
func TestSteerPendingsExpiry(t *testing.T) {
	now := time.Now()
	s := newTestSteerPendings(&now)
	runID, userID := uuid.New(), uuid.New()

	s.Arm("D1", "root1", runID, userID)
	now = now.Add(steerPendingTTL + time.Second)

	if _, ok := s.Take("D1", "root1", userID); ok {
		t.Fatalf("an expired pending must not be taken")
	}
}

// A Take by a different user does NOT consume the pending — it stays armed for the real
// requester.
func TestSteerPendingsForeignUserDoesNotConsume(t *testing.T) {
	now := time.Now()
	s := newTestSteerPendings(&now)
	runID, owner, foreign := uuid.New(), uuid.New(), uuid.New()

	s.Arm("D1", "root1", runID, owner)

	if _, ok := s.Take("D1", "root1", foreign); ok {
		t.Fatalf("a foreign user must not consume the pending")
	}
	got, ok := s.Take("D1", "root1", owner)
	if !ok || got != runID {
		t.Fatalf("the owner's Take must still succeed after a foreign attempt: got=%v ok=%v", got, ok)
	}
}

// Arming twice on the same thread is last-write-wins: the second target is what Take
// returns.
func TestSteerPendingsCollisionLastWins(t *testing.T) {
	now := time.Now()
	s := newTestSteerPendings(&now)
	first, second, userID := uuid.New(), uuid.New(), uuid.New()

	s.Arm("D1", "root1", first, userID)
	s.Arm("D1", "root1", second, userID)

	got, ok := s.Take("D1", "root1", userID)
	if !ok || got != second {
		t.Fatalf("a collision must be last-write-wins: got=%v want=%v", got, second)
	}
}

// The cap is a HARD bound: arming more than steerPendingsCap distinct live keys keeps
// the map at the cap by evicting the soonest-to-expire entry, and the most recently
// armed pending survives.
func TestSteerPendingsCapIsHardBound(t *testing.T) {
	now := time.Now()
	s := newTestSteerPendings(&now)
	userID := uuid.New()

	var lastThread string
	var lastRun uuid.UUID
	for i := 0; i < steerPendingsCap+50; i++ {
		// Advance the clock a tick per arm so every live entry has a distinct expiry and
		// the "soonest to expire" the cap evicts is the earliest-armed one.
		now = now.Add(time.Millisecond)
		lastThread = "root" + strconv.Itoa(i)
		lastRun = uuid.New()
		s.Arm("D1", lastThread, lastRun, userID)
	}

	s.mu.Lock()
	n := len(s.m)
	s.mu.Unlock()
	if n > steerPendingsCap {
		t.Fatalf("map must be hard-bounded at the cap: len=%d cap=%d", n, steerPendingsCap)
	}

	got, ok := s.Take("D1", lastThread, userID)
	if !ok || got != lastRun {
		t.Fatalf("the most recently armed pending must survive the cap: got=%v ok=%v", got, ok)
	}
	if _, ok := s.Take("D1", "root0", userID); ok {
		t.Fatalf("the earliest-armed (soonest-to-expire) pending must have been evicted under cap pressure")
	}
}

// A pending is keyed by (channel, threadTS): a Take on a different thread or channel
// finds nothing.
func TestSteerPendingsKeyedByThread(t *testing.T) {
	now := time.Now()
	s := newTestSteerPendings(&now)
	runID, userID := uuid.New(), uuid.New()

	s.Arm("D1", "root1", runID, userID)

	if _, ok := s.Take("D1", "root2", userID); ok {
		t.Fatalf("a reply on a different thread must not consume the pending")
	}
	if _, ok := s.Take("D2", "root1", userID); ok {
		t.Fatalf("a reply in a different channel must not consume the pending")
	}
	if _, ok := s.Take("D1", "root1", userID); !ok {
		t.Fatalf("the exact-key Take must still succeed")
	}
}
