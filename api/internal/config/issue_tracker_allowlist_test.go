package config

import (
	"os"
	"strings"
	"testing"
)

// TestLoadReadsIssueTrackerAllowlist (PRD #4173 D8): the variable reaches Config, defaults to an
// empty list while the instance still boots, and a non-https entry fails the boot naming the variable.
func TestLoadReadsIssueTrackerAllowlist(t *testing.T) {
	autoselectEnv(t)
	const env = "ISSUE_TRACKER_ALLOWED_BASE_URLS"

	t.Setenv(env, "")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load with the variable empty: %v", err)
	}
	if cfg.IssueTrackerAllowedBaseURLs != nil {
		t.Errorf("empty variable gave %v, want nil", cfg.IssueTrackerAllowedBaseURLs)
	}

	// Truly unset, not just empty: t.Setenv registers the restore, then Unsetenv removes it.
	t.Setenv(env, "x")
	if err := os.Unsetenv(env); err != nil {
		t.Fatal(err)
	}
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load with the variable unset: %v", err)
	}
	if cfg.IssueTrackerAllowedBaseURLs != nil {
		t.Errorf("unset variable gave %v, want nil", cfg.IssueTrackerAllowedBaseURLs)
	}

	t.Setenv(env, "https://Issues.Example.com/, https://issues.example.com")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.IssueTrackerAllowedBaseURLs; len(got) != 1 || got[0] != "https://issues.example.com" {
		t.Errorf("allowlist = %v, want one normalized entry", got)
	}

	t.Setenv(env, "http://issues.example.com")
	_, err = Load()
	if err == nil {
		t.Fatal("Load accepted an http entry; want a boot error")
	}
	if !strings.Contains(err.Error(), env) {
		t.Errorf("boot error %q does not name %s", err, env)
	}
}

// TestIssueTrackerBaseURLAllowed: an empty list refuses everything, a listed host is allowed, and
// the list is separate from the forge list.
func TestIssueTrackerBaseURLAllowed(t *testing.T) {
	if (Config{}).IssueTrackerBaseURLAllowed("https://issues.example.com") {
		t.Error("an empty allowlist allowed a URL")
	}
	c := Config{
		ForgeAllowedBaseURLs:        []string{"https://gitlab.example.com"},
		IssueTrackerAllowedBaseURLs: []string{"https://issues.example.com"},
	}
	for _, ok := range []string{"https://issues.example.com", "https://ISSUES.example.com/"} {
		if !c.IssueTrackerBaseURLAllowed(ok) {
			t.Errorf("IssueTrackerBaseURLAllowed(%q) = false, want true", ok)
		}
	}
	for _, no := range []string{"https://evil.example.com", "http://issues.example.com", "https://issues.example.com:8443", "https://gitlab.example.com", "not a url", ""} {
		if c.IssueTrackerBaseURLAllowed(no) {
			t.Errorf("IssueTrackerBaseURLAllowed(%q) = true, want false", no)
		}
	}
}
