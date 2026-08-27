package config

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/secretbox"
)

// hostingBaseEnv sets a syntactically valid minimum environment so each test can
// vary only the WORKER_HOSTING_* vars under test.
func hostingBaseEnv(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://uzi:pw@db:5432/uzi?sslmode=disable")
	// Low-entropy but valid (non-placeholder, long-enough) signing key; a
	// high-entropy literal would trip the secret scanner on a fresh add.
	t.Setenv("JWT_SECRET", "unit-test-jwt-signing-key-not-a-real-secret")
	varied := make([]byte, secretbox.KeySize)
	for i := range varied {
		varied[i] = byte(i + 1)
	}
	t.Setenv("UZI_SECRET_KEY", base64.StdEncoding.EncodeToString(varied))
}

func controllerTokenHashHex(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// The compose default: hosting is off and fully dormant. This is the zero-behavior-
// change criterion at the config layer.
func TestWorkerHostingIsOffByDefault(t *testing.T) {
	hostingBaseEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if cfg.WorkerHostingEnabled {
		t.Fatal("WorkerHostingEnabled = true with nothing set; hosting must default off")
	}
	if cfg.ControllerTokenSHA256 != nil {
		t.Fatal("ControllerTokenSHA256 is set with hosting off")
	}
}

func TestWorkerHostingEnabledDecodesTheTokenHash(t *testing.T) {
	hostingBaseEnv(t)
	t.Setenv("WORKER_HOSTING_ENABLED", "true")
	t.Setenv("WORKER_HOSTING_CONTROLLER_TOKEN_SHA256", controllerTokenHashHex("uzc_the-controller-credential"))

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if !cfg.WorkerHostingEnabled {
		t.Fatal("WorkerHostingEnabled = false")
	}
	want := sha256.Sum256([]byte("uzc_the-controller-credential"))
	if string(cfg.ControllerTokenSHA256) != string(want[:]) {
		t.Fatal("ControllerTokenSHA256 did not decode to the token's hash")
	}
}

// Hosting on without a credential would mount a controller endpoint no controller
// could ever authenticate to. Loud, not half-configured.
func TestWorkerHostingEnabledRequiresATokenHash(t *testing.T) {
	hostingBaseEnv(t)
	t.Setenv("WORKER_HOSTING_ENABLED", "true")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() = nil error with hosting on and no token hash")
	}
	if !strings.Contains(err.Error(), "WORKER_HOSTING_CONTROLLER_TOKEN_SHA256") {
		t.Fatalf("err = %v, want it to name the missing var", err)
	}
}

// A credential set while hosting is off means the operator provisioned something
// the api silently ignores while their controller 404s forever. Same all-or-nothing
// stance loadOIDC takes on its gating vars.
func TestTokenHashWithoutHostingRefusesToBoot(t *testing.T) {
	hostingBaseEnv(t)
	t.Setenv("WORKER_HOSTING_CONTROLLER_TOKEN_SHA256", controllerTokenHashHex("uzc_orphaned"))

	if _, err := Load(); err == nil {
		t.Fatal("Load() = nil error with a token hash but hosting off")
	}
}

func TestWorkerHostingRejectsAMalformedTokenHash(t *testing.T) {
	cases := map[string]string{
		"not hex":     "zzzz-not-hex-at-all",
		"too short":   hex.EncodeToString([]byte("short")),
		"a plaintext": "uzc_someone-pasted-the-token-instead-of-its-hash",
	}
	for name, val := range cases {
		t.Run(name, func(t *testing.T) {
			hostingBaseEnv(t)
			t.Setenv("WORKER_HOSTING_ENABLED", "true")
			t.Setenv("WORKER_HOSTING_CONTROLLER_TOKEN_SHA256", val)
			if _, err := Load(); err == nil {
				t.Fatalf("Load() = nil error for a %s token hash", name)
			}
		})
	}
}

// A hash is not a secret, but it is adjacent to one — errors name the variable,
// never the value. Same discipline loadSeedSlack keeps.
func TestWorkerHostingErrorsDoNotEchoTheValue(t *testing.T) {
	hostingBaseEnv(t)
	t.Setenv("WORKER_HOSTING_ENABLED", "true")
	const val = "uzc_a-value-that-must-not-appear-in-any-error"
	t.Setenv("WORKER_HOSTING_CONTROLLER_TOKEN_SHA256", val)

	_, err := Load()
	if err == nil {
		t.Fatal("want an error")
	}
	if strings.Contains(err.Error(), val) {
		t.Fatalf("error echoed the configured value: %v", err)
	}
}

// The flag gates a privileged protocol, so a typo aborts boot rather than silently
// taking the default — the stance every other kill-switch here takes.
func TestWorkerHostingEnabledRejectsAMalformedBool(t *testing.T) {
	hostingBaseEnv(t)
	t.Setenv("WORKER_HOSTING_ENABLED", "yes-please")

	if _, err := Load(); err == nil {
		t.Fatal("Load() = nil error for a malformed WORKER_HOSTING_ENABLED")
	}
}

// A credential whose plaintext is a well-known placeholder must not boot. The api
// holds only the hash so it cannot judge entropy — but the hash of a value copied
// from a README or left in a chart default is recognisable, and that is the
// failure mode that actually ships.
func TestWorkerHostingRefusesAPlaceholderCredential(t *testing.T) {
	for _, plaintext := range []string{"changeme", "change-me", "secret", "password", "uzi-controller-token"} {
		t.Run(plaintext, func(t *testing.T) {
			hostingBaseEnv(t)
			t.Setenv("WORKER_HOSTING_ENABLED", "true")
			t.Setenv("WORKER_HOSTING_CONTROLLER_TOKEN_SHA256", controllerTokenHashHex(plaintext))

			_, err := Load()
			if err == nil {
				t.Fatalf("Load() = nil error for the hash of placeholder %q", plaintext)
			}
			if !strings.Contains(err.Error(), "placeholder") {
				t.Fatalf("err = %v, want it to name the problem", err)
			}
		})
	}
}

// The at-rest bound is read REGARDLESS of the flag: a stack that provisioned
// hosted workers and then disabled hosting is exactly the one whose sealed tokens
// must still be swept, so the TTL outlives the feature gate.
func TestHostedTokenTTLIsLoadedEvenWithHostingDisabled(t *testing.T) {
	hostingBaseEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if cfg.WorkerHostingEnabled {
		t.Fatal("hosting should be off")
	}
	if cfg.HostedTokenTTL != time.Hour {
		t.Fatalf("HostedTokenTTL = %v with hosting off, want the 1h default (the sweep runs regardless)", cfg.HostedTokenTTL)
	}
}

func TestHostedTokenTTLIsConfigurableAndDisablable(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want time.Duration
	}{
		{"15m", 15 * time.Minute},
		{"0", 0},               // disabled, with a boot warning
		{"garbage", time.Hour}, // a tuning knob, not a security control: fall back
		{"-5m", time.Hour},     // negative is meaningless: fall back
	} {
		t.Run(tc.raw, func(t *testing.T) {
			hostingBaseEnv(t)
			t.Setenv("WORKER_HOSTING_PENDING_TOKEN_TTL", tc.raw)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load(): %v", err)
			}
			if cfg.HostedTokenTTL != tc.want {
				t.Fatalf("TTL for %q = %v, want %v", tc.raw, cfg.HostedTokenTTL, tc.want)
			}
		})
	}
}
