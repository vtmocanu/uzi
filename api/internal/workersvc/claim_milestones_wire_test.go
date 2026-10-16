package workersvc

import (
	"encoding/json"
	"testing"
)

func TestClaimMilestonesWire(t *testing.T) {
	for _, tc := range []struct {
		name       string
		milestones []Milestone
		want       string
	}{
		{"absent", nil, ""},
		{"empty replacement", []Milestone{}, "[]"},
		{"replacement", []Milestone{{ID: "M1", Title: "New work"}}, `[{"id":"M1","title":"New work"}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, err := json.Marshal(ClaimPayload{Milestones: tc.milestones})
			if err != nil {
				t.Fatal(err)
			}
			var wire map[string]json.RawMessage
			if err := json.Unmarshal(body, &wire); err != nil {
				t.Fatal(err)
			}
			if got := string(wire["milestones"]); got != tc.want {
				t.Fatalf("claim milestones=%q, want %q", got, tc.want)
			}
		})
	}
}
