package agenttmpl

import "strings"

// unquoteScalar strips one layer of matching surrounding double or single
// quotes, the same rule as agentsource's stripQuotes, so a builtin parsed here and
// the same file parsed by an agent-source sync yield the same description.
func unquoteScalar(v string) string {
	if len(v) >= 2 {
		first, last := v[0], v[len(v)-1]
		if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
			return v[1 : len(v)-1]
		}
	}
	return v
}

// quoteScalar returns v bare when it reads back as the same plain YAML scalar,
// and double-quoted otherwise, mirroring the upstream publisher
// (publish_roles.py scalar), so Render reproduces a published file byte for
// byte. It is conservative: anything that could start a YAML structure, a
// comment or a mapping is quoted.
func quoteScalar(v string) string {
	if v == "" || v != strings.TrimSpace(v) ||
		strings.Contains(v, ": ") || strings.HasSuffix(v, ":") || strings.Contains(v, " #") ||
		strings.ContainsAny(v[:1], "-?:,[]{}#&*!|>'\"%@`") {
		return `"` + v + `"`
	}
	return v
}
