package handler

import (
	"encoding/json"
	"testing"
)

// Issue #4257: the reconciled worker state ACK carries checkpoint_publication and
// registration_evidence when set, and omits both when empty.
func TestWorkerReconciledStateAckCarriesPublicationAndEvidence(t *testing.T) {
	raw, err := json.Marshal(workerReconciledStateAck{Run: map[string]any{}, CheckpointPublication: "recorded", RegistrationEvidence: "ok"})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got["checkpoint_publication"] != "recorded" || got["registration_evidence"] != "ok" {
		t.Fatalf("keys missing from %s", raw)
	}
	raw, _ = json.Marshal(workerReconciledStateAck{Run: map[string]any{}})
	got = nil
	_ = json.Unmarshal(raw, &got)
	if _, ok := got["checkpoint_publication"]; ok {
		t.Fatalf("empty checkpoint_publication not omitted: %s", raw)
	}
	if _, ok := got["registration_evidence"]; ok {
		t.Fatalf("empty registration_evidence not omitted: %s", raw)
	}
}
