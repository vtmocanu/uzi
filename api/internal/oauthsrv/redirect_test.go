package oauthsrv

import (
	"strings"
	"testing"
)

func TestValidateRedirectURI(t *testing.T) {
	long := "https://example.com/" + strings.Repeat("a", MaxRedirectURIBytes)
	exact := "https://example.com/" + strings.Repeat("a", MaxRedirectURIBytes-len("https://example.com/"))
	cases := []struct {
		name string
		uri  string
		ok   bool
	}{
		{"https", "https://app.example.com/oauth/callback", true},
		{"https with port and query", "https://app.example.com:8443/cb?x=1", true},
		{"https localhost is a name but https is fine", "https://localhost/cb", true},
		{"https at the byte cap", exact, true},
		{"loopback v4", "http://127.0.0.1:8123/cb", true},
		{"loopback v6", "http://[::1]:8123/cb", true},
		{"loopback v4 no path", "http://127.0.0.1:8123", true},

		{"empty", "", false},
		{"over the byte cap", long, false},
		{"http non-loopback host", "http://app.example.com/cb", false},
		{"http localhost name", "http://localhost:8080/cb", false},
		{"http other loopback address", "http://127.0.0.2:8080/cb", false},
		{"http loopback without port", "http://127.0.0.1/cb", false},
		{"http v6 loopback without port", "http://[::1]/cb", false},
		{"http loopback port zero", "http://127.0.0.1:0/cb", false},
		{"http loopback port too large", "http://127.0.0.1:70000/cb", false},
		{"http loopback non-numeric port", "http://127.0.0.1:abc/cb", false},
		{"http private address", "http://10.0.0.1:80/cb", false},
		{"fragment", "https://app.example.com/cb#frag", false},
		{"empty fragment", "https://app.example.com/cb#", false},
		{"userinfo", "https://user:pw@app.example.com/cb", false},
		{"userinfo name only", "https://user@app.example.com/cb", false},
		{"custom scheme", "myapp://cb", false},
		{"javascript scheme", "javascript:alert(1)", false},
		{"data scheme", "data:text/html,x", false},
		{"relative", "/cb", false},
		{"scheme-relative", "//app.example.com/cb", false},
		{"uppercase scheme", "HTTPS://app.example.com/cb", false},
		{"no host", "https:///cb", false},
		{"space", "https://app.example.com/a b", false},
		{"leading space", " https://app.example.com/cb", false},
		{"newline", "https://app.example.com/cb\n", false},
		{"tab", "https://app.example.com/\tcb", false},
		{"non-ascii", "https://app.example.com/café", false},
		{"nul byte", "https://app.example.com/\x00", false},
		{"unterminated v6 host", "https://[::1/cb", false},
		{"query with a harmless key", "https://app.example.com/cb?tenant=a&codex=1&states=2", true},
		{"reserved key code", "https://app.example.com/cb?code=x", false},
		{"reserved key state", "https://app.example.com/cb?a=1&state=x", false},
		{"reserved key iss", "https://app.example.com/cb?iss=https://x", false},
		{"reserved key error", "https://app.example.com/cb?error=x", false},
		{"reserved key error_description", "https://app.example.com/cb?error_description=x", false},
		{"reserved key error_uri", "https://app.example.com/cb?error_uri=x", false},
		{"reserved key without a value", "https://app.example.com/cb?code", false},
		{"reserved key percent-encoded", "https://app.example.com/cb?%63ode=x", false},
		{"empty pair then a reserved key", "https://app.example.com/cb?a=b&&code=x", false},
		{"undecodable key is refused", "https://app.example.com/cb?%zz=1", false},
		{"reserved key is case-sensitive", "https://app.example.com/cb?Code=x&STATE=y", true},
		{"loopback with reserved key", "http://127.0.0.1:8123/cb?code=x", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateRedirectURI(tc.uri)
			if tc.ok && err != nil {
				t.Fatalf("ValidateRedirectURI(%q) = %v, want nil", tc.uri, err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("ValidateRedirectURI(%q) = nil, want an error", tc.uri)
			}
		})
	}
}

func TestValidateRedirectURIs(t *testing.T) {
	mk := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = "https://app.example.com/cb" + strings.Repeat("x", i)
		}
		return out
	}
	cases := []struct {
		name string
		in   []string
		ok   bool
	}{
		{"nil clears", nil, true},
		{"empty clears", []string{}, true},
		{"one", mk(1), true},
		{"five is the cap", mk(5), true},
		{"six is refused", mk(6), false},
		{"duplicate", []string{"https://a.example.com/cb", "https://a.example.com/cb"}, false},
		{"one bad entry among good", []string{"https://a.example.com/cb", "http://a.example.com/cb"}, false},
		{"empty entry", []string{""}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateRedirectURIs(tc.in)
			if tc.ok != (err == nil) {
				t.Fatalf("ValidateRedirectURIs(%v) = %v, want ok=%v", tc.in, err, tc.ok)
			}
		})
	}
}

func TestValidateRedirectURIsErrorDoesNotEchoTheURI(t *testing.T) {
	err := ValidateRedirectURIs([]string{"https://secret-host.example.com/cb#x"})
	if err == nil || strings.Contains(err.Error(), "secret-host") {
		t.Fatalf("err = %v, want a non-echoing error", err)
	}
}
