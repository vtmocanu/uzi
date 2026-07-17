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

	// APICAPEM is the raw PEM behind APICAPool, kept so the controller can RELAY it
	// to each worker (M3). Hosted workers live in a different namespace and cannot
	// mount the api's TLS Secret, so the CA rides the per-worker Secret this
	// controller already creates — no new RBAC verb, and Decision 1's Secrets line
	// stays verbatim.
	APICAPEM []byte

	// --- the worker namespace (M3) --------------------------------------------
	// Everything below describes the pods this controller renders. It is all
	// operator config: the api sends NAMES and this side owns every mapping to a
	// concrete pod-spec value (Decision 1), so none of it can come off the wire.

	// WorkerNamespace is the dedicated namespace holding nothing but hosted workers.
	// The emptiness is the point: it is what bounds even a compromised controller.
	WorkerNamespace string
	// WorkerServiceAccount is the workers' own zero-permission SA. It carries the
	// imagePullSecrets (so this process need not know about Harbor) and its token is
	// never automounted.
	WorkerServiceAccount string
	// WorkerImageRepo is the registry prefix the per-template agent images live under
	// (<repo>/agent-<template>:<tag>).
	WorkerImageRepo string
	// WorkerImageTag is the release tag of the agent image to run. It lives HERE, not
	// in the api's config: the api knows no image tag and M1's wire carries no image
	// field. Changing it is what rolls every hosted worker onto a new release
	// (Decision 9), via the spec hash.
	WorkerImageTag string
	// WorkerAPIURL is what a WORKER dials — necessarily the FQDN
	// (https://api.<ns>.svc.cluster.local:8443), since a short Service name does not
	// resolve cross-namespace. Deliberately separate from APIBaseURL, which is this
	// controller's own hop and may legitimately be the short name.
	WorkerAPIURL string
	// WorkerStorageClass is optional; empty means the cluster default.
	WorkerStorageClass string
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
	if err := loadWorkerSettings(&cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// loadWorkerSettings reads the worker-namespace knobs (M3).
//
// All required, none defaulted, and that is deliberate in each case: this process
// has exactly one job, so a controller that cannot describe the pods it renders has
// nothing to do. Every default that suggests itself is worse than refusing to boot
// — an empty image repo silently means Docker Hub, a "latest" tag means a fleet on
// an unknown release, and a guessed namespace means listing somewhere this
// controller has no RBAC.
func loadWorkerSettings(cfg *Config) error {
	required := []struct {
		key  string
		dst  *string
		what string
	}{
		{"UZI_WORKER_NAMESPACE", &cfg.WorkerNamespace, "the dedicated namespace hosted workers are rendered into"},
		{"UZI_WORKER_SERVICE_ACCOUNT", &cfg.WorkerServiceAccount, "the workers' own zero-permission ServiceAccount"},
		{"UZI_WORKER_IMAGE_REPO", &cfg.WorkerImageRepo, "the registry prefix the per-template agent images live under"},
		{"UZI_WORKER_IMAGE_TAG", &cfg.WorkerImageTag, "the release tag of the agent image to run"},
		{"UZI_WORKER_API_URL", &cfg.WorkerAPIURL, "the api URL a WORKER dials (the FQDN: a short Service name does not resolve cross-namespace)"},
	}
	for _, r := range required {
		v := strings.TrimSpace(os.Getenv(r.key))
		if v == "" {
			return fmt.Errorf("%s is required (%s)", r.key, r.what)
		}
		*r.dst = v
	}
	if _, err := normalizeBaseURL(cfg.WorkerAPIURL); err != nil {
		return fmt.Errorf("UZI_WORKER_API_URL: %w", err)
	}
	// A worker verifying against the system roots cannot verify a cluster-issued
	// cert, so a CA pinned for THIS process's hop and not relayed to the workers is a
	// half-finished rollout that fails at the far end instead of here. The two travel
	// together or the operator hears about it at boot.
	if cfg.APICAPool != nil && !strings.HasPrefix(strings.ToLower(cfg.WorkerAPIURL), "https://") {
		return fmt.Errorf("UZI_API_CA_FILE is set but UZI_WORKER_API_URL is not https; the workers would carry a CA they never consult and their claim traffic — which carries the user's decrypted forge PAT and Anthropic token — would cross the pod network in the clear")
	}
	cfg.WorkerStorageClass = strings.TrimSpace(os.Getenv("UZI_WORKER_STORAGE_CLASS"))
	return nil
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
	// Kept raw as well: the pool verifies THIS process's hop, the PEM is what gets
	// relayed into each worker's Secret so the worker can verify its own.
	cfg.APICAPEM = pem
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
