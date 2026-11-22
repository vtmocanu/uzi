package main

import (
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// TestAllRunStatusesMatchAPITypes pins the CLI's `--until` vocabulary to the server-side
// apitypes.RunStatuses set, which the admin run listing's ?status= filter validates against.
func TestAllRunStatusesMatchAPITypes(t *testing.T) {
	if len(allRunStatuses) != len(apitypes.RunStatuses) {
		t.Fatalf("allRunStatuses has %d entries, apitypes.RunStatuses has %d", len(allRunStatuses), len(apitypes.RunStatuses))
	}
	for _, s := range apitypes.RunStatuses {
		if !allRunStatuses[s] {
			t.Errorf("apitypes.RunStatuses has %q, allRunStatuses does not", s)
		}
	}
	for s := range allRunStatuses {
		if !apitypes.IsRunStatus(s) {
			t.Errorf("allRunStatuses has %q, apitypes.IsRunStatus rejects it", s)
		}
	}
}
