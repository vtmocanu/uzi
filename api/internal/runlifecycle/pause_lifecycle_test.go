package runlifecycle

import (
	"testing"

	"github.com/vtmocanu/uzi/api/internal/board"
)

// TestReconcilerDecisionPaused pins that the owner-requested park (PRD #1190 M1) lands In
// Progress, like the other parks (limit_wait/pool_wait/awaiting_*): a paused run still holds
// its issue, session and worker affinity and resumes on demand, so the default {act:false}
// arm would leave its pending move marker unhealed and trip the 30m give-up warn as the
// normal case.
func TestReconcilerDecisionPaused(t *testing.T) {
	got := reconcilerDecision("paused", txt("Later"))
	want := decision{act: true, target: board.ColumnInProgress}
	if got != want {
		t.Fatalf("reconcilerDecision(\"paused\") = %+v, want %+v", got, want)
	}
}
