package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"uzi.local/codex-supervisor/internal/safetree"
)

// runMeasure runs --measure-cache through runMode and returns its output line
// (without the newline) and exit code.
func runMeasure(t *testing.T, root string, edit func(*modeEnv)) (string, int) {
	t.Helper()
	var out bytes.Buffer
	env := testModeEnv(&out, t.TempDir(), nil)
	if edit != nil {
		edit(&env)
	}
	code := runMode([]string{modeMeasureCache, "--expect-uid", fmt.Sprint(os.Geteuid()), "--cache-root", root, "--cache-token", testToken}, env)
	if !strings.HasSuffix(out.String(), "\n") || strings.Count(out.String(), "\n") != 1 {
		t.Fatalf("output is not exactly one line: %q", out.String())
	}
	return strings.TrimSuffix(out.String(), "\n"), code
}

// lstatTotal charges every entry below dir max(size, blocks*512), counted
// independently of the measurer.
func lstatTotal(t *testing.T, dir string) (bytes, entries uint64) {
	t.Helper()
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == dir {
			return nil
		}
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		st := fi.Sys().(*syscall.Stat_t)
		c := uint64(st.Size)
		if b := uint64(st.Blocks) * 512; b > c {
			c = b
		}
		bytes += c
		entries++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return bytes, entries
}

func measureLine(state string, bytes, entries uint64, reason string) string {
	return fmt.Sprintf(`{"event":"cache_measure","state":"%s","bytes":%d,"entries":%d,"reason":"%s"}`, state, bytes, entries, reason)
}

func TestMeasureCacheMode(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	root := newCacheRoot(t)
	dir := filepath.Join(root, testToken)

	// Absent token: incomplete, exit 3.
	got, code := runMeasure(t, root, nil)
	if got != measureLine("incomplete", 0, 0, "absent") || code != 3 {
		t.Fatalf("absent: %q exit %d", got, code)
	}

	// A module-cache-shaped tree (a 0555 directory, a 0444 file) is complete.
	mkTree(t, dir)
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	wantBytes, wantEntries := lstatTotal(t, dir)
	got, code = runMeasure(t, root, nil)
	if got != measureLine("complete", wantBytes, wantEntries, "") || code != 0 || wantEntries != 3 {
		t.Fatalf("complete: %q exit %d, want %d bytes in %d entries", got, code, wantBytes, wantEntries)
	}

	// The line carries exactly these five keys, in both states.
	for _, line := range []string{got, measureLine("incomplete", 0, 0, "absent")} {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatal(err)
		}
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if want := []string{"bytes", "entries", "event", "reason", "state"}; !reflect.DeepEqual(keys, want) {
			t.Fatalf("keys = %v, want %v", keys, want)
		}
	}
	var decoded cacheMeasureLine
	if err := json.Unmarshal([]byte(got), &decoded); err != nil || decoded.Event != "cache_measure" {
		t.Fatalf("decode: %+v %v", decoded, err)
	}

	// A symlink at the token is not followed: mismatch.
	if err := os.Rename(dir, dir+".real"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(dir+".real", dir); err != nil {
		t.Fatal(err)
	}
	got, code = runMeasure(t, root, nil)
	if got != measureLine("incomplete", 0, 0, "mismatch") || code != 3 {
		t.Fatalf("symlinked token: %q exit %d", got, code)
	}
}

func TestMeasureCacheModeSetupErrors(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	uid := fmt.Sprint(os.Geteuid())
	wantErr := func(name, reason, got string, code int) {
		t.Helper()
		if want := `{"event":"cache_error","reason":"` + reason + `"}`; got != want || code != 2 {
			t.Errorf("%s: %q exit %d, want %q exit 2", name, got, code, want)
		}
	}

	root := newCacheRoot(t)
	mkTree(t, filepath.Join(root, testToken))

	got, code := runMeasure(t, root, func(e *modeEnv) { e.uids = func() (int, int) { return 1, 1 } })
	wantErr("uid", "uid", got, code)

	got, code = runMeasure(t, root, func(e *modeEnv) { e.hygiene = func() error { return errors.New("x") } })
	wantErr("fd hygiene", "fd_hygiene", got, code)

	// The nondumpable step runs first, as for --hold-cache.
	got, code = runMeasure(t, root, func(e *modeEnv) {
		e.nondumpable = func() bool { return false }
		e.hygiene = func() error { t.Error("hygiene ran after a nondumpable failure"); return nil }
	})
	wantErr("dumpable", "dumpable", got, code)

	wrongMode := newCacheRoot(t)
	if err := os.Chmod(wrongMode, 0o755); err != nil {
		t.Fatal(err)
	}
	got, code = runMeasure(t, wrongMode, nil)
	wantErr("wrong root mode", "root", got, code)
	got, code = runMeasure(t, filepath.Join(t.TempDir(), "absent"), nil)
	wantErr("missing root", "root", got, code)

	for name, args := range map[string][]string{
		"no token":      {modeMeasureCache, "--expect-uid", uid, "--cache-root", root},
		"bad token":     {modeMeasureCache, "--expect-uid", uid, "--cache-root", root, "--cache-token", "../x"},
		"upper token":   {modeMeasureCache, "--expect-uid", uid, "--cache-root", root, "--cache-token", strings.ToUpper(testToken)},
		"wrong order":   {modeMeasureCache, "--cache-root", root, "--expect-uid", uid, "--cache-token", testToken},
		"token flag":    {modeMeasureCache, "--expect-uid", uid, "--cache-root", root, "--token", testToken},
		"extra arg":     {modeMeasureCache, "--expect-uid", uid, "--cache-root", root, "--cache-token", testToken, "x"},
		"relative root": {modeMeasureCache, "--expect-uid", uid, "--cache-root", "rel", "--cache-token", testToken},
		"signed uid":    {modeMeasureCache, "--expect-uid", "+" + uid, "--cache-root", root, "--cache-token", testToken},
	} {
		var out bytes.Buffer
		code := runMode(args, testModeEnv(&out, t.TempDir(), nil))
		wantErr(name, "args", strings.TrimSuffix(out.String(), "\n"), code)
	}
}

type entryState struct {
	mode  uint32
	uid   uint32
	size  int64
	mtime syscall.Timespec
	ctime syscall.Timespec
	ino   uint64
}

func snapshot(t *testing.T, root string) map[string]entryState {
	t.Helper()
	snap := map[string]entryState{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		st := fi.Sys().(*syscall.Stat_t)
		snap[p] = entryState{st.Mode, st.Uid, st.Size, st.Mtim, st.Ctim, st.Ino}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

func TestMeasureCacheModeIsNonMutating(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	root := newCacheRoot(t)
	dir := filepath.Join(root, testToken)
	mkTree(t, dir)
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Siblings the measurement must not touch, one of them a 0555 module tree.
	mkTree(t, filepath.Join(root, tokenN(1)))
	if err := os.Mkdir(filepath.Join(root, "unrelated"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "unrelated", "keep"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A held lock must not matter: the mode takes no flock.
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(fd) }()
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}

	before := snapshot(t, root)
	got, code := runMeasure(t, root, nil)
	if code != 0 || !strings.Contains(got, `"state":"complete"`) {
		t.Fatalf("measure under a held lock: %q exit %d", got, code)
	}
	after := snapshot(t, root)
	if !reflect.DeepEqual(before, after) {
		for p, b := range before {
			if a := after[p]; a != b {
				t.Errorf("%s changed: %+v -> %+v", p, b, a)
			}
		}
		t.Fatalf("the tree changed (before %d entries, after %d)", len(before), len(after))
	}
	// The lock is still ours alone: another fd cannot take it.
	other, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(other) }()
	if err := unix.Flock(other, unix.LOCK_EX|unix.LOCK_NB); !errors.Is(err, unix.EWOULDBLOCK) {
		t.Fatalf("lock state changed: %v", err)
	}
}

func TestMeasureCacheCompiledInLimits(t *testing.T) {
	if measureMaxEntries != 500_000 || measureMaxDepth != 64 || measureWall != 5*time.Second {
		t.Fatalf("limits = %d, %d, %v; doc.go states 500000, 64, 5s", measureMaxEntries, measureMaxDepth, measureWall)
	}
}

func TestMeasureCacheLimitsAndDeadline(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	root := newCacheRoot(t)
	dir := filepath.Join(root, testToken)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for i := range 6 {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprint("f", i)), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	deep := filepath.Join(dir, "a", "b")
	if err := os.MkdirAll(deep, 0o700); err != nil {
		t.Fatal(err)
	}
	rootFd, err := openCacheRoot(root, os.Geteuid())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(rootFd) }()

	run := func(lim safetree.Limits) (cacheMeasureLine, int) {
		var out bytes.Buffer
		code := measureCacheLimits(rootFd, testToken, os.Geteuid(), &out, lim)
		var l cacheMeasureLine
		if err := json.Unmarshal(out.Bytes(), &l); err != nil {
			t.Fatalf("%q: %v", out.String(), err)
		}
		return l, code
	}

	l, code := run(safetree.Limits{MaxEntries: 3, MaxDepth: 64})
	if l.State != "incomplete" || l.Reason != "entries" || l.Entries != 3 || l.Bytes == 0 || code != 3 {
		t.Fatalf("entries: %+v exit %d", l, code)
	}
	l, code = run(safetree.Limits{MaxEntries: 100, MaxDepth: 1})
	if l.State != "incomplete" || l.Reason != "depth" || code != 3 {
		t.Fatalf("depth: %+v exit %d", l, code)
	}
	l, code = run(safetree.Limits{MaxEntries: 100, MaxDepth: 64, Deadline: time.Now().Add(-time.Second)})
	if l.State != "incomplete" || l.Reason != "deadline" || code != 3 {
		t.Fatalf("deadline: %+v exit %d", l, code)
	}
	l, code = run(safetree.Limits{MaxEntries: 8, MaxDepth: 2})
	if l.State != "complete" || l.Entries != 8 || l.Reason != "" || code != 0 {
		t.Fatalf("at the caps: %+v exit %d", l, code)
	}

	// measureCache derives its deadline from the injected clock plus the wall
	// budget: a clock far in the past gives an already-expired deadline.
	var out bytes.Buffer
	code = measureCache(rootFd, testToken, os.Geteuid(), &out, func() time.Time { return time.Unix(0, 0) })
	if got := strings.TrimSuffix(out.String(), "\n"); got != measureLine("incomplete", 0, 0, "deadline") || code != 3 {
		t.Fatalf("expired wall budget: %q exit %d", got, code)
	}
	out.Reset()
	code = measureCache(rootFd, testToken, os.Geteuid(), &out, time.Now)
	if code != 0 || !strings.Contains(out.String(), `"state":"complete"`) {
		t.Fatalf("compiled-in limits: %q exit %d", out.String(), code)
	}
}
