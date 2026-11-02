package apitypes

import (
	"encoding/json"
	"os"
	"testing"
)

// API consumers share malformed wire examples with the projection cases.
// Typed flags/timestamps must reject malformed input rather than imply protection.
func TestOwnerClosureSharedMalformedWire(t *testing.T) {
	raw, err := os.ReadFile("../../../fixtures/recovery-prerequisite-closure/cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var table struct{ Malformed []struct{ Name, Wire string } }
	if err := json.Unmarshal(raw, &table); err != nil {
		t.Fatal(err)
	}
	if len(table.Malformed) != 3 {
		t.Fatal("malformed wire cases missing")
	}
	for _, tc := range table.Malformed {
		t.Run(tc.Name, func(t *testing.T) {
			var got RecoveryPrerequisiteClosureDTO
			if err := json.Unmarshal([]byte(tc.Wire), &got); err == nil {
				t.Fatalf("malformed wire accepted: %s", tc.Wire)
			}
		})
	}
}
