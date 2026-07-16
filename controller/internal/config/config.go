// Package config loads and validates the controller's runtime configuration from
// the environment. Like the api's config package it fails loud at boot rather than
// running half-configured.
package config

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
)

// Config holds the controller's runtime settings.
type Config struct {
	// APIBaseURL is the uzi api's base URL (scheme://host[:port], no path). https
	// from M4 on, when the api gains its TLS listener (Decision 4) — this is the hop
	// that carries join tokens across a shared cluster's pod network.
	APIBaseURL string
	// Token is the controller's bearer credential. Read from a FILE, never an env
	// var: an env-borne secret is readable via /proc/<pid>/environ, the leak class
	// docs/proc-hardening.md closed for the worker and that PRD #58 Decision 3
	// deliberately keeps closed by file-mounting the worker's join token. The same
	// argument applies verbatim to this process's own credential, so there is no
	// env-var fallback to be tempted by.
	Token string
	// PollInterval is the reconcile cadence: how often the whole fleet's desired
	// state is fetched and reconciled against the cluster.
	PollInterval time.Duration
	// HTTPTimeout bounds every call to the api, so an unreachable api can never
	// wedge the reconcile loop — the same guard the api applies to its own outbound
	// calls (ForgeHTTPTimeout and friends).
	HTTPTimeout time.Duration
}

// Load reads and validates the configuration.
func Load() (Config, error) {
	cfg := Config{
		PollInterval: parseDuration("CONTROLLER_POLL_INTERVAL", 10*time.Second),
		HTTPTimeout:  parseDuration("CONTROLLER_HTTP_TIMEOUT", 15*time.Second),
	}

	base := strings.TrimSpace(os.Getenv("UZI_API_URL"))
	if base == "" {
		return Config{}, fmt.Errorf("UZI_API_URL is required (the uzi api's base URL)")
	}
	norm, err := normalizeBaseURL(base)
	if err != nil {
		return Config{}, err
	}
	cfg.APIBaseURL = norm

	path := strings.TrimSpace(os.Getenv("UZI_CONTROLLER_TOKEN_FILE"))
	if path == "" {
		return Config{}, fmt.Errorf("UZI_CONTROLLER_TOKEN_FILE is required (the path to the mounted controller bearer token; this credential is deliberately not accepted from an env var)")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		// The path is operator config, not a secret; the contents never surface.
		return Config{}, fmt.Errorf("UZI_CONTROLLER_TOKEN_FILE: read %s: %w", path, err)
	}
	// Trailing newlines are near-universal in mounted secrets and in whatever the
	// operator echoed into one; a token that authenticates or not depending on a
	// stray \n is a debugging trap with no upside.
	cfg.Token = strings.TrimSpace(string(raw))
	if cfg.Token == "" {
		return Config{}, fmt.Errorf("UZI_CONTROLLER_TOKEN_FILE: %s is empty", path)
	}
	return cfg, nil
}

// normalizeBaseURL validates the api base URL and strips it to scheme://host[:port].
// http is permitted: the controller may run beside the api inside the cluster
// before M4 adds the TLS listener, and unlike the forge allowlist this URL is not
// an SSRF surface (it is one operator-set destination, not user input).
func normalizeBaseURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("UZI_API_URL is not a valid URL: %w", err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return "", fmt.Errorf("UZI_API_URL %q must use http or https", raw)
	}
	if u.Host == "" {
		return "", fmt.Errorf("UZI_API_URL %q has no host", raw)
	}
	return strings.ToLower(u.Scheme) + "://" + u.Host, nil
}

// parseDuration mirrors the api's: empty or malformed falls back to the default,
// and a non-positive value is rejected (neither knob has a meaningful zero — a
// controller that never polls is a controller that does nothing).
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
