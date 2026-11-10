package slacksvc

import (
	"strings"
	"testing"
)

// Issue #3767: an owner-held spend-limit hold reads as "Needs your attention", not "Recovering",
// and names the three ways forward.
func TestSpendLimitHoldLabelAndDetail(t *testing.T) {
	rc := baseRun("recovery_wait")
	rc.RecoveryWaitCause = txt("anthropic_spend_limit")
	emoji, label := statusGlyph(rc)
	if emoji != "⚠️" || label != "Needs your attention · API spend limit" {
		t.Fatalf("glyph=%q label=%q", emoji, label)
	}
	detail := spendLimitStateDetail(rc)
	for _, want := range []string{"Raise the limit in the provider console, then Resume", "uzi run set-token " + rc.ID.String() + " <label>", "Cancel"} {
		if !strings.Contains(detail, want) {
			t.Fatalf("detail %q lacks %q", detail, want)
		}
	}
	blocks, _ := rootBlocksLabelled(rc, "https://uzi.example", emoji, label)
	if !strings.Contains(contextText(blocks), "set-token") {
		t.Fatal("root blocks omit the spend-limit detail")
	}
	other := baseRun("recovery_wait")
	other.RecoveryWaitCause = txt("forge_unreachable")
	if _, l := statusGlyph(other); l != "Recovering" {
		t.Fatalf("other cause label %q", l)
	}
	if spendLimitStateDetail(other) != "" {
		t.Fatal("detail leaked onto another cause")
	}
}
