package agenttmpl

import "testing"

func TestResolveCrossCheckPins(t *testing.T) {
	ptr := func(v string) *string { return &v }
	for _, tc := range []struct {
		name, harness                         string
		model, effort, workerModel, template  *string
		wantModel                             *string
		wantEffort, modelSource, effortSource string
	}{
		{"acceptance", "codex", ptr("gpt-6-astra"), ptr("xhigh"), ptr("gpt-6-sol"), nil, ptr("gpt-6-astra"), "xhigh", "pin", "pin"},
		{"default", "codex", nil, nil, ptr("gpt-6-sol"), nil, ptr("gpt-6-sol"), "high", "worker default", "cross-check default"},
		{"worker low does not lower checker", "codex", nil, nil, ptr("gpt-6-astra"), nil, ptr("gpt-6-astra"), "high", "worker default", "cross-check default"},
		{"product", "codex", nil, nil, nil, ptr("opus"), ptr("gpt-6.1-sol"), "high", "worker default", "cross-check default"},
		{"independent effort", "codex", nil, ptr("xhigh"), nil, nil, ptr("gpt-6.1-sol"), "xhigh", "worker default", "pin"},
		{"codex low pin kept", "codex", nil, ptr("low"), nil, nil, ptr("gpt-6.1-sol"), "low", "worker default", "pin"},
		{"claude template", "claude", nil, nil, nil, ptr("sonnet"), ptr("sonnet"), "high", "worker default", "cross-check default"},
		{"claude sdk", "claude", nil, nil, nil, nil, nil, "high", "worker default", "cross-check default"},
		{"claude lane", "claude", nil, nil, ptr("haiku"), ptr("sonnet"), ptr("haiku"), "high", "worker default", "cross-check default"},
		{"claude medium pin kept", "claude", ptr("opus"), ptr("medium"), ptr("haiku"), nil, ptr("opus"), "medium", "pin", "pin"},
		{"claude ignores codex template", "claude", nil, nil, nil, ptr("gpt-6.1-sol"), nil, "high", "worker default", "cross-check default"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolveCrossCheck(tc.harness, tc.model, tc.effort, tc.workerModel, tc.template)
			if (got.Model == nil) != (tc.wantModel == nil) || (got.Model != nil && *got.Model != *tc.wantModel) ||
				got.Effort != tc.wantEffort || got.ModelSource != tc.modelSource || got.EffortSource != tc.effortSource {
				t.Fatalf("resolution = %+v", got)
			}
		})
	}
}

func TestCrossCheckFamilyValidation(t *testing.T) {
	for _, tc := range []struct {
		harness, model string
		valid          bool
	}{
		{"codex", "gpt-6-astra", true}, {"codex", "custom-root", true}, {"codex", "sonnet", false},
		{"claude", "sonnet", true}, {"claude", "claude-custom", true}, {"claude", "gpt-6-sol", false},
		{"claude", "a\u202eb", false}, {"codex", "a\nb", false}, {"codex", "two words", false},
	} {
		_, err := ValidateFamilyModel(tc.harness, tc.model)
		if (err == nil) != tc.valid {
			t.Errorf("%s %q: %v", tc.harness, tc.model, err)
		}
	}
	for _, harness := range []string{"claude", "codex"} {
		for _, effort := range EffortLevels {
			_, err := ValidateFamilyEffort(harness, effort)
			if err != nil {
				t.Errorf("%s %s: %v", harness, effort, err)
			}
		}
	}
}

func TestValidateClaudeLaneModel(t *testing.T) {
	for _, tc := range []struct {
		in, want string
		wantErr  bool
	}{
		{"gpt-5.6-sol", "", true},
		{"gpt-6-astra", "", true},
		{"gpt-6-luna", "", true},
		{"gpt-6-sol", "", true},
		{"gpt-6.1-sol", "", true},
		{" gpt-6-luna ", "", true},
		{"opus", "opus", false},
		{"claude-opus-4-8", "claude-opus-4-8", false},
		{" sonnet ", "sonnet", false},
		{"", "", false},
		{"   ", "", false},
		{"two words", "", true},
	} {
		got, err := ValidateClaudeLaneModel(tc.in)
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("ValidateClaudeLaneModel(%q) = %q, %v; want %q, err=%v", tc.in, got, err, tc.want, tc.wantErr)
		}
	}
}

func TestCrossCheckDefaultEffortIsALevel(t *testing.T) {
	for _, harness := range []string{"claude", "codex"} {
		if got, err := ValidateFamilyEffort(harness, CrossCheckDefaultEffort); err != nil || got != CrossCheckDefaultEffort {
			t.Fatalf("%s: %q %v", harness, got, err)
		}
	}
	if UziDefaultEffort != "medium" || UziDefaultCodexEffort != "medium" {
		t.Fatal("ordinary worker effort defaults changed")
	}
}
