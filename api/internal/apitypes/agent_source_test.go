package apitypes

import (
	"encoding/json"
	"testing"
)

func TestAgentSourceRoleModelPinsJSON(t *testing.T) {
	for _, tc := range []struct {
		raw, claude, codex string
	}{
		{"gpt-6-luna", "", "gpt-6-luna"},
		{"custom-claude", "custom-claude", ""},
		{"", "", ""},
	} {
		role := AgentSourceRoleDTO{Model: tc.raw, ClaudeModel: tc.claude, CodexModel: tc.codex}
		wire, err := json.Marshal(role)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]any
		if err := json.Unmarshal(wire, &fields); err != nil {
			t.Fatal(err)
		}
		if fields["claude_model"] != tc.claude || fields["codex_model"] != tc.codex {
			t.Fatalf("derived pins missing from JSON: %s", wire)
		}
		var got AgentSourceRoleDTO
		if err := json.Unmarshal(wire, &got); err != nil {
			t.Fatal(err)
		}
		if got.Model != tc.raw || got.ClaudeModel != tc.claude || got.CodexModel != tc.codex {
			t.Fatalf("round trip changed pins: %+v", got)
		}
	}
}
