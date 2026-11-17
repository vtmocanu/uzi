package providers

import (
	"strings"
	"testing"
)

func TestNormalizeBaseURL(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		loopback bool
		want     string // empty = must be refused
	}{
		{"exact", "https://gw.example.com/anthropic", false, "https://gw.example.com/anthropic"},
		{"trailing slash removed", "https://gw.example.com/anthropic/", false, "https://gw.example.com/anthropic"},
		{"uppercase scheme and host", "HTTPS://GW.Example.COM/anthropic", false, "https://gw.example.com/anthropic"},
		{"path case preserved", "https://gw.example.com/Anthropic", false, "https://gw.example.com/Anthropic"},
		{"default https port elided", "https://gw.example.com:443/anthropic", false, "https://gw.example.com/anthropic"},
		{"non-default port kept", "https://gw.example.com:8443/anthropic", false, "https://gw.example.com:8443/anthropic"},
		{"host only", "https://gw.example.com", false, "https://gw.example.com"},
		{"host only trailing slash", "https://gw.example.com/", false, "https://gw.example.com"},
		{"dot segment", "https://gw.example.com/anthropic/../other", false, ""},
		{"single dot segment", "https://gw.example.com/./anthropic", false, ""},
		{"empty segment", "https://gw.example.com//anthropic", false, ""},
		{"double trailing slash", "https://gw.example.com/anthropic//", false, ""},
		{"encoded dot lower", "https://gw.example.com/%2e%2e/other", false, ""},
		{"encoded dot upper", "https://gw.example.com/%2E/other", false, ""},
		{"encoded slash lower", "https://gw.example.com/a%2fb", false, ""},
		{"encoded slash upper", "https://gw.example.com/a%2Fb", false, ""},
		{"any percent", "https://gw.example.com/a%20b", false, ""},
		{"userinfo", "https://user:pw@gw.example.com/anthropic", false, ""},
		{"userinfo at only", "https://@gw.example.com/anthropic", false, ""},
		{"semicolon", "https://gw.example.com/anthropic;x=1", false, ""},
		{"backslash", "https://gw.example.com\\anthropic", false, ""},
		{"backslash path", "https://gw.example.com/a\\..\\b", false, ""},
		{"query", "https://gw.example.com/anthropic?x=1", false, ""},
		{"empty query marker", "https://gw.example.com/anthropic?", false, ""},
		{"fragment", "https://gw.example.com/anthropic#f", false, ""},
		{"http remote", "http://gw.example.com/anthropic", false, ""},
		{"http remote with flag", "http://gw.example.com/anthropic", true, ""},
		{"ftp", "ftp://gw.example.com/anthropic", false, ""},
		{"no scheme", "gw.example.com/anthropic", false, ""},
		{"opaque", "https:gw.example.com", false, ""},
		{"empty host", "https:///anthropic", false, ""},
		{"non-ascii host", "https://gw.exämple.com/anthropic", false, ""},
		{"whitespace", "https://gw.example.com/any thing", false, ""},
		{"control char", "https://gw.example.com/a\nb", false, ""},
		{"bad port", "https://gw.example.com:99999/a", false, ""},
		{"empty port", "https://gw.example.com:/a", false, ""},
		{"non-numeric port", "https://gw.example.com:abc/a", false, ""},
		{"empty", "", false, ""},
		{"too long", "https://gw.example.com/" + strings.Repeat("a", MaxBaseURLBytes), false, ""},
		{"loopback https without flag", "https://localhost:8443/anthropic", false, ""},
		{"loopback http without flag", "http://127.0.0.1:9000/anthropic", false, ""},
		{"loopback http with flag", "http://127.0.0.1:9000/anthropic", true, "http://127.0.0.1:9000/anthropic"},
		{"loopback localhost default port with flag", "http://LOCALHOST:80/x/", true, "http://localhost/x"},
		{"loopback https with flag", "https://localhost/x", true, "https://localhost/x"},
		{"ipv6 loopback with flag", "http://[::1]:9000/x", true, "http://[::1]:9000/x"},
		{"ipv6 loopback without flag", "https://[::1]/x", false, ""},
	}
	for _, c := range cases {
		got, err := NormalizeBaseURL(c.in, c.loopback)
		if c.want == "" {
			if err == nil {
				t.Errorf("%s: NormalizeBaseURL(%q) = %q, want refusal", c.name, c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: NormalizeBaseURL(%q): %v", c.name, c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s: NormalizeBaseURL(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}

func TestNormalizeBaseURLErrorDoesNotEchoInput(t *testing.T) {
	_, err := NormalizeBaseURL("https://user:supersecret@gw.example.com/a", false)
	if err == nil || strings.Contains(err.Error(), "supersecret") {
		t.Fatalf("err = %v, want a refusal that does not echo the input", err)
	}
}

func TestMatchAllowed(t *testing.T) {
	list := []string{"https://gw.example.com/anthropic", "https://other.example.com:8443/team-a"}
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"exact", "https://gw.example.com/anthropic", "https://gw.example.com/anthropic"},
		{"trailing slash and case", "HTTPS://GW.example.com/anthropic/", "https://gw.example.com/anthropic"},
		{"second entry", "https://other.example.com:8443/team-a", "https://other.example.com:8443/team-a"},
		{"other path", "https://gw.example.com/other", ""},
		{"prefix is not a match", "https://gw.example.com/anthropic/v1", ""},
		{"parent is not a match", "https://gw.example.com", ""},
		{"dot segment", "https://gw.example.com/anthropic/../other", ""},
		{"look-alike host", "https://gw.example.com.evil.test/anthropic", ""},
		{"suffix host", "https://evilgw.example.com/anthropic", ""},
		{"userinfo host trick", "https://gw.example.com@evil.test/anthropic", ""},
		{"encoded", "https://gw.example.com/anthro%70ic", ""},
		{"wrong port", "https://gw.example.com:8443/anthropic", ""},
	}
	for _, c := range cases {
		got, ok := MatchAllowed(list, c.in, false)
		if (c.want != "") != ok || got != c.want {
			t.Errorf("%s: MatchAllowed(%q) = (%q, %v), want %q", c.name, c.in, got, ok, c.want)
		}
	}
	if got, ok := MatchAllowed(nil, "https://gw.example.com/anthropic", false); ok || got != "" {
		t.Errorf("empty list matched: (%q, %v), want feature off", got, ok)
	}
	if _, ok := MatchAllowed([]string{"http://127.0.0.1:9000/a"}, "http://127.0.0.1:9000/a", false); ok {
		t.Error("loopback candidate matched without the dev flag")
	}
	if got, ok := MatchAllowed([]string{"http://127.0.0.1:9000/a"}, "http://127.0.0.1:9000/a/", true); !ok || got != "http://127.0.0.1:9000/a" {
		t.Errorf("loopback candidate with flag = (%q, %v)", got, ok)
	}
}
