package agenttmpl

import (
	"encoding/json"
	"os"
	"testing"
)

// The shared fixture is outside the Go module; run with -count=1 after edits.
func TestSelectLeadFixture(t *testing.T) {
	raw, err := os.ReadFile("../../../fixtures/lead-template-selection/cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			ID            string   `json:"id"`
			Names         []string `json:"names"`
			SelectedIndex *int     `json:"selected_index"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool)
	exercised := 0
	for _, c := range fixture.Cases {
		if c.ID == "" || seen[c.ID] {
			t.Fatalf("empty or duplicate fixture ID %q", c.ID)
		}
		seen[c.ID] = true
		t.Run(c.ID, func(t *testing.T) {
			index, found := SelectLead(c.Names)
			if found != (c.SelectedIndex != nil) {
				t.Fatalf("SelectLead(%q) = (%d, %v), expected selected_index %v", c.Names, index, found, c.SelectedIndex)
			}
			if c.SelectedIndex != nil && index != *c.SelectedIndex {
				t.Fatalf("SelectLead(%q) index = %d, want %d", c.Names, index, *c.SelectedIndex)
			}
			exercised++
		})
	}
	if exercised == 0 || exercised != len(fixture.Cases) {
		t.Fatalf("exercised %d cases, fixture has %d", exercised, len(fixture.Cases))
	}
}
