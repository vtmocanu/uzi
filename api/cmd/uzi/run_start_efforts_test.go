package main

import (
	"slices"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/agenttmpl"
)

// TestRunStartEffortsMatchServerSet keeps the CLI's import-free --effort list equal to the server's
// closed set, agenttmpl.EffortLevels.
func TestRunStartEffortsMatchServerSet(t *testing.T) {
	if !slices.Equal(runStartEfforts, agenttmpl.EffortLevels) {
		t.Fatalf("runStartEfforts = %v, want agenttmpl.EffortLevels %v", runStartEfforts, agenttmpl.EffortLevels)
	}
}
