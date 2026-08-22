package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
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

// D4: `auth token --context admin` stores the token under `admin` (creating the
// context — no D9 error), leaves the default context untouched, and the
// confirmation names admin.
func TestAuthTokenNamedContext(t *testing.T) {
	env := storeEnv(t, "uza_adminsecret999\n")
	out, _, code := runCLI(t, env, "auth", "token", "--context", "admin")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	got, err := env.Store.Resolve("admin")
	if err != nil {
		t.Fatal(err)
	}
	if got.Token != "uza_adminsecret999" {
		t.Errorf("admin token = %q, want uza_adminsecret999", got.Token)
	}
	// The default context is untouched.
	if d, _ := env.Store.Resolve("default"); d.Token != "" {
		t.Errorf("default context was written: %q", d.Token)
	}
	if !strings.Contains(out, "admin") {
		t.Errorf("confirmation does not name the target context:\n%s", out)
	}
	if strings.Contains(out, "uza_adminsecret999") {
		t.Errorf("confirmation leaked the full token:\n%s", out)
	}
}

// A control/bidi context name is rejected at the write path (it would otherwise
// land raw in credentials.toml and later print in `context list --json`).
func TestAuthTokenBadContextName(t *testing.T) {
	env := storeEnv(t, "uzc_secrettoken123\n")
	_, _, code := runCLI(t, env, "auth", "token", "--context", "ad\x07min")
	if code != uzicli.ExitUsage {
		t.Fatalf("exit = %d, want %d (usage)", code, uzicli.ExitUsage)
	}
	// Nothing was written under any context.
	creds, err := env.Store.LoadCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if len(creds.Contexts) != 0 {
		t.Errorf("a rejected name still wrote credentials: %+v", creds.Contexts)
	}
}

// D4: `auth status` reports the ACTIVE context — "default" when nothing is set,
// and the named one under -c.
func TestAuthStatusActiveContext(t *testing.T) {
	env := storeEnv(t, "")
	// Seed two contexts.
	if err := env.Store.SaveCredentials(&uzicli.Credentials{Contexts: map[string]uzicli.Credential{
		"default": {Token: "uzc_defsecret"},
		"admin":   {Token: "uza_admsecret"},
	}}); err != nil {
		t.Fatal(err)
	}

	// Default active context.
	out, _, code := runCLI(t, env, "auth", "status", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("status exit = %d", code)
	}
	var got struct {
		Context  string `json:"context"`
		HasToken bool   `json:"has_token"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("json: %v\n%s", err, out)
	}
	if got.Context != "default" {
		t.Errorf("active context = %q, want default", got.Context)
	}

	// Under -c admin the active context is admin.
	out, _, code = runCLI(t, env, "auth", "status", "--json", "-c", "admin")
	if code != uzicli.ExitOK {
		t.Fatalf("status -c admin exit = %d", code)
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("json: %v\n%s", err, out)
	}
	if got.Context != "admin" {
		t.Errorf("active context = %q, want admin", got.Context)
	}
	if !got.HasToken {
		t.Errorf("admin status has_token=false, want true")
	}
}

// `auth status --all` lists every stored context, marks the current one, and
// never prints a full token value.
func TestAuthStatusAll(t *testing.T) {
	cfg := &uzicli.Config{
		Current: "admin",
		Contexts: map[string]uzicli.Context{
			"default": {URL: "https://default.example"},
			// admin has no URL of its own.
		},
	}
	creds := &uzicli.Credentials{Contexts: map[string]uzicli.Credential{
		"default": {Token: "uzc_defaultsecret"},
		"admin":   {Token: "uza_adminsecret"},
	}}
	env := seedStore(t, cfg, creds)

	out, _, code := runCLI(t, env, "auth", "status", "--all", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	var rows []struct {
		Context     string `json:"context"`
		URL         string `json:"url"`
		HasToken    bool   `json:"has_token"`
		TokenPrefix string `json:"token_prefix"`
		Current     bool   `json:"current"`
	}
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("json: %v\n%s", err, out)
	}
	byName := map[string]int{}
	for i, r := range rows {
		byName[r.Context] = i
	}
	di, ok := byName["default"]
	if !ok {
		t.Fatalf("no default row:\n%s", out)
	}
	ai, ok := byName["admin"]
	if !ok {
		t.Fatalf("no admin row:\n%s", out)
	}
	// admin is the sticky current; default is not.
	if !rows[ai].Current || rows[di].Current {
		t.Errorf("current marker wrong: admin=%v default=%v", rows[ai].Current, rows[di].Current)
	}
	// admin's own URL is blank (inherits default per D3); default keeps its URL.
	if rows[ai].URL != "" {
		t.Errorf("admin url = %q, want blank", rows[ai].URL)
	}
	if rows[di].URL != "https://default.example" {
		t.Errorf("default url = %q", rows[di].URL)
	}
	// No full token value anywhere in the output.
	if strings.Contains(out, "uzc_defaultsecret") || strings.Contains(out, "uza_adminsecret") {
		t.Errorf("--all leaked a token value:\n%s", out)
	}
	if !rows[ai].HasToken || rows[ai].TokenPrefix == "" {
		t.Errorf("admin row = %+v, want has_token + a prefix", rows[ai])
	}
}

// File-safety: a named-context write lands 0600 on credentials.toml, and is
// refused when the file has been made group-readable.
func TestAuthTokenNamedWritePerms(t *testing.T) {
	env := storeEnv(t, "uza_adminsecret999\n")
	if _, _, code := runCLI(t, env, "auth", "token", "-c", "admin"); code != uzicli.ExitOK {
		t.Fatalf("named write exit = %d", code)
	}
	credsPath := filepath.Join(env.Store.Dir(), "credentials.toml")
	fi, err := os.Stat(credsPath)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("credentials.toml perm after a named write = %04o, want 0600", perm)
	}

	// Make it group-readable; the next write must refuse rather than read the file.
	if err := os.Chmod(credsPath, 0o640); err != nil {
		t.Fatal(err)
	}
	env2 := storeEnv(t, "uza_other\n")
	env2.Store = env.Store
	if _, _, code := runCLI(t, env2, "auth", "token", "-c", "admin"); code == uzicli.ExitOK {
		t.Fatal("a write into a group-readable credentials file must be refused")
	}
}
