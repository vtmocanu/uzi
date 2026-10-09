package workersvc

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestPlanCrossCheckCodexLeadSupported pins when the claim carries the additive
// plan_cross_check_codex_lead api-capability signal.
func TestPlanCrossCheckCodexLeadSupported(t *testing.T) {
	cases := []struct {
		name     string
		harness  string
		required bool
		want     bool
	}{
		{"codex lead requiring the cross-check", harnessCodex, true, true},
		{"claude lead requiring the cross-check", "claude", true, false},
		{"codex lead without the requirement", harnessCodex, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := planCrossCheckCodexLeadSupported(store.Run{Harness: tc.harness, PlanCrossCheckRequired: tc.required})
			if got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// TestPlanCrossCheckCodexLeadWireKey pins the JSON key a worker reads the capability from, and that a
// false value omits the key entirely (older workers and Claude claims see no new field).
func TestPlanCrossCheckCodexLeadWireKey(t *testing.T) {
	on, err := json.Marshal(ClaimPayload{PlanCrossCheckCodexLead: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(on), `"plan_cross_check_codex_lead":true`) {
		t.Fatalf("true must serialize the wire key, got %s", on)
	}
	off, err := json.Marshal(ClaimPayload{PlanCrossCheckCodexLead: false})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(off), "plan_cross_check_codex_lead") {
		t.Fatalf("false must omit the wire key, got %s", off)
	}
}
