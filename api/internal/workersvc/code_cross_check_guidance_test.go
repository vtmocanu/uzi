package workersvc

import (
	"encoding/json"
	"testing"
)

// The public plan encoder must retain its legacy wire bytes with no code overlay.
func TestCodeCrossCheckFieldsAbsentFromPlanClaim(t *testing.T) {
	payload := boundedCheckerFixture(t)
	raw, err := MarshalCrossCheckClaim(payload)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		CrossCheck map[string]json.RawMessage `json:"cross_check"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.CrossCheck) == 0 {
		t.Fatal("missing plan claim")
	}
	for _, key := range []string{"guidance_digest", "repo_instructions_enabled", "repo_instructions_text", "repo_instructions_digest", "custom_guidance_applied"} {
		if _, present := body.CrossCheck[key]; present {
			t.Fatalf("code field %s present on plan", key)
		}
	}
}
