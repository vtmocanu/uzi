package middleware

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func mustCIDRs(t *testing.T, cidrs ...string) []*net.IPNet {
	t.Helper()
	var out []*net.IPNet
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			t.Fatalf("parse cidr %q: %v", c, err)
		}
		out = append(out, n)
	}
	return out
}

func TestClientIP(t *testing.T) {
	trusted := mustCIDRs(t, "172.16.0.0/12")

	tests := []struct {
		name       string
		remoteAddr string
		xff        string
		proxies    []*net.IPNet
		want       string
	}{
		{
			name:       "no trusted proxies ignores xff",
			remoteAddr: "172.18.0.5:5000",
			xff:        "203.0.113.9",
			proxies:    nil,
			want:       "172.18.0.5",
		},
		{
			name:       "trusted remote honors xff",
			remoteAddr: "172.18.0.5:5000",
			xff:        "203.0.113.9",
			proxies:    trusted,
			want:       "203.0.113.9",
		},
		{
			name:       "untrusted remote ignores spoofed xff",
			remoteAddr: "8.8.8.8:5000",
			xff:        "203.0.113.9",
			proxies:    trusted,
			want:       "8.8.8.8",
		},
		{
			name:       "rightmost non-trusted is picked",
			remoteAddr: "172.18.0.5:5000",
			xff:        "203.0.113.9, 172.18.0.7",
			proxies:    trusted,
			want:       "203.0.113.9",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/api/auth/login", nil)
			r.RemoteAddr = tt.remoteAddr
			if tt.xff != "" {
				r.Header.Set("X-Forwarded-For", tt.xff)
			}
			if got := ClientIP(r, tt.proxies); got != tt.want {
				t.Errorf("ClientIP = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLimiterAllows(t *testing.T) {
	l := NewLimiter(2, time.Hour, nil)
	if !l.allow("k") {
		t.Fatal("first request should pass")
	}
	if !l.allow("k") {
		t.Fatal("second request should pass")
	}
	if l.allow("k") {
		t.Fatal("third request should be limited")
	}
	if !l.allow("other") {
		t.Fatal("different key should have its own budget")
	}
}

// TestMiddlewareKeysIPv6OnItsSlash64 is the #2075 regression: the IP-keyed middleware
// used the full client address, so an IPv6 client rotating addresses inside its /64
// got a fresh budget per address on every authLimiter route (login included).
func TestMiddlewareKeysIPv6OnItsSlash64(t *testing.T) {
	h := NewLimiter(2, time.Hour, nil).Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	hit := func(remote string) int {
		r := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
		r.RemoteAddr = remote
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	steps := []struct {
		remote string
		want   int
	}{
		// Three addresses in one /64 share one budget of 2.
		{"[2001:db8:1:2::1]:1", http.StatusOK},
		{"[2001:db8:1:2::abcd]:2", http.StatusOK},
		{"[2001:db8:1:2:ffff::9]:3", http.StatusTooManyRequests},
		// The neighbouring /64 has its own budget.
		{"[2001:db8:1:3::1]:1", http.StatusOK},
		// IPv4 stays per address.
		{"192.0.2.1:1", http.StatusOK},
		{"192.0.2.2:1", http.StatusOK},
		// An IPv4-mapped address and 6to4 addresses embedding 192.0.2.1 (c000:0201)
		// share 192.0.2.1's bucket, whichever /64 the 6to4 address picks.
		{"[::ffff:192.0.2.1]:1", http.StatusOK},
		{"[2002:c000:201:1::1]:1", http.StatusTooManyRequests},
		{"[2002:c000:201:ffff::1]:1", http.StatusTooManyRequests},
		// NAT64 well-known-prefix clients keep their own IPv4 budgets: one client
		// exhausting its own does not lock out another IPv4 client behind the edge.
		{"[64:ff9b::c633:6407]:1", http.StatusOK},
		{"[64:ff9b::c633:6407]:2", http.StatusOK},
		{"[64:ff9b::c633:6407]:3", http.StatusTooManyRequests},
		{"[64:ff9b::cb00:7109]:1", http.StatusOK},
		// ...and the translated client is keyed as its IPv4 (198.51.100.7).
		{"198.51.100.7:1", http.StatusTooManyRequests},
	}
	for i, s := range steps {
		if got := hit(s.remote); got != s.want {
			t.Fatalf("step %d: request from %s = %d, want %d", i, s.remote, got, s.want)
		}
	}
}

func TestRateLimitSubject(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{"203.0.113.9", "203.0.113.9"},
		{"::ffff:203.0.113.9", "203.0.113.9"},
		{"2001:db8:1:2:3:4:5:6", "2001:db8:1:2::/64"},
		{"2001:db8:1:2::", "2001:db8:1:2::/64"},
		{"fe80::1%eth0", "fe80::/64"},
		{"2002:cb00:7109:1234::1", "203.0.113.9"},
		{"64:ff9b::cb00:7109", "203.0.113.9"},
		{"64:ff9b:1::cb00:7109", "64:ff9b:1::/64"},
		// A /48 RFC 6052 layout puts only the IPv4's first 16 bits in the /64:
		// 198.51.100.7 and 198.51.200.9 share 64:ff9b:1:c633::/64.
		{"64:ff9b:1:c633:64:700::", "64:ff9b:1:c633::/64"},
		{"64:ff9b:1:c633:c8:900::", "64:ff9b:1:c633::/64"},
		{"2001:0:4136:e378:8000:63bf:3fff:fdd2", "2001:0:4136:e378::/64"},
		{"not-an-ip", "not-an-ip"},
		{"", ""},
	} {
		if got := rateLimitSubject(tt.in); got != tt.want {
			t.Errorf("rateLimitSubject(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
