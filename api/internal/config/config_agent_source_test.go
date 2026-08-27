package config

import (
	"reflect"
	"testing"
)

// TestParseAgentSourceAllowedBaseURLs pins the SEPARATE agent-source allowlist parse
// (PRD #602 M2): unlike parseAllowedBaseURLs it TOLERATES an empty list (returns nil,
// no error) because the feature is off by default; it normalizes and dedups https
// entries; and it hard-errors on a non-https/malformed entry (fail-closed).
func TestParseAgentSourceAllowedBaseURLs(t *testing.T) {
	// Empty tolerated: nil, no error (the forge parse would hard-error here).
	for _, empty := range []string{"", "   ", " , , "} {
		got, err := parseAgentSourceAllowedBaseURLs(empty)
		if err != nil {
			t.Errorf("parseAgentSourceAllowedBaseURLs(%q) errored: %v, want nil", empty, err)
		}
		if got != nil {
			t.Errorf("parseAgentSourceAllowedBaseURLs(%q) = %v, want nil", empty, got)
		}
	}

	// Normalizes (scheme+host, no trailing slash) and dedups.
	got, err := parseAgentSourceAllowedBaseURLs("https://a.example.com, https://b.example.com/ , https://a.example.com")
	if err != nil {
		t.Fatalf("parseAgentSourceAllowedBaseURLs: %v", err)
	}
	want := []string{"https://a.example.com", "https://b.example.com"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseAgentSourceAllowedBaseURLs = %v, want %v", got, want)
	}

	// A present non-https entry is a hard error (fail-closed on malformed).
	if _, err := parseAgentSourceAllowedBaseURLs("http://insecure.example.com"); err == nil {
		t.Error("http entry should error")
	}
	if _, err := parseAgentSourceAllowedBaseURLs("https://ok.example.com, not a url"); err == nil {
		t.Error("malformed entry should error")
	}
}

// TestAgentSourceBaseURLAllowed pins the membership check (PRD #602 M2): false for an
// empty list (nothing allowed until configured), true for an exact normalized match,
// false for a non-match or a scheme mismatch.
func TestAgentSourceBaseURLAllowed(t *testing.T) {
	// Empty list: nothing is allowed.
	empty := Config{}
	if empty.AgentSourceBaseURLAllowed("https://git.example.com") {
		t.Error("empty allowlist allowed a URL; want false (nothing allowed until configured)")
	}

	c := Config{AgentSourceAllowedBaseURLs: []string{"https://git.example.com"}}
	if !c.AgentSourceBaseURLAllowed("https://git.example.com/roster.git") {
		t.Error("exact normalized host should be allowed")
	}
	if !c.AgentSourceBaseURLAllowed("https://GIT.example.com") {
		t.Error("host match is case-insensitive after normalization")
	}
	if c.AgentSourceBaseURLAllowed("https://evil.example.com") {
		t.Error("a non-matching host must not be allowed")
	}
	if c.AgentSourceBaseURLAllowed("http://git.example.com") {
		t.Error("an http URL must not be allowed (https-only)")
	}
}
