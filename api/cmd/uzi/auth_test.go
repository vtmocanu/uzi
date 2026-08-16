package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// storeEnv builds an Env backed by a real (temp-dir) Store and a fake client, so
// the credential-plumbing verbs can be exercised end to end without touching the
// user's ~/.config/uzi.
func storeEnv(t *testing.T, stdin string) Env {
	t.Helper()
	return Env{
		Stdout:    &bytes.Buffer{},
		Stderr:    &bytes.Buffer{},
		Stdin:     strings.NewReader(stdin),
		StdoutTTY: false,
		StdinTTY:  false,
		NewClient: func(uzicli.Settings) uzicli.Client { return &uzicli.FakeClient{} },
		Store:     uzicli.NewStore(t.TempDir()),
	}
}

// `uzi auth token` reads the token from stdin (never argv) and persists it.
func TestAuthTokenStdin(t *testing.T) {
	env := storeEnv(t, "uzc_secrettoken123\n")
	_, _, code := runCLI(t, env, "auth", "token")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	creds, err := env.Store.LoadCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if got := creds.Contexts["default"].Token; got != "uzc_secrettoken123" {
		t.Errorf("stored token = %q, want uzc_secrettoken123", got)
	}
}

// An empty stdin is a usage error (2), not a silent no-op.
func TestAuthTokenEmptyStdin(t *testing.T) {
	env := storeEnv(t, "\n")
	_, _, code := runCLI(t, env, "auth", "token")
	if code != uzicli.ExitUsage {
		t.Fatalf("exit = %d, want %d (usage)", code, uzicli.ExitUsage)
	}
}

// The confirmation line never echoes the full token.
func TestAuthTokenMasksOutput(t *testing.T) {
	env := storeEnv(t, "uzc_verysecretvalue\n")
	out, _, code := runCLI(t, env, "auth", "token")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if strings.Contains(out, "uzc_verysecretvalue") {
		t.Errorf("confirmation leaked the full token:\n%s", out)
	}
}

// `uzi auth status` reports a stored credential without printing its value.
func TestAuthStatus(t *testing.T) {
	env := storeEnv(t, "uzc_secrettoken123\n")
	if _, _, code := runCLI(t, env, "auth", "token"); code != uzicli.ExitOK {
		t.Fatalf("seed exit = %d", code)
	}
	out, _, code := runCLI(t, env, "auth", "status")
	if code != uzicli.ExitOK {
		t.Fatalf("status exit = %d, want 0", code)
	}
	if !strings.Contains(out, "stored") {
		t.Errorf("status did not report a stored token:\n%s", out)
	}
	if strings.Contains(out, "uzc_secrettoken123") {
		t.Errorf("status leaked the full token:\n%s", out)
	}
}
