package agenttmpl

import (
	"encoding/json"
	"os"
	"testing"
)

func sp(s string) *string { return &s }

func TestResolveWorkerDefaultModel(t *testing.T) {
	for _, c := range []struct {
		name                     string
		harness                  string
		lane, tmplClaude, tmplCx *string
		wantModel, wantSource    string
	}{
		// Cases carried over from the retired workersvc.CodexWorkerDefaultModel table.
		{"codex lane wins over pin", "codex", sp("gpt-6-sol"), nil, sp("gpt-6-astra"), "gpt-6-sol", "user_default"},
		{"codex lane custom id passes through", "codex", sp("gpt-custom-1"), nil, sp("gpt-6-astra"), "gpt-custom-1", "user_default"},
		{"codex provider default", "codex", nil, nil, nil, "gpt-6.1-sol", "builtin"},
		{"codex empty lane is unset", "codex", sp(""), nil, nil, "gpt-6.1-sol", "builtin"},
		{"codex empty lane still wins over a role-model pin", "codex", sp(""), nil, sp("gpt-6-luna"), "gpt-6.1-sol", "builtin"},
		{"codex lead role-model pin", "codex", nil, nil, sp("gpt-6-luna"), "gpt-6-luna", "template"},
		{"codex pin outside the role models is ignored", "codex", nil, nil, sp("gpt-custom-1"), "gpt-6.1-sol", "builtin"},
		{"codex claude alias pin is ignored", "codex", nil, nil, sp("opus"), "gpt-6.1-sol", "builtin"},
		// Claude.
		{"claude lane", "claude", sp("sonnet"), sp("opus"), nil, "sonnet", "user_default"},
		{"claude lane holding a Codex role id is unset, template wins", "claude", sp("gpt-6-sol"), sp("opus"), nil, "opus", "template"},
		{"claude lane holding a Codex role id, no template", "claude", sp(" gpt-6-sol "), nil, nil, "", "builtin"},
		{"claude template", "claude", nil, sp("opus"), sp("gpt-6-luna"), "opus", "template"},
		{"claude template holding luna is filtered", "claude", nil, sp("gpt-6-luna"), nil, "", "builtin"},
		{"claude ignores the codex pin", "claude", nil, nil, sp("gpt-6-luna"), "", "builtin"},
		{"claude no template is the SDK default", "claude", nil, nil, nil, "", "builtin"},
		{"claude blank template is the SDK default", "claude", nil, sp("  "), nil, "", "builtin"},
		{"claude template with spaces is invalid", "claude", nil, sp("a b"), nil, "", "builtin"},
	} {
		model, source := ResolveWorkerDefaultModel(c.harness, c.lane, c.tmplClaude, c.tmplCx)
		if model != c.wantModel || source != c.wantSource {
			t.Errorf("%s: got (%q, %q), want (%q, %q)", c.name, model, source, c.wantModel, c.wantSource)
		}
	}
}

// TestWorkerDefaultVsCrossCheckWorkerStep documents where the worker-default rule and the
// checker's worker step (ResolveCrossCheck with no worker lane) agree and where they differ.
// The differences only arise for legacy inputs: the settings PUT stores an empty lane as NULL
// and refuses a Codex role id in the Claude lane.
func TestWorkerDefaultVsCrossCheckWorkerStep(t *testing.T) {
	for _, c := range []struct {
		name                         string
		harness                      string
		lane, tmplClaude, tmplCx     *string
		wantWorker, wantChecker      string // "" for the SDK default (nil checker model)
		divergesOnLegacyLaneInputsOK bool
	}{
		{"no lane, template claude", "claude", nil, sp("opus"), nil, "opus", "opus", false},
		{"no lane, no template claude", "claude", nil, nil, nil, "", "", false},
		{"no lane, filtered luna claude", "claude", nil, sp("gpt-6-luna"), nil, "", "", false},
		{"no lane, template codex", "codex", nil, nil, sp("gpt-6-luna"), "gpt-6-luna", "gpt-6-luna", false},
		{"no lane, no template codex", "codex", nil, nil, nil, "gpt-6.1-sol", "gpt-6.1-sol", false},
		{"lane value codex", "codex", sp("gpt-6-sol"), nil, nil, "gpt-6-sol", "gpt-6-sol", false},
		{"lane value claude", "claude", sp("sonnet"), nil, nil, "sonnet", "sonnet", false},
		// Legacy-only divergences.
		{"empty codex lane", "codex", sp(""), nil, nil, "gpt-6.1-sol", "", true},
		{"claude lane holding a Codex role id", "claude", sp("gpt-6-sol"), sp("opus"), nil, "opus", "gpt-6-sol", true},
	} {
		worker, _ := ResolveWorkerDefaultModel(c.harness, c.lane, c.tmplClaude, c.tmplCx)
		checker := ResolveCrossCheck(c.harness, nil, nil, c.lane, c.tmplClaude, c.tmplCx)
		got := ""
		if checker.Model != nil {
			got = *checker.Model
		}
		if worker != c.wantWorker {
			t.Errorf("%s: worker default = %q, want %q", c.name, worker, c.wantWorker)
		}
		if got != c.wantChecker {
			t.Errorf("%s: checker model = %q, want %q", c.name, got, c.wantChecker)
		}
		if (worker != got) != c.divergesOnLegacyLaneInputsOK {
			t.Errorf("%s: divergence = %v, documented %v", c.name, worker != got, c.divergesOnLegacyLaneInputsOK)
		}
	}
}

func TestTaskReviewBuiltinsMatchFixture(t *testing.T) {
	raw, err := os.ReadFile("../../../fixtures/model-settings/task-review.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx struct {
		Builtins map[string]string `json:"builtins"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	if ClaudeTaskReviewModel != fx.Builtins["claude"] {
		t.Errorf("ClaudeTaskReviewModel = %q, want fixture builtins.claude %q", ClaudeTaskReviewModel, fx.Builtins["claude"])
	}
	if CodexTaskReviewModel != fx.Builtins["codex"] {
		t.Errorf("CodexTaskReviewModel = %q, want fixture builtins.codex %q", CodexTaskReviewModel, fx.Builtins["codex"])
	}
	if !CodexRoleModels[CodexTaskReviewModel] {
		t.Errorf("CodexTaskReviewModel %q is not a Codex role model", CodexTaskReviewModel)
	}
}
