// Package scrubtest holds test support for secretscrub.RedactOverlapping callers:
// a quadratic reference that finds a match at EVERY start position, and a corpus
// of strings built from overlapping credential fragments. Tests only.
package scrubtest

import (
	"math/rand"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// naive redacts the union of all matches starting at every byte offset. It is the
// unbounded-work reference the windowed scan must agree with.
func naive(s string, patterns []*regexp.Regexp, placeholder string) string {
	var spans [][2]int
	for _, pattern := range patterns {
		for start := 0; start < len(s); start++ {
			loc := pattern.FindStringIndex(s[start:])
			if loc == nil {
				break
			}
			if loc[0] != 0 {
				start += loc[0] - 1
				continue
			}
			spans = append(spans, [2]int{start, start + loc[1]})
		}
	}
	if len(spans) == 0 {
		return s
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i][0] < spans[j][0] })
	var b strings.Builder
	prev := 0
	start, end := spans[0][0], spans[0][1]
	for _, sp := range spans[1:] {
		if sp[0] < end {
			end = max(end, sp[1])
			continue
		}
		b.WriteString(s[prev:start] + placeholder)
		prev, start, end = end, sp[0], sp[1]
	}
	b.WriteString(s[prev:start] + placeholder + s[end:])
	return b.String()
}

// fragments are prefixes, body characters and separators of every registered
// family, so random concatenations produce nested and overlapping shapes.
var fragments = []string{
	"ghp_", "ghs_", "gh", "p_", "github_pat_", "uzc_", "uzp_", "xoxb-", "xoxe-", "xapp-",
	"sk-", "sk-ant-", "sk-proj-", "glpat-", "glrt-", "AKIA", "AIza", "eyJ", ".",
	"-", "_", "A", "Z", "b", "9", "AAAAAAAAAAAAAAAA", "bbbbbbbb", " ", "\n", "@", ":",
	"postgres://", "Bearer ", "Authorization: ", "-----BEGIN RSA PRIVATE KEY-----",
	"-----END RSA PRIVATE KEY-----",
}

// AssertMatchesReference checks redact against the every-offset reference on a
// deterministic random corpus of overlapping fragments.
func AssertMatchesReference(t *testing.T, redact func(string) string, patterns []*regexp.Regexp, placeholder string) {
	t.Helper()
	rng := rand.New(rand.NewSource(1)) //nolint:gosec // G404: a fixed seed keeps the test corpus reproducible; nothing secret.
	for i := 0; i < 3000; i++ {
		var b strings.Builder
		for n := 1 + rng.Intn(24); n > 0; n-- {
			b.WriteString(fragments[rng.Intn(len(fragments))])
		}
		in := b.String()
		if got, want := redact(in), naive(in, patterns, placeholder); got != want {
			t.Fatalf("input %q:\n got %q\nwant %q", in, got, want)
		}
	}
}
