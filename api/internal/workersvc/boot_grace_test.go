package workersvc

import (
	"testing"
	"time"
)

// TestBootGraceActive pins the pure boot-grace decision (PRD #1390 M1, D1): whether the three
// stale-worker passes are suppressed this sweep tick. It is a pure function of the knob, the
// listener-ready timestamp and the current time, so it needs no DB.
func TestBootGraceActive(t *testing.T) {
	ready := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	grace := 60 * time.Second

	cases := []struct {
		name    string
		grace   time.Duration
		readyAt time.Time
		now     time.Time
		want    bool
	}{
		{
			// SWEEPER_BOOT_GRACE=0 ⇒ never active — today's immediate behaviour, even before bind.
			name: "grace zero is never active", grace: 0, readyAt: time.Time{}, now: ready, want: false,
		},
		{
			// grace zero with a ready listener and a now far in the future is still never active.
			name: "grace zero with ready set", grace: 0, readyAt: ready, now: ready.Add(time.Hour), want: false,
		},
		{
			// readyAt zero (the listener has not bound yet) with grace>0 ⇒ active: no worker
			// could have reconnected, so nothing may be swept as stale.
			name: "ready zero with grace is active", grace: grace, readyAt: time.Time{}, now: ready, want: true,
		},
		{
			// now strictly before readyAt+grace ⇒ active.
			name: "within window is active", grace: grace, readyAt: ready, now: ready.Add(59 * time.Second), want: true,
		},
		{
			// now exactly at readyAt+grace ⇒ elapsed (Before is strict), so not active.
			name: "at boundary is elapsed", grace: grace, readyAt: ready, now: ready.Add(60 * time.Second), want: false,
		},
		{
			// now after readyAt+grace ⇒ elapsed.
			name: "after window is elapsed", grace: grace, readyAt: ready, now: ready.Add(61 * time.Second), want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := bootGraceActive(tc.grace, tc.readyAt, tc.now); got != tc.want {
				t.Fatalf("bootGraceActive(%v, %v, %v) = %v, want %v", tc.grace, tc.readyAt, tc.now, got, tc.want)
			}
		})
	}
}

// TestSetReadyAtRoundTrip pins the race-safe accessor pair: SetReadyAt stores the moment and
// readyAtTime reads it back (to the nanosecond), and a zero time clears it back to "not ready".
func TestSetReadyAtRoundTrip(t *testing.T) {
	s := &Service{}
	if got := s.readyAtTime(); !got.IsZero() {
		t.Fatalf("fresh Service readyAtTime = %v, want zero (not ready)", got)
	}
	ts := time.Date(2026, 9, 16, 12, 0, 0, 123456789, time.UTC)
	s.SetReadyAt(ts)
	if got := s.readyAtTime(); !got.Equal(ts) {
		t.Fatalf("readyAtTime = %v, want %v", got, ts)
	}
	s.SetReadyAt(time.Time{})
	if got := s.readyAtTime(); !got.IsZero() {
		t.Fatalf("readyAtTime after clear = %v, want zero", got)
	}
}
