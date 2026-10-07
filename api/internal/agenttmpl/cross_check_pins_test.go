package agenttmpl

import "testing"

func TestResolveCrossCheckPins(t *testing.T) {
	ptr := func(v string) *string { return &v }
	for _, tc := range []struct {
		name, harness                                      string
		model, effort, workerModel, workerEffort, template *string
		wantModel                                          *string
		wantEffort, modelSource, effortSource              string
	}{
		{"acceptance", "codex", ptr("gpt-6-astra"), ptr("xhigh"), ptr("gpt-6-sol"), ptr("high"), nil, ptr("gpt-6-astra"), "xhigh", "pin", "pin"},
		{"default", "codex", nil, nil, ptr("gpt-6-sol"), ptr("high"), nil, ptr("gpt-6-sol"), "high", "worker default", "worker default"},
		{"later lane", "codex", nil, nil, ptr("gpt-6-astra"), ptr("low"), nil, ptr("gpt-6-astra"), "low", "worker default", "worker default"},
		{"product", "codex", nil, nil, nil, nil, ptr("opus"), ptr("gpt-6.1-sol"), "medium", "worker default", "worker default"},
		{"independent effort", "codex", nil, ptr("xhigh"), nil, ptr("low"), nil, ptr("gpt-6.1-sol"), "xhigh", "worker default", "pin"},
		{"claude template", "claude", nil, nil, nil, nil, ptr("sonnet"), ptr("sonnet"), "medium", "worker default", "worker default"},
		{"claude sdk", "claude", nil, nil, nil, nil, nil, nil, "medium", "worker default", "worker default"},
		{"claude lane", "claude", nil, nil, ptr("haiku"), ptr("max"), ptr("sonnet"), ptr("haiku"), "max", "worker default", "worker default"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolveCrossCheck(tc.harness, tc.model, tc.effort, tc.workerModel, tc.workerEffort, tc.template)
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
