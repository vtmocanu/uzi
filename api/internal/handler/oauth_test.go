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

// The fairness bucket of the authorize pending cap: the IPv4 address, or the IPv6 /64 in
// canonical form; addresses of one /64 share it, different /64s and IPv4 addresses do not.
func TestOAuthSourcePrefix(t *testing.T) {
	tests := []struct{ in, want string }{
		{"203.0.113.9", "203.0.113.9"},
		{"203.0.113.10", "203.0.113.10"},
		{"::ffff:203.0.113.9", "203.0.113.9"},
		{"2001:db8:1:2::1", "2001:db8:1:2::/64"},
		{"2001:db8:1:2:ffff:ffff:ffff:ffff", "2001:db8:1:2::/64"},
		{"2001:DB8:1:2:0:0:0:7", "2001:db8:1:2::/64"},
		{"2001:db8:1:3::1", "2001:db8:1:3::/64"},
		{"fe80::1%eth0", "fe80::/64"},
		{"::1", "::/64"},
		{"", ""},
		{"not-an-ip", ""},
	}
	for _, tt := range tests {
		if got := oauthSourcePrefix(tt.in); got != tt.want {
			t.Errorf("oauthSourcePrefix(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
	if oauthSourcePrefix("2001:db8:1:2::1") != oauthSourcePrefix("2001:db8:1:2:aaaa::2") {
		t.Error("two addresses of one /64 must share a bucket")
	}
	if oauthSourcePrefix("2001:db8:1:2::1") == oauthSourcePrefix("2001:db8:1:3::1") {
		t.Error("different /64s must not share a bucket")
	}
}
