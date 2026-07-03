package agenttmpl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// builtinNames is the exact set of roles PRD #4 depends on existing.
var builtinNames = []string{
	"auditor", "coder", "documenter", "fact-checker",
	"reviewer", "spec-keeper", "tester",
}

// repoAgentPath resolves this repo's checked-in .claude/agents/<name>.md,
// relative to this package directory (api/internal/agenttmpl -> repo root).
func repoAgentPath(name string) string {
	return filepath.Join("..", "..", "..", ".claude", "agents", name+".md")
}

// TestRenderBuiltinsByteMatch is the hard constraint: each rendered builtin must
// be byte-identical to the checked-in .claude/agents/<name>.md, so PRD #4 can
// write the render output straight into an agent workspace.
func TestRenderBuiltinsByteMatch(t *testing.T) {
	for _, name := range builtinNames {
		t.Run(name, func(t *testing.T) {
			def, ok := BuiltinByName(name)
			if !ok {
				t.Fatalf("builtin %q not found", name)
			}
			want, err := os.ReadFile(repoAgentPath(name))
			if err != nil {
				t.Fatalf("read checked-in agent file: %v", err)
			}
			got := Render(def)
			if string(got) != string(want) {
				t.Errorf("rendered %q does not byte-match .claude/agents/%s.md\n--- got ---\n%s\n--- want ---\n%s",
					name, name, got, want)
			}
		})
	}
}

// TestEmbeddedCopiesMatchRepo guards against the embedded builtins/*.md copies
// drifting from the checked-in originals. If .claude/agents/<name>.md changes,
// resync api/internal/agenttmpl/builtins/<name>.md.
func TestEmbeddedCopiesMatchRepo(t *testing.T) {
	for _, name := range builtinNames {
		t.Run(name, func(t *testing.T) {
			embedded, err := builtinFS.ReadFile("builtins/" + name + ".md")
			if err != nil {
				t.Fatalf("read embedded copy: %v", err)
			}
			repo, err := os.ReadFile(repoAgentPath(name))
			if err != nil {
				t.Fatalf("read checked-in original: %v", err)
			}
			if string(embedded) != string(repo) {
				t.Errorf("embedded builtins/%s.md drifted from .claude/agents/%s.md; resync the copy", name, name)
			}
		})
	}
}

func TestBuiltinsSetIsExactlySeven(t *testing.T) {
	got := Builtins()
	if len(got) != len(builtinNames) {
		t.Fatalf("got %d builtins, want %d", len(got), len(builtinNames))
	}
	for i, name := range builtinNames {
		if got[i].Name != name {
			t.Errorf("builtin[%d] = %q, want %q (should be sorted)", i, got[i].Name, name)
		}
	}
}

// TestCoderInheritsAllTools pins the confirmed gotcha: coder.md carries no tools
// line, so its builtin must inherit all tools (empty Tools) while still setting
// a model.
func TestCoderInheritsAllTools(t *testing.T) {
	coder, ok := BuiltinByName("coder")
	if !ok {
		t.Fatal("coder builtin missing")
	}
	if len(coder.Tools) != 0 {
		t.Errorf("coder.Tools = %v, want empty (inherit all)", coder.Tools)
	}
	if coder.Model != "opus" {
		t.Errorf("coder.Model = %q, want %q", coder.Model, "opus")
	}
	rendered := string(Render(coder))
	if strings.Contains(rendered, "\ntools:") {
		t.Error("rendered coder must not contain a tools line")
	}
	if !strings.Contains(rendered, "\nmodel: opus\n") {
		t.Error("rendered coder must contain the model line")
	}
}

// TestRenderFieldOrderAndOmission pins the fixed field order and the omit-when-
// empty behaviour for a synthetic definition covering all four fields.
func TestRenderFieldOrderAndOmission(t *testing.T) {
	full := Render(Definition{
		Name:        "example",
		Description: "an example.",
		Model:       "sonnet",
		Tools:       []string{"Bash", "Read", "Grep"},
		PromptBody:  "Body.\n",
	})
	want := "---\nname: example\ndescription: an example.\ntools: Bash, Read, Grep\nmodel: sonnet\n---\n\nBody.\n"
	if string(full) != want {
		t.Errorf("full render mismatch\n got: %q\nwant: %q", full, want)
	}

	inherit := Render(Definition{
		Name:        "bare",
		Description: "no tools, no model.",
		PromptBody:  "Body.\n",
	})
	wantInherit := "---\nname: bare\ndescription: no tools, no model.\n---\n\nBody.\n"
	if string(inherit) != wantInherit {
		t.Errorf("inherit render mismatch\n got: %q\nwant: %q", inherit, wantInherit)
	}
}
