package main

import (
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func TestPolicyHoldPreservationClaimsFollowEvidence(t *testing.T) {
	hold := holdProviderPolicyRefusal
	local, published := false, true
	unverified, verified := "unverified", "verified"
	for _, tc := range []struct {
		name            string
		policy          *apitypes.PolicyRefusalDTO
		want, forbidden string
	}{
		{"unknown", nil, "preservation unknown", "local-only checkpoint"},
		{"unknown-publication", &apitypes.PolicyRefusalDTO{}, "checkpoint publication unknown", "local-only checkpoint"},
		{"unverified", &apitypes.PolicyRefusalDTO{Provenance: &unverified, Published: &local}, "latest local work unverified", "local-only checkpoint"},
		{"verified-local", &apitypes.PolicyRefusalDTO{Provenance: &verified, Published: &local}, "local-only checkpoint", "checkpoint publication unknown"},
		{"published", &apitypes.PolicyRefusalDTO{Provenance: &verified, Published: &published}, "published checkpoint", "local-only checkpoint"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fc := &uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{
				"r1": {ID: "r1", Kind: "issue", Status: statusPaused, HoldReason: &hold, PolicyRefusal: tc.policy},
			}}
			out, stderr, code := runCLI(t, fakeEnv(fc), "run", "get", "r1")
			if code != uzicli.ExitOK {
				t.Fatalf("exit=%d: %s", code, stderr)
			}
			if !strings.Contains(out, tc.want) || strings.Contains(out, tc.forbidden) {
				t.Fatalf("want %q without %q in: %s", tc.want, tc.forbidden, out)
			}
		})
	}
}
