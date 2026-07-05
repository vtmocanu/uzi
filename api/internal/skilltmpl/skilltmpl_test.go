package skilltmpl

import (
	"strings"
	"testing"
)

// TestBuiltinsParseAndValidate is the standing guarantee that replaces the
// agenttmpl golden byte-match: every embedded builtin parses, and its parsed
// fields satisfy the constraints delivery relies on (name regex, single-line
// non-empty description, non-empty body). init() already panics on a parse
// failure, but this pins the validity rules explicitly and gives a readable
// failure per skill.
func TestBuiltinsParseAndValidate(t *testing.T) {
	got := Builtins()
	if len(got) == 0 {
		t.Fatal("no builtin skills embedded")
	}
	for _, d := range got {
		t.Run(d.Name, func(t *testing.T) {
			if !NameRe.MatchString(d.Name) {
				t.Errorf("name %q does not match %s", d.Name, NameRe)
			}
			if strings.TrimSpace(d.Description) == "" {
				t.Error("description is empty")
			}
			if strings.ContainsAny(d.Description, "\r\n") {
				t.Error("description must be a single line")
			}
			if strings.TrimSpace(d.Body) == "" {
				t.Error("body is empty")
			}
		})
	}
}

// TestBuiltinNamesUnique guards against two builtin dirs resolving to the same
// name (uq_skills_shared_name would reject the second at seed time otherwise).
func TestBuiltinNamesUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, d := range Builtins() {
		if seen[d.Name] {
			t.Errorf("duplicate builtin name %q", d.Name)
		}
		seen[d.Name] = true
	}
}

// TestexampleCicdBuiltinPresent pins that the first builtin skill ships.
func TestexampleCicdBuiltinPresent(t *testing.T) {
	d, ok := BuiltinByName("ci-cd-norms")
	if !ok {
		t.Fatal("ci-cd-norms builtin not found")
	}
	if !strings.Contains(d.Body, "argo-apps") {
		t.Error("ci-cd-norms body missing expected content")
	}
}

func TestParseRejectsMalformed(t *testing.T) {
	cases := map[string]string{
		"no frontmatter":     "# just a body\n",
		"unterminated":       "---\nname: x\ndescription: y\n",
		"unknown key":        "---\nname: x\ndescription: y\nallowed-tools: Bash\n---\n\nbody\n",
		"bad name":           "---\nname: Not_Kebab\ndescription: y\n---\n\nbody\n",
		"empty description":  "---\nname: x\ndescription: \n---\n\nbody\n",
		"empty body":         "---\nname: x\ndescription: y\n---\n\n",
		"no blank line":      "---\nname: x\ndescription: y\n---\nbody\n",
	}
	for name, raw := range cases {
		if _, err := Parse([]byte(raw)); err == nil {
			t.Errorf("%s: expected parse error", name)
		}
	}
}

func TestParseHappyPath(t *testing.T) {
	d, err := Parse([]byte("---\nname: my-skill\ndescription: does a thing.\n---\n\n# Body\n\ntext\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if d.Name != "my-skill" || d.Description != "does a thing." {
		t.Errorf("frontmatter not parsed: %+v", d)
	}
	if d.Body != "# Body\n\ntext\n" {
		t.Errorf("body = %q", d.Body)
	}
}
