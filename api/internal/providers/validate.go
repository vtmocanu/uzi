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

// ModelMap maps an engine-specific model key to its gateway model.
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
	return validateModelMap(m, ModelAliases, HaikuAlias)
}

// ValidateModelMapForEngine validates the exact model vocabulary of an engine.
func ValidateModelMapForEngine(engine string, m ModelMap) error {
	switch engine {
	case EngineClaude:
		return ValidateModelMap(m)
	case EngineCodex:
		return validateModelMap(m, []string{"default", "gpt-6-astra", "gpt-5.6-sol", "gpt-6-sol", "gpt-6.1-sol", "gpt-6.2-sol", "gpt-6.2-astra", "gpt-6-luna"}, "default")
	default:
		return errors.New("unsupported provider engine")
	}
}

func validateModelMap(m ModelMap, aliases []string, required string) error {
	if len(m) > MaxModelMapEntries {
		return fmt.Errorf("model map must have at most %d entries", MaxModelMapEntries)
	}
	if _, ok := m[required]; !ok {
		return fmt.Errorf("model map must define %s", required)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		known := false
		for _, a := range aliases {
			if a == k {
				known = true
			}
		}
		if !known {
			if required == HaikuAlias {
				return errors.New("model map keys must be opus, sonnet, haiku or fable")
			}
			return fmt.Errorf("model map keys must be one of %v", aliases)
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

// Supported provider engines, protocols and auth styles.
const (
	AuthBearer        = "bearer"
	AuthXAPIKey       = "x_api_key"
	EngineClaude      = "claude"
	EngineCodex       = "codex"
	ProtocolResponses = "openai_responses"
	ProtocolAnth      = "anthropic_messages"
)

// ValidateAuthStyle accepts bearer or x_api_key.
func ValidateAuthStyle(s string) error {
	if s != AuthBearer && s != AuthXAPIKey {
		return errors.New("auth_style must be bearer or x_api_key")
	}
	return nil
}

// ValidateProfile validates engine/protocol/auth pairing before credentials are used.
func ValidateProfile(engine, protocol, authStyle string, models ModelMap) error {
	switch engine {
	case EngineClaude:
		if protocol != ProtocolAnth {
			return errors.New("claude requires anthropic_messages")
		}
		if err := ValidateAuthStyle(authStyle); err != nil {
			return err
		}
	case EngineCodex:
		if protocol != ProtocolResponses || authStyle != AuthBearer {
			return errors.New("codex requires openai_responses with bearer auth")
		}
	default:
		return errors.New("unsupported provider engine")
	}
	return ValidateModelMapForEngine(engine, models)
}
