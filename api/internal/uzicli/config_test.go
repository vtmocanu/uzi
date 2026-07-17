package uzicli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConfigRoundtripAndPerms(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(filepath.Join(dir, "uzi"))

	cfg := &Config{Contexts: map[string]Context{"default": {URL: "https://uzi.example"}}}
	if err := s.SaveConfig(cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	// Dir is 0700, config file is 0644.
	if fi, err := os.Stat(s.Dir()); err != nil {
		t.Fatalf("stat dir: %v", err)
	} else if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Errorf("config dir perm = %04o, want 0700", perm)
	}
	if fi, err := os.Stat(s.configPath()); err != nil {
		t.Fatalf("stat config: %v", err)
	} else if perm := fi.Mode().Perm(); perm != 0o644 {
		t.Errorf("config.toml perm = %04o, want 0644", perm)
	}

	got, err := s.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got.Contexts["default"].URL != "https://uzi.example" {
		t.Errorf("roundtrip URL = %q", got.Contexts["default"].URL)
	}
}

func TestCredentialsPermsAndRefuse(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(filepath.Join(dir, "uzi"))

	creds := &Credentials{Contexts: map[string]Credential{"default": {Token: "uzc_secret"}}}
	if err := s.SaveCredentials(creds); err != nil {
		t.Fatalf("SaveCredentials: %v", err)
	}
	// credentials.toml is 0600.
	fi, err := os.Stat(s.credentialsPath())
	if err != nil {
		t.Fatalf("stat credentials: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("credentials.toml perm = %04o, want 0600", perm)
	}

	// A group/world-readable credentials file is refused.
	if err := os.Chmod(s.credentialsPath(), 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if _, err := s.LoadCredentials(); err == nil {
		t.Error("LoadCredentials accepted a 0644 credentials file; want refusal")
	}
}

func TestResolveReadsBothFiles(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(filepath.Join(dir, "uzi"))
	if err := s.SaveConfig(&Config{Contexts: map[string]Context{"default": {URL: "https://u"}}}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveCredentials(&Credentials{Contexts: map[string]Credential{"default": {Token: "uzc_x"}}}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Resolve("")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.URL != "https://u" || got.Token != "uzc_x" {
		t.Errorf("Resolve = %+v", got)
	}
}

func TestMissingFilesAreEmptyNotError(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "uzi"))
	if c, err := s.LoadConfig(); err != nil || len(c.Contexts) != 0 {
		t.Errorf("LoadConfig on missing file = %+v, %v", c, err)
	}
	if c, err := s.LoadCredentials(); err != nil || len(c.Contexts) != 0 {
		t.Errorf("LoadCredentials on missing file = %+v, %v", c, err)
	}
}

func TestDefaultStoreIgnoresXDG(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// A hostile XDG_CONFIG_HOME (per PRD #64, on some machines this points into
	// a git-tracked repo) must be ignored.
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "xdg-decoy"))

	s, err := DefaultStore()
	if err != nil {
		t.Fatalf("DefaultStore: %v", err)
	}
	want := filepath.Join(home, ".config", "uzi")
	if s.Dir() != want {
		t.Errorf("DefaultStore dir = %q, want %q (must NOT honour XDG_CONFIG_HOME)", s.Dir(), want)
	}
}
