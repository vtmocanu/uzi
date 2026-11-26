package workersvc

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

// runEnvStrictDecode decodes raw into v rejecting unknown fields, so a fixture key the Go type does
// not carry fails here instead of being dropped silently.
func runEnvStrictDecode(t *testing.T, raw []byte, v any) {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		t.Fatal(err)
	}
}

// runEnvAssertSameJSON compares two documents semantically (key order and whitespace ignored).
func runEnvAssertSameJSON(t *testing.T, want, got []byte) {
	t.Helper()
	var w, g any
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(w, g) {
		t.Fatalf("re-marshalled JSON differs from the fixture:\n got:  %s\n want: %s", got, want)
	}
}

// The agent decodes the same fixture (agent/test/run-env-claim.test.ts): the Go types and the
// TS decoder agree on one wire.
func TestRunEnvClaimFixtureRoundTrips(t *testing.T) {
	raw, err := os.ReadFile("../../../fixtures/claims/run-env-claim.json")
	if err != nil {
		t.Fatal(err)
	}
	var secrets ClaimSecrets
	runEnvStrictDecode(t, raw, &secrets)
	if len(secrets.RunEnv) != 2 || len(secrets.RunEnvRedact) != 1 {
		t.Fatalf("decoded %d run_env and %d run_env_redact entries", len(secrets.RunEnv), len(secrets.RunEnvRedact))
	}
	out, err := json.Marshal(secrets)
	if err != nil {
		t.Fatal(err)
	}
	// ClaimSecrets also carries always-present credential keys; compare only the run-env keys.
	var m map[string]json.RawMessage
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	sub, err := json.Marshal(map[string]json.RawMessage{"run_env": m["run_env"], "run_env_redact": m["run_env_redact"]})
	if err != nil {
		t.Fatal(err)
	}
	runEnvAssertSameJSON(t, raw, sub)
}

func TestRunEnvRedactResponseFixtureRoundTrips(t *testing.T) {
	raw, err := os.ReadFile("../../../fixtures/claims/run-env-redact-response.json")
	if err != nil {
		t.Fatal(err)
	}
	var resp RunEnvRedactResponse
	runEnvStrictDecode(t, raw, &resp)
	if !resp.Protected || len(resp.Versions) != 2 {
		t.Fatalf("decoded %+v", resp)
	}
	out, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	runEnvAssertSameJSON(t, raw, out)
}

// omitempty is load-bearing: a claim with no variables keeps today's wire, with no run_env key.
func TestRunEnvClaimOmittedWhenEmpty(t *testing.T) {
	for _, s := range []ClaimSecrets{{}, {RunEnv: []ClaimRunEnvVar{}, RunEnvRedact: []ClaimRunEnvRedact{}}} {
		out, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(out, &m); err != nil {
			t.Fatal(err)
		}
		for _, k := range []string{"run_env", "run_env_redact"} {
			if _, ok := m[k]; ok {
				t.Errorf("empty ClaimSecrets emitted %q: %s", k, out)
			}
		}
	}
	out, err := json.Marshal(RunEnvRedactResponse{Protected: false})
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"protected":false}` {
		t.Errorf("unprotected response = %s", out)
	}
}
