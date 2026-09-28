package agenttmpl

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The upstream publisher double-quotes tester's description (it contains ": ").
// It used to be stored with the literal quotes, which the Agents UI showed and
// which made an agent-source sync of the same file read as a change.
func TestBuiltinDescriptionsCarryNoYAMLQuotes(t *testing.T) {
	for _, d := range Builtins() {
		if strings.HasPrefix(d.Description, `"`) || strings.HasPrefix(d.Description, "'") {
			t.Errorf("%s: description keeps its YAML quotes: %q", d.Name, d.Description)
		}
	}
	tester, _ := BuiltinByName("tester")
	if !strings.Contains(tester.Description, ": ") {
		t.Fatalf("fixture: tester's description no longer needs quoting (%q); pick another quoted builtin", tester.Description)
	}
}

// Render also serves admin-authored templates (the /rendered export), whose
// descriptions can hold quotes, backslashes or colons. The frontmatter must be
// valid YAML that reads back as the same description, and parse must agree.
func TestRenderedDescriptionIsValidYAML(t *testing.T) {
	for _, v := range []string{
		"plain words", "has: colon space", "ends with:", "- leading dash", "tab #comment",
		`He said "go": now`, `back\slash: yes`, `"starts quoted`, "it's fine", "'single' start",
	} {
		raw := Render(Definition{Name: "x", Description: v, PromptBody: "body\n"})
		fm := strings.SplitN(strings.TrimPrefix(string(raw), "---\n"), "\n---\n", 2)[0]
		var got struct {
			Description string `yaml:"description"`
		}
		if err := yaml.Unmarshal([]byte(fm), &got); err != nil {
			t.Errorf("%q: rendered frontmatter is not YAML: %v\n%s", v, err, fm)
			continue
		}
		if got.Description != v {
			t.Errorf("%q: YAML reads back %q", v, got.Description)
		}
		if d, err := parse(raw); err != nil || d.Description != v {
			t.Errorf("%q: parse gives %q (err %v)", v, d.Description, err)
		}
	}
	if quoteScalar("plain words") != "plain words" {
		t.Error("a plain value must stay bare")
	}
}
