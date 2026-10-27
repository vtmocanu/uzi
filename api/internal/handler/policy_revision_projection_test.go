package handler

import (
	"encoding/json"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Both actual response representations must preserve all three wire states.
func TestPolicyRevisionGateStateAckEncoding(t *testing.T) {
	for _, raw := range []json.RawMessage{nil, json.RawMessage("null"), json.RawMessage(`{"revision":"1","presentation_id":"30680000-0000-0000-0000-000000000001","payload_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`)} {
		for _, proof := range []*workersvc.LeadReconciliation{nil, {}} {
			envelope := workerStateReportAck(map[string]any{"run": map[string]any{}}, workersvc.StateReportResult{
				PolicyRevisionGate: raw, Reconciliation: proof,
			})
			_, isReconciled := envelope.(workerReconciledStateAck)
			if isReconciled != (proof != nil) {
				t.Fatalf("wrong envelope %T", envelope)
			}
			data, err := json.Marshal(envelope)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(data, &fields); err != nil {
				t.Fatal(err)
			}
			got, present := fields["policy_revision_gate"]
			if present != (raw != nil) || string(got) != string(raw) {
				t.Fatalf("envelope %T field=%s present=%v want=%s", envelope, got, present, raw)
			}
		}
	}
}
