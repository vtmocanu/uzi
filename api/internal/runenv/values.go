package runenv

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Limits (PRD #4416 "Store and vault"). Counts include live rows (enabled and
// disabled) and rows pending deletion; the retired budget is per user.
const (
	MaxValueBytes        = 65536
	MaxDescriptionBytes  = 1024
	MinSecretLen         = 8 // JS string length, as MIN_SECRET_LEN in agent/src/redact.ts
	MaxVarsPerScope      = 50
	MaxScopeValueBytes   = 262144
	MaxRetiredVersions   = 200
	MaxRetiredValueBytes = 2097152
)

// userinfoURL matches scheme://user[:pass]@ anywhere in a value, on any line: a URL
// whose authority section (up to the first path, query or fragment delimiter) holds a
// non-empty userinfo. The scheme may be a chain (jdbc:postgresql://).
var userinfoURL = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.\-:]*://[^/?#\s@]+@`)

// JSLength counts s the way a JavaScript string length does: UTF-16 code units.
func JSLength(s string) int {
	n := 0
	for _, r := range s {
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// ValidateValue applies the value rules. A non-nil result is a *RuleError naming
// the rule; it never contains the value.
func ValidateValue(secret bool, value string) error {
	if !utf8.ValidString(value) {
		return &RuleError{Field: "value", Rule: "must be valid UTF-8"}
	}
	if strings.IndexByte(value, 0) >= 0 {
		return &RuleError{Field: "value", Rule: "must not contain NUL: a process environment cannot carry it"}
	}
	if len(value) > MaxValueBytes {
		return &RuleError{Field: "value", Rule: fmt.Sprintf("must be at most %d bytes", MaxValueBytes)}
	}
	if secret {
		if JSLength(value) < MinSecretLen {
			return &RuleError{Field: "value", Rule: fmt.Sprintf("a secret must be at least %d characters so it can be masked in output; store a shorter value as a plain variable", MinSecretLen)}
		}
		return nil
	}
	if userinfoURL.MatchString(value) {
		return &RuleError{Field: "value", Rule: "a plain value must not be a URL carrying a user or password (scheme://user:password@host); store it as a secret"}
	}
	return nil
}

// ValidateDescription bounds the free-text description.
func ValidateDescription(description string) error {
	if !utf8.ValidString(description) {
		return &RuleError{Field: "description", Rule: "must be valid UTF-8"}
	}
	if len(description) > MaxDescriptionBytes {
		return &RuleError{Field: "description", Rule: fmt.Sprintf("must be at most %d bytes", MaxDescriptionBytes)}
	}
	if strings.IndexByte(description, 0) >= 0 {
		return &RuleError{Field: "description", Rule: "must not contain NUL"}
	}
	for _, r := range description {
		if r == 0x7f || (r >= 0x80 && r <= 0x9f) || r < 0x20 || isBidiOrFormat(r) {
			return &RuleError{Field: "description", Rule: "must be plain text: control characters, escape sequences and bidirectional or invisible formatting characters are not allowed"}
		}
	}
	return nil
}

// isBidiOrFormat reports the code points that can reorder or hide text where a description is
// shown or put in a prompt: zero-width and directional marks (U+200B-200F), the line and paragraph
// separators (U+2028, U+2029), the embedding and override controls (U+202A-202E), the word joiner
// and invisible operators (U+2060-2064), the isolates and the deprecated formatting controls
// (U+2066-206F), and the byte order mark (U+FEFF).
func isBidiOrFormat(r rune) bool {
	switch {
	case r >= 0x200b && r <= 0x200f,
		r == 0x2028, r == 0x2029,
		r >= 0x202a && r <= 0x202e,
		r >= 0x2060 && r <= 0x2064,
		r >= 0x2066 && r <= 0x206f,
		r == 0xfeff:
		return true
	}
	return false
}
