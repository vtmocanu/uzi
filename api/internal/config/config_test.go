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
// while a missing, non-base64, or wrong-length key aborts start. Only LoadKey
// (the primitive) was covered before; this exercises the wiring in Load().
func TestLoadSecretKeyBootGuard(t *testing.T) {
	// A syntactically valid environment except for UZI_SECRET_KEY, which each
	// subtest sets. JWT_SECRET is a real (non-placeholder, long-enough) value.
	setBase := func(t *testing.T) {
		t.Setenv("DATABASE_URL", "postgres://uzi:pw@db:5432/uzi?sslmode=disable")
		t.Setenv("JWT_SECRET", "0f9a1c3e5b7d9f1a3c5e7b9d1f3a5c7e")
	}
	validKey := base64.StdEncoding.EncodeToString(make([]byte, secretbox.KeySize))

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
	// also reject non-base64 and a correctly-encoded but wrong-length key.
	bad := map[string]string{
		"missing":      "",
		"not base64":   "!!!not-base64!!!",
		"wrong length": base64.StdEncoding.EncodeToString(make([]byte, 16)),
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
