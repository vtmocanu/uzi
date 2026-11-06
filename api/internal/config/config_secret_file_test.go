package config

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/secretbox"
)

// Issue #3938: JWT_SECRET, UZI_SECRET_KEY and DATABASE_URL each load from the
// plain variable or from the file that <NAME>_FILE names, never both. Every
// case goes through Load() so it pins the boot behaviour, not a helper.

const (
	secretFileTestDSN = "postgres://uzi:pw@db:5432/uzi?sslmode=disable" //nolint:gosec // G101: test fixture, not a credential
	secretFileTestJWT = "unit-test-jwt-signing-key-not-a-real-secret"   //nolint:gosec // G101: test fixture, not a credential
)

func secretFileTestKey() string {
	varied := make([]byte, secretbox.KeySize)
	for i := range varied {
		varied[i] = byte(i + 1)
	}
	return base64.StdEncoding.EncodeToString(varied)
}

// setSecretFileBase sets all three secrets in their plain form and clears the
// three _FILE variables, so each case changes only the secret it tests.
func setSecretFileBase(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", secretFileTestDSN)
	t.Setenv("JWT_SECRET", secretFileTestJWT)
	t.Setenv("UZI_SECRET_KEY", secretFileTestKey())
	for _, k := range []string{"DATABASE_URL_FILE", "JWT_SECRET_FILE", "UZI_SECRET_KEY_FILE"} {
		t.Setenv(k, "")
	}
}

func writeSecretFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// useFileForm moves one secret from its plain variable to a file.
func useFileForm(t *testing.T, name, content string) {
	t.Helper()
	t.Setenv(name, "")
	t.Setenv(name+"_FILE", writeSecretFile(t, content))
}

func TestSecretFileEnvOnly(t *testing.T) {
	setSecretFileBase(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() with plain env secrets: %v", err)
	}
	if cfg.DatabaseURL != secretFileTestDSN {
		t.Errorf("DatabaseURL = %q, want %q", cfg.DatabaseURL, secretFileTestDSN)
	}
	if string(cfg.JWTSecret) != secretFileTestJWT {
		t.Errorf("JWTSecret = %q, want %q", cfg.JWTSecret, secretFileTestJWT)
	}
	if base64.StdEncoding.EncodeToString(cfg.SecretKey) != secretFileTestKey() {
		t.Errorf("SecretKey did not load from UZI_SECRET_KEY")
	}
}

func TestSecretFileFileOnly(t *testing.T) {
	setSecretFileBase(t)
	useFileForm(t, "DATABASE_URL", secretFileTestDSN+"\n")
	useFileForm(t, "JWT_SECRET", secretFileTestJWT+"\n")
	useFileForm(t, "UZI_SECRET_KEY", secretFileTestKey()+"\n")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() with file secrets: %v", err)
	}
	if cfg.DatabaseURL != secretFileTestDSN {
		t.Errorf("DatabaseURL = %q, want %q", cfg.DatabaseURL, secretFileTestDSN)
	}
	if string(cfg.JWTSecret) != secretFileTestJWT {
		t.Errorf("JWTSecret = %q, want %q", cfg.JWTSecret, secretFileTestJWT)
	}
	if base64.StdEncoding.EncodeToString(cfg.SecretKey) != secretFileTestKey() {
		t.Errorf("SecretKey did not load from UZI_SECRET_KEY_FILE")
	}
}

func TestSecretFileBothSetRefuses(t *testing.T) {
	for _, name := range []string{"DATABASE_URL", "JWT_SECRET", "UZI_SECRET_KEY"} {
		t.Run(name, func(t *testing.T) {
			setSecretFileBase(t)
			// The plain variable stays set from the base; add the file form.
			t.Setenv(name+"_FILE", writeSecretFile(t, "unused"))
			_, err := Load()
			if err == nil {
				t.Fatalf("Load() with %s and %s_FILE both set = nil error, want refusal", name, name)
			}
			want := name + " and " + name + "_FILE are both set"
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not contain %q", err, want)
			}
		})
	}
}

// An empty plain variable beside a _FILE path is not "both set": a compose or
// Helm render that always emits the key must still boot on the file form.
func TestSecretFileEmptyEnvBesideFileLoads(t *testing.T) {
	setSecretFileBase(t)
	useFileForm(t, "JWT_SECRET", secretFileTestJWT)
	t.Setenv("JWT_SECRET", "  ")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if string(cfg.JWTSecret) != secretFileTestJWT {
		t.Errorf("JWTSecret = %q, want %q", cfg.JWTSecret, secretFileTestJWT)
	}
}

func TestSecretFileUnreadableRefuses(t *testing.T) {
	cases := map[string]func(t *testing.T) string{
		"missing":   func(t *testing.T) string { return filepath.Join(t.TempDir(), "absent") },
		"directory": func(t *testing.T) string { return t.TempDir() },
	}
	for _, name := range []string{"DATABASE_URL", "JWT_SECRET", "UZI_SECRET_KEY"} {
		for kind, path := range cases {
			t.Run(name+"/"+kind, func(t *testing.T) {
				setSecretFileBase(t)
				t.Setenv(name, "")
				t.Setenv(name+"_FILE", path(t))
				_, err := Load()
				if err == nil {
					t.Fatalf("Load() with an unreadable %s_FILE = nil error, want refusal", name)
				}
				if !strings.HasPrefix(err.Error(), name+"_FILE: read ") {
					t.Errorf("error %q does not name %s_FILE", err, name)
				}
			})
		}
	}
}

// DATABASE_URL is the one secret Load stores untrimmed, so it shows the file
// reader removes exactly one trailing newline and leaves a second one.
func TestSecretFileTrimsOneTrailingNewline(t *testing.T) {
	for content, want := range map[string]string{
		secretFileTestDSN:          secretFileTestDSN,
		secretFileTestDSN + "\n":   secretFileTestDSN,
		secretFileTestDSN + "\n\n": secretFileTestDSN + "\n",
		" " + secretFileTestDSN:    " " + secretFileTestDSN,
	} {
		setSecretFileBase(t)
		useFileForm(t, "DATABASE_URL", content)
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() with DATABASE_URL_FILE content %q: %v", content, err)
		}
		if cfg.DatabaseURL != want {
			t.Errorf("file content %q: DatabaseURL = %q, want %q", content, cfg.DatabaseURL, want)
		}
	}
}

// A validation error on a file-sourced value names the _FILE variable.
func TestSecretFileValidationNamesFileVar(t *testing.T) {
	for name, bad := range map[string]string{
		"JWT_SECRET":     "short\n",
		"UZI_SECRET_KEY": "not-base64!\n",
		"DATABASE_URL":   "\n",
	} {
		t.Run(name, func(t *testing.T) {
			setSecretFileBase(t)
			useFileForm(t, name, bad)
			_, err := Load()
			if err == nil {
				t.Fatalf("Load() with a bad %s_FILE = nil error", name)
			}
			if !strings.Contains(err.Error(), name+"_FILE") {
				t.Errorf("error %q does not name %s_FILE", err, name)
			}
		})
	}
}
