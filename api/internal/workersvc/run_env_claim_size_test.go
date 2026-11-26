package workersvc

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/runenv"
)

// pinnedRunEnvWorstCaseBytes bounds the serialized run_env and run_env_redact at the worst case
// the test below builds from the store's own limits. Every value and description byte is a
// control byte 0x01, which encoding/json writes as the six bytes \u0001, so wire bytes are six
// times payload bytes. A name is MaxNameBytes plain ASCII 'N' bytes, one wire byte each:
//
//	run_env: MaxVarsPerScope (50) entries holding MaxScopeValueBytes (262144) value bytes in all,
//	each value at most MaxValueBytes (65536): 1,572,864 wire bytes of value. Each entry also has
//	a MaxDescriptionBytes (1024) description (6144 wire bytes) and a MaxNameBytes name, plus a
//	var_id, scope, rev and the field names (about 200 bytes): about 0.4 MB of description and
//	structure for the 50 entries.
//	run_env_redact: MaxRetiredVersions+2*MaxVarsPerScope (300) entries holding
//	MaxRetiredValueBytes+2*MaxScopeValueBytes (2,621,440) value bytes in all: 15,728,640 wire
//	bytes of value, plus a MaxNameBytes name and about 150 bytes of structure per entry, about
//	0.1 MB.
//	Measured total: 17,691,164 bytes (about 16.9 MiB), against a quarter of
//	claim_response_max_bytes of 44,040,192. The pin is the measured figure rounded up by under
//	64 KiB to a multiple of 4 KiB (17,756,160).
//
// The pin is derived from the store limits asserted in the test; when one changes, recompute it.
const pinnedRunEnvWorstCaseBytes = 17756160

func TestRunEnvWorstCaseClaimStaysUnderQuarterOfClaimLimit(t *testing.T) {
	raw, err := os.ReadFile("../../../fixtures/claims/claim-response-limit.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx struct {
		Max int `json:"claim_response_max_bytes"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil || fx.Max != claimResponseMaxBytes {
		t.Fatalf("fixture limit %d (%v), constant %d", fx.Max, err, claimResponseMaxBytes)
	}
	limits, err := os.ReadFile("../../../fixtures/run-env/claim-limits.json")
	if err != nil {
		t.Fatal(err)
	}
	var lim struct {
		Entries       int `json:"max_run_env_entries"`
		ValueBytes    int `json:"max_value_bytes"`
		RedactEntries int `json:"max_redact_entries"`
		RedactBytes   int `json:"max_redact_total_bytes"`
	}
	if err := json.Unmarshal(limits, &lim); err != nil {
		t.Fatal(err)
	}

	// The fixture the clients read must agree with the store limits this worst case is built from.
	if lim.Entries != runenv.MaxVarsPerScope || lim.ValueBytes != runenv.MaxValueBytes ||
		lim.RedactEntries != runenv.MaxRetiredVersions+2*runenv.MaxVarsPerScope ||
		lim.RedactBytes != runenv.MaxRetiredValueBytes+2*runenv.MaxScopeValueBytes {
		t.Fatalf("fixture %+v disagrees with the store limits", lim)
	}
	// Each store limit the worst case derives from, at the literal the pin was computed with.
	for name, c := range map[string][2]int{
		"MaxVarsPerScope":      {runenv.MaxVarsPerScope, 50},
		"MaxScopeValueBytes":   {runenv.MaxScopeValueBytes, 262144},
		"MaxValueBytes":        {runenv.MaxValueBytes, 65536},
		"MaxDescriptionBytes":  {runenv.MaxDescriptionBytes, 1024},
		"MaxNameBytes":         {runenv.MaxNameBytes, 128},
		"MaxRetiredVersions":   {runenv.MaxRetiredVersions, 200},
		"MaxRetiredValueBytes": {runenv.MaxRetiredValueBytes, 2097152},
	} {
		if c[0] != c[1] {
			t.Fatalf("store limit changed: recompute pinnedRunEnvWorstCaseBytes (%s is %d, was %d)", name, c[0], c[1])
		}
	}

	control := func(n int) string { return strings.Repeat("\x01", n) }
	name := strings.Repeat("N", runenv.MaxNameBytes)
	// distribute splits total into n parts differing by at most one.
	distribute := func(total, n int) []int {
		parts := make([]int, n)
		for i := range parts {
			parts[i] = total / n
			if i < total%n {
				parts[i]++
			}
		}
		return parts
	}
	var env []ClaimRunEnvVar
	for _, n := range distribute(runenv.MaxScopeValueBytes, runenv.MaxVarsPerScope) {
		if n > runenv.MaxValueBytes {
			t.Fatalf("a value of %d bytes is over MaxValueBytes %d", n, runenv.MaxValueBytes)
		}
		env = append(env, ClaimRunEnvVar{
			VarID: "00000000-0000-4000-8000-000000000000", Scope: runenv.ScopeRepo, Rev: 2147483647, Name: name,
			Value: control(n), Secret: true, Description: control(runenv.MaxDescriptionBytes),
		})
	}
	var red []ClaimRunEnvRedact
	for _, n := range distribute(runenv.MaxRetiredValueBytes+2*runenv.MaxScopeValueBytes, runenv.MaxRetiredVersions+2*runenv.MaxVarsPerScope) {
		red = append(red, ClaimRunEnvRedact{
			VarID: "00000000-0000-4000-8000-000000000000", Scope: runenv.ScopeUser, Rev: 2147483647, Name: name, Value: control(n),
		})
	}
	secrets := ClaimSecrets{RunEnv: env, RunEnvRedact: red}
	empty, err := json.Marshal(ClaimSecrets{})
	if err != nil {
		t.Fatal(err)
	}
	full, err := json.Marshal(secrets)
	if err != nil {
		t.Fatal(err)
	}
	size := len(full) - len(empty) + len(`,"run_env":`) + len(`,"run_env_redact":`)
	// Every value byte really encodes as six: a control byte left unescaped would understate this.
	if !strings.Contains(string(full), `\u0001\u0001`) {
		t.Fatal("the worst case did not encode as \\u00XX")
	}
	if quarter := fx.Max / 4; size >= quarter {
		t.Fatalf("worst-case run_env + run_env_redact = %d bytes, not below a quarter (%d) of claim_response_max_bytes %d", size, quarter, fx.Max)
	}
	if size > pinnedRunEnvWorstCaseBytes || pinnedRunEnvWorstCaseBytes-size > 64*1024 {
		t.Fatalf("worst-case run_env + run_env_redact = %d bytes, outside the pin %d (the pin must be at or above the measure and within 64 KiB of it): recompute the arithmetic above and update the pin", size, pinnedRunEnvWorstCaseBytes)
	}
	t.Logf("worst-case run_env + run_env_redact = %d bytes", size)
}
