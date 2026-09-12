package workersvc

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestSanitizeAgentLabelMirrorsRunActivitySanitizeRule pins sanitizeAgentLabel (milestones.go) to
// the SAME fold runactivity.sanitize (api/internal/runactivity/runactivity.go) applies to the
// RunActivity.AgentLabel display field (PRD #1224 D4/M3). runactivity.sanitize is package-private,
// so this cannot call it directly; instead it pins sanitizeAgentLabel's contract against the
// DOCUMENTED rule the two copies must share:
//
//   - strip every terminal-unsafe rune under the exact predicate
//     `unicode.IsControl(r) || unicode.In(r, unicode.Cf)` (C0/C1/DEL controls, and the Cf format
//     characters -- bidi overrides, zero-widths, the BOM),
//   - cap the result at 200 runes (runactivity's detailCapRunes; workersvc's maxMilestoneTitleRunes),
//   - keep ordinary text, and decode invalid UTF-8 to U+FFFD rather than dropping the byte.
//
// The two copies must be kept in sync by hand: this test reddens if sanitizeAgentLabel is weakened
// -- dropping the unicode.Cf arm (a bidi override or zero-width would ride through), softening the
// unicode.IsControl arm, or raising/removing the 200-rune cap.
//
// Note: the Cf sample runes are written as numeric rune literals (rune(0x202E), ...) rather than as
// character literals, so this source stays pure ASCII -- an invisible bidi/zero-width/BOM byte in a
// source file is unreviewable (and a raw BOM mid-file will not even compile).
func TestSanitizeAgentLabelMirrorsRunActivitySanitizeRule(t *testing.T) {
	t.Run("strips control and Cf runes", func(t *testing.T) {
		// Each rune is dropped; the ordinary letters bracketing it survive verbatim. The
		// control set and the Cf set are BOTH represented so neither arm of the predicate can be
		// removed without reddening.
		dropped := []struct {
			name string
			r    rune
		}{
			{"C0 ESC (0x1b)", 0x1b},
			{"C0 BEL (0x07)", 0x07},
			{"NUL (0x00)", 0x00},
			{"RLO bidi override U+202E", 0x202E},
			{"zero-width space U+200B", 0x200B},
			{"BOM / ZWNBSP U+FEFF", 0xFEFF},
		}
		for _, d := range dropped {
			in := "ab" + string(d.r) + "cd"
			got := sanitizeAgentLabel(in)
			if strings.ContainsRune(got, d.r) {
				t.Errorf("%s: rune U+%04X must be stripped, got %q", d.name, d.r, got)
			}
			if got != "abcd" {
				t.Errorf("%s: expected the unsafe rune dropped and the text kept, got %q want %q", d.name, got, "abcd")
			}
		}
	})

	t.Run("caps at 200 runes", func(t *testing.T) {
		// Feed 250 ordinary runes; the output must be capped to exactly 200. A 250-rune input
		// distinguishes cap==200 from any other cap value (min(250, cap) == 200 iff cap == 200).
		out := sanitizeAgentLabel(strings.Repeat("x", 250))
		if n := len([]rune(out)); n != 200 {
			t.Fatalf("output must cap at 200 runes (runactivity.detailCapRunes), got %d", n)
		}
	})

	t.Run("keeps ordinary text and decodes invalid UTF-8 to U+FFFD", func(t *testing.T) {
		const plain = "coder wiring the limiter 123"
		if got := sanitizeAgentLabel(plain); got != plain {
			t.Errorf("ordinary text must survive unchanged, got %q want %q", got, plain)
		}
		// A raw undecodable byte (0xff) decodes to utf8.RuneError (U+FFFD) via the range loop and is
		// KEPT (U+FFFD is neither a control nor a Cf rune) -- a visible replacement mark, not a
		// vanished byte, matching runactivity.sanitize / termsafe.SanitizeTTY.
		in := "a" + string([]byte{0xff}) + "b"
		got := sanitizeAgentLabel(in)
		want := "a" + string(utf8.RuneError) + "b"
		if got != want {
			t.Errorf("invalid UTF-8 must decode to U+FFFD and be kept, got %q want %q", got, want)
		}
		if !strings.ContainsRune(got, utf8.RuneError) {
			t.Errorf("expected a U+FFFD replacement rune in %q", got)
		}
	})
}
