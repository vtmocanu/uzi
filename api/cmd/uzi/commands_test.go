package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"gitlab.example.com/vtmocanu/uzi/api/internal/uzicli"
)

// fakeEnv builds an Env backed by the given fake client, with no config store
// (so no file IO) and a non-TTY stdout (so no colour).
func fakeEnv(fc *uzicli.FakeClient) Env {
	return Env{
		Stdout:    &bytes.Buffer{},
		Stderr:    &bytes.Buffer{},
		Stdin:     strings.NewReader(""),
		StdoutTTY: false,
		NewClient: func(uzicli.Settings) uzicli.Client { return fc },
		Store:     nil,
	}
}

// runCLI runs the CLI with fresh output buffers and returns stdout, stderr and
// the process exit code.
func runCLI(t *testing.T, env Env, args ...string) (string, string, int) {
	t.Helper()
	// Keep resolveSettings deterministic regardless of the dev shell.
	t.Setenv("UZI_URL", "")
	t.Setenv("UZI_TOKEN", "")
	var out, errb bytes.Buffer
	env.Stdout = &out
	env.Stderr = &errb
	code := Main(env, args)
	return out.String(), errb.String(), code
}

func TestCommandTree(t *testing.T) {
	root := newRootCmd(fakeEnv(&uzicli.FakeClient{}))

	topWant := []string{
		"login", "logout", "auth", "whoami", "run",
		"worker", "repo", "admin", "skill", "version",
	}
	for _, name := range topWant {
		if findCmd(root, name) == nil {
			t.Errorf("missing top-level command %q", name)
		}
	}

	subWant := map[string][]string{
		"run":    {"list", "get", "logs", "review", "create", "approve", "reject", "cancel", "follow-up"},
		"worker": {"list", "rm"},
		"repo":   {"list"},
		"admin":  {"users", "runs", "workers", "usage", "rate-limits"},
		"skill":  {"status", "install"},
		"auth":   {"token", "status"},
	}
	for parent, kids := range subWant {
		pc := findCmd(root, parent)
		if pc == nil {
			t.Errorf("missing parent command %q", parent)
			continue
		}
		for _, kid := range kids {
			if findCmd(pc, kid) == nil {
				t.Errorf("missing %q subcommand %q", parent, kid)
			}
		}
	}

	// Global flags are the whole agent contract; assert they exist.
	for _, f := range []string{"json", "url", "quiet", "no-color"} {
		if root.PersistentFlags().Lookup(f) == nil {
			t.Errorf("missing global flag --%s", f)
		}
	}
}

func findCmd(parent *cobra.Command, name string) *cobra.Command {
	for _, c := range parent.Commands() {
		if c.Name() == name {
			return c
		}
	}
	return nil
}

func TestHelpRendersTree(t *testing.T) {
	out, _, code := runCLI(t, fakeEnv(&uzicli.FakeClient{}), "--help")
	if code != uzicli.ExitOK {
		t.Fatalf("--help exit = %d, want 0", code)
	}
	for _, want := range []string{"run", "worker", "repo", "admin", "skill", "version", "whoami", "--json"} {
		if !strings.Contains(out, want) {
			t.Errorf("--help output missing %q\n%s", want, out)
		}
	}
}

func TestWhoamiJSON(t *testing.T) {
	fc := &uzicli.FakeClient{User: uzicli.User{ID: "u1", Email: "a@example.com", IsAdmin: false}}
	out, _, code := runCLI(t, fakeEnv(fc), "whoami", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, `"id": "u1"`) || !strings.Contains(out, `"is_admin": false`) {
		t.Errorf("unexpected JSON:\n%s", out)
	}
}

func TestWhoamiTable(t *testing.T) {
	fc := &uzicli.FakeClient{User: uzicli.User{ID: "u1", Email: "a@example.com", IsAdmin: true}}
	out, _, code := runCLI(t, fakeEnv(fc), "whoami")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, "EMAIL") || !strings.Contains(out, "a@example.com") {
		t.Errorf("unexpected table:\n%s", out)
	}
}

func TestRunListJSON(t *testing.T) {
	fc := &uzicli.FakeClient{Runs: []uzicli.Run{{ID: "r1", Status: "running", Title: "fix"}}}
	out, _, code := runCLI(t, fakeEnv(fc), "run", "list", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, `"id": "r1"`) {
		t.Errorf("unexpected JSON:\n%s", out)
	}
}

func TestRunGetPresent(t *testing.T) {
	fc := &uzicli.FakeClient{RunByID: map[string]uzicli.Run{"r1": {ID: "r1", Status: "queued"}}}
	_, _, code := runCLI(t, fakeEnv(fc), "run", "get", "r1")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
}

func TestRunGetMissingExit4(t *testing.T) {
	fc := &uzicli.FakeClient{RunByID: map[string]uzicli.Run{}}
	_, _, code := runCLI(t, fakeEnv(fc), "run", "get", "nope")
	if code != uzicli.ExitNotFound {
		t.Fatalf("exit = %d, want %d (not found)", code, uzicli.ExitNotFound)
	}
}

func TestAdminAuthErrorExit3(t *testing.T) {
	fc := &uzicli.FakeClient{Err: uzicli.Exitf(uzicli.ExitAuth, "admin scope required")}
	_, _, code := runCLI(t, fakeEnv(fc), "admin", "users")
	if code != uzicli.ExitAuth {
		t.Fatalf("exit = %d, want %d (auth)", code, uzicli.ExitAuth)
	}
}

func TestUnknownCommandExit2(t *testing.T) {
	_, _, code := runCLI(t, fakeEnv(&uzicli.FakeClient{}), "bogus")
	if code != uzicli.ExitUsage {
		t.Fatalf("exit = %d, want %d (usage)", code, uzicli.ExitUsage)
	}
}

func TestUnknownFlagExit2(t *testing.T) {
	_, _, code := runCLI(t, fakeEnv(&uzicli.FakeClient{}), "run", "list", "--nope")
	if code != uzicli.ExitUsage {
		t.Fatalf("exit = %d, want %d (usage)", code, uzicli.ExitUsage)
	}
}

func TestStubExit1(t *testing.T) {
	_, _, code := runCLI(t, fakeEnv(&uzicli.FakeClient{}), "login")
	if code != uzicli.ExitGeneric {
		t.Fatalf("exit = %d, want %d (generic/not-implemented)", code, uzicli.ExitGeneric)
	}
}

func TestVersion(t *testing.T) {
	out, _, code := runCLI(t, fakeEnv(&uzicli.FakeClient{}), "version")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, version) {
		t.Errorf("version output %q missing %q", out, version)
	}
}
