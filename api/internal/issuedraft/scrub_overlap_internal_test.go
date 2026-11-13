package issuedraft

import (
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/secretscrub/scrubtest"
)

func TestScrubSecretShapesMatchesEveryOffsetReference(t *testing.T) {
	scrubtest.AssertMatchesReference(t, ScrubSecretShapes, secretShapes, "[redacted]")
}

// TestScrubSecretShapesSameFamilyOverlap pins overlaps longer than two bytes: a
// second token starting inside the first and running past its end.
func TestScrubSecretShapesSameFamilyOverlap(t *testing.T) {
	seg := strings.Repeat("s", 80)
	for _, tok := range []string{
		// The GitHub body excludes '_': the inner ghp_ starts three bytes before the outer end.
		"ghp_" + strings.Repeat("A", 34) + "ghp_" + strings.Repeat("B", 36),
		// Fixed-length AWS body: an inner AKIA starts 6 bytes in and runs 6 past.
		"AKIA" + "XX" + "AKIA" + "0123456789" + "ZZZZZZ",
		// An inner JWT starting in the second segment, segments longer than any window.
		"eyJ" + seg + ".eyJ" + seg + "." + seg + "." + seg,
	} {
		if out := ScrubSecretShapes("leak " + tok + " end"); out != "leak [redacted] end" {
			t.Errorf("ScrubSecretShapes(%q) = %q, want the whole span replaced", tok, out)
		}
	}
}
