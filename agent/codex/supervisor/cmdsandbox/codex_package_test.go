package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// codexPackageRoot is the image-baked, root-owned pinned Codex package.
const codexPackageRoot = "/opt/uzi-codex"

// TestAddRulesGrantsCodexPackageReadExecuteOnly: the /opt/uzi-codex path rule
// is present with exactly read&handled, and no write, remove, make, refer or
// truncate right, even when every right is handled.
func TestAddRulesGrantsCodexPackageReadExecuteOnly(t *testing.T) {
	handled := baseRights | unix.LANDLOCK_ACCESS_FS_REFER | unix.LANDLOCK_ACCESS_FS_TRUNCATE
	read := uint64(unix.LANDLOCK_ACCESS_FS_EXECUTE | unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_READ_DIR)
	forbidden := uint64(unix.LANDLOCK_ACCESS_FS_WRITE_FILE |
		unix.LANDLOCK_ACCESS_FS_REMOVE_DIR |
		unix.LANDLOCK_ACCESS_FS_REMOVE_FILE |
		unix.LANDLOCK_ACCESS_FS_MAKE_CHAR |
		unix.LANDLOCK_ACCESS_FS_MAKE_DIR |
		unix.LANDLOCK_ACCESS_FS_MAKE_REG |
		unix.LANDLOCK_ACCESS_FS_MAKE_SOCK |
		unix.LANDLOCK_ACCESS_FS_MAKE_FIFO |
		unix.LANDLOCK_ACCESS_FS_MAKE_BLOCK |
		unix.LANDLOCK_ACCESS_FS_MAKE_SYM |
		unix.LANDLOCK_ACCESS_FS_REFER |
		unix.LANDLOCK_ACCESS_FS_TRUNCATE)

	var codexRules []uint64
	err := addRules(7, t.TempDir(), grantFds{tmp: 3, cache: -1}, handled, ruleAdders{
		path: func(_ int, path string, access uint64) error {
			if path == codexPackageRoot {
				codexRules = append(codexRules, access)
			}
			return nil
		},
		fd: func(int, int, uint64) error { return nil },
	})
	if err != nil {
		t.Fatalf("addRules: %v", err)
	}
	if len(codexRules) != 1 {
		t.Fatalf("%s rules = %#v, want exactly one", codexPackageRoot, codexRules)
	}
	if got, want := codexRules[0], read&handled; got != want {
		t.Fatalf("%s access = %#x, want read&handled %#x", codexPackageRoot, got, want)
	}
	if extra := codexRules[0] & forbidden; extra != 0 {
		t.Fatalf("%s access carries forbidden rights %#x", codexPackageRoot, extra)
	}
}

// codexPackageHelperEnv carries the JSON argv for the re-exec'd sandbox helper.
const codexPackageHelperEnv = "CMDSANDBOX_TEST_CODEX_PACKAGE_ARGS"

// TestLandlockCodexPackageExecutable runs the real sandbox (required mode) in a
// re-exec'd copy of this test binary: the sandboxed /bin/sh can exec the pinned
// /opt/uzi-codex/<version>/bin/codex --version, and cannot create a file under
// /opt/uzi-codex. It skips where Landlock, the baked deny probe or the baked
// Codex package is unavailable. The exec half is the discriminating one: when
// the test runs as non-root, DAC already refuses the write to the root-owned
// package, so that half only proves the rule adds no write right on a root run.
func TestLandlockCodexPackageExecutable(t *testing.T) {
	if raw := os.Getenv(codexPackageHelperEnv); raw != "" {
		var args []string
		if err := json.Unmarshal([]byte(raw), &args); err != nil {
			os.Exit(90)
		}
		os.Exit(runOnLockedThread(args, runtime.LockOSThread, realMain))
	}
	if avail, _, _ := classifyLandlock(realVersionProbe); avail != landlockAvailable {
		t.Skip("Landlock is not available on this kernel")
	}
	if f, err := os.Open(landlockDenyProbe); err != nil {
		t.Skipf("the deny probe %s is not readable here: %v", landlockDenyProbe, err)
	} else {
		_ = f.Close()
	}
	bins, err := filepath.Glob(filepath.Join(codexPackageRoot, "*", "bin", "codex"))
	if err != nil || len(bins) == 0 {
		t.Skipf("no baked Codex binary under %s", codexPackageRoot)
	}
	codex := bins[0]
	root := t.TempDir()
	// A scratch HOME/CODEX_HOME under root keeps both runs off the real Codex home.
	codexEnv := append(os.Environ(), "HOME="+root, "CODEX_HOME="+root)
	// Unsandboxed baseline: the binary runs here, so a sandboxed failure is the policy's.
	baseline := exec.Command(codex, "--version")
	baseline.Env = codexEnv
	if out, err := baseline.CombinedOutput(); err != nil {
		t.Skipf("%s --version fails even unsandboxed: %v\n%s", codex, err, out)
	}
	probe := filepath.Join(codexPackageRoot, ".cmdsandbox-write-probe-"+strings.ReplaceAll(t.Name(), "/", "_"))
	t.Cleanup(func() { _ = os.Remove(probe) })

	tmp := privateTmp(t)
	script := `"$CODEX" --version || exit 10
if touch "$PROBE" 2>/dev/null; then exit 11; fi
exit 0`
	args, err := json.Marshal([]string{"--root", root, "--tmp", tmp, "--cwd", root, "--mode", "required", "--", "/bin/sh", "-c", script})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestLandlockCodexPackageExecutable$")
	cmd.Env = append(codexEnv, codexPackageHelperEnv+"="+string(args), "CODEX="+codex, "PROBE="+probe)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sandboxed child failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "codex-cli ") {
		t.Fatalf("sandboxed codex --version printed no version:\n%s", out)
	}
}
