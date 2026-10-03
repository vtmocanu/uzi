package main

import (
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func TestRunGetPlanCrossCheckReason(t *testing.T) {
	for _, tc := range []struct {
		name, harness, status, want string
		required                    bool
	}{
		{"claude gate", "claude", "awaiting_approval", "plan cross-check: checker unavailable", true},
		{"codex gate", "codex", "awaiting_approval", "plan cross-check: not yet supported for a Codex lead", true},
		{"ordinary gate", "claude", "awaiting_approval", "", false},
		{"approved", "claude", "running", "", true},
		{"rejected", "codex", "failed", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fc := &uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{
				"r1": {ID: "r1", Kind: "issue", Harness: tc.harness, Status: tc.status, PlanCrossCheckRequired: tc.required},
			}}
			out, stderr, code := runCLI(t, fakeEnv(fc), "run", "get", "r1")
			if code != uzicli.ExitOK {
				t.Fatalf("run get exit %d: %s", code, stderr)
			}
			if tc.want != "" && !strings.Contains(out, tc.want) {
				t.Errorf("missing %q in:\n%s", tc.want, out)
			}
			if tc.want == "" && strings.Contains(out, "plan cross-check:") {
				t.Errorf("stale cross-check reason in:\n%s", out)
			}
		})
	}
}
