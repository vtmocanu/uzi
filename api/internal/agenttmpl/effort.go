package agenttmpl

import (
	"fmt"
	"strings"
)

// EffortLevels is the single source of truth for the closed set of Agent SDK
// reasoning-effort levels (PRD #617). It mirrors the SDK's `EffortLevel` union
// (agent/node_modules/@anthropic-ai/claude-agent-sdk/sdk.d.ts:555 =
// 'low' | 'medium' | 'high' | 'xhigh' | 'max') and the web EffortSelect list.
// There is no shared source across the three, so they must be kept in lockstep:
// change one and change the other two.
var EffortLevels = []string{"low", "medium", "high", "xhigh", "max"}

// UziDefaultEffort is uzi's own default reasoning-effort level (issue #1157),
// applied to a run whose owner has NOT chosen an explicit level (the per-user
// default_effort column is NULL/blank). It replaces the Claude Agent SDK's own
// built-in fallback (`high`) as the *effective* uzi default. It is a member of
// EffortLevels; changing it must keep it inside that closed set.
const UziDefaultEffort = "xhigh"

// ResolveDefaultEffort resolves the owner's per-user default effort to the level a
// run actually uses (issue #1157): the owner's explicit choice when set, or
// UziDefaultEffort when the value is nil (NULL) or blank/whitespace-only (inherit).
// Callers pass the per-user value (e.g. via textPtr over the NULLable column); the
// resolver never returns "" — an inheriting owner rides the uzi default, not an
// omitted key.
func ResolveDefaultEffort(userEffort *string) string {
	if userEffort == nil {
		return UziDefaultEffort
	}
	if e := strings.TrimSpace(*userEffort); e != "" {
		return e
	}
	return UziDefaultEffort
}

// ValidateEffort is the single source of truth for the per-user default-effort
// rules (PRD #617). It lives in this neutral, dependency-free package so every
// surface can share it without an import cycle, mirroring ValidateModel.
//
// A blank (or whitespace-only) value means inherit and returns ("", nil): the
// caller stores "inherit" as NULL, and inherit resolves to the uzi default
// (UziDefaultEffort = `xhigh`) at claim assembly (see ResolveDefaultEffort). A
// non-blank value is trimmed and must then EQUAL exactly one of EffortLevels
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
