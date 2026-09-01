package usagepoller

import "testing"

// TestUsagepollerPgTextValidEmpty is a characterization pin. usagepoller's
// pgText (engine.go) maps ""→VALID empty — like slacksvc's and opposite to
// workersvc's same-named helper (which maps ""→NULL). This records the intent:
// usagepoller built on valid-empty semantics, so its pgconv migration target is
// pgconv.Text (always valid, "" included), NOT pgconv.TextOrNull. No production
// site passes a literal "" here, so this cannot gate the migration; it records
// which behavior the site was built on for the review-gated mapping.
func TestUsagepollerPgTextValidEmpty(t *testing.T) {
	if got := pgText(""); !got.Valid || got.String != "" {
		t.Errorf("pgText(\"\"): got {Valid=%v String=%q}, want {Valid=true String=\"\"} — usagepoller pgText is valid-empty (→ pgconv.Text)", got.Valid, got.String)
	}
	if got := pgText("x"); !got.Valid || got.String != "x" {
		t.Errorf("pgText(\"x\"): got {Valid=%v String=%q}, want {Valid=true String=\"x\"}", got.Valid, got.String)
	}
}
