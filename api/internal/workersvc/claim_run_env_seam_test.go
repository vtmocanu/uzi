package workersvc

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// The wire shape of ClaimSecrets.run_env / run_env_redact and of the maintenance response is
// pinned by two shared fixtures, which the worker decodes in agent/test/run-env-claim.test.ts.

const (
	runEnvClaimFixture  = "../../../fixtures/claims/run-env-claim.json"
	runEnvRedactFixture = "../../../fixtures/claims/run-env-redact-response.json"
)

func readSeamFixture(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // G304: path is one of the two constant fixture paths above
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// strictDecode fails on any field the Go types do not know, so a fixture field added without a
// Go field (or the reverse) cannot go unnoticed.
func seamStrictDecode(t *testing.T, raw []byte, into any) {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		t.Fatal(err)
	}
}

func seamRequireJSONEqual(t *testing.T, got, want []byte) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatal(err)
	}
	gb, _ := json.Marshal(g)
	wb, _ := json.Marshal(w)
	if string(gb) != string(wb) {
		t.Fatalf("re-marshalled JSON differs from the fixture:\n got:  %s\n want: %s", gb, wb)
	}
}

func TestRunEnvClaimFixtureRoundTrips(t *testing.T) {
	raw := readSeamFixture(t, runEnvClaimFixture)
	var secrets ClaimSecrets
	seamStrictDecode(t, raw, &secrets)

	if len(secrets.RunEnv) != 2 || len(secrets.RunEnvRedact) != 2 {
		t.Fatalf("decoded %d run_env and %d run_env_redact entries, want 2 and 2", len(secrets.RunEnv), len(secrets.RunEnvRedact))
	}
	var plain, secret int
	for _, v := range secrets.RunEnv {
		if v.VarID == uuid.Nil || v.Rev < 1 || v.Scope != "repo" || v.Name == "" || v.Value == "" || v.Description == "" {
			t.Errorf("run_env entry not fully decoded: %+v", v)
		}
		if v.Secret {
			secret++
		} else {
			plain++
		}
	}
	if plain != 1 || secret != 1 {
		t.Errorf("fixture holds %d plain and %d secret entries, want one of each", plain, secret)
	}
	for _, v := range secrets.RunEnvRedact {
		if v.VarID == uuid.Nil || v.Rev < 1 || v.Scope == "" || v.Name == "" || v.Value == "" {
			t.Errorf("run_env_redact entry not fully decoded: %+v", v)
		}
	}

	out, err := json.Marshal(secrets)
	if err != nil {
		t.Fatal(err)
	}
	// The fixture is an example `secrets` fragment: the always-present string fields of
	// ClaimSecrets are not part of it, so compare only the two run-env keys.
	var got, want map[string]json.RawMessage
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"run_env", "run_env_redact"} {
		seamRequireJSONEqual(t, got[k], want[k])
	}
}

func TestRunEnvFieldsOmittedWhenEmpty(t *testing.T) {
	out, err := json.Marshal(ClaimSecrets{ForgePAT: "x"})
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"run_env", "run_env_redact"} {
		if strings.Contains(string(out), `"`+k+`"`) {
			t.Errorf("empty %s must be omitted so a claim without variables stays byte-identical: %s", k, out)
		}
	}
}

func TestRunEnvRedactResponseFixtureRoundTrips(t *testing.T) {
	raw := readSeamFixture(t, runEnvRedactFixture)
	var resp apitypes.RunEnvRedactResponse
	seamStrictDecode(t, raw, &resp)
	if !resp.Protected || len(resp.Versions) != 3 {
		t.Fatalf("decoded protected=%v with %d versions, want true and 3", resp.Protected, len(resp.Versions))
	}
	for _, v := range resp.Versions {
		if v.VarID == uuid.Nil || v.Rev < 1 || v.Scope == "" || v.Name == "" || v.Value == "" {
			t.Errorf("version not fully decoded: %+v", v)
		}
	}
	out, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	seamRequireJSONEqual(t, out, raw)
}
