package apitypes

import (
	"encoding/json"
	"testing"
)

func TestIssue2009PendingAggregateWire(t *testing.T) {
	for _, tc := range []struct {
		name, input     string
		count, overflow string
	}{
		{"legacy", "{}", "", ""},
		{"zero", `{"pending_outcome_count":0,"pending_overflow":false}`, "0", "false"},
		{"count only", `{"pending_outcome_count":0}`, "0", ""},
		{"overflow only", `{"pending_overflow":true}`, "", "true"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var worker WorkerDTO
			if err := json.Unmarshal([]byte(tc.input), &worker); err != nil {
				t.Fatal(err)
			}
			for _, dto := range []any{worker, AdminWorkerDTO{WorkerDTO: worker}} {
				raw, err := json.Marshal(dto)
				if err != nil {
					t.Fatal(err)
				}
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(raw, &fields); err != nil {
					t.Fatal(err)
				}
				if string(fields["pending_outcome_count"]) != tc.count || string(fields["pending_overflow"]) != tc.overflow {
					t.Fatalf("wire: %s", raw)
				}
			}
		})
	}
}
