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

// ValidateEffort is the single source of truth for the per-user default-effort
// rules (PRD #617). It lives in this neutral, dependency-free package so every
// surface can share it without an import cycle, mirroring ValidateModel.
//
// A blank (or whitespace-only) value means inherit and returns ("", nil): the
// caller decides how to store "inherit" (NULL — we then omit the SDK `effort`
// key, so the SDK default `high` applies). A non-blank value is trimmed and must
// then EQUAL exactly one of EffortLevels (case-sensitive): trimming only strips
// the ends, so an interior-whitespace value ("hi gh"), an unknown token, or a
// differently-cased value ("HIGH") is rejected.
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
