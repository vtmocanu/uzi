package agenttmpl

import (
	"slices"
	"testing"
)

// TestBuiltinsCarryProductToolDelta pins D5 of PRD #1849 on the shipped side: the
// embedded file mirrors upstream (no forge tools), and the builtin the product
// seeds carries every delta tool on top of the file's allowlist.
func TestBuiltinsCarryProductToolDelta(t *testing.T) {
	for name, extra := range productToolDelta {
		def, ok := BuiltinByName(name)
		if !ok {
			t.Fatalf("productToolDelta names %q, which is not a builtin", name)
		}
		raw, err := builtinFS.ReadFile("builtins/" + name + ".md")
		if err != nil {
			t.Fatalf("read embedded builtin: %v", err)
		}
		file, err := parse(raw)
		if err != nil {
			t.Fatalf("parse embedded builtin: %v", err)
		}
		if len(file.Tools) == 0 {
			t.Fatalf("%s: the file inherits all tools, so the delta would never apply; drop it from productToolDelta", name)
		}
		for _, tool := range extra {
			if slices.Contains(file.Tools, tool) {
				t.Errorf("%s: file already lists %q; the file must mirror upstream, which cannot name it", name, tool)
			}
			if !slices.Contains(def.Tools, tool) {
				t.Errorf("%s: shipped builtin lacks delta tool %q", name, tool)
			}
		}
		if !slices.Equal(def.Tools[:len(file.Tools)], file.Tools) {
			t.Errorf("%s: delta must append to the file's allowlist, not reorder it: got %v", name, def.Tools)
		}
	}
}

func TestWithProductTools(t *testing.T) {
	t.Run("appends to an allowlist, deduplicated", func(t *testing.T) {
		in := Definition{Name: "fact-checker", Tools: []string{"Read", "mcp__forge__get_issue"}}
		got := WithProductTools(in)
		if got.Tools[0] != "Read" || got.Tools[1] != "mcp__forge__get_issue" {
			t.Fatalf("existing order changed: %v", got.Tools)
		}
		for _, tool := range productToolDelta["fact-checker"] {
			n := 0
			for _, x := range got.Tools {
				if x == tool {
					n++
				}
			}
			if n != 1 {
				t.Errorf("%q appears %d times, want 1: %v", tool, n, got.Tools)
			}
		}
		if len(in.Tools) != 2 {
			t.Errorf("input definition was modified: %v", in.Tools)
		}
	})
	t.Run("inherit-all stays inherit-all", func(t *testing.T) {
		if got := WithProductTools(Definition{Name: "fact-checker"}); len(got.Tools) != 0 {
			t.Errorf("empty (inherit-all) tools became an allowlist: %v", got.Tools)
		}
	})
	t.Run("other roles unchanged", func(t *testing.T) {
		in := Definition{Name: "reviewer", Tools: []string{"Read"}}
		if got := WithProductTools(in); !slices.Equal(got.Tools, in.Tools) {
			t.Errorf("reviewer tools changed: %v", got.Tools)
		}
	})
}
