package main

import (
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// TestRunGetShowsWeeklyReserveHold (PRD #3522 M3): a queued run the weekly usage reserve holds renders
// a RESERVE row with the visible reason and spells out the weekly_reserve queue reason; a run without
// either gets no row, and hostile text in the persisted reason is sanitized.
func TestRunGetShowsWeeklyReserveHold(t *testing.T) {
	reason := "weekly usage reserve: 72% >= 70%"
	queue := "weekly_reserve"
	fc := &uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{
		"r1": {ID: "r1", Status: "queued", QueueReason: &queue, ReserveHoldReason: &reason},
		"r2": {ID: "r2", Status: "queued"},
		"r3": {ID: "r3", Status: "queued", ReserveHoldReason: strp(hostileANSI)},
	}}
	out, _, _ := runCLI(t, fakeEnv(fc), "run", "get", "r1")
	if !strings.Contains(out, "RESERVE") || !strings.Contains(out, reason) {
		t.Errorf("reserve reason not rendered:\n%s", out)
	}
	if !strings.Contains(out, "QUEUE_REASON") || !strings.Contains(out, "weekly_reserve (held by your weekly usage reserve)") {
		t.Errorf("weekly_reserve queue reason not spelled out:\n%s", out)
	}
	if out, _, _ = runCLI(t, fakeEnv(fc), "run", "get", "r2"); strings.Contains(out, "RESERVE") {
		t.Errorf("a run without a reserve hold got a RESERVE row:\n%s", out)
	}
	if out, _, _ = runCLI(t, fakeEnv(fc), "run", "get", "r3"); strings.ContainsRune(out, 0x1b) {
		t.Errorf("reserve reason not sanitized: %q", out)
	}
}

func TestQueueReasonCellSpellsOutBothReasons(t *testing.T) {
	if got := queueReasonCell("maintenance"); !strings.Contains(got, "maintenance mode") {
		t.Errorf("maintenance = %q", got)
	}
	if got := queueReasonCell("weekly_reserve"); !strings.Contains(got, "weekly usage reserve") {
		t.Errorf("weekly_reserve = %q", got)
	}
	if got := queueReasonCell("future_reason"); got != "future_reason" {
		t.Errorf("an unknown reason must still render verbatim, got %q", got)
	}
}
