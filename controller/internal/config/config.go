// Package config loads and validates the controller's runtime configuration from
// the environment. Like the api's config package it fails loud at boot rather than
// running half-configured.
package config

import (
	"crypto/x509"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
)

// Config holds the controller's runtime settings.
type Config struct {
	// APIBaseURL is the uzi api's base URL (scheme://host[:port], no path). https
	// in k8s, where the api serves its TLS listener (Decision 4) — this is the hop
	// that carries join tokens across a shared cluster's pod network.
	APIBaseURL string
	// APICAPool verifies the api's TLS certificate (UZI_API_CA_FILE, a PEM bundle).
	//
	// nil means "use the system roots", which is right for an api behind a
	// publicly-trusted cert and wrong for the cluster-issued one: cert-manager's CA
	// is in nobody's trust store, so verification against the system roots would
	// fail closed (a loud connection error, never a silent downgrade).
	//
	// The pool is EXCLUSIVE, not additive, when set: the api is one operator-set
	// destination with one known issuer, so trusting every public CA on top of it
	// only widens who can impersonate the hop that hands out join tokens.
	APICAPool *x509.CertPool
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

	if err := loadAPICA(&cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// loadAPICA reads the optional CA bundle used to verify the api's TLS certificate
// (Decision 4). Unset leaves verification on the system roots.
//
// A CA file against an http base URL is a loud failure rather than an ignored
// setting: it is precisely the shape of a half-finished TLS rollout, and the
// operator who set it believes the hop is encrypted. The same reasoning the api
// applies to a controller token hash set while hosting is off.
func loadAPICA(cfg *Config) error {
	path := strings.TrimSpace(os.Getenv("UZI_API_CA_FILE"))
	if path == "" {
		return nil
	}
	if !strings.HasPrefix(cfg.APIBaseURL, "https://") {
		return fmt.Errorf("UZI_API_CA_FILE is set but UZI_API_URL is not https; the CA would never be consulted and the join tokens this controller carries would cross the pod network in the clear")
	}
	pem, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("UZI_API_CA_FILE: read %s: %w", path, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		// AppendCertsFromPEM reports only "did anything parse", so there is nothing
		// more specific to say. An empty pool would fail every handshake at runtime;
		// failing here makes it a boot error instead.
		return fmt.Errorf("UZI_API_CA_FILE: %s contains no PEM certificate", path)
	}
	cfg.APICAPool = pool
	return nil
}

// normalizeBaseURL validates the api base URL and strips it to scheme://host[:port].
// http is still permitted (a local dev controller pointed at a plain api, where
// there is no pod network to cross), and unlike the forge allowlist this URL is not
// an SSRF surface (it is one operator-set destination, not user input) — but the
// k8s deployment sets https and a CA: see loadAPICA, and the chart's api.tls values.
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
