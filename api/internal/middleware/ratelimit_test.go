package middleware

import (
	"net"
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
