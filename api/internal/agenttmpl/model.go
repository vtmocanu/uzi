package agenttmpl

import (
	"fmt"
	"strings"
	"unicode"
)

// MaxModelLen caps a model alias / full ID. Real model IDs (aliases like "opus"
// through full bedrock IDs) sit well under this; the cap only bounds a pasted
// blob. Kept in lockstep with the web MAX_MODEL_LEN (PRD #17 Decision 4).
const MaxModelLen = 100

// ValidateModel is the single source of truth for the Decision 4 model rules
// (PRD #17). It lives in this neutral, dependency-free package so every surface
// can share it without an import cycle: the handler's template + per-user
// default-model endpoints wrap it (mapping to pgtype.Text + an HTTP error), and
// the builtin parse/validity tests call it directly — no drifting second/third
// copy of the rules.
//
// A blank (or whitespace-only) value means inherit and returns ("", nil): the
// caller decides how to store "inherit" (NULL). A non-blank value must be a
// single token — trimmed, free of control characters and interior whitespace,
// and at most MaxModelLen bytes — and is returned trimmed. A typo in a custom ID
// is accepted here and only surfaces as a run-time SDK error; the API cannot
// enumerate valid IDs without calling Anthropic.
func ValidateModel(raw string) (string, error) {
	m := strings.TrimSpace(raw)
	if m == "" {
		return "", nil
	}
	if len(m) > MaxModelLen {
		return "", fmt.Errorf("model must be at most %d characters", MaxModelLen)
	}
	for _, r := range m {
		if r == unicode.ReplacementChar || unicode.IsControl(r) {
			return "", fmt.Errorf("model must not contain newlines or control characters")
		}
		if unicode.IsSpace(r) {
			return "", fmt.Errorf("model must be a single token with no spaces")
		}
	}
	return m, nil
}
