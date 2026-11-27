package providers

import (
	"fmt"
	"strings"
)

// ResolveModelID resolves a requested model to the target model id a provider's frozen model map
// sends upstream. ok is false for Unresolved. It mirrors the agent: for Claude,
// resolveProviderModel (agent/src/provider-binding.ts); for Codex,
// resolveCodexProviderModelSlot followed by the map lookup. The pair is pinned by
// fixtures/provider-model-identity/cases.json, read by the Go test beside this file.
//
// explicit only matters for Codex: an explicit pin that is not a map key is Unresolved instead of
// falling back to the default slot.
func ResolveModelID(engine string, m ModelMap, requested string, explicit bool) (string, bool) {
	switch engine {
	case EngineClaude:
		return resolveClaudeModelID(m, requested)
	case EngineCodex:
		slot := "default"
		if _, isKey := m[requested]; isKey {
			slot = requested
		} else if explicit {
			return "", false
		}
		return targetOf(m, slot)
	default:
		return "", false
	}
}

func resolveClaudeModelID(m ModelMap, requested string) (string, bool) {
	effective := requested
	if strings.TrimSpace(requested) == "" || requested == "inherit" {
		effective = "sonnet"
	}
	for _, alias := range ModelAliases {
		if effective == alias {
			return targetOf(m, alias)
		}
	}
	for _, alias := range ModelAliases {
		if e, ok := m[alias]; ok && e.Model == effective {
			return effective, true
		}
	}
	return "", false
}

func targetOf(m ModelMap, key string) (string, bool) {
	e, ok := m[key]
	if !ok || e.Model == "" {
		return "", false
	}
	return e.Model, true
}

// NormalizedHost returns the host of a stored (already normalized) provider base URL. Stored
// values are lowercase with no trailing dot, so the result is comparable by string equality.
//
// The port and path are deliberately dropped: two gateways on one host are treated as the same
// provider. A SameModel outcome fails closed (the checker is refused), so the wider match can only
// over-refuse, never admit a shared model. A caller must treat an error here as a refusal and never
// compare an empty host.
func NormalizedHost(baseURL string) (string, error) {
	norm, err := NormalizeBaseURL(baseURL, true)
	if err != nil {
		return "", err
	}
	host, err := hostOf(norm)
	if err != nil {
		return "", fmt.Errorf("base URL host: %w", err)
	}
	return host, nil
}

// IdentityOutcome is the result of comparing the lead's and the checker's resolved model identity.
type IdentityOutcome int

const (
	// NotEvaluated: both sides are vendor-served, so the guard does not apply.
	NotEvaluated IdentityOutcome = iota
	// Independent: the two sides do not share a model.
	Independent
	// SameModel: the two sides resolve to the same model.
	SameModel
	// Unresolved: at least one side's identity could not be resolved.
	Unresolved
)

// IdentitySide is one side of the comparison. Host is only meaningful when ProviderBound.
type IdentitySide struct {
	ProviderBound bool
	Host          string
	ModelID       string
	Resolved      bool
}

// CompareCheckerIdentity compares resolved ids by exact string equality (no case folding).
func CompareCheckerIdentity(lead, checker IdentitySide) IdentityOutcome {
	if !lead.ProviderBound && !checker.ProviderBound {
		return NotEvaluated
	}
	if !lead.Resolved || !checker.Resolved {
		return Unresolved
	}
	// A provider-bound side with no host cannot be compared (NormalizedHost failed or was never
	// read): fail closed rather than let an empty host read as a different gateway.
	if (lead.ProviderBound && lead.Host == "") || (checker.ProviderBound && checker.Host == "") {
		return Unresolved
	}
	if lead.ModelID != checker.ModelID {
		return Independent
	}
	if lead.ProviderBound && checker.ProviderBound && lead.Host != checker.Host {
		return Independent
	}
	return SameModel
}
