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
// single-process demo. It fails closed on the limit and prunes expired buckets
// opportunistically.
type Limiter struct {
	mu      sync.Mutex
	max     int
	window  time.Duration
	buckets map[string]*bucket
}

type bucket struct {
	count   int
	resetAt time.Time
}

// NewLimiter returns a limiter allowing max requests per window per key.
func NewLimiter(max int, window time.Duration) *Limiter {
	return &Limiter{
		max:     max,
		window:  window,
		buckets: make(map[string]*bucket),
	}
}

// allow records a hit for key and reports whether it is within budget.
func (l *Limiter) allow(key string) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()

	if len(l.buckets) > 10000 {
		l.pruneLocked(now)
	}

	b, ok := l.buckets[key]
	if !ok || now.After(b.resetAt) {
		l.buckets[key] = &bucket{count: 1, resetAt: now.Add(l.window)}
		return true
	}
	b.count++
	return b.count <= l.max
}

func (l *Limiter) pruneLocked(now time.Time) {
	for k, b := range l.buckets {
		if now.After(b.resetAt) {
			delete(l.buckets, k)
		}
	}
}

// Middleware limits by (route pattern, client IP). Apply it per-route so each
// endpoint gets its own budget.
func (l *Limiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.URL.Path + "|" + ClientIP(r)
		if !l.allow(key) {
			w.Header().Set("Retry-After", strconv.Itoa(int(l.window.Seconds())))
			httpx.Error(w, http.StatusTooManyRequests, "too many requests")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ClientIP returns the real client IP. Our nginx appends the connecting
// client's address to X-Forwarded-For (via $proxy_add_x_forwarded_for), so the
// rightmost entry is the hop nginx observed. We trust only that hop; any
// left-of-it values are client-supplied and ignored. Falls back to RemoteAddr
// when no XFF is present (e.g. direct hits during local testing).
func ClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		last := strings.TrimSpace(parts[len(parts)-1])
		if ip := net.ParseIP(last); ip != nil {
			return ip.String()
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.String()
	}
	return host
}
