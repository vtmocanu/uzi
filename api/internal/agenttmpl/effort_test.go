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

// TestUziDefaultEffort pins uzi's effective default reasoning effort (issue #1157)
// to xhigh — the level an inheriting owner rides in place of the SDK's own `high`.
func TestUziDefaultEffort(t *testing.T) {
	if UziDefaultEffort != "xhigh" {
		t.Errorf("UziDefaultEffort = %q, want \"xhigh\"", UziDefaultEffort)
	}
}

// TestResolveDefaultEffort covers the inherit-vs-explicit resolution (issue #1157):
// nil (NULL) and blank/whitespace-only values ride the uzi default; an explicit
// level is returned trimmed and verbatim, never "".
func TestResolveDefaultEffort(t *testing.T) {
	strptr := func(s string) *string { return &s }

	// Inherit cases: nil pointer and blank/whitespace-only values → the uzi default.
	if got := ResolveDefaultEffort(nil); got != UziDefaultEffort {
		t.Errorf("ResolveDefaultEffort(nil) = %q, want %q", got, UziDefaultEffort)
	}
	for _, in := range []string{"", "   ", "\t"} {
		if got := ResolveDefaultEffort(strptr(in)); got != UziDefaultEffort {
			t.Errorf("ResolveDefaultEffort(%q) = %q, want %q (inherit)", in, got, UziDefaultEffort)
		}
	}

	// Explicit levels are returned verbatim; surrounding whitespace is trimmed.
	for in, want := range map[string]string{
		"low":    "low",
		"medium": "medium",
		"high":   "high",
		"xhigh":  "xhigh",
		"max":    "max",
		" low ":  "low",
	} {
		if got := ResolveDefaultEffort(strptr(in)); got != want {
			t.Errorf("ResolveDefaultEffort(%q) = %q, want %q", in, got, want)
		}
	}
}
