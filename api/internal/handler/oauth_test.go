package handler

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// PRD #1910 M2, D3: the authorize paths that are refused before any database read. A bare
// Handler is enough: none of these reaches h.q.

func TestOAuthAuthorizeStaticErrorWithoutDB(t *testing.T) {
	const stateSentinel = "STATE-SENTINEL-77"
	const uriSentinel = "https://evil.example/REDIRECT-SENTINEL"
	good := "11111111-1111-1111-1111-111111111111"
	tests := []struct {
		name  string
		query string
	}{
		{"no parameters", ""},
		{"client_id not a uuid", "client_id=nope&redirect_uri=" + url.QueryEscape(uriSentinel) + "&state=" + stateSentinel},
		{"client_id repeated", "client_id=" + good + "&client_id=" + good + "&redirect_uri=" + url.QueryEscape(uriSentinel) + "&state=" + stateSentinel},
		{"redirect_uri repeated", "client_id=" + good + "&redirect_uri=a&redirect_uri=b&state=" + stateSentinel},
		{"malformed percent escape", "client_id=" + good + "&redirect_uri=%zz&state=" + stateSentinel},
	}
	h := &Handler{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/oauth/authorize?"+tt.query, nil)
			rec := httptest.NewRecorder()
			h.OAuthAuthorize(rec, req)
			assertOAuthStaticError(t, rec, stateSentinel, uriSentinel)
		})
	}
}

// assertOAuthStaticError pins RFC 6749 section 4.1.2.1: an untrusted client_id / redirect_uri
// never redirects, never sets the binding cookie and never echoes a parameter.
func assertOAuthStaticError(t *testing.T, rec *httptest.ResponseRecorder, sentinels ...string) {
	t.Helper()
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "" {
		t.Errorf("Location = %q, want none", loc)
	}
	if sc := rec.Header()["Set-Cookie"]; len(sc) != 0 {
		t.Errorf("Set-Cookie = %v, want none", sc)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	body := rec.Body.String()
	if body != oauthStaticErrorPage {
		t.Errorf("body is not the fixed static page: %q", body)
	}
	for _, s := range sentinels {
		if strings.Contains(body, s) {
			t.Errorf("body echoes %q", s)
		}
	}
}

func TestWellFormedOAuthNonce(t *testing.T) {
	good := strings.Repeat("A", 43)
	for v, want := range map[string]bool{
		good:                          true,
		"":                            false,
		good[:42]:                     false,
		good + "A":                    false,
		strings.Repeat("A", 42) + "!": false,
		strings.Repeat("A", 42) + "=": false,
	} {
		if got := wellFormedOAuthNonce(v); got != want {
			t.Errorf("wellFormedOAuthNonce(%q) = %v, want %v", v, got, want)
		}
	}
}

// The fairness buckets of the authorize pending cap, finest first: IPv6 /64, /56, /48; IPv4 the
// address, then its /24 twice; a 6to4 address is the IPv4 it embeds.
func TestOAuthSourceBucketsFor(t *testing.T) {
	tests := []struct{ in, fine, mid, wide string }{
		{"203.0.113.9", "203.0.113.9", "203.0.113.0/24", "203.0.113.0/24"},
		{"::ffff:203.0.113.9", "203.0.113.9", "203.0.113.0/24", "203.0.113.0/24"},
		{"2001:db8:1:2::1", "2001:db8:1:2::/64", "2001:db8:1::/56", "2001:db8:1::/48"},
		{"2001:DB8:1:2:ffff:ffff:ffff:ffff", "2001:db8:1:2::/64", "2001:db8:1::/56", "2001:db8:1::/48"},
		{"2001:db8:1:ff::1", "2001:db8:1:ff::/64", "2001:db8:1::/56", "2001:db8:1::/48"},
		{"2001:db8:1:100::1", "2001:db8:1:100::/64", "2001:db8:1:100::/56", "2001:db8:1::/48"},
		{"2001:db8:2::1", "2001:db8:2::/64", "2001:db8:2::/56", "2001:db8:2::/48"},
		{"fe80::1%eth0", "fe80::/64", "fe80::/56", "fe80::/48"},
		{"::1", "::/64", "::/56", "::/48"},
		{"2002:cb00:7109::1", "203.0.113.9", "203.0.113.0/24", "203.0.113.0/24"},
		{"2002:cb00:7109:1234:5678::9", "203.0.113.9", "203.0.113.0/24", "203.0.113.0/24"},
		{"", "", "", ""},
		{"not-an-ip", "", "", ""},
	}
	for _, tt := range tests {
		got := oauthSourceBucketsFor(tt.in)
		if got.Fine != tt.fine || got.Mid != tt.mid || got.Wide != tt.wide {
			t.Errorf("oauthSourceBucketsFor(%q) = %q / %q / %q, want %q / %q / %q", tt.in, got.Fine, got.Mid, got.Wide, tt.fine, tt.mid, tt.wide)
		}
	}
	a, b := oauthSourceBucketsFor("2001:db8:1:2::1"), oauthSourceBucketsFor("2001:db8:1:2:aaaa::2")
	if a != b {
		t.Error("two addresses of one /64 must share every bucket")
	}
	c := oauthSourceBucketsFor("2001:db8:1:3::1")
	if a.Fine == c.Fine || a.Mid != c.Mid || a.Wide != c.Wide {
		t.Errorf("a different /64 of the same /56 must share mid and wide only: %+v vs %+v", a, c)
	}
	if x, y := oauthSourceBucketsFor("2002:cb00:7109::1"), oauthSourceBucketsFor("2002:cb00:7109:ffff::1"); x != y {
		t.Error("every /64 of one 6to4 address must be one bucket set")
	}
	if v4, v6 := oauthSourceBucketsFor("203.0.113.9"), oauthSourceBucketsFor("2001:db8::1"); v4.MidCap != oauthPendingV4MidCap || v6.MidCap != oauthPendingV6MidCap || v6.WideCap != oauthPendingV6WideCap {
		t.Errorf("tier caps: v4 %+v, v6 %+v", v4, v6)
	}
}
