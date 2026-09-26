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

// TestRunAgeCellWaitingUsesStatusSince pins issue #1727 on `uzi run list`'s AGE column: a
// waiting-bucket run ages from status_since, not from a later updated_at, and falls back to
// updated_at when status_since is absent (an older server).
func TestRunAgeCellWaitingUsesStatusSince(t *testing.T) {
	now := time.Date(2026, 9, 7, 13, 42, 0, 0, time.UTC)
	since := now.Add(-2*time.Hour - 40*time.Minute)
	updated := now.Add(-5 * time.Minute)

	for _, status := range []string{
		"awaiting_approval", "awaiting_input", "awaiting_followup",
		statusLimitWait, statusPoolWait, statusRecoveryWait,
	} {
		withSince := apitypes.RunDTO{Status: status, UpdatedAt: updated, StatusSince: &since}
		if got, want := runAgeCell(withSince, now), "2h 40m"; got != want {
			t.Errorf("runAgeCell(%s) with status_since = %q, want %q", status, got, want)
		}
		fallback := apitypes.RunDTO{Status: status, UpdatedAt: updated}
		if got, want := runAgeCell(fallback, now), "5m"; got != want {
			t.Errorf("runAgeCell(%s) without status_since = %q, want %q (updated_at fallback)", status, got, want)
		}
	}
}
