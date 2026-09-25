package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"uzi.local/codex-supervisor/internal/safetree"
)

// Helper-process modes of this test binary (see TestMain).
const (
	testHelperEnv     = "SUPERVISOR_TEST_HELPER"
	testHelperArgEnv  = "SUPERVISOR_TEST_HELPER_ARG"
	testHelperLock    = "lock" // flock the dir in ARG, print "locked", sleep
	testHelperRunMode = "mode" // runMode(JSON argv in ARG) with the real env
	// testHelperRealMain runs realMain(JSON argv in ARG): the whole dispatch.
	testHelperRealMain = "realmain"
)

func TestMain(m *testing.M) {
	switch os.Getenv(testHelperEnv) {
	case testHelperLock:
		fd, err := unix.Open(os.Getenv(testHelperArgEnv), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		if err != nil || unix.Flock(fd, unix.LOCK_EX) != nil {
			os.Exit(90)
		}
		fmt.Println("locked")
		time.Sleep(time.Hour)
		os.Exit(0)
	case testHelperRunMode:
		var args []string
		if err := json.Unmarshal([]byte(os.Getenv(testHelperArgEnv)), &args); err != nil {
			os.Exit(90)
		}
		os.Exit(runMode(args, realModeEnv()))
	case testHelperRealMain:
		var args []string
		if err := json.Unmarshal([]byte(os.Getenv(testHelperArgEnv)), &args); err != nil {
			os.Exit(90)
		}
		os.Exit(realMain(args))
	}
	os.Exit(m.Run())
}

// helperProc is a started helper process and its stdout.
type helperProc struct {
	cmd *exec.Cmd
	in  io.WriteCloser
	out *bufio.Reader
}

// startHelper starts this test binary as helper mode with arg and returns it
// once its first stdout line is read (returned too). It is SIGKILLed and
// reaped at cleanup if still running.
func startHelper(t *testing.T, mode, arg string) (*helperProc, string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), testHelperEnv+"="+mode, testHelperArgEnv+"="+arg)
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	h := &helperProc{cmd: cmd, in: in, out: bufio.NewReader(stdout)}
	t.Cleanup(func() { h.kill() })
	line, err := h.out.ReadString('\n')
	if err != nil {
		t.Fatalf("helper %s: first line: %v", mode, err)
	}
	return h, strings.TrimSpace(line)
}

// kill SIGKILLs the helper and waits for it, so its pid is gone.
func (h *helperProc) kill() {
	if h.cmd.ProcessState != nil {
		return
	}
	_ = h.cmd.Process.Signal(syscall.SIGKILL)
	_ = h.cmd.Wait()
}

// spawnUser starts a same-uid "command" process (sleep) with cwd dir, killed
// and reaped at cleanup.
func spawnUser(t *testing.T, dir string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	cmd.Dir = dir
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { killWait(cmd) })
	return cmd
}

func killWait(cmd *exec.Cmd) {
	if cmd.ProcessState != nil {
		return
	}
	_ = cmd.Process.Signal(syscall.SIGKILL)
	_ = cmd.Wait()
}

// filteredTable is the REAL proc scan restricted to the pids a test spawned
// plus this process (the only exempt pid), so that unrelated same-uid
// processes on the host, this test binary's own ancestors included (go test
// and the shell above it run as the test's uid), do not decide the proof. An
// invalid scan (a listed pid vanished anywhere on the host) is retaken here up
// to 200 times, 10 ms apart (back-to-back retries exhaust in milliseconds on a
// busy host), before it reaches proveNoUser's own rescan rule, so a busy host
// does not turn an expected "held" into "unknown"; a vanished scan is still
// never accepted.
type filteredTable struct {
	keep map[int]bool
}

func (f filteredTable) rows() ([]procUserRow, error) {
	var all []procUserRow
	var err error
	for range 200 {
		all, err = procFS{root: procRootPath, self: os.Getpid()}.rows()
		if !errors.Is(err, errProcVanished) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		return nil, err
	}
	var out []procUserRow
	for _, r := range all {
		if f.keep[r.pid] || r.pid == os.Getpid() {
			out = append(out, r)
		}
	}
	return out, nil
}

// realTable returns a filteredTable over pids, skipping where the real scan
// cannot run (a hiding proc mount).
func realTable(t *testing.T, pids ...int) procTable {
	t.Helper()
	if _, err := (filteredTable{}).rows(); err != nil {
		if errors.Is(err, errProcHidden) {
			t.Skip("the proc mount here hides processes")
		}
		t.Fatalf("real proc scan: %v", err)
	}
	keep := map[int]bool{}
	for _, p := range pids {
		keep[p] = true
	}
	return filteredTable{keep: keep}
}

// tableConfig proves against table as this process.
func tableConfig(table procTable) reapConfig {
	uid := os.Geteuid()
	return reapConfig{uid: uid, prove: func() string { return proveNoUser(table, uid, os.Getpid()) }, now: time.Now}
}

// proofConfig returns the given proofs in order (the last one repeats) and
// counts the calls.
func proofConfig(calls *int, proofs ...string) reapConfig {
	return reapConfig{uid: os.Geteuid(), now: time.Now, prove: func() string {
		i := min(*calls, len(proofs)-1)
		*calls++
		return proofs[i]
	}}
}

// testRoots is a stand-in /tmp and cache root, with their fds.
type testRoots struct {
	tmp, cache     string
	tmpFd, cacheFd int
}

func newRoots(t *testing.T) testRoots {
	t.Helper()
	r := testRoots{tmp: t.TempDir(), cache: filepath.Join(t.TempDir(), "uzi-codex-cmd")}
	if err := os.Mkdir(r.cache, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(r.cache, 0o700); err != nil {
		t.Fatal(err)
	}
	r.tmpFd = openParent(t, r.tmp)
	r.cacheFd = openParent(t, r.cache)
	return r
}

// tokenN is a distinct lowercase uuid per n, assembled at runtime.
func tokenN(n int) string { return fmt.Sprintf("%08x", n) + testToken[8:] }

func tmpNameN(n int) string { return commandTmpNamePrefix + tokenN(n) }

// mkTree makes a module-cache-shaped tree at path: a file plus a 0555
// directory holding a 0444 file.
func mkTree(t *testing.T, path string) {
	t.Helper()
	// Runs before the enclosing TempDir's removal (cleanups are LIFO), which
	// cannot descend a 0555 directory; covers the tree under any later name.
	t.Cleanup(func() { makeWritable(filepath.Dir(path)) })
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "f"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	ro := filepath.Join(path, "mod")
	if err := os.Mkdir(ro, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ro, "go.mod"), []byte("module x\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(ro, 0o555); err != nil {
		t.Fatal(err)
	}
}

// makeWritable adds owner rwx to every directory under root, best effort.
func makeWritable(root string) {
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(p, 0o700)
		}
		return nil
	})
}

func requireExists(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(path, "f")); err != nil {
		t.Fatalf("%s (or its content) is gone: %v", path, err)
	}
}

func requireGone(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("%s still present: %v", path, err)
	}
}

func mustReap(t *testing.T, r testRoots, cfg reapConfig) reapResult {
	t.Helper()
	res, err := reap(r.tmpFd, r.cacheFd, cfg)
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	checkSums(t, res)
	if res.Truncated {
		t.Fatalf("a pass with budget to spare is truncated: %s", counts(res))
	}
	return res
}

// checkSums asserts the result line's invariants.
func checkSums(t *testing.T, res reapResult) {
	t.Helper()
	if res.Scanned != res.Live+res.Removed+res.Retained+res.Foreign {
		t.Fatalf("scanned %d != sum of %+v", res.Scanned, res)
	}
	if res.Scanned > res.DirentsExamined {
		t.Fatalf("scanned %d > dirents_examined %d", res.Scanned, res.DirentsExamined)
	}
}

// dirEntries lists dir through getdents on a fresh fd, in the order the
// kernel returns them ("." and ".." skipped).
func dirEntries(t *testing.T, dir string) []string {
	t.Helper()
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(fd) }()
	buf := make([]byte, 8192)
	var names []string
	for {
		n, err := unix.Getdents(fd, buf)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if n <= 0 {
			return names
		}
		_, _, names = unix.ParseDirent(buf[:n], -1, names)
	}
}

// snapshotTree renders every entry under root: name, mode, size, mtime and a
// regular file's content.
func snapshotTree(t *testing.T, root string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		fmt.Fprintf(&b, "%s %v %d %d", p, info.Mode(), info.Size(), info.ModTime().UnixNano())
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			fmt.Fprintf(&b, " %q", data)
		}
		b.WriteString("\n")
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot %s: %v", root, err)
	}
	return b.String()
}

// countSeeks counts reapSeek calls per fd.
func countSeeks(t *testing.T) map[int]int {
	t.Helper()
	seeks := map[int]int{}
	reapSeek = func(fd int, offset int64, whence int) (int64, error) {
		seeks[fd]++
		return unix.Seek(fd, offset, whence)
	}
	t.Cleanup(func() { reapSeek = unix.Seek })
	return seeks
}

// lockDir holds LOCK_EX on dir through a separate open file description until
// cleanup.
func lockDir(t *testing.T, dir string) {
	t.Helper()
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Close(fd) })
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
}

func counts(res reapResult) string {
	return fmt.Sprintf("scanned=%d live=%d removed=%d retained=%d foreign=%d proof=%s",
		res.Scanned, res.Live, res.Removed, res.Retained, res.Foreign, res.Proof)
}

// (a) A lock held through another open file description (flock is per-OFD)
// is live: never touched, even with the proof held.
func TestReapLiveLockIsNeverTouched(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	r := newRoots(t)
	tmpDir := filepath.Join(r.tmp, tmpNameN(1))
	cacheDir := filepath.Join(r.cache, tokenN(2))
	for _, d := range []string{tmpDir, cacheDir} {
		mkTree(t, d)
		fd, err := unix.Open(d, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = unix.Close(fd) })
		if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
			t.Fatal(err)
		}
	}
	var calls int
	res := mustReap(t, r, proofConfig(&calls, proofHeld))
	if got := counts(res); got != "scanned=2 live=2 removed=0 retained=0 foreign=0 proof=held" {
		t.Fatalf("reap = %s", got)
	}
	requireExists(t, tmpDir)
	requireExists(t, cacheDir)
}

// (b) The supervisor (the lock holder) is SIGKILLed while its child lives:
// the lock is released, but the child still runs as the command uid, so the
// proof fails and the tmp is retained. Once the child is gone the next pass
// removes it.
func TestReapSupervisorKilledWhileChildLives(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	r := newRoots(t)
	dir := filepath.Join(r.tmp, tmpNameN(3))
	mkTree(t, dir)
	holder, line := startHelper(t, testHelperLock, dir)
	if line != "locked" {
		t.Fatalf("lock helper said %q", line)
	}
	child := spawnUser(t, dir)
	table := realTable(t, holder.cmd.Process.Pid, child.Process.Pid)

	// The holder itself runs as the uid: the first proof is not held, so the
	// candidate is retained without even being locked.
	res := mustReap(t, r, tableConfig(table))
	if got := counts(res); got != "scanned=1 live=0 removed=0 retained=1 foreign=0 proof=user_alive" || res.outcomes[0].reason != "proof" {
		t.Fatalf("with the holder alive: %s %+v", got, res.outcomes)
	}

	holder.kill()
	res = mustReap(t, r, tableConfig(table))
	if got := counts(res); got != "scanned=1 live=0 removed=0 retained=1 foreign=0 proof=user_alive" {
		t.Fatalf("holder dead, child alive: %s", got)
	}
	requireExists(t, dir)

	killWait(child)
	res = mustReap(t, r, tableConfig(table))
	if got := counts(res); got != "scanned=1 live=0 removed=1 retained=0 foreign=0 proof=held" {
		t.Fatalf("child gone: %s", got)
	}
	requireGone(t, dir)
}

// (c) The real --hold-cache holder is SIGKILLed while a command process uses
// the cache: retained until the command process is gone.
func TestReapCacheHolderKilledWhileCommandLives(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	r := newRoots(t)
	uid := fmt.Sprint(os.Geteuid())
	args, err := json.Marshal([]string{modeHoldCache, "--expect-uid", uid, "--cache-root", r.cache, "--cache-token", tokenN(4)})
	if err != nil {
		t.Fatal(err)
	}
	holder, line := startHelper(t, testHelperRunMode, string(args))
	path := filepath.Join(r.cache, tokenN(4))
	if line != `{"event":"cache_ready","path":"`+path+`"}` {
		t.Fatalf("holder said %q", line)
	}
	if err := os.WriteFile(filepath.Join(path, "f"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	command := spawnUser(t, filepath.Join(path, "gomod"))
	table := realTable(t, holder.cmd.Process.Pid, command.Process.Pid)

	res := mustReap(t, r, tableConfig(table))
	if res.Retained != 1 || res.Removed != 0 || res.Proof != proofUserAlive {
		t.Fatalf("holder alive: %s", counts(res))
	}
	// Only the holder's own lock protects it once the proof is held: with the
	// command gone and the holder alive, it is live.
	killWait(command)
	res = mustReap(t, r, tableConfig(realTable(t)))
	if got := counts(res); got != "scanned=1 live=1 removed=0 retained=0 foreign=0 proof=held" {
		t.Fatalf("holder alive, filtered out of the proof: %s", got)
	}
	command = spawnUser(t, filepath.Join(path, "gomod"))
	table = realTable(t, holder.cmd.Process.Pid, command.Process.Pid)
	holder.kill()
	res = mustReap(t, r, tableConfig(table))
	if res.Retained != 1 || res.Removed != 0 || res.Proof != proofUserAlive {
		t.Fatalf("holder dead, command alive: %s", counts(res))
	}
	requireExists(t, path)

	killWait(command)
	res = mustReap(t, r, tableConfig(table))
	if res.Removed != 1 || res.Proof != proofHeld {
		t.Fatalf("command gone: %s", counts(res))
	}
	requireGone(t, path)
}

// (d) Restart: a fresh pass with a surviving same-uid process retains an
// orphan; a fresh pass once every spawned process is gone removes it.
func TestReapRestart(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	r := newRoots(t)
	tmpDir := filepath.Join(r.tmp, tmpNameN(5))
	cacheDir := filepath.Join(r.cache, tokenN(6))
	mkTree(t, tmpDir)
	mkTree(t, cacheDir)
	survivor := spawnUser(t, r.tmp)
	table := realTable(t, survivor.Process.Pid)

	res := mustReap(t, r, tableConfig(table))
	if got := counts(res); got != "scanned=2 live=0 removed=0 retained=2 foreign=0 proof=user_alive" {
		t.Fatalf("with a survivor: %s", got)
	}
	requireExists(t, tmpDir)
	requireExists(t, cacheDir)

	killWait(survivor)
	res = mustReap(t, r, tableConfig(table))
	if got := counts(res); got != "scanned=2 live=0 removed=2 retained=0 foreign=0 proof=held" {
		t.Fatalf("after restart: %s", got)
	}
	requireGone(t, tmpDir)
	requireGone(t, cacheDir)
}

// (e) An unknown proof fails closed, and so does a lock error that is not
// EWOULDBLOCK.
func TestReapUnknownFailsClosed(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	self := os.Getpid()
	selfStatus := statusText(1, uids4(os.Geteuid()))
	fake := func(t *testing.T, superOpts string, statuses map[int]string) procTable {
		statuses[self] = selfStatus
		return procFS{root: fakeProcRoot(t, self, superOpts, statuses), self: self}
	}
	for name, mk := range map[string]func(t *testing.T) procTable{
		"hidepid": func(t *testing.T) procTable { return fake(t, "rw,hidepid=invisible", map[int]string{}) },
		"subset":  func(t *testing.T) procTable { return fake(t, "rw,subset=pid", map[int]string{}) },
		"unparsable status": func(t *testing.T) procTable {
			return fake(t, "rw", map[int]string{9: "Name:\tx\nUid:\t1\t1\n"})
		},
		"scan bound": func(t *testing.T) procTable {
			tbl := fake(t, "rw", map[int]string{9: selfStatus, 10: selfStatus}).(procFS)
			tbl.maxPids = 2
			return tbl
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRoots(t)
			dir := filepath.Join(r.tmp, tmpNameN(7))
			mkTree(t, dir)
			res := mustReap(t, r, tableConfig(mk(t)))
			if got := counts(res); got != "scanned=1 live=0 removed=0 retained=1 foreign=0 proof=unknown" {
				t.Fatalf("reap = %s", got)
			}
			requireExists(t, dir)
		})
	}

	t.Run("flock error", func(t *testing.T) {
		r := newRoots(t)
		dir := filepath.Join(r.tmp, tmpNameN(8))
		mkTree(t, dir)
		setFlock(t, func(int, int) error { return unix.ENOLCK })
		var calls int
		res := mustReap(t, r, proofConfig(&calls, proofHeld))
		if got := counts(res); got != "scanned=1 live=0 removed=0 retained=1 foreign=0 proof=held" {
			t.Fatalf("reap = %s", got)
		}
		if res.outcomes[0].reason != "lock" {
			t.Fatalf("outcome = %+v", res.outcomes[0])
		}
		requireExists(t, dir)
	})
}

// The proof is re-taken with the candidate's lock held: an initial "held"
// followed by a user appearing retains the candidate.
func TestReapRetakesProofUnderTheLock(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	r := newRoots(t)
	dir := filepath.Join(r.tmp, tmpNameN(9))
	mkTree(t, dir)
	for _, second := range []string{proofUserAlive, proofUnknown} {
		var calls int
		res := mustReap(t, r, proofConfig(&calls, proofHeld, second))
		if calls != 2 || res.Retained != 1 || res.Removed != 0 || res.Proof != second {
			t.Fatalf("initial held, then %s: calls=%d %s", second, calls, counts(res))
		}
		requireExists(t, dir)
	}
	// The lock was released after the pass: a later pass may take it.
	var calls int
	if res := mustReap(t, r, proofConfig(&calls, proofHeld)); res.Removed != 1 {
		t.Fatalf("final pass: %s", counts(res))
	}
	requireGone(t, dir)
}

// (f) A swap between the lock (and proof) and the removal is a mismatch:
// retained, and both directories are left alone.
func TestReapSwapBeforeRemoveIsRetained(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	r := newRoots(t)
	name := tmpNameN(10)
	dir := filepath.Join(r.tmp, name)
	mkTree(t, dir)
	hookBeforeReapRemove = func(parentFd int, n string) {
		if parentFd != r.tmpFd || n != name {
			t.Errorf("hook got %d/%q", parentFd, n)
		}
		if err := os.Rename(dir, dir+".pinned"); err != nil {
			t.Fatal(err)
		}
		mkTree(t, dir)
	}
	t.Cleanup(func() { hookBeforeReapRemove = nil })
	var calls int
	res := mustReap(t, r, proofConfig(&calls, proofHeld))
	if res.Retained != 1 || res.outcomes[0].reason != "mismatch" {
		t.Fatalf("reap = %s %+v", counts(res), res.outcomes)
	}
	requireExists(t, dir)
	requireExists(t, dir+".pinned")
}

// (g) Names that do not match exactly are never candidates; a symlink, a
// non-directory and a foreign owner are "foreign" and never touched.
func TestReapSkipsNonCandidatesAndForeign(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	r := newRoots(t)
	upper := strings.ToUpper(tokenN(11))
	ignored := []string{
		filepath.Join(r.tmp, "unrelated"),
		filepath.Join(r.tmp, commandTmpNamePrefix+upper),
		filepath.Join(r.tmp, commandTmpNamePrefix+tokenN(11)+"x"),
		filepath.Join(r.tmp, commandTmpNamePrefix+tokenN(11)[:35]),
		filepath.Join(r.tmp, "x"+tmpNameN(11)),
		filepath.Join(r.tmp, tokenN(11)), // a bare uuid is a cache name, not a tmp name
		filepath.Join(r.cache, tmpNameN(11)),
		filepath.Join(r.cache, upper),
		filepath.Join(r.cache, strings.ReplaceAll(tokenN(11), "-", "_")),
	}
	for _, p := range ignored {
		mkTree(t, p)
	}
	target := filepath.Join(t.TempDir(), "target")
	mkTree(t, target)
	if err := os.Symlink(target, filepath.Join(r.tmp, tmpNameN(12))); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(r.cache, tokenN(13))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.cache, tokenN(14)), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	var calls int
	res := mustReap(t, r, proofConfig(&calls, proofHeld))
	if got := counts(res); got != "scanned=3 live=0 removed=0 retained=0 foreign=3 proof=held" {
		t.Fatalf("reap = %s %+v", got, res.outcomes)
	}
	for _, p := range append(ignored, target) {
		requireExists(t, p)
	}

	// A foreign owner, through the pin seam.
	owned := filepath.Join(r.tmp, tmpNameN(15))
	mkTree(t, owned)
	pinFromFd = func(int, int) (safetree.Pin, error) { return safetree.Pin{}, safetree.ErrOwner }
	t.Cleanup(func() { pinFromFd = safetree.PinFromFd })
	res = mustReap(t, r, proofConfig(&calls, proofHeld))
	if res.Foreign != 4 || res.Removed != 0 {
		t.Fatalf("foreign owner: %s", counts(res))
	}
	requireExists(t, owned)
}

// (h) One pass reads each root to its end in pages and batches: far more
// candidates than one batch holds are all removed, in both roots, by ONE pass.
func TestReapHandlesManyBatchesInOnePass(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	r := newRoots(t)
	const nTmp = 3*maxReapCandidates + 6
	const nCache = maxReapCandidates + 5
	for i := range nTmp {
		if err := os.Mkdir(filepath.Join(r.tmp, tmpNameN(100+i)), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for i := range nCache {
		if err := os.Mkdir(filepath.Join(r.cache, tokenN(1000+i)), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// A clock advancing 1 ms per call: the pass stays well inside its budget.
	start := time.Now()
	calls := 0
	cfg := reapConfig{uid: os.Geteuid(), prove: func() string { return proofHeld }, now: func() time.Time {
		calls++
		return start.Add(time.Duration(calls) * time.Millisecond)
	}}
	res := mustReap(t, r, cfg)
	if last := start.Add(time.Duration(calls) * time.Millisecond); !last.Before(start.Add(reapPassBudget)) {
		t.Fatalf("the test clock reached the pass deadline after %d calls", calls)
	}
	if got, want := counts(res), fmt.Sprintf("scanned=%d live=0 removed=%d retained=0 foreign=0 proof=held", nTmp+nCache, nTmp+nCache); got != want {
		t.Fatalf("reap = %s, want %s", got, want)
	}
	if res.DirentsExamined != nTmp+nCache {
		t.Fatalf("dirents_examined = %d, want %d", res.DirentsExamined, nTmp+nCache)
	}
	if left := len(dirEntries(t, r.tmp)) + len(dirEntries(t, r.cache)); left != 0 {
		t.Fatalf("%d entries left after one pass", left)
	}
}

// A candidate past the first maxReapDirents entries of a root is reached by
// the same pass.
func TestReapReachesBeyondTheFirstPage(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	r := newRoots(t)
	if err := os.Chmod(r.tmp, 0o1777); err != nil {
		t.Fatal(err)
	}
	// Twice a page of fillers, with candidates made before and after them: a
	// creation-ordered directory (tmpfs) lists the early ones last, and a
	// hash-ordered one puts about half of the later ones past the first page;
	// more are made (up to 32) until one is there.
	const fillers = 2 * maxReapDirents
	var cands []string
	mk := func(n int) {
		name := tmpNameN(n)
		mkTree(t, filepath.Join(r.tmp, name))
		cands = append(cands, name)
	}
	for i := range 3 {
		mk(2000 + i)
	}
	for i := range fillers {
		if err := os.WriteFile(filepath.Join(r.tmp, fmt.Sprintf("filler-%05d", i)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var entries []string
	late := 0
	for i := 0; i < 32 && (i < 3 || late == 0); i++ {
		mk(3000 + i)
		entries = dirEntries(t, r.tmp)
		late = 0
		for pos, name := range entries {
			if pos >= maxReapDirents && isCommandTmpName(name) {
				late++
			}
		}
	}
	if late == 0 {
		t.Fatalf("invalid fixture: no candidate at position >= %d of %d entries", maxReapDirents, len(entries))
	}
	cacheCand := filepath.Join(r.cache, tokenN(4000))
	mkTree(t, cacheCand)
	if err := os.WriteFile(filepath.Join(r.cache, "not-a-token"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	cacheEntries := len(dirEntries(t, r.cache))

	var calls int
	res := mustReap(t, r, proofConfig(&calls, proofHeld))
	if got, want := counts(res), fmt.Sprintf("scanned=%d live=0 removed=%d retained=0 foreign=0 proof=held", len(cands)+1, len(cands)+1); got != want {
		t.Fatalf("reap = %s, want %s (%d candidates past the first page)", got, want, late)
	}
	if res.DirentsExamined != len(entries)+cacheEntries {
		t.Fatalf("dirents_examined = %d, want %d", res.DirentsExamined, len(entries)+cacheEntries)
	}
	for _, c := range cands {
		requireGone(t, filepath.Join(r.tmp, c))
	}
	requireGone(t, cacheCand)
	if left := dirEntries(t, r.tmp); len(left) != fillers {
		t.Fatalf("%d entries left in tmp, want the %d fillers", len(left), fillers)
	}
	for i := range fillers {
		if _, err := os.Lstat(filepath.Join(r.tmp, fmt.Sprintf("filler-%05d", i))); err != nil {
			t.Fatalf("filler %d: %v", i, err)
		}
	}
}

// Names that pin as foreign are counted and left alone, and many of them
// (more than a batch holds) cannot starve the real candidates.
func TestReapForeignDoesNotConsumeTheCap(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	r := newRoots(t)
	target := filepath.Join(t.TempDir(), "target")
	mkTree(t, target)
	const foreign = 3 * maxReapCandidates
	for i := range foreign {
		if err := os.Symlink(target, filepath.Join(r.tmp, tmpNameN(500+i))); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 5 {
		if err := os.Mkdir(filepath.Join(r.tmp, tmpNameN(900+i)), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	var calls int
	res := mustReap(t, r, proofConfig(&calls, proofHeld))
	if got := counts(res); got != fmt.Sprintf("scanned=%d live=0 removed=5 retained=0 foreign=%d proof=held", foreign+5, foreign) {
		t.Fatalf("reap = %s", got)
	}
	requireExists(t, target)
}

// When the first proof is not held nothing is locked at all: every candidate
// that pins as ours is retained ("proof") without a flock, so a concurrent
// setup's LOCK_NB is never refused because of the reaper.
func TestReapWithoutAHeldProofNeverLocks(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	for _, first := range []string{proofUserAlive, proofUnknown} {
		r := newRoots(t)
		tmpDir := filepath.Join(r.tmp, tmpNameN(40))
		cacheDir := filepath.Join(r.cache, tokenN(41))
		mkTree(t, tmpDir)
		mkTree(t, cacheDir)
		// A held lock: still not "live", because it is never tried.
		fd, err := unix.Open(cacheDir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		if err != nil {
			t.Fatal(err)
		}
		if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
			t.Fatal(err)
		}
		var flocks int
		setFlock(t, func(fd int, how int) error { flocks++; return unix.Flock(fd, how) })
		var calls int
		res := mustReap(t, r, proofConfig(&calls, first))
		_ = unix.Close(fd)
		want := "scanned=2 live=0 removed=0 retained=2 foreign=0 proof=" + first
		if got := counts(res); got != want || flocks != 0 || calls != 1 {
			t.Fatalf("first proof %s: %s (flocks %d, proofs %d), want %s with no flock", first, got, flocks, calls, want)
		}
		for _, o := range res.outcomes {
			if o.reason != "proof" {
				t.Fatalf("outcome %+v, want reason proof", o)
			}
		}
		requireExists(t, tmpDir)
		requireExists(t, cacheDir)
	}
}

// pagingFixture is a tmp and cache root for small-bounds paging: candidates
// mixed with non-matching names, one tmp candidate held live by a lock.
type pagingFixture struct {
	r        testRoots
	live     string   // the locked tmp candidate's path
	cands    []string // every other candidate's path
	entries  int      // entries of both roots
	tmpOrder []string // tmp names in getdents order
}

func newPagingFixture(t *testing.T) pagingFixture {
	t.Helper()
	f := pagingFixture{r: newRoots(t)}
	for i := range 7 {
		p := filepath.Join(f.r.tmp, tmpNameN(60+i))
		mkTree(t, p)
		f.cands = append(f.cands, p)
	}
	for i := range 5 {
		if err := os.WriteFile(filepath.Join(f.r.tmp, fmt.Sprintf("other-%d", i)), []byte("o"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	f.live = filepath.Join(f.r.tmp, tmpNameN(70))
	mkTree(t, f.live)
	lockDir(t, f.live)
	for i := range 3 {
		p := filepath.Join(f.r.cache, tokenN(80+i))
		mkTree(t, p)
		f.cands = append(f.cands, p)
	}
	mkTree(t, filepath.Join(f.r.cache, "not-a-token"))
	f.tmpOrder = dirEntries(t, f.r.tmp)
	f.entries = len(f.tmpOrder) + len(dirEntries(t, f.r.cache))
	return f
}

// Small pages and batches: every candidate is acted on exactly once, each root
// is rewound once only, a live one is left byte for byte, and every deletion
// had its own fresh proof under the lock.
func TestReapPagesWithSmallBounds(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	f := newPagingFixture(t)
	before := snapshotTree(t, f.live)
	seeks := countSeeks(t)
	var calls int
	res, err := reapWith(f.r.tmpFd, f.r.cacheFd, proofConfig(&calls, proofHeld), reapBounds{pageDirents: 3, batchCandidates: 2})
	if err != nil {
		t.Fatal(err)
	}
	checkSums(t, res)
	want := fmt.Sprintf("scanned=%d live=1 removed=%d retained=0 foreign=0 proof=held", len(f.cands)+1, len(f.cands))
	if got := counts(res); got != want || res.Truncated {
		t.Fatalf("reap = %s truncated=%v, want %s", got, res.Truncated, want)
	}
	if res.DirentsExamined != f.entries {
		t.Fatalf("dirents_examined = %d, want %d", res.DirentsExamined, f.entries)
	}
	seen := map[string]bool{}
	for _, o := range res.outcomes {
		if seen[o.name] {
			t.Fatalf("%s recorded twice: %+v", o.name, res.outcomes)
		}
		seen[o.name] = true
	}
	if len(seeks) != 2 || seeks[f.r.tmpFd] != 1 || seeks[f.r.cacheFd] != 1 {
		t.Fatalf("seeks per fd = %v, want exactly one per root", seeks)
	}
	if calls != 1+len(f.cands) {
		t.Fatalf("proofs = %d, want 1 + %d locked candidates", calls, len(f.cands))
	}
	for _, p := range f.cands {
		requireGone(t, p)
	}
	if after := snapshotTree(t, f.live); after != before {
		t.Fatalf("live candidate changed:\nbefore %s\nafter  %s", before, after)
	}
}

// A fresh proof that is not held for a candidate on a later page retains that
// candidate untouched; the rest are still removed.
func TestReapPagesRetainOnALaterPageProof(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	f := newPagingFixture(t)
	bounds := reapBounds{pageDirents: 3, batchCandidates: 2}
	// The first page ends after pageDirents entries or batchCandidates
	// matches; the target is the first removable candidate after it.
	firstPage, matched := 0, 0
	for firstPage < len(f.tmpOrder) && firstPage < bounds.pageDirents && matched < bounds.batchCandidates {
		if isCommandTmpName(f.tmpOrder[firstPage]) {
			matched++
		}
		firstPage++
	}
	target := ""
	for _, name := range f.tmpOrder[firstPage:] {
		if p := filepath.Join(f.r.tmp, name); isCommandTmpName(name) && p != f.live {
			target = p
			break
		}
	}
	if target == "" {
		t.Fatalf("invalid fixture: no removable candidate past the first page of %v", f.tmpOrder)
	}
	var targetStat unix.Stat_t
	if err := unix.Stat(target, &targetStat); err != nil {
		t.Fatal(err)
	}
	before := snapshotTree(t, target)
	// The pin seam marks which candidate the next fresh proof is for.
	current := false
	pinFromFd = func(fd, uid int) (safetree.Pin, error) {
		var st unix.Stat_t
		current = unix.Fstat(fd, &st) == nil && st.Dev == targetStat.Dev && st.Ino == targetStat.Ino
		return safetree.PinFromFd(fd, uid)
	}
	t.Cleanup(func() { pinFromFd = safetree.PinFromFd })
	calls := 0
	cfg := reapConfig{uid: os.Geteuid(), now: time.Now, prove: func() string {
		calls++
		if current && calls > 1 {
			return proofUnknown
		}
		return proofHeld
	}}
	res, err := reapWith(f.r.tmpFd, f.r.cacheFd, cfg, bounds)
	if err != nil {
		t.Fatal(err)
	}
	checkSums(t, res)
	if res.Live != 1 || res.Retained != 1 || res.Removed != len(f.cands)-1 || res.Truncated {
		t.Fatalf("reap = %s truncated=%v", counts(res), res.Truncated)
	}
	for _, o := range res.outcomes {
		if o.state == reapRetained && (filepath.Join(f.r.tmp, o.name) != target || o.reason != "proof") {
			t.Fatalf("retained %+v, want %s for proof", o, target)
		}
	}
	if after := snapshotTree(t, target); after != before {
		t.Fatalf("retained candidate changed:\nbefore %s\nafter  %s", before, after)
	}
	for _, p := range f.cands {
		if p != target {
			requireGone(t, p)
		}
	}
}

// The pass budget runs out before the second batch: the pass stops there,
// truncated, and what it did not reach is untouched.
func TestReapTruncatesBetweenBatches(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	r := newRoots(t)
	var cands []string
	for i := range 5 {
		p := filepath.Join(r.tmp, tmpNameN(110+i))
		mkTree(t, p)
		cands = append(cands, p)
	}
	for i := range 4 {
		if err := os.WriteFile(filepath.Join(r.tmp, fmt.Sprintf("other-%d", i)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	entries := len(dirEntries(t, r.tmp))
	start := time.Now()
	removals := 0
	hookBeforeReapRemove = func(int, string) { removals++ }
	t.Cleanup(func() { hookBeforeReapRemove = nil })
	cfg := reapConfig{uid: os.Geteuid(), prove: func() string { return proofHeld }, now: func() time.Time {
		if removals >= 2 {
			return start.Add(reapPassBudget)
		}
		return start
	}}
	res, err := reapWith(r.tmpFd, r.cacheFd, cfg, reapBounds{pageDirents: 3, batchCandidates: 2})
	if err != nil {
		t.Fatal(err)
	}
	checkSums(t, res)
	if got := counts(res); got != "scanned=2 live=0 removed=2 retained=0 foreign=0 proof=held" || !res.Truncated {
		t.Fatalf("reap = %s truncated=%v", got, res.Truncated)
	}
	if res.DirentsExamined >= entries {
		t.Fatalf("dirents_examined = %d, want fewer than %d", res.DirentsExamined, entries)
	}
	left := 0
	for _, p := range cands {
		if _, err := os.Lstat(p); err == nil {
			requireExists(t, p)
			left++
		}
	}
	if left != len(cands)-2 {
		t.Fatalf("%d candidates left, want %d", left, len(cands)-2)
	}
}

// The pass budget runs out once the tmp root is done: the cache root is not
// examined at all.
func TestReapTruncatesBeforeTheCacheRoot(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	r := newRoots(t)
	tmpDir := filepath.Join(r.tmp, tmpNameN(120))
	cacheDir := filepath.Join(r.cache, tokenN(121))
	mkTree(t, tmpDir)
	mkTree(t, cacheDir)
	start := time.Now()
	removed := false
	hookBeforeReapRemove = func(int, string) { removed = true }
	t.Cleanup(func() { hookBeforeReapRemove = nil })
	cfg := reapConfig{uid: os.Geteuid(), prove: func() string { return proofHeld }, now: func() time.Time {
		if removed {
			return start.Add(reapPassBudget)
		}
		return start
	}}
	res, err := reap(r.tmpFd, r.cacheFd, cfg)
	if err != nil {
		t.Fatal(err)
	}
	checkSums(t, res)
	if got := counts(res); got != "scanned=1 live=0 removed=1 retained=0 foreign=0 proof=held" || !res.Truncated || res.DirentsExamined != 1 {
		t.Fatalf("reap = %s truncated=%v dirents_examined=%d", got, res.Truncated, res.DirentsExamined)
	}
	requireGone(t, tmpDir)
	requireExists(t, cacheDir)
}

// A pass whose budget is spent retains instead of starting a removal, and is
// truncated.
func TestReapPassBudget(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	r := newRoots(t)
	dir := filepath.Join(r.tmp, tmpNameN(20))
	mkTree(t, dir)
	start := time.Now()
	// The pass start, the page check and the pre-candidate check are in
	// budget; the check inside reapOne, after the fresh proof, is not.
	clock := []time.Time{start, start, start, start.Add(reapPassBudget)}
	cfg := reapConfig{uid: os.Geteuid(), prove: func() string { return proofHeld }, now: func() time.Time {
		now := clock[0]
		if len(clock) > 1 {
			clock = clock[1:]
		}
		return now
	}}
	res, err := reap(r.tmpFd, r.cacheFd, cfg)
	if err != nil {
		t.Fatal(err)
	}
	checkSums(t, res)
	if res.Retained != 1 || res.outcomes[0].reason != "deadline" || !res.Truncated {
		t.Fatalf("reap = %s truncated=%v %+v", counts(res), res.Truncated, res.outcomes)
	}
	requireExists(t, dir)
}

// A getdents failure mid-traversal is an error; a name it had not yet
// returned is untouched, and what was done before stays recorded.
func TestReapListErrorMidTraversal(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	setup := func(t *testing.T) (testRoots, []string, map[string]bool) {
		r := newRoots(t)
		var cands []string
		for i := range 6 {
			name := tmpNameN(130 + i)
			mkTree(t, filepath.Join(r.tmp, name))
			cands = append(cands, name)
		}
		// The first call returns only what fits 200 bytes (at least one
		// candidate); the second fails.
		returned := map[string]bool{}
		calls := 0
		reapGetdents = func(fd int, buf []byte) (int, error) {
			calls++
			if calls == 2 {
				return 0, unix.EIO
			}
			n, err := unix.Getdents(fd, buf[:200])
			if n > 0 {
				_, _, names := unix.ParseDirent(buf[:n], -1, nil)
				for _, name := range names {
					returned[name] = true
				}
			}
			return n, err
		}
		t.Cleanup(func() { reapGetdents = unix.Getdents })
		return r, cands, returned
	}
	checkUntouched := func(t *testing.T, r testRoots, cands []string, returned map[string]bool) {
		t.Helper()
		notReturned := 0
		for _, c := range cands {
			if !returned[c] {
				notReturned++
				requireExists(t, filepath.Join(r.tmp, c))
			}
		}
		if notReturned == 0 {
			t.Fatalf("invalid fixture: the first getdents returned every candidate")
		}
	}

	t.Run("reap", func(t *testing.T) {
		r, cands, returned := setup(t)
		var calls int
		res, err := reapWith(r.tmpFd, r.cacheFd, proofConfig(&calls, proofHeld), reapBounds{pageDirents: maxReapDirents, batchCandidates: 1})
		if !errors.Is(err, unix.EIO) {
			t.Fatalf("reap error = %v, want EIO", err)
		}
		if len(returned) == 0 || res.Removed != len(returned) || res.Scanned != res.Removed {
			t.Fatalf("reap = %s, want the %d names returned before the failure removed", counts(res), len(returned))
		}
		checkUntouched(t, r, cands, returned)
	})
	t.Run("runMode", func(t *testing.T) {
		r, cands, returned := setup(t)
		uid := fmt.Sprint(os.Geteuid())
		selfOnly := staticTable{list: []procUserRow{{pid: os.Getpid(), uids: uids4(os.Geteuid())}}}
		var out bytes.Buffer
		code := runMode([]string{modeReapOrphans, "--expect-uid", uid, "--cache-root", r.cache}, testModeEnv(&out, r.tmp, selfOnly))
		if want := `{"event":"reap_error","reason":"list"}` + "\n"; code != 2 || out.String() != want {
			t.Fatalf("code %d, line %q, want %q", code, out.String(), want)
		}
		checkUntouched(t, r, cands, returned)
	})
}

// testModeEnv is a modeEnv for in-process runs.
func testModeEnv(stdout io.Writer, tmpDir string, table procTable) modeEnv {
	uid := os.Geteuid()
	return modeEnv{
		stdin:       strings.NewReader(""),
		stdout:      stdout,
		hygiene:     func() error { return nil },
		nondumpable: func() bool { return true },
		uids:        func() (int, int) { return uid, uid },
		tmpDir:      tmpDir,
		procs:       table,
		selfPid:     os.Getpid(),
		now:         time.Now,
	}
}

// The --reap-orphans CLI: its one line, its exit codes, a missing cache root.
func TestRunModeReapOrphans(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	uid := fmt.Sprint(os.Geteuid())
	selfOnly := staticTable{list: []procUserRow{{pid: os.Getpid(), uids: uids4(os.Geteuid())}}}

	r := newRoots(t)
	mkTree(t, filepath.Join(r.tmp, tmpNameN(30)))
	mkTree(t, filepath.Join(r.cache, tokenN(31)))
	examined := len(dirEntries(t, r.tmp)) + len(dirEntries(t, r.cache))
	var out bytes.Buffer
	code := runMode([]string{modeReapOrphans, "--expect-uid", uid, "--cache-root", r.cache}, testModeEnv(&out, r.tmp, selfOnly))
	if want := fmt.Sprintf(`{"event":"reap","scanned":2,"live":0,"removed":2,"retained":0,"foreign":0,"proof":"held","truncated":false,"dirents_examined":%d}`, examined) + "\n"; code != 0 || out.String() != want {
		t.Fatalf("reap: code %d, line %q, want %q", code, out.String(), want)
	}

	// A missing cache root is skipped; the tmp is still reaped.
	mkTree(t, filepath.Join(r.tmp, tmpNameN(32)))
	examined = len(dirEntries(t, r.tmp))
	out.Reset()
	code = runMode([]string{modeReapOrphans, "--expect-uid", uid, "--cache-root", filepath.Join(r.cache, "absent")}, testModeEnv(&out, r.tmp, selfOnly))
	if want := fmt.Sprintf(`{"event":"reap","scanned":1,"live":0,"removed":1,"retained":0,"foreign":0,"proof":"held","truncated":false,"dirents_examined":%d}`, examined) + "\n"; code != 0 || out.String() != want {
		t.Fatalf("missing cache root: code %d, line %q", code, out.String())
	}

	if err := os.Chmod(r.cache, 0o750); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		args   []string
		env    func(modeEnv) modeEnv
		reason string
	}{
		"bad args":   {[]string{modeReapOrphans, "--expect-uid", uid}, nil, "args"},
		"uid":        {[]string{modeReapOrphans, "--expect-uid", uid + "0", "--cache-root", r.cache}, nil, "uid"},
		"cache root": {[]string{modeReapOrphans, "--expect-uid", uid, "--cache-root", r.cache}, nil, "cache_root"},
		"tmp root":   {[]string{modeReapOrphans, "--expect-uid", uid, "--cache-root", r.cache}, func(e modeEnv) modeEnv { e.tmpDir = "/nonexistent-uzi-tmp"; return e }, "tmp_root"},
		"fd hygiene": {[]string{modeReapOrphans, "--expect-uid", uid, "--cache-root", r.cache}, func(e modeEnv) modeEnv { e.hygiene = func() error { return errors.New("x") }; return e }, "fd_hygiene"},
		"euid differs": {[]string{modeReapOrphans, "--expect-uid", uid, "--cache-root", r.cache}, func(e modeEnv) modeEnv {
			e.uids = func() (int, int) { return os.Geteuid(), os.Geteuid() + 1 }
			return e
		}, "uid"},
	} {
		out.Reset()
		env := testModeEnv(&out, r.tmp, selfOnly)
		if tc.env != nil {
			env = tc.env(env)
		}
		code := runMode(tc.args, env)
		if want := `{"event":"reap_error","reason":"` + tc.reason + `"}` + "\n"; code != 2 || out.String() != want {
			t.Errorf("%s: code %d, line %q, want %q", name, code, out.String(), want)
		}
	}
}

// Through realMain: a mode flag first runs the mode (its own stdout line), not
// the supervisor, which would write nothing to stdout. Each case fails in the
// strict argv parser, before any fd, root or uid is touched.
func TestRealMainDispatchesModes(t *testing.T) {
	for _, tc := range []struct {
		args []string
		line string
	}{
		{[]string{modeRemoveCache, "--expect-uid"}, `{"event":"cache_error","reason":"args"}`},
		{[]string{modeHoldCache, "--expect-uid", "x"}, `{"event":"cache_error","reason":"args"}`},
		{[]string{modeReapOrphans, "--expect-uid"}, `{"event":"reap_error","reason":"args"}`},
	} {
		argv, err := json.Marshal(tc.args)
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(os.Args[0], "-test.run=^$")
		cmd.Env = append(os.Environ(), testHelperEnv+"="+testHelperRealMain, testHelperArgEnv+"="+string(argv))
		var stdout bytes.Buffer
		cmd.Stdout = &stdout
		err = cmd.Run()
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 2 {
			t.Errorf("realMain(%v): %v, want exit 2", tc.args, err)
		}
		if got := stdout.String(); got != tc.line+"\n" {
			t.Errorf("realMain(%v) stdout = %q, want %q", tc.args, got, tc.line+"\n")
		}
	}
}

func TestParseModeArgs(t *testing.T) {
	root := "/var/cache/uzi-codex-cmd"
	a, err := parseModeArgs([]string{modeReapOrphans, "--expect-uid", "10003", "--cache-root", root})
	if err != nil || a != (modeArgs{mode: modeReapOrphans, expectUID: 10003, cacheRoot: root}) {
		t.Fatalf("reap args = %+v %v", a, err)
	}
	for _, mode := range []string{modeHoldCache, modeRemoveCache} {
		a, err := parseModeArgs([]string{mode, "--expect-uid", "10003", "--cache-root", root, "--cache-token", testToken})
		if err != nil || a != (modeArgs{mode: mode, expectUID: 10003, cacheRoot: root, token: testToken}) {
			t.Fatalf("%s args = %+v %v", mode, a, err)
		}
	}
	tok := []string{"--cache-token", testToken}
	for name, args := range map[string][]string{
		"empty":             {},
		"not a mode":        {"--expect-uid", "10003", modeReapOrphans},
		"reap with token":   append([]string{modeReapOrphans, "--expect-uid", "10003", "--cache-root", root}, tok...),
		"hold without tok":  {modeHoldCache, "--expect-uid", "10003", "--cache-root", root},
		"swapped order":     {modeReapOrphans, "--cache-root", root, "--expect-uid", "10003"},
		"relative root":     {modeReapOrphans, "--expect-uid", "10003", "--cache-root", "var/cache"},
		"unclean root":      {modeReapOrphans, "--expect-uid", "10003", "--cache-root", root + "/"},
		"dotdot root":       {modeReapOrphans, "--expect-uid", "10003", "--cache-root", root + "/../etc"},
		"slash root":        {modeReapOrphans, "--expect-uid", "10003", "--cache-root", "/"},
		"negative uid":      {modeReapOrphans, "--expect-uid", "-1", "--cache-root", root},
		"padded uid":        {modeReapOrphans, "--expect-uid", "010003", "--cache-root", root},
		"bad token":         {modeHoldCache, "--expect-uid", "10003", "--cache-root", root, "--cache-token", "../x"},
		"upper token":       {modeHoldCache, "--expect-uid", "10003", "--cache-root", root, "--cache-token", strings.ToUpper(testToken)},
		"extra arg":         append([]string{modeRemoveCache, "--expect-uid", "10003", "--cache-root", root}, append(tok, "--")...),
		"two modes":         {modeReapOrphans, modeHoldCache, "--expect-uid", "10003", "--cache-root", root},
		"token flag misnam": {modeHoldCache, "--expect-uid", "10003", "--cache-root", root, "--cleanup-token", testToken},
	} {
		if _, err := parseModeArgs(args); err == nil {
			t.Errorf("%s: %v accepted", name, args)
		}
	}
	// The supervisor grammar is unchanged and never takes a mode flag.
	for _, args := range [][]string{
		{"--expect-uid", "10003", modeReapOrphans, "--", "/bin/true"},
		{modeHoldCache, "--expect-uid", "10003", "--", "/bin/true"},
	} {
		if _, _, _, _, err := parseArgs(args); err == nil {
			t.Errorf("supervisor parseArgs accepted %v", args)
		}
	}
	if _, _, _, child, err := parseArgs([]string{"--expect-uid", "10003", "--", "/bin/x", modeReapOrphans}); err != nil || child[1] != modeReapOrphans {
		t.Fatalf("a mode flag after -- is the child's: %v %v", child, err)
	}
}
