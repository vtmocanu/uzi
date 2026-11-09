package agenttmpl

import (
	"strings"
	"testing"
)

func TestValidateTemplatePin(t *testing.T) {
	for _, harness := range []string{"claude", "codex"} {
		for _, raw := range []string{"", " \t\n"} {
			if got, err := ValidateTemplatePin(harness, raw); got != "" || err != nil {
				t.Errorf("%s blank: %q, %v", harness, got, err)
			}
		}
		for _, raw := range []string{strings.Repeat("x", 101), "a\nb", "a\u202eb", "a b", "a\x00b"} {
			if _, err := ValidateTemplatePin(harness, raw); err == nil {
				t.Errorf("%s accepted invalid %q", harness, raw)
			}
		}
	}
	for model := range CodexRoleModels {
		if _, err := ValidateTemplatePin("claude", model); err == nil {
			t.Errorf("Claude accepted %s", model)
		}
		if got, err := ValidateTemplatePin("codex", " "+model+" "); err != nil || got != model {
			t.Errorf("Codex %s: %q, %v", model, got, err)
		}
	}
	for _, raw := range []string{"sonnet", "custom-claude", "gpt-future"} {
		if got, err := ValidateTemplatePin("claude", raw); err != nil || got != raw {
			t.Errorf("Claude custom %q: %q, %v", raw, got, err)
		}
		if _, err := ValidateTemplatePin("codex", raw); err == nil {
			t.Errorf("Codex accepted %q", raw)
		}
	}
	if _, err := ValidateTemplatePin("claude", strings.Repeat("x", 100)); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"", "sonnet"} {
		if _, err := ValidateTemplatePin("unknown", raw); err == nil {
			t.Error("unknown harness accepted")
		}
	}
}

func TestSplitModelPin(t *testing.T) {
	for _, tc := range []struct{ raw, claude, codex string }{
		{" \t", "", ""}, {" sonnet ", "sonnet", ""},
		{"custom-id", "custom-id", ""}, {" gpt-6-luna ", "", "gpt-6-luna"},
	} {
		c, x := SplitModelPin(tc.raw)
		if c != tc.claude || x != tc.codex {
			t.Errorf("%q => %q, %q", tc.raw, c, x)
		}
	}
	for model := range CodexRoleModels {
		if c, x := SplitModelPin(model); c != "" || x != model {
			t.Errorf("%q => %q, %q", model, c, x)
		}
	}
}

func TestPinForHarness(t *testing.T) {
	for _, tc := range []struct{ harness, claude, codex, want string }{
		{"claude", "sonnet", "gpt-6-luna", "sonnet"},
		{"codex", "sonnet", "gpt-6-luna", "gpt-6-luna"},
		{"claude", "gpt-6-luna", "", ""},
		{"claude", " gpt-6-sol ", "", ""},
		{"codex", "sonnet", "", ""},
		{"codex", "", "sonnet", ""},
		{"unknown", "sonnet", "gpt-6-sol", ""},
	} {
		if got := PinForHarness(tc.harness, tc.claude, tc.codex); got != tc.want {
			t.Errorf("%+v => %q", tc, got)
		}
	}
	for model := range CodexRoleModels {
		if got := PinForHarness("claude", model, ""); got != "" {
			t.Errorf("Claude delivered %s", got)
		}
		if got := PinForHarness("codex", "", model); got != model {
			t.Errorf("Codex dropped %s", model)
		}
	}
}

func TestCodexPinContentAndRender(t *testing.T) {
	a := Definition{Name: "coder", Description: "Code", Model: "sonnet", PromptBody: "Code.\n"}
	b := a
	b.CodexModel = "gpt-6-luna"
	if SameContent(a, b) {
		t.Error("Codex pin did not change content")
	}
	if string(Render(a)) != string(Render(b)) {
		t.Error("Codex pin leaked into Claude Markdown")
	}
}
