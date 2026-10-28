package main

import (
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

func TestRenderCapabilityProvenance(t *testing.T) {
	r := apitypes.RunDTO{ID: "r1", Status: "awaiting_approval", CapabilityProvenance: &apitypes.CapabilityProvenance{Capabilities: []apitypes.CapabilityProvenanceItem{{Capability: "jvm", Sources: []string{"repo_hint", "inferred"}, Required: true, InferenceIgnored: true, Triggers: []apitypes.CapabilityTrigger{{Path: "app/\u202eandroid/build.gradle\x1b", Reason: "jvm_manifest"}}, Truncated: true}}}}
	out := renderDetail(t, r)
	for _, want := range []string{"CAPABILITY_PROVENANCE", "repo-hint history unknown", "inference evidence unknown", "CAPABILITY_SOURCE", "jvm: repo_hint+inferred; required; inference ignored", "CAPABILITY_TRIGGER", "app/android/build.gradle", "trigger paths omitted"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in %s", want, out)
		}
	}
	if strings.ContainsAny(out, "\x1b\u202e") {
		t.Fatalf("unsafe output: %q", out)
	}
	r.CapabilityProvenance.Capabilities[0].Required = false
	if out := renderDetail(t, r); !strings.Contains(out, "not currently required") {
		t.Fatal(out)
	}
}
