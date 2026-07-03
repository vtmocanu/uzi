// Package config loads and validates runtime configuration from the
// environment. It fails loud at boot on an unsafe or missing JWT secret so a
// misconfigured deployment never silently runs with guessable credentials.
package config

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
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
}

// placeholderSecrets are values that must never be accepted as a real signing
// key. They are the well-known dev fallbacks the inspiration projects shipped
// (and refused, in bottega's case) plus common footguns.
var placeholderSecrets = map[string]struct{}{
	"multica-dev-secret-change-in-production": {},
	"bottega-dev-secret-change-in-production": {},
	"uzi-dev-secret-change-in-production":     {},
	"change-me":                              {},
	"changeme":                               {},
	"secret":                                 {},
	"password":                               {},
}

// minSecretLen is the minimum acceptable JWT secret length. `openssl rand -hex
// 64` produces 128 chars; this floor rejects obviously weak keys while staying
// permissive enough for hand-set values in local demos.
const minSecretLen = 16

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
	}

	if strings.TrimSpace(cfg.DatabaseURL) == "" {
		return Config{}, fmt.Errorf("DATABASE_URL is required")
	}

	secret, err := validateSecret(os.Getenv("JWT_SECRET"))
	if err != nil {
		return Config{}, err
	}
	cfg.JWTSecret = secret

	cfg.CookieSecure = originIsHTTPS(cfg.FrontendOrigin)

	return cfg, nil
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
