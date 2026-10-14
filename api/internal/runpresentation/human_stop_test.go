package runpresentation

import "testing"

func TestHumanStopPolicy(t *testing.T) {
	for _, tc := range []struct{ status, kind, reason, label string }{
		{"cancelled", "", "cancelled", "stopped (cancelled)"},
		{"cancelled", "auto_stopped", "cancelled", "stopped (cancelled)"},
		{"cancelled", "plan_rejected", "cancelled", "stopped (cancelled)"},
		{"cancelled", "unknown", "cancelled", "stopped (cancelled)"},
		{"failed", "cancelled", "cancelled", "stopped (cancelled)"},
		{"failed", "plan_rejected", "plan_rejected", "stopped (plan rejected)"},
		{"failed", "auto_stopped", "", ""},
		{"failed", "", "", ""},
		{"failed", "unknown", "", ""},
		{"running", "cancelled", "", ""},
		{"running", "plan_rejected", "", ""},
		{"completed", "cancelled", "", ""},
		{"completed", "plan_rejected", "", ""},
	} {
		t.Run(tc.status+"/"+tc.kind, func(t *testing.T) {
			if got := HumanStopReason(tc.status, tc.kind); got != tc.reason {
				t.Errorf("reason %q want %q", got, tc.reason)
			}
			if got := HumanStopLabel(tc.status, tc.kind); got != tc.label {
				t.Errorf("label %q want %q", got, tc.label)
			}
		})
	}
}
