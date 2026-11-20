package workersvc

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

// The fixtures are shared with agent/test/run-env-claim.test.ts: both sides decode the
// same bytes, so a field renamed on one side fails the other's test.
func readRunEnvFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile("../../../fixtures/claims/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func runEnvStrictDecode(t *testing.T, raw []byte, into any) {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		t.Fatal(err)
	}
}

func runEnvAssertJSONEqual(t *testing.T, want, got []byte) {
	t.Helper()
	var w, g any
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(w, g) {
		t.Fatalf("round trip differs:\n want %s\n got  %s", want, got)
	}
}

func TestRunEnvClaimFixtureRoundTrips(t *testing.T) {
	raw := readRunEnvFixture(t, "run-env-claim.json")
	var s ClaimSecrets
	runEnvStrictDecode(t, raw, &s)
	if len(s.RunEnv) != 2 || len(s.RunEnvRedact) != 1 {
		t.Fatalf("decoded %d live and %d redact entries, want 2 and 1", len(s.RunEnv), len(s.RunEnvRedact))
	}
	if !s.RunEnv[1].Secret || s.RunEnv[0].Secret {
		t.Fatalf("secret flags wrong: %+v", s.RunEnv)
	}
	out, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	// ClaimSecrets has other always-present keys; compare only the run-env keys.
	var m map[string]json.RawMessage
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	sub := map[string]json.RawMessage{"run_env": m["run_env"], "run_env_redact": m["run_env_redact"]}
	subJSON, err := json.Marshal(sub)
	if err != nil {
		t.Fatal(err)
	}
	runEnvAssertJSONEqual(t, raw, subJSON)
}

func TestClaimSecretsWithoutRunEnvOmitsTheKeys(t *testing.T) {
	out, err := json.Marshal(ClaimSecrets{ForgePAT: "x"})
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"run_env", "run_env_redact"} {
		if strings.Contains(string(out), `"`+k+`"`) {
			t.Fatalf("claim without variables carries %q: %s", k, out)
		}
	}
}

func TestRunEnvRedactResponseFixtureRoundTrips(t *testing.T) {
	raw := readRunEnvFixture(t, "run-env-redact-response.json")
	var r RunEnvRedactResponse
	runEnvStrictDecode(t, raw, &r)
	if !r.Protected || len(r.Versions) != 2 {
		t.Fatalf("decoded %+v", r)
	}
	out, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	runEnvAssertJSONEqual(t, raw, out)

	out, err = json.Marshal(RunEnvRedactResponse{Protected: false})
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"protected":false}` {
		t.Fatalf("unprotected response = %s", out)
	}
}
