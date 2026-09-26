package workersvc

import (
	"github.com/vtmocanu/uzi/api/internal/secretscrub"
	"github.com/vtmocanu/uzi/api/internal/termsafe"
)

// ScrubUntrustedText renders one untrusted, agent-authored FLOWING text (markdown kept:
// \n and \t survive) inert for persistence or publication WITHOUT truncating it: strip
// terminal-control / bidi-override runes over the WHOLE value, then secret-scrub the
// WHOLE normalized value. The SanitizeBounded cap is 3 bytes per input byte, which it
// can never reach: len(s)+1 is NOT enough, because an invalid UTF-8 byte is rewritten as
// the 3-byte U+FFFD, so normalized text can be longer than its input. Use it directly for
// a sink with no byte bound of its own (an MR-thread reply body); a bounded sink goes
// through scrubThenBound.
func ScrubUntrustedText(s string) string {
	return secretscrub.Scrub(termsafe.SanitizeBounded(s, 3*len(s)+1))
}

// scrubThenBound sanitizes the FULL untrusted field, secret-scrubs the whole value, and
// ONLY THEN applies the byte cap. The order is a security property: secretscrub.Scrub
// matches a credential only when it sees the WHOLE token, so truncating first can leave a
// sub-match-length prefix that Scrub misses, leaking it into the filed forge issue (a
// public artifact). ScrubUntrustedText does the uncut sanitize and the scrub; the second
// SanitizeBounded applies the real, rune-safe cap to the already-scrubbed text. Shared by
// every bounded agent-text sink in this package (report_md, proposals, findings,
// summaries).
func scrubThenBound(s string, max int) string {
	return termsafe.SanitizeBounded(ScrubUntrustedText(s), max)
}
