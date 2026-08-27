package uzicli

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// Store reads and writes the CLI's on-disk config and credentials under a fixed
// directory. DefaultStore hardcodes ~/.config/uzi; NewStore takes an explicit
// directory for tests.
type Store struct {
	dir string
}

// DefaultStore locates the config directory at ~/.config/uzi.
//
// The path is HARDCODED and deliberately does NOT honour $XDG_CONFIG_HOME
// (PRD #64). On at least one machine in this team XDG_CONFIG_HOME points into a
// git-tracked, mackup-synced repo, so honouring it would write a never-expiring
// token into version control. Do not "fix" this to read XDG.
func DefaultStore() (*Store, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return &Store{dir: filepath.Join(home, ".config", "uzi")}, nil
}

// NewStore builds a Store rooted at an explicit directory (tests).
func NewStore(dir string) *Store { return &Store{dir: dir} }

// Dir returns the config directory.
func (s *Store) Dir() string { return s.dir }

func (s *Store) configPath() string      { return filepath.Join(s.dir, "config.toml") }
func (s *Store) credentialsPath() string { return filepath.Join(s.dir, "credentials.toml") }

// Config is the non-secret CLI configuration. It is a map of contexts from day
// one (`[contexts.default]`) so a later `uzi context use` is additive, not a
// file-format migration.
type Config struct {
	Current  string             `toml:"current,omitempty"`
	Contexts map[string]Context `toml:"contexts"`
}

// Context is one named endpoint's non-secret settings.
type Context struct {
	URL string `toml:"url,omitempty"`
}

// Credentials holds the per-context bearer tokens, in a separate 0600 file.
type Credentials struct {
	Contexts map[string]Credential `toml:"contexts"`
}

// Credential is one context's stored token.
type Credential struct {
	Token string `toml:"token,omitempty"`
}

// Settings is the resolved endpoint+token for a context, before env/flag
// overrides are layered on by the caller.
type Settings struct {
	URL   string
	Token string
}

// LoadConfig reads config.toml. A missing file is not an error (empty config).
func (s *Store) LoadConfig() (*Config, error) {
	c := &Config{Contexts: map[string]Context{}}
	b, err := os.ReadFile(s.configPath())
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	if err := toml.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", s.configPath(), err)
	}
	if c.Contexts == nil {
		c.Contexts = map[string]Context{}
	}
	return c, nil
}

// SaveConfig writes config.toml (0644) into the config dir (created 0700).
func (s *Store) SaveConfig(c *Config) error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	b, err := marshalTOML(c)
	if err != nil {
		return err
	}
	return writeFileAtomic(s.configPath(), b, 0o644)
}

// LoadCredentials reads credentials.toml. A missing file is not an error. It
// REFUSES to read a file that is group- or world-accessible, since it holds a
// bearer token (PRD #64).
func (s *Store) LoadCredentials() (*Credentials, error) {
	c := &Credentials{Contexts: map[string]Credential{}}
	fi, err := os.Stat(s.credentialsPath())
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf(
			"refusing to read %s: permissions %04o are group/world-accessible; run `chmod 600 %s`",
			s.credentialsPath(), perm, s.credentialsPath())
	}
	b, err := os.ReadFile(s.credentialsPath())
	if err != nil {
		return nil, err
	}
	if err := toml.Unmarshal(b, c); err != nil {
		// Deliberately drop the underlying toml error: BurntSushi/toml echoes the
		// offending line, and this file holds a bearer token, so a corrupt creds
		// file must not leak a token fragment onto stderr. (config.toml is
		// non-secret and keeps its detailed parse error.)
		return nil, fmt.Errorf("failed to parse credentials file at %s", s.credentialsPath())
	}
	if c.Contexts == nil {
		c.Contexts = map[string]Credential{}
	}
	return c, nil
}

// SaveCredentials writes credentials.toml (0600) into the config dir
// (created 0700).
func (s *Store) SaveCredentials(c *Credentials) error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	b, err := marshalTOML(c)
	if err != nil {
		return err
	}
	return writeFileAtomic(s.credentialsPath(), b, 0o600)
}

// Resolve reads the config + credentials files and returns the URL and token
// stored for the named context (empty ctxName means "default"). It applies no
// env or flag overrides — that is the caller's layer.
func (s *Store) Resolve(ctxName string) (Settings, error) {
	if ctxName == "" {
		ctxName = "default"
	}
	cfg, err := s.LoadConfig()
	if err != nil {
		return Settings{}, err
	}
	creds, err := s.LoadCredentials()
	if err != nil {
		return Settings{}, err
	}
	var out Settings
	if c, ok := cfg.Contexts[ctxName]; ok {
		out.URL = c.URL
	}
	if c, ok := creds.Contexts[ctxName]; ok {
		out.Token = c.Token
	}
	return out, nil
}

func marshalTOML(v any) ([]byte, error) {
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// writeFileAtomic writes data to a temp file in the same directory, sets perm,
// and renames it over path — so a crashed write never leaves a truncated
// credentials file, and racing writers do not interleave.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
