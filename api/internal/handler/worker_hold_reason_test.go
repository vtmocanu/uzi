package handler

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// A paused run parked by the weekly usage reserve (PRD #3522) is an ordinary pause to a worker: the
// agent keys its recovery and refusal paths on a non-null hold, so every worker-facing run body
// (workerRunDTO) carries a null hold for weekly_reserve while every other hold passes through. The
// owner-facing runToDTO keeps the real value.
func TestWorkerRunDTOHoldReason(t *testing.T) {
	h := &Handler{}
	for _, tc := range []struct {
		hold string
		want string // "" = null
	}{
		{"weekly_reserve", ""},
		{"budget_exhausted", "budget_exhausted"},
		{"completion_blocked", "completion_blocked"},
		{"provider_unavailable", "provider_unavailable"},
		{"credential_switch_settling", "credential_switch_settling"},
		{"", ""},
	} {
		run := store.Run{ID: uuid.New(), Status: "paused"}
		if tc.hold != "" {
			run.HoldReason = pgtype.Text{String: tc.hold, Valid: true}
		}
		got := h.workerRunDTO(context.Background(), run).HoldReason
		switch {
		case tc.want == "" && got != nil:
			t.Errorf("worker hold %q = %q, want null", tc.hold, *got)
		case tc.want != "" && (got == nil || *got != tc.want):
			t.Errorf("worker hold %q = %v, want %q", tc.hold, got, tc.want)
		}
		owner := runToDTO(run, "normal", 0, 0, 0, dtoTestNow, 0).HoldReason
		if tc.hold != "" && (owner == nil || *owner != tc.hold) {
			t.Errorf("owner hold %q = %v, want it unchanged", tc.hold, owner)
		}
	}
}

// A pending weekly-reserve pause request (pause_mode 'reserve') is served to a worker as an ordinary
// 'milestone' request in every worker-facing run body; the owner-facing DTO keeps 'reserve'.
func TestWorkerRunDTOPauseMode(t *testing.T) {
	h := &Handler{}
	for _, tc := range []struct{ mode, want string }{
		{"reserve", "milestone"},
		{"milestone", "milestone"},
		{"now", "now"},
		{"wall", "wall"},
	} {
		run := store.Run{ID: uuid.New(), Status: "running",
			PauseRequestedAt: pgtype.Timestamptz{Time: dtoTestNow, Valid: true},
			PauseMode:        pgtype.Text{String: tc.mode, Valid: true}}
		got := h.workerRunDTO(context.Background(), run).PauseMode
		if got == nil || *got != tc.want {
			t.Errorf("worker pause_mode %q = %v, want %q", tc.mode, got, tc.want)
		}
		owner := runToDTO(run, "normal", 0, 0, 0, dtoTestNow, 0).PauseMode
		if owner == nil || *owner != tc.mode {
			t.Errorf("owner pause_mode %q = %v, want it unchanged", tc.mode, owner)
		}
	}
}
