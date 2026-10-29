package agenttmpl

import (
	"fmt"
	"strings"
)

// EffortLevels is the single source of truth for the closed set of Agent SDK
// reasoning-effort levels (PRD #617). It mirrors the SDK's `EffortLevel` union
// (agent/node_modules/@anthropic-ai/claude-agent-sdk/sdk.d.ts:555 =
// 'low' | 'medium' | 'high' | 'xhigh' | 'max') and the web EffortSelect list.
// There is no shared source across the three (the agent keeps a copy as
// HARNESS_EFFORTS in agent/src/harness.ts), so they must be kept in lockstep:
// change one and change the others. The preference-only token EffortRuntime is
// deliberately not a member.
var EffortLevels = []string{"low", "medium", "high", "xhigh", "max"}

// UziDefaultEffort is uzi's own default reasoning-effort level (issue #1157),
// applied to a run whose owner has NOT chosen an explicit level (the per-user
// default_effort column is NULL/blank). It replaces the Claude Agent SDK's own
// built-in fallback (`high`) as the *effective* uzi default. It is a member of
// EffortLevels; changing it must keep it inside that closed set.
const UziDefaultEffort = "medium"

// UziDefaultCodexEffort is the independently overridable Codex product default.
const UziDefaultCodexEffort = "medium"

// EffortRuntime is the stored and API token for the "Runtime default" choice of
// the two per-user ordinary effort preferences (PRD #3247). It is a
// preference-only token: it is not a member of EffortLevels, no claim ever
// carries it, and cross-check pins (ValidateEffort) reject it.
const EffortRuntime = "runtime"

// Effort resolution modes and sources (PRD #3247).
const (
	EffortModeExplicit       = "explicit"
	EffortModeRuntimeDefault = "runtime_default"

	EffortSourceUserDefault = "user_default"
	EffortSourceBuiltin     = "builtin"
)

// EffortResolution is the outcome of resolving an ordinary effort preference.
// Level is set only for EffortModeExplicit.
type EffortResolution struct {
	Mode   string
	Level  string
	Source string
}

// ResolveHarnessEffort is the single stored-to-wire mapping for the owner's
// per-harness ordinary effort preference. NULL/blank rides the harness's uzi
// default (explicit, builtin); an exact level is the owner's choice (explicit,
// user_default); `runtime` sends no level (runtime_default, user_default). Any
// other stored string (corrupt or future) falls back to the uzi default and is
// never passed through verbatim. Checkers do not use it: their effort is
// independent (ResolveCrossCheck).
func ResolveHarnessEffort(harness string, stored *string) EffortResolution {
	fallback := UziDefaultEffort
	if harness == "codex" {
		fallback = UziDefaultCodexEffort
	}
	builtin := EffortResolution{Mode: EffortModeExplicit, Level: fallback, Source: EffortSourceBuiltin}
	if stored == nil {
		return builtin
	}
	e := strings.TrimSpace(*stored)
	if e == "" {
		return builtin
	}
	if e == EffortRuntime {
		return EffortResolution{Mode: EffortModeRuntimeDefault, Source: EffortSourceUserDefault}
	}
	for _, level := range EffortLevels {
		if e == level {
			return EffortResolution{Mode: EffortModeExplicit, Level: e, Source: EffortSourceUserDefault}
		}
	}
	return builtin
}

// ValidateDefaultEffort validates the two per-user preference fields: ValidateEffort's
// rules plus the preference-only EffortRuntime token. Blank returns ("", nil).
func ValidateDefaultEffort(raw string) (string, error) {
	if strings.TrimSpace(raw) == EffortRuntime {
		return EffortRuntime, nil
	}
	if e, err := ValidateEffort(raw); err == nil {
		return e, nil
	}
	return "", fmt.Errorf("effort must be one of low, medium, high, xhigh, max or runtime")
}

// ValidateEffort is the single source of truth for the effort-level rules (PRD
// #617), used by cross-check pins. It lives in this neutral, dependency-free
// package so every surface can share it without an import cycle, mirroring
// ValidateModel. It rejects `runtime`; the per-user preferences use
// ValidateDefaultEffort.
//
// A blank (or whitespace-only) value means inherit and returns ("", nil): the
// caller stores "inherit" as NULL. A non-blank value is trimmed and must then
// EQUAL exactly one of EffortLevels
// (case-sensitive): trimming only strips the ends, so an interior-whitespace value
// ("hi gh"), an unknown token, or a differently-cased value ("HIGH") is rejected.
func ValidateEffort(raw string) (string, error) {
	e := strings.TrimSpace(raw)
	if e == "" {
		return "", nil
	}
	for _, level := range EffortLevels {
		if e == level {
			return e, nil
		}
	}
	return "", fmt.Errorf("effort must be one of low, medium, high, xhigh, max")
}
