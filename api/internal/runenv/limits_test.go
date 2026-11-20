package runenv

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// fixtures/run-env/limits.json is shared with the worker, whose decoder
// (agent/src/run-env-claim.ts) is pinned to the same file by test/run-env-claim.test.ts.
func TestLimitsEqualSharedFixture(t *testing.T) {
	raw, err := os.ReadFile("../../../fixtures/run-env/limits.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx struct {
		MaxVarsPerScope     int `json:"max_vars_per_scope"`
		MaxValueBytes       int `json:"max_value_bytes"`
		MaxScopeValueBytes  int `json:"max_scope_value_bytes"`
		MaxRetiredVersions  int `json:"max_retired_versions"`
		MaxRetiredBytes     int `json:"max_retired_bytes"`
		MinSecretLen        int `json:"min_secret_len"`
		MaxDescriptionBytes int `json:"max_description_bytes"`
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&fx); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		what      string
		got, want int
	}{
		{"max_vars_per_scope", fx.MaxVarsPerScope, MaxVarsPerScope},
		{"max_value_bytes", fx.MaxValueBytes, MaxValueBytes},
		{"max_scope_value_bytes", fx.MaxScopeValueBytes, MaxScopeValueBytes},
		{"max_retired_versions", fx.MaxRetiredVersions, MaxRetiredVersions},
		{"max_retired_bytes", fx.MaxRetiredBytes, MaxRetiredValueBytes},
		{"min_secret_len", fx.MinSecretLen, MinSecretLen},
		{"max_description_bytes", fx.MaxDescriptionBytes, MaxDescriptionBytes},
	} {
		if c.got != c.want {
			t.Errorf("fixtures/run-env/limits.json %s = %d, Go constant = %d", c.what, c.got, c.want)
		}
	}
}
