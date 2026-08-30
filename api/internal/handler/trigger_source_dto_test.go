package handler

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestRunToDTOTriggerSource pins issue #857 M3: the run's trigger_source VALUE flows
// through runToDTO into the API JSON. The exact-key wire test (apitypes/wire_test.go)
// already pins that the "trigger_source" KEY exists; this pins that whatever value the
// store row carries reaches the marshalled JSON verbatim.
//
// Asserting on the marshalled JSON (not just the DTO field) is deliberate: a future
// `json:"-"` or omitempty on the field would drop a non-empty value from the wire while
// leaving the struct field set, and only a marshal-and-read catches that. One case per
// distinct value shape (a threaded createRun value and a fixed-literal value) so a
// regression that hardcoded a single value in runToDTO is caught.
func TestRunToDTOTriggerSource(t *testing.T) {
	for _, want := range []string{"schedule", "ci_fix", "judge_rerun"} {
		t.Run(want, func(t *testing.T) {
			dto := runToDTO(store.Run{ID: uuid.New(), TriggerSource: want}, "normal")
			if dto.TriggerSource != want {
				t.Fatalf("dto.TriggerSource = %q, want %q", dto.TriggerSource, want)
			}
			b, err := json.Marshal(dto)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var got map[string]json.RawMessage
			if err := json.Unmarshal(b, &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			raw, ok := got["trigger_source"]
			if !ok {
				t.Fatalf("trigger_source absent from runs DTO JSON, want present")
			}
			var gotVal string
			if err := json.Unmarshal(raw, &gotVal); err != nil {
				t.Fatalf("decode trigger_source: %v", err)
			}
			if gotVal != want {
				t.Fatalf("JSON trigger_source = %q, want %q", gotVal, want)
			}
		})
	}
}
