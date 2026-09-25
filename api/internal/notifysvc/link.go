package notifysvc

import (
	"net/url"
	"strings"
	"unicode"
)

// SafeLinkURL returns raw (trimmed) when it is safe to embed in Slack's `<url|label>`
// link markup, and "" otherwise. Safe means: an absolute http/https URL with a
// non-empty host and no userinfo (`user:pass@`, which would leak a credential or let a
// "trusted.example@" prefix spoof the real host), carrying none of `<`, `>`, `|`,
// whitespace, control characters (any of which would end or split the markup and let
// the rest render as live text) or Unicode format characters (category Cf, e.g. U+202E
// RIGHT-TO-LEFT OVERRIDE or U+200B ZERO WIDTH SPACE, which are invisible and can make
// the link read as something it is not).
// Producers whose link is forge-supplied (a pipeline web URL) filter through it, and
// the Slack notifier re-checks every link with it before rendering.
func SafeLinkURL(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	for _, r := range s {
		if r == '<' || r == '>' || r == '|' || unicode.IsSpace(r) || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return ""
		}
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return ""
	}
	return s
}
