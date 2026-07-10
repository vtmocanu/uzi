package config

import (
	"encoding/base64"
	"reflect"
	"testing"
	"time"

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

// TestLoadAgentRuntimeDefaults locks the PRD #4 runtime knob defaults so a typo
// in a default never ships silently. RUN_MAX_REQUEUES=0 must survive (it is a
// legitimate "never re-queue" value, which the ordinary >0 int parser would
// reject).
func TestLoadAgentRuntimeDefaults(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://uzi:pw@db:5432/uzi?sslmode=disable")
	// Low-entropy but valid (non-placeholder, long-enough) signing key; a
	// high-entropy literal would trip the secret scanner on a fresh add.
	t.Setenv("JWT_SECRET", "unit-test-jwt-signing-key-not-a-real-secret")
	varied := make([]byte, secretbox.KeySize)
	for i := range varied {
		varied[i] = byte(i + 1)
	}
	t.Setenv("UZI_SECRET_KEY", base64.StdEncoding.EncodeToString(varied))

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	checks := []struct {
		name string
		got  any
		want any
	}{
		{"RunTimeout", cfg.RunTimeout, 2 * time.Hour},
		{"RunIdleTimeout", cfg.RunIdleTimeout, 10 * time.Minute},
		{"RunMaxIterations", cfg.RunMaxIterations, 5},
		{"RunMaxRequeues", cfg.RunMaxRequeues, 1},
		{"WorkerHeartbeatInterval", cfg.WorkerHeartbeatInterval, 15 * time.Second},
		{"WorkerHeartbeatStale", cfg.WorkerHeartbeatStale, 45 * time.Second},
		{"WorkerPollInterval", cfg.WorkerPollInterval, 3 * time.Second},
		{"WorkerAffinityGrace", cfg.WorkerAffinityGrace, 2 * time.Minute},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s default = %v, want %v", c.name, c.got, c.want)
		}
	}

	// RUN_MAX_REQUEUES=0 is a valid, non-default value.
	t.Setenv("RUN_MAX_REQUEUES", "0")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load() with RUN_MAX_REQUEUES=0: %v", err)
	}
	if cfg.RunMaxRequeues != 0 {
		t.Errorf("RunMaxRequeues = %d, want 0 (never re-queue)", cfg.RunMaxRequeues)
	}
}

// TestProposalConfirmStuckTimeoutClamped pins the load-bearing ordering invariant:
// the stuck-confirming sweep timeout must sit safely above the forge HTTP timeout, so
// a slow CreateIssue can never be reverted mid-flight and re-confirmed into a
// duplicate issue. A too-low value is clamped up (to 2x the forge timeout); a
// comfortably-large value is left untouched.
func TestProposalConfirmStuckTimeoutClamped(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://uzi:pw@db:5432/uzi?sslmode=disable")
	t.Setenv("JWT_SECRET", "unit-test-jwt-signing-key-not-a-real-secret")
	varied := make([]byte, secretbox.KeySize)
	for i := range varied {
		varied[i] = byte(i + 1)
	}
	t.Setenv("UZI_SECRET_KEY", base64.StdEncoding.EncodeToString(varied))

	// A stuck timeout below 2x the (raised) forge timeout is clamped up.
	t.Setenv("FORGE_HTTP_TIMEOUT", "90s")
	t.Setenv("PROPOSAL_CONFIRM_STUCK_TIMEOUT", "30s")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if cfg.ProposalConfirmStuckTimeout != 180*time.Second {
		t.Errorf("clamped stuck timeout = %v, want 180s (2x the 90s forge timeout)", cfg.ProposalConfirmStuckTimeout)
	}

	// A comfortably-large value is left as configured.
	t.Setenv("PROPOSAL_CONFIRM_STUCK_TIMEOUT", "10m")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if cfg.ProposalConfirmStuckTimeout != 10*time.Minute {
		t.Errorf("stuck timeout = %v, want 10m (above the floor, unchanged)", cfg.ProposalConfirmStuckTimeout)
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

func TestParseEmailDomains(t *testing.T) {
	cases := []struct {
		raw  string
		want []string
	}{
		{"", nil},
		{"   ", nil},
		{"example.com", []string{"example.com"}},
		{"example.COM", []string{"example.com"}},      // lowercased
		{" a.com , b.com ", []string{"a.com", "b.com"}},   // trimmed
		{"a.com,a.com,b.com", []string{"a.com", "b.com"}}, // deduped
		{"a.com,,b.com,", []string{"a.com", "b.com"}},     // empty entries dropped
		{"B.com,a.com", []string{"b.com", "a.com"}},       // first-seen order preserved
	}
	for _, tc := range cases {
		got := parseEmailDomains(tc.raw)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("parseEmailDomains(%q) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}

func TestParseBool(t *testing.T) {
	t.Run("empty returns default", func(t *testing.T) {
		t.Setenv("UZI_TEST_BOOL", "")
		for _, def := range []bool{true, false} {
			got, err := parseBool("UZI_TEST_BOOL", def)
			if err != nil || got != def {
				t.Errorf("empty with def=%v: got %v, err %v", def, got, err)
			}
		}
	})
	t.Run("valid forms parse", func(t *testing.T) {
		valid := map[string]bool{"true": true, "false": false, "1": true, "0": false, "TRUE": true, "F": false}
		for raw, want := range valid {
			t.Setenv("UZI_TEST_BOOL", raw)
			got, err := parseBool("UZI_TEST_BOOL", !want)
			if err != nil || got != want {
				t.Errorf("parseBool(%q) = %v, err %v; want %v", raw, got, err, want)
			}
		}
	})
	t.Run("malformed is a boot error", func(t *testing.T) {
		t.Setenv("UZI_TEST_BOOL", "yes-please")
		if _, err := parseBool("UZI_TEST_BOOL", true); err == nil {
			t.Fatal("a malformed boolean must abort boot, got nil error")
		}
	})
}

// TestLoadRegistrationPolicy exercises the registration knobs end to end through
// Load(): the default (open registration, no allowlist), an explicit disable, a
// domain allowlist, and a malformed kill-switch aborting boot.
func TestLoadRegistrationPolicy(t *testing.T) {
	setBase := func(t *testing.T) {
		t.Setenv("DATABASE_URL", "postgres://uzi:pw@db:5432/uzi?sslmode=disable")
		t.Setenv("JWT_SECRET", "unit-test-jwt-signing-key-not-a-real-secret")
		varied := make([]byte, secretbox.KeySize)
		for i := range varied {
			varied[i] = byte(i + 1)
		}
		t.Setenv("UZI_SECRET_KEY", base64.StdEncoding.EncodeToString(varied))
	}

	t.Run("defaults: registration on, no allowlist", func(t *testing.T) {
		setBase(t)
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if !cfg.RegistrationEnabled {
			t.Error("RegistrationEnabled should default to true")
		}
		if len(cfg.AllowedEmailDomains) != 0 {
			t.Errorf("AllowedEmailDomains should default to empty, got %v", cfg.AllowedEmailDomains)
		}
	})

	t.Run("explicit disable + allowlist", func(t *testing.T) {
		setBase(t)
		t.Setenv("UZI_REGISTRATION_ENABLED", "false")
		t.Setenv("UZI_ALLOWED_EMAIL_DOMAINS", "example.com, example.org")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.RegistrationEnabled {
			t.Error("RegistrationEnabled should be false")
		}
		want := []string{"example.com", "example.org"}
		if !reflect.DeepEqual(cfg.AllowedEmailDomains, want) {
			t.Errorf("AllowedEmailDomains = %v, want %v", cfg.AllowedEmailDomains, want)
		}
	})

	t.Run("malformed kill-switch aborts boot", func(t *testing.T) {
		setBase(t)
		t.Setenv("UZI_REGISTRATION_ENABLED", "maybe")
		if _, err := Load(); err == nil {
			t.Fatal("a malformed UZI_REGISTRATION_ENABLED must abort boot")
		}
	})
}

// TestPrivilegeCheckInterval covers the sweep-cadence knob, including the
// reviewer-flagged 0=disabled case that parseDuration would wrongly reject.
func TestPrivilegeCheckInterval(t *testing.T) {
	setBase := func(t *testing.T) {
		t.Setenv("DATABASE_URL", "postgres://uzi:pw@db:5432/uzi?sslmode=disable")
		t.Setenv("JWT_SECRET", "unit-test-jwt-signing-key-not-a-real-secret")
		varied := make([]byte, secretbox.KeySize)
		for i := range varied {
			varied[i] = byte(i + 1)
		}
		t.Setenv("UZI_SECRET_KEY", base64.StdEncoding.EncodeToString(varied))
	}

	t.Run("default is 24h", func(t *testing.T) {
		setBase(t)
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.PrivilegeCheckInterval != 24*time.Hour {
			t.Errorf("default = %v, want 24h", cfg.PrivilegeCheckInterval)
		}
	})

	t.Run("0 disables (not rejected as parseDuration would)", func(t *testing.T) {
		setBase(t)
		t.Setenv("UZI_PRIVILEGE_CHECK_INTERVAL", "0")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.PrivilegeCheckInterval != 0 {
			t.Errorf("interval = %v, want 0 (disabled)", cfg.PrivilegeCheckInterval)
		}
	})

	t.Run("explicit duration parses", func(t *testing.T) {
		setBase(t)
		t.Setenv("UZI_PRIVILEGE_CHECK_INTERVAL", "6h")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.PrivilegeCheckInterval != 6*time.Hour {
			t.Errorf("interval = %v, want 6h", cfg.PrivilegeCheckInterval)
		}
	})
}
