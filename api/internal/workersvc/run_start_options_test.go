package workersvc

import (
	"errors"
	"strings"
	"testing"
)

// TestRunOverrideModelHonorable pins the run_override predicate (PRD #3250): stricter than
// harnessModelCompatible, which keeps screening schedule pins.
func TestRunOverrideModelHonorable(t *testing.T) {
	tests := []struct {
		harness Harness
		model   string
		want    bool
	}{
		{HarnessClaude, "opus", true},
		{HarnessClaude, "claude-x-1", true},
		{HarnessClaude, "gpt-6-sol", false},
		{HarnessClaude, "gpt-6-luna", false},
		{HarnessClaude, "gpt-6.1-sol", false},
		{HarnessClaude, "", false},
		{HarnessClaude, "   ", false},
		{HarnessClaude, "\t\n", false},
		{HarnessClaude, " opus", false},
		{HarnessClaude, "opus ", false},
		{HarnessClaude, "op us", false},
		{HarnessClaude, "op" + string(rune(0x202e)) + "us", false},
		{HarnessCodex, "gpt-6-sol", true},
		{HarnessCodex, "gpt-5.6-sol", true},
		{HarnessCodex, "gpt-6-astra", true},
		{HarnessCodex, "gpt-6.1-sol", true},
		{HarnessCodex, "gpt-6-luna", false},
		{HarnessCodex, "opus", false},
		{HarnessCodex, "gpt-foo", false},
		{HarnessCodex, "", false},
		{HarnessCodex, " gpt-6-sol", false},
		{HarnessCodex, "gpt-6-sol" + string(rune(0xa0)), false},
		{Harness("other"), "opus", false},
	}
	for _, tt := range tests {
		if got := runOverrideModelHonorable(tt.harness, tt.model); got != tt.want {
			t.Errorf("runOverrideModelHonorable(%s, %q) = %v, want %v", tt.harness, tt.model, got, tt.want)
		}
	}
	// Schedule pins keep the soft harnessModelCompatible screen; the two predicates stay distinct.
	if !harnessModelCompatible(HarnessClaude, "opus") || harnessModelCompatible(HarnessCodex, "opus") {
		t.Fatal("harnessModelCompatible changed behaviour")
	}
}

func TestModelIncompatibleErrorText(t *testing.T) {
	codex := &ModelIncompatibleError{Model: "opus", Harness: HarnessCodex}
	want := "opus is not a Codex model, and this run resolved to the codex harness; start it on the Claude harness or choose one of: gpt-5.6-sol, gpt-6-astra, gpt-6-sol, gpt-6.1-sol"
	if got := codex.Error(); got != want {
		t.Errorf("codex text = %q, want %q", got, want)
	}
	claude := &ModelIncompatibleError{Model: "gpt-6-sol", Harness: HarnessClaude}
	if got := claude.Error(); !strings.HasPrefix(got, "gpt-6-sol cannot run on the Claude harness") {
		t.Errorf("claude text = %q", got)
	}
	if !errors.Is(codex, ErrModelIncompatibleWithHarness) {
		t.Error("ModelIncompatibleError must match ErrModelIncompatibleWithHarness")
	}
}
