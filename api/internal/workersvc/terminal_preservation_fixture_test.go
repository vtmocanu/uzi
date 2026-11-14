package workersvc

import (
	"encoding/json"
	"os"
	"slices"
	"testing"
)

// fixtures/terminal-preservation/completion-reasons.json is the list the worker decoder
// (agent/src/client.ts decodeCompletedPublicationReason) is pinned to.
func TestTerminalPreservationFixtureCompletionReasons(t *testing.T) {
	b, err := os.ReadFile("../../../fixtures/terminal-preservation/completion-reasons.json")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	want := []string{reasonTerminalPreservationPending, reasonTerminalPreservationRefused}
	if !slices.Equal(got, want) {
		t.Fatalf("completion-reasons.json = %v, want %v", got, want)
	}
}
