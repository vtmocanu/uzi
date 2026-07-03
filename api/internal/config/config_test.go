package config

import (
	"reflect"
	"testing"
)

func TestValidateSecretRejectsUnsafe(t *testing.T) {
	bad := []string{
		"",
		"   ",
		"change-me",
		"CHANGEME",
		"uzi-dev-secret-change-in-production",
		"short",
	}
	for _, s := range bad {
		if _, err := validateSecret(s); err == nil {
			t.Errorf("validateSecret(%q) = nil error, want rejection", s)
		}
	}
}

func TestValidateSecretAcceptsGood(t *testing.T) {
	good := "0f9a1c3e5b7d9f1a3c5e7b9d1f3a5c7e" // 32 hex chars
	if _, err := validateSecret(good); err != nil {
		t.Errorf("validateSecret rejected a good secret: %v", err)
	}
}

func TestOriginIsHTTPS(t *testing.T) {
	cases := map[string]bool{
		"https://uzi.example.com": true,
		"http://127.0.0.1:8080":   false,
		"":                        false,
		"not a url":               false,
	}
	for origin, want := range cases {
		if got := originIsHTTPS(origin); got != want {
			t.Errorf("originIsHTTPS(%q) = %v, want %v", origin, got, want)
		}
	}
}

func TestNormalizeForgeBaseURL(t *testing.T) {
	ok := map[string]string{
		"https://gitlab.example.com":      "https://gitlab.example.com",
		"https://gitlab.example.com/":     "https://gitlab.example.com",
		"https://gitlab.example.com/path": "https://gitlab.example.com",
		"https://GitLab.example.com":      "https://gitlab.example.com",
		"https://host:8443/x?y=1#z":         "https://host:8443",
		"  https://spaced.example.com  ":    "https://spaced.example.com",
	}
	for in, want := range ok {
		got, err := NormalizeForgeBaseURL(in)
		if err != nil {
			t.Errorf("NormalizeForgeBaseURL(%q) errored: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("NormalizeForgeBaseURL(%q) = %q, want %q", in, got, want)
		}
	}

	bad := []string{
		"http://gitlab.example.com", // plain http rejected (SSRF/credential-leak guard)
		"ftp://x",
		"gitlab.example.com", // no scheme
		"https://",             // no host
		"",
	}
	for _, in := range bad {
		if _, err := NormalizeForgeBaseURL(in); err == nil {
			t.Errorf("NormalizeForgeBaseURL(%q) = nil error, want rejection", in)
		}
	}
}

func TestParseAllowedBaseURLs(t *testing.T) {
	got, err := parseAllowedBaseURLs("https://a.example.com, https://b.example.com/ , https://a.example.com")
	if err != nil {
		t.Fatalf("parseAllowedBaseURLs: %v", err)
	}
	want := []string{"https://a.example.com", "https://b.example.com"} // deduped, normalized
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseAllowedBaseURLs = %v, want %v", got, want)
	}

	if _, err := parseAllowedBaseURLs("   "); err == nil {
		t.Error("empty allowlist should error")
	}
	if _, err := parseAllowedBaseURLs("http://insecure.example.com"); err == nil {
		t.Error("http entry should error")
	}
}

func TestLoadSeedAdmin(t *testing.T) {
	t.Run("off when email unset", func(t *testing.T) {
		t.Setenv("UZI_SEED_EMAIL", "")
		var c Config
		if err := loadSeedAdmin(&c); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if c.SeedEmail != "" {
			t.Fatalf("seeding should be off, got email %q", c.SeedEmail)
		}
	})
	t.Run("invalid email errors", func(t *testing.T) {
		t.Setenv("UZI_SEED_EMAIL", "not-an-email")
		t.Setenv("UZI_SEED_PASSWORD", "correct-horse-battery-staple")
		if err := loadSeedAdmin(&Config{}); err == nil {
			t.Fatal("expected error on invalid email")
		}
	})
	t.Run("short password errors (refuse boot)", func(t *testing.T) {
		t.Setenv("UZI_SEED_EMAIL", "admin@uzi.test")
		t.Setenv("UZI_SEED_PASSWORD", "short")
		if err := loadSeedAdmin(&Config{}); err == nil {
			t.Fatal("expected error on short seed password")
		}
	})
	t.Run("valid seed normalizes email", func(t *testing.T) {
		t.Setenv("UZI_SEED_EMAIL", "  Admin@Uzi.TEST ")
		t.Setenv("UZI_SEED_PASSWORD", "correct-horse-battery-staple")
		t.Setenv("UZI_SEED_NAME", "Root")
		var c Config
		if err := loadSeedAdmin(&c); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if c.SeedEmail != "admin@uzi.test" {
			t.Fatalf("email not normalized: %q", c.SeedEmail)
		}
		if c.SeedName != "Root" {
			t.Fatalf("name = %q", c.SeedName)
		}
	})
}

func TestForgeBaseURLAllowed(t *testing.T) {
	c := Config{ForgeAllowedBaseURLs: []string{"https://gitlab.example.com"}}
	if !c.ForgeBaseURLAllowed("https://gitlab.example.com/") {
		t.Error("trailing-slash variant should be allowed")
	}
	if !c.ForgeBaseURLAllowed("https://GitLab.example.com") {
		t.Error("case-insensitive host should be allowed")
	}
	if c.ForgeBaseURLAllowed("https://evil.example.com") {
		t.Error("non-allowlisted host must be rejected")
	}
	if c.ForgeBaseURLAllowed("http://gitlab.example.com") {
		t.Error("http scheme must be rejected")
	}
}
