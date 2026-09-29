package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/producttoken"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// PRD #1907 M6: a product token (uzp_) handed to the CLI, whether through $UZI_TOKEN or
// a stored context credential, fails fast with an actionable message before any client
// is built, and the message never echoes the token value.

// productTokenFixture assembles a product-token-shaped value at runtime (never a
// source literal).
func productTokenFixture() string {
	return producttoken.Prefix + strings.Repeat("a", 43)
}

// runWithToken runs the CLI with $UZI_TOKEN set to envToken (runCLI would clear it),
// recording whether any client was built.
func runWithToken(t *testing.T, env Env, envToken string, args ...string) (stdout, stderr string, code int, built bool) {
	t.Helper()
	t.Setenv("UZI_URL", "https://uzi.example")
	t.Setenv("UZI_TOKEN", envToken)
	t.Setenv("UZI_CONTEXT", "")
	env.NewClient = func(uzicli.Settings) uzicli.Client {
		built = true
		return &uzicli.FakeClient{}
	}
	var out, errb bytes.Buffer
	env.Stdout = &out
	env.Stderr = &errb
	code = Main(env, args)
	return out.String(), errb.String(), code, built
}

func assertProductTokenRefused(t *testing.T, tok, stdout, stderr string, code int, built bool) {
	t.Helper()
	if code != uzicli.ExitAuth {
		t.Errorf("exit = %d, want %d (auth)", code, uzicli.ExitAuth)
	}
	if built {
		t.Error("a client was built for a product token; the refusal must precede any request")
	}
	if !strings.Contains(stderr, msgProductTokenCLI) {
		t.Errorf("stderr = %q, want the product-token message", stderr)
	}
	if strings.Contains(stderr, tok) || strings.Contains(stdout, tok) {
		t.Error("the token value was echoed")
	}
}

func TestProductTokenRejectedFromEnv(t *testing.T) {
	tok := productTokenFixture()
	out, errOut, code, built := runWithToken(t, fakeEnv(nil), tok, "whoami")
	assertProductTokenRefused(t, tok, out, errOut, code, built)
}

func TestProductTokenRejectedFromStoredContext(t *testing.T) {
	tok := productTokenFixture()
	env := seedStore(t,
		&uzicli.Config{Contexts: map[string]uzicli.Context{"default": {URL: "https://uzi.example"}}},
		&uzicli.Credentials{Contexts: map[string]uzicli.Credential{"default": {Token: tok}}},
	)
	out, errOut, code, built := runWithToken(t, env, "", "whoami")
	assertProductTokenRefused(t, tok, out, errOut, code, built)
}

// A CLI token in $UZI_TOKEN outranks a stored product token, as for any stored token:
// the check applies to the credential actually used, and a CLI token is not refused.
func TestCLITokenInEnvOverridesStoredProductToken(t *testing.T) {
	env := seedStore(t,
		&uzicli.Config{Contexts: map[string]uzicli.Context{"default": {URL: "https://uzi.example"}}},
		&uzicli.Credentials{Contexts: map[string]uzicli.Credential{"default": {Token: productTokenFixture()}}},
	)
	_, errOut, code, built := runWithToken(t, env, "uzc_"+strings.Repeat("b", 43), "whoami")
	if code != uzicli.ExitOK || !built {
		t.Fatalf("exit = %d built = %v stderr = %q, want a CLI token to proceed", code, built, errOut)
	}
}
