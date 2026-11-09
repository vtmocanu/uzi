package main

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func TestAdminAgentSourceJSONModelPins(t *testing.T) {
	// Decode an API-shaped document through the shared DTO, as the real client
	// does, then drive the command's JSON output. Text rendering has no model row.
	const wire = `{"staged":{"fetched_sha":"sha","roles":[
		{"name":"coder","ok":true,"model":"gpt-6-luna","claude_model":"","codex_model":"gpt-6-luna","body_sanitized":false},
		{"name":"reviewer","ok":true,"model":"custom-claude","claude_model":"custom-claude","codex_model":"","body_sanitized":false}
	]}}`
	var doc apitypes.AgentSourceDTO
	if err := json.Unmarshal([]byte(wire), &doc); err != nil {
		t.Fatal(err)
	}
	fc := &uzicli.FakeClient{AgentSourceV: doc}
	out, stderr, code := runCLI(t, fakeEnv(fc), "admin", "agent-source", "status", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit=%d stderr=%s", code, stderr)
	}
	var got apitypes.AgentSourceDTO
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode output: %v\n%s", err, out)
	}
	if !reflect.DeepEqual(got, doc) {
		t.Fatalf("JSON changed shared DTO: %+v", got.Staged)
	}
	var envelope struct {
		Staged struct {
			Roles []map[string]any `json:"roles"`
		} `json:"staged"`
	}
	if err := json.Unmarshal([]byte(out), &envelope); err != nil {
		t.Fatal(err)
	}
	for i, want := range []struct{ claude, codex string }{{"", "gpt-6-luna"}, {"custom-claude", ""}} {
		role := envelope.Staged.Roles[i]
		if role["claude_model"] != want.claude || role["codex_model"] != want.codex {
			t.Fatalf("role %d lost derived fields: %v", i, role)
		}
	}
}
