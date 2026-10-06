package apitypes

import (
	"encoding/json"
	"testing"
)

func TestScheduleCapacityIntegerPresence(t *testing.T) {
	for _, field := range []string{"capacity_limit", "capacity_room_needed"} {
		for _, tc := range []struct {
			name, body string
			present    bool
			value      *int
		}{
			{"omitted", `{}`, false, nil},
			{"null", `{"` + field + `":null}`, true, nil},
			{"integer", `{"` + field + `":4}`, true, func() *int { v := 4; return &v }()},
		} {
			t.Run(field+"/"+tc.name, func(t *testing.T) {
				var req ScheduleRequest
				if err := json.Unmarshal([]byte(tc.body), &req); err != nil {
					t.Fatal(err)
				}
				got := req.CapacityLimit
				if field == "capacity_room_needed" {
					got = req.CapacityRoomNeeded
				}
				if got.Present != tc.present || (got.Value == nil) != (tc.value == nil) {
					t.Fatalf("presence=%+v", got)
				}
				if tc.value != nil && *got.Value != 4 {
					t.Fatal("integer changed")
				}
				b, err := json.Marshal(req)
				if err != nil {
					t.Fatal(err)
				}
				var keys map[string]json.RawMessage
				if err := json.Unmarshal(b, &keys); err != nil {
					t.Fatal(err)
				}
				encoded, present := keys[field]
				if present != tc.present {
					t.Fatalf("presence lost: %s", b)
				}
				if tc.present {
					want := "null"
					if tc.value != nil {
						want = "4"
					}
					if string(encoded) != want {
						t.Fatalf("encoding=%s want=%s", encoded, want)
					}
				}
			})
		}
		for _, value := range []string{`"4"`, `1.5`, `true`, `{}`, `[]`} {
			var req ScheduleRequest
			if err := json.Unmarshal([]byte(`{"`+field+`":`+value+`}`), &req); err == nil {
				t.Fatalf("accepted %s=%s", field, value)
			}
		}
	}
}
