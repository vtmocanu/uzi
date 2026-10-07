package handler

import (
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// mergeScheduleCapacity preserves omitted values but leaves a malformed explicit
// clear intact for validation rather than silently clearing the other half.
func mergeScheduleCapacity(cur store.RunSchedule, req apitypes.ScheduleRequest) (apitypes.OptionalInteger, apitypes.OptionalInteger) {
	a, b := req.CapacityLimit, req.CapacityRoomNeeded
	if (a.Present && a.Value == nil) || (b.Present && b.Value == nil) {
		return a, b
	}
	if !a.Present && cur.CapacityLimit.Valid {
		v := int(cur.CapacityLimit.Int32)
		a = apitypes.OptionalInteger{Present: true, Value: &v}
	}
	if !b.Present && cur.CapacityRoomNeeded.Valid {
		v := int(cur.CapacityRoomNeeded.Int32)
		b = apitypes.OptionalInteger{Present: true, Value: &v}
	}
	return a, b
}

func validateScheduleCapacity(req apitypes.ScheduleRequest, selector string) (int, string) {
	a, b := req.CapacityLimit, req.CapacityRoomNeeded
	if (a.Present && a.Value == nil) || (b.Present && b.Value == nil) {
		if !a.Present || !b.Present || a.Value != nil || b.Value != nil {
			return http.StatusBadRequest, "clear both capacity fields together"
		}
		return 0, ""
	}
	if a.Value == nil && b.Value == nil {
		return 0, ""
	}
	if a.Value == nil || b.Value == nil {
		return http.StatusBadRequest, "capacity_limit and capacity_room_needed must be set together"
	}
	if *b.Value < 1 || *a.Value < *b.Value || *a.Value > 50 {
		return http.StatusBadRequest, "capacity must satisfy 1 <= room_needed <= limit <= 50"
	}
	if req.Target != "sweep" || req.Timing != "recurring" || (selector != "" && selector != "label") {
		return http.StatusBadRequest, "capacity requires a recurring label-selected sweep"
	}
	return 0, ""
}

func capacityColumn(v apitypes.OptionalInteger) pgtype.Int4 {
	if v.Value == nil {
		return pgtype.Int4{}
	}
	return pgtype.Int4{Int32: int32(*v.Value), Valid: true} //nolint:gosec // validated capacity integers are in [1,50].
}
