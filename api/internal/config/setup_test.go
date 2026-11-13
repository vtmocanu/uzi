package config

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setupKey is a varied 32-byte fixture key (a uniform one is refused as weak).
func setupKey() []byte {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	return key
}

// setupEnv pins every variable LoadSetup and the Load comparisons below read,
// so a developer's or CI runner's environment cannot change the outcome.
func setupEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"DATABASE_URL_FILE", "UZI_SECRET_KEY_FILE", "JWT_SECRET", "JWT_SECRET_FILE",
		"UZI_REGISTRATION_ENABLED", "UZI_ALLOWED_EMAIL_DOMAINS", "AGENT_SOURCE_ALLOWED_BASE_URLS", "ANTHROPIC_GATEWAY_ALLOWED_BASE_URLS",
		"UZI_SEED_NAME", "UZI_SEED_FORGE_PAT", "UZI_SEED_FORGE_TYPE", "UZI_SEED_FORGE_BASE_URL",
		"UZI_SEED_FORGE_REPOS", "UZI_SEED_ANTHROPIC_TOKEN", "FORGE_HTTP_TIMEOUT",
	} {
		t.Setenv(name, "")
	}
	t.Setenv("DATABASE_URL", "postgres://u@db.example/uzi")
	t.Setenv("UZI_SECRET_KEY", base64.StdEncoding.EncodeToString(setupKey()))
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
	// Load itself still refuses this environment, for the missing seed password:
	// setup relaxes nothing for the server. Built at runtime: a 32-byte fixture,
	// not a credential.
	t.Setenv("JWT_SECRET", strings.Repeat("0123456789abcdef", 2))
	_, err = Load()
	if err == nil || !strings.Contains(err.Error(), "UZI_SEED_PASSWORD") {
		t.Fatalf("Load must keep refusing a seed email without a seed password; err = %v", err)
	}
	// Control: the same environment with a valid seed password loads, so the
	// refusal above is the seed password and nothing else.
	t.Setenv("UZI_SEED_PASSWORD", strings.Repeat("p", 16))
	if _, err := Load(); err != nil {
		t.Fatalf("Load with a valid seed password: %v", err)
	}
}

// LoadSetup reads its boot secrets like Load: from the plain variable or from a
// <NAME>_FILE path (a mounted secret), so /server setup runs unchanged on a
// server that is configured only with secret files.
func TestLoadSetupReadsSecretFiles(t *testing.T) {
	setupEnv(t)
	dir := t.TempDir()
	write := func(name, content string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	const dbURL = "postgres://u@db-file.example/uzi"
	t.Setenv("DATABASE_URL", "")
	t.Setenv("DATABASE_URL_FILE", write("db", dbURL+"\n"))
	t.Setenv("UZI_SECRET_KEY", "")
	t.Setenv("UZI_SECRET_KEY_FILE", write("key", base64.StdEncoding.EncodeToString(setupKey())+"\n"))
	cfg, err := LoadSetup()
	if err != nil {
		t.Fatalf("LoadSetup with _FILE secrets: %v", err)
	}
	if cfg.DatabaseURL != dbURL {
		t.Errorf("DatabaseURL = %q, want %q", cfg.DatabaseURL, dbURL)
	}
	if !bytes.Equal(cfg.SecretKey, setupKey()) {
		t.Error("SecretKey was not read from UZI_SECRET_KEY_FILE")
	}

	// Setting both forms of one secret is refused, as Load refuses it.
	t.Setenv("DATABASE_URL", dbURL)
	if _, err := LoadSetup(); err == nil || !strings.Contains(err.Error(), "DATABASE_URL_FILE") {
		t.Fatalf("both DATABASE_URL and DATABASE_URL_FILE: err = %v", err)
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
