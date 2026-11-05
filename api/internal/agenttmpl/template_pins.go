package agenttmpl

import (
	"fmt"
	"strings"
)

// ValidateTemplatePin validates a template's per-harness model pin.
func ValidateTemplatePin(harness, raw string) (string, error) {
	switch harness {
	case "claude":
		return ValidateClaudeLaneModel(raw)
	case "codex":
		model, err := ValidateModel(raw)
		if err != nil {
			return "", err
		}
		if model != "" && !CodexRoleModels[model] {
			return "", fmt.Errorf("model must be a supported Codex role model id")
		}
		return model, nil
	default:
		return "", fmt.Errorf("unknown harness")
	}
}

// SplitModelPin classifies a single source model by the role vocabulary.
func SplitModelPin(raw string) (claude, codex string) {
	model := strings.TrimSpace(raw)
	if CodexRoleModels[model] {
		return "", model
	}
	return model, ""
}

// PinForHarness filters stored pins at delivery, including legacy rows.
func PinForHarness(harness, claudePin, codexPin string) string {
	switch harness {
	case "claude":
		if !CodexRoleModels[strings.TrimSpace(claudePin)] {
			return claudePin
		}
	case "codex":
		if CodexRoleModels[codexPin] {
			return codexPin
		}
	}
	return ""
}
