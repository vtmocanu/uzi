//go:build unix

package kube

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The meter exits 0 promptly on SIGTERM while it is waiting out its interval (issue
// #3421). The rendered script runs for real under /bin/sh with its paths rewritten to
// scratch dirs, and `sleep 30` replaced by an instrumented fixture that signals readiness
// when invoked and then stays blocked, so SIGTERM is sent only once the script is
// provably inside its wait. Dropping the trap fails the exit-0 assertion (the shell dies
// of the signal); a foreground sleep fails the 2s bound (a trap waits for a foreground
// child to finish).
//
// Limits: this does NOT reproduce PID-1 signal semantics (here an untrapped SIGTERM
// kills the shell; as a container's PID 1 the kernel drops it). Once the shell exits,
// the fixture is an orphaned grandchild: cleanup still kills it through the process
// group, but the test cannot wait() it, so init reaps it. In production the orphaned
// sleep dies when the meter container's private PID namespace is torn down.
func TestDindMeterScriptExitsZeroPromptlyOnSIGTERM(t *testing.T) {
	const sh = "/bin/sh"
	if _, err := os.Stat(sh); err != nil {
		t.Skip("no /bin/sh")
	}
	dir := t.TempDir()
	ready := filepath.Join(dir, "ready")
	fixture := filepath.Join(dir, "sleep-fixture")
	fixtureBody := "#!/bin/sh\n" +
		"printf '%s\\n' \"$$\" > '" + ready + ".tmp' && mv -f '" + ready + ".tmp' '" + ready + "'\n" +
		"exec sleep 600 >/dev/null 2>&1\n"
	if err := os.WriteFile(fixture, []byte(fixtureBody), 0o700); err != nil { //nolint:gosec // G306: an executable test fixture under t.TempDir()
		t.Fatal(err)
	}
	script := strings.NewReplacer(
		dindMeterDataDir, t.TempDir(),
		dindMeterDir, t.TempDir(),
		"sleep 30", "'"+fixture+"' 30",
	).Replace(dindMeterScript())
	if !strings.Contains(script, fixture) {
		t.Fatalf("the fixture was not substituted into the meter script:\n%s", script)
	}

	// Stdout/Stderr stay nil (/dev/null), so Wait never blocks on a pipe the background
	// fixture holds open; WaitDelay is a second guard against the same.
	cmd := exec.Command(sh, "-c", script) //nolint:gosec // G204: runs the renderer's own meter script with test-controlled temp paths, not user input
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = time.Second
	if err := cmd.Start(); err != nil {
		t.Fatalf("start meter script: %v", err)
	}
	pgid := cmd.Process.Pid

	// The ONE owner of cmd.Wait: everything else observes done.
	var waitErr error
	done := make(chan struct{})
	go func() {
		waitErr = cmd.Wait()
		close(done)
	}()
	// On every outcome: kill the owned process group (the shell and the fixture, which
	// stays in the shell's group), then join the same Wait.
	t.Cleanup(func() {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Errorf("meter script (pid %d) was not reaped within 10s of SIGKILL", pgid)
		}
	})

	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		select {
		case <-done:
			t.Fatalf("meter script exited before reaching its wait: %v", waitErr)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the sleep fixture never signalled readiness within 10s")
		}
		time.Sleep(10 * time.Millisecond)
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM the meter script: %v", err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the meter script did not exit within 2s of SIGTERM")
	}
	var exitErr *exec.ExitError
	switch {
	case waitErr == nil:
	case errors.As(waitErr, &exitErr):
		t.Fatalf("the meter script must exit 0 on SIGTERM, got %v (state %v)", waitErr, exitErr.ProcessState)
	default:
		t.Fatalf("waiting for the meter script: %v", waitErr)
	}
}
