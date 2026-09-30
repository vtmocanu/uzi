package termsafe

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestEscapeBounded(t *testing.T) {
	cases := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"plain", "https://docs.example.com/a?b=c", 100, "https://docs.example.com/a?b=c"},
		{"escape sequence", "a\x1b[2Jb", 100, `a\u{001B}[2Jb`},
		{"bidi override", "ab\u202Ecd", 100, `ab\u{202E}cd`},
		{"newline and tab", "a\nb\tc", 100, `a\u{000A}b\u{0009}c`},
		{"invalid byte", "a\xffb", 100, `a\xFFb`},
		{"backslash is escaped so an escape is never forged", `a\u{202E}b`, 100, `a\\u{202E}b`},
		{"replacement char kept", "a\uFFFDb", 100, "a\uFFFDb"},
		{"multi-byte rune not split", "ab\U0001F600", 5, "ab"},
		{"escape not split", "a\u202E", 5, "a"},
		{"exact fit", "abc", 3, "abc"},
		{"zero max", "abc", 0, ""},
	}
	for _, c := range cases {
		got := EscapeBounded(c.in, c.max)
		if got != c.want {
			t.Errorf("%s: EscapeBounded(%q, %d) = %q, want %q", c.name, c.in, c.max, got, c.want)
		}
		if len(got) > c.max && c.max > 0 {
			t.Errorf("%s: %d bytes exceeds max %d", c.name, len(got), c.max)
		}
		if !utf8.ValidString(got) {
			t.Errorf("%s: output %q is not valid UTF-8", c.name, got)
		}
		if strings.TrimSpace(got) == got {
			if err := Validate("x", got); err != nil {
				t.Errorf("%s: output %q fails Validate: %v", c.name, got, err)
			}
		}
	}
}

// TestEscapeBoundedEveryUnsafeRune: no rune Unsafe classifies survives the escape.
func TestEscapeBoundedEveryUnsafeRune(t *testing.T) {
	for r := rune(0); r <= utf8.MaxRune; r++ {
		if !utf8.ValidRune(r) || !Unsafe(r) {
			continue
		}
		got := EscapeBounded("x"+string(r)+"y", 64)
		for _, g := range got {
			if Unsafe(g) {
				t.Fatalf("rune %U survived as %q", r, got)
			}
		}
	}
}
