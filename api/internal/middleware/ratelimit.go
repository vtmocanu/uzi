// Package middleware holds HTTP middleware: authentication, admin gating and
// rate limiting.
package middleware

import (
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"gitlab.example.com/vtmocanu/uzi/api/internal/httpx"
)

// Limiter is an in-process, per-IP fixed-window rate limiter. It intentionally
// avoids an external dependency (multica uses Redis) since the MVP is a
// single-process demo. Expired buckets are reclaimed both lazily (on access)
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

// Middleware limits by (route pattern, client IP). Apply it per-route so each
// endpoint gets its own budget.
func (l *Limiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.URL.Path + "|" + ClientIP(r, l.trustedProxies)
		if !l.allow(key) {
			w.Header().Set("Retry-After", strconv.Itoa(int(l.window.Seconds())))
			httpx.Error(w, http.StatusTooManyRequests, "too many requests")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ClientIP determines the real client IP. It honors X-Forwarded-For ONLY when
// the direct connection (RemoteAddr) comes from a trusted proxy CIDR — our
// nginx overwrites XFF with $remote_addr, so behind nginx the trusted hop
// carries the real client. When RemoteAddr is not trusted (e.g. someone hits
// the API directly, bypassing nginx) any XFF header is ignored, so a spoofed
// header cannot forge a client IP. Empty trustedProxies => never trust XFF.
// Mirrors multica's audited extractIP (rightmost non-trusted entry).
func ClientIP(r *http.Request, trustedProxies []*net.IPNet) string {
	remoteHost, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		remoteHost = r.RemoteAddr
	}

	if len(trustedProxies) > 0 {
		if remoteIP := net.ParseIP(remoteHost); remoteIP != nil && isTrustedProxy(remoteIP, trustedProxies) {
			if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
				parts := strings.Split(xff, ",")
				for i := len(parts) - 1; i >= 0; i-- {
					candidate := net.ParseIP(strings.TrimSpace(parts[i]))
					if candidate != nil && !isTrustedProxy(candidate, trustedProxies) {
						return candidate.String()
					}
				}
			}
		}
	}

	if ip := net.ParseIP(remoteHost); ip != nil {
		return ip.String()
	}
	return remoteHost
}

func isTrustedProxy(ip net.IP, cidrs []*net.IPNet) bool {
	for _, cidr := range cidrs {
		if cidr.Contains(ip) {
			return true
		}
	}
	return false
}
