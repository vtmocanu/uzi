// Package middleware holds HTTP middleware: authentication, admin gating and
// rate limiting.
package middleware

import (
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/vtmocanu/uzi/api/internal/httpx"
)

// Limiter is an in-process, per-IP fixed-window rate limiter (an IPv6 client is
// keyed on its /64, see rateLimitSubject). It intentionally
// avoids an external dependency, since the MVP is a single-process demo.
// Expired buckets are reclaimed both lazily (on access)
// and by a background sweeper so the map cannot grow without bound.
type Limiter struct {
	mu             sync.Mutex
	max            int
	window         time.Duration
	trustedProxies []*net.IPNet
	buckets        map[string]*bucket
}

type bucket struct {
	count   int
	resetAt time.Time
}

// NewLimiter returns a limiter allowing max requests per window per key and
// starts a background sweeper. trustedProxies gates X-Forwarded-For handling
// (see ClientIP).
func NewLimiter(max int, window time.Duration, trustedProxies []*net.IPNet) *Limiter {
	l := &Limiter{
		max:            max,
		window:         window,
		trustedProxies: trustedProxies,
		buckets:        make(map[string]*bucket),
	}
	go l.sweep()
	return l
}

// sweep periodically evicts expired buckets.
func (l *Limiter) sweep() {
	ticker := time.NewTicker(l.window)
	defer ticker.Stop()
	for range ticker.C {
		now := time.Now()
		l.mu.Lock()
		for k, b := range l.buckets {
			if now.After(b.resetAt) {
				delete(l.buckets, k)
			}
		}
		l.mu.Unlock()
	}
}

// allow records a hit for key and reports whether it is within budget.
func (l *Limiter) allow(key string) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()

	b, ok := l.buckets[key]
	if !ok || now.After(b.resetAt) {
		l.buckets[key] = &bucket{count: 1, resetAt: now.Add(l.window)}
		return true
	}
	b.count++
	return b.count <= l.max
}

// Allow records a hit for key and reports whether it is within budget. It is the
// exported seam over the same fixed-window accounting PerUserMiddleware uses, for
// non-HTTP callers (the Slack chat surface, PRD #191 Decision 9) that must draw from
// the SAME per-user budget as the web routes rather than silently bypassing the
// guard. Compose the key exactly as the middleware does — the route pattern, "|",
// then the user id — to share a bucket with that route's mount.
func (l *Limiter) Allow(key string) bool { return l.allow(key) }

// Window is the fixed window every key's budget resets over: the longest a refused caller can
// have to wait, which is what a Retry-After header advertises.
func (l *Limiter) Window() time.Duration { return l.window }

// Middleware limits by (route pattern, client IP, an IPv6 client by its /64). Apply
// it per-route so each endpoint gets its own budget.
func (l *Limiter) Middleware(next http.Handler) http.Handler {
	return l.MiddlewareRejecting(func(w http.ResponseWriter, _ int) {
		httpx.Error(w, http.StatusTooManyRequests, "too many requests")
	})(next)
}

// MiddlewareRejecting is Middleware with the refusal body chosen by the caller, for routes whose
// clients expect a protocol-shaped error (the OAuth token endpoint's RFC 6749 JSON). reject is
// called after Retry-After (the window in seconds) is set; it writes the status and body, and the
// request goes no further.
func (l *Limiter) MiddlewareRejecting(reject func(w http.ResponseWriter, retryAfterSeconds int)) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := r.URL.Path + "|" + rateLimitSubject(ClientIP(r, l.trustedProxies))
			if !l.allow(key) {
				secs := int(l.window.Seconds())
				w.Header().Set("Retry-After", strconv.Itoa(secs))
				reject(w, secs)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// PerUserMiddleware limits by (route pattern, authenticated user id). It MUST
// run after RequireAuth (which sets the user in context). Keying on the chi
// route pattern rather than the exact path means all calls to e.g.
// /repos/{id}/sync share one budget per user, so hitting many different repos
// or issues cannot bypass the limit. Used on the forge-proxying endpoints to
// keep one user from hammering the upstream forge. Falls back to the client IP
// if no user is in context (should not happen post-auth).
func (l *Limiter) PerUserMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pattern := chi.RouteContext(r.Context()).RoutePattern()
		if pattern == "" {
			pattern = r.URL.Path
		}
		var who string
		if user, ok := UserFromContext(r.Context()); ok {
			who = user.ID.String()
		} else {
			who = rateLimitSubject(ClientIP(r, l.trustedProxies))
		}
		if !l.allow(pattern + "|" + who) {
			w.Header().Set("Retry-After", strconv.Itoa(int(l.window.Seconds())))
			httpx.Error(w, http.StatusTooManyRequests, "too many requests")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// PerWorkerMiddleware limits by (route pattern, authenticated worker id). It MUST
// run after RequireWorker (which sets the worker in context). Used on the worker's
// proposal-creation endpoint so a single (possibly prompt-injected) worker cannot
// mass-create proposal rows across its user's chat runs, complementing the per-run
// pending cap. Falls back to the client IP if no worker is in context (should not
// happen post-auth).
func (l *Limiter) PerWorkerMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pattern := chi.RouteContext(r.Context()).RoutePattern()
		if pattern == "" {
			pattern = r.URL.Path
		}
		var who string
		if wkr, ok := WorkerFromContext(r.Context()); ok {
			who = wkr.ID.String()
		} else {
			who = rateLimitSubject(ClientIP(r, l.trustedProxies))
		}
		if !l.allow(pattern + "|" + who) {
			w.Header().Set("Retry-After", strconv.Itoa(int(l.window.Seconds())))
			httpx.Error(w, http.StatusTooManyRequests, "too many requests")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// rateLimitSubject is the source a client IP is rate-limited as. An IPv6 client
// usually controls at least a /64 and can rotate source addresses inside it, so an
// IPv6 address is keyed on its /64, the same bucket as the OAuth authorize flow's
// finest tier (oauthSourceBucketsFor in the handler package). An IPv4-mapped address
// is its IPv4, and a 6to4 (2002::/16) address is the IPv4 address it embeds, so one
// IPv4 host cannot mint a budget per /64; a NAT64 well-known-prefix (64:ff9b::/96)
// address is likewise its embedded IPv4. Residual: a holder of a larger delegation
// (/56, /48) still gets one budget per /64 inside it; Teredo (2001::/32) and a
// network-specific or local-use NAT64 prefix are not mapped. All clients of one
// Teredo server share one budget. Behind such a translator, clients share a budget
// per IPv4 range that the prefix layout (RFC 6052) places in the /64: all of them
// for a /64 or /96 prefix, one per IPv4 /8, /16 or /24 for a /56, /48 or /40, and
// none for a /32. An unparsable input is returned unchanged.
func rateLimitSubject(clientIP string) string {
	addr, err := netip.ParseAddr(clientIP)
	if err != nil {
		return clientIP
	}
	addr = addr.WithZone("").Unmap()
	if addr.Is6() {
		if b := addr.As16(); b[0] == 0x20 && b[1] == 0x02 {
			// 6to4: bits 16..47 are the embedded IPv4 address.
			addr = netip.AddrFrom4([4]byte{b[2], b[3], b[4], b[5]})
		} else if nat64WellKnown.Contains(addr) {
			// NAT64/SIIT well-known prefix (RFC 6052): the low 32 bits are the IPv4 client.
			// Keyed on its /64, every IPv4 client behind such an edge would share one budget.
			addr = netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]})
		}
	}
	if addr.Is4() {
		return addr.String()
	}
	return netip.PrefixFrom(addr, 64).Masked().String()
}

// nat64WellKnown is the RFC 6052 NAT64 well-known prefix.
var nat64WellKnown = netip.MustParsePrefix("64:ff9b::/96")

// ClientIP determines the real client IP, honoring X-Forwarded-For ONLY when the
// direct connection (RemoteAddr) comes from a trusted-proxy CIDR; otherwise the
// canonical RemoteAddr is used, so a spoofed header from an untrusted peer cannot
// forge a client IP. Empty trustedProxies => never trust XFF.
func ClientIP(r *http.Request, trustedProxies []*net.IPNet) string {
	// The direct peer's host, without any port suffix.
	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}

	// X-Forwarded-For is only trustworthy when the peer that set it is itself a
	// trusted proxy. With no trusted proxies, or a direct peer that is not one, the
	// header is attacker-controlled and must be ignored — the peer's own IP stands.
	peer := net.ParseIP(host)
	if len(trustedProxies) == 0 || peer == nil || !isTrustedProxy(peer, trustedProxies) {
		return host
	}

	xff := r.Header.Get("X-Forwarded-For")
	if xff == "" {
		return host
	}

	// nginx overwrites XFF and appends the immediate client, so the real client is
	// the rightmost entry that is not itself one of our trusted proxies. Walk the
	// chain from the trusted hops (rightmost) back toward the client and return the
	// first entry outside the trusted set.
	entries := strings.Split(xff, ",")
	for i := len(entries) - 1; i >= 0; i-- {
		candidate := strings.TrimSpace(entries[i])
		ip := net.ParseIP(candidate)
		if ip == nil {
			continue
		}
		if isTrustedProxy(ip, trustedProxies) {
			continue
		}
		return candidate
	}

	// Every parsable hop was trusted (or the header held nothing usable): fall back
	// to the direct peer rather than inventing a client IP.
	return host
}

// isTrustedProxy reports whether ip falls within any of the CIDRs.
func isTrustedProxy(ip net.IP, cidrs []*net.IPNet) bool {
	for _, cidr := range cidrs {
		if cidr != nil && cidr.Contains(ip) {
			return true
		}
	}
	return false
}
