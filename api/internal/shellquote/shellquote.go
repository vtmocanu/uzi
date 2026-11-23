// Package shellquote renders a string as one POSIX shell word. It is a stdlib-only leaf so the
// server (copyable fix commands, PRD #4123) and the CLI share one quoting rule.
package shellquote

import "strings"

// Quote wraps s in POSIX single quotes (each embedded single quote becomes close-quote,
// backslash-quote, reopen-quote) unless it is non-empty and made only of characters that are safe
// unquoted.
func Quote(s string) string {
	// A leading '=' is not safe: zsh expands a word starting with it to a command path.
	safe := s != "" && s[0] != '='
	for _, r := range s {
		if !safeRune(r) {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func safeRune(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._:/@=+-", r)
}
