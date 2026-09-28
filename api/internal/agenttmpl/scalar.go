package agenttmpl

import (
	"strconv"
	"strings"
)

// unquoteScalar reverses quoteScalar and strips the one layer of quotes the
// upstream publisher adds, so a builtin parsed here and the same file parsed by
// an agent-source sync (agentsource stripQuotes) yield the same description. A
// double-quoted value with escapes is decoded; a single-quoted one has its ''
// pairs folded back to '.
func unquoteScalar(v string) string {
	if len(v) < 2 {
		return v
	}
	first, last := v[0], v[len(v)-1]
	switch {
	case first == '"' && last == '"':
		if strings.Contains(v, `\`) {
			if u, err := strconv.Unquote(v); err == nil {
				return u
			}
		}
		return v[1 : len(v)-1]
	case first == '\'' && last == '\'':
		return strings.ReplaceAll(v[1:len(v)-1], "''", "'")
	}
	return v
}

// quoteScalar returns v bare when it reads back as the same plain YAML scalar,
// and double-quoted otherwise, mirroring the upstream publisher
// (publish_roles.py scalar), so Render reproduces a published file byte for
// byte. The publisher refuses a value holding a quote, backslash or control
// character; an admin-authored template can hold one, so such a value is
// escaped (strconv.Quote's escapes are all valid in a YAML double-quoted
// scalar) rather than emitted as broken YAML.
func quoteScalar(v string) string {
	needs := v == "" || v != strings.TrimSpace(v) ||
		strings.Contains(v, ": ") || strings.HasSuffix(v, ":") || strings.Contains(v, " #") ||
		strings.ContainsAny(v[:1], "-?:,[]{}#&*!|>'\"%@`") || hasControl(v)
	if !needs {
		return v
	}
	if strings.ContainsAny(v, `"\`) || hasControl(v) {
		return strconv.Quote(v)
	}
	return `"` + v + `"`
}

func hasControl(v string) bool {
	for _, r := range v {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}
