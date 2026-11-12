package issuetrackersvc

import (
	"errors"
	"net/url"
	"strings"
	"unicode"

	"github.com/vtmocanu/uzi/api/internal/config"
	"github.com/vtmocanu/uzi/api/internal/issuetracker"
)

// strictBaseURL validates raw as an https scheme-and-host base URL and returns
// config.NormalizeForgeBaseURL's canonical form. Unlike that function it
// refuses, rather than strips, userinfo, an opaque part, a path other than
// empty or "/", a query and a fragment: stripping would let
// "https://allowed@evil" or "https://host/x" mean something other than what was
// checked. The service keeps its own check, rather than trusting the driver's,
// because it must normalize to the stored and AAD form via
// config.NormalizeForgeBaseURL; the driver re-checks the URL independently as
// defence in depth. The returned string is the one value that is checked against the
// allowlist, stored, and bound into the token's AAD.
func strictBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("invalid base URL")
	}
	// "?" and "#" with nothing after them parse to empty RawQuery/Fragment, so
	// they are refused on the text too.
	if u.User != nil || u.Opaque != "" || (u.Path != "" && u.Path != "/") ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.ContainsAny(raw, "?#") {
		return "", errors.New("invalid base URL: use https scheme and host only, with no credentials, path, query or fragment")
	}
	norm, err := config.NormalizeForgeBaseURL(raw)
	if err != nil {
		return "", errors.New("invalid base URL: it must be an https URL with a host")
	}
	// The canonical form must be a fixed point with a real hostname, so an
	// odd host (an escaped "%", an empty name before a port) cannot be stored
	// in a form that normalizes to something else later.
	again, err := config.NormalizeForgeBaseURL(norm)
	if err != nil || again != norm {
		return "", errors.New("invalid base URL: it must be an https URL with a host")
	}
	nu, err := url.Parse(norm)
	if err != nil || nu.Hostname() == "" {
		return "", errors.New("invalid base URL: it must be an https URL with a host")
	}
	// A control, format or non-ASCII character in the host (a zero-width or bidi
	// character) would look like another host once it is shown.
	for _, r := range nu.Host {
		if issuetracker.InvisibleRune(r) {
			return "", errors.New("invalid base URL: the host holds a control or invisible character")
		}
		// An internationalised name arrives as "xn--" punycode.
		if r > unicode.MaxASCII {
			return "", errors.New("invalid base URL: the host must be ASCII; use the xn-- form of an internationalised name")
		}
	}
	return norm, nil
}
