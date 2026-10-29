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

func TestResolveHarnessEffort(t *testing.T) {
	str := func(v string) *string { return &v }
	explicitBuiltin := EffortResolution{Mode: EffortModeExplicit, Level: "medium", Source: EffortSourceBuiltin}
	runtimeDefault := EffortResolution{Mode: EffortModeRuntimeDefault, Source: EffortSourceUserDefault}
	cases := []struct {
		name   string
		stored *string
		want   EffortResolution
	}{
		{"NULL", nil, explicitBuiltin},
		{"blank", str(""), explicitBuiltin},
		{"whitespace", str(" \t "), explicitBuiltin},
		{"low", str("low"), EffortResolution{EffortModeExplicit, "low", EffortSourceUserDefault}},
		{"medium", str("medium"), EffortResolution{EffortModeExplicit, "medium", EffortSourceUserDefault}},
		{"high", str("high"), EffortResolution{EffortModeExplicit, "high", EffortSourceUserDefault}},
		{"xhigh trimmed", str(" xhigh "), EffortResolution{EffortModeExplicit, "xhigh", EffortSourceUserDefault}},
		{"max", str("max"), EffortResolution{EffortModeExplicit, "max", EffortSourceUserDefault}},
		{"runtime", str("runtime"), runtimeDefault},
		{"runtime trimmed", str(" runtime "), runtimeDefault},
		{"corrupt ultra", str("ultra"), explicitBuiltin},
		{"corrupt uppercase", str("HIGH"), explicitBuiltin},
	}
	for _, harness := range []string{"claude", "codex"} {
		for _, tc := range cases {
			if got := ResolveHarnessEffort(harness, tc.stored); got != tc.want {
				t.Errorf("%s/%s: ResolveHarnessEffort = %+v, want %+v", harness, tc.name, got, tc.want)
			}
		}
	}
}

func TestValidateDefaultEffort(t *testing.T) {
	for in, want := range map[string]string{
		"": "", "  ": "", "runtime": "runtime", " runtime ": "runtime",
		"low": "low", "max": "max", " high ": "high",
	} {
		got, err := ValidateDefaultEffort(in)
		if err != nil || got != want {
			t.Errorf("ValidateDefaultEffort(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"ultra", "HIGH", "Runtime", "hi gh", "none"} {
		if _, err := ValidateDefaultEffort(in); err == nil {
			t.Errorf("ValidateDefaultEffort(%q) accepted", in)
		}
	}
	// The pin validator still refuses the preference-only token.
	if _, err := ValidateEffort("runtime"); err == nil {
		t.Error("ValidateEffort accepted runtime")
	}
}
