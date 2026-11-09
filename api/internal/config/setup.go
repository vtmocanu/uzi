package config

import (
	"errors"
	"fmt"
	"io"
	"net/mail"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/vtmocanu/uzi/api/internal/secretbox"
)

// SetupConfig is the slice of configuration the operator setup command
// (`/server setup`, PRD #3760) reads. It is deliberately not Config: Load
// refuses a seed email without a valid seed password, and setup must run
// without UZI_SEED_PASSWORD in the environment. Load is unchanged.
type SetupConfig struct {
	DatabaseURL          string
	SecretKey            []byte
	ForgeAllowedBaseURLs []string
	ForgeHTTPTimeout     time.Duration
	// SeedEmail is the normalised UZI_SEED_EMAIL: the account setup acts for.
	SeedEmail string
}

// ForgeBaseURLAllowed reports whether raw normalizes to an allowlisted base
// URL, with the same rule as Config.ForgeBaseURLAllowed.
func (c SetupConfig) ForgeBaseURLAllowed(raw string) bool {
	return Config{ForgeAllowedBaseURLs: c.ForgeAllowedBaseURLs}.ForgeBaseURLAllowed(raw)
}

// LoadSetup reads and validates the setup configuration from the environment.
// It shares the DATABASE_URL, UZI_SECRET_KEY, FORGE_ALLOWED_BASE_URLS and
// seed-email validators with Load, requires UZI_SEED_EMAIL, and never reads
// UZI_SEED_PASSWORD. Like Load, each boot secret comes from the plain variable
// or its <NAME>_FILE path, never both, so a server configured with mounted
// secret files runs setup unchanged. Unlike Load, a <NAME>_FILE must name a
// regular file (readRegularSecretFile): setup runs before its deadline applies,
// so a FIFO with no writer would otherwise hang it.
func LoadSetup() (SetupConfig, error) {
	cfg := SetupConfig{
		ForgeHTTPTimeout: parseDuration("FORGE_HTTP_TIMEOUT", 15*time.Second),
	}
	dbURL, dbSource, err := resolveSecretWith("DATABASE_URL", readRegularSecretFile)
	if err != nil {
		return SetupConfig{}, err
	}
	if strings.TrimSpace(dbURL) == "" {
		return SetupConfig{}, fmt.Errorf("%s is required", dbSource)
	}
	cfg.DatabaseURL = dbURL
	keyRaw, keySource, err := resolveSecretWith("UZI_SECRET_KEY", readRegularSecretFile)
	if err != nil {
		return SetupConfig{}, err
	}
	key, err := secretbox.DecodeKey(keySource, keyRaw)
	if err != nil {
		return SetupConfig{}, fmt.Errorf("%w; generate one with: openssl rand -base64 32", err)
	}
	cfg.SecretKey = key
	allowed, err := parseAllowedBaseURLs(getenv("FORGE_ALLOWED_BASE_URLS", defaultForgeBaseURL))
	if err != nil {
		return SetupConfig{}, err
	}
	cfg.ForgeAllowedBaseURLs = allowed
	email, err := parseSeedEmail(os.Getenv("UZI_SEED_EMAIL"))
	if err != nil {
		return SetupConfig{}, err
	}
	if email == "" {
		return SetupConfig{}, fmt.Errorf("UZI_SEED_EMAIL is required: setup acts for the seed admin")
	}
	cfg.SeedEmail = email
	return cfg, nil
}

// errNotRegularSecretFile marks a <NAME>_FILE that is not a regular file;
// resolveSecretWith wraps it with the variable's name.
var errNotRegularSecretFile = errors.New("must name a regular file (a FIFO, device or directory is refused)")

// readRegularSecretFile reads a secret file for the setup command without
// blocking on a special file. A plain open of a FIFO with no writer blocks in
// the kernel, before any deadline can apply, so the open is non-blocking
// (O_NONBLOCK has no effect on a regular file) and the type is checked on the
// opened descriptor, which a path swap cannot change.
func readRegularSecretFile(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0) //nolint:gosec // G304: operator config naming the mounted secret file.
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errNotRegularSecretFile
	}
	return io.ReadAll(f)
}

// parseSeedEmail normalises a UZI_SEED_EMAIL value the way registration stores
// it. An empty value returns ("", nil); a malformed one is an error.
func parseSeedEmail(raw string) (string, error) {
	email := strings.TrimSpace(strings.ToLower(raw))
	if email == "" {
		return "", nil
	}
	if _, err := mail.ParseAddress(email); err != nil {
		return "", fmt.Errorf("UZI_SEED_EMAIL is not a valid email address")
	}
	return email, nil
}
