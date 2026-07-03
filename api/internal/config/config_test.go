package config

import (
	"encoding/base64"
	"reflect"
	"testing"

	"gitlab.example.com/vtmocanu/uzi/api/internal/secretbox"
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

// TestLoadSecretKeyBootGuard covers the UZI_SECRET_KEY boot guard end to end
// through config.Load(): a valid base64 32-byte key loads into cfg.SecretKey,
// while a missing, non-base64, wrong-length, or low-entropy key aborts start.
// Only LoadKey (the primitive) was covered before; this exercises the wiring in
// Load().
func TestLoadSecretKeyBootGuard(t *testing.T) {
	// A syntactically valid environment except for UZI_SECRET_KEY, which each
	// subtest sets. JWT_SECRET is a real (non-placeholder, long-enough) value.
	setBase := func(t *testing.T) {
		t.Setenv("DATABASE_URL", "postgres://uzi:pw@db:5432/uzi?sslmode=disable")
		t.Setenv("JWT_SECRET", "0f9a1c3e5b7d9f1a3c5e7b9d1f3a5c7e")
	}
	// A real (varied-byte) 32-byte key. An all-zero key would trip the
	// low-entropy guard, so distinct bytes are used here.
	varied := make([]byte, secretbox.KeySize)
	for i := range varied {
		varied[i] = byte(i + 1)
	}
	validKey := base64.StdEncoding.EncodeToString(varied)

	t.Run("valid key loads", func(t *testing.T) {
		setBase(t)
		t.Setenv("UZI_SECRET_KEY", validKey)
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() errored on a valid key: %v", err)
		}
		if len(cfg.SecretKey) != secretbox.KeySize {
			t.Fatalf("cfg.SecretKey len = %d, want %d", len(cfg.SecretKey), secretbox.KeySize)
		}
	})

	// Empty behaves as "unset" (LoadKey treats "" as not set); the guard must
	// also reject non-base64, a correctly-encoded but wrong-length key, and a
	// low-entropy all-identical-byte placeholder.
	bad := map[string]string{
		"missing":      "",
		"not base64":   "!!!not-base64!!!",
		"wrong length": base64.StdEncoding.EncodeToString(make([]byte, 16)),
		"low entropy":  base64.StdEncoding.EncodeToString(make([]byte, secretbox.KeySize)),
	}
	for name, val := range bad {
		t.Run(name, func(t *testing.T) {
			setBase(t)
			t.Setenv("UZI_SECRET_KEY", val)
			if _, err := Load(); err == nil {
				t.Errorf("Load() = nil error for %s UZI_SECRET_KEY, want boot-guard failure", name)
			}
		})
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

func TestLoadSeedForge(t *testing.T) {
	const base = "https://gitlab.example.com"
	withAllowlist := func() *Config {
		return &Config{ForgeAllowedBaseURLs: []string{base}, SeedEmail: "admin@uzi.test"}
	}

	t.Run("off when PAT unset", func(t *testing.T) {
		t.Setenv("UZI_SEED_FORGE_PAT", "")
		c := withAllowlist()
		if err := loadSeedForge(c); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if c.SeedForgePAT != "" || c.SeedForgeBaseURL != "" || c.SeedForgeRepos != nil {
			t.Fatalf("forge seed should be off, got %+v", *c)
		}
	})

	t.Run("PAT without seed email refuses boot", func(t *testing.T) {
		t.Setenv("UZI_SEED_FORGE_PAT", "glpat-xxx")
		c := &Config{ForgeAllowedBaseURLs: []string{base}} // SeedEmail deliberately empty
		if err := loadSeedForge(c); err == nil {
			t.Fatal("expected boot-fatal error when PAT is set without a seed email")
		}
	})

	t.Run("non-allowlisted base URL refuses boot", func(t *testing.T) {
		t.Setenv("UZI_SEED_FORGE_PAT", "glpat-xxx")
		t.Setenv("UZI_SEED_FORGE_BASE_URL", "https://evil.example.com")
		if err := loadSeedForge(withAllowlist()); err == nil {
			t.Fatal("expected boot-fatal error for a base URL outside the allowlist")
		}
	})

	t.Run("base URL defaults to first allowlisted entry", func(t *testing.T) {
		t.Setenv("UZI_SEED_FORGE_PAT", "glpat-xxx")
		t.Setenv("UZI_SEED_FORGE_BASE_URL", "")
		t.Setenv("UZI_SEED_FORGE_REPOS", "")
		c := withAllowlist()
		if err := loadSeedForge(c); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if c.SeedForgeBaseURL != base {
			t.Fatalf("base URL = %q, want default %q", c.SeedForgeBaseURL, base)
		}
	})

	t.Run("valid seed trims PAT, normalizes base URL, dedups repos", func(t *testing.T) {
		t.Setenv("UZI_SEED_FORGE_PAT", "  glpat-xxx  ")
		t.Setenv("UZI_SEED_FORGE_BASE_URL", "https://gitlab.example.com/")
		t.Setenv("UZI_SEED_FORGE_REPOS", "vtmocanu/uzi, vtmocanu/other , vtmocanu/uzi")
		c := withAllowlist()
		if err := loadSeedForge(c); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if c.SeedForgePAT != "glpat-xxx" {
			t.Fatalf("PAT not trimmed: %q", c.SeedForgePAT)
		}
		if c.SeedForgeBaseURL != base {
			t.Fatalf("base URL not normalized: %q", c.SeedForgeBaseURL)
		}
		want := []string{"vtmocanu/uzi", "vtmocanu/other"}
		if !reflect.DeepEqual(c.SeedForgeRepos, want) {
			t.Fatalf("repos = %v, want %v (trimmed, deduped, order-preserving)", c.SeedForgeRepos, want)
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
