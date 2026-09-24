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
// plus this process and its ancestor chain, so that unrelated same-uid
// processes on the host do not decide the proof.
type filteredTable struct {
	keep map[int]bool
}

func (f filteredTable) rows() ([]procUserRow, error) {
	all, err := procFS{root: procRootPath, self: os.Getpid()}.rows()
	if err != nil {
		return nil, err
	}
	parent := map[int]int{}
	for _, r := range all {
		parent[r.pid] = r.ppid
	}
	chain := map[int]bool{}
	for pid := os.Getpid(); pid > 0 && !chain[pid]; {
		chain[pid] = true
		pp, ok := parent[pid]
		if !ok {
			break
		}
		pid = pp
	}
	var out []procUserRow
	for _, r := range all {
		if f.keep[r.pid] || chain[r.pid] {
			out = append(out, r)
		}
	}
	return out, nil
}

// realTable returns a filteredTable over pids, skipping where the real scan
// cannot run (a hiding proc mount).
func realTable(t *testing.T, pids ...int) procTable {
	t.Helper()
	if _, err := (procFS{root: procRootPath, self: os.Getpid()}).rows(); err != nil {
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
	if res.Scanned != res.Live+res.Removed+res.Retained+res.Foreign {
		t.Fatalf("scanned %d != sum of %+v", res.Scanned, res)
	}
	return res
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

	// Held: live, whatever the proof.
	res := mustReap(t, r, tableConfig(table))
	if res.Live != 1 || res.Proof != proofUserAlive {
		t.Fatalf("with the holder alive: %s", counts(res))
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
	if res.Live != 1 || res.Removed != 0 {
		t.Fatalf("holder alive: %s", counts(res))
	}
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

// (h) One pass acts on at most maxReapCandidates; the next pass continues.
func TestReapBoundsCandidatesPerPass(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	r := newRoots(t)
	const n = maxReapCandidates + 6
	for i := range n {
		parent := r.tmp
		name := tmpNameN(100 + i)
		if i%2 == 1 {
			parent, name = r.cache, tokenN(100+i)
		}
		if err := os.Mkdir(filepath.Join(parent, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	var calls int
	res := mustReap(t, r, proofConfig(&calls, proofHeld))
	if res.Scanned != maxReapCandidates || res.Removed != maxReapCandidates {
		t.Fatalf("first pass: %s", counts(res))
	}
	res = mustReap(t, r, proofConfig(&calls, proofHeld))
	if res.Scanned != 6 || res.Removed != 6 {
		t.Fatalf("second pass: %s", counts(res))
	}
}

// The dirent bound: a name past the first maxDirents entries is not seen.
func TestListCandidatesDirentBound(t *testing.T) {
	dir := t.TempDir()
	for i := range 10 {
		if err := os.Mkdir(filepath.Join(dir, tmpNameN(i)), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	fd := openParent(t, dir)
	names, err := listCandidates(fd, isCommandTmpName, 4)
	if err != nil || len(names) != 4 {
		t.Fatalf("bounded listing = %v %v", names, err)
	}
	names, err = listCandidates(fd, isCommandTmpName, maxReapDirents)
	if err != nil || len(names) != 10 {
		t.Fatalf("full listing (from offset 0 again) = %v %v", names, err)
	}
}

// A pass whose budget is spent retains instead of starting a removal.
func TestReapPassBudget(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	r := newRoots(t)
	dir := filepath.Join(r.tmp, tmpNameN(20))
	mkTree(t, dir)
	start := time.Now()
	clock := []time.Time{start, start.Add(reapPassBudget)}
	cfg := reapConfig{uid: os.Geteuid(), prove: func() string { return proofHeld }, now: func() time.Time {
		now := clock[0]
		if len(clock) > 1 {
			clock = clock[1:]
		}
		return now
	}}
	res := mustReap(t, r, cfg)
	if res.Retained != 1 || res.outcomes[0].reason != "deadline" {
		t.Fatalf("reap = %s %+v", counts(res), res.outcomes)
	}
	requireExists(t, dir)
}

// testModeEnv is a modeEnv for in-process runs.
func testModeEnv(stdout io.Writer, tmpDir string, table procTable) modeEnv {
	uid := os.Geteuid()
	return modeEnv{
		stdin:   strings.NewReader(""),
		stdout:  stdout,
		hygiene: func() error { return nil },
		uids:    func() (int, int) { return uid, uid },
		tmpDir:  tmpDir,
		procs:   table,
		selfPid: os.Getpid(),
		now:     time.Now,
	}
}

// The --reap-orphans CLI: its one line, its exit codes, a missing cache root.
func TestRunModeReapOrphans(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	uid := fmt.Sprint(os.Geteuid())
	selfOnly := staticTable{list: []procUserRow{{pid: os.Getpid(), ppid: 1, uids: uids4(os.Geteuid())}}}

	r := newRoots(t)
	mkTree(t, filepath.Join(r.tmp, tmpNameN(30)))
	mkTree(t, filepath.Join(r.cache, tokenN(31)))
	var out bytes.Buffer
	code := runMode([]string{modeReapOrphans, "--expect-uid", uid, "--cache-root", r.cache}, testModeEnv(&out, r.tmp, selfOnly))
	if want := `{"event":"reap","scanned":2,"live":0,"removed":2,"retained":0,"foreign":0,"proof":"held"}` + "\n"; code != 0 || out.String() != want {
		t.Fatalf("reap: code %d, line %q, want %q", code, out.String(), want)
	}

	// A missing cache root is skipped; the tmp is still reaped.
	mkTree(t, filepath.Join(r.tmp, tmpNameN(32)))
	out.Reset()
	code = runMode([]string{modeReapOrphans, "--expect-uid", uid, "--cache-root", filepath.Join(r.cache, "absent")}, testModeEnv(&out, r.tmp, selfOnly))
	if want := `{"event":"reap","scanned":1,"live":0,"removed":1,"retained":0,"foreign":0,"proof":"held"}` + "\n"; code != 0 || out.String() != want {
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

// Through realMain: a mode flag first runs the mode, with no fd 3/4.
func TestRealMainDispatchesModes(t *testing.T) {
	// No "--expect-uid" value: the strict parser refuses before anything opens.
	if code := realMain([]string{modeRemoveCache, "--expect-uid"}); code != 2 {
		t.Fatalf("realMain(--remove-cache …) = %d, want 2", code)
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
