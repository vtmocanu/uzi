package main

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

type execRec struct {
	calls []execCall
	err   error
}

type execCall struct {
	path       string
	argv, envv []string
}

func (r *execRec) fn(path string, argv, envv []string) error {
	r.calls = append(r.calls, execCall{path, append([]string(nil), argv...), append([]string(nil), envv...)})
	return r.err
}

func okBrew(bool, ...string) (string, error) { return "", nil }

// A verified "Update now" restarts the TUI: it execs the verified formula-owned file
// with the original invocation's arguments and the caller's unmodified environment.
func TestPendingUpgradeRelaunchesVerifiedBinary(t *testing.T) {
	const path = "/fixture/Cellar/uzi-cli/0.86.0/bin/uzi"
	t.Setenv("UZI_VERSION_CHECK", "1")
	t.Setenv("UZI_RELAUNCH_FIXTURE", "kept")
	for _, installed := range []string{"v0.85.0", "v0.86.0"} {
		t.Run(installed, func(t *testing.T) {
			var out strings.Builder
			rec := &execRec{}
			probed := false
			env := Env{Stdout: &out, Stderr: io.Discard, Brew: okBrew,
				InstalledCLI: func(string) (installedCLI, error) {
					probed = true
					return installedCLI{Version: installed, Path: path}, nil
				},
				Exec: func(p string, argv, envv []string) error {
					if !probed {
						t.Fatal("exec before verification")
					}
					return rec.fn(p, argv, envv)
				},
				Args: []string{"--context", "work", "tui"},
			}
			if err := runPendingUpgrade(env, []string{"upgrade", "vtmocanu/tap/uzi-cli"}, "v0.85.0"); err != nil {
				t.Fatalf("runPendingUpgrade: %v", err)
			}
			if len(rec.calls) != 1 {
				t.Fatalf("exec calls = %d, want 1", len(rec.calls))
			}
			call := rec.calls[0]
			if call.path != path || !reflect.DeepEqual(call.argv, []string{path, "--context", "work", "tui"}) {
				t.Fatalf("exec %q %v", call.path, call.argv)
			}
			if !reflect.DeepEqual(call.envv, os.Environ()) {
				t.Fatal("relaunch environment differs from the caller's")
			}
			if !strings.Contains(out.String(), "Restarting the TUI") {
				t.Fatalf("stdout = %q", out.String())
			}
		})
	}
}

// Nothing short of a verified upgrade reaches exec.
func TestPendingUpgradeNoRelaunchWithoutVerification(t *testing.T) {
	for name, tc := range map[string]struct {
		brewErr error
		cli     installedCLI
		probe   error
	}{
		"brew failure":    {brewErr: errors.New("compile failed"), cli: installedCLI{Version: "v0.85.0", Path: "/x"}},
		"probe failure":   {probe: errors.New("formula prefix"), cli: installedCLI{Path: "/x"}},
		"older installed": {cli: installedCLI{Version: "v0.84.0", Path: "/x"}},
		"invalid version": {cli: installedCLI{Version: "vv0.85.0", Path: "/x"}},
	} {
		t.Run(name, func(t *testing.T) {
			rec := &execRec{}
			env := Env{Stdout: io.Discard, Stderr: io.Discard,
				Brew:         func(bool, ...string) (string, error) { return "", tc.brewErr },
				InstalledCLI: func(string) (installedCLI, error) { return tc.cli, tc.probe },
				Exec:         rec.fn,
			}
			if err := runPendingUpgrade(env, []string{"upgrade", "vtmocanu/tap/uzi-cli"}, "v0.85.0"); err == nil || len(rec.calls) != 0 {
				t.Fatalf("err=%v exec calls=%d", err, len(rec.calls))
			}
		})
	}
}

// The upgrade succeeded but the restart did not: exit nonzero, sanitized, with the manual step.
func TestPendingUpgradeRelaunchFailure(t *testing.T) {
	for name, execFn := range map[string]func(string, []string, []string) error{
		"exec error": (&execRec{err: errors.New("exec format error\x1b[2J")}).fn,
		"nil exec":   nil,
	} {
		t.Run(name, func(t *testing.T) {
			env := Env{Stdout: io.Discard, Stderr: io.Discard, Brew: okBrew,
				InstalledCLI: func(string) (installedCLI, error) { return installedCLI{Version: "v0.85.0", Path: "/x"}, nil },
				Exec:         execFn,
			}
			err := runPendingUpgrade(env, []string{"upgrade", "vtmocanu/tap/uzi-cli"}, "v0.85.0")
			if err == nil || uzicli.ExitCodeFor(err) != uzicli.ExitGeneric {
				t.Fatalf("err=%v", err)
			}
			if !strings.Contains(err.Error(), "update succeeded, but restarting the TUI failed") || !strings.Contains(err.Error(), "run `uzi tui`") {
				t.Fatalf("message = %q", err.Error())
			}
			assertNoRawControls(t, "relaunch failure", err.Error())
		})
	}
}

// The probe runs, and reports for exec, the symlink-resolved file inside the prefix,
// so a relink between verification and exec cannot swap the target.
func TestInstalledProbeRunsResolvedExecutable(t *testing.T) {
	prefix, err := filepath.EvalSymlinks(formulaFixture(t, "uzi-cli"))
	if err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(prefix, "libexec", "uzi")
	if err := os.MkdirAll(filepath.Dir(real), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(prefix, "bin", "uzi"), real); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "libexec", "uzi"), filepath.Join(prefix, "bin", "uzi")); err != nil {
		t.Fatal(err)
	}
	var ran string
	run := func(_ context.Context, cmd *exec.Cmd) error {
		if cmd.Args[0] == "brew" {
			_, _ = io.WriteString(cmd.Stdout, prefix)
			return nil
		}
		ran = cmd.Path
		_, _ = io.WriteString(cmd.Stdout, "v0.85.0\n")
		return nil
	}
	cli, err := probeInstalledCLI(run, "uzi-cli")
	if err != nil || ran != real || cli.Path != real || cli.Version != "v0.85.0" {
		t.Fatalf("probe=%+v err=%v ran=%q want %q", cli, err, ran, real)
	}
}

// An executable escaping the formula prefix fails verification, so it never reaches exec.
func TestPendingUpgradeEscapingExecutableNeverExecs(t *testing.T) {
	prefix := formulaFixture(t, "uzi-cli")
	outside := filepath.Join(t.TempDir(), "uzi")
	if err := os.WriteFile(outside, []byte("fixture"), 0700); err != nil { //nolint:gosec // G306: fixture must look executable; it is never run.
		t.Fatal(err)
	}
	bin := filepath.Join(prefix, "bin", "uzi")
	if err := os.Remove(bin); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, bin); err != nil {
		t.Fatal(err)
	}
	run := func(_ context.Context, cmd *exec.Cmd) error {
		if cmd.Args[0] != "brew" {
			t.Fatalf("escaping executable was run: %v", cmd.Args)
		}
		_, _ = io.WriteString(cmd.Stdout, prefix)
		return nil
	}
	rec := &execRec{}
	env := Env{Stdout: io.Discard, Stderr: io.Discard, Brew: okBrew,
		InstalledCLI: func(f string) (installedCLI, error) { return probeInstalledCLI(run, f) },
		Exec:         rec.fn,
	}
	err := runPendingUpgrade(env, []string{"upgrade", "vtmocanu/tap/uzi-cli"}, "v0.85.0")
	if err == nil || !strings.Contains(err.Error(), "escapes formula prefix") || len(rec.calls) != 0 {
		t.Fatalf("err=%v exec calls=%d", err, len(rec.calls))
	}
}
