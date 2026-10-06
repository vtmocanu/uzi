package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func TestRunGetProviderPolicyRefusal(t *testing.T) {
	origin := "provider_policy_refusal"
	reason := "Codex provider safety-policy refusal (cyberPolicy)"
	fc := &uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{
		"r1": {ID: "r1", Kind: "issue", Status: "failed", FailOrigin: &origin, FailureReason: &reason},
	}}
	out, stderr, code := runCLI(t, fakeEnv(fc), "run", "get", "r1")
	if code != uzicli.ExitOK {
		t.Fatalf("run get exit = %d: %s", code, stderr)
	}
	if got := strings.Fields(lineWith(t, out, "FAIL_ORIGIN")); len(got) != 2 || got[1] != origin {
		t.Errorf("FAIL_ORIGIN row = %v, want [FAIL_ORIGIN %s]", got, origin)
	}
	if line := lineWith(t, out, "FAILURE_REASON"); !strings.Contains(line, reason) {
		t.Errorf("FAILURE_REASON row = %q, want %q", line, reason)
	}
	out, stderr, code = runCLI(t, fakeEnv(fc), "run", "get", "r1", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("run get --json exit = %d: %s", code, stderr)
	}
	var decoded apitypes.RunDTO
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.FailOrigin == nil || *decoded.FailOrigin != origin ||
		decoded.FailureReason == nil || *decoded.FailureReason != reason {
		t.Errorf("run get --json lost origin/reason: %s", out)
	}

	// Fake untrusted input exercises the existing terminal sanitizer at the command seam.
	unsafeOrigin := "provider_\u202Epolicy_refusal\u200B\x1b"
	run := fc.RunByID["r1"]
	run.FailOrigin = &unsafeOrigin
	fc.RunByID["r1"] = run
	out, stderr, code = runCLI(t, fakeEnv(fc), "run", "get", "r1")
	if code != uzicli.ExitOK {
		t.Fatalf("run get unsafe origin exit = %d: %s", code, stderr)
	}
	line := lineWith(t, out, "FAIL_ORIGIN")
	if got := strings.Fields(line); len(got) != 2 || got[1] != origin {
		t.Errorf("sanitized FAIL_ORIGIN row = %q, want %q", line, origin)
	}
	for _, r := range []rune{'\u202E', '\u200B', '\x1b'} {
		if strings.ContainsRune(line, r) {
			t.Errorf("FAIL_ORIGIN leaked control rune %U: %q", r, line)
		}
	}
}

func TestRunLogsProviderPolicyRefusalJSON(t *testing.T) {
	for _, phase := range []string{"planning", "implementation"} {
		t.Run(phase, func(t *testing.T) {
			// These worker-shaped IDs are fake opaque correlation values, not provider IDs.
			payload := json.RawMessage(`{"event":"provider_policy_refusal","provider":"codex","category":"policy_refusal","policy_tag":"misalignmentPolicyViolation","phase":"` + phase + `","correlation_id":"pr-00000000000000000000000000000000-2","origin":"child","role":"implementer","parent_correlation_id":"pr-00000000000000000000000000000000-1"}`)
			fc := &uzicli.FakeClient{LogsByID: map[string][]apitypes.MessageDTO{
				"r1": {{Seq: 1, Kind: "status", Payload: payload}},
			}}
			out, stderr, code := runCLI(t, fakeEnv(fc), "run", "logs", "r1", "--json")
			if code != uzicli.ExitOK {
				t.Fatalf("run logs --json exit = %d: %s", code, stderr)
			}
			var decoded apitypes.MessageDTO
			if err := json.Unmarshal([]byte(out), &decoded); err != nil {
				t.Fatal(err)
			}
			// Remove only printer whitespace; preserve the opaque payload's keys and values.
			var compact bytes.Buffer
			if err := json.Compact(&compact, decoded.Payload); err != nil {
				t.Fatal(err)
			}
			if compact.String() != string(payload) {
				t.Errorf("run logs changed policy metadata: got %s, want %s", compact.String(), payload)
			}
		})
	}
}
