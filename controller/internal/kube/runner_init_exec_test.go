package kube

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runnerInitHost is one executable run of the rendered runner-init script against a
// temp directory standing in for the data PVC and the image's layout marker.
type runnerInitHost struct {
	t      *testing.T
	data   string // stands in for /data-seed
	marker string // stands in for /etc/uzi-runner-layout
}

func newRunnerInitHost(t *testing.T) *runnerInitHost {
	t.Helper()
	if os.Getuid() == 0 {
		t.Skip("uid 0: the script takes its root (uid-split) branch, which needs the worker/runner users; that branch is exercised by the e2e worker-uid lane")
	}
	if _, err := exec.LookPath("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	root := t.TempDir()
	h := &runnerInitHost{t: t, data: filepath.Join(root, "data"), marker: filepath.Join(root, "layout-marker")}
	if err := os.Mkdir(h.data, 0o750); err != nil {
		t.Fatal(err)
	}
	h.writeMarker(runnerLayoutMarkerValue)
	return h
}

func (h *runnerInitHost) writeMarker(content string) {
	h.t.Helper()
	if err := os.WriteFile(h.marker, []byte(content), 0o600); err != nil {
		h.t.Fatal(err)
	}
}

// run substitutes the two absolute roots and executes the script with /bin/sh as the
// current uid. It returns the exit code and stderr.
func (h *runnerInitHost) run() (int, string) {
	h.t.Helper()
	script := runnerInitScript()
	for _, abs := range []string{dataSeedMountPath, runnerLayoutMarkerFile} {
		if !strings.Contains(script, abs) {
			h.t.Fatalf("script no longer references %s; update this harness", abs)
		}
	}
	script = strings.ReplaceAll(script, runnerLayoutMarkerFile, h.marker)
	script = strings.ReplaceAll(script, dataSeedMountPath, h.data)
	path := filepath.Join(h.t.TempDir(), "init.sh")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		h.t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd := exec.Command("/bin/sh", path) //nolint:gosec // G204: fixed shell, script rendered by this package under t.TempDir.
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			h.t.Fatalf("run sh: %v", err)
		}
		code = ee.ExitCode()
	}
	return code, stderr.String()
}

func (h *runnerInitHost) shared() string { return filepath.Join(h.data, dindWorkdirSubPath) }

func (h *runnerInitHost) mustNotExist(p string) {
	h.t.Helper()
	if _, err := os.Lstat(p); err == nil {
		h.t.Errorf("%s exists; the script must not have written it", p)
	}
}

func (h *runnerInitHost) wantRefusal(code int, stderr, substr string) {
	h.t.Helper()
	if code == 0 {
		h.t.Fatalf("exit 0, want a refusal containing %q; stderr=%q", substr, stderr)
	}
	if !strings.Contains(stderr, substr) {
		h.t.Fatalf("stderr %q lacks %q", stderr, substr)
	}
}

// hostSupportsOwnerStickyTests reports whether /bin/sh implements test -O and -k; the
// script's single-uid branch fails closed on a shell that does not.
func hostSupportsOwnerStickyTests(t *testing.T) bool {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	err := exec.Command("/bin/sh", "-c", `[ -O "$1" ] && [ -k "$1" ]`, "sh", dir).Run() //nolint:gosec // G204: fixed shell and args.
	return err == nil
}

func TestRunnerInitExecFreshVolumeCreatesStickyRunnerSharedAndTmp(t *testing.T) {
	h := newRunnerInitHost(t)
	if !hostSupportsOwnerStickyTests(t) {
		t.Skip("host /bin/sh lacks test -O/-k; the single-uid branch fails closed there by design")
	}
	// Sibling data content the script must not touch.
	if err := os.MkdirAll(filepath.Join(h.data, "repos"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(h.data, "runner", "old"), 0o750); err != nil {
		t.Fatal(err)
	}
	repoFile := filepath.Join(h.data, "repos", "x")
	oldFile := filepath.Join(h.data, "runner", "old", "keep")
	for _, p := range []string{repoFile, oldFile} {
		if err := os.WriteFile(p, []byte("data"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	code, stderr := h.run()
	if code != 0 {
		t.Fatalf("exit %d, want 0; stderr=%q", code, stderr)
	}
	st, err := os.Lstat(h.shared())
	if err != nil || !st.IsDir() {
		t.Fatalf("runner-shared not a directory: %v", err)
	}
	if st.Mode()&os.ModeSticky == 0 {
		t.Errorf("runner-shared mode %v is not sticky", st.Mode())
	}
	if st, err := os.Lstat(filepath.Join(h.shared(), ".tmp")); err != nil || !st.IsDir() {
		t.Errorf(".tmp not a directory: %v", err)
	}
	for _, p := range []string{repoFile, oldFile} {
		if b, err := os.ReadFile(p); err != nil || string(b) != "data" {
			t.Errorf("sibling %s changed: %q %v", p, b, err)
		}
	}
	entries, err := os.ReadDir(h.data)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if got := strings.Join(names, ","); got != "repos,runner,runner-shared" {
		t.Errorf("data root entries = %q, want only repos,runner,runner-shared", got)
	}

	// A second boot over the migrated volume is idempotent.
	if code, stderr := h.run(); code != 0 {
		t.Errorf("rerun exit %d; stderr=%q", code, stderr)
	}
}

func TestRunnerInitExecRefusesHostileVolumeState(t *testing.T) {
	t.Run("symlink at runner-shared", func(t *testing.T) {
		h := newRunnerInitHost(t)
		if err := os.Symlink(".", h.shared()); err != nil {
			t.Fatal(err)
		}
		code, stderr := h.run()
		h.wantRefusal(code, stderr, "is a symlink")
		if dest, err := os.Readlink(h.shared()); err != nil || dest != "." {
			t.Errorf("symlink changed: %q %v", dest, err)
		}
		h.mustNotExist(filepath.Join(h.data, ".tmp"))
		entries, _ := os.ReadDir(h.data)
		if len(entries) != 1 {
			t.Errorf("data root gained entries: %v", entries)
		}
	})
	t.Run("symlink at .tmp", func(t *testing.T) {
		h := newRunnerInitHost(t)
		if err := os.Mkdir(h.shared(), 0o750); err != nil {
			t.Fatal(err)
		}
		tmp := filepath.Join(h.shared(), ".tmp")
		if err := os.Symlink("..", tmp); err != nil {
			t.Fatal(err)
		}
		code, stderr := h.run()
		h.wantRefusal(code, stderr, "is a symlink")
		if dest, err := os.Readlink(tmp); err != nil || dest != ".." {
			t.Errorf("symlink changed: %q %v", dest, err)
		}
	})
	t.Run("regular file at runner-shared", func(t *testing.T) {
		h := newRunnerInitHost(t)
		if err := os.WriteFile(h.shared(), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		code, stderr := h.run()
		h.wantRefusal(code, stderr, "is not a directory")
		if b, err := os.ReadFile(h.shared()); err != nil || string(b) != "x" {
			t.Errorf("file changed: %q %v", b, err)
		}
	})
}

func TestRunnerInitExecRefusesBadImageMarkerBeforeAnyWrite(t *testing.T) {
	const want = "worker image lacks runner-shared layout support"
	t.Run("missing", func(t *testing.T) {
		h := newRunnerInitHost(t)
		if err := os.Remove(h.marker); err != nil {
			t.Fatal(err)
		}
		code, stderr := h.run()
		h.wantRefusal(code, stderr, want)
		h.mustNotExist(h.shared())
	})
	t.Run("wrong content", func(t *testing.T) {
		h := newRunnerInitHost(t)
		h.writeMarker("runner-shared-v0")
		code, stderr := h.run()
		h.wantRefusal(code, stderr, want)
		h.mustNotExist(h.shared())
	})
	t.Run("symlink to a correct-content file", func(t *testing.T) {
		h := newRunnerInitHost(t)
		real := h.marker + ".real"
		if err := os.WriteFile(real, []byte(runnerLayoutMarkerValue), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(h.marker); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(real, h.marker); err != nil {
			t.Fatal(err)
		}
		code, stderr := h.run()
		h.wantRefusal(code, stderr, want)
		h.mustNotExist(h.shared())
	})
}

// A volume whose runner-shared exists but is not sticky/owned cannot be fixed by a
// non-root container that does not own it; here the current uid owns it, so the chmod
// succeeds and the script converges (the success path of the single-uid branch).
func TestRunnerInitExecSingleUIDRepairsModeOnOwnedDirectories(t *testing.T) {
	h := newRunnerInitHost(t)
	if !hostSupportsOwnerStickyTests(t) {
		t.Skip("host /bin/sh lacks test -O/-k; the single-uid branch fails closed there by design")
	}
	if err := os.MkdirAll(filepath.Join(h.shared(), ".tmp"), 0o700); err != nil {
		t.Fatal(err)
	}
	if code, stderr := h.run(); code != 0 {
		t.Fatalf("exit %d; stderr=%q", code, stderr)
	}
	st, err := os.Lstat(h.shared())
	if err != nil || st.Mode()&os.ModeSticky == 0 {
		t.Errorf("runner-shared not made sticky: %v %v", st, err)
	}
}
