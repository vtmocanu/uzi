// Package config loads and validates runtime configuration from the
// environment. It fails loud at boot on an unsafe or missing JWT secret so a
// misconfigured deployment never silently runs with guessable credentials.
package config

import (
	"fmt"
	"log/slog"
	"net"
	"net/mail"
	"net/url"
	"os"
	"strings"
	"time"

	"gitlab.example.com/vtmocanu/uzi/api/internal/auth"
	"gitlab.example.com/vtmocanu/uzi/api/internal/secretbox"
)

// Config holds all runtime settings derived from the environment.
type Config struct {
	// Addr is the TCP address the API listens on (e.g. ":8080").
	Addr string
	// DatabaseURL is the pgx connection string.
	DatabaseURL string
	// JWTSecret is the validated HS256 signing key.
	JWTSecret []byte
	// AuthTokenTTL is the session lifetime and cookie MaxAge.
	AuthTokenTTL time.Duration
	// FrontendOrigin is the user-facing origin; its scheme decides the
	// cookie Secure flag.
	FrontendOrigin string
	// CookieSecure is derived from FrontendOrigin (https => true).
	CookieSecure bool
	// RateLimitMax is the request budget per window for auth endpoints.
	RateLimitMax int
	// RateLimitWindow is the fixed window for the rate limiter.
	RateLimitWindow time.Duration
	// TrustedProxies is the set of CIDRs whose direct connections are allowed
	// to speak for a real client via X-Forwarded-For. Empty means never trust
	// XFF (RemoteAddr only) — the conservative default.
	TrustedProxies []*net.IPNet
	// SecretKey is the validated 32-byte AES-256 master key (from
	// UZI_SECRET_KEY) that encrypts secrets at rest: forge bot PATs and per-user
	// Anthropic tokens. Boot fails without it.
	SecretKey []byte
	// ForgeAllowedBaseURLs is the SSRF allowlist: the only forge base URLs a
	// connection may target. Normalized (scheme+host, no trailing slash), https
	// only. The connect UI offers exactly this set.
	ForgeAllowedBaseURLs []string
	// ForgePollInterval is the per-enabled-repo incremental poll cadence.
	ForgePollInterval time.Duration
	// ForgeReconcileEvery is the number of incremental polls between full
	// reconcile passes (the eviction-capable freshness floor).
	ForgeReconcileEvery int
	// ForgeHTTPTimeout bounds every outbound forge HTTP call.
	ForgeHTTPTimeout time.Duration
	// ForgeRateLimitMax/Window bound how often one authenticated user may hit
	// the forge-proxying endpoints (verify/projects/sync/move), protecting the
	// upstream forge from a single user's abuse.
	ForgeRateLimitMax    int
	ForgeRateLimitWindow time.Duration
	// SeedEmail/SeedPassword/SeedName optionally provision an admin at startup.
	// Empty SeedEmail disables seeding. Validated at boot (see Load).
	SeedEmail    string
	SeedPassword string
	SeedName     string
	// SeedForgePAT/SeedForgeBaseURL/SeedForgeRepos optionally seed a forge
	// connection (belonging to the seed admin) and enable a set of repos at
	// startup. Empty SeedForgePAT disables it. Requires SeedEmail; the base URL
	// must be allowlisted. Validated at boot (see loadSeedForge). SeedForgeBaseURL
	// is stored normalized to match how a connection's base_url is stored.
	SeedForgePAT     string
	SeedForgeBaseURL string
	SeedForgeRepos   []string
}

// placeholderSecrets are values that must never be accepted as a real signing
// key. They are the well-known dev fallbacks the inspiration projects shipped
// (and refused, in bottega's case) plus common footguns.
var placeholderSecrets = map[string]struct{}{
	"multica-dev-secret-change-in-production": {},
	"bottega-dev-secret-change-in-production": {},
	"uzi-dev-secret-change-in-production":     {},
	"change-me":                               {},
	"changeme":                                {},
	"secret":                                  {},
	"password":                                {},
}

// minSecretLen is the minimum acceptable JWT secret length. `openssl rand -hex
// 64` produces 128 chars; this floor rejects obviously weak keys while staying
// permissive enough for hand-set values in local demos.
const minSecretLen = 16

// defaultForgeBaseURL is the single allowlisted forge for the example demo.
const defaultForgeBaseURL = "https://gitlab.example.com"

// Load reads configuration from the environment and validates it. It returns
// an error (rather than exiting) so the caller controls the failure path.
func Load() (Config, error) {
	cfg := Config{
		Addr:            getenv("API_ADDR", ":8080"),
		DatabaseURL:     os.Getenv("DATABASE_URL"),
		FrontendOrigin:  getenv("FRONTEND_ORIGIN", "http://127.0.0.1:8080"),
		AuthTokenTTL:    parseDuration("AUTH_TOKEN_TTL", 168*time.Hour),
		RateLimitMax:    parseInt("RATE_LIMIT_MAX", 10),
		RateLimitWindow: parseDuration("RATE_LIMIT_WINDOW", time.Minute),
		TrustedProxies:  parseTrustedProxies(os.Getenv("TRUSTED_PROXIES")),
	}

	if strings.TrimSpace(cfg.DatabaseURL) == "" {
		return Config{}, fmt.Errorf("DATABASE_URL is required")
	}

	secret, err := validateSecret(os.Getenv("JWT_SECRET"))
	if err != nil {
		return Config{}, err
	}
	cfg.JWTSecret = secret

	// Boot guard for the at-rest encryption key, same stance as JWT_SECRET: a
	// missing/short/mis-encoded UZI_SECRET_KEY aborts start rather than running
	// with unencryptable or guessable-keyed token storage.
	key, err := secretbox.LoadKey("UZI_SECRET_KEY")
	if err != nil {
		return Config{}, fmt.Errorf("%w; generate one with: openssl rand -base64 32", err)
	}
	cfg.SecretKey = key

	allowed, err := parseAllowedBaseURLs(getenv("FORGE_ALLOWED_BASE_URLS", defaultForgeBaseURL))
	if err != nil {
		return Config{}, err
	}
	cfg.ForgeAllowedBaseURLs = allowed
	cfg.ForgePollInterval = parseDuration("FORGE_POLL_INTERVAL", time.Minute)
	cfg.ForgeReconcileEvery = parseInt("FORGE_RECONCILE_EVERY", 10)
	cfg.ForgeHTTPTimeout = parseDuration("FORGE_HTTP_TIMEOUT", 15*time.Second)
	cfg.ForgeRateLimitMax = parseInt("FORGE_RATE_LIMIT_MAX", 30)
	cfg.ForgeRateLimitWindow = parseDuration("FORGE_RATE_LIMIT_WINDOW", time.Minute)

	if err := loadSeedAdmin(&cfg); err != nil {
		return Config{}, err
	}
	// Must run after loadSeedAdmin (needs SeedEmail) and after the allowlist is
	// set (needs it for the default base URL and the allowlist check).
	if err := loadSeedForge(&cfg); err != nil {
		return Config{}, err
	}

	cfg.CookieSecure = originIsHTTPS(cfg.FrontendOrigin)

	return cfg, nil
}

// loadSeedAdmin reads and validates the optional startup-admin seed. Seeding is
// off unless UZI_SEED_EMAIL is set; when it is, the email must be valid and the
// password must satisfy the same length policy as registration, or boot fails
// (a set-but-invalid seed should be a loud misconfiguration, not a silent
// skip). The email is normalized to match how registration stores it.
func loadSeedAdmin(cfg *Config) error {
	email := strings.TrimSpace(strings.ToLower(os.Getenv("UZI_SEED_EMAIL")))
	if email == "" {
		return nil
	}
	if _, err := mail.ParseAddress(email); err != nil {
		return fmt.Errorf("UZI_SEED_EMAIL is not a valid email address")
	}
	password := os.Getenv("UZI_SEED_PASSWORD")
	if len(password) < auth.MinPasswordLen {
		return fmt.Errorf("UZI_SEED_PASSWORD must be at least %d characters when UZI_SEED_EMAIL is set", auth.MinPasswordLen)
	}
	if len(password) > auth.MaxPasswordLen {
		return fmt.Errorf("UZI_SEED_PASSWORD is too long (max %d characters)", auth.MaxPasswordLen)
	}
	cfg.SeedEmail = email
	cfg.SeedPassword = password
	cfg.SeedName = strings.TrimSpace(os.Getenv("UZI_SEED_NAME"))
	return nil
}

// loadSeedForge reads and validates the optional startup forge-connection seed.
// It is off unless UZI_SEED_FORGE_PAT is set; when set it requires the admin
// seed (the connection belongs to that user) and a base URL that is in the
// FORGE_ALLOWED_BASE_URLS allowlist, or boot fails — a set-but-invalid seed is a
// loud misconfiguration, consistent with the other static boot guards.
// UZI_SEED_FORGE_BASE_URL defaults to the first allowlisted entry and is stored
// normalized so it matches how a connection's base_url is stored. Runtime forge
// failures at seed time are handled non-fatally by the seeder, not here.
func loadSeedForge(cfg *Config) error {
	pat := strings.TrimSpace(os.Getenv("UZI_SEED_FORGE_PAT"))
	if pat == "" {
		return nil
	}
	if cfg.SeedEmail == "" {
		return fmt.Errorf("UZI_SEED_FORGE_PAT is set but UZI_SEED_EMAIL is not; the seeded forge connection must belong to the seed admin")
	}
	baseURL := strings.TrimSpace(os.Getenv("UZI_SEED_FORGE_BASE_URL"))
	if baseURL == "" {
		baseURL = cfg.ForgeAllowedBaseURLs[0] // already normalized; default to the first allowlisted forge
	}
	if !cfg.ForgeBaseURLAllowed(baseURL) {
		return fmt.Errorf("UZI_SEED_FORGE_BASE_URL %q is not in FORGE_ALLOWED_BASE_URLS", baseURL)
	}
	norm, err := NormalizeForgeBaseURL(baseURL)
	if err != nil {
		return fmt.Errorf("UZI_SEED_FORGE_BASE_URL: %w", err)
	}
	cfg.SeedForgePAT = pat
	cfg.SeedForgeBaseURL = norm
	cfg.SeedForgeRepos = parseCommaList(os.Getenv("UZI_SEED_FORGE_REPOS"))
	return nil
}

// parseCommaList splits a comma-separated env value into trimmed, non-empty,
// de-duplicated entries, preserving first-seen order.
func parseCommaList(raw string) []string {
	var out []string
	seen := map[string]struct{}{}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if _, dup := seen[part]; dup {
			continue
		}
		seen[part] = struct{}{}
		out = append(out, part)
	}
	return out
}

// parseAllowedBaseURLs parses the comma-separated SSRF allowlist. Every entry
// must be an absolute https URL; it is normalized to scheme://host[:port] with
// no path or trailing slash so comparisons against a connection's base_url are
// exact. An empty list or any non-https/malformed entry is a hard error — an
// empty allowlist would silently forbid all connections, and a plain-http entry
// would defeat the guard.
func parseAllowedBaseURLs(raw string) ([]string, error) {
	var out []string
	seen := map[string]struct{}{}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		norm, err := NormalizeForgeBaseURL(part)
		if err != nil {
			return nil, fmt.Errorf("FORGE_ALLOWED_BASE_URLS: %w", err)
		}
		if _, dup := seen[norm]; dup {
			continue
		}
		seen[norm] = struct{}{}
		out = append(out, norm)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("FORGE_ALLOWED_BASE_URLS resolves to an empty allowlist; set at least one https URL")
	}
	return out, nil
}

// NormalizeForgeBaseURL validates and canonicalizes a forge base URL to
// scheme://host[:port]. It requires the https scheme and a host, and strips any
// path, query, or fragment. Used both to build the allowlist and to check a
// user-supplied base_url against it.
func NormalizeForgeBaseURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("invalid URL %q: %w", raw, err)
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return "", fmt.Errorf("base URL %q must use https", raw)
	}
	if u.Host == "" {
		return "", fmt.Errorf("base URL %q has no host", raw)
	}
	return "https://" + strings.ToLower(u.Host), nil
}

// ForgeBaseURLAllowed reports whether raw normalizes to an allowlisted base URL.
func (c Config) ForgeBaseURLAllowed(raw string) bool {
	norm, err := NormalizeForgeBaseURL(raw)
	if err != nil {
		return false
	}
	for _, a := range c.ForgeAllowedBaseURLs {
		if a == norm {
			return true
		}
	}
	return false
}

// validateSecret enforces the boot guard: reject missing, empty, placeholder,
// or too-short secrets.
func validateSecret(raw string) ([]byte, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil, fmt.Errorf("JWT_SECRET is required and must not be empty; generate one with: openssl rand -hex 64")
	}
	if _, bad := placeholderSecrets[strings.ToLower(s)]; bad {
		return nil, fmt.Errorf("JWT_SECRET is set to a well-known placeholder value; generate a real one with: openssl rand -hex 64")
	}
	if len(s) < minSecretLen {
		return nil, fmt.Errorf("JWT_SECRET is too short (%d chars, need >= %d); generate one with: openssl rand -hex 64", len(s), minSecretLen)
	}
	return []byte(s), nil
}

// originIsHTTPS reports whether the given origin uses the https scheme, which
// is what makes Secure cookies deliverable (browsers drop Secure cookies on
// plain-HTTP pages).
func originIsHTTPS(origin string) bool {
	u, err := url.Parse(strings.TrimSpace(origin))
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Scheme, "https")
}

// parseTrustedProxies parses a comma-separated list of CIDRs. Invalid entries
// are warned and skipped. Returns nil for empty input (never trust XFF).
func parseTrustedProxies(raw string) []*net.IPNet {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var nets []*net.IPNet
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		_, cidr, err := net.ParseCIDR(part)
		if err != nil {
			slog.Warn("TRUSTED_PROXIES: skipping invalid CIDR", "cidr", part, "error", err)
			continue
		}
		nets = append(nets, cidr)
	}
	return nets
}

func getenv(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func parseDuration(key string, def time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	if d, err := time.ParseDuration(raw); err == nil && d > 0 {
		return d
	}
	return def
}

func parseInt(key string, def int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	var n int
	if _, err := fmt.Sscanf(raw, "%d", &n); err == nil && n > 0 {
		return n
	}
	return def
}
