package agenttmpl

import (
	"strings"
	"testing"
)

func TestValidateModel(t *testing.T) {
	// Blank / whitespace-only means inherit: no error, empty result.
	for _, in := range []string{"", "   ", "\t"} {
		got, err := ValidateModel(in)
		if err != nil {
			t.Errorf("ValidateModel(%q) errored: %v", in, err)
		}
		if got != "" {
			t.Errorf("ValidateModel(%q) = %q, want \"\" (inherit)", in, got)
		}
	}

	// Aliases and full IDs pass; surrounding whitespace is trimmed off the result.
	for in, want := range map[string]string{
		"opus":                     "opus",
		"claude-fable-5":           "claude-fable-5",
		"  sonnet  ":               "sonnet",
		"us.anthropic.claude-x:v1": "us.anthropic.claude-x:v1",
	} {
		got, err := ValidateModel(in)
		if err != nil {
			t.Errorf("ValidateModel(%q) errored: %v", in, err)
		}
		if got != want {
			t.Errorf("ValidateModel(%q) = %q, want %q", in, got, want)
		}
	}

	// Interior whitespace, control chars, and over-length reject (Decision 4).
	for name, in := range map[string]string{
		"interior space":    "claude 3",
		"tab":               "claude\t3",
		"newline":           "opus\nmodel: sonnet",
		"too long":          strings.Repeat("x", MaxModelLen+1),
		"bidi override":     "opus\u202eionrever", // U+202E RIGHT-TO-LEFT OVERRIDE (Cf)
		"zero-width space":  "op\u200bus",         // U+200B ZERO WIDTH SPACE (Cf)
		"zero-width joiner": "opus\u200d",         // U+200D ZERO WIDTH JOINER (Cf)
	} {
		if _, err := ValidateModel(in); err == nil {
			t.Errorf("%s: expected rejection for %q", name, in)
		}
	}

	// Exactly MaxModelLen is allowed (boundary).
	if _, err := ValidateModel(strings.Repeat("x", MaxModelLen)); err != nil {
		t.Errorf("model of exactly MaxModelLen should be allowed: %v", err)
	}
}
