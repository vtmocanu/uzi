package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func TestRepoListShowsAgentSourceDefault(t *testing.T) {
	own := "own"
	fc := &uzicli.FakeClient{Repos: []apitypes.RepoDTO{
		{ID: "r1", PathWithNamespace: "org/a", AgentSourceDefault: &own},
		{ID: "r2", PathWithNamespace: "org/b"},
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "repo", "list", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d", code)
	}
	var got []map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	if len(got) != 2 {
		t.Fatalf("repos = %v", got)
	}
	if v, ok := got[0]["agent_source_default"]; !ok || v != "own" {
		t.Errorf("r1 agent_source_default = %v (present %v), want own", v, ok)
	}
	if v, ok := got[1]["agent_source_default"]; !ok || v != nil {
		t.Errorf("r2 agent_source_default = %v (present %v), want null", v, ok)
	}

	out, _, code = runCLI(t, fakeEnv(fc), "repo", "list")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(out, "DEFAULT_AGENTS") || !strings.Contains(out, "own") || !strings.Contains(out, "unset") {
		t.Errorf("table = %q, want a DEFAULT_AGENTS column with own and unset", out)
	}
}

func TestRunGetAgentSourceRows(t *testing.T) {
	s := func(v string) *string { return &v }
	for _, tc := range []struct {
		name      string
		run       apitypes.RunDTO
		want      []string
		forbidden []string
	}{
		{"unset", apitypes.RunDTO{ID: "r1", Status: "running", AgentSource: s("repo")},
			[]string{"IMPLEMENTED_WITH", "repo agents"}, []string{"AGENTS_REQUESTED", "as requested", "changed at approval"}},
		{"as requested", apitypes.RunDTO{ID: "r1", Status: "running", AgentSource: s("own"), AgentSourceRequested: s("own"), AgentSourceRequestedBasis: s("repo_default")},
			[]string{"AGENTS_REQUESTED", "my templates (repo default)", "my templates (as requested)"}, nil},
		{"changed at approval", apitypes.RunDTO{ID: "r1", Status: "running", AgentSource: s("own"), AgentSourceRequested: s("repo"), AgentSourceRequestedBasis: s("explicit")},
			[]string{"repo agents (this run)", "my templates (changed at approval)"}, nil},
		{"requested before approval", apitypes.RunDTO{ID: "r1", Status: "awaiting_approval", AgentSourceRequested: s("repo"), AgentSourceRequestedBasis: s("repo_default")},
			[]string{"repo agents (repo default)"}, []string{"IMPLEMENTED_WITH"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fc := &uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{"r1": tc.run}}
			out, _, code := runCLI(t, fakeEnv(fc), "run", "get", "r1")
			if code != uzicli.ExitOK {
				t.Fatalf("exit = %d", code)
			}
			for _, w := range tc.want {
				if !strings.Contains(out, w) {
					t.Errorf("run get output lacks %q:\n%s", w, out)
				}
			}
			for _, f := range tc.forbidden {
				if strings.Contains(out, f) {
					t.Errorf("run get output carries %q:\n%s", f, out)
				}
			}
		})
	}
}
