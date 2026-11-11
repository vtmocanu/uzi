package slacksvc

import (
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/runkind"
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
	for _, want := range []string{
		"Resume uses this run's current credential selection. If it still selects the capped token, raise the limit first. To choose another token now, use Switch token or `uzi run set-token`.",
		"Cancel from the run page.",
	} {
		if !strings.Contains(detail, want) {
			t.Fatalf("detail %q lacks %q", detail, want)
		}
	}
	si := baseRun("recovery_wait")
	si.RecoveryWaitCause = txt("anthropic_spend_limit")
	si.Kind = runkind.SelfImprove
	siDetail := spendLimitStateDetail(si)
	if strings.Contains(siDetail, "To choose another token now") {
		t.Fatalf("non-switchable lane detail %q offers a token switch", siDetail)
	}
	if !strings.Contains(siDetail, "raise the limit first.") || !strings.Contains(siDetail, "Cancel from the run page.") {
		t.Fatalf("non-switchable detail %q lost the shared guidance", siDetail)
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
