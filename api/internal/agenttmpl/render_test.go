package agenttmpl

import (
	"strings"
	"testing"
)

// builtinNames is the exact set of roles the product ships and PRD #4 depends on
// existing. The lead orchestrator template (PRD #17) is the eighth. This is the
// single source of truth: builtins are the embedded builtins/*.md files, no
// longer mirrored from .claude/agents/.
var builtinNames = []string{
	"auditor", "coder", "documenter", "fact-checker",
	"lead", "reviewer", "spec-keeper", "tester",
}

func TestBuiltinsSetIsExactlyEight(t *testing.T) {
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

// TestBuiltinsParseAndValid replaces the removed golden byte-match tests
// (TestRenderBuiltinsByteMatch / TestEmbeddedCopiesMatchRepo, which pinned the
// builtins to .claude/agents/*.md). Now that builtins/ is the single source of
// truth, the guarantee is that every embedded file parses and carries sane
// frontmatter: non-empty name/description, unique names, a non-empty body, and
// a well-formed model alias when one is set.
func TestBuiltinsParseAndValid(t *testing.T) {
	seen := make(map[string]bool)
	for _, def := range Builtins() {
		if def.Name == "" {
			t.Errorf("builtin has empty name: %+v", def)
		}
		if def.Description == "" {
			t.Errorf("builtin %q has empty description", def.Name)
		}
		if strings.TrimSpace(def.PromptBody) == "" {
			t.Errorf("builtin %q has empty prompt body", def.Name)
		}
		if seen[def.Name] {
			t.Errorf("duplicate builtin name %q", def.Name)
		}
		seen[def.Name] = true
		// Validate the model against the shared, authoritative rule (no separate
		// test-local copy that could drift from the server validator).
		if _, err := ValidateModel(def.Model); err != nil {
			t.Errorf("builtin %q has an invalid model %q: %v", def.Name, def.Model, err)
		}
	}
}

// TestBuiltinsRoundTripRender pins the parse->Render round-trip byte-for-byte
// against the embedded source, replacing the old golden test's byte-match (which
// compared against .claude/agents/). Render of the parsed definition must
// reproduce the on-disk builtin exactly, so the renderer stays the canonical
// serializer for what boot-seeds into the database.
func TestBuiltinsRoundTripRender(t *testing.T) {
	for _, name := range builtinNames {
		t.Run(name, func(t *testing.T) {
			def, ok := BuiltinByName(name)
			if !ok {
				t.Fatalf("builtin %q not found", name)
			}
			want, err := builtinFS.ReadFile("builtins/" + name + ".md")
			if err != nil {
				t.Fatalf("read embedded builtin: %v", err)
			}
			if got := Render(def); string(got) != string(want) {
				t.Errorf("Render(%q) does not round-trip the embedded file\n--- got ---\n%s\n--- want ---\n%s",
					name, got, want)
			}
		})
	}
}

// TestLeadBuiltin pins the shipped lead orchestrator template: it exists, runs
// on opus, and inherits all tools (no tools line — the lead needs the full
// toolset, same null-tools contract the coder uses). The worker routes it to the
// main thread by name convention; the agent side asserts the name matches its
// LEAD_NAME_RE.
func TestLeadBuiltin(t *testing.T) {
	lead, ok := BuiltinByName("lead")
	if !ok {
		t.Fatal("lead builtin missing")
	}
	if lead.Model != "opus" {
		t.Errorf("lead.Model = %q, want %q", lead.Model, "opus")
	}
	if len(lead.Tools) != 0 {
		t.Errorf("lead.Tools = %v, want empty (inherit all)", lead.Tools)
	}
	if strings.Contains(string(Render(lead)), "\ntools:") {
		t.Error("rendered lead must not contain a tools line")
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
