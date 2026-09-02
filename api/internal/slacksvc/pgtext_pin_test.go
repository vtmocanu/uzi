package slacksvc

import (
	"testing"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
)

// TestSlacksvcPgTextValidEmpty is a characterization pin. slacksvc's pgText
// (gate.go) maps ""→VALID empty — the opposite of workersvc's same-named helper
// (which maps ""→NULL). This records the intent: slacksvc built on valid-empty
// semantics, so its pgconv migration target is pgconv.Text (always valid,
// "" included), NOT pgconv.TextOrNull. No production site passes a literal ""
// here, so this cannot gate the migration; it records which behavior the sites
// were built on for the review-gated mapping.
func TestSlacksvcPgTextValidEmpty(t *testing.T) {
	if got := pgconv.Text(""); !got.Valid || got.String != "" {
		t.Errorf("pgText(\"\"): got {Valid=%v String=%q}, want {Valid=true String=\"\"} — slacksvc pgText is valid-empty (→ pgconv.Text)", got.Valid, got.String)
	}
	if got := pgconv.Text("x"); !got.Valid || got.String != "x" {
		t.Errorf("pgText(\"x\"): got {Valid=%v String=%q}, want {Valid=true String=\"x\"}", got.Valid, got.String)
	}
}
