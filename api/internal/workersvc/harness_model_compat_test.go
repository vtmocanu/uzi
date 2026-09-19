package workersvc

import "testing"

// TestHarnessModelCompatible pins the D6 model-vocabulary rule (PRD #1429 M2) as a pure,
// DB-free unit: a Claude alias must never ride a Codex claim, a Codex-only model must never
// ride a Claude claim, and an empty (inherit) model is always compatible with either harness.
func TestHarnessModelCompatible(t *testing.T) {
	cases := []struct {
		name  string
		h     Harness
		model string
		want  bool
	}{
		{"codex rejects a claude alias", HarnessCodex, "sonnet", false},
		{"codex accepts gpt-6-astra", HarnessCodex, "gpt-6-astra", true},
		{"codex accepts gpt-5.6-sol", HarnessCodex, "gpt-5.6-sol", true},
		{"claude rejects a codex-only model", HarnessClaude, "gpt-6-astra", false},
		{"claude rejects the other codex-only model", HarnessClaude, "gpt-5.6-sol", false},
		{"claude accepts a claude alias", HarnessClaude, "sonnet", true},
		{"claude accepts an arbitrary custom id", HarnessClaude, "claude-opus-4-8", true},
		{"codex rejects an unknown/custom id", HarnessCodex, "some-unknown-model", false},
		{"empty model is inherit, compatible with codex", HarnessCodex, "", true},
		{"empty model is inherit, compatible with claude", HarnessClaude, "", true},
		{"whitespace-only model is inherit", HarnessCodex, "   ", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := harnessModelCompatible(c.h, c.model); got != c.want {
				t.Fatalf("harnessModelCompatible(%q, %q) = %v, want %v", c.h, c.model, got, c.want)
			}
		})
	}
}
