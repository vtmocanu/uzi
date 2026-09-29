package termsafe

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// EscapeBounded is the write-time form for untrusted single-line text that must be KEPT
// rather than refused (PRD #1906 M3: the fetch source log's URLs, content types and
// reasons, which a site or an agent controls and which the log must still record). Every
// rune Unsafe classifies becomes a visible `\u{XXXX}` escape, every byte that is not valid
// UTF-8 becomes `\xNN`, and a literal backslash becomes `\\`, so an escape in the output
// always means an escaped input and never a spelling the input chose. The result passes
// Validate (no control or format rune, valid UTF-8), apart from edge whitespace, which is
// kept as the input had it.
//
// max bounds the output in bytes and is applied per whole token: a rune or an escape is
// either written whole or not at all, so the cut never splits a multi-byte rune or leaves
// half an escape. A non-positive max yields "".
func EscapeBounded(s string, max int) string {
	if max <= 0 {
		return ""
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		var tok string
		switch {
		case r == utf8.RuneError && size <= 1:
			tok = fmt.Sprintf(`\x%02X`, s[i])
			size = 1
		case r == '\\':
			tok = `\\`
		case Unsafe(r):
			tok = fmt.Sprintf(`\u{%04X}`, r)
		default:
			tok = s[i : i+size]
		}
		if b.Len()+len(tok) > max {
			break
		}
		b.WriteString(tok)
		i += size
	}
	return b.String()
}
