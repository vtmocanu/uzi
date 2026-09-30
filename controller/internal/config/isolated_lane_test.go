package config

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// --- isolated research lane (PRD #1906 M5) -----------------------------------

// setLaneEnv sets all three lane knobs to a valid configuration on top of the base env
// Load needs, so each refusal test below can break exactly one thing.
func setLaneEnv(t *testing.T) []byte {
	t.Helper()
	setDockerBaseEnv(t)
	pem := caPEM(t)
	t.Setenv("UZI_WORKER_ISOLATED_NAMESPACE", "uzi-workers-isolated")
	t.Setenv("UZI_WORKER_ISOLATED_FETCHER_URL", "https://uzi-fetcher.uzi.svc.cluster.local:8443")
	t.Setenv("UZI_WORKER_ISOLATED_FETCHER_CA_FILE", writeCA(t, pem))
	return pem
}

func TestLoadIsolatedLaneOffLeavesTheFieldsEmpty(t *testing.T) {
	setDockerBaseEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.WorkerIsolatedNamespace != "" || cfg.WorkerIsolatedFetcherURL != "" || cfg.WorkerIsolatedFetcherCAPEM != nil {
		t.Fatalf("lane fields = %q / %q / %d bytes, want empty when the lane is off",
			cfg.WorkerIsolatedNamespace, cfg.WorkerIsolatedFetcherURL, len(cfg.WorkerIsolatedFetcherCAPEM))
	}
}

func TestLoadIsolatedLaneOnPopulatesAllThree(t *testing.T) {
	pem := setLaneEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.WorkerIsolatedNamespace != "uzi-workers-isolated" {
		t.Errorf("namespace = %q", cfg.WorkerIsolatedNamespace)
	}
	if cfg.WorkerIsolatedFetcherURL != "https://uzi-fetcher.uzi.svc.cluster.local:8443" {
		t.Errorf("fetcher url = %q", cfg.WorkerIsolatedFetcherURL)
	}
	if !bytes.Equal(cfg.WorkerIsolatedFetcherCAPEM, pem) {
		t.Error("the fetcher CA PEM must be kept verbatim for relay into lane workers' Secrets")
	}
}

// All or none: a lane missing any one knob is refused at boot, naming the set.
func TestLoadIsolatedLaneIsAllOrNone(t *testing.T) {
	for _, missing := range []string{
		"UZI_WORKER_ISOLATED_NAMESPACE",
		"UZI_WORKER_ISOLATED_FETCHER_URL",
		"UZI_WORKER_ISOLATED_FETCHER_CA_FILE",
	} {
		t.Run(missing, func(t *testing.T) {
			setLaneEnv(t)
			t.Setenv(missing, "")
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), "must be set together") {
				t.Fatalf("err = %v, want an all-or-none refusal", err)
			}
		})
	}
}

func TestLoadIsolatedFetcherURLMustBePlainHTTPS(t *testing.T) {
	for _, raw := range []string{
		"http://uzi-fetcher.uzi.svc.cluster.local:8080",
		"uzi-fetcher.uzi.svc.cluster.local:8443",
		"https://",
		"https://user:pw@uzi-fetcher.uzi.svc:8443",
		"https://uzi-fetcher.uzi.svc:8443/?x=1",
		"https://uzi-fetcher.uzi.svc:8443/#frag",
		"://bad",
	} {
		t.Run(raw, func(t *testing.T) {
			setLaneEnv(t)
			t.Setenv("UZI_WORKER_ISOLATED_FETCHER_URL", raw)
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), "UZI_WORKER_ISOLATED_FETCHER_URL") {
				t.Fatalf("err = %v, want a refusal naming UZI_WORKER_ISOLATED_FETCHER_URL", err)
			}
		})
	}
}

func TestLoadIsolatedFetcherCAMustHoldAPEMCertificate(t *testing.T) {
	t.Run("unreadable", func(t *testing.T) {
		setLaneEnv(t)
		t.Setenv("UZI_WORKER_ISOLATED_FETCHER_CA_FILE", "/nonexistent/fetcher-ca.crt")
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "UZI_WORKER_ISOLATED_FETCHER_CA_FILE") {
			t.Fatalf("err = %v, want a read refusal", err)
		}
	})
	t.Run("not PEM", func(t *testing.T) {
		setLaneEnv(t)
		t.Setenv("UZI_WORKER_ISOLATED_FETCHER_CA_FILE", writeCA(t, []byte("not a certificate")))
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "no PEM certificate") {
			t.Fatalf("err = %v, want a no-PEM refusal", err)
		}
	})
}

// The lane namespace must differ from the restricted worker namespace: sharing it would
// put lane workers under the ordinary tier's wider egress policy.
func TestLoadIsolatedNamespaceMustDifferFromTheWorkerNamespace(t *testing.T) {
	setLaneEnv(t)
	t.Setenv("UZI_WORKER_ISOLATED_NAMESPACE", "uzi-workers") // == UZI_WORKER_NAMESPACE
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "must differ from UZI_WORKER_NAMESPACE") {
		t.Fatalf("err = %v, want a refusal that the lane namespace must differ from the worker namespace", err)
	}
}

// ...and from the privileged docker tier's namespace.
func TestLoadIsolatedNamespaceMustDifferFromTheDockerNamespace(t *testing.T) {
	setLaneEnv(t)
	t.Setenv("UZI_WORKER_DOCKER_NAMESPACE", "uzi-workers-docker")
	t.Setenv("UZI_WORKER_DIND_IMAGE", "docker:28-dind-rootless@sha256:deadbeef")
	t.Setenv("UZI_WORKER_DIND_ROOTLESS", "true")
	t.Setenv("UZI_WORKER_ISOLATED_NAMESPACE", "uzi-workers-docker")
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "must differ from UZI_WORKER_DOCKER_NAMESPACE") {
		t.Fatalf("err = %v, want a refusal that the lane namespace must differ from the docker namespace", err)
	}
}

// ...and from the controller's own namespace, which main reads in-cluster at boot. With
// the lane configured, an unreadable or empty own namespace refuses to boot: the
// separation cannot be verified.
func TestCheckIsolatedLaneOwnNamespace(t *testing.T) {
	lane := Config{WorkerIsolatedNamespace: "uzi-workers-isolated"}
	unreadable := func() (string, error) { return "", errors.New("open namespace file: no such file or directory") }
	ns := func(v string) func() (string, error) { return func() (string, error) { return v, nil } }

	t.Run("lane unset, namespace unreadable: ok and never read", func(t *testing.T) {
		called := false
		read := func() (string, error) { called = true; return unreadable() }
		if err := CheckIsolatedLaneOwnNamespace(Config{}, read); err != nil {
			t.Fatalf("a lane that is off must pass: %v", err)
		}
		if called {
			t.Fatal("a lane that is off must not read the controller's own namespace")
		}
	})
	t.Run("lane set, namespace unreadable: refused", func(t *testing.T) {
		err := CheckIsolatedLaneOwnNamespace(lane, unreadable)
		if err == nil || !strings.Contains(err.Error(), "controller's own namespace") ||
			!strings.Contains(err.Error(), "no such file") {
			t.Fatalf("err = %v, want a refusal naming the unreadable own namespace and the cause", err)
		}
	})
	t.Run("lane set, namespace empty: refused", func(t *testing.T) {
		if err := CheckIsolatedLaneOwnNamespace(lane, ns(" \n")); err == nil ||
			!strings.Contains(err.Error(), "controller's own namespace") {
			t.Fatalf("err = %v, want a refusal for an empty own namespace", err)
		}
	})
	t.Run("lane set, equal: refused", func(t *testing.T) {
		if err := CheckIsolatedLaneOwnNamespace(lane, ns("uzi-workers-isolated\n")); err == nil ||
			!strings.Contains(err.Error(), "must differ from the controller's own namespace") {
			t.Fatalf("err = %v, want a refusal naming the controller's own namespace", err)
		}
	})
	t.Run("lane set, distinct: ok", func(t *testing.T) {
		if err := CheckIsolatedLaneOwnNamespace(lane, ns("uzi\n")); err != nil {
			t.Fatalf("a distinct controller namespace must pass: %v", err)
		}
	})
}
