package providers

import (
	"strings"
	"testing"
)

func TestCodexProfileValidation(t *testing.T) {
	valid := ModelMap{"default": {Model: "gateway/default"}}
	for _, id := range []string{"gpt-6-astra", "gpt-5.6-sol", "gpt-6-sol", "gpt-6.1-sol", "gpt-6.2-sol", "gpt-6.2-astra", "gpt-6-luna"} {
		valid[id] = ModelEntry{Model: "gateway/" + id}
	}
	if err := ValidateProfile(EngineCodex, ProtocolResponses, AuthBearer, valid); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, engine, protocol, auth string
		models                       ModelMap
	}{
		{"no default", EngineCodex, ProtocolResponses, AuthBearer, ModelMap{"gpt-6-sol": {Model: "x"}}},
		{"claude alias", EngineCodex, ProtocolResponses, AuthBearer, ModelMap{"default": {Model: "x"}, "haiku": {Model: "x"}}},
		{"unknown id", EngineCodex, ProtocolResponses, AuthBearer, ModelMap{"default": {Model: "x"}, "gpt-6": {Model: "x"}}},
		{"x api key", EngineCodex, ProtocolResponses, AuthXAPIKey, valid},
		{"wrong protocol", EngineCodex, ProtocolAnth, AuthBearer, valid},
		{"unknown engine", "other", ProtocolResponses, AuthBearer, valid},
		{"empty model", EngineCodex, ProtocolResponses, AuthBearer, ModelMap{"default": {}}},
		{"long model", EngineCodex, ProtocolResponses, AuthBearer, ModelMap{"default": {Model: strings.Repeat("a", MaxModelLen+1)}}},
		{"invalid model", EngineCodex, ProtocolResponses, AuthBearer, ModelMap{"default": {Model: "has space"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateProfile(tc.engine, tc.protocol, tc.auth, tc.models); err == nil {
				t.Fatal("accepted invalid profile")
			}
		})
	}
	for _, slot := range []string{"default", "gpt-6.2-sol", "gpt-6.2-astra"} {
		for _, n := range []int{MinContextWindow - 1, MinContextWindow, MaxContextWindow, MaxContextWindow + 1} {
			err := ValidateModelMapForEngine(EngineCodex, ModelMap{"default": {Model: "x"}, slot: {Model: "x", ContextWindow: &n}})
			if (err == nil) != (n >= MinContextWindow && n <= MaxContextWindow) {
				t.Fatalf("context %d: %v", n, err)
			}
		}
	}
	if err := ValidateModelMap(ModelMap{"haiku": {Model: "legacy"}}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateModelMap(valid); err == nil {
		t.Fatal("Claude helper accepted Codex map")
	}
}
