package workersvc

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestPendingAllDecodeIndependentFields(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	for _, tc := range []struct {
		name, field string
		available   bool
		count       int
	}{
		{"missing", "", false, 0},
		{"null", `,"pending_all":null`, false, 0},
		{"empty", `,"pending_all":[]`, true, 0},
		{"zero", `,"pending_all":[{"run_id":"` + id + `","claim_generation":0}]`, true, 1},
		{"maxint", `,"pending_all":[{"run_id":"` + id + `","claim_generation":9223372036854775807}]`, true, 1},
		{"object", `,"pending_all":{}`, false, 0},
		{"string", `,"pending_all":"bad"`, false, 0},
		{"missing_id", `,"pending_all":[{"claim_generation":0}]`, false, 0},
		{"missing_generation", `,"pending_all":[{"run_id":"` + id + `"}]`, false, 0},
		{"null_id", `,"pending_all":[{"run_id":null,"claim_generation":0}]`, false, 0},
		{"null_generation", `,"pending_all":[{"run_id":"` + id + `","claim_generation":null}]`, false, 0},
		{"fraction", `,"pending_all":[{"run_id":"` + id + `","claim_generation":1.5}]`, false, 0},
		{"string_generation", `,"pending_all":[{"run_id":"` + id + `","claim_generation":"0"}]`, false, 0},
		{"overflow", `,"pending_all":[{"run_id":"` + id + `","claim_generation":9223372036854775808}]`, false, 0},
		{"null_entry", `,"pending_all":[null]`, false, 0},
	} {
		for _, finalize := range []string{"", `,"finalize_resume":null`, `,"finalize_resume":"bad"`, `,"finalize_resume":[{"run_id":"` + id + `","claim_generation":0}]`} {
			t.Run(tc.name+fmt.Sprint(len(finalize)), func(t *testing.T) {
				wire := `{"snapshot_epoch":1,"active":[{"run_id":"` + id + `","claim_generation":0,"phase":"running","terminal_pending":true}],"pending_overflow":true` + tc.field + finalize + `}`
				var snap ActiveSnapshot
				if err := json.Unmarshal([]byte(wire), &snap); err != nil {
					t.Fatal(err)
				}
				if (snap.PendingAll != nil) != tc.available || len(snap.PendingAll) != tc.count {
					t.Fatalf("pending_all = %#v, want available=%v count=%d", snap.PendingAll, tc.available, tc.count)
				}
				if tc.count == 1 {
					wantGeneration := int64(0)
					if tc.name == "maxint" {
						wantGeneration = 9223372036854775807
					}
					if snap.PendingAll[0].RunID != id || snap.PendingAll[0].ClaimGeneration != wantGeneration {
						t.Fatalf("decoded literal = %#v, want UUID %s generation %d", snap.PendingAll[0], id, wantGeneration)
					}
				}
				if len(snap.Active) != 1 || snap.Active[0].RunID != id || !snap.Active[0].TerminalPending || !snap.PendingOverflow {
					t.Fatalf("optional field damaged enclosing snapshot: %#v", snap)
				}
			})
		}
	}
}

func TestPendingAllDoesNotRelaxStrictActiveDecode(t *testing.T) {
	var snap ActiveSnapshot
	if err := json.Unmarshal([]byte(`{"active":"bad","pending_all":[]}`), &snap); err == nil {
		t.Fatal("strict active shape was accepted")
	}
}
