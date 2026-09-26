// Package redirectguard holds the redirect policy for every HTTP client that
// carries a forge credential. net/http re-sends Authorization on a redirect to the
// same hostname whatever the scheme or port, re-sends custom headers (GitLab's
// PRIVATE-TOKEN) to ANY host, and go-github's auth transport re-adds its Bearer
// header on every hop. None of that is safe once the forge answers with a 3xx, so a
// credentialed client may follow a redirect only while it stays on the origin the
// caller allowlist-validated: the same scheme, hostname and effective port as the
// first request of the chain.
package redirectguard

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// MaxHops mirrors net/http's default redirect ceiling. Installing a CheckRedirect
// replaces that default, so SameOrigin enforces it itself.
const MaxHops = 10

// ErrCrossOrigin marks a refused redirect. The wrapping error names only the
// target's scheme://host:port, never its path, query or userinfo.
var ErrCrossOrigin = errors.New("redirect leaves the request origin")

// SameOrigin is an http.Client CheckRedirect that follows a redirect only when the
// target shares the original request's scheme, hostname and effective port, and
// only for up to MaxHops hops. The origin is via[0], the first request of the
// chain (the one the caller built from an allowlisted base URL), so a chain can
// never walk off the origin one hop at a time. It runs BEFORE net/http sends the
// redirected request, so a refused target receives no request at all.
func SameOrigin(req *http.Request, via []*http.Request) error {
	if len(via) == 0 {
		return nil
	}
	if len(via) >= MaxHops {
		return fmt.Errorf("stopped after %d redirects", MaxHops)
	}
	if !sameOrigin(via[0].URL, req.URL) {
		return fmt.Errorf("%w: refusing %s", ErrCrossOrigin, originString(req.URL))
	}
	return nil
}

// StopOffOrigin is SameOrigin for a client whose SDK retries a failed request:
// instead of failing the chain it stops at the redirect response
// (http.ErrUseLastResponse), which the SDK reads as an ordinary non-2xx answer.
// GitLab's retryable client re-sends a GET whose CheckRedirect errored, five
// times with backoff; the 3xx it gets here is not retried. The refused target
// still receives nothing.
func StopOffOrigin(req *http.Request, via []*http.Request) error {
	if SameOrigin(req, via) != nil {
		return http.ErrUseLastResponse
	}
	return nil
}

func sameOrigin(a, b *url.URL) bool {
	if a == nil || b == nil {
		return false
	}
	as, bs := strings.ToLower(a.Scheme), strings.ToLower(b.Scheme)
	ah, bh := normalizedHost(a), normalizedHost(b)
	return as == bs && ah != "" && ah == bh &&
		effectivePort(as, a.Port()) == effectivePort(bs, b.Port())
}

// normalizedHost lowercases the hostname and drops a trailing root dot, so
// "Example.COM." and "example.com" compare equal.
func normalizedHost(u *url.URL) string {
	return strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
}

func effectivePort(scheme, port string) string {
	if port != "" {
		return port
	}
	switch scheme {
	case "https":
		return "443"
	case "http":
		return "80"
	}
	return ""
}

func originString(u *url.URL) string {
	if u == nil {
		return "<nil>"
	}
	return (&url.URL{Scheme: u.Scheme, Host: u.Host}).String()
}

// OriginPin is an http.RoundTripper that sends a request only when its URL is on
// one of the allowed origins, and fails it before any byte leaves the process
// otherwise. It exists for SDKs that retarget a request themselves instead of
// letting net/http follow the redirect (go-github's bareDoUntilFound re-issues a
// 301 through a redirect-ignoring client and compares only Host, so an https to
// http downgrade on the same host:port bypasses any CheckRedirect). Placed BELOW
// the SDK's auth transport, it sees every credentialed request however the SDK
// built it.
//
// Allow must be called before the first request; the allowed set is read without
// locking afterwards. An empty set refuses everything.
type OriginPin struct {
	next    http.RoundTripper
	allowed []*url.URL
}

// NewOriginPin wraps next (http.DefaultTransport when nil).
func NewOriginPin(next http.RoundTripper) *OriginPin {
	if next == nil {
		next = http.DefaultTransport
	}
	return &OriginPin{next: next}
}

// Allow adds the origin of each absolute URL to the allowed set.
func (p *OriginPin) Allow(rawURLs ...string) error {
	for _, raw := range rawURLs {
		u, err := url.Parse(raw)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return fmt.Errorf("redirectguard: not an absolute URL: %q", originHint(raw))
		}
		p.allowed = append(p.allowed, u)
	}
	return nil
}

// RoundTrip implements http.RoundTripper.
func (p *OriginPin) RoundTrip(req *http.Request) (*http.Response, error) {
	for _, a := range p.allowed {
		if sameOrigin(a, req.URL) {
			return p.next.RoundTrip(req)
		}
	}
	if req.Body != nil {
		_ = req.Body.Close()
	}
	return nil, fmt.Errorf("%w: refusing %s", ErrCrossOrigin, originString(req.URL))
}

// originHint keeps a malformed input out of an error verbatim beyond its origin.
func originHint(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		return originString(u)
	}
	return "<unparseable>"
}
