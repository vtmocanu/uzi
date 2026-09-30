package config

import (
	"crypto/sha256"
	"strings"
	"testing"
)

// PRD #1906 M3: UZI_FETCHER_TOKEN_SHA256 gates the fetcher control routes. Unset is off
// (nil), a valid hash loads, and a malformed or placeholder value refuses boot.
func TestFetcherTokenUnsetIsNil(t *testing.T) {
	hostingBaseEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if cfg.FetcherTokenSHA256 != nil {
		t.Fatal("FetcherTokenSHA256 is set with UZI_FETCHER_TOKEN_SHA256 unset")
	}
}

func TestFetcherTokenLoadsHash(t *testing.T) {
	hostingBaseEnv(t)
	t.Setenv("UZI_FETCHER_TOKEN_SHA256", "  "+strings.ToUpper(controllerTokenHashHex("the-fetcher-service-credential"))+"\n")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	want := sha256.Sum256([]byte("the-fetcher-service-credential"))
	if string(cfg.FetcherTokenSHA256) != string(want[:]) {
		t.Fatal("FetcherTokenSHA256 did not decode to the token's hash")
	}
}

func TestFetcherTokenMalformedRefusesBoot(t *testing.T) {
	for name, val := range map[string]string{ //nolint:gosec // G101: malformed-value fixtures for the validator, not credentials.
		"not hex":     "zz" + strings.Repeat("0", 62),
		"short":       "abcd",
		"the token":   "the-fetcher-service-credential",
		"placeholder": controllerTokenHashHex("changeme"),
	} {
		t.Run(name, func(t *testing.T) {
			hostingBaseEnv(t)
			t.Setenv("UZI_FETCHER_TOKEN_SHA256", val)
			_, err := Load()
			if err == nil {
				t.Fatalf("Load() accepted UZI_FETCHER_TOKEN_SHA256=%q", val)
			}
			if !strings.Contains(err.Error(), "UZI_FETCHER_TOKEN_SHA256") {
				t.Fatalf("error does not name the variable: %v", err)
			}
			if strings.Contains(err.Error(), val) {
				t.Fatalf("error echoes the value: %v", err)
			}
		})
	}
}

// The fetcher credential must never be the controller credential: RequireController and
// RequireFetcher each compare only against their own hash, so a shared hash would let a
// compromised fetcher (the lane's internet-facing process) pass RequireController and
// drive the hosted-worker protocol. Boot refuses the pair, and names both variables.
func TestFetcherTokenEqualToControllerTokenRefusesBoot(t *testing.T) {
	hostingBaseEnv(t)
	shared := controllerTokenHashHex("one-credential-for-two-services")
	t.Setenv("WORKER_HOSTING_ENABLED", "true")
	t.Setenv("WORKER_HOSTING_CONTROLLER_TOKEN_SHA256", shared)
	// Case and whitespace differ from the controller's value: the compare is on the
	// decoded bytes, not the strings.
	t.Setenv("UZI_FETCHER_TOKEN_SHA256", " "+strings.ToUpper(shared)+" ")
	_, err := Load()
	if err == nil {
		t.Fatal("Load() accepted a fetcher token hash equal to the controller token hash")
	}
	for _, name := range []string{"UZI_FETCHER_TOKEN_SHA256", "WORKER_HOSTING_CONTROLLER_TOKEN_SHA256"} {
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("error does not name %s: %v", name, err)
		}
	}
	if strings.Contains(strings.ToLower(err.Error()), shared) {
		t.Fatalf("error echoes the hash: %v", err)
	}
}

// Distinct credentials both load: the guard compares, it does not forbid coexistence.
func TestFetcherTokenDistinctFromControllerTokenLoads(t *testing.T) {
	hostingBaseEnv(t)
	t.Setenv("WORKER_HOSTING_ENABLED", "true")
	t.Setenv("WORKER_HOSTING_CONTROLLER_TOKEN_SHA256", controllerTokenHashHex("the-controller-service-credential"))
	t.Setenv("UZI_FETCHER_TOKEN_SHA256", controllerTokenHashHex("the-fetcher-service-credential"))
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if cfg.FetcherTokenSHA256 == nil || cfg.ControllerTokenSHA256 == nil {
		t.Fatal("both hashes should load")
	}
}
