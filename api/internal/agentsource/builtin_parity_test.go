package agentsource

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/agenttmpl"
)

// PRD #1849: the builtins are verbatim copies of the upstream product-agents
// files, so an agent-source sync of the same file must yield the same
// description, model and body as the shipped builtin. A mismatch shows a
// synced row as changed forever (issue #1851 docs audit: tester's quoted
// description).
func TestSyncParseMatchesShippedBuiltins(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "agenttmpl", "builtins", "*.md"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no builtin files found: %v", err)
	}
	for _, path := range files {
		name := strings.TrimSuffix(filepath.Base(path), ".md")
		if name == "lead" {
			continue
		}
		raw, err := os.ReadFile(path) //nolint:gosec // G304: a fixed test glob under this repo.
		if err != nil {
			t.Fatal(err)
		}
		got := ParseFile(raw, name)
		if !got.OK {
			t.Errorf("%s: sync parser rejected the shipped file: %s", name, got.Skip)
			continue
		}
		want, _ := agenttmpl.BuiltinByName(name)
		if got.Role.Description != want.Description {
			t.Errorf("%s: description differs\n sync:    %q\n shipped: %q", name, got.Role.Description, want.Description)
		}
		if got.Role.Model != want.Model {
			t.Errorf("%s: model differs: sync %q, shipped %q", name, got.Role.Model, want.Model)
		}
		if got.Role.PromptBody != want.PromptBody {
			t.Errorf("%s: prompt body differs", name)
		}
	}
}
