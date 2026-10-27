package workersvc

import (
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// A run the weekly usage reserve parked (PRD #3522) names its cause and both ways out in the worker
// upgrade blockers; an ordinary pause and any other hold keep the plain owner action.
func TestUpgradeBlockerActionWeeklyReservePause(t *testing.T) {
	row := func(hold string) store.ListWorkerUpgradeBlockersRow {
		r := store.ListWorkerUpgradeBlockersRow{Status: blockerText("paused")}
		if hold != "" {
			r.HoldReason = blockerText(hold)
		}
		return r
	}
	got := blockerAction(row("weekly_reserve"))
	for _, part := range []string{"weekly usage reserve", "usage drops below the threshold", "owner resumes the run"} {
		if !strings.Contains(got, part) {
			t.Errorf("reserve-paused action = %q, want it to contain %q", got, part)
		}
	}
	for _, hold := range []string{"", "budget_exhausted"} {
		if got := blockerAction(row(hold)); got != "owner resumes the run" {
			t.Errorf("hold %q action = %q, want the plain owner action", hold, got)
		}
	}
}
