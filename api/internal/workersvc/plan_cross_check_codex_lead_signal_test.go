package workersvc

import (
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
