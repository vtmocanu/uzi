package youtracktest

import (
	"strings"
)

// fieldSpec is a parsed YouTrack "fields" query: a leaf selects a whole
// value, a node selects named members of an object or of each array element.
type fieldSpec map[string]fieldSpec

// parseFields parses "a,b(c,d(e)),$type". It reports false for malformed
// input (unbalanced parentheses, empty names).
func parseFields(s string) (fieldSpec, bool) {
	spec, rest, ok := parseFieldList(s)
	if !ok || rest != "" || len(spec) == 0 {
		return nil, false
	}
	return spec, true
}

func parseFieldList(s string) (fieldSpec, string, bool) {
	spec := fieldSpec{}
	for {
		end := strings.IndexAny(s, ",()")
		if end < 0 {
			end = len(s)
		}
		name := s[:end]
		if name == "" {
			return nil, "", false
		}
		s = s[end:]
		var child fieldSpec
		if strings.HasPrefix(s, "(") {
			var ok bool
			child, s, ok = parseFieldList(s[1:])
			if !ok || !strings.HasPrefix(s, ")") {
				return nil, "", false
			}
			s = s[1:]
		}
		spec[name] = child
		if !strings.HasPrefix(s, ",") {
			return spec, s, true
		}
		s = s[1:]
	}
}

// project returns v restricted to spec, so a driver that forgets to request a
// field sees it missing, as it would on a real server.
func (spec fieldSpec) project(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for name, child := range spec {
			member, ok := x[name]
			if !ok {
				continue
			}
			if child == nil {
				out[name] = member
			} else {
				out[name] = child.project(member)
			}
		}
		return out
	case []map[string]any:
		out := make([]any, 0, len(x))
		for _, e := range x {
			out = append(out, spec.project(e))
		}
		return out
	case []any:
		out := make([]any, 0, len(x))
		for _, e := range x {
			out = append(out, spec.project(e))
		}
		return out
	}
	return v
}
