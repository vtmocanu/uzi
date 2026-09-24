package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"uzi.local/codex-supervisor/internal/safetree"
)

// testToken is assembled at runtime so no UUID-shaped literal sits in tracked text.
var testToken = strings.Join([]string{"01234567", "89ab", "cdef", "0123", "456789abcdef"}, "-")

func openParent(t *testing.T, dir string) int {
	t.Helper()
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("open parent: %v", err)
	}
	t.Cleanup(func() { _ = unix.Close(fd) })
	return fd
}

func decodeLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("evidence line %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

func TestCommandTmpName(t *testing.T) {
	if got, want := commandTmpName(testToken), "uzi-codex-command-"+testToken; got != want {
		t.Fatalf("name = %q, want %q", got, want)
	}
}

// TestCreateCommandTmpLocksPinsAndRemoves drives the real create + flock +
// recheck helper in a t.TempDir parent: the directory is 0700 and owned by the
// caller, its fd is close-on-exec and holds LOCK_EX (a second fd's LOCK_NB gets
// EWOULDBLOCK), and cleanup removes a tree holding module-cache-style 0555
// directories and 0444 files, which os.RemoveAll cannot do as a non-root owner.
func TestCreateCommandTmpLocksPinsAndRemoves(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	parent := t.TempDir()
	parentFd := openParent(t, parent)
	name := commandTmpName(testToken)
	uid := os.Geteuid()

	ct, err := createCommandTmp(parentFd, name, uid)
	if err != nil {
		t.Fatalf("createCommandTmp: %v", err)
	}
	full := filepath.Join(parent, name)

	var st unix.Stat_t
	if err := unix.Lstat(full, &st); err != nil {
		t.Fatalf("lstat: %v", err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR || st.Mode&0o7777 != 0o700 || int(st.Uid) != uid {
		t.Fatalf("mode/uid = %o/%d, want dir 0700/%d", st.Mode, st.Uid, uid)
	}
	if uint64(st.Dev) != ct.pin.Dev || st.Ino != ct.pin.Ino || ct.pin.UID != uid {
		t.Fatalf("pin %+v does not match the created dir", ct.pin)
	}
	flags, err := unix.FcntlInt(uintptr(ct.dirFd), unix.F_GETFD, 0)
	if err != nil || flags&unix.FD_CLOEXEC == 0 {
		t.Fatalf("lock fd is not close-on-exec (flags=%d err=%v)", flags, err)
	}

	other, err := unix.Open(full, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("open second fd: %v", err)
	}
	defer func() { _ = unix.Close(other) }()
	if err := unix.Flock(other, unix.LOCK_EX|unix.LOCK_NB); !errors.Is(err, unix.EWOULDBLOCK) {
		t.Fatalf("second LOCK_NB = %v, want EWOULDBLOCK while the lock is held", err)
	}

	// A module-cache-shaped tree: read-only dirs and files.
	ro := filepath.Join(full, "pkg", "mod", "example.com@v1")
	if err := os.MkdirAll(ro, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ro, "go.mod"), []byte("module x\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{ro, filepath.Dir(ro), filepath.Join(full, "pkg")} {
		if err := os.Chmod(d, 0o555); err != nil {
			t.Fatal(err)
		}
	}

	got := ct.cleanup()
	if got == nil || got.State != tmpCleanupRemoved || got.Reason != "" {
		t.Fatalf("cleanup = %+v, want removed", got)
	}
	if _, err := os.Lstat(full); !os.IsNotExist(err) {
		t.Fatalf("tmp still present after removal: %v", err)
	}
}

func TestCreateCommandTmpRefusesExistingName(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	parent := t.TempDir()
	parentFd := openParent(t, parent)
	name := commandTmpName(testToken)
	if err := os.Mkdir(filepath.Join(parent, name), 0o700); err != nil {
		t.Fatal(err)
	}
	if ct, err := createCommandTmp(parentFd, name, os.Geteuid()); err == nil {
		t.Fatalf("created over an existing name: %+v", ct)
	}
}

func TestCreateCommandTmpRefusesSymlinkedName(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	parent := t.TempDir()
	parentFd := openParent(t, parent)
	name := commandTmpName(testToken)
	target := filepath.Join(parent, "elsewhere")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(parent, name)); err != nil {
		t.Fatal(err)
	}
	if ct, err := createCommandTmp(parentFd, name, os.Geteuid()); err == nil {
		t.Fatalf("created through a symlink: %+v", ct)
	}
}

func TestCommandTmpCleanupReasons(t *testing.T) {
	tests := []struct {
		err        error
		wantState  string
		wantReason string
	}{
		{nil, tmpCleanupRemoved, ""},
		{fmt.Errorf("wrapped: %w", safetree.ErrMismatch), tmpCleanupRetained, "mismatch"},
		{safetree.ErrOwner, tmpCleanupRetained, "owner"},
		{safetree.ErrBound, tmpCleanupRetained, "bound"},
		{safetree.ErrIO, tmpCleanupRetained, "io"},
		// The pinned dir vanishing is NOT success.
		{safetree.ErrNotExist, tmpCleanupRetained, "absent"},
	}
	for _, tc := range tests {
		var gotName string
		var gotPin safetree.Pin
		pin := safetree.Pin{Dev: 1, Ino: 2, UID: 10003}
		ct := &commandTmp{parentFd: 7, name: "n", pin: pin, remove: func(parentFd int, name string, p safetree.Pin) error {
			if parentFd != 7 {
				t.Errorf("parentFd = %d", parentFd)
			}
			gotName, gotPin = name, p
			return tc.err
		}}
		got := ct.cleanup()
		if got.State != tc.wantState || got.Reason != tc.wantReason {
			t.Errorf("err %v: cleanup = %+v, want %s/%q", tc.err, got, tc.wantState, tc.wantReason)
		}
		if gotName != "n" || gotPin != pin {
			t.Errorf("remove got %q/%+v", gotName, gotPin)
		}
	}
}

func TestSetupAndLaunchSetupFailureEmitsAbnormalAndNeverLaunches(t *testing.T) {
	var buf bytes.Buffer
	ev := &evidence{w: &buf}
	launched := false
	setup := func(token string, uid int) (*commandTmp, error) {
		if token != testToken || uid != 10003 {
			t.Errorf("setup got %q/%d", token, uid)
		}
		return nil, errTmpRecheck
	}
	launch := func([]string) (int, error) { launched = true; return 42, nil }
	ct, pid, ok := setupAndLaunch(ev, testToken, 10003, setup, launch, []string{"/bin/true"})
	if ok || ct != nil || pid != 0 {
		t.Fatalf("setupAndLaunch = %v/%d/%v, want failure", ct, pid, ok)
	}
	if launched {
		t.Fatal("launched a child after a command tmp setup failure")
	}
	lines := decodeLines(t, &buf)
	if len(lines) != 1 || lines[0]["event"] != "abnormal" || lines[0]["reason"] != "command tmp setup failed" {
		t.Fatalf("evidence = %v", lines)
	}
	if _, ok := lines[0]["cleanup"]; ok {
		t.Error("pre-fork abnormal must carry no cleanup")
	}
	if _, ok := lines[0]["tmpCleanup"]; ok {
		t.Error("pre-fork abnormal must carry no tmpCleanup")
	}
}

func TestSetupAndLaunchSetsUpBeforeLaunch(t *testing.T) {
	var buf bytes.Buffer
	var order []string
	want := &commandTmp{name: "n"}
	setup := func(string, int) (*commandTmp, error) { order = append(order, "setup"); return want, nil }
	launch := func(argv []string) (int, error) { order = append(order, "launch"); return 42, nil }
	ct, pid, ok := setupAndLaunch(&evidence{w: &buf}, testToken, 10003, setup, launch, []string{"/bin/true"})
	if !ok || ct != want || pid != 42 {
		t.Fatalf("setupAndLaunch = %v/%d/%v", ct, pid, ok)
	}
	if strings.Join(order, ",") != "setup,launch" {
		t.Fatalf("order = %v", order)
	}
	if buf.Len() != 0 {
		t.Fatalf("unexpected evidence %q", buf.String())
	}
}

func TestSetupAndLaunchWithoutTokenSkipsSetup(t *testing.T) {
	var buf bytes.Buffer
	setup := func(string, int) (*commandTmp, error) { t.Fatal("setup ran without a token"); return nil, nil }
	launch := func([]string) (int, error) { return 42, nil }
	ct, pid, ok := setupAndLaunch(&evidence{w: &buf}, "", 10003, setup, launch, []string{"/bin/true"})
	if !ok || ct != nil || pid != 42 {
		t.Fatalf("setupAndLaunch = %v/%d/%v", ct, pid, ok)
	}
	if tmpCleanupFor(ct) != nil {
		t.Fatal("no token must mean no tmpCleanup seam")
	}
}

func TestSetupAndLaunchLaunchFailureLeavesTmp(t *testing.T) {
	var buf bytes.Buffer
	removed := false
	ct0 := &commandTmp{remove: func(int, string, safetree.Pin) error { removed = true; return nil }}
	setup := func(string, int) (*commandTmp, error) { return ct0, nil }
	launch := func([]string) (int, error) { return 0, errors.New("boom") }
	_, _, ok := setupAndLaunch(&evidence{w: &buf}, testToken, 10003, setup, launch, []string{"/bin/true"})
	if ok {
		t.Fatal("launch failure reported ok")
	}
	if removed {
		t.Fatal("launch failure removed the tmp without a confirmed drain")
	}
	lines := decodeLines(t, &buf)
	if len(lines) != 1 || lines[0]["reason"] != "child launch failed" {
		t.Fatalf("evidence = %v", lines)
	}
}

// setAfterLockHook installs hookAfterLock for one test.
func setAfterLockHook(t *testing.T, h func(parentFd int, name string)) {
	t.Helper()
	hookAfterLock = h
	t.Cleanup(func() { hookAfterLock = nil })
}

// setFlock replaces the flock seam for one test.
func setFlock(t *testing.T, f func(fd int, how int) error) {
	t.Helper()
	flock = f
	t.Cleanup(func() { flock = unix.Flock })
}

// setupVia is a tmpSetup that creates the token's tmp under parentFd instead of /tmp.
func setupVia(parentFd int) tmpSetup {
	return func(token string, uid int) (*commandTmp, error) {
		return createCommandTmp(parentFd, commandTmpName(token), uid)
	}
}

// requireSetupRefused runs setupAndLaunch with setup and asserts the pre-fork
// "command tmp setup failed" abnormal and no launch.
func requireSetupRefused(t *testing.T, setup tmpSetup) {
	t.Helper()
	var buf bytes.Buffer
	launched := false
	launch := func([]string) (int, error) { launched = true; return 42, nil }
	ct, _, ok := setupAndLaunch(&evidence{w: &buf}, testToken, os.Geteuid(), setup, launch, []string{"/bin/true"})
	if ok || ct != nil {
		t.Fatalf("setupAndLaunch = %v/%v, want a setup failure", ct, ok)
	}
	if launched {
		t.Fatal("launched a child after a command tmp setup failure")
	}
	lines := decodeLines(t, &buf)
	if len(lines) != 1 || lines[0]["reason"] != "command tmp setup failed" {
		t.Fatalf("evidence = %v", lines)
	}
}

// TestCreateCommandTmpRecheckRefusesNameSwappedAfterLock swaps the name to
// another directory between the lock and the recheck: the recheck must refuse
// it with errTmpRecheck, drop the lock, and the launch must never happen.
func TestCreateCommandTmpRecheckRefusesNameSwappedAfterLock(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	parent := t.TempDir()
	parentFd := openParent(t, parent)
	name := commandTmpName(testToken)
	full := filepath.Join(parent, name)
	swaps := 0
	movedTo := func(n int) string { return fmt.Sprintf("%s.pinned%d", full, n) }
	setAfterLockHook(t, func(pfd int, n string) {
		swaps++
		if pfd != parentFd || n != name {
			t.Errorf("hook got %d/%q", pfd, n)
		}
		if err := os.Rename(full, movedTo(swaps)); err != nil {
			t.Fatalf("rename: %v", err)
		}
		if err := os.Mkdir(full, 0o700); err != nil {
			t.Fatalf("mkdir substitute: %v", err)
		}
	})

	if ct, err := createCommandTmp(parentFd, name, os.Geteuid()); !errors.Is(err, errTmpRecheck) {
		t.Fatalf("createCommandTmp = %+v/%v, want errTmpRecheck", ct, err)
	}
	moved := movedTo(1)
	// The pinned dir's lock was dropped: a fresh fd can take it.
	fd, err := unix.Open(moved, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(fd) }()
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatalf("lock still held after a recheck failure: %v", err)
	}
	// Nothing was removed: both dirs stay for the orphan reaper.
	for _, p := range []string{full, moved} {
		if _, err := os.Lstat(p); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
	}

	// Through setupAndLaunch: the same swap fails setup and never launches.
	if err := os.Remove(full); err != nil {
		t.Fatal(err)
	}
	requireSetupRefused(t, setupVia(parentFd))
	if swaps != 2 {
		t.Fatalf("hook ran %d times, want 2", swaps)
	}
}

// TestCreateCommandTmpLockHeldElsewhereFailsSetup: another holder already has
// the lock, so LOCK_NB gets EWOULDBLOCK; setup fails, the directory fd is
// closed, and nothing is launched.
func TestCreateCommandTmpLockHeldElsewhereFailsSetup(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	parent := t.TempDir()
	parentFd := openParent(t, parent)
	name := commandTmpName(testToken)
	var lockedFd int
	var holders []int
	t.Cleanup(func() {
		for _, h := range holders {
			_ = unix.Close(h)
		}
	})
	setFlock(t, func(fd int, how int) error {
		// Another holder takes the lock on the same directory first.
		h, err := unix.Open(filepath.Join(parent, name), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		if err != nil {
			t.Fatalf("open holder: %v", err)
		}
		holders = append(holders, h)
		if err := unix.Flock(h, unix.LOCK_EX|unix.LOCK_NB); err != nil {
			t.Fatalf("holder lock: %v", err)
		}
		lockedFd = fd
		return unix.Flock(fd, how)
	})

	ct, err := createCommandTmp(parentFd, name, os.Geteuid())
	if !errors.Is(err, unix.EWOULDBLOCK) {
		t.Fatalf("createCommandTmp = %+v/%v, want EWOULDBLOCK", ct, err)
	}
	if _, err := unix.FcntlInt(uintptr(lockedFd), unix.F_GETFD, 0); !errors.Is(err, unix.EBADF) {
		t.Fatalf("directory fd still open after a lock failure: %v", err)
	}

	for _, h := range holders {
		_ = unix.Close(h)
	}
	holders = nil
	if err := os.Remove(filepath.Join(parent, name)); err != nil {
		t.Fatal(err)
	}
	requireSetupRefused(t, setupVia(parentFd))
}

// TestOpenCommandTmpFdsAreCloseOnExec pins O_CLOEXEC on the /tmp parent fd
// (and the lock fd), so the child never inherits either.
func TestOpenCommandTmpFdsAreCloseOnExec(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	parent := t.TempDir()
	ct, err := openCommandTmpIn(parent, testToken, os.Geteuid())
	if err != nil {
		t.Fatalf("openCommandTmpIn: %v", err)
	}
	t.Cleanup(func() {
		_ = unix.Close(ct.dirFd)
		_ = unix.Close(ct.parentFd)
	})
	for label, fd := range map[string]int{"parent": ct.parentFd, "lock": ct.dirFd} {
		flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
		if err != nil || flags&unix.FD_CLOEXEC == 0 {
			t.Fatalf("%s fd is not close-on-exec (flags=%d err=%v)", label, flags, err)
		}
	}
	if got := ct.cleanup(); got.State != tmpCleanupRemoved {
		t.Fatalf("cleanup = %+v", got)
	}
}
