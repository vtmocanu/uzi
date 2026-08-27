package agenttmpl

import "testing"

func TestValidateEffort(t *testing.T) {
	// Blank / whitespace-only means inherit: no error, empty result.
	for _, in := range []string{"", "   ", "\t"} {
		got, err := ValidateEffort(in)
		if err != nil {
			t.Errorf("ValidateEffort(%q) errored: %v", in, err)
		}
		if got != "" {
			t.Errorf("ValidateEffort(%q) = %q, want \"\" (inherit)", in, got)
		}
	}

	// Each level is accepted and returned verbatim; surrounding whitespace is
	// trimmed off the result.
	for in, want := range map[string]string{
		"low":    "low",
		"medium": "medium",
		"high":   "high",
		"xhigh":  "xhigh",
		"max":    "max",
		" high ": "high",
	} {
		got, err := ValidateEffort(in)
		if err != nil {
			t.Errorf("ValidateEffort(%q) errored: %v", in, err)
		}
		if got != want {
			t.Errorf("ValidateEffort(%q) = %q, want %q", in, got, want)
		}
	}

	// Unknown token, interior whitespace, and wrong case reject (closed,
	// case-sensitive enum).
	for name, in := range map[string]string{
		"unknown token":     "turbo",
		"interior space":    "hi gh",
		"uppercase":         "HIGH",
		"empty-ish variant": "none",
	} {
		if _, err := ValidateEffort(in); err == nil {
			t.Errorf("%s: expected rejection for %q", name, in)
		}
	}
}
