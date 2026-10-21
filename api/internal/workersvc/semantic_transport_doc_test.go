package workersvc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The semantic coverage transport limits are a documented activation precondition (PRD
// #1230, #1231). This pins the five sentences verbatim in the three documents that carry
// them, and the ADR 2149 amendment heading. The files live outside the Go module, so go's
// test cache cannot see an edit to them: run with -count=1.
func TestSemanticTransportLimitsDocumented(t *testing.T) {
	sentences := []string{
		"Codex currently lacks an enforceable token cap.",
		"Claude's output cap alone does not prove a conservative full-call bound.",
		"Complete large sources can exceed the 40,000-token allowance.",
		"Unsupported or unbounded transports and inadmissibly large inputs yield unavailable without a model call.",
		"#1231 must establish enforceable bounds before activation.",
	}
	read := func(rel string) string {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join("..", "..", "..", rel)) //nolint:gosec // G304: fixed repo-relative doc paths from a literal list.
		if err != nil {
			t.Fatalf("%s unreadable: %v", rel, err)
		}
		return string(raw)
	}
	for _, rel := range []string{"adr/1225-structural-completion-interlock-invariants.md", "docs/cross-check.md", "docs/autopilot.md"} {
		doc := read(rel)
		for _, sentence := range sentences {
			if !strings.Contains(doc, sentence) {
				t.Errorf("%s lacks the sentence %q", rel, sentence)
			}
		}
	}
	const heading = "#1230 coverage-park and semantic digest amendment (2026-10-20)"
	if !strings.Contains(read("adr/2149-cross-check.md"), heading) {
		t.Errorf("adr/2149-cross-check.md lacks the heading %q", heading)
	}
	for _, rel := range []string{"adr/1225-structural-completion-interlock-invariants.md", "prds/1230-semantic-completion-contract.md"} {
		for _, sentence := range []string{
			"Maximum-size owner semantic transport is not production-readiness proof.",
			"#1231 must verify maximum-size owner reads and decisions against production transport deadlines before activation.",
		} {
			if !strings.Contains(read(rel), sentence) {
				t.Errorf("%s lacks the sentence %q", rel, sentence)
			}
		}
	}
}
