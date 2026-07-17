package config

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeToken drops a token file in a temp dir and returns its path.
func writeToken(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}
	return path
}

func TestLoadReadsTheTokenFromFileAndDefaultsTheKnobs(t *testing.T) {
	t.Setenv("UZI_API_URL", "https://uzi.example.com/some/path")
	t.Setenv("UZI_CONTROLLER_TOKEN_FILE", writeToken(t, "the-controller-token\n"))

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// The base URL is stripped to scheme://host: the client appends its own path.
	if cfg.APIBaseURL != "https://uzi.example.com" {
		t.Fatalf("APIBaseURL = %q", cfg.APIBaseURL)
	}
	// A mounted secret almost always carries a trailing newline; a token that
	// authenticates or not depending on one is a debugging trap.
	if cfg.Token != "the-controller-token" {
		t.Fatalf("Token = %q, want it trimmed", cfg.Token)
	}
	if cfg.PollInterval != 10*time.Second || cfg.HTTPTimeout != 15*time.Second {
		t.Fatalf("defaults = %v / %v", cfg.PollInterval, cfg.HTTPTimeout)
	}
}

// The credential is deliberately file-only: an env-borne secret is readable via
// /proc/<pid>/environ, the leak class docs/proc-hardening.md closed and PRD #58
// Decision 3 keeps closed. There must be no env fallback to drift back into.
func TestLoadRequiresTheTokenFileAndHasNoEnvFallback(t *testing.T) {
	t.Setenv("UZI_API_URL", "https://uzi.example.com")
	t.Setenv("UZI_CONTROLLER_TOKEN", "should-be-ignored-entirely")

	_, err := Load()
	if err == nil {
		t.Fatal("want an error when UZI_CONTROLLER_TOKEN_FILE is unset")
	}
	if !strings.Contains(err.Error(), "UZI_CONTROLLER_TOKEN_FILE") {
		t.Fatalf("err = %v, want it to name the file var", err)
	}
}

func TestLoadRejectsAnEmptyTokenFile(t *testing.T) {
	t.Setenv("UZI_API_URL", "https://uzi.example.com")
	t.Setenv("UZI_CONTROLLER_TOKEN_FILE", writeToken(t, "   \n"))

	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("err = %v, want an empty-token error", err)
	}
}

func TestLoadRejectsABadAPIURL(t *testing.T) {
	tokenFile := writeToken(t, "t")
	for _, tc := range []struct{ name, url string }{
		{"unset", ""},
		{"no host", "https://"},
		{"wrong scheme", "ftp://uzi.example.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("UZI_API_URL", tc.url)
			t.Setenv("UZI_CONTROLLER_TOKEN_FILE", tokenFile)
			if _, err := Load(); err == nil {
				t.Fatalf("UZI_API_URL=%q loaded without error", tc.url)
			}
		})
	}
}

// http is permitted: the controller runs beside the api in-cluster before M4's TLS
// listener lands, and this URL is one operator-set destination, not user input.
func TestLoadAllowsPlainHTTPForTheInClusterHop(t *testing.T) {
	t.Setenv("UZI_API_URL", "http://uzi-api:8080")
	t.Setenv("UZI_CONTROLLER_TOKEN_FILE", writeToken(t, "t"))

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.APIBaseURL != "http://uzi-api:8080" {
		t.Fatalf("APIBaseURL = %q", cfg.APIBaseURL)
	}
}

func TestLoadParsesTheIntervalKnobs(t *testing.T) {
	t.Setenv("UZI_API_URL", "https://uzi.example.com")
	t.Setenv("UZI_CONTROLLER_TOKEN_FILE", writeToken(t, "t"))
	t.Setenv("CONTROLLER_POLL_INTERVAL", "30s")
	t.Setenv("CONTROLLER_HTTP_TIMEOUT", "5s")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.PollInterval != 30*time.Second || cfg.HTTPTimeout != 5*time.Second {
		t.Fatalf("knobs = %v / %v", cfg.PollInterval, cfg.HTTPTimeout)
	}
}

// A malformed or non-positive knob falls back to the default rather than failing
// boot: these are tuning values, not security controls, which is the same split the
// api's config package draws.
func TestLoadFallsBackOnMalformedKnobs(t *testing.T) {
	t.Setenv("UZI_API_URL", "https://uzi.example.com")
	t.Setenv("UZI_CONTROLLER_TOKEN_FILE", writeToken(t, "t"))
	t.Setenv("CONTROLLER_POLL_INTERVAL", "not-a-duration")
	t.Setenv("CONTROLLER_HTTP_TIMEOUT", "0s")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.PollInterval != 10*time.Second || cfg.HTTPTimeout != 15*time.Second {
		t.Fatalf("knobs = %v / %v, want the defaults", cfg.PollInterval, cfg.HTTPTimeout)
	}
}

// caPEM returns a self-signed CA certificate in PEM form. Only the certificate is
// needed: the pool is a trust anchor, never a signing identity.
func caPEM(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "uzi-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func writeCA(t *testing.T, contents []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(path, contents, 0o644); err != nil {
		t.Fatalf("write ca: %v", err)
	}
	return path
}

// The api's certificate is cert-manager's, issued by a CA in nobody's trust store
// (PRD #58 Decision 4), so the pool is what makes the hop verifiable at all.
func TestLoadReadsTheAPICABundle(t *testing.T) {
	t.Setenv("UZI_API_URL", "https://api.uzi.svc.cluster.local:8443")
	t.Setenv("UZI_CONTROLLER_TOKEN_FILE", writeToken(t, "tok"))
	t.Setenv("UZI_API_CA_FILE", writeCA(t, caPEM(t)))

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.APICAPool == nil {
		t.Fatal("APICAPool = nil, want the mounted bundle parsed")
	}
}

// Unset is not an error: a controller pointed at a plain local api has no pod
// network to cross, and one behind a publicly-trusted cert wants the system roots.
func TestLoadWithoutACAFileLeavesTheSystemRoots(t *testing.T) {
	t.Setenv("UZI_API_URL", "https://uzi.example.com")
	t.Setenv("UZI_CONTROLLER_TOKEN_FILE", writeToken(t, "tok"))

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.APICAPool != nil {
		t.Fatal("APICAPool != nil with UZI_API_CA_FILE unset")
	}
}

func TestLoadRejectsABadAPICAFile(t *testing.T) {
	t.Run("missing file", func(t *testing.T) {
		t.Setenv("UZI_API_URL", "https://uzi.example.com")
		t.Setenv("UZI_CONTROLLER_TOKEN_FILE", writeToken(t, "tok"))
		t.Setenv("UZI_API_CA_FILE", filepath.Join(t.TempDir(), "absent.crt"))
		if _, err := Load(); err == nil {
			t.Fatal("Load: want error for an unreadable CA file, got nil")
		}
	})

	// An empty pool would fail every handshake at runtime. Boot is the honest place.
	t.Run("no PEM in it", func(t *testing.T) {
		t.Setenv("UZI_API_URL", "https://uzi.example.com")
		t.Setenv("UZI_CONTROLLER_TOKEN_FILE", writeToken(t, "tok"))
		t.Setenv("UZI_API_CA_FILE", writeCA(t, []byte("not a certificate")))
		if _, err := Load(); err == nil {
			t.Fatal("Load: want error for a CA file with no PEM certificate, got nil")
		}
	})

	// The shape of a half-finished TLS rollout: the operator set a CA and believes
	// the hop is encrypted, while http quietly ignores it.
	t.Run("CA against an http api aborts boot", func(t *testing.T) {
		t.Setenv("UZI_API_URL", "http://api:8080")
		t.Setenv("UZI_CONTROLLER_TOKEN_FILE", writeToken(t, "tok"))
		t.Setenv("UZI_API_CA_FILE", writeCA(t, caPEM(t)))
		if _, err := Load(); err == nil {
			t.Fatal("Load: want error for UZI_API_CA_FILE with an http UZI_API_URL, got nil")
		}
	})
}
