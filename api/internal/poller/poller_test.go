package poller

import "testing"

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
