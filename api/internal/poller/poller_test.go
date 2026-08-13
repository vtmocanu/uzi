package poller

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestReconcileDue(t *testing.T) {
	const every = 10
	// The first poll after (re)enable is always a full reconcile (seeds the
	// cache and establishes the eviction baseline), then every 10th poll.
	fullPolls := map[int]bool{1: true, 10: true, 20: true, 30: true}
	for poll := 1; poll <= 31; poll++ {
		got := reconcileDue(poll, every)
		want := fullPolls[poll]
		if got != want {
			t.Errorf("reconcileDue(%d, %d) = %v, want %v", poll, every, got, want)
		}
	}
}

func TestReconcileDueEveryOne(t *testing.T) {
	// reconcileEvery=1 (the clamp floor): every poll is a full reconcile.
	for poll := 1; poll <= 5; poll++ {
		if !reconcileDue(poll, 1) {
			t.Errorf("reconcileDue(%d, 1) = false, want true", poll)
		}
	}
}

func TestTickBudget(t *testing.T) {
	// tickBudget floors the per-tick deadline at 2x the forge HTTP timeout so a poll
	// interval shorter than the forge calls a tick makes cannot preempt an in-flight
	// sync (issue #139). Deterministic: pure function of its two duration inputs.
	tests := []struct {
		name         string
		interval     time.Duration
		forgeTimeout time.Duration
		want         time.Duration
	}{
		{"interval below the 2x floor is floored (the e2e 2s case)", 2 * time.Second, 15 * time.Second, 30 * time.Second},
		{"interval between 1x and 2x is still floored", 20 * time.Second, 15 * time.Second, 30 * time.Second},
		{"interval above the 2x floor is unchanged (production)", time.Minute, 15 * time.Second, time.Minute},
		{"interval equal to the 2x floor is unchanged", 30 * time.Second, 15 * time.Second, 30 * time.Second},
		{"zero forge timeout disables the floor (pre-#139 behaviour)", 2 * time.Second, 0, 2 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tickBudget(tt.interval, tt.forgeTimeout); got != tt.want {
				t.Errorf("tickBudget(%v, %v) = %v, want %v", tt.interval, tt.forgeTimeout, got, tt.want)
			}
		})
	}
}

func TestForceReconcileNonBlockingAndCoalesces(t *testing.T) {
	// The settings PUT handler must never block on the poller, and a burst of
	// changes must not queue a backlog of reconciles: repeated signals coalesce
	// into the single buffered slot.
	e := New(nil, nil, time.Minute, 10)
	for i := 0; i < 5; i++ {
		e.ForceReconcile() // would deadlock a test on a blocking send
	}
	if n := len(e.forceReconcile); n != 1 {
		t.Fatalf("pending reconcile signals = %d, want 1 (coalesced)", n)
	}
}

func TestResetReconcileStateForcesFullSyncNextTick(t *testing.T) {
	e := New(nil, nil, time.Minute, 10)
	// Two repos mid-cycle, neither due for a reconcile on its own.
	e.states[uuid.New()] = &repoState{pollCount: 5}
	e.states[uuid.New()] = &repoState{pollCount: 12}

	e.resetReconcileState()

	for id, st := range e.states {
		if st.pollCount != 0 {
			t.Fatalf("repo %s pollCount = %d after reset, want 0", id, st.pollCount)
		}
		// The next tick increments to 1, which reconcileDue treats as a full sync.
		if !reconcileDue(st.pollCount+1, e.reconcileEvery) {
			t.Fatalf("repo %s not due for reconcile on the tick after reset", id)
		}
	}
}
