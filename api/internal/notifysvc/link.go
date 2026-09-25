package notifysvc

import (
	"net/url"
	"strings"
	"unicode"
)

// SafeLinkURL returns raw (trimmed) when it is safe to embed in Slack's `<url|label>`
// link markup, and "" otherwise. Safe means: an absolute http/https URL with a
// non-empty host, carrying none of `<`, `>`, `|`, whitespace or control characters
// (any of which would end or split the markup and let the rest render as live text).
// Producers whose link is forge-supplied (a pipeline web URL) filter through it, and
// the Slack notifier re-checks every link with it before rendering.
func SafeLinkURL(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	for _, r := range s {
		if r == '<' || r == '>' || r == '|' || unicode.IsSpace(r) || unicode.IsControl(r) {
			return ""
		}
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	return s
}
