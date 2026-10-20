package apitypes

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// These worker-only goldens are also read by agent/test/interrupted-calls-contract.test.ts.
// Record fixtures from the failing test's actual marshal output; there is no update mode.
// Run with -count=1: fixtures live outside the Go module's test-cache boundary.
func TestInterruptedCallsWireContract(t *testing.T) {
	source := int64(2)
	instance, label := "worker-display", "Implementation"
	empty := InterruptedCallsResponse{
		Format: 1, RunID: "22222222-2222-4222-8222-222222222222",
		WorkerID: "11111111-1111-4111-8111-111111111111", RegisterNonce: "registration-nonce",
		ClaimGeneration: 3, HistoryIncomplete: true, Entries: []InterruptedCall{},
	}
	full := empty
	full.SourceGeneration = &source
	full.InspectedCount = 4
	full.Truncated = true
	full.Entries = []InterruptedCall{
		{Seq: 10, SourceGeneration: 2, Agent: "lead", AgentInstance: &instance,
			ToolName: "Bash", Command: "git status", DisplayOmitted: false},
		{Seq: 8, SourceGeneration: 2, Agent: "lead", AgentLabel: &label,
			ToolName: "Bash", Command: "git diff --stat", DisplayOmitted: true},
	}
	for _, tc := range []struct {
		name string
		dto  InterruptedCallsResponse
	}{
		{"response.empty.json", empty},
		{"response.full.json", full},
	} {
		t.Run(tc.name, func(t *testing.T) {
			actual, err := json.MarshalIndent(tc.dto, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			actual = append(actual, '\n')
			path := filepath.Join("../../../fixtures/interrupted-calls", tc.name)
			recorded, err := os.ReadFile(path) //nolint:gosec // Fixed test fixture directory and case names.
			if err != nil || !bytes.Equal(recorded, actual) {
				t.Fatalf("fixture %s missing or mismatched (read error: %v); record this exact actual marshal JSON:\n%s", tc.name, err, actual)
			}
			decoder := json.NewDecoder(bytes.NewReader(recorded))
			decoder.DisallowUnknownFields()
			var roundtrip InterruptedCallsResponse
			if err := decoder.Decode(&roundtrip); err != nil {
				t.Fatal(err)
			}
			if err := decoder.Decode(new(any)); err != io.EOF {
				t.Fatalf("expected exactly one JSON value, got %v", err)
			}
			if !reflect.DeepEqual(roundtrip, tc.dto) {
				t.Fatalf("strict roundtrip changed DTO: got %+v, want %+v", roundtrip, tc.dto)
			}
		})
	}
}
