package workersvc

import (
	"encoding/json"
	"path"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

const CapabilityEvidenceFeature = "capability_inference_evidence_v1"

func capabilityEvidenceParam(req StateRequest) []byte {
	if req.ClaimGeneration == nil || *req.ClaimGeneration < 1 ||
		(req.State != "awaiting_approval" && (req.State != "running" || req.PlanMd == nil)) {
		return nil
	}
	return NormalizeCapabilityEvidence(req.CapabilityInferenceEvidence)
}

// NormalizeCapabilityEvidence bounds advisory metadata independently of admission.
// Invalid roots preserve the prior observation; invalid entries never alter requirements.
func NormalizeCapabilityEvidence(raw json.RawMessage) []byte {
	if len(raw) == 0 || len(raw) > 32*1024 || !utf8.Valid(raw) || string(raw) == "null" {
		return nil
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return nil
	}
	out := map[string]apitypes.CapabilityInferenceObservation{}
	for _, cap := range capability.Filter([]string{"docker", "jvm"}) {
		value, present := fields[cap]
		if !present {
			continue
		}
		var entry struct {
			Triggers  []json.RawMessage `json:"triggers"`
			Truncated bool              `json:"truncated"`
		}
		if json.Unmarshal(value, &entry) != nil || string(value) == "null" {
			continue
		}
		obs := apitypes.CapabilityInferenceObservation{Triggers: []apitypes.CapabilityTrigger{}, Truncated: entry.Truncated}
		for _, item := range entry.Triggers {
			var trigger apitypes.CapabilityTrigger
			if json.Unmarshal(item, &trigger) != nil || !validCapabilityTrigger(cap, trigger) {
				obs.Truncated = true
				continue
			}
			if slices.Contains(obs.Triggers, trigger) {
				continue
			}
			if len(obs.Triggers) >= 3 {
				obs.Truncated = true
				continue
			}
			obs.Triggers = append(obs.Triggers, trigger)
		}
		out[cap] = obs
	}
	if len(fields) > 0 && len(out) == 0 {
		return nil // Malformed/unknown observations are not a successful empty scan.
	}
	encoded, _ := json.Marshal(out)
	return encoded
}

func validCapabilityTrigger(cap string, trigger apitypes.CapabilityTrigger) bool {
	p := trigger.Path
	if p == "" || len(p) > 512 || !utf8.ValidString(p) || path.IsAbs(p) || p == "." || p == ".." || path.Clean(p) != p || strings.HasPrefix(p, "../") || strings.Contains(p, "\\") {
		return false
	}
	for _, r := range p {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf) {
			return false
		}
	}
	if cap == "jvm" {
		return trigger.Reason == "jvm_manifest"
	}
	return slices.Contains([]string{"docker_marker", "testcontainers_reference", "docker_command"}, trigger.Reason)
}

// CapabilityProvenanceForRun never supplies scheduling or approval inputs.
func CapabilityProvenanceForRun(run store.Run) *apitypes.CapabilityProvenance {
	var inferred map[string]apitypes.CapabilityInferenceObservation
	if len(run.CapabilityInferenceEvidence) > 0 {
		_ = json.Unmarshal(NormalizeCapabilityEvidence(run.CapabilityInferenceEvidence), &inferred)
	}
	out := &apitypes.CapabilityProvenance{HintsKnown: run.HintedCapabilities != nil, InferenceKnown: inferred != nil, Capabilities: []apitypes.CapabilityProvenanceItem{}}
	caps := slices.Clone(run.RequiredCapabilities)
	caps = append(caps, run.HintedCapabilities...)
	for cap := range inferred {
		caps = append(caps, cap)
	}
	for _, cap := range capability.Filter(caps) {
		obs, observed := inferred[cap]
		item := apitypes.CapabilityProvenanceItem{Capability: cap, Required: slices.Contains(run.RequiredCapabilities, cap), Sources: []string{}, Triggers: []apitypes.CapabilityTrigger{}}
		hinted := slices.Contains(run.HintedCapabilities, cap)
		if hinted {
			item.Sources = append(item.Sources, "repo_hint")
		}
		if observed || (out.HintsKnown && !hinted && item.Required) {
			item.Sources = append(item.Sources, "inferred")
		}
		if len(item.Sources) == 0 {
			item.Sources = append(item.Sources, "unknown")
		}
		item.InferenceIgnored = observed && slices.Contains(run.IgnoredInferredCapabilities, cap)
		if observed {
			item.Triggers = obs.Triggers
			item.Truncated = obs.Truncated
		}
		out.Capabilities = append(out.Capabilities, item)
	}
	return out
}
