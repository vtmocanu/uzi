// Package agenttmpl renders agent templates to Claude Code subagent Markdown
// and holds the Go-embedded builtin role definitions. It has no database
// dependency: Render is a pure function and the builtins are parsed from files
// embedded in the binary, so the renderer can be exercised in isolation by the
// golden-file tests.
package agenttmpl

import "strings"

// Definition is the render-time shape of an agent template: exactly the fields
// that determine the subagent Markdown. It is intentionally independent of the
// store row so Render carries no pgtype/uuid baggage.
type Definition struct {
	Name        string
	Description string
	// Model is the model alias or full ID. Empty means inherit — the rendered
	// frontmatter omits the model line.
	Model string
	// Tools is the allowlist. Empty means inherit all — the rendered
	// frontmatter omits the tools line.
	Tools []string
	// PromptBody is the system prompt body (Markdown). It includes its own
	// trailing newline so rendered output byte-matches the source file.
	PromptBody string
}

// Render produces the canonical Claude Code subagent Markdown for a template:
// YAML frontmatter in fixed field order (name, description, tools, model), with
// tools serialized as an inline comma-separated string (not a YAML sequence),
// then a blank line, then the prompt body. tools and model lines are omitted
// when empty (inherit). The frontmatter is built as ordered strings rather than
// via yaml.Marshal, which would reorder the map keys and break byte-stability.
func Render(d Definition) []byte {
	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString("name: " + d.Name + "\n")
	b.WriteString("description: " + d.Description + "\n")
	if len(d.Tools) > 0 {
		b.WriteString("tools: " + strings.Join(d.Tools, ", ") + "\n")
	}
	if d.Model != "" {
		b.WriteString("model: " + d.Model + "\n")
	}
	b.WriteString("---\n\n")
	b.WriteString(d.PromptBody)
	return []byte(b.String())
}
