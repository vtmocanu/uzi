package recovery

import (
	"strings"
	"testing"
)

// cp builds a one-rune string from a code point. Hostile test inputs are constructed this
// way — from numeric code points, never pasted invisible bytes — so this source file stays
// pure ASCII and reviewable.
func cp(r rune) string { return string(r) }

// TestValidateSafeLabelRejectsHostileRunes proves the write-side validator (D6/R2) rejects
// every display-attack byte class: C0 controls incl. newlines/tabs, the ESC introducer,
// DEL, the C1 range, and the Unicode bidi overrides/isolates and zero-width formatting runes.
func TestValidateSafeLabelRejectsHostileRunes(t *testing.T) {
	hostile := map[string]string{
		"newline":       "line1\nline2",
		"carriage":      "a\rb",
		"tab":           "a\tb",
		"nul":           "a\x00b",
		"ansi escape":   "a\x1b[31mred",
		"del":           "a\x7fb",
		"c1 csi":        "a" + cp(0x9b) + "b",
		"lro override":  "safe" + cp(0x202E) + "gnirts",
		"rlo override":  cp(0x202B) + "x",
		"lre embedding": "a" + cp(0x202A) + "b",
		"pdf embedding": "a" + cp(0x202C) + "b",
		"lri isolate":   "a" + cp(0x2066) + "b",
		"pdi isolate":   "a" + cp(0x2069) + "b",
		"zwnj":          "a" + cp(0x200C) + "b",
		"lrm mark":      "a" + cp(0x200E) + "b",
		"rlm mark":      "a" + cp(0x200F) + "b",
		"arabic mark":   "a" + cp(0x061C) + "b",
	}
	for name, s := range hostile {
		if _, err := validateSafeLabel(s, 256); err == nil {
			t.Errorf("%s: validateSafeLabel accepted a hostile string, want rejection", name)
		}
	}
}

// TestValidateSafeLabelAcceptsAndBounds proves ordinary text passes and length is bounded.
func TestValidateSafeLabelAcceptsAndBounds(t *testing.T) {
	ok := []string{"finalization_blocked", "pre_partial", "a normal reason with spaces.", "unicode cafe ok"}
	for _, s := range ok {
		if got, err := validateSafeLabel(s, 256); err != nil || got != s {
			t.Errorf("validateSafeLabel(%q) = (%q, %v), want it accepted unchanged", s, got, err)
		}
	}
	if _, err := validateSafeLabel(strings.Repeat("x", 10), 4); err == nil {
		t.Error("validateSafeLabel accepted an over-length string, want rejection")
	}
}

// TestSanitizeLabelStripsAndTruncates proves the stripping path removes hostile runes,
// keeps the safe remainder, and truncates to the bound.
func TestSanitizeLabelStripsAndTruncates(t *testing.T) {
	if got := sanitizeLabel("re\naso\x1bn"+cp(0x202E), maxReasonLen); got != "reason" {
		t.Errorf("sanitizeLabel stripped to %q, want %q", got, "reason")
	}
	if got := sanitizeLabel("abcdef", 3); got != "abc" {
		t.Errorf("sanitizeLabel truncated to %q, want %q", got, "abc")
	}
	// A string that is ALL hostile runes sanitizes to empty (never fails).
	if got := sanitizeReason("\n\r\x1b" + cp(0x202E)); got != "" {
		t.Errorf("sanitizeReason(all hostile) = %q, want empty", got)
	}
}

// TestValidateSha proves the git-object-name validator accepts real shas and rejects
// non-hex / out-of-range / (for the required case) empty values.
func TestValidateSha(t *testing.T) {
	good := []string{"abc123", "0123456789abcdef0123456789abcdef01234567", strings.Repeat("a", 64)}
	for _, s := range good {
		if _, err := validateSha(s, false); err != nil {
			t.Errorf("validateSha(%q) rejected a valid sha: %v", s, err)
		}
	}
	bad := []string{"", "xyz", "abc", "gggg", "abc\n123", strings.Repeat("a", 65), "ab cd"}
	for _, s := range bad {
		if _, err := validateSha(s, false); err == nil {
			t.Errorf("validateSha(%q, required) accepted an invalid sha", s)
		}
	}
	// Optional empty is allowed; optional non-empty is still validated.
	if _, err := validateSha("", true); err != nil {
		t.Errorf("validateSha(empty, optional) = %v, want nil", err)
	}
	if _, err := validateSha("nothex", true); err == nil {
		t.Error("validateSha(nothex, optional) accepted a non-hex value")
	}
}

// TestIsSHA256Hex proves the checksum format check.
func TestIsSHA256Hex(t *testing.T) {
	if !isSHA256Hex(strings.Repeat("a", 64)) {
		t.Error("isSHA256Hex rejected a 64-char hex string")
	}
	upper := "AB" + strings.Repeat("c", 62)
	for _, s := range []string{"", strings.Repeat("a", 63), strings.Repeat("a", 65), strings.Repeat("g", 64), upper} {
		want := s == upper // uppercase hex is still hex
		if got := isSHA256Hex(s); got != want {
			t.Errorf("isSHA256Hex(%q) = %v, want %v", s, got, want)
		}
	}
}
