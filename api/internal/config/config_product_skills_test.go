package config

import (
	"reflect"
	"strings"
	"testing"
)

// TestParseProductSkillsAllowlist (PRD #1909 D9): UZI_PRODUCT_SKILLS_ALLOWED_BASE_URLS is https
// only, normalized and deduped, empty means the feature is off (nil, and the instance still
// boots), and a malformed or non-https entry is a boot error that names the variable.
func TestParseProductSkillsAllowlist(t *testing.T) {
	const env = "UZI_PRODUCT_SKILLS_ALLOWED_BASE_URLS"
	for _, empty := range []string{"", "   ", " , ,"} {
		got, err := parseBaseURLAllowlist(env, empty)
		if err != nil || got != nil {
			t.Errorf("parse(%q) = %v, %v; want nil, nil", empty, got, err)
		}
	}
	got, err := parseBaseURLAllowlist(env, "https://skills.example.com, https://Git.Example.com/ , https://skills.example.com")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if want := []string{"https://skills.example.com", "https://git.example.com"}; !reflect.DeepEqual(got, want) {
		t.Errorf("parse = %v, want %v", got, want)
	}
	for _, bad := range []string{"http://insecure.example.com", "ftp://x.example.com", "https://ok.example.com, not a url"} {
		_, err := parseBaseURLAllowlist(env, bad)
		if err == nil {
			t.Errorf("parse(%q) accepted a non-https or malformed entry", bad)
			continue
		}
		if !strings.Contains(err.Error(), env) {
			t.Errorf("parse(%q) error %q does not name %s", bad, err, env)
		}
	}
}

// TestProductSkillsBaseURLAllowed: false for an empty list (feature off), true for an exact
// normalized match, false for a non-match or an http URL, and the list is SEPARATE from the
// agent-source list in both directions.
func TestProductSkillsBaseURLAllowed(t *testing.T) {
	if (Config{}).ProductSkillsBaseURLAllowed("https://skills.example.com/r.git") {
		t.Error("an empty allowlist allowed a URL; the feature is off")
	}
	c := Config{ProductSkillsAllowedBaseURLs: []string{"https://skills.example.com"}}
	if !c.ProductSkillsBaseURLAllowed("https://skills.example.com/org/r.git") || !c.ProductSkillsBaseURLAllowed("https://SKILLS.example.com/x") {
		t.Error("the listed host must be allowed, case-insensitively")
	}
	for _, no := range []string{"https://evil.example.com/r.git", "http://skills.example.com/r.git", "https://skills.example.com:8443/r.git", "not a url"} {
		if c.ProductSkillsBaseURLAllowed(no) {
			t.Errorf("ProductSkillsBaseURLAllowed(%q) = true, want false", no)
		}
	}

	// Separate lists: enabling one never widens the other.
	both := Config{
		AgentSourceAllowedBaseURLs:   []string{"https://roles.example.com"},
		ProductSkillsAllowedBaseURLs: []string{"https://skills.example.com"},
	}
	if both.ProductSkillsBaseURLAllowed("https://roles.example.com/r.git") {
		t.Error("an agent-source host must not be allowed as a product skills source")
	}
	if both.AgentSourceBaseURLAllowed("https://skills.example.com/r.git") {
		t.Error("a product skills host must not be allowed as an agent source")
	}
}

// TestLoadReadsProductSkillsAllowlist: the variable reaches Config, defaults to off, and a bad
// entry fails the boot.
func TestLoadReadsProductSkillsAllowlist(t *testing.T) {
	autoselectEnv(t)
	t.Setenv("UZI_PRODUCT_SKILLS_ALLOWED_BASE_URLS", "")
	cfg, err := Load()
	if err != nil || len(cfg.ProductSkillsAllowedBaseURLs) != 0 {
		t.Fatalf("Load with the variable unset = %v, %v; want the feature off", cfg.ProductSkillsAllowedBaseURLs, err)
	}
	t.Setenv("UZI_PRODUCT_SKILLS_ALLOWED_BASE_URLS", "https://skills.example.com")
	cfg, err = Load()
	if err != nil || !cfg.ProductSkillsBaseURLAllowed("https://skills.example.com/r.git") {
		t.Fatalf("Load = %v, %v; want skills.example.com allowed", cfg.ProductSkillsAllowedBaseURLs, err)
	}
	t.Setenv("UZI_PRODUCT_SKILLS_ALLOWED_BASE_URLS", "http://skills.example.com")
	if _, err := Load(); err == nil {
		t.Fatal("Load accepted an http entry; want a boot error")
	}
}
