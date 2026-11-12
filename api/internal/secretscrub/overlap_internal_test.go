package secretscrub

import (
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/secretscrub/scrubtest"
)

// TestRedactOverlappingMatchesEveryOffsetReference pins the windowed scan to the
// every-start-offset reference for each pattern set in this package.
func TestRedactOverlappingMatchesEveryOffsetReference(t *testing.T) {
	scrubtest.AssertMatchesReference(t, Scrub, scrubPatterns, "[redacted]")
	scrubtest.AssertMatchesReference(t, ScrubHistory, historyPatterns, "[redacted]")
	scrubtest.AssertMatchesReference(t, ScrubSlackTokens, slackPatterns, "[redacted]")
}

// TestRedactOverlappingRepeatedPrefixes keeps the bounded-rescan input functional:
// a body of repeated prefixes is one credential. Timing lives in the benchmark,
// not here, so a loaded -race runner cannot fail it.
func TestRedactOverlappingRepeatedPrefixes(t *testing.T) {
	in := strings.Repeat("uzc_", 1<<15) // 128 KiB
	if got := Scrub(in); got != "[redacted]" {
		t.Fatalf("Scrub(repeated prefixes) = %.40q..., want one placeholder", got)
	}
}

// BenchmarkRedactOverlappingRepeatedPrefixes measures the same input; a quadratic
// rescan shows up here as seconds per op instead of milliseconds.
func BenchmarkRedactOverlappingRepeatedPrefixes(b *testing.B) {
	in := strings.Repeat("uzc_", 1<<15)
	for b.Loop() {
		Scrub(in)
	}
}
