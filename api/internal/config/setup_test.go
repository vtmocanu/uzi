package config

import (
	"encoding/base64"
	"testing"
)

func setupEnv(t *testing.T) {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	t.Setenv("DATABASE_URL", "postgres://u@db.example/uzi")
	t.Setenv("UZI_SECRET_KEY", base64.StdEncoding.EncodeToString(key))
	t.Setenv("FORGE_ALLOWED_BASE_URLS", "https://git.example")
	t.Setenv("UZI_SEED_EMAIL", " Admin@Example.com ")
	t.Setenv("UZI_SEED_PASSWORD", "")
}

func TestLoadSetupNeedsNoSeedPassword(t *testing.T) {
	setupEnv(t)
	cfg, err := LoadSetup()
	if err != nil {
		t.Fatalf("LoadSetup: %v", err)
	}
	if cfg.SeedEmail != "admin@example.com" {
		t.Errorf("SeedEmail = %q, want it normalised", cfg.SeedEmail)
	}
	if !cfg.ForgeBaseURLAllowed("https://git.example/") || cfg.ForgeBaseURLAllowed("https://other.example") {
		t.Error("the forge allowlist is not applied")
	}
	// Load itself still refuses this environment: setup relaxes nothing for the server.
	t.Setenv("JWT_SECRET", "0123456789abcdef0123456789abcdef")
	if _, err := Load(); err == nil {
		t.Fatal("Load must keep refusing a seed email without a seed password")
	}
}

func TestLoadSetupRefusesBadEnvironment(t *testing.T) {
	for name, mut := range map[string]func(t *testing.T){
		"no database":    func(t *testing.T) { t.Setenv("DATABASE_URL", "") },
		"no secret key":  func(t *testing.T) { t.Setenv("UZI_SECRET_KEY", "") },
		"bad allowlist":  func(t *testing.T) { t.Setenv("FORGE_ALLOWED_BASE_URLS", "http://insecure.example") },
		"no seed email":  func(t *testing.T) { t.Setenv("UZI_SEED_EMAIL", "") },
		"bad seed email": func(t *testing.T) { t.Setenv("UZI_SEED_EMAIL", "not an email") },
	} {
		t.Run(name, func(t *testing.T) {
			setupEnv(t)
			mut(t)
			if _, err := LoadSetup(); err == nil {
				t.Fatal("LoadSetup accepted it")
			}
		})
	}
}
