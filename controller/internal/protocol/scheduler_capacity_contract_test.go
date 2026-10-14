package protocol

import (
	"encoding/json"
	"testing"
)

// Older APIs decoded the known fields leniently. The optional cause must leave
// the legacy phase and fixed reason usable by that unchanged decoder.
func TestSchedulerCapacityLegacyAPIDecode(t *testing.T) {
	kind, reason := "scheduler_capacity", "scheduler capacity: insufficient memory"
	encoded, err := json.Marshal(WorkerStatus{ID: "11111111-1111-1111-1111-111111111111", Phase: PhaseStuck, BlockingKind: &kind, BlockingReason: &reason})
	if err != nil {
		t.Fatal(err)
	}
	var old struct {
		Phase          string  `json:"phase"`
		BlockingReason *string `json:"blocking_reason"`
	}
	if err := json.Unmarshal(encoded, &old); err != nil {
		t.Fatal(err)
	}
	if old.Phase != "stuck" || old.BlockingReason == nil || *old.BlockingReason != reason {
		t.Fatalf("legacy decode: %+v", old)
	}
	var wire map[string]any
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	if wire["blocking_kind"] != "scheduler_capacity" {
		t.Fatalf("capacity wire: %s", encoded)
	}
}
