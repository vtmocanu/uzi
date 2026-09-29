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
