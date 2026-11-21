package workersvc

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/runenv"
)

// pinnedRunEnvWorstCaseBytes bounds the serialized run_env and run_env_redact at the worst case
// the stricter test below builds. The arithmetic, for the DTOs as encoded by encoding/json
// (every control byte 0x01 encodes as the six bytes \u0001):
//
//	run_env: 50 entries, each a 65536-byte value (393216 wire bytes), a 1024-byte description
//	(6144), a 128-byte name (128), a 36-byte var_id, scope, rev and field names ~ 120 bytes of
//	structure: about 400 KB per entry, 19.9 MB for 50.
//	run_env_redact: 300 entries and 2621440 value bytes in all (15728640 wire bytes), a 128-byte
//	name and ~ 110 bytes of structure each: about 15.8 MB.
//	Measured total: 35,779,100 bytes (about 34.1 MiB), against a quarter of
//	claim_response_max_bytes of 44,040,192. The pin is that figure plus a margin of under 2 MiB
//	(36 MiB = 37,748,736), well inside the 16 MiB ceiling for the margin. The real per-scope
//	bound (262144 value bytes in all) is far smaller; the stricter 50 x 65536 shape is asserted
//	so a store limit raised later still has to fit the budget.
const pinnedRunEnvWorstCaseBytes = 36 << 20

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

	control := func(n int) string { return strings.Repeat("\x01", n) }
	name := strings.Repeat("N", runenv.MaxNameBytes)
	var env []ClaimRunEnvVar
	for i := 0; i < lim.Entries; i++ {
		env = append(env, ClaimRunEnvVar{
			VarID: "00000000-0000-4000-8000-000000000000", Scope: runenv.ScopeRepo, Rev: 2147483647, Name: name,
			Value: control(lim.ValueBytes), Secret: true, Description: control(runenv.MaxDescriptionBytes),
		})
	}
	var red []ClaimRunEnvRedact
	per := lim.RedactBytes / lim.RedactEntries
	rest := lim.RedactBytes
	for i := 0; i < lim.RedactEntries; i++ {
		n := per
		if i == lim.RedactEntries-1 {
			n = rest
		}
		rest -= n
		red = append(red, ClaimRunEnvRedact{
			VarID: "00000000-0000-4000-8000-000000000000", Scope: runenv.ScopeUser, Rev: 2147483647, Name: name, Value: control(n),
		})
	}
	if rest != 0 {
		t.Fatalf("redact bytes not fully distributed: %d left", rest)
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
	if size > pinnedRunEnvWorstCaseBytes {
		t.Fatalf("worst-case run_env + run_env_redact = %d bytes, over the pinned %d: recompute the arithmetic above and update the pin", size, pinnedRunEnvWorstCaseBytes)
	}
	t.Logf("worst-case run_env + run_env_redact = %d bytes", size)
}
