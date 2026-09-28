package workersvc

import (
	"bytes"
	"encoding/json"
	"testing"
)

// TestStateRequestDecodesCheckpointContainsLatest (PRD #1809 M6, D8): the /state decode is strict
// (DisallowUnknownFields), so the wire key the worker sends after seeing run_checkpoint_durability
// must be a known field, decoded as a tri-state (absent stays nil).
func TestStateRequestDecodesCheckpointContainsLatest(t *testing.T) {
	decode := func(body string) StateRequest {
		t.Helper()
		dec := json.NewDecoder(bytes.NewReader([]byte(body)))
		dec.DisallowUnknownFields()
		var req StateRequest
		if err := dec.Decode(&req); err != nil {
			t.Fatalf("strict decode of %s: %v", body, err)
		}
		return req
	}
	if r := decode(`{"status":"paused","checkpoint_contains_latest":false}`); r.CheckpointContainsLatest == nil || *r.CheckpointContainsLatest {
		t.Fatalf("checkpoint_contains_latest=false decoded as %v", r.CheckpointContainsLatest)
	}
	if r := decode(`{"status":"limit_wait","checkpoint_contains_latest":true}`); r.CheckpointContainsLatest == nil || !*r.CheckpointContainsLatest {
		t.Fatalf("checkpoint_contains_latest=true decoded as %v", r.CheckpointContainsLatest)
	}
	if r := decode(`{"status":"recovery_wait"}`); r.CheckpointContainsLatest != nil {
		t.Fatalf("absent checkpoint_contains_latest decoded as %v, want nil", *r.CheckpointContainsLatest)
	}
}
