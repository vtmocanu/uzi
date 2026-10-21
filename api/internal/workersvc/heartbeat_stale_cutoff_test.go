package workersvc

import (
	"testing"
	"time"
)

// TestHeartbeatStaleCutoff (issue #3181): a positive window is now minus the window; a non-positive
// window is an INVALID (NULL) cutoff so the settling fence keeps failing closed.
func TestHeartbeatStaleCutoff(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	got := HeartbeatStaleCutoff(now, 45*time.Second)
	if !got.Valid || !got.Time.Equal(now.Add(-45*time.Second)) {
		t.Fatalf("positive window = %+v, want valid %v", got, now.Add(-45*time.Second))
	}
	for _, w := range []time.Duration{0, -time.Second} {
		if c := HeartbeatStaleCutoff(now, w); c.Valid {
			t.Fatalf("window %v = %+v, want invalid (NULL)", w, c)
		}
	}
}
