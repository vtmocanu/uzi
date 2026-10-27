package handler

import (
	"encoding/json"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Approval presence is independent of gate coverage in both ACK encodings.
func TestPlanApprovedStateAckEncoding(t *testing.T) {
	for _, approval := range []*bool{nil, new(bool), func() *bool { v := true; return &v }()} {
		for _, gate := range []json.RawMessage{nil, json.RawMessage("null")} {
			for _, proof := range []*workersvc.LeadReconciliation{nil, {ClaimGeneration: 7}} {
				result := workersvc.StateReportResult{PlanApproved: approval, PolicyRevisionGate: gate, Reconciliation: proof}
				envelope := workerStateReportAck(map[string]any{"run": map[string]any{}, "credential_switch": "existing"}, result)
				data, err := json.Marshal(envelope)
				if err != nil {
					t.Fatal(err)
				}
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(data, &fields); err != nil {
					t.Fatal(err)
				}
				got, present := fields["plan_approved"]
				if present != (approval != nil) {
					t.Fatalf("approval=%v envelope=%T data=%s", approval, envelope, data)
				}
				if approval != nil {
					var value bool
					if err := json.Unmarshal(got, &value); err != nil || value != *approval {
						t.Fatalf("approval=%s err=%v", got, err)
					}
				}
				if string(fields["credential_switch"]) != `"existing"` {
					t.Fatalf("lost existing field: %s", data)
				}
				if proof != nil && string(fields["claim_generation"]) != "7" {
					t.Fatalf("lost reconciliation: %s", data)
				}
				if _, present := fields["policy_revision_gate"]; present != (gate != nil) {
					t.Fatalf("changed gate presence: %s", data)
				}
				internal, err := json.Marshal(result)
				if err != nil || string(internal) != "{}" {
					t.Fatalf("internal result leaked: %s %v", internal, err)
				}
			}
		}
	}
}

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
