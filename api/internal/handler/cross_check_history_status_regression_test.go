package handler

import "testing"

// Current runs_status_check, validated by migrations 00206/00207.
func TestCrossCheckHistoryRecordedLifecycleStates(t *testing.T) {
	for _, status := range []string{"queued", "claimed", "running", "awaiting_approval", "awaiting_input", "limit_wait", "completed", "failed", "cancelled", "awaiting_followup", "pool_wait", "paused", "recovery_wait"} {
		t.Run(status, func(t *testing.T) {
			if got := historyStatus(status); got != status {
				t.Fatalf("recorded lifecycle %q became %q", status, got)
			}
		})
	}
	if got := historyStatus("untrusted unknown status"); got != "unknown" {
		t.Fatal("unknown lifecycle reflected")
	}
}
