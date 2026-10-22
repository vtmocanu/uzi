package agenttmpl

import (
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"testing"
)

const (
	codexModelsFixture = "../../../fixtures/codex-models/models.json"
	judgeModelFixture  = "../../../fixtures/model-settings/judge.json"
)

// The fixtures live above api/, so a fixture-only edit is not in the package's
// test cache key: run this package with -count=1 after editing them.

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestCodexModelSetsMatchFixture(t *testing.T) {
	raw, err := os.ReadFile(codexModelsFixture)
	if err != nil {
		t.Fatal(err)
	}
	var fx struct {
		Curated []string `json:"curated"`
		Role    []string `json:"role"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	sort.Strings(fx.Curated)
	sort.Strings(fx.Role)
	if got := keysOf(CuratedCodexModels); !reflect.DeepEqual(got, fx.Curated) {
		t.Errorf("CuratedCodexModels = %v, want fixtures/codex-models/models.json curated %v", got, fx.Curated)
	}
	if got := keysOf(CodexRoleModels); !reflect.DeepEqual(got, fx.Role) {
		t.Errorf("CodexRoleModels = %v, want fixtures/codex-models/models.json role %v", got, fx.Role)
	}
}

type judgeFixture struct {
	Builtins map[string]string `json:"builtins"`
	Cases    []struct {
		Name     string `json:"name"`
		Harness  string `json:"harness"`
		User     string `json:"user"`
		Instance string `json:"instance"`
		Want     struct {
			Model         string   `json:"model"`
			Source        string   `json:"source"`
			DefaultModel  string   `json:"default_model"`
			DefaultSource string   `json:"default_source"`
			Dropped       []string `json:"dropped"`
		} `json:"want"`
	} `json:"cases"`
}

func loadJudgeFixture(t *testing.T) judgeFixture {
	t.Helper()
	raw, err := os.ReadFile(judgeModelFixture)
	if err != nil {
		t.Fatal(err)
	}
	var fx judgeFixture
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	return fx
}

func TestResolveJudgeModelFixtureCases(t *testing.T) {
	fx := loadJudgeFixture(t)
	if len(fx.Cases) == 0 {
		t.Fatal("fixtures/model-settings/judge.json has no cases")
	}
	for _, tc := range fx.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			got := ResolveJudgeModel(tc.Harness, tc.User, tc.Instance)
			if got.Model != tc.Want.Model || got.Source != tc.Want.Source ||
				got.DefaultModel != tc.Want.DefaultModel || got.DefaultSource != tc.Want.DefaultSource {
				t.Errorf("got %+v, want %+v", got, tc.Want)
			}
			if len(got.Dropped) != len(tc.Want.Dropped) ||
				(len(got.Dropped) > 0 && !reflect.DeepEqual(got.Dropped, tc.Want.Dropped)) {
				t.Errorf("Dropped = %v, want %v", got.Dropped, tc.Want.Dropped)
			}
			if tc.Harness == "codex" && !CuratedCodexModels[got.Model] {
				t.Errorf("codex Model %q is not curated", got.Model)
			}
			if got.Model == "" {
				t.Errorf("Model is empty although the built-in is accepted")
			}
		})
	}
}

func TestJudgeBuiltinsMatchFixture(t *testing.T) {
	fx := loadJudgeFixture(t)
	if DefaultClaudeJudgeModel != fx.Builtins["claude"] {
		t.Errorf("DefaultClaudeJudgeModel = %q, want fixtures/model-settings/judge.json builtins.claude %q", DefaultClaudeJudgeModel, fx.Builtins["claude"])
	}
	if DefaultCodexJudgeModel != fx.Builtins["codex"] {
		t.Errorf("DefaultCodexJudgeModel = %q, want fixtures/model-settings/judge.json builtins.codex %q", DefaultCodexJudgeModel, fx.Builtins["codex"])
	}
}
