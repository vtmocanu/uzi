package config

import (
	"fmt"
	"strings"
	"testing"
)

type allowlistParser struct {
	env   string
	parse func(raw string) ([]string, error)
}

func allowlistParsers() []allowlistParser {
	return []allowlistParser{
		{"FORGE_ALLOWED_BASE_URLS", parseAllowedBaseURLs},
		{"AGENT_SOURCE_ALLOWED_BASE_URLS", parseAgentSourceAllowedBaseURLs},
		{"UZI_PRODUCT_SKILLS_ALLOWED_BASE_URLS", func(raw string) ([]string, error) {
			return parseBaseURLAllowlist("UZI_PRODUCT_SKILLS_ALLOWED_BASE_URLS", raw)
		}},
		{"ISSUE_TRACKER_ALLOWED_BASE_URLS", func(raw string) ([]string, error) {
			return parseBaseURLAllowlist("ISSUE_TRACKER_ALLOWED_BASE_URLS", raw)
		}},
	}
}

const userinfoReason = "contains userinfo"

// TestAllowlistEntriesRefuseUnsafeForms: an entry with userinfo, a path, a query or a fragment is
// refused (not stripped to a different host), at its 1-based position, and the error never echoes
// any part of the entry.
func TestAllowlistEntriesRefuseUnsafeForms(t *testing.T) {
	cases := []struct {
		entry    string
		reason   string
		userinfo bool
	}{
		{"https://tracker.example.com@evil.example.com", userinfoReason, true},
		{"https://user:s3cret@forge.example.com", userinfoReason, true},
		{"https://@forge.example.com", userinfoReason, true},
		{"https://u:s3cret@forge.example.com/%zz", userinfoReason, true},
		{"https://forge.example.com/some/path", "has a path", false},
		{"https://forge.example.com//", "has a path", false},
		{"https://forge.example.com?x=1", "has a query", false},
		{"https://forge.example.com?", "has a query", false},
		{"https://forge.example.com#frag", "has a fragment", false},
		{"https://forge.example.com#", "has a fragment", false},
	}
	for _, p := range allowlistParsers() {
		for _, tc := range cases {
			for _, pos := range []int{1, 2} {
				raw := tc.entry
				if pos == 2 {
					raw = "https://ok.example.com," + tc.entry
				}
				name := fmt.Sprintf("%s/%s/entry%d", p.env, tc.entry, pos)
				t.Run(name, func(t *testing.T) {
					_, err := p.parse(raw)
					if err == nil {
						t.Fatalf("parse(%q) accepted the entry", raw)
					}
					msg := err.Error()
					want := []string{p.env, fmt.Sprintf("entry %d", pos), tc.reason}
					for _, w := range want {
						if !strings.Contains(msg, w) {
							t.Errorf("error %q lacks %q", msg, w)
						}
					}
					// The fixed reason text itself says "user:password@", so the entry's own
					// "user:" prefix is checked together with its secret ("user:s3").
					forbidden := []string{"s3cret", "user:s3", "tracker.example.com", "evil.example.com"}
					if !tc.userinfo {
						forbidden = append(forbidden, "forge.example.com")
					}
					for _, f := range forbidden {
						if strings.Contains(msg, f) {
							t.Errorf("error %q leaks %q", msg, f)
						}
					}
				})
			}
		}
	}
}

func TestAllowlistEntriesAcceptBareHosts(t *testing.T) {
	cases := map[string]string{
		"https://forge.example.com":       "https://forge.example.com",
		"https://forge.example.com/":      "https://forge.example.com",
		"https://forge.example.com:8443":  "https://forge.example.com:8443",
		"https://forge.example.com:8443/": "https://forge.example.com:8443",
	}
	for _, p := range allowlistParsers() {
		for in, want := range cases {
			got, err := p.parse(in)
			if err != nil {
				t.Errorf("%s: parse(%q): %v", p.env, in, err)
				continue
			}
			if len(got) != 1 || got[0] != want {
				t.Errorf("%s: parse(%q) = %v, want [%s]", p.env, in, got, want)
			}
		}
	}
}

// TestLoadRefusesUserinfoAllowlistEntries: Load fails the boot naming the variable, without
// echoing the credential, for each of the four allowlists.
func TestLoadRefusesUserinfoAllowlistEntries(t *testing.T) {
	for _, p := range allowlistParsers() {
		t.Run(p.env, func(t *testing.T) {
			autoselectEnv(t)
			t.Setenv(p.env, "https://user:s3cret@forge.example.com")
			_, err := Load()
			if err == nil {
				t.Fatal("Load accepted a userinfo entry")
			}
			if !strings.Contains(err.Error(), p.env) {
				t.Errorf("boot error %q does not name %s", err, p.env)
			}
			if strings.Contains(err.Error(), "s3cret") {
				t.Errorf("boot error %q leaks the credential", err)
			}
		})
	}
}
