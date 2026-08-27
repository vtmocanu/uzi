package agenttmpl

import (
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
)

// builtinFS holds the product's builtin agent-template definitions. This
// directory is the single source of truth for builtins: they are versioned in
// git, shipped in the binary via go:embed, and boot-seeded into the database.
// It is independent of this repo's own .claude/agents/ dev-team roster (which
// is free to drift); parse/validity tests, not a byte-match against those
// files, guard these definitions.
//
//go:embed builtins/*.md
var builtinFS embed.FS

// builtins is the parsed set of builtin definitions, sorted by name. Parsing
// happens once at package init; a parse failure is a build-time-embedded-file
// bug, so it panics rather than being silently skipped.
var builtins []Definition

func init() {
	entries, err := fs.ReadDir(builtinFS, "builtins")
	if err != nil {
		panic(fmt.Sprintf("agenttmpl: read builtins dir: %v", err))
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		raw, err := builtinFS.ReadFile(path.Join("builtins", e.Name()))
		if err != nil {
			panic(fmt.Sprintf("agenttmpl: read builtin %s: %v", e.Name(), err))
		}
		def, err := parse(raw)
		if err != nil {
			panic(fmt.Sprintf("agenttmpl: parse builtin %s: %v", e.Name(), err))
		}
		builtins = append(builtins, def)
	}
	sort.Slice(builtins, func(i, j int) bool { return builtins[i].Name < builtins[j].Name })
}

// Builtins returns the embedded builtin definitions, sorted by name. The
// returned slice is a copy so callers cannot mutate the package state.
func Builtins() []Definition {
	out := make([]Definition, len(builtins))
	copy(out, builtins)
	return out
}

// BuiltinByName returns the builtin definition with the given name.
func BuiltinByName(name string) (Definition, bool) {
	for _, d := range builtins {
		if d.Name == name {
			return d, true
		}
	}
	return Definition{}, false
}

// parse reads a Claude Code subagent Markdown file into a Definition. It is the
// inverse of Render for the subset of frontmatter these files use (name,
// version, description, tools, model). It is only ever fed the embedded builtin
// files; user templates never go through it (they arrive as structured JSON). The
// round-trip parse->Render is pinned byte-for-byte by the parse/validity test.
func parse(raw []byte) (Definition, error) {
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
	d.PromptBody = body
	for _, line := range strings.Split(frontmatter, "\n") {
		key, val, ok := strings.Cut(line, ": ")
		if !ok {
			return Definition{}, fmt.Errorf("malformed frontmatter line: %q", line)
		}
		switch key {
		case "name":
			d.Name = val
		case "version":
			n, err := strconv.Atoi(val)
			if err != nil || n <= 0 {
				return Definition{}, fmt.Errorf("invalid version %q: want a positive integer", val)
			}
			d.Version = n
		case "description":
			d.Description = val
		case "model":
			d.Model = val
		case "tools":
			for _, t := range strings.Split(val, ", ") {
				if t != "" {
					d.Tools = append(d.Tools, t)
				}
			}
		default:
			return Definition{}, fmt.Errorf("unknown frontmatter key: %q", key)
		}
	}
	if d.Name == "" {
		return Definition{}, fmt.Errorf("frontmatter missing name")
	}
	if d.Description == "" {
		return Definition{}, fmt.Errorf("frontmatter missing description")
	}
	if d.PromptBody == "" {
		return Definition{}, fmt.Errorf("empty prompt body")
	}
	return d, nil
}
