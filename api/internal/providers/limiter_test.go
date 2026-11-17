package providers

import (
	"fmt"
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

func newTestLimiter() (*ProbeLimiter, *fakeClock) {
	c := &fakeClock{t: time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)}
	l := NewProbeLimiter()
	l.now = c.now
	return l, c
}

func TestProbeLimiterOneInFlightPerOwner(t *testing.T) {
	l, _ := newTestLimiter()
	release, _, _, ok := l.Acquire("alice")
	if !ok {
		t.Fatal("first probe refused")
	}
	if _, reason, retry, ok := l.Acquire("alice"); ok || reason != LimitOwnerBusy || retry <= 0 {
		t.Fatalf("second concurrent probe = %q, %v, %v", reason, retry, ok)
	}
	// Another owner is not affected.
	relB, _, _, ok := l.Acquire("bob")
	if !ok {
		t.Fatal("another owner refused")
	}
	relB()
	release()
	release() // idempotent: must not free a slot twice
	if l.inflight != 0 {
		t.Fatalf("inflight = %d after releases", l.inflight)
	}
	if _, _, _, ok := l.Acquire("alice"); !ok {
		t.Error("probe refused after release")
	}
}

func TestProbeLimiterInstanceWide(t *testing.T) {
	l, _ := newTestLimiter()
	var releases []func()
	for i := 0; i < probeMaxInFlight; i++ {
		rel, _, _, ok := l.Acquire(fmt.Sprintf("owner-%d", i))
		if !ok {
			t.Fatalf("probe %d refused", i)
		}
		releases = append(releases, rel)
	}
	if _, reason, retry, ok := l.Acquire("one-too-many"); ok || reason != LimitBusy || retry <= 0 {
		t.Fatalf("fifth concurrent probe = %q, %v, %v", reason, retry, ok)
	}
	// The refusal did not consume the refused owner's hourly allowance.
	releases[0]()
	for i := 0; i < probeMaxPerOwnerPerHour; i++ {
		rel, _, _, ok := l.Acquire("one-too-many")
		if !ok {
			t.Fatalf("probe %d for the previously refused owner = refused", i)
		}
		rel()
	}
}

func TestProbeLimiterHourlyPerOwnerSlidingWindow(t *testing.T) {
	l, clock := newTestLimiter()
	start := clock.t
	for i := 0; i < probeMaxPerOwnerPerHour; i++ {
		rel, reason, _, ok := l.Acquire("alice")
		if !ok {
			t.Fatalf("probe %d refused: %s", i, reason)
		}
		rel()
		clock.t = clock.t.Add(time.Minute)
	}
	// 30 probes in the last 30 minutes: the 31st is refused until the first leaves the window.
	_, reason, retry, ok := l.Acquire("alice")
	if ok || reason != LimitHourly {
		t.Fatalf("31st probe = %q, %v", reason, ok)
	}
	if want := start.Add(time.Hour).Sub(clock.t); retry != want {
		t.Errorf("Retry-After = %v, want %v (when the oldest probe leaves the window)", retry, want)
	}
	// Another owner has its own allowance.
	if rel, _, _, ok := l.Acquire("bob"); !ok {
		t.Error("another owner refused by alice's allowance")
	} else {
		rel()
	}
	// Just before the oldest probe ages out: still refused. Just after: one slot.
	clock.t = start.Add(time.Hour - time.Second)
	if _, _, _, ok := l.Acquire("alice"); ok {
		t.Error("admitted before the oldest probe left the window")
	}
	clock.t = start.Add(time.Hour + time.Second)
	rel, _, _, ok := l.Acquire("alice")
	if !ok {
		t.Fatal("refused after the oldest probe left the window")
	}
	rel()
	if _, _, _, ok := l.Acquire("alice"); ok {
		t.Error("a second slot opened when only one probe aged out")
	}
	// A full hour later everything has aged out.
	clock.t = clock.t.Add(2 * time.Hour)
	if rel, _, _, ok := l.Acquire("alice"); !ok {
		t.Error("refused after the whole window aged out")
	} else {
		rel()
	}
}

func TestProbeLimiterSweepKeepsActiveOwners(t *testing.T) {
	l, clock := newTestLimiter()
	hold, _, _, ok := l.Acquire("running")
	if !ok {
		t.Fatal("refused")
	}
	for i := 0; i < 300; i++ {
		rel, _, _, _ := l.Acquire(fmt.Sprintf("o%d", i))
		if rel != nil {
			rel()
		}
	}
	clock.t = clock.t.Add(2 * time.Hour)
	rel, _, _, _ := l.Acquire("trigger")
	rel()
	if _, ok := l.owners["o1"]; ok {
		t.Error("an idle owner with an aged-out window was kept")
	}
	if _, _, _, ok := l.Acquire("running"); ok {
		t.Error("the owner with a probe in flight lost its slot to the sweep")
	}
	hold()
}
