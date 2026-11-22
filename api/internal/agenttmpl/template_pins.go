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

// EffortForHarness projects a template's stored effort pins onto the run's harness (issue #3258):
// the family's own column, and only when it is exactly one of EffortLevels. Another family's pin,
// an unknown harness and any corrupt stored value yield "" (no pin), so a hostile row never
// reaches a worker. The match is exact (no trimming), mirroring the column CHECK.
func EffortForHarness(harness, claudeEffort, codexEffort string) string {
	var v string
	switch harness {
	case "claude":
		v = claudeEffort
	case "codex":
		v = codexEffort
	default:
		return ""
	}
	for _, level := range EffortLevels {
		if v == level {
			return v
		}
	}
	return ""
}
