package main

import (
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// TestStatusClausesUseStatusSince pins issue #1727 on the CLI: the paused "since" clause and
// the hold "parked" clause read the instant the run entered its status (status_since), not
// updated_at, which any unrelated write to the row moves. With status_since absent (an older
// server) both fall back to updated_at.
func TestStatusClausesUseStatusSince(t *testing.T) {
	now := time.Date(2026, 9, 7, 13, 42, 0, 0, time.UTC)
	since := time.Date(2026, 9, 7, 11, 2, 0, 0, time.UTC)    // 2h40m before now
	updated := time.Date(2026, 9, 7, 13, 37, 0, 0, time.UTC) // a later unrelated write, 5m before now

	withSince := apitypes.RunDTO{UpdatedAt: updated, StatusSince: &since}
	if got, want := pauseSinceClause(withSince, now), "since 11:02 (2h40m)"; got != want {
		t.Errorf("pauseSinceClause with status_since = %q, want %q", got, want)
	}
	if got, want := holdParkedClause(withSince, now), "parked 11:02 (2h40m)"; got != want {
		t.Errorf("holdParkedClause with status_since = %q, want %q", got, want)
	}

	fallback := apitypes.RunDTO{UpdatedAt: updated}
	if got, want := pauseSinceClause(fallback, now), "since 13:37 (5m)"; got != want {
		t.Errorf("pauseSinceClause without status_since = %q, want %q (updated_at fallback)", got, want)
	}
	if got, want := holdParkedClause(fallback, now), "parked 13:37 (5m)"; got != want {
		t.Errorf("holdParkedClause without status_since = %q, want %q (updated_at fallback)", got, want)
	}
}
