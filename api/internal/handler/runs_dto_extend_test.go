package handler

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// dtoTestNow is a fixed wall-clock instant for runToDTO's `now` parameter (PRD #1189 M1
// added it, so budget_used_seconds can be computed purely). Shared by the extend DTO tests
// below and by the pre-existing runToDTO unit tests that do not exercise budget_used_seconds.
var dtoTestNow = time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)

// TestRunToDTOExtendBudgetFields pins the four PRD #1189 M1 budget fields runToDTO serves
// on GET /api/runs/{id} for a running, timed run: the owner-granted extension, the effective
// admin cap, the total budget (frozen wall + extension) and the paused-aware used seconds.
//
// The run is a running issue with a frozen 8h (28800s) budget, a 2h (7200s) extension, no
// pause, and started_at exactly 1h before the fixed `now` — so used is 3600 and total is
// 28800 + 7200 = 36000. Every expected number is derived from the spec, not the mapping code.
func TestRunToDTOExtendBudgetFields(t *testing.T) {
	const (
		wallSeconds = 8 * 60 * 60  // 28800
		extSeconds  = 2 * 60 * 60  // 7200
		capSeconds  = 16 * 60 * 60 // 57600 (the default admin cap)
	)
	started := dtoTestNow.Add(-1 * time.Hour) // active for exactly 1h → used = 3600

	dto := runToDTO(store.Run{
		Kind:                   "issue",
		Status:                 "running",
		StartedAt:              pgtype.Timestamptz{Time: started, Valid: true},
		BudgetWallSeconds:      pgtype.Int4{Int32: wallSeconds, Valid: true},
		BudgetExtensionSeconds: extSeconds,
		BudgetPausedSeconds:    0,
	}, "normal", 2*time.Hour, capSeconds, dtoTestNow)

	if dto.BudgetExtensionSeconds != extSeconds {
		t.Errorf("budget_extension_seconds = %d, want %d", dto.BudgetExtensionSeconds, extSeconds)
	}
	if dto.BudgetExtensionCapSeconds != capSeconds {
		t.Errorf("budget_extension_cap_seconds = %d, want %d", dto.BudgetExtensionCapSeconds, capSeconds)
	}
	if dto.BudgetTotalSeconds == nil {
		t.Fatal("budget_total_seconds must be non-nil for a running timed run, got nil")
	}
	if *dto.BudgetTotalSeconds != wallSeconds+extSeconds {
		t.Errorf("budget_total_seconds = %d, want %d (wall %d + ext %d)", *dto.BudgetTotalSeconds, wallSeconds+extSeconds, wallSeconds, extSeconds)
	}
	if dto.BudgetUsedSeconds == nil {
		t.Fatal("budget_used_seconds must be non-nil when started_at is set, got nil")
	}
	if *dto.BudgetUsedSeconds != 3600 {
		t.Errorf("budget_used_seconds = %d, want 3600 (1h active, no pause)", *dto.BudgetUsedSeconds)
	}
}

// TestRunToDTOExtendBudgetTotalNilForChat pins that a run of a kind the sweep never times out (chat)
// carries budget_total_seconds null — the same "no wall deadline" predicate DeadlineAt uses —
// even though the extension/cap scalar fields are still populated unconditionally.
func TestRunToDTOExtendBudgetTotalNilForChat(t *testing.T) {
	dto := runToDTO(store.Run{
		Kind:              "chat",
		Status:            "running",
		StartedAt:         pgtype.Timestamptz{Time: dtoTestNow.Add(-1 * time.Hour), Valid: true},
		BudgetWallSeconds: pgtype.Int4{Int32: 8 * 60 * 60, Valid: true},
	}, "normal", 2*time.Hour, 57600, dtoTestNow)

	if dto.BudgetTotalSeconds != nil {
		t.Errorf("budget_total_seconds must be nil for a chat run (no wall deadline), got %d", *dto.BudgetTotalSeconds)
	}
	// The scalar extension/cap fields are still served (they are not gated on a wall deadline).
	if dto.BudgetExtensionCapSeconds != 57600 {
		t.Errorf("budget_extension_cap_seconds = %d, want 57600 even for a chat run", dto.BudgetExtensionCapSeconds)
	}
}

// TestRunToDTOExtendBudgetUsedNilWhenNotStarted pins that budget_used_seconds is null when the run
// never started (started_at invalid): there is no active-time origin to measure from.
func TestRunToDTOExtendBudgetUsedNilWhenNotStarted(t *testing.T) {
	dto := runToDTO(store.Run{
		Kind:   "issue",
		Status: "queued", // never started → StartedAt invalid
	}, "normal", 2*time.Hour, 57600, dtoTestNow)

	if dto.BudgetUsedSeconds != nil {
		t.Errorf("budget_used_seconds must be nil when started_at is invalid, got %d", *dto.BudgetUsedSeconds)
	}
}
