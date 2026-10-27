package workersvc

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestNormalizeCapabilityEvidence(t *testing.T) {
	for _, raw := range []string{"null", `[]`, `"bad"`, `{"jvm":null}`, `{"jvm":42}`, `{"unknown":{}}`, strings.Repeat(" ", 32*1024+1)} {
		if got := NormalizeCapabilityEvidence([]byte(raw)); got != nil {
			t.Fatalf("invalid root updated evidence: %q", got)
		}
	}
	if got := string(NormalizeCapabilityEvidence([]byte(`{}`))); got != "{}" {
		t.Fatalf("empty scan=%q", got)
	}
	raw := `{"unknown":{"triggers":[]},"jvm":{"triggers":[{"path":"app/android/build.gradle","reason":"jvm_manifest"},{"path":"app/android/build.gradle","reason":"jvm_manifest"},{"path":"/outside/pom.xml","reason":"jvm_manifest"},{"path":"../pom.xml","reason":"jvm_manifest"},{"path":"a/../pom.xml","reason":"jvm_manifest"},{"path":"pom.xml","reason":"docker_command"},{"path":"a/pom.xml","reason":"jvm_manifest"},{"path":"b/pom.xml","reason":"jvm_manifest"},{"path":"c/pom.xml","reason":"jvm_manifest"}]}}`
	var got map[string]apitypes.CapabilityInferenceObservation
	if err := json.Unmarshal(NormalizeCapabilityEvidence([]byte(raw)), &got); err != nil {
		t.Fatal(err)
	}
	want := []apitypes.CapabilityTrigger{{Path: "app/android/build.gradle", Reason: "jvm_manifest"}, {Path: "a/pom.xml", Reason: "jvm_manifest"}, {Path: "b/pom.xml", Reason: "jvm_manifest"}}
	if len(got) != 1 || !reflect.DeepEqual(got["jvm"].Triggers, want) || !got["jvm"].Truncated {
		t.Fatalf("normalization=%+v", got)
	}
	for _, p := range []string{"..", "bad\x1b/pom.xml", "bad\u202e/pom.xml", strings.Repeat("é", 257) + "/pom.xml", `a\pom.xml`} {
		if validCapabilityTrigger("jvm", apitypes.CapabilityTrigger{Path: p, Reason: "jvm_manifest"}) {
			t.Fatalf("unsafe path accepted: %q", p)
		}
	}
}

func TestCapabilityEvidenceNeedsPlanFence(t *testing.T) {
	plan := "plan"
	for _, req := range []StateRequest{
		{State: "awaiting_approval", CapabilityInferenceEvidence: json.RawMessage(`{}`)},
		{State: "running", PlanMd: &plan, ClaimGeneration: i64(0), CapabilityInferenceEvidence: json.RawMessage(`{}`)},
		{State: "running", ClaimGeneration: i64(1), CapabilityInferenceEvidence: json.RawMessage(`{}`)},
		{State: "completed", ClaimGeneration: i64(1), CapabilityInferenceEvidence: json.RawMessage(`{}`)},
	} {
		if got := capabilityEvidenceParam(req); got != nil {
			t.Fatalf("unfenced/non-plan evidence accepted: %+v", req)
		}
	}
	if got := capabilityEvidenceParam(StateRequest{State: "running", PlanMd: &plan, ClaimGeneration: i64(1), CapabilityInferenceEvidence: json.RawMessage(`{}`)}); string(got) != "{}" {
		t.Fatalf("fenced evidence omitted: %s", got)
	}
}

func TestCapabilityProvenanceSources(t *testing.T) {
	obs := []byte(`{"jvm":{"triggers":[{"path":"app/android/build.gradle","reason":"jvm_manifest"}],"truncated":false}}`)
	run := store.Run{HintedCapabilities: []string{"jvm"}, RequiredCapabilities: []string{"jvm"}, IgnoredInferredCapabilities: []string{"jvm"}, CapabilityInferenceEvidence: obs}
	p := CapabilityProvenanceForRun(run)
	if !p.HintsKnown || !p.InferenceKnown || len(p.Capabilities) != 1 {
		t.Fatalf("provenance=%+v", p)
	}
	item := p.Capabilities[0]
	if !reflect.DeepEqual(item.Sources, []string{"repo_hint", "inferred"}) || !item.Required || !item.InferenceIgnored || item.Triggers[0].Path != "app/android/build.gradle" {
		t.Fatalf("overlap=%+v", item)
	}
	run.RequiredCapabilities = nil
	if CapabilityProvenanceForRun(run).Capabilities[0].Required {
		t.Fatal("override was presented as still required")
	}
	legacy := CapabilityProvenanceForRun(store.Run{RequiredCapabilities: []string{"docker"}})
	if legacy.HintsKnown || legacy.InferenceKnown || !reflect.DeepEqual(legacy.Capabilities[0].Sources, []string{"unknown"}) {
		t.Fatalf("legacy provenance guessed: %+v", legacy)
	}
	known := CapabilityProvenanceForRun(store.Run{HintedCapabilities: []string{}, CapabilityInferenceEvidence: []byte(`{}`)})
	if !known.HintsKnown || !known.InferenceKnown || len(known.Capabilities) != 0 {
		t.Fatalf("known empty lost: %+v", known)
	}
}
