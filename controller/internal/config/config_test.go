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

// setWorkerEnv supplies the worker-namespace settings M3 made required. Tests that
// expect Load to SUCCEED need it; tests asserting a failure earlier in Load do not,
// since Load validates in order.
func setWorkerEnv(t *testing.T) {
	t.Helper()
	t.Setenv("UZI_WORKER_NAMESPACE", "uzi-workers")
	t.Setenv("UZI_WORKER_SERVICE_ACCOUNT", "uzi-hosted-worker")
	t.Setenv("UZI_WORKER_IMAGE_REPO", "harbor.example.com/uzi")
	t.Setenv("UZI_WORKER_IMAGE_TAG", "v1.0.0")
	t.Setenv("UZI_WORKER_API_URL", "https://api.uzi.svc.cluster.local:8443")
}

func TestLoadReadsTheTokenFromFileAndDefaultsTheKnobs(t *testing.T) {
	setWorkerEnv(t)
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
	setWorkerEnv(t)
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
	setWorkerEnv(t)
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

// The drain policy (PRD #422 M5): DrainDeadline defaults to 24h and ForceRoll to false
// when the envs are unset, a set UZI_WORKER_DRAIN_DEADLINE parses, UZI_WORKER_FORCE_ROLL
// "true" parses true, and an unparseable force-roll value is a boot error (the emergency
// lever must never be silently disarmed by a typo).
func TestLoadParsesTheDrainPolicy(t *testing.T) {
	t.Run("defaults when unset", func(t *testing.T) {
		setWorkerEnv(t)
		t.Setenv("UZI_API_URL", "https://uzi.example.com")
		t.Setenv("UZI_CONTROLLER_TOKEN_FILE", writeToken(t, "tok"))
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.DrainDeadline != 24*time.Hour {
			t.Errorf("DrainDeadline = %v, want the default 24h", cfg.DrainDeadline)
		}
		if cfg.ForceRoll {
			t.Error("ForceRoll = true, want the default false when UZI_WORKER_FORCE_ROLL is unset")
		}
	})

	t.Run("a set deadline parses", func(t *testing.T) {
		setWorkerEnv(t)
		t.Setenv("UZI_API_URL", "https://uzi.example.com")
		t.Setenv("UZI_CONTROLLER_TOKEN_FILE", writeToken(t, "tok"))
		t.Setenv("UZI_WORKER_DRAIN_DEADLINE", "1h")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.DrainDeadline != time.Hour {
			t.Errorf("DrainDeadline = %v, want 1h", cfg.DrainDeadline)
		}
	})

	t.Run("force-roll true parses true", func(t *testing.T) {
		setWorkerEnv(t)
		t.Setenv("UZI_API_URL", "https://uzi.example.com")
		t.Setenv("UZI_CONTROLLER_TOKEN_FILE", writeToken(t, "tok"))
		t.Setenv("UZI_WORKER_FORCE_ROLL", "true")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if !cfg.ForceRoll {
			t.Error("UZI_WORKER_FORCE_ROLL=true must set ForceRoll")
		}
	})

	t.Run("an unparseable force-roll value is a boot error", func(t *testing.T) {
		setWorkerEnv(t)
		t.Setenv("UZI_API_URL", "https://uzi.example.com")
		t.Setenv("UZI_CONTROLLER_TOKEN_FILE", writeToken(t, "tok"))
		t.Setenv("UZI_WORKER_FORCE_ROLL", "maybe")
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "UZI_WORKER_FORCE_ROLL") {
			t.Fatalf("err = %v, want a boot refusal naming UZI_WORKER_FORCE_ROLL", err)
		}
	})
}

// The disk self-heal policy (PRD #837 M4): WorkerDiskRecycleEnabled DEFAULTS TRUE when
// UZI_WORKER_DISK_RECYCLE_ENABLED is unset, honours an explicit "false", and treats a
// set-but-unparseable value as a BOOT error (an operator disabling self-heal by typo
// must not silently leave it ON). The cooldown defaults to 1h and parses a set value.
func TestLoadParsesTheDiskRecyclePolicy(t *testing.T) {
	t.Run("enabled defaults true when unset", func(t *testing.T) {
		setWorkerEnv(t)
		t.Setenv("UZI_API_URL", "https://uzi.example.com")
		t.Setenv("UZI_CONTROLLER_TOKEN_FILE", writeToken(t, "tok"))
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if !cfg.WorkerDiskRecycleEnabled {
			t.Error("WorkerDiskRecycleEnabled = false, want the default true when UZI_WORKER_DISK_RECYCLE_ENABLED is unset")
		}
		if cfg.WorkerDiskRecycleCooldown != time.Hour {
			t.Errorf("WorkerDiskRecycleCooldown = %v, want the default 1h", cfg.WorkerDiskRecycleCooldown)
		}
	})

	t.Run("an explicit false disables self-heal", func(t *testing.T) {
		setWorkerEnv(t)
		t.Setenv("UZI_API_URL", "https://uzi.example.com")
		t.Setenv("UZI_CONTROLLER_TOKEN_FILE", writeToken(t, "tok"))
		t.Setenv("UZI_WORKER_DISK_RECYCLE_ENABLED", "false")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.WorkerDiskRecycleEnabled {
			t.Error("UZI_WORKER_DISK_RECYCLE_ENABLED=false must clear WorkerDiskRecycleEnabled")
		}
	})

	t.Run("a set cooldown parses", func(t *testing.T) {
		setWorkerEnv(t)
		t.Setenv("UZI_API_URL", "https://uzi.example.com")
		t.Setenv("UZI_CONTROLLER_TOKEN_FILE", writeToken(t, "tok"))
		t.Setenv("UZI_WORKER_DISK_RECYCLE_COOLDOWN", "30m")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.WorkerDiskRecycleCooldown != 30*time.Minute {
			t.Errorf("WorkerDiskRecycleCooldown = %v, want 30m", cfg.WorkerDiskRecycleCooldown)
		}
	})

	t.Run("an unparseable enabled value is a boot error", func(t *testing.T) {
		setWorkerEnv(t)
		t.Setenv("UZI_API_URL", "https://uzi.example.com")
		t.Setenv("UZI_CONTROLLER_TOKEN_FILE", writeToken(t, "tok"))
		t.Setenv("UZI_WORKER_DISK_RECYCLE_ENABLED", "maybe")
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "UZI_WORKER_DISK_RECYCLE_ENABLED") {
			t.Fatalf("err = %v, want a boot refusal naming UZI_WORKER_DISK_RECYCLE_ENABLED", err)
		}
	})
}

// A malformed or non-positive knob falls back to the default rather than failing
// boot: these are tuning values, not security controls, which is the same split the
// api's config package draws.
func TestLoadFallsBackOnMalformedKnobs(t *testing.T) {
	setWorkerEnv(t)
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
	if err := os.WriteFile(path, contents, 0o644); err != nil { //nolint:gosec // G306: a CA certificate is public trust material, not a secret (unlike the token file, written 0o600); this is a throwaway test fixture in t.TempDir().
		t.Fatalf("write ca: %v", err)
	}
	return path
}

// The api's certificate is cert-manager's, issued by a CA in nobody's trust store
// (PRD #58 Decision 4), so the pool is what makes the hop verifiable at all.
func TestLoadReadsTheAPICABundle(t *testing.T) {
	setWorkerEnv(t)
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
	setWorkerEnv(t)
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

// --- worker-namespace settings (M3) ----------------------------------------

func TestLoadReadsTheWorkerSettings(t *testing.T) {
	setWorkerEnv(t)
	t.Setenv("UZI_API_URL", "https://uzi.example.com")
	t.Setenv("UZI_CONTROLLER_TOKEN_FILE", writeToken(t, "tok"))
	t.Setenv("UZI_WORKER_STORAGE_CLASS", "standard")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.WorkerNamespace != "uzi-workers" || cfg.WorkerServiceAccount != "uzi-hosted-worker" {
		t.Errorf("namespace/sa = %q/%q", cfg.WorkerNamespace, cfg.WorkerServiceAccount)
	}
	if cfg.WorkerImageRepo != "harbor.example.com/uzi" || cfg.WorkerImageTag != "v1.0.0" {
		t.Errorf("image = %q:%q", cfg.WorkerImageRepo, cfg.WorkerImageTag)
	}
	if cfg.WorkerAPIURL != "https://api.uzi.svc.cluster.local:8443" {
		t.Errorf("WorkerAPIURL = %q", cfg.WorkerAPIURL)
	}
	if cfg.WorkerStorageClass != "standard" {
		t.Errorf("WorkerStorageClass = %q", cfg.WorkerStorageClass)
	}
}

// Every one of these is required, and none has a defensible default: an empty image
// repo silently means Docker Hub, a "latest" tag means a fleet on an unknown
// release, and a guessed namespace means listing where this controller has no RBAC.
// A controller that cannot describe the pods it renders has nothing to do.
func TestLoadRequiresEveryWorkerSetting(t *testing.T) {
	for _, key := range []string{
		"UZI_WORKER_NAMESPACE",
		"UZI_WORKER_SERVICE_ACCOUNT",
		"UZI_WORKER_IMAGE_REPO",
		"UZI_WORKER_IMAGE_TAG",
		"UZI_WORKER_API_URL",
	} {
		t.Run(key, func(t *testing.T) {
			setWorkerEnv(t)
			t.Setenv("UZI_API_URL", "https://uzi.example.com")
			t.Setenv("UZI_CONTROLLER_TOKEN_FILE", writeToken(t, "tok"))
			t.Setenv(key, "")

			_, err := Load()
			if err == nil {
				t.Fatalf("Load succeeded with %s unset", key)
			}
			if !strings.Contains(err.Error(), key) {
				t.Fatalf("err = %v, want it to name %s", err, key)
			}
		})
	}
}

// The per-worker slot cap defaults to 1 (unset), takes a valid integer, and rejects a
// non-integer or a value < 1 at boot rather than silently clamping — the same "fail at
// boot" rule the CA/https and docker-tier couplings follow.
func TestLoadParsesTheMaxConcurrentRuns(t *testing.T) {
	t.Run("unset defaults to 1", func(t *testing.T) {
		setWorkerEnv(t)
		t.Setenv("UZI_API_URL", "https://uzi.example.com")
		t.Setenv("UZI_CONTROLLER_TOKEN_FILE", writeToken(t, "tok"))
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.WorkerMaxConcurrentRuns != 1 {
			t.Errorf("WorkerMaxConcurrentRuns = %d, want the default 1", cfg.WorkerMaxConcurrentRuns)
		}
	})

	t.Run("a valid value is read", func(t *testing.T) {
		setWorkerEnv(t)
		t.Setenv("UZI_API_URL", "https://uzi.example.com")
		t.Setenv("UZI_CONTROLLER_TOKEN_FILE", writeToken(t, "tok"))
		t.Setenv("UZI_WORKER_MAX_CONCURRENT_RUNS", "3")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.WorkerMaxConcurrentRuns != 3 {
			t.Errorf("WorkerMaxConcurrentRuns = %d, want 3", cfg.WorkerMaxConcurrentRuns)
		}
	})

	// "257" and "100000" are ABOVE the [1, 256] ceiling (the api's advertised-cap sanity
	// band). They matter because the worker only WARNS above its soft ceiling rather than
	// rejecting, so an unbounded typo would boot and render into the pod.
	for _, bad := range []string{"0", "-1", "abc", "257", "100000"} {
		t.Run("rejects "+bad, func(t *testing.T) {
			setWorkerEnv(t)
			t.Setenv("UZI_API_URL", "https://uzi.example.com")
			t.Setenv("UZI_CONTROLLER_TOKEN_FILE", writeToken(t, "tok"))
			t.Setenv("UZI_WORKER_MAX_CONCURRENT_RUNS", bad)
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), "UZI_WORKER_MAX_CONCURRENT_RUNS") {
				t.Fatalf("err = %v, want a boot error naming UZI_WORKER_MAX_CONCURRENT_RUNS", err)
			}
		})
	}
}

// The storage class is the one optional knob: empty legitimately means "the cluster
// default", which is a real configuration rather than a missing one.
func TestLoadLeavesTheStorageClassOptional(t *testing.T) {
	setWorkerEnv(t)
	t.Setenv("UZI_API_URL", "https://uzi.example.com")
	t.Setenv("UZI_CONTROLLER_TOKEN_FILE", writeToken(t, "tok"))

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.WorkerStorageClass != "" {
		t.Errorf("WorkerStorageClass = %q, want empty (the cluster default)", cfg.WorkerStorageClass)
	}
}

func TestLoadRejectsABadWorkerAPIURL(t *testing.T) {
	for _, bad := range []string{"api.uzi.svc.cluster.local:8443", "ftp://api", "https://"} {
		t.Run(bad, func(t *testing.T) {
			setWorkerEnv(t)
			t.Setenv("UZI_API_URL", "https://uzi.example.com")
			t.Setenv("UZI_CONTROLLER_TOKEN_FILE", writeToken(t, "tok"))
			t.Setenv("UZI_WORKER_API_URL", bad)
			if _, err := Load(); err == nil {
				t.Fatalf("UZI_WORKER_API_URL=%q loaded without error", bad)
			}
		})
	}
}

// A CA pinned for THIS process's hop but not consultable by the workers is a
// half-finished TLS rollout that fails at the far end instead of here. The workers'
// claim traffic carries the user's decrypted forge PAT and Anthropic token, so it
// is exactly the hop that must not silently stay plaintext.
func TestLoadRejectsAPinnedCAWithPlainHTTPWorkers(t *testing.T) {
	setWorkerEnv(t)
	t.Setenv("UZI_API_URL", "https://api.uzi.svc.cluster.local:8443")
	t.Setenv("UZI_CONTROLLER_TOKEN_FILE", writeToken(t, "tok"))
	t.Setenv("UZI_API_CA_FILE", writeCA(t, caPEM(t)))
	t.Setenv("UZI_WORKER_API_URL", "http://api.uzi.svc.cluster.local:8080")

	_, err := Load()
	if err == nil {
		t.Fatal("Load succeeded with a pinned CA and plain-http workers")
	}
	if !strings.Contains(err.Error(), "UZI_WORKER_API_URL") {
		t.Fatalf("err = %v, want it to name the worker URL", err)
	}
}

// The PEM is kept alongside the parsed pool: the pool verifies this process's hop,
// the PEM is what gets relayed into each worker's Secret so the worker can verify
// its own. Hosted workers are in a different namespace and cannot mount the api's
// TLS Secret, so this relay is the whole mechanism — and it needs no new RBAC verb.
func TestLoadKeepsTheCAPEMForRelayToWorkers(t *testing.T) {
	pem := caPEM(t)
	setWorkerEnv(t)
	t.Setenv("UZI_API_URL", "https://api.uzi.svc.cluster.local:8443")
	t.Setenv("UZI_CONTROLLER_TOKEN_FILE", writeToken(t, "tok"))
	t.Setenv("UZI_API_CA_FILE", writeCA(t, pem))

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if string(cfg.APICAPEM) != string(pem) {
		t.Error("APICAPEM must carry the raw bundle for relay to the worker Secrets")
	}
}

// --- docker tier (PRD #83 M3) ----------------------------------------------

// setDockerBaseEnv supplies everything Load needs to reach the docker-tier
// validation successfully.
func setDockerBaseEnv(t *testing.T) {
	t.Helper()
	setWorkerEnv(t)
	t.Setenv("UZI_API_URL", "https://api.uzi.svc.cluster.local:8443")
	t.Setenv("UZI_CONTROLLER_TOKEN_FILE", writeToken(t, "tok"))
}

// The docker tier is OPT-IN: neither knob set means a controller that renders no
// docker workers, and Load must succeed with the fields empty (the kind smoke, any
// instance with docker off).
func TestLoadDockerTierOffLeavesTheFieldsEmpty(t *testing.T) {
	setDockerBaseEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.WorkerDockerNamespace != "" || cfg.WorkerDinDImage != "" {
		t.Fatalf("docker fields = %q / %q, want empty when the tier is off", cfg.WorkerDockerNamespace, cfg.WorkerDinDImage)
	}
}

// Both knobs travel together: a namespace without the sidecar image (or vice versa)
// is a half-configured docker tier that would strand every docker worker, so Load
// refuses it at boot rather than at the far end.
func TestLoadDockerNamespaceWithoutImageIsRejected(t *testing.T) {
	setDockerBaseEnv(t)
	t.Setenv("UZI_WORKER_DOCKER_NAMESPACE", "uzi-workers-docker")
	// UZI_WORKER_DIND_IMAGE deliberately unset.
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "UZI_WORKER_DIND_IMAGE") {
		t.Fatalf("err = %v, want a both-or-neither failure naming the image", err)
	}
}

func TestLoadDockerImageWithoutNamespaceIsRejected(t *testing.T) {
	setDockerBaseEnv(t)
	t.Setenv("UZI_WORKER_DIND_IMAGE", "docker:28-dind-rootless@sha256:deadbeef")
	// UZI_WORKER_DOCKER_NAMESPACE deliberately unset.
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "UZI_WORKER_DOCKER_NAMESPACE") {
		t.Fatalf("err = %v, want a both-or-neither failure naming the namespace", err)
	}
}

// The docker namespace MUST differ from the restricted default: equal namespaces
// would run privileged DinD sidecars in uzi-workers, dissolving the blast-radius
// separation that is the tier's entire reason to exist (Decision 7 / Q-B).
func TestLoadDockerNamespaceMustDifferFromTheRestrictedDefault(t *testing.T) {
	setDockerBaseEnv(t)
	t.Setenv("UZI_WORKER_DOCKER_NAMESPACE", "uzi-workers") // == UZI_WORKER_NAMESPACE
	t.Setenv("UZI_WORKER_DIND_IMAGE", "docker:28-dind-rootless@sha256:deadbeef")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "must differ") {
		t.Fatalf("err = %v, want a refusal that the docker namespace must differ from the restricted default", err)
	}
}

func TestLoadDockerTierOnPopulatesBothFields(t *testing.T) {
	setDockerBaseEnv(t)
	t.Setenv("UZI_WORKER_DOCKER_NAMESPACE", "uzi-workers-docker")
	t.Setenv("UZI_WORKER_DIND_IMAGE", "docker:28-dind-rootless@sha256:deadbeef")
	t.Setenv("UZI_WORKER_DIND_ROOTLESS", "true")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.WorkerDockerNamespace != "uzi-workers-docker" || cfg.WorkerDinDImage != "docker:28-dind-rootless@sha256:deadbeef" {
		t.Fatalf("docker fields = %q / %q", cfg.WorkerDockerNamespace, cfg.WorkerDinDImage)
	}
	if !cfg.WorkerDinDRootless {
		t.Error("UZI_WORKER_DIND_ROOTLESS=true must set WorkerDinDRootless")
	}
}

// The DinD posture (PRD #89) is REQUIRED and unambiguous when the tier is on: it selects
// node-root (non-rootless) vs a userns-mapped uid (rootless), so a docker tier that
// leaves it unset refuses to boot rather than default a security posture.
func TestLoadDockerTierRequiresAnExplicitPosture(t *testing.T) {
	setDockerBaseEnv(t)
	t.Setenv("UZI_WORKER_DOCKER_NAMESPACE", "uzi-workers-docker")
	t.Setenv("UZI_WORKER_DIND_IMAGE", "docker:28-dind-rootless@sha256:deadbeef")
	// UZI_WORKER_DIND_ROOTLESS deliberately unset.
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "UZI_WORKER_DIND_ROOTLESS") {
		t.Fatalf("err = %v, want a refusal naming the required posture var", err)
	}
}

func TestLoadDockerTierRejectsANonBooleanPosture(t *testing.T) {
	setDockerBaseEnv(t)
	t.Setenv("UZI_WORKER_DOCKER_NAMESPACE", "uzi-workers-docker")
	t.Setenv("UZI_WORKER_DIND_IMAGE", "docker:28-dind@sha256:deadbeef")
	t.Setenv("UZI_WORKER_DIND_ROOTLESS", "maybe")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "not a boolean") {
		t.Fatalf("err = %v, want a refusal that the posture is not a boolean", err)
	}
}

// rootless:false is a valid, explicit choice (dev-cluster): it threads through to
// WorkerDinDRootless=false, which main.go inverts into RenderConfig.DinDNonRootless.
func TestLoadDockerNonRootlessPostureThreadsThrough(t *testing.T) {
	setDockerBaseEnv(t)
	t.Setenv("UZI_WORKER_DOCKER_NAMESPACE", "uzi-workers-docker")
	t.Setenv("UZI_WORKER_DIND_IMAGE", "docker:28-dind@sha256:deadbeef")
	t.Setenv("UZI_WORKER_DIND_ROOTLESS", "false")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.WorkerDinDRootless {
		t.Error("UZI_WORKER_DIND_ROOTLESS=false must clear WorkerDinDRootless")
	}
}

// With the tier off, the posture defaults to the SAFE value (rootless) and a stray
// UZI_WORKER_DIND_ROOTLESS with no tier is ignored (nothing for it to configure).
func TestLoadDockerTierOffDefaultsRootless(t *testing.T) {
	setDockerBaseEnv(t)
	t.Setenv("UZI_WORKER_DIND_ROOTLESS", "false") // stray value, no tier
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.WorkerDinDRootless {
		t.Error("with the docker tier off, WorkerDinDRootless must keep its safe default (true)")
	}
}

// The DinD resource overrides (PRD #89 0.8.1) are optional; when set they are read verbatim
// and (below) validated as k8s quantities. Requests + limits both. Empty leaves them empty
// (the render default).
func TestLoadDockerDinDResourceOverrides(t *testing.T) {
	setDockerBaseEnv(t)
	t.Setenv("UZI_WORKER_DOCKER_NAMESPACE", "uzi-workers-docker")
	t.Setenv("UZI_WORKER_DIND_IMAGE", "docker:28-dind@sha256:deadbeef")
	t.Setenv("UZI_WORKER_DIND_ROOTLESS", "false")
	t.Setenv("UZI_WORKER_DIND_REQUEST_CPU", "500m")
	t.Setenv("UZI_WORKER_DIND_REQUEST_MEMORY", "2Gi")
	t.Setenv("UZI_WORKER_DIND_LIMIT_CPU", "4")
	t.Setenv("UZI_WORKER_DIND_LIMIT_MEMORY", "6Gi")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.WorkerDinDRequestCPU != "500m" || cfg.WorkerDinDRequestMemory != "2Gi" {
		t.Fatalf("dind requests = %q / %q, want 500m / 2Gi", cfg.WorkerDinDRequestCPU, cfg.WorkerDinDRequestMemory)
	}
	if cfg.WorkerDinDLimitCPU != "4" || cfg.WorkerDinDLimitMemory != "6Gi" {
		t.Fatalf("dind limits = %q / %q, want 4 / 6Gi", cfg.WorkerDinDLimitCPU, cfg.WorkerDinDLimitMemory)
	}
}

// A typo'd override fails at BOOT (validated as a k8s Quantity), not at the far end when
// the apiserver rejects the rendered pod — for a REQUEST var as well as a limit var.
func TestLoadDockerDinDResourceRejectsABadQuantity(t *testing.T) {
	setDockerBaseEnv(t)
	t.Setenv("UZI_WORKER_DOCKER_NAMESPACE", "uzi-workers-docker")
	t.Setenv("UZI_WORKER_DIND_IMAGE", "docker:28-dind@sha256:deadbeef")
	t.Setenv("UZI_WORKER_DIND_ROOTLESS", "false")
	t.Setenv("UZI_WORKER_DIND_REQUEST_MEMORY", "2GB") // not a k8s quantity (want Gi/Mi/G/M)
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "UZI_WORKER_DIND_REQUEST_MEMORY") {
		t.Fatalf("err = %v, want a boot refusal naming the bad request var", err)
	}
}

// Unset overrides leave the fields empty (the render side supplies its default).
func TestLoadDockerDinDResourceDefaultsEmpty(t *testing.T) {
	setDockerBaseEnv(t)
	t.Setenv("UZI_WORKER_DOCKER_NAMESPACE", "uzi-workers-docker")
	t.Setenv("UZI_WORKER_DIND_IMAGE", "docker:28-dind@sha256:deadbeef")
	t.Setenv("UZI_WORKER_DIND_ROOTLESS", "false")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.WorkerDinDRequestCPU != "" || cfg.WorkerDinDRequestMemory != "" ||
		cfg.WorkerDinDLimitCPU != "" || cfg.WorkerDinDLimitMemory != "" {
		t.Errorf("unset dind resources must stay empty, got req %q/%q lim %q/%q",
			cfg.WorkerDinDRequestCPU, cfg.WorkerDinDRequestMemory, cfg.WorkerDinDLimitCPU, cfg.WorkerDinDLimitMemory)
	}
	// Issue #224 M-a: the data-root size is the same shape — unset means "let the
	// render side pick", never a zero quantity.
	if cfg.WorkerDinDDataSize != "" {
		t.Errorf("unset UZI_WORKER_DIND_DATA_SIZE must stay empty, got %q", cfg.WorkerDinDDataSize)
	}
}

// The DinD data-root PVC size (issue #224 M-a): read when set, and validated as a k8s
// Quantity at BOOT. The far end is worse than usual for this one — an unparseable size
// would surface as a PVC the apiserver rejects, i.e. a worker that provisions and never
// appears, with nothing in the controller's own logs naming the cause.
func TestLoadDockerDinDDataSize(t *testing.T) {
	setDockerBaseEnv(t)
	t.Setenv("UZI_WORKER_DOCKER_NAMESPACE", "uzi-workers-docker")
	t.Setenv("UZI_WORKER_DIND_IMAGE", "docker:28-dind@sha256:deadbeef")
	t.Setenv("UZI_WORKER_DIND_ROOTLESS", "false")
	// 10Gi, chosen to stay UNDER the chart's limitRange.maxPVCStorage (20Gi). This was
	// 40Gi, which loads perfectly well and is then rejected at admission — and that is
	// the point worth stating rather than silently fixing: this validation checks only
	// that the string PARSES, and it structurally CANNOT check the size, because the
	// controller does not know the LimitRange. The ceiling is enforced chart-side, in
	// worker-invariants.yaml. A fixture demonstrating an inadmissible value invites
	// someone to copy it.
	t.Setenv("UZI_WORKER_DIND_DATA_SIZE", "10Gi")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.WorkerDinDDataSize != "10Gi" {
		t.Fatalf("dind data size = %q, want 10Gi", cfg.WorkerDinDDataSize)
	}

	t.Setenv("UZI_WORKER_DIND_DATA_SIZE", "20GB!") // not a k8s quantity
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "UZI_WORKER_DIND_DATA_SIZE") {
		t.Fatalf("err = %v, want a boot refusal naming UZI_WORKER_DIND_DATA_SIZE", err)
	}
}

// The two tier PVC ceilings (issue #224). Same split and the same reason as the
// ephemeral requests: the RESTRICTED one is read outside validateDockerTier because
// every instance has that tier, and a var read only when docker is configured would be
// silently ignored on most of them — here that would mean booting a controller whose
// /nix claims are rejected at admission, which is the failure the check exists to stop.
func TestLoadWorkerMaxPVCStoragePerTier(t *testing.T) {
	t.Run("restricted tier is read with the docker tier OFF", func(t *testing.T) {
		setWorkerEnv(t)
		t.Setenv("UZI_CONTROLLER_TOKEN_FILE", writeToken(t, "tok"))
		t.Setenv("UZI_API_URL", "https://api.uzi.svc.cluster.local:8443")
		t.Setenv("UZI_WORKER_MAX_PVC_STORAGE", "20Gi")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.WorkerMaxPVCStorage != "20Gi" {
			t.Fatalf("restricted ceiling = %q, want 20Gi (read even with no docker tier configured)", cfg.WorkerMaxPVCStorage)
		}

		t.Setenv("UZI_WORKER_MAX_PVC_STORAGE", "20GB!")
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "UZI_WORKER_MAX_PVC_STORAGE") {
			t.Fatalf("err = %v, want a boot refusal naming UZI_WORKER_MAX_PVC_STORAGE", err)
		}
	})

	t.Run("docker tier", func(t *testing.T) {
		setDockerBaseEnv(t)
		t.Setenv("UZI_WORKER_DOCKER_NAMESPACE", "uzi-workers-docker")
		t.Setenv("UZI_WORKER_DIND_IMAGE", "docker:28-dind@sha256:deadbeef")
		t.Setenv("UZI_WORKER_DIND_ROOTLESS", "false")
		t.Setenv("UZI_WORKER_DOCKER_MAX_PVC_STORAGE", "20Gi")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.WorkerDockerMaxPVCStorage != "20Gi" {
			t.Fatalf("docker ceiling = %q, want 20Gi", cfg.WorkerDockerMaxPVCStorage)
		}
		// Unset stays EMPTY rather than defaulting. A guessed ceiling that is too low
		// refuses to boot a healthy fleet, which is worse than not checking that tier.
		if cfg.WorkerMaxPVCStorage != "" {
			t.Errorf("unset UZI_WORKER_MAX_PVC_STORAGE must stay empty, got %q", cfg.WorkerMaxPVCStorage)
		}

		t.Setenv("UZI_WORKER_DOCKER_MAX_PVC_STORAGE", "20GB!")
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "UZI_WORKER_DOCKER_MAX_PVC_STORAGE") {
			t.Fatalf("err = %v, want a boot refusal naming UZI_WORKER_DOCKER_MAX_PVC_STORAGE", err)
		}
	})
}

// The worker's requests.ephemeral-storage overrides (issue #224 M-b), per tier.
//
// The PLAIN one is read OUTSIDE validateDockerTier and this test's first half is what
// pins that: every worker declares the resource, so a var validated only when the
// docker tier is on would be silently ignored on the instances that do not run one —
// which is most of them, and the failure would be a worker evicted exactly as before
// with a values.yaml that says otherwise.
func TestLoadWorkerEphemeralRequestsPerTier(t *testing.T) {
	t.Run("plain tier is read with the docker tier OFF", func(t *testing.T) {
		setWorkerEnv(t)
		t.Setenv("UZI_CONTROLLER_TOKEN_FILE", writeToken(t, "tok"))
		t.Setenv("UZI_API_URL", "https://api.uzi.svc.cluster.local:8443")
		t.Setenv("UZI_WORKER_EPHEMERAL_REQUEST", "1Gi")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.WorkerEphemeralRequest != "1Gi" {
			t.Fatalf("plain ephemeral request = %q, want 1Gi (read even with no docker tier configured)", cfg.WorkerEphemeralRequest)
		}

		t.Setenv("UZI_WORKER_EPHEMERAL_REQUEST", "1GB!")
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "UZI_WORKER_EPHEMERAL_REQUEST") {
			t.Fatalf("err = %v, want a boot refusal naming UZI_WORKER_EPHEMERAL_REQUEST", err)
		}
	})

	t.Run("docker tier", func(t *testing.T) {
		setDockerBaseEnv(t)
		t.Setenv("UZI_WORKER_DOCKER_NAMESPACE", "uzi-workers-docker")
		t.Setenv("UZI_WORKER_DIND_IMAGE", "docker:28-dind@sha256:deadbeef")
		t.Setenv("UZI_WORKER_DIND_ROOTLESS", "false")
		t.Setenv("UZI_WORKER_DOCKER_EPHEMERAL_REQUEST", "8Gi")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.WorkerDockerEphemeralRequest != "8Gi" {
			t.Fatalf("docker ephemeral request = %q, want 8Gi", cfg.WorkerDockerEphemeralRequest)
		}
		// Unset stays empty so the render side supplies its own default, never a zero
		// quantity — which would re-create the very bug (a declared request of 0 ranks
		// identically to declaring nothing).
		if cfg.WorkerEphemeralRequest != "" {
			t.Errorf("unset UZI_WORKER_EPHEMERAL_REQUEST must stay empty, got %q", cfg.WorkerEphemeralRequest)
		}

		t.Setenv("UZI_WORKER_DOCKER_EPHEMERAL_REQUEST", "8GB!")
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "UZI_WORKER_DOCKER_EPHEMERAL_REQUEST") {
			t.Fatalf("err = %v, want a boot refusal naming UZI_WORKER_DOCKER_EPHEMERAL_REQUEST", err)
		}
	})
}
