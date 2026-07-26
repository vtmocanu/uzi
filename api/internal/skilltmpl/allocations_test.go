package skilltmpl_test

import (
	"testing"

	"gitlab.example.com/vtmocanu/uzi/api/internal/agenttmpl"
	"gitlab.example.com/vtmocanu/uzi/api/internal/skilltmpl"
)

// The default-allocation map is keyed by skill name and valued by AGENT TEMPLATE
// names, neither of which the compiler checks. The reconciler's zero-row warning
// (PRD #72 Decision 9) catches a template an ADMIN deleted at runtime; these tests
// catch the typo at build time, which is the half that should never reach a boot
// log. Package-external (skilltmpl_test) so importing agenttmpl cannot introduce a
// dependency into the shipped package.

func TestDefaultAllocationKeysAreShippedBuiltinSkills(t *testing.T) {
	shipped := map[string]bool{}
	for _, def := range skilltmpl.Builtins() {
		shipped[def.Name] = true
	}
	for _, name := range skilltmpl.DefaultAllocationNames() {
		if !shipped[name] {
			t.Errorf("defaultAllocations names %q, which is not a shipped builtin skill "+
				"(a renamed or removed skill leaves a map entry that can never fire)", name)
		}
	}
}

func TestDefaultAllocationTargetsAreShippedBuiltinTemplates(t *testing.T) {
	// A target naming no shipped template would seed nothing on a fresh instance
	// and only ever surface as a boot warning.
	for _, name := range skilltmpl.DefaultAllocationNames() {
		targets := skilltmpl.DefaultAllocationsFor(name)
		if len(targets) == 0 {
			t.Errorf("skill %q has an entry with no targets; remove the entry instead", name)
		}
		for _, tmpl := range targets {
			if _, ok := agenttmpl.BuiltinByName(tmpl); !ok {
				t.Errorf("skill %q defaults to agent template %q, which is not a shipped builtin", name, tmpl)
			}
		}
	}
}

func TestDefaultAllocationsForIsACopy(t *testing.T) {
	names := skilltmpl.DefaultAllocationNames()
	if len(names) == 0 {
		t.Skip("no default allocations declared")
	}
	first := skilltmpl.DefaultAllocationsFor(names[0])
	if len(first) == 0 {
		t.Fatalf("%q has no targets", names[0])
	}
	first[0] = "mutated"
	if again := skilltmpl.DefaultAllocationsFor(names[0]); again[0] == "mutated" {
		t.Error("DefaultAllocationsFor must return a copy; a caller mutated package state")
	}
}

func TestDefaultAllocationsForUnknownSkillIsNil(t *testing.T) {
	if got := skilltmpl.DefaultAllocationsFor("no-such-skill"); got != nil {
		t.Errorf("expected nil for an unknown skill; got %v", got)
	}
}
