// Package skilltmpl holds the Go-embedded builtin agent skills and the SKILL.md
// frontmatter parser. A skill is a named Markdown playbook: its name and
// one-line description sit cheaply in an agent's context, and its body loads on
// demand (progressive disclosure).
//
// Unlike agenttmpl, there is no `.claude/skills` mirror and no golden byte-match
// test: `builtins/<name>/SKILL.md` is the single source of truth (a product
// skill under this repo's own `.claude/skills` would masquerade as — and load
// as — dev tooling). The guarantee is instead a set of parse/validity tests over
// the embedded files (see skilltmpl_test.go).
package skilltmpl

import (
	"embed"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"
)

// NameRe is the kebab-case constraint on a skill name: a lowercase letter or
// digit, then up to 63 more of the same or hyphens. It is the skill identity
// (the synthesized `skills/<name>/SKILL.md` directory and the SDK enable-list
// key), so it is validated on create and immutable after. The same regexp gates
// server-stored skills (handler) and repo-borne skills (worker).
var NameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// Definition is the parsed shape of a builtin skill: exactly the fields stored
// in the skills table. The SKILL.md frontmatter (name, description) is
// synthesized at delivery from these columns, so Body is the markdown below the
// frontmatter only.
type Definition struct {
	Name        string
	Description string
	Body        string
}

//go:embed builtins/*/SKILL.md
var builtinFS embed.FS

// builtins is the parsed set of builtin skills, sorted by name. Parsing happens
// once at package init; a parse failure is a build-time-embedded-file bug, so it
// panics rather than being silently skipped.
var builtins []Definition

func init() {
	entries, err := fs.ReadDir(builtinFS, "builtins")
	if err != nil {
		panic(fmt.Sprintf("skilltmpl: read builtins dir: %v", err))
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		raw, err := builtinFS.ReadFile(path.Join("builtins", e.Name(), "SKILL.md"))
		if err != nil {
			panic(fmt.Sprintf("skilltmpl: read builtin %s: %v", e.Name(), err))
		}
		def, err := Parse(raw)
		if err != nil {
			panic(fmt.Sprintf("skilltmpl: parse builtin %s: %v", e.Name(), err))
		}
		if def.Name != e.Name() {
			panic(fmt.Sprintf("skilltmpl: builtin dir %q does not match frontmatter name %q", e.Name(), def.Name))
		}
		builtins = append(builtins, def)
	}
	sort.Slice(builtins, func(i, j int) bool { return builtins[i].Name < builtins[j].Name })
	// Must run AFTER the parse loop above — it reads `builtins` (allocations.go).
	validateDefaultAllocations()
}

// Builtins returns the embedded builtin skills, sorted by name. The returned
// slice is a copy so callers cannot mutate the package state.
func Builtins() []Definition {
	out := make([]Definition, len(builtins))
	copy(out, builtins)
	return out
}

// BuiltinByName returns the builtin skill with the given name.
func BuiltinByName(name string) (Definition, bool) {
	for _, d := range builtins {
		if d.Name == name {
			return d, true
		}
	}
	return Definition{}, false
}

// Parse reads a SKILL.md file into a Definition. It accepts exactly the
// name/description frontmatter keys (any other key is an authoring error, so it
// is rejected rather than silently dropped — the worker's repo-skill path is the
// one that strips unknown keys, from untrusted input). It is only ever fed the
// embedded builtin files.
func Parse(raw []byte) (Definition, error) {
	const delim = "---\n"
	content := string(raw)
	if !strings.HasPrefix(content, delim) {
		return Definition{}, fmt.Errorf("missing opening frontmatter delimiter")
	}
	rest := content[len(delim):]
	idx := strings.Index(rest, "\n"+delim)
	if idx < 0 {
		return Definition{}, fmt.Errorf("missing closing frontmatter delimiter")
	}
	frontmatter := rest[:idx]
	afterClose := rest[idx+len("\n"+delim):]
	// A single blank line must separate the frontmatter from the body.
	if !strings.HasPrefix(afterClose, "\n") {
		return Definition{}, fmt.Errorf("missing blank line after frontmatter")
	}
	body := afterClose[len("\n"):]

	var d Definition
	d.Body = body
	for _, line := range strings.Split(frontmatter, "\n") {
		key, val, ok := strings.Cut(line, ": ")
		if !ok {
			return Definition{}, fmt.Errorf("malformed frontmatter line: %q", line)
		}
		switch key {
		case "name":
			d.Name = val
		case "description":
			d.Description = val
		default:
			return Definition{}, fmt.Errorf("unknown frontmatter key: %q", key)
		}
	}
	if !NameRe.MatchString(d.Name) {
		return Definition{}, fmt.Errorf("name %q is not valid kebab-case", d.Name)
	}
	if strings.TrimSpace(d.Description) == "" {
		return Definition{}, fmt.Errorf("frontmatter missing description")
	}
	if strings.ContainsAny(d.Description, "\r\n") {
		return Definition{}, fmt.Errorf("description must be a single line")
	}
	if strings.TrimSpace(d.Body) == "" {
		return Definition{}, fmt.Errorf("empty skill body")
	}
	return d, nil
}
