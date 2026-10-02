package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"testing"
	"time"
)

// stdinHelperEnv carries the JSON argv for the re-exec'd sandbox helper.
const stdinHelperEnv = "CMDSANDBOX_TEST_STDIN_ARGS"

// runSandbox re-execs this test binary as the sandbox (the helper branch lives
// at the top of the calling top-level test) with the given stdin and returns
// the exit code and stdout. A hang is killed after a deadline and fails.
func runSandbox(t *testing.T, testName string, args []string, stdin *os.File) (int, string) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^"+testName+"$")
	cmd.Env = append(os.Environ(), stdinHelperEnv+"="+string(raw))
	cmd.Stdin = stdin
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		code := 0
		if err != nil {
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				t.Fatalf("wait: %v", err)
			}
			code = exit.ExitCode()
		}
		if code != 0 && stderr.Len() > 0 {
			t.Logf("sandbox stderr:\n%s", stderr.String())
		}
		return code, stdout.String()
	case <-time.After(10 * time.Second):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		t.Fatalf("sandboxed command blocked on stdin; stderr:\n%s", stderr.String())
		return 0, ""
	}
}

// sandboxArgs builds the sandbox argv; nullStdin adds the opt-in `--stdin null`.
func sandboxArgs(t *testing.T, mode string, nullStdin bool, child ...string) []string {
	t.Helper()
	root := t.TempDir()
	args := []string{"--root", root, "--tmp", privateTmp(t), "--cwd", root, "--mode", mode}
	if nullStdin {
		args = append(args, "--stdin", "null")
	}
	return append(append(args, "--"), child...)
}

func helperMain() {
	if raw := os.Getenv(stdinHelperEnv); raw != "" {
		var args []string
		if err := json.Unmarshal([]byte(raw), &args); err != nil {
			os.Exit(90)
		}
		os.Exit(runOnLockedThread(args, runtime.LockOSThread, realMain))
	}
}

// TestCommandStdinIsEmpty keeps the write end of the inherited stdin pipe open,
// as the supervisor does: with `--stdin null` a command reading stdin must
// still see EOF.
func TestCommandStdinIsEmpty(t *testing.T) {
	helperMain()
	cat, err := exec.LookPath("cat")
	if err != nil {
		t.Skipf("cat not found: %v", err)
	}
	for _, mode := range []string{"off", "required"} {
		t.Run(mode, func(t *testing.T) {
			if mode == "required" {
				if avail, _, _ := classifyLandlock(realVersionProbe); avail != landlockAvailable {
					t.Skip("Landlock is not available on this kernel")
				}
				if f, err := os.Open(landlockDenyProbe); err != nil {
					t.Skipf("the deny probe %s is not readable here: %v", landlockDenyProbe, err)
				} else {
					_ = f.Close()
				}
			}
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = r.Close(); _ = w.Close() })
			code, out := runSandbox(t, "TestCommandStdinIsEmpty", sandboxArgs(t, mode, true, cat), r)
			if code != 0 || out != "" {
				t.Fatalf("exit=%d stdout=%q, want 0 and empty", code, out)
			}
		})
	}
}

// TestShellProvidedStdinStillWorks proves pipes and heredocs inside the
// command are unaffected by `--stdin null`, and that exit status passes through.
func TestShellProvidedStdinStillWorks(t *testing.T) {
	helperMain()
	cases := []struct {
		name, script, out string
		code              int
	}{
		{"pipe", "printf 'x\\n' | cat", "x\n", 0},
		{"heredoc", "cat <<EOF\nx\nEOF\n", "x\n", 0},
		{"exit status", "exit 7", "", 7},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, out := runSandbox(t, "TestShellProvidedStdinStillWorks", sandboxArgs(t, "off", true, "/bin/sh", "-c", c.script), nil)
			if code != c.code || out != c.out {
				t.Fatalf("exit=%d stdout=%q, want %d %q", code, out, c.code, c.out)
			}
		})
	}
}

// TestDefaultStdinIsInherited pins the contract the worker's own streaming
// processes (fileop, git index-pack --stdin) rely on: without `--stdin null`
// the command reads the sandbox's stdin.
func TestDefaultStdinIsInherited(t *testing.T) {
	helperMain()
	cat, err := exec.LookPath("cat")
	if err != nil {
		t.Skipf("cat not found: %v", err)
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close(); _ = w.Close() })
	if _, err := w.WriteString("payload\n"); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	code, out := runSandbox(t, "TestDefaultStdinIsInherited", sandboxArgs(t, "off", false, cat), r)
	if code != 0 || out != "payload\n" {
		t.Fatalf("exit=%d stdout=%q, want 0 and %q", code, out, "payload\n")
	}
}
