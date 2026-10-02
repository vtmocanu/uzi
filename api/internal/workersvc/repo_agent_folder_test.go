package workersvc

import (
	"bytes"
	"encoding/json"
	"testing"
)

// TestStateRequestDecodesRepoAgentFolder (issue #2085): the /state decode is strict
// (DisallowUnknownFields), so the per-element folder a worker sends after seeing
// repo_agent_folder must be a known field, and it must survive the encode/decode the
// runs.repo_agents column goes through.
func TestStateRequestDecodesRepoAgentFolder(t *testing.T) {
	body := `{"status":"running","repo_agents":[{"name":"a","description":"d","folder":".codex/agents"}]}`
	dec := json.NewDecoder(bytes.NewReader([]byte(body)))
	dec.DisallowUnknownFields()
	var req StateRequest
	if err := dec.Decode(&req); err != nil {
		t.Fatalf("strict decode: %v", err)
	}
	if req.RepoAgents == nil || len(*req.RepoAgents) != 1 || (*req.RepoAgents)[0].Folder != ".codex/agents" {
		t.Fatalf("folder not decoded: %+v", req.RepoAgents)
	}
	if err := validateRepoAgents(*req.RepoAgents); err != nil {
		t.Fatalf("validate: %v", err)
	}
	raw, err := encodeJSONArray(*req.RepoAgents)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeRepoAgents(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Folder != ".codex/agents" || got[0].Name != "a" {
		t.Fatalf("round trip lost folder: %+v", got)
	}
	// Absent folder stays absent on the wire (omitempty), so old readers see the old shape.
	raw, _ = encodeJSONArray([]RepoAgent{{Name: "a", Description: "d"}})
	if bytes.Contains(raw, []byte("folder")) {
		t.Fatalf("empty folder serialized: %s", raw)
	}
}
