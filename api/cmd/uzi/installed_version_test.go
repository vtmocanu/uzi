package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func formulaFixture(t *testing.T, formula string) string {
	t.Helper()
	prefix := filepath.Join(t.TempDir(), "Cellar", formula, "0.85.0")
	if err := os.MkdirAll(filepath.Join(prefix, "bin"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(prefix, "bin", "uzi"), []byte("fixture only; never executed"), 0700); err != nil { //nolint:gosec // G306: private fixture must pass executable-bit validation; the fake runner never executes it.
		t.Fatal(err)
	}
	return prefix
}

func assertProbeCommand(t *testing.T, ctx context.Context, cmd *exec.Cmd) {
	t.Helper()
	deadline, ok := ctx.Deadline()
	remaining := time.Until(deadline)
	if !ok || remaining > 10*time.Second || remaining < 9*time.Second {
		t.Fatalf("deadline = %v, remaining %s", ok, remaining)
	}
	if cmd.WaitDelay != time.Second {
		t.Fatalf("WaitDelay=%s", cmd.WaitDelay)
	}
	if cmd.Stdout == cmd.Stderr {
		t.Fatal("stdout and stderr are not separate")
	}
	if cmd.Stdin != nil {
		t.Fatal("quiet probe inherits stdin")
	}
}

func TestInstalledProbeCommandConstruction(t *testing.T) {
	for _, formula := range []string{"uzi-cli", "uzi-cli-rc"} {
		t.Run(formula, func(t *testing.T) {
			prefix := formulaFixture(t, formula)
			opt := filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(prefix))), "opt", formula)
			if err := os.MkdirAll(filepath.Dir(opt), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(prefix, opt); err != nil {
				t.Fatal(err)
			}
			t.Setenv("HOMEBREW_PREFIX", "/fixture/homebrew")
			t.Setenv("HOMEBREW_NO_AUTO_UPDATE", "1")
			t.Setenv("UZI_URL", "https://configured.example")
			t.Setenv("UZI_VERSION_CHECK", "1")
			t.Setenv("UZI_SKILL_AUTO_UPGRADE", "1")
			calls := 0
			run := func(ctx context.Context, cmd *exec.Cmd) error {
				assertProbeCommand(t, ctx, cmd)
				calls++
				if calls == 1 {
					if !reflect.DeepEqual(cmd.Args, []string{"brew", "--prefix", formula}) || cmd.Env != nil {
						t.Fatalf("Brew command/env: %v %v", cmd.Args, cmd.Env)
					}
					_, _ = io.WriteString(cmd.Stderr, "brew notice\n")
					_, _ = io.WriteString(cmd.Stdout, opt+"\n")
				} else {
					if cmd.Path != filepath.Join(prefix, "bin", "uzi") || !reflect.DeepEqual(cmd.Args, []string{cmd.Path, "version"}) {
						t.Fatalf("installed command: %q %v", cmd.Path, cmd.Args)
					}
					expected := map[string]string{"UZI_URL": "://", "UZI_VERSION_CHECK": "0", "UZI_SKILL_AUTO_UPGRADE": "0", "HOMEBREW_PREFIX": "/fixture/homebrew", "HOMEBREW_NO_AUTO_UPDATE": "1"}
					counts := map[string]int{}
					for _, entry := range cmd.Env {
						key, val, _ := strings.Cut(entry, "=")
						if want, ok := expected[key]; ok {
							counts[key]++
							if val != want {
								t.Fatalf("%s=%s, want %s", key, val, want)
							}
						}
					}
					for key := range expected {
						if counts[key] != 1 {
							t.Fatalf("environment %s count=%d", key, counts[key])
						}
					}
					_, _ = io.WriteString(cmd.Stderr, "stderr is not the version\n")
					_, _ = io.WriteString(cmd.Stdout, "v0.85.0\nother stdout\n")
				}
				return nil
			}
			got, err := probeInstalledVersion(run, formula)
			if err != nil || got != "v0.85.0" || calls != 2 {
				t.Fatalf("probe = %q %v calls=%d", got, err, calls)
			}
		})
	}
}

func TestInstalledProbeRejectsPathsAndOutput(t *testing.T) {
	for _, name := range []string{"relative prefix", "foreign prefix", "missing prefix", "missing executable", "not executable", "escaping symlink", "invalid", "double v", "oversized", "overflow", "missing brew"} {
		t.Run(name, func(t *testing.T) {
			prefix := formulaFixture(t, "uzi-cli")
			path := filepath.Join(prefix, "bin", "uzi")
			output := "v0.85.0\n"
			switch name {
			case "relative prefix":
				prefix = "Cellar/uzi-cli/0.85.0"
			case "foreign prefix":
				prefix = formulaFixture(t, "other")
			case "missing prefix":
				prefix = filepath.Join(t.TempDir(), "missing")
			case "missing executable":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case "not executable":
				if err := os.Chmod(path, 0600); err != nil {
					t.Fatal(err)
				}
			case "escaping symlink":
				outside := filepath.Join(t.TempDir(), "uzi")
				if err := os.WriteFile(outside, []byte("fixture"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, path); err != nil {
					t.Fatal(err)
				}
			case "invalid":
				output = "not a version"
			case "double v":
				output = "vv0.85.0"
			case "oversized":
				output = "v0.85.0+" + strings.Repeat("a", 1000)
			case "overflow":
				output = "v0.85.0\n" + strings.Repeat("a", 64*1024)
			}
			calls := 0
			run := func(ctx context.Context, cmd *exec.Cmd) error {
				calls++
				if name == "missing brew" {
					return exec.ErrNotFound
				}
				if calls == 1 {
					_, _ = io.WriteString(cmd.Stdout, prefix+"\n")
				} else {
					_, _ = io.WriteString(cmd.Stdout, output)
				}
				return nil
			}
			got, err := probeInstalledVersion(run, "uzi-cli")
			if err == nil || got != "" {
				t.Fatalf("accepted %s: %q %v", name, got, err)
			}
			if name == "escaping symlink" && !strings.Contains(err.Error(), "escapes formula prefix") {
				t.Fatalf("escaping symlink rejected for wrong reason: %v", err)
			}
			if strings.Contains(name, "prefix") || strings.Contains(name, "executable") || name == "escaping symlink" || name == "missing brew" {
				if calls != 1 {
					t.Fatalf("unsafe path reached child: calls=%d", calls)
				}
			}
		})
	}
	calls := 0
	if _, err := probeInstalledVersion(func(context.Context, *exec.Cmd) error { calls++; return nil }, "other/tap/uzi-cli"); err == nil || calls != 0 {
		t.Fatal("foreign formula reached runner")
	}
}

func TestQuietProbeSharedBudgetAndTimeout(t *testing.T) {
	t.Run("shared budget cancels", func(t *testing.T) {
		got, err := quietProcess(func(ctx context.Context, cmd *exec.Cmd) error {
			assertProbeCommand(t, ctx, cmd)
			_, _ = io.WriteString(cmd.Stdout, strings.Repeat("a", 40*1024))
			n, err := io.WriteString(cmd.Stderr, strings.Repeat("b", 30*1024))
			if n != 24*1024 || !errors.Is(err, errBrewProbeOverflow) || !errors.Is(ctx.Err(), context.Canceled) {
				t.Fatalf("shared budget n=%d err=%v ctx=%v", n, err, ctx.Err())
			}
			return nil
		}, "brew", []string{"--prefix", "uzi-cli"}, nil)
		if got != "" || !errors.Is(err, errBrewProbeOverflow) {
			t.Fatalf("overflow=%q %v", got, err)
		}
	})
	t.Run("deadline", func(t *testing.T) {
		start := time.Now()
		got, err := quietProcess(func(ctx context.Context, cmd *exec.Cmd) error {
			assertProbeCommand(t, ctx, cmd)
			<-ctx.Done()
			return ctx.Err()
		}, "brew", []string{"--prefix", "uzi-cli"}, nil)
		if got != "" || !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 12*time.Second {
			t.Fatalf("timeout=%q %v after %s", got, err, time.Since(start))
		}
	})
}

func TestInstalledProbeVersionCommandStaysOffline(t *testing.T) {
	withVersion(t, "v0.85.0")
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = fmt.Fprint(w, `{"version":"0.86.0"}`)
	}))
	defer server.Close()
	store := uzicli.NewStore(t.TempDir())
	if err := store.SaveConfig(&uzicli.Config{Contexts: map[string]uzicli.Context{"default": {URL: server.URL}}}); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0700); err != nil {
		t.Fatal(err)
	}
	env := DefaultEnv()
	env.Store = store
	env.SkillHome = home
	env.Getenv = func(string) string { return "" }
	t.Setenv("UZI_URL", server.URL)
	t.Setenv("UZI_SKILL_AUTO_UPGRADE", "0")
	t.Setenv("UZI_VERSION_CHECK", "0")
	// Preserve the child environment: the general runCLI helper clears UZI_URL.
	runVersion := func(args ...string) (string, string, int) {
		var out, stderr strings.Builder
		commandEnv := env
		commandEnv.Stdout, commandEnv.Stderr = &out, &stderr
		code := Main(commandEnv, args)
		return out.String(), stderr.String(), code
	}
	// Positive control proves the stored server is reachable by the unchanged version command.
	_, _, code := runVersion("version")
	if code != 0 || hits.Load() != 1 {
		t.Fatalf("configured-server control code=%d hits=%d", code, hits.Load())
	}
	hits.Store(0)
	// A separate home proves the startup-write fixture is reachable without the override.
	controlHome := t.TempDir()
	if err := os.MkdirAll(filepath.Join(controlHome, ".claude"), 0700); err != nil {
		t.Fatal(err)
	}
	controlEnv := env
	controlEnv.SkillHome = controlHome
	t.Setenv("UZI_SKILL_AUTO_UPGRADE", "1")
	maybeAutoUpgradeSkill(controlEnv)
	if _, err := os.Stat(filepath.Join(controlHome, ".claude", "skills", "uzi-cli")); err != nil {
		t.Fatalf("skill startup-write control failed: %v", err)
	}
	t.Setenv("UZI_VERSION_CHECK", "1")
	prefix := formulaFixture(t, "uzi-cli")
	run := func(_ context.Context, cmd *exec.Cmd) error {
		if cmd.Args[0] == "brew" {
			_, _ = io.WriteString(cmd.Stdout, prefix)
			return nil
		}
		for _, entry := range cmd.Env {
			key, val, _ := strings.Cut(entry, "=")
			if key == "UZI_URL" || key == "UZI_SKILL_AUTO_UPGRADE" || key == "UZI_VERSION_CHECK" {
				t.Setenv(key, val)
			}
		}
		out, stderr, code := runVersion(cmd.Args[1:]...)
		if code != 0 || stderr != "" {
			t.Fatalf("child command code=%d stderr=%q", code, stderr)
		}
		_, _ = io.WriteString(cmd.Stdout, out)
		return nil
	}
	got, err := probeInstalledVersion(run, "uzi-cli")
	if err != nil || got != "v0.85.0" || hits.Load() != 0 {
		t.Fatalf("probe=%q %v HTTP hits=%d", got, err, hits.Load())
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "skills", "uzi-cli")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("skill installation occurred: %v", err)
	}
}
