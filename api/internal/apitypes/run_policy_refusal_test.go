package apitypes

import (
	"encoding/json"
	"testing"
)

func TestRunDTOProviderPolicyRefusalPassThrough(t *testing.T) {
	const wire = `{"id":"r1","status":"failed","fail_origin":"provider_policy_refusal","failure_reason":"Codex provider safety-policy refusal (cyberPolicy)"}`
	var run RunDTO
	if err := json.Unmarshal([]byte(wire), &run); err != nil {
		t.Fatal(err)
	}
	// The existing string field passes the new origin through.
	origin := run.FailOrigin
	if origin == nil || *origin != "provider_policy_refusal" {
		t.Fatalf("fail_origin = %v, want provider_policy_refusal", origin)
	}
	encoded, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	if string(fields["fail_origin"]) != `"provider_policy_refusal"` {
		t.Errorf("encoded fail_origin = %s", fields["fail_origin"])
	}
	if string(fields["failure_reason"]) != `"Codex provider safety-policy refusal (cyberPolicy)"` {
		t.Errorf("encoded failure_reason = %s", fields["failure_reason"])
	}
}

func TestMessageDTOProviderPolicyRefusalPassThrough(t *testing.T) {
	for _, phase := range []string{"planning", "implementation"} {
		for _, origin := range []string{"root", "child"} {
			t.Run(phase+"/"+origin, func(t *testing.T) {
				payload := `{"event":"provider_policy_refusal","provider":"codex","category":"policy_refusal","policy_tag":"cyberPolicy","phase":"` + phase + `","correlation_id":"pr-00000000000000000000000000000000-2","origin":"` + origin + `"`
				if origin == "child" {
					payload += `,"role":"reviewer","parent_correlation_id":"pr-00000000000000000000000000000000-1"`
				}
				payload += "}"
				wire := `{"seq":1,"kind":"status","payload":` + payload + "}"
				var message MessageDTO
				if err := json.Unmarshal([]byte(wire), &message); err != nil {
					t.Fatal(err)
				}
				// The payload stays raw JSON; this consumer does not validate worker metadata.
				raw := message.Payload
				if string(raw) != payload {
					t.Fatalf("decoded payload = %s, want %s", raw, payload)
				}
				encoded, err := json.Marshal(message)
				if err != nil {
					t.Fatal(err)
				}
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(encoded, &fields); err != nil {
					t.Fatal(err)
				}
				if string(fields["payload"]) != payload {
					t.Errorf("encoded payload = %s, want %s", fields["payload"], payload)
				}
			})
		}
	}
}
