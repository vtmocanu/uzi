package runenv

import (
	"encoding/json"
	"os"
	"testing"
)

// The worker's claim decoder (agent/src/run-env-claim.ts) bounds what it accepts with limits it
// derives from these constants; fixtures/run-env/claim-limits.json is the one document both sides
// are pinned to, so a change to a store limit cannot leave the decoder rejecting a legal claim.
func TestClaimLimitsFixtureMatchesStoreLimits(t *testing.T) {
	raw, err := os.ReadFile("../../../fixtures/run-env/claim-limits.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx struct {
		MaxRunEnvEntries    int `json:"max_run_env_entries"`
		MaxValueBytes       int `json:"max_value_bytes"`
		MaxRunEnvTotalBytes int `json:"max_run_env_total_bytes"`
		MaxRedactEntries    int `json:"max_redact_entries"`
		MaxRedactTotalBytes int `json:"max_redact_total_bytes"`
		MinSecretLen        int `json:"min_secret_len"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	// run_env is repo scope only in M2. run_env_redact also carries disabled and pending-deletion
	// versions (counted against the per-scope limits, for the repo and the user scope) on top of
	// the retired-version budget.
	for _, c := range []struct {
		name      string
		got, want int
	}{
		{"max_run_env_entries", fx.MaxRunEnvEntries, MaxVarsPerScope},
		{"max_value_bytes", fx.MaxValueBytes, MaxValueBytes},
		{"max_run_env_total_bytes", fx.MaxRunEnvTotalBytes, MaxScopeValueBytes},
		{"max_redact_entries", fx.MaxRedactEntries, MaxRetiredVersions + 2*MaxVarsPerScope},
		{"max_redact_total_bytes", fx.MaxRedactTotalBytes, MaxRetiredValueBytes + 2*MaxScopeValueBytes},
		{"min_secret_len", fx.MinSecretLen, MinSecretLen},
	} {
		if c.got != c.want {
			t.Errorf("%s: fixture %d, store limits %d", c.name, c.got, c.want)
		}
	}
}
