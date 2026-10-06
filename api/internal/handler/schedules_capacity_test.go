package handler

import (
	"encoding/json"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/schedsvc"
	"github.com/vtmocanu/uzi/api/internal/store"
	"testing"
	"time"
)

func TestScheduleCapacityPatchValidation(t *testing.T) {
	cur := store.RunSchedule{Target: "sweep", Timing: "recurring", CronExpr: pgtype.Text{String: "0 * * * *", Valid: true}, Timezone: "UTC", CapacityLimit: pgtype.Int4{Int32: 4, Valid: true}, CapacityRoomNeeded: pgtype.Int4{Int32: 2, Valid: true}}
	for _, tc := range []struct {
		body        string
		want        int
		limit, room int
	}{
		{`{"cron_expr":"0 2 * * *"}`, 0, 4, 2},
		{`{"enabled":true,"capacity_limit":5}`, 0, 5, 2},
		{`{"capacity_room_needed":3}`, 0, 4, 3},
		{`{"capacity_limit":null,"capacity_room_needed":null}`, 0, 0, 0},
		{`{"capacity_limit":null}`, 400, 0, 0},
		{`{"capacity_limit":null,"capacity_room_needed":2}`, 400, 0, 0},
		{`{"capacity_limit":1}`, 400, 0, 0},
		{`{"capacity_limit":51}`, 400, 0, 0},
		{`{"capacity_room_needed":0}`, 400, 0, 0},
		{`{"target":"prompt","prompt":"x"}`, 400, 0, 0},
		{`{"timing":"once"}`, 400, 0, 0},
	} {
		t.Run(tc.body, func(t *testing.T) {
			var req apitypes.ScheduleRequest
			if err := json.Unmarshal([]byte(tc.body), &req); err != nil {
				t.Fatal(err)
			}
			m, status, _ := validateScheduleConfig(mergeSchedule(cur, req), time.Now(), false)
			if status != tc.want {
				t.Fatalf("status=%d want %d", status, tc.want)
			}
			if status == 0 {
				a, b := capacityColumn(m.CapacityLimit), capacityColumn(m.CapacityRoomNeeded)
				if int(a.Int32) != tc.limit || int(b.Int32) != tc.room {
					t.Fatalf("columns=%+v/%+v", a, b)
				}
			}
			if (req.CapacityLimit.Present || req.CapacityRoomNeeded.Present) && onlyEnabled(req) {
				t.Fatal("capacity edit was enabled-only")
			}
		})
	}
}

func TestRunNowCapacityWire(t *testing.T) {
	got := runNowResponse(schedsvc.FireOutcome{Capacity: &schedsvc.CapacityCheck{InFlight: 3, Limit: 4, RoomNeeded: 2, Room: 1, Blocked: true}})
	if got.Capacity == nil || *got.Capacity != (apitypes.CapacityCheck{InFlight: 3, Limit: 4, RoomNeeded: 2, Room: 1, Blocked: true}) {
		t.Fatalf("%+v", got)
	}
}
