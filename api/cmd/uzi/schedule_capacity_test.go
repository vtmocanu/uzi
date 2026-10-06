package main

import (
	"bytes"
	"encoding/json"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
	"strings"
	"testing"
)

func TestScheduleCapacityCLI(t *testing.T) {
	for _, tc := range []struct {
		name           string
		flags          []string
		code           int
		present, clear bool
	}{
		{"set", []string{"--capacity-limit", "4", "--room-needed", "2"}, 0, true, false},
		{"clear", []string{"--clear-capacity"}, 0, true, true},
		{"preserve", []string{"--cron", "0 2 * * *"}, 0, false, false},
		{"half", []string{"--capacity-limit", "4"}, 2, false, false},
		{"room only", []string{"--room-needed", "2"}, 2, false, false},
		{"exclusive", []string{"--clear-capacity", "--capacity-limit", "4", "--room-needed", "2"}, 2, false, false},
		{"bounds", []string{"--capacity-limit", "1", "--room-needed", "2"}, 2, false, false},
	} {
		for _, origin := range []string{"user", "default"} {
			t.Run(tc.name+"/"+origin, func(t *testing.T) {
				c, k := 4, 2
				fc := &uzicli.FakeClient{ScheduleByID: map[string]apitypes.ScheduleDTO{"s": {ID: "s", Origin: origin, Target: "sweep", Timing: "recurring", CronExpr: "0 * * * *", CapacityLimit: &c, CapacityRoomNeeded: &k}}}
				args := append([]string{"schedule", "edit", "s"}, tc.flags...)
				_, errOut, code := runCLI(t, fakeEnv(fc), args...)
				if code != tc.code {
					t.Fatalf("exit=%d want=%d err=%s", code, tc.code, errOut)
				}
				if code == 0 {
					r := fc.LastPatchSchedReq
					if r.CapacityLimit.Present != tc.present || r.CapacityRoomNeeded.Present != tc.present {
						t.Fatalf("presence=%+v", r)
					}
					if tc.present && ((r.CapacityLimit.Value == nil) != tc.clear || (r.CapacityRoomNeeded.Value == nil) != tc.clear) {
						t.Fatal("clear/set lost")
					}
					if tc.present && !tc.clear && (*r.CapacityLimit.Value != 4 || *r.CapacityRoomNeeded.Value != 2) {
						t.Fatal("flag values lost")
					}
					raw, err := json.Marshal(r)
					if err != nil {
						t.Fatal(err)
					}
					var keys map[string]json.RawMessage
					if err := json.Unmarshal(raw, &keys); err != nil {
						t.Fatal(err)
					}
					for _, field := range []string{"capacity_limit", "capacity_room_needed"} {
						value, ok := keys[field]
						if ok != tc.present {
							t.Fatalf("wire presence lost: %s", raw)
						}
						if tc.clear && string(value) != "null" {
							t.Fatalf("clear not null: %s", raw)
						}
					}
				}
			})
		}
	}
	fc := &uzicli.FakeClient{}
	_, errOut, code := runCLI(t, fakeEnv(fc), "schedule", "create", "--repo", "r", "--sweep", "--cron", "0 * * * *", "--capacity-limit", "4", "--room-needed", "2")
	if code != 0 || fc.LastCreateSchedReq.CapacityLimit.Value == nil || *fc.LastCreateSchedReq.CapacityLimit.Value != 4 {
		t.Fatalf("create=%d %s", code, errOut)
	}
}

func TestScheduleCapacityRender(t *testing.T) {
	var buf bytes.Buffer
	p := uzicli.NewPrinter(&buf, false, false, true, false)
	c, k := 4, 2
	s := apitypes.ScheduleDTO{CapacityLimit: &c, CapacityRoomNeeded: &k, LastFire: &apitypes.LastFire{Capacity: &apitypes.CapacityCheck{InFlight: 3, Limit: 4, RoomNeeded: 2, Room: 1, Blocked: true}}}
	if err := renderScheduleDetail(p, s); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "WHEN TO SEND") || !strings.Contains(buf.String(), "limit 4 · room 2") {
		t.Fatal(buf.String())
	}
	if !strings.Contains(buf.String(), "Waiting for room: space for 1 more run; needs 2") {
		t.Fatal(buf.String())
	}
	buf.Reset()
	if err := renderScheduleDetail(p, apitypes.ScheduleDTO{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "WHEN TO SEND") || !strings.Contains(buf.String(), "off") || strings.Contains(buf.String(), "Waiting for room") {
		t.Fatal(buf.String())
	}
	buf.Reset()
	renderRunNow(p, "s", apitypes.RunNowResponse{Capacity: &apitypes.CapacityCheck{InFlight: 3, Limit: 4, RoomNeeded: 2, Room: 1, Blocked: true}})
	if buf.String() != "Waiting for room: space for 1 more run; needs 2\n" {
		t.Fatal(buf.String())
	}
	buf.Reset()
	renderRunNow(p, "s", apitypes.RunNowResponse{Capacity: &apitypes.CapacityCheck{Limit: 4, RoomNeeded: 3, Room: 2, Blocked: true}})
	if buf.String() != "Waiting for room: space for 2 more runs; needs 3\n" {
		t.Fatal(buf.String())
	}
}
