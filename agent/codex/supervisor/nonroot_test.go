package main

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
)

// nonRootChildEnv marks the re-exec'd test process so it runs the body instead
// of re-exec'ing again.
const nonRootChildEnv = "SUPERVISOR_TEST_NONROOT_CHILD"

// commandUID is the non-root uid the Codex command sandbox runs as.
const commandUID = 10003

// requireNonRootCommandUID guarantees the calling test body runs with a non-root
// euid, because root bypasses the permission and ownership checks a command tmp
// test exists to exercise, and would let it pass for the wrong reason. Copied
// from internal/safetree/export_test.go.
//
// It returns true when the caller should run its body in this process. As root it
// instead re-execs this exact test as uid/gid 10003 (a copy of the test binary in
// a world-readable dir, with a world-writable sticky TMPDIR so the child's
// t.TempDir works), fails the test if the child fails, and returns false.
func requireNonRootCommandUID(t *testing.T) bool {
	t.Helper()
	euid := os.Geteuid()
	if os.Getenv(nonRootChildEnv) == "1" {
		if euid == 0 {
			t.Fatalf("re-exec'd child still runs as root")
		}
		return true
	}
	if euid != 0 {
		return true
	}

	// The go-build dir holding os.Args[0] is root-only 0700, so stage a copy the
	// command uid can execute.
	stage, err := os.MkdirTemp("", "supervisor-nonroot-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stage) })
	if err := os.Chmod(stage, 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(stage, "supervisor.test")
	copyExecutable(t, os.Args[0], bin)
	tmp := filepath.Join(stage, "tmp")
	if err := os.Mkdir(tmp, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(tmp, 0o1777); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bin, "-test.run", "^"+regexp.QuoteMeta(t.Name())+"$", "-test.count=1", "-test.v")
	cmd.Dir = stage
	cmd.Env = append(os.Environ(), nonRootChildEnv+"=1", "TMPDIR="+tmp)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: commandUID, Gid: commandUID},
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("non-root re-exec of %s failed: %v\n%s", t.Name(), err, out)
	}
	if !strings.Contains(string(out), "--- PASS: "+t.Name()) {
		t.Fatalf("non-root re-exec of %s did not run the test:\n%s", t.Name(), out)
	}
	t.Logf("ran as uid %d:\n%s", commandUID, out)
	return false
}

func copyExecutable(t *testing.T, src, dst string) {
	t.Helper()
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o555)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}
