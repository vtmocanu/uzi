package providers

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
)

// Bounds on user-supplied provider input (PRD #4320, untrusted input).
const (
	MinKeyLen          = 16
	MaxKeyLen          = 4096
	MaxModelMapEntries = 8
	MaxModelLen        = 256
	MinContextWindow   = 8192
	MaxContextWindow   = 10000000
)

// ModelAliases are the model-map keys a provider may define; HaikuAlias is required because the
// SDK's background calls use the small/fast slot.
var ModelAliases = []string{"opus", "sonnet", "haiku", "fable"}

// HaikuAlias is the mandatory model-map key.
const HaikuAlias = "haiku"

var modelNameRE = regexp.MustCompile(`^[A-Za-z0-9._:/@-]+$`)

// ModelEntry is one mapped gateway model. ContextWindow is optional (tokens).
type ModelEntry struct {
	Model         string `json:"model"`
	ContextWindow *int   `json:"context_window,omitempty"`
}

// ModelMap maps a Claude alias to its gateway model.
type ModelMap map[string]ModelEntry

// ValidateKey checks a pasted provider key: 16..4096 characters, each printable ASCII 0x21..0x7E
// (no whitespace, control or non-ASCII). The error never contains the key.
func ValidateKey(key string) error {
	if len(key) < MinKeyLen {
		return fmt.Errorf("key must be at least %d characters", MinKeyLen)
	}
	if len(key) > MaxKeyLen {
		return fmt.Errorf("key must be at most %d characters", MaxKeyLen)
	}
	for i := 0; i < len(key); i++ {
		if key[i] < 0x21 || key[i] > 0x7E {
			return errors.New("key must contain only printable ASCII characters without spaces")
		}
	}
	return nil
}

// ValidateModelMap enforces the model-map bounds: at most MaxModelMapEntries entries, keys within
// ModelAliases, haiku present, each model 1..MaxModelLen of [A-Za-z0-9._:/@-], and an optional
// integer context window within [MinContextWindow, MaxContextWindow].
func ValidateModelMap(m ModelMap) error {
	if len(m) > MaxModelMapEntries {
		return fmt.Errorf("model map must have at most %d entries", MaxModelMapEntries)
	}
	if _, ok := m[HaikuAlias]; !ok {
		return errors.New("model map must define haiku")
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		known := false
		for _, a := range ModelAliases {
			if a == k {
				known = true
			}
		}
		if !known {
			return errors.New("model map keys must be opus, sonnet, haiku or fable")
		}
		e := m[k]
		if e.Model == "" || len(e.Model) > MaxModelLen {
			return fmt.Errorf("model for %s must be 1..%d characters", k, MaxModelLen)
		}
		if !modelNameRE.MatchString(e.Model) {
			return fmt.Errorf("model for %s may contain only letters, digits and . _ : / @ -", k)
		}
		if e.ContextWindow != nil && (*e.ContextWindow < MinContextWindow || *e.ContextWindow > MaxContextWindow) {
			return fmt.Errorf("context_window for %s must be between %d and %d", k, MinContextWindow, MaxContextWindow)
		}
	}
	return nil
}

// Auth styles and the single protocol and engine accepted today.
const (
	AuthBearer   = "bearer"
	AuthXAPIKey  = "x_api_key"
	EngineClaude = "claude"
	ProtocolAnth = "anthropic_messages"
)

// ValidateAuthStyle accepts bearer or x_api_key.
func ValidateAuthStyle(s string) error {
	if s != AuthBearer && s != AuthXAPIKey {
		return errors.New("auth_style must be bearer or x_api_key")
	}
	return nil
}
