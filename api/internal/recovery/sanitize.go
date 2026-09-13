package recovery

import (
	"errors"
	"strings"
	"unicode/utf8"
)

// Write-side sanitization (PRD #1296 D6, auditor R2). Every worker-authored label,
// reason, context and SHA is untrusted text that a later web/CLI/TUI render or a raw
// terminal could display. Before ANY such value is stored we reject (validate*) or
// strip (sanitizeLabel) the byte classes that make a stored string a display attack:
// control characters and DEL, the ANSI escape introducer, the Unicode bidirectional
// overrides/isolates, and newlines. This is a bounded, allocation-cheap parser — the
// API never spawns git, checks out files or inflates an object graph (D4); it only
// stores opaque bytes with bounded metadata.

// ErrUnsafeLabel is returned by the validating (reject) path when a value carries a
// disallowed rune, so a caller records a needs_action reason rather than storing it.
var ErrUnsafeLabel = errors.New("recovery: value contains a control, escape, bidi or newline character")

// maxReasonLen bounds a stored sanitized reason/context. A reason is an operator- and
// owner-facing label, never a payload; anything longer is truncated.
const maxReasonLen = 512

// maxIdempotencyKeyLen bounds the worker's durable source-journal identity. It is a
// stable opaque handle, not free text.
const maxIdempotencyKeyLen = 256

// Bidirectional and zero-width formatting code points an attacker uses to reorder or
// hide rendered text. Written as numeric hex (never the literal invisible glyphs, which
// are unreviewable) so the source stays legible.
const (
	runeALM = 0x061C // Arabic letter mark
	// U+200C..U+200F: ZWNJ, ZWJ, LRM, RLM.
	runeZeroWidthLo = 0x200C
	runeZeroWidthHi = 0x200F
	// U+202A..U+202E: LRE, RLE, PDF, LRO, RLO (embeddings/overrides).
	runeBidiEmbedLo = 0x202A
	runeBidiEmbedHi = 0x202E
	// U+2066..U+2069: LRI, RLI, FSI, PDI (isolates).
	runeBidiIsolateLo = 0x2066
	runeBidiIsolateHi = 0x2069
)

// isUnsafeLabelRune reports whether r must never appear in a stored label. It covers
// C0 controls (which include \n, \r and \t), DEL and the C1 range (0x7f-0x9f, which
// includes the ANSI/VT escape's C1 forms), the ESC introducer at 0x1b (inside the C0
// range), the invalid-UTF-8 replacement rune, and the bidirectional/zero-width
// formatting code points enumerated above.
func isUnsafeLabelRune(r rune) bool {
	switch {
	case r == utf8.RuneError: // an invalid UTF-8 byte decoded to the replacement rune
		return true
	case r < 0x20 || (r >= 0x7f && r <= 0x9f): // C0 controls (incl. ESC), DEL, C1 controls
		return true
	case r == runeALM:
		return true
	case r >= runeZeroWidthLo && r <= runeZeroWidthHi:
		return true
	case r >= runeBidiEmbedLo && r <= runeBidiEmbedHi:
		return true
	case r >= runeBidiIsolateLo && r <= runeBidiIsolateHi:
		return true
	}
	return false
}

// validateSafeLabel returns s unchanged when it is a safe label no longer than max
// runes, or ErrUnsafeLabel / an over-length error otherwise. Use it for values that
// must be REJECTED rather than silently altered — the worker-authored idempotency key
// and any field whose exact bytes are load-bearing.
func validateSafeLabel(s string, max int) (string, error) {
	if !utf8.ValidString(s) {
		return "", ErrUnsafeLabel
	}
	if utf8.RuneCountInString(s) > max {
		return "", errors.New("recovery: value exceeds the maximum length")
	}
	for _, r := range s {
		if isUnsafeLabelRune(r) {
			return "", ErrUnsafeLabel
		}
	}
	return s, nil
}

// sanitizeLabel strips every unsafe rune and truncates to max runes, returning a
// bounded safe form. Use it for a stored reason/context the server generates or folds
// worker text into: a needs_action reason must always store SOMETHING, so stripping
// (never failing) is correct here. The result is also trimmed of surrounding spaces.
func sanitizeLabel(s string, max int) string {
	var b strings.Builder
	count := 0
	for _, r := range s {
		if count >= max {
			break
		}
		if isUnsafeLabelRune(r) {
			continue
		}
		b.WriteRune(r)
		count++
	}
	return strings.TrimSpace(b.String())
}

// sanitizeReason is sanitizeLabel bound to the stored-reason ceiling.
func sanitizeReason(s string) string { return sanitizeLabel(s, maxReasonLen) }

// validateSha validates a git object name: 4-64 hexadecimal digits. This both bounds
// the value and, by construction, rejects every control/escape/bidi/newline byte, so a
// stored source_sha / attempted_head_sha can never carry a display attack. An empty
// string is allowed only when optional is true (attempted_head_sha may be absent).
func validateSha(s string, optional bool) (string, error) {
	if s == "" {
		if optional {
			return "", nil
		}
		return "", errors.New("recovery: source sha is required")
	}
	if len(s) < 4 || len(s) > 64 {
		return "", errors.New("recovery: sha must be 4-64 hex characters")
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
		if !isHex {
			return "", errors.New("recovery: sha must be hexadecimal")
		}
	}
	return s, nil
}
