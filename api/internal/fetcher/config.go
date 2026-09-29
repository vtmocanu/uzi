package fetcher

import (
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is uzi-fetcher's runtime configuration, read from the environment:
//
//	UZI_FETCHER_ADDR              listen address (default ":8443")
//	UZI_FETCHER_TLS_CERT          PEM certificate of the worker-facing TLS listener (required)
//	UZI_FETCHER_TLS_KEY           its PEM key (required)
//	UZI_API_URL                   the api base URL, https (required)
//	UZI_API_CA_FILE               PEM CA bundle that is the only trust anchor for the api's
//	                              certificate (optional; unset means the system roots)
//	UZI_FETCHER_TOKEN_FILE        path to the fetcher's service token (required; the token is
//	                              deliberately not accepted from an env var)
//	UZI_FETCHER_BLOCKED_CIDRS     comma-separated extra refused ranges: the cluster's pod,
//	                              service and node CIDRs
//	UZI_FETCHER_MAX_FILE_BYTES    the fetcher's per-file ceiling (default 25 MiB); the api's
//	                              per-file cap applies below it
//	UZI_FETCHER_TIMEOUT           one fetch's total time limit (Go duration, default 60s)
//	UZI_FETCHER_MAX_INFLIGHT      concurrent fetches per process (default 16)
type Config struct {
	Addr         string
	TLSCertFile  string
	TLSKeyFile   string
	APIURL       string
	APICAPool    *x509.CertPool
	ServiceToken string
	Policy       AddressPolicy
	MaxFileBytes int64
	Timeout      time.Duration
	MaxInflight  int
}

// LoadConfig reads Config through getenv, refusing to start on anything missing or
// malformed.
func LoadConfig(getenv func(string) string) (Config, error) {
	get := func(k string) string { return strings.TrimSpace(getenv(k)) }
	cfg := Config{
		Addr:         get("UZI_FETCHER_ADDR"),
		TLSCertFile:  get("UZI_FETCHER_TLS_CERT"),
		TLSKeyFile:   get("UZI_FETCHER_TLS_KEY"),
		APIURL:       strings.TrimRight(get("UZI_API_URL"), "/"),
		MaxFileBytes: DefaultMaxFileBytes,
		Timeout:      DefaultTimeout,
		MaxInflight:  DefaultMaxInflight,
	}
	if cfg.Addr == "" {
		cfg.Addr = ":8443"
	}
	// There is no plain-HTTP mode: both hops carry credentials (the worker hop the run's
	// fetch credential, the api hop the service token and every run credential).
	if cfg.TLSCertFile == "" || cfg.TLSKeyFile == "" {
		return Config{}, errors.New("UZI_FETCHER_TLS_CERT and UZI_FETCHER_TLS_KEY are required: the worker hop carries the run's fetch credential")
	}
	if cfg.APIURL == "" {
		return Config{}, errors.New("UZI_API_URL is required (the uzi api's base URL)")
	}
	if !strings.HasPrefix(cfg.APIURL, "https://") {
		return Config{}, fmt.Errorf("UZI_API_URL %q must be https: the fetcher's service token and every run credential cross that hop", cfg.APIURL)
	}
	if path := get("UZI_API_CA_FILE"); path != "" {
		pem, err := os.ReadFile(path) //nolint:gosec // G304: operator config naming the mounted CA bundle.
		if err != nil {
			return Config{}, fmt.Errorf("UZI_API_CA_FILE: read %s: %w", path, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return Config{}, fmt.Errorf("UZI_API_CA_FILE: %s contains no PEM certificate", path)
		}
		cfg.APICAPool = pool
	}

	path := get("UZI_FETCHER_TOKEN_FILE")
	if path == "" {
		return Config{}, errors.New("UZI_FETCHER_TOKEN_FILE is required (the path to the mounted fetcher service token)")
	}
	raw, err := os.ReadFile(path) //nolint:gosec // G304: operator config naming the mounted token file.
	if err != nil {
		return Config{}, fmt.Errorf("UZI_FETCHER_TOKEN_FILE: read %s: %w", path, err)
	}
	cfg.ServiceToken = strings.TrimSpace(string(raw))
	if cfg.ServiceToken == "" {
		return Config{}, fmt.Errorf("UZI_FETCHER_TOKEN_FILE: %s is empty", path)
	}

	var cidrs []string
	if raw := get("UZI_FETCHER_BLOCKED_CIDRS"); raw != "" {
		cidrs = strings.Split(raw, ",")
	}
	cfg.Policy, err = NewAddressPolicy(cidrs)
	if err != nil {
		return Config{}, fmt.Errorf("UZI_FETCHER_BLOCKED_CIDRS: %w", err)
	}

	if raw := get("UZI_FETCHER_MAX_FILE_BYTES"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n <= 0 {
			return Config{}, fmt.Errorf("UZI_FETCHER_MAX_FILE_BYTES=%q must be a positive integer", raw)
		}
		cfg.MaxFileBytes = n
	}
	if raw := get("UZI_FETCHER_TIMEOUT"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d <= 0 {
			return Config{}, fmt.Errorf("UZI_FETCHER_TIMEOUT=%q must be a positive duration", raw)
		}
		cfg.Timeout = d
	}
	if raw := get("UZI_FETCHER_MAX_INFLIGHT"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			return Config{}, fmt.Errorf("UZI_FETCHER_MAX_INFLIGHT=%q must be a positive integer", raw)
		}
		cfg.MaxInflight = n
	}
	return cfg, nil
}

// FetcherOptions returns the Options production runs the Fetcher with: the configured
// policy and limits, the system resolver and the system roots. It is the only way
// cmd/fetcher builds Options, and it sets no test seam.
func (c Config) FetcherOptions(userAgent string) Options {
	return Options{
		Policy:       c.Policy,
		MaxFileBytes: c.MaxFileBytes,
		Timeout:      c.Timeout,
		UserAgent:    userAgent,
	}
}
