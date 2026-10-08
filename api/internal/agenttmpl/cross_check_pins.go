package agenttmpl

import "fmt"

const CrossCheckWorkerDefault = "worker default"
const CrossCheckPin = "pin"
const DefaultCodexModel = "gpt-6.1-sol"

// CuratedCodexModels and KnownClaudeAliases are the closed family guards shared
// by worker lanes and checker pins. Custom IDs remain provider-validated.
var CuratedCodexModels = map[string]bool{
	"gpt-6-astra": true, "gpt-5.6-sol": true, "gpt-6-sol": true, "gpt-6.1-sol": true,
}
var KnownClaudeAliases = map[string]bool{"opus": true, "sonnet": true, "haiku": true, "fable": true}

func ValidateFamilyModel(harness, raw string) (string, error) {
	model, err := ValidateModel(raw)
	if err != nil {
		return "", err
	}
	switch harness {
	case "claude":
		if CuratedCodexModels[model] {
			return "", fmt.Errorf("model is a Codex model id")
		}
	case "codex":
		if KnownClaudeAliases[model] {
			return "", fmt.Errorf("model is a Claude model alias")
		}
	default:
		return "", fmt.Errorf("unknown harness")
	}
	return model, nil
}

func ValidateFamilyEffort(harness, raw string) (string, error) {
	if harness != "claude" && harness != "codex" {
		return "", fmt.Errorf("unknown harness")
	}
	effort, err := ValidateEffort(raw)
	if err != nil {
		return "", err
	}
	return effort, nil
}

// CrossCheckResolution preserves independent field provenance. templateModel is
// the first delivered lead/orchestrator's model, even when that model is nil.
type CrossCheckResolution struct {
	Model        *string
	Effort       string
	ModelSource  string
	EffortSource string
}

func ResolveCrossCheck(harness string, model, effort, workerModel, workerEffort, templateModel *string) CrossCheckResolution {
	r := CrossCheckResolution{Model: workerModel, Effort: ResolveDefaultEffort(workerEffort),
		ModelSource: CrossCheckWorkerDefault, EffortSource: CrossCheckWorkerDefault}
	if harness == "codex" {
		r.Effort = ResolveDefaultCodexEffort(workerEffort)
	}
	if r.Model == nil {
		if harness == "codex" {
			fallback := DefaultCodexModel
			r.Model = &fallback
		} else {
			r.Model = templateModel
		}
	}
	if model != nil {
		r.Model, r.ModelSource = model, CrossCheckPin
	}
	if effort != nil {
		r.Effort, r.EffortSource = *effort, CrossCheckPin
	}
	return r
}
