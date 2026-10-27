package handler

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestRunDTOReserveHoldReasonOnlyWhileQueued (PRD #3522 M3): the persisted weekly-reserve marker
// describes a QUEUED run. runToDTO surfaces it only then, so a stale marker on a run that has
// since been claimed, paused or finished never reads as a live hold, and the wire key is absent
// (omitempty) when there is nothing to say.
func TestRunDTOReserveHoldReasonOnlyWhileQueued(t *testing.T) {
	reason := "weekly usage reserve: 72% >= 70%"
	for _, tc := range []struct {
		status string
		marker pgtype.Text
		want   *string
	}{
		{"queued", pgtype.Text{String: reason, Valid: true}, &reason},
		{"queued", pgtype.Text{}, nil},
		{"running", pgtype.Text{String: reason, Valid: true}, nil},
		{"paused", pgtype.Text{String: reason, Valid: true}, nil},
		{"claimed", pgtype.Text{String: reason, Valid: true}, nil},
	} {
		dto := runToDTO(store.Run{ID: uuid.New(), Status: tc.status, ReserveHoldReason: tc.marker}, "normal", 0, 0, 0, dtoTestNow, 0)
		switch {
		case tc.want == nil && dto.ReserveHoldReason != nil:
			t.Errorf("%s with marker %v: reserve_hold_reason = %q, want absent", tc.status, tc.marker.Valid, *dto.ReserveHoldReason)
		case tc.want != nil && (dto.ReserveHoldReason == nil || *dto.ReserveHoldReason != *tc.want):
			t.Errorf("%s: reserve_hold_reason = %v, want %q", tc.status, dto.ReserveHoldReason, *tc.want)
		}
	}
	// The summary row and the list row carry it too (RunSummaryOf / ListItem are lossless copies).
	full := apitypes.RunListItemDTO{RunDTO: apitypes.RunDTO{ReserveHoldReason: &reason}}
	if got := apitypes.RunSummaryOf(full).ReserveHoldReason; got == nil || *got != reason {
		t.Errorf("summary dropped reserve_hold_reason: %v", got)
	}
	if got := apitypes.RunSummaryOf(full).ListItem().ReserveHoldReason; got == nil || *got != reason {
		t.Errorf("list item round trip dropped reserve_hold_reason: %v", got)
	}
}

// TestRunDTOReserveExemptUntil (PRD #3522 M4a): the manual-resume override's end rides the run DTO
// whatever the status, and the key is absent without one; the summary and list rows carry it too.
func TestRunDTOReserveExemptUntil(t *testing.T) {
	until := dtoTestNow.Add(36 * time.Hour)
	for _, status := range []string{"queued", "running"} {
		dto := runToDTO(store.Run{ID: uuid.New(), Status: status, ReserveExemptUntil: pgtype.Timestamptz{Time: until, Valid: true}}, "normal", 0, 0, 0, dtoTestNow, 0)
		if dto.ReserveExemptUntil == nil || !dto.ReserveExemptUntil.Equal(until) {
			t.Errorf("%s: reserve_exempt_until = %v, want %v", status, dto.ReserveExemptUntil, until)
		}
	}
	if dto := runToDTO(store.Run{ID: uuid.New(), Status: "queued"}, "normal", 0, 0, 0, dtoTestNow, 0); dto.ReserveExemptUntil != nil {
		t.Errorf("reserve_exempt_until = %v without an exemption, want absent", dto.ReserveExemptUntil)
	}
	// A reserve request reads as pause_mode 'reserve' and parks at the milestone boundary.
	dto := runToDTO(store.Run{ID: uuid.New(), Status: "running",
		PauseMode: pgtype.Text{String: "reserve", Valid: true}, PauseRequestedAt: pgtype.Timestamptz{Time: dtoTestNow, Valid: true}}, "normal", 0, 0, 0, dtoTestNow, 0)
	if dto.PauseMode == nil || *dto.PauseMode != "reserve" || !dto.PauseRequested {
		t.Errorf("reserve request DTO = (mode %v, requested %v), want (reserve, true)", dto.PauseMode, dto.PauseRequested)
	}
	full := apitypes.RunListItemDTO{RunDTO: apitypes.RunDTO{ReserveExemptUntil: &until}}
	if got := apitypes.RunSummaryOf(full).ReserveExemptUntil; got == nil || !got.Equal(until) {
		t.Errorf("summary dropped reserve_exempt_until: %v", got)
	}
	if got := apitypes.RunSummaryOf(full).ListItem().ReserveExemptUntil; got == nil || !got.Equal(until) {
		t.Errorf("list item round trip dropped reserve_exempt_until: %v", got)
	}
}
