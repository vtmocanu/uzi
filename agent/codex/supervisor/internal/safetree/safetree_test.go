package safetree

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"golang.org/x/sys/unix"
)

const (
	treeName       = "tree"
	sentinelName   = "sentinel"
	sentinelFile   = "secret.txt"
	sentinelBody   = "sentinel-content"
	moduleCacheRel = "go/pkg/mod/example.com/a@v1"
)

// fixture is a writable base dir, an fd on it (the parentFd), and an outside
// sentinel dir that no removal may ever touch.
type fixture struct {
	base     string
	parentFd int
	sentinel string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	base := t.TempDir()
	// Registered after TempDir, so it runs first and makes every 0555 dir
	// writable again before TempDir's RemoveAll.
	t.Cleanup(func() { makeWritable(base) })
	fd, err := unix.Open(base, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Close(fd) })

	sentinel := filepath.Join(base, sentinelName)
	mustMkdir(t, sentinel)
	mustWrite(t, filepath.Join(sentinel, sentinelFile), sentinelBody)
	mustChmod(t, filepath.Join(sentinel, sentinelFile), 0o444)
	mustChmod(t, sentinel, 0o555)
	return &fixture{base: base, parentFd: fd, sentinel: sentinel}
}

// createTree makes the pinned root via Create and closes its fd.
func (f *fixture) createTree(t *testing.T) Pin {
	t.Helper()
	fd, pin, err := Create(f.parentFd, treeName, os.Geteuid())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	_ = unix.Close(fd)
	return pin
}

func (f *fixture) root() string { return filepath.Join(f.base, treeName) }

type sentinelState struct {
	dirMode  fs.FileMode
	dirUID   uint32
	fileMode fs.FileMode
	fileUID  uint32
	body     string
}

func readSentinel(t *testing.T, dir string) sentinelState {
	t.Helper()
	var s sentinelState
	var st unix.Stat_t
	if err := unix.Lstat(dir, &st); err != nil {
		t.Fatalf("lstat sentinel: %v", err)
	}
	s.dirMode, s.dirUID = fs.FileMode(st.Mode), st.Uid
	file := filepath.Join(dir, sentinelFile)
	if err := unix.Lstat(file, &st); err != nil {
		t.Fatalf("lstat sentinel file: %v", err)
	}
	s.fileMode, s.fileUID = fs.FileMode(st.Mode), st.Uid
	body, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read sentinel file: %v", err)
	}
	s.body = string(body)
	return s
}

func assertSentinelUnchanged(t *testing.T, dir string, want sentinelState) {
	t.Helper()
	if got := readSentinel(t, dir); got != want {
		t.Fatalf("sentinel changed: got %+v, want %+v", got, want)
	}
	if want.body != sentinelBody || want.dirMode.Perm() != 0o555 || want.fileMode.Perm() != 0o444 {
		t.Fatalf("sentinel baseline unexpected: %+v", want)
	}
}

// buildModuleCache lays down a Go-module-cache-shaped tree under root with
// nested 0555 dirs and 0444 files, the shape os.RemoveAll cannot delete.
func buildModuleCache(t *testing.T, root string) {
	t.Helper()
	mod := filepath.Join(root, moduleCacheRel)
	dirs := []string{
		mod,
		filepath.Join(mod, "sub"),
		filepath.Join(mod, "sub", "deeper"),
		filepath.Join(mod, "sub", "deeper", "deepest"),
		filepath.Join(mod, "other"),
	}
	for _, d := range dirs {
		mustMkdir(t, d)
	}
	files := []string{
		filepath.Join(mod, "go.mod"),
		filepath.Join(mod, "a.go"),
		filepath.Join(mod, "sub", "sub.go"),
		filepath.Join(mod, "sub", "deeper", "d.go"),
		filepath.Join(mod, "sub", "deeper", "deepest", "x.go"),
		filepath.Join(mod, "other", "o.go"),
	}
	for _, p := range files {
		mustWrite(t, p, "package x\n")
		mustChmod(t, p, 0o444)
	}
	// Read-only bottom-up, so each chmod still has a writable parent path.
	ro := []string{
		filepath.Join(mod, "sub", "deeper", "deepest"),
		filepath.Join(mod, "sub", "deeper"),
		filepath.Join(mod, "sub"),
		filepath.Join(mod, "other"),
		mod,
		filepath.Join(root, "go/pkg/mod/example.com"),
		filepath.Join(root, "go/pkg/mod"),
	}
	for _, d := range ro {
		mustChmod(t, d, 0o555)
	}
}

// makeWritable restores owner rwx on every dir under base (not following
// symlinks) so the test's own TempDir cleanup can delete it.
func makeWritable(base string) {
	_ = filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(p, 0o755)
		}
		return nil
	})
}

func mustMkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, p, body string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustChmod(t *testing.T, p string, mode os.FileMode) {
	t.Helper()
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
}

func assertExists(t *testing.T, p string) {
	t.Helper()
	if _, err := os.Lstat(p); err != nil {
		t.Fatalf("expected %s to remain: %v", p, err)
	}
}

func assertGone(t *testing.T, p string) {
	t.Helper()
	if _, err := os.Lstat(p); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("expected %s to be gone, lstat err=%v", p, err)
	}
}

func setStatOpenHook(t *testing.T, h func(dirfd int, name string)) {
	t.Helper()
	prev := hookBetweenStatAndOpen
	hookBetweenStatAndOpen = h
	t.Cleanup(func() { hookBetweenStatAndOpen = prev })
}

func setRootRecheckHook(t *testing.T, h func(parentFd int, name string)) {
	t.Helper()
	prev := hookBeforeRootRecheck
	hookBeforeRootRecheck = h
	t.Cleanup(func() { hookBeforeRootRecheck = prev })
}

// setForeignOwner makes the fstatat seam report a foreign uid for one name.
func setForeignOwner(t *testing.T, victim string) {
	t.Helper()
	prev := fstatat
	fstatat = func(dirfd int, path string, st *unix.Stat_t, flags int) error {
		if err := prev(dirfd, path, st, flags); err != nil {
			return err
		}
		if path == victim {
			st.Uid++
		}
		return nil
	}
	t.Cleanup(func() { fstatat = prev })
}

func setBounds(t *testing.T, depth, entries int) {
	t.Helper()
	pd, pe := maxDepth, maxEntries
	maxDepth, maxEntries = depth, entries
	t.Cleanup(func() { maxDepth, maxEntries = pd, pe })
}

// swapHook fires once, on the entry named target: it renames the verified dir
// away and puts replace(dirfd, target) in its place.
func swapHook(t *testing.T, target string, replace func(dirfd int, name string) error) *bool {
	t.Helper()
	fired := false
	setStatOpenHook(t, func(dirfd int, name string) {
		if fired || name != target {
			return
		}
		fired = true
		if err := unix.Renameat(dirfd, name, dirfd, name+".moved"); err != nil {
			t.Errorf("swap rename: %v", err)
			return
		}
		if err := replace(dirfd, name); err != nil {
			t.Errorf("swap replace: %v", err)
		}
	})
	return &fired
}

func TestRemoveReadOnlyModuleCacheTree(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	f := newFixture(t)
	pin := f.createTree(t)
	buildModuleCache(t, f.root())

	if err := Remove(f.parentFd, treeName, pin); err != nil {
		t.Fatalf("Remove: %v (reason %q)", err, Reason(err))
	}
	assertGone(t, f.root())
}

func TestOSRemoveAllLeavesReadOnlyTree(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	f := newFixture(t)
	f.createTree(t)
	buildModuleCache(t, f.root())

	if err := os.RemoveAll(f.root()); err == nil {
		t.Fatalf("os.RemoveAll unexpectedly removed a read-only module cache as uid %d", os.Geteuid())
	}
	assertExists(t, filepath.Join(f.root(), moduleCacheRel, "sub", "deeper", "deepest", "x.go"))
}

func TestRemoveUnlinksStableSymlinkWithoutFollowing(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	f := newFixture(t)
	before := readSentinel(t, f.sentinel)
	pin := f.createTree(t)
	mustMkdir(t, filepath.Join(f.root(), "d"))
	if err := os.Symlink(f.sentinel, filepath.Join(f.root(), "d", "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(f.sentinel, sentinelFile), filepath.Join(f.root(), "filelink")); err != nil {
		t.Fatal(err)
	}
	mustChmod(t, filepath.Join(f.root(), "d"), 0o555)

	if err := Remove(f.parentFd, treeName, pin); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	assertGone(t, f.root())
	assertSentinelUnchanged(t, f.sentinel, before)
}

func TestRemoveFailsClosedOnDescendantSwapToSymlink(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	f := newFixture(t)
	before := readSentinel(t, f.sentinel)
	pin := f.createTree(t)
	buildModuleCache(t, f.root())
	fired := swapHook(t, "sub", func(dirfd int, name string) error {
		return unix.Symlinkat(f.sentinel, dirfd, name)
	})

	err := Remove(f.parentFd, treeName, pin)
	if !*fired {
		t.Fatal("swap hook never fired")
	}
	if !errors.Is(err, ErrMismatch) || Reason(err) != "mismatch" {
		t.Fatalf("Remove = %v, want ErrMismatch", err)
	}
	// Refused by O_NOFOLLOW at open time, not only by the later inode compare.
	if !errors.Is(err, unix.ELOOP) && !errors.Is(err, unix.ENOTDIR) {
		t.Fatalf("Remove = %v, want the open to fail with ELOOP/ENOTDIR", err)
	}
	assertExists(t, f.root())
	assertExists(t, filepath.Join(f.root(), moduleCacheRel, "sub.moved", "deeper", "d.go"))
	assertSentinelUnchanged(t, f.sentinel, before)
}

func TestRemoveFailsClosedOnDescendantSwapToOtherDir(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	f := newFixture(t)
	pin := f.createTree(t)
	buildModuleCache(t, f.root())
	fired := swapHook(t, "sub", func(dirfd int, name string) error {
		if err := unix.Mkdirat(dirfd, name, 0o700); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(f.root(), moduleCacheRel, name, "planted"), []byte("x"), 0o644)
	})

	err := Remove(f.parentFd, treeName, pin)
	if !*fired {
		t.Fatal("swap hook never fired")
	}
	if !errors.Is(err, ErrMismatch) {
		t.Fatalf("Remove = %v, want ErrMismatch", err)
	}
	assertExists(t, f.root())
	assertExists(t, filepath.Join(f.root(), moduleCacheRel, "sub", "planted"))
	assertExists(t, filepath.Join(f.root(), moduleCacheRel, "sub.moved", "sub.go"))
}

func TestRemoveFailsClosedOnRootSwap(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	// plantDir puts a same-uid dir holding a 0444 file at path.
	plantDir := func(t *testing.T, path string) string {
		t.Helper()
		mustMkdir(t, path)
		file := filepath.Join(path, "keep.txt")
		mustWrite(t, file, "keep")
		mustChmod(t, file, 0o444)
		return file
	}

	t.Run("before remove, dir", func(t *testing.T) {
		f := newFixture(t)
		pin := f.createTree(t)
		buildModuleCache(t, f.root())
		if err := os.Rename(f.root(), f.root()+".orig"); err != nil {
			t.Fatal(err)
		}
		keep := plantDir(t, f.root())

		if err := Remove(f.parentFd, treeName, pin); !errors.Is(err, ErrMismatch) {
			t.Fatalf("Remove = %v, want ErrMismatch", err)
		}
		assertExists(t, keep)
		assertExists(t, filepath.Join(f.root()+".orig", moduleCacheRel, "go.mod"))
	})

	t.Run("before remove, symlink", func(t *testing.T) {
		f := newFixture(t)
		before := readSentinel(t, f.sentinel)
		pin := f.createTree(t)
		if err := os.Rename(f.root(), f.root()+".orig"); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(f.sentinel, f.root()); err != nil {
			t.Fatal(err)
		}

		err := Remove(f.parentFd, treeName, pin)
		if !errors.Is(err, ErrMismatch) || (!errors.Is(err, unix.ELOOP) && !errors.Is(err, unix.ENOTDIR)) {
			t.Fatalf("Remove = %v, want ErrMismatch from ELOOP/ENOTDIR at open", err)
		}
		assertExists(t, f.root())
		assertSentinelUnchanged(t, f.sentinel, before)
	})

	t.Run("at recheck, dir", func(t *testing.T) {
		f := newFixture(t)
		pin := f.createTree(t)
		buildModuleCache(t, f.root())
		var keep string
		setRootRecheckHook(t, func(int, string) {
			if err := os.Rename(f.root(), f.root()+".orig"); err != nil {
				t.Errorf("rename: %v", err)
			}
			keep = plantDir(t, f.root())
		})

		if err := Remove(f.parentFd, treeName, pin); !errors.Is(err, ErrMismatch) {
			t.Fatalf("Remove = %v, want ErrMismatch", err)
		}
		assertExists(t, keep)
		assertExists(t, f.root()+".orig")
	})

	t.Run("at recheck, symlink", func(t *testing.T) {
		f := newFixture(t)
		before := readSentinel(t, f.sentinel)
		pin := f.createTree(t)
		setRootRecheckHook(t, func(int, string) {
			if err := os.Rename(f.root(), f.root()+".orig"); err != nil {
				t.Errorf("rename: %v", err)
			}
			if err := os.Symlink(f.sentinel, f.root()); err != nil {
				t.Errorf("symlink: %v", err)
			}
		})

		if err := Remove(f.parentFd, treeName, pin); !errors.Is(err, ErrMismatch) {
			t.Fatalf("Remove = %v, want ErrMismatch", err)
		}
		assertExists(t, f.root())
		assertSentinelUnchanged(t, f.sentinel, before)
	})
}

func TestRemoveFailsClosedOnForeignOwnedFile(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	f := newFixture(t)
	pin := f.createTree(t)
	buildModuleCache(t, f.root())
	victim := filepath.Join(f.root(), moduleCacheRel, "sub", "deeper", "d.go")
	setForeignOwner(t, "d.go")

	err := Remove(f.parentFd, treeName, pin)
	if !errors.Is(err, ErrOwner) || Reason(err) != "owner" {
		t.Fatalf("Remove = %v (reason %q), want ErrOwner", err, Reason(err))
	}
	assertExists(t, victim)
	assertExists(t, f.root())
}

func TestRemoveFailsClosedOnForeignOwnedDir(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	f := newFixture(t)
	pin := f.createTree(t)
	buildModuleCache(t, f.root())
	setForeignOwner(t, "deeper")

	err := Remove(f.parentFd, treeName, pin)
	if !errors.Is(err, ErrOwner) || Reason(err) != "owner" {
		t.Fatalf("Remove = %v (reason %q), want ErrOwner", err, Reason(err))
	}
	assertExists(t, filepath.Join(f.root(), moduleCacheRel, "sub", "deeper", "deepest", "x.go"))
	assertExists(t, f.root())
}

func TestRemoveReportsBound(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	t.Run("depth", func(t *testing.T) {
		f := newFixture(t)
		pin := f.createTree(t)
		buildModuleCache(t, f.root())
		setBounds(t, 3, 1_000_000)
		err := Remove(f.parentFd, treeName, pin)
		if !errors.Is(err, ErrBound) || Reason(err) != "bound" {
			t.Fatalf("Remove = %v, want ErrBound", err)
		}
		assertExists(t, f.root())
	})
	t.Run("entries", func(t *testing.T) {
		f := newFixture(t)
		pin := f.createTree(t)
		buildModuleCache(t, f.root())
		setBounds(t, 256, 5)
		err := Remove(f.parentFd, treeName, pin)
		if !errors.Is(err, ErrBound) || Reason(err) != "bound" {
			t.Fatalf("Remove = %v, want ErrBound", err)
		}
		assertExists(t, f.root())
	})
}

func TestRemoveAbsentRoot(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	f := newFixture(t)
	err := Remove(f.parentFd, treeName, Pin{UID: os.Geteuid()})
	if !errors.Is(err, ErrNotExist) || Reason(err) != "absent" {
		t.Fatalf("Remove = %v, want ErrNotExist", err)
	}
}

func TestCreatePinsAndLocksable(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	f := newFixture(t)
	old := unix.Umask(0o077)
	fd, pin, err := Create(f.parentFd, treeName, os.Geteuid())
	unix.Umask(old)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = unix.Close(fd) })

	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		t.Fatal(err)
	}
	if st.Mode&0o7777 != 0o700 || !isDir(&st) {
		t.Fatalf("mode = %o, want dir 0700", st.Mode)
	}
	if int(st.Uid) != os.Geteuid() {
		t.Fatalf("owner = %d, want %d", st.Uid, os.Geteuid())
	}
	if want := (Pin{Dev: uint64(st.Dev), Ino: st.Ino, UID: os.Geteuid()}); pin != want {
		t.Fatalf("pin = %+v, want %+v", pin, want)
	}
	if again, err := PinFromFd(fd, os.Geteuid()); err != nil || again != pin {
		t.Fatalf("PinFromFd = %+v, %v; want %+v", again, err, pin)
	}
	if _, err := PinFromFd(fd, os.Geteuid()+1); !errors.Is(err, ErrOwner) {
		t.Fatalf("PinFromFd(foreign uid) = %v, want ErrOwner", err)
	}

	if fd2, _, err := Create(f.parentFd, treeName, os.Geteuid()); err == nil || !errors.Is(err, unix.EEXIST) {
		if err == nil {
			_ = unix.Close(fd2)
		}
		t.Fatalf("second Create = %v, want EEXIST", err)
	}

	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatalf("flock: %v", err)
	}
	other, err := OpenDirNoFollow(f.parentFd, treeName)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Close(other) })
	if err := unix.Flock(other, unix.LOCK_EX|unix.LOCK_NB); !errors.Is(err, unix.EWOULDBLOCK) {
		t.Fatalf("second flock = %v, want EWOULDBLOCK", err)
	}

	if err := Remove(f.parentFd, treeName, pin); err != nil {
		t.Fatalf("Remove of created tree: %v", err)
	}
	assertGone(t, f.root())
}

func TestReasonIsFixedVocabulary(t *testing.T) {
	cases := map[error]string{
		nil:                               "",
		ErrMismatch:                       "mismatch",
		ErrOwner:                          "owner",
		ErrBound:                          "bound",
		ErrNotExist:                       "absent",
		ioErr("openat", unix.EACCES):      "io",
		openErr("openat", unix.ELOOP):     "mismatch",
		openErr("openat", unix.EACCES):    "io",
		ErrName:                           "name",
		errors.New("/some/secret/path x"): "io",
	}
	for err, want := range cases {
		if got := Reason(err); got != want {
			t.Errorf("Reason(%v) = %q, want %q", err, got, want)
		}
	}
	if !errors.Is(ioErr("x", unix.EACCES), unix.EACCES) {
		t.Error("ErrIO must wrap the errno")
	}
	if !errors.Is(openErr("x", unix.ELOOP), unix.ELOOP) {
		t.Error("an open ErrMismatch must wrap the errno")
	}
}

func setCreateHook(t *testing.T, h func(parentFd int, name string)) {
	t.Helper()
	prev := hookCreateBetweenMkdirAndOpen
	hookCreateBetweenMkdirAndOpen = h
	t.Cleanup(func() { hookCreateBetweenMkdirAndOpen = prev })
}

// setFstatat wraps the fstatat seam; wrap receives the real implementation.
func setFstatat(t *testing.T, wrap func(real func(int, string, *unix.Stat_t, int) error, dirfd int, path string, st *unix.Stat_t, flags int) error) {
	t.Helper()
	prev := fstatat
	fstatat = func(dirfd int, path string, st *unix.Stat_t, flags int) error {
		return wrap(prev, dirfd, path, st, flags)
	}
	t.Cleanup(func() { fstatat = prev })
}

// setFstat makes the fstat seam apply edit to every result for inode ino.
func setFstat(t *testing.T, ino uint64, edit func(st *unix.Stat_t)) {
	t.Helper()
	prev := fstat
	fstat = func(fd int, st *unix.Stat_t) error {
		if err := prev(fd, st); err != nil {
			return err
		}
		if st.Ino == ino {
			edit(st)
		}
		return nil
	}
	t.Cleanup(func() { fstat = prev })
}

func inoOf(t *testing.T, p string) uint64 {
	t.Helper()
	var st unix.Stat_t
	if err := unix.Lstat(p, &st); err != nil {
		t.Fatal(err)
	}
	return st.Ino
}

func TestCreateVerifiesThenChmodsExactly0700(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	f := newFixture(t)
	// A setgid parent makes mkdirat's result 02700 even with umask 0, so only
	// Create's fchmod can make it exactly 0700.
	parent := filepath.Join(f.base, "sgid")
	mustMkdir(t, parent)
	mustChmod(t, parent, 0o755|os.ModeSetgid)
	var pst unix.Stat_t
	if err := unix.Stat(parent, &pst); err != nil {
		t.Fatal(err)
	}
	if pst.Mode&unix.S_ISGID == 0 {
		t.Fatalf("precondition: parent mode %o lacks setgid", pst.Mode)
	}
	pfd, err := unix.Open(parent, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Close(pfd) })

	old := unix.Umask(0)
	fd, pin, err := Create(pfd, treeName, os.Geteuid())
	unix.Umask(old)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = unix.Close(fd) })
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		t.Fatal(err)
	}
	if st.Mode&0o7777 != 0o700 {
		t.Fatalf("mode = %o, want exactly 0700", st.Mode&0o7777)
	}
	if err := Remove(pfd, treeName, pin); err != nil {
		t.Fatalf("Remove: %v", err)
	}
}

func TestCreateRejectsSubstitutedNonEmptyDir(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	for _, tc := range []struct {
		name  string
		plant func(t *testing.T, dir string) string
	}{
		{"file", func(t *testing.T, dir string) string {
			p := filepath.Join(dir, "planted")
			mustWrite(t, p, "x")
			return p
		}},
		{"subdir", func(t *testing.T, dir string) string {
			p := filepath.Join(dir, "planted")
			mustMkdir(t, p)
			return p
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			var planted string
			setCreateHook(t, func(parentFd int, name string) {
				// Stand-in for a peer's renameat2(RENAME_EXCHANGE): the fresh dir
				// leaves, a pre-filled one takes its name.
				if err := unix.Renameat(parentFd, name, parentFd, name+".fresh"); err != nil {
					t.Errorf("rename: %v", err)
					return
				}
				mustMkdir(t, f.root())
				planted = tc.plant(t, f.root())
			})
			fd, _, err := Create(f.parentFd, treeName, os.Geteuid())
			if err == nil {
				_ = unix.Close(fd)
			}
			if !errors.Is(err, ErrMismatch) || fd != -1 {
				t.Fatalf("Create = %d, %v; want -1, ErrMismatch", fd, err)
			}
			assertExists(t, planted)
		})
	}
}

func TestNameValidation(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	f := newFixture(t)
	for _, name := range []string{"", ".", "..", "a/b", "../" + sentinelName, "/abs", "a\x00b"} {
		if fd, _, err := Create(f.parentFd, name, os.Geteuid()); !errors.Is(err, ErrName) || Reason(err) != "name" || fd != -1 {
			t.Errorf("Create(%q) = %d, %v; want ErrName", name, fd, err)
		}
		if err := Remove(f.parentFd, name, Pin{UID: os.Geteuid()}); !errors.Is(err, ErrName) || Reason(err) != "name" {
			t.Errorf("Remove(%q) = %v; want ErrName", name, err)
		}
	}
}

func TestRemoveOwnerUnreadableDirs(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	t.Run("descendants", func(t *testing.T) {
		f := newFixture(t)
		before := readSentinel(t, f.sentinel)
		pin := f.createTree(t)
		for _, d := range []struct {
			name string
			mode os.FileMode
		}{{"m0000", 0o000}, {"m0300", 0o300}, {"m0100", 0o100}} {
			dir := filepath.Join(f.root(), d.name)
			mustMkdir(t, filepath.Join(dir, "nested"))
			mustWrite(t, filepath.Join(dir, "nested", "f"), "x")
			mustWrite(t, filepath.Join(dir, "g"), "y")
			mustChmod(t, filepath.Join(dir, "nested"), 0o000)
			mustChmod(t, dir, d.mode)
		}
		if err := Remove(f.parentFd, treeName, pin); err != nil {
			t.Fatalf("Remove: %v (reason %q)", err, Reason(err))
		}
		assertGone(t, f.root())
		assertSentinelUnchanged(t, f.sentinel, before)
	})
	t.Run("root 0000", func(t *testing.T) {
		f := newFixture(t)
		pin := f.createTree(t)
		buildModuleCache(t, f.root())
		mustChmod(t, f.root(), 0o000)
		if err := Remove(f.parentFd, treeName, pin); err != nil {
			t.Fatalf("Remove: %v (reason %q)", err, Reason(err))
		}
		assertGone(t, f.root())
	})
}

func TestRemoveFailsClosedOnOtherFilesystem(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	f := newFixture(t)
	pin := f.createTree(t)
	buildModuleCache(t, f.root())
	deeper := filepath.Join(f.root(), moduleCacheRel, "sub", "deeper")
	ino := inoOf(t, deeper)
	// A consistent lie in both seams, as a real mount point would present: the
	// fstatat and the fstat agree, so only the pin.Dev check can catch it.
	setFstatat(t, func(real func(int, string, *unix.Stat_t, int) error, dirfd int, path string, st *unix.Stat_t, flags int) error {
		if err := real(dirfd, path, st, flags); err != nil {
			return err
		}
		if path == "deeper" {
			st.Dev++
		}
		return nil
	})
	setFstat(t, ino, func(st *unix.Stat_t) { st.Dev++ })

	err := Remove(f.parentFd, treeName, pin)
	if !errors.Is(err, ErrMismatch) {
		t.Fatalf("Remove = %v, want ErrMismatch", err)
	}
	assertExists(t, filepath.Join(deeper, "deepest", "x.go"))
}

func TestRemoveFstatOwnerChecks(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	foreign := func(st *unix.Stat_t) { st.Uid++ }
	t.Run("opened child dir", func(t *testing.T) {
		f := newFixture(t)
		pin := f.createTree(t)
		buildModuleCache(t, f.root())
		deeper := filepath.Join(f.root(), moduleCacheRel, "sub", "deeper")
		setFstat(t, inoOf(t, deeper), foreign)
		if err := Remove(f.parentFd, treeName, pin); !errors.Is(err, ErrOwner) {
			t.Fatalf("Remove = %v, want ErrOwner", err)
		}
		assertExists(t, filepath.Join(deeper, "deepest", "x.go"))
	})
	t.Run("root at first open", func(t *testing.T) {
		f := newFixture(t)
		pin := f.createTree(t)
		buildModuleCache(t, f.root())
		setFstat(t, pin.Ino, foreign)
		if err := Remove(f.parentFd, treeName, pin); !errors.Is(err, ErrOwner) {
			t.Fatalf("Remove = %v, want ErrOwner", err)
		}
		assertExists(t, filepath.Join(f.root(), moduleCacheRel, "go.mod"))
	})
	t.Run("root recheck", func(t *testing.T) {
		f := newFixture(t)
		pin := f.createTree(t)
		buildModuleCache(t, f.root())
		setForeignOwner(t, treeName)
		if err := Remove(f.parentFd, treeName, pin); !errors.Is(err, ErrOwner) {
			t.Fatalf("Remove = %v, want ErrOwner", err)
		}
		assertExists(t, f.root())
	})
}

func TestRemoveFirstErrorAborts(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	f := newFixture(t)
	pin := f.createTree(t)
	const siblings = 64
	for i := range siblings {
		mustWrite(t, filepath.Join(f.root(), "f"+strconv.Itoa(i)), "x")
	}
	// The first entry stat'ed is reported foreign; any later fstatat call means
	// the walk went on to a sibling.
	calls, afterFail := 0, 0
	setFstatat(t, func(real func(int, string, *unix.Stat_t, int) error, dirfd int, path string, st *unix.Stat_t, flags int) error {
		calls++
		if calls > 1 {
			afterFail++
		}
		if err := real(dirfd, path, st, flags); err != nil {
			return err
		}
		if calls == 1 {
			st.Uid++
		}
		return nil
	})

	if err := Remove(f.parentFd, treeName, pin); !errors.Is(err, ErrOwner) {
		t.Fatalf("Remove = %v, want ErrOwner", err)
	}
	if calls != 1 || afterFail != 0 {
		t.Fatalf("fstatat called %d times (%d after the failing entry), want exactly 1", calls, afterFail)
	}
	entries, err := os.ReadDir(f.root())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != siblings {
		t.Fatalf("%d entries remain, want all %d", len(entries), siblings)
	}
}

func TestRemoveSkipsConcurrentlyDeletedEntries(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	f := newFixture(t)
	pin := f.createTree(t)
	buildModuleCache(t, f.root())
	mustWrite(t, filepath.Join(f.root(), "gone-before-stat"), "x")
	mustWrite(t, filepath.Join(f.root(), "gone-before-unlink"), "x")
	var skippedStat, skippedUnlink bool
	setFstatat(t, func(real func(int, string, *unix.Stat_t, int) error, dirfd int, path string, st *unix.Stat_t, flags int) error {
		switch path {
		case "gone-before-stat":
			if err := unix.Unlinkat(dirfd, path, 0); err != nil {
				t.Errorf("unlink: %v", err)
			}
			err := real(dirfd, path, st, flags)
			skippedStat = errors.Is(err, unix.ENOENT)
			return err
		case "gone-before-unlink":
			if err := real(dirfd, path, st, flags); err != nil {
				return err
			}
			skippedUnlink = unix.Unlinkat(dirfd, path, 0) == nil
			return nil
		}
		return real(dirfd, path, st, flags)
	})

	if err := Remove(f.parentFd, treeName, pin); err != nil {
		t.Fatalf("Remove: %v (reason %q)", err, Reason(err))
	}
	if !skippedStat || !skippedUnlink {
		t.Fatalf("seam did not exercise both ENOENT paths: stat=%v unlink=%v", skippedStat, skippedUnlink)
	}
	assertGone(t, f.root())
}

func TestRemoveReportsNameBytesBound(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	f := newFixture(t)
	pin := f.createTree(t)
	for i := range 10 {
		mustWrite(t, filepath.Join(f.root(), "name-"+strconv.Itoa(i)), "x")
	}
	prev := maxNameBytes
	maxNameBytes = 40
	t.Cleanup(func() { maxNameBytes = prev })

	if err := Remove(f.parentFd, treeName, pin); !errors.Is(err, ErrBound) || Reason(err) != "bound" {
		t.Fatalf("Remove = %v, want ErrBound", err)
	}
	assertExists(t, filepath.Join(f.root(), "name-0"))
}

func TestRemoveFailsClosedOnSwapBetweenOpens(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	f := newFixture(t)
	pin := f.createTree(t)
	buildModuleCache(t, f.root())
	fired := false
	prev := hookBetweenPathAndReadOpen
	hookBetweenPathAndReadOpen = func(dirfd int, name string) {
		if fired || name != "sub" {
			return
		}
		fired = true
		if err := unix.Renameat(dirfd, name, dirfd, name+".moved"); err != nil {
			t.Errorf("rename: %v", err)
			return
		}
		if err := unix.Mkdirat(dirfd, name, 0o700); err != nil {
			t.Errorf("mkdir: %v", err)
		}
	}
	t.Cleanup(func() { hookBetweenPathAndReadOpen = prev })

	err := Remove(f.parentFd, treeName, pin)
	if !fired {
		t.Fatal("hook never fired")
	}
	if !errors.Is(err, ErrMismatch) {
		t.Fatalf("Remove = %v, want ErrMismatch", err)
	}
	assertExists(t, filepath.Join(f.root(), moduleCacheRel, "sub"))
	assertExists(t, filepath.Join(f.root(), moduleCacheRel, "sub.moved", "sub.go"))
}

func setPathReadHook(t *testing.T, h func(dirfd int, name string)) {
	t.Helper()
	prev := hookBetweenPathAndReadOpen
	hookBetweenPathAndReadOpen = h
	t.Cleanup(func() { hookBetweenPathAndReadOpen = prev })
}

func setProcFdPrefix(t *testing.T, prefix string) {
	t.Helper()
	prev := procFdPrefix
	procFdPrefix = prefix
	t.Cleanup(func() { procFdPrefix = prev })
}

func modeOf(t *testing.T, p string) uint32 {
	t.Helper()
	var st unix.Stat_t
	if err := unix.Lstat(p, &st); err != nil {
		t.Fatal(err)
	}
	return st.Mode & 0o7777
}

// An ENOENT after the root's first open is not "absent": the tree still exists.
func TestRemoveRootRenamedBetweenOpensIsMismatch(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	f := newFixture(t)
	pin := f.createTree(t)
	buildModuleCache(t, f.root())
	fired := false
	setPathReadHook(t, func(dirfd int, name string) {
		if fired || name != treeName {
			return
		}
		fired = true
		if err := unix.Renameat(dirfd, name, dirfd, name+".moved"); err != nil {
			t.Errorf("rename: %v", err)
		}
	})

	err := Remove(f.parentFd, treeName, pin)
	if !fired {
		t.Fatal("hook never fired")
	}
	if !errors.Is(err, ErrMismatch) || errors.Is(err, ErrNotExist) || Reason(err) != "mismatch" {
		t.Fatalf("Remove = %v (reason %q), want ErrMismatch, not ErrNotExist", err, Reason(err))
	}
	assertExists(t, filepath.Join(f.root()+".moved", moduleCacheRel, "go.mod"))
}

// An ENOENT from the magic-link chmod (no /proc) is an I/O failure, not "absent".
func TestRemoveChmodENOENTIsIO(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	f := newFixture(t)
	pin := f.createTree(t)
	buildModuleCache(t, f.root())
	mustChmod(t, f.root(), 0o000)
	setProcFdPrefix(t, filepath.Join(f.base, "no-such-dir")+"/")

	err := Remove(f.parentFd, treeName, pin)
	if !errors.Is(err, ErrIO) || !errors.Is(err, unix.ENOENT) || errors.Is(err, ErrNotExist) || Reason(err) != "io" {
		t.Fatalf("Remove = %v (reason %q), want ErrIO wrapping ENOENT", err, Reason(err))
	}
	if got := modeOf(t, f.root()); got != 0 {
		t.Fatalf("root mode = %o, want untouched 0000", got)
	}
	mustChmod(t, f.root(), 0o700)
	assertExists(t, filepath.Join(f.root(), moduleCacheRel, "go.mod"))
}

// A magic-link directory that is not procfs misdirects the chmod; the post-chmod
// fstat of the verified fd must catch it.
func TestRemoveMisdirectedChmodIsMismatch(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	f := newFixture(t)
	pin := f.createTree(t)
	buildModuleCache(t, f.root())
	decoy := filepath.Join(f.base, "decoy")
	mustMkdir(t, decoy)
	mustChmod(t, decoy, 0o755)
	fake := filepath.Join(f.base, "fakefd")
	mustMkdir(t, fake)
	for i := range 1024 {
		if err := os.Symlink(decoy, filepath.Join(fake, strconv.Itoa(i))); err != nil {
			t.Fatal(err)
		}
	}
	setProcFdPrefix(t, fake+"/")
	mustChmod(t, f.root(), 0o000)

	err := Remove(f.parentFd, treeName, pin)
	if !errors.Is(err, ErrMismatch) {
		t.Fatalf("Remove = %v, want ErrMismatch", err)
	}
	if got := modeOf(t, decoy); got != 0o700 {
		t.Fatalf("decoy mode = %o, want 0700 (the chmod must have been misdirected there)", got)
	}
	if got := modeOf(t, f.root()); got != 0 {
		t.Fatalf("root mode = %o, want untouched 0000", got)
	}
	mustChmod(t, f.root(), 0o700)
	assertExists(t, filepath.Join(f.root(), moduleCacheRel, "go.mod"))
}

// The restore adds the owner bits and nothing else.
func TestRemoveChmodOnlyAddsOwnerBits(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	f := newFixture(t)
	pin := f.createTree(t)
	want := map[string]uint32{"m0055": 0o755, "m0000": 0o700}
	orig := map[string]os.FileMode{"m0055": 0o055, "m0000": 0o000}
	for name := range want {
		dir := filepath.Join(f.root(), name)
		mustMkdir(t, dir)
		mustWrite(t, filepath.Join(dir, "f"), "x")
		mustChmod(t, dir, orig[name])
	}
	got := map[string]uint32{}
	setPathReadHook(t, func(dirfd int, name string) {
		if _, ok := want[name]; !ok {
			return
		}
		var st unix.Stat_t
		if err := unix.Fstatat(dirfd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			t.Errorf("fstatat %s: %v", name, err)
			return
		}
		got[name] = st.Mode & 0o7777
	})

	if err := Remove(f.parentFd, treeName, pin); err != nil {
		t.Fatalf("Remove: %v (reason %q)", err, Reason(err))
	}
	for name, w := range want {
		if g, ok := got[name]; !ok || g != w {
			t.Errorf("%s mode after chmod = %o (seen %v), want %o", name, g, ok, w)
		}
	}
	assertGone(t, f.root())
}

// The O_RDONLY reopen must not follow a symlink swapped in after the O_PATH
// open, even one pointing back at the verified inode.
func TestRemoveReopenIsNoFollow(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	f := newFixture(t)
	pin := f.createTree(t)
	buildModuleCache(t, f.root())
	fired := false
	setPathReadHook(t, func(dirfd int, name string) {
		if fired || name != "sub" {
			return
		}
		fired = true
		if err := unix.Renameat(dirfd, name, dirfd, name+".moved"); err != nil {
			t.Errorf("rename: %v", err)
			return
		}
		if err := unix.Symlinkat(name+".moved", dirfd, name); err != nil {
			t.Errorf("symlink: %v", err)
		}
	})

	err := Remove(f.parentFd, treeName, pin)
	if !fired {
		t.Fatal("hook never fired")
	}
	if !errors.Is(err, ErrMismatch) || (!errors.Is(err, unix.ELOOP) && !errors.Is(err, unix.ENOTDIR)) {
		t.Fatalf("Remove = %v, want ErrMismatch from ELOOP/ENOTDIR at the reopen", err)
	}
	assertExists(t, filepath.Join(f.root(), moduleCacheRel, "sub.moved", "sub.go"))
	assertExists(t, filepath.Join(f.root(), moduleCacheRel, "sub.moved", "deeper", "d.go"))
}

// Create must never chmod a directory it has not proven it made.
func TestCreateNeverChmodsSubstitutedDir(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	f := newFixture(t)
	var planted string
	setCreateHook(t, func(parentFd int, name string) {
		if err := unix.Renameat(parentFd, name, parentFd, name+".fresh"); err != nil {
			t.Errorf("rename: %v", err)
			return
		}
		mustMkdir(t, f.root())
		planted = filepath.Join(f.root(), "planted")
		mustWrite(t, planted, "x")
		mustChmod(t, f.root(), 0o555)
	})
	fd, _, err := Create(f.parentFd, treeName, os.Geteuid())
	if err == nil {
		_ = unix.Close(fd)
	}
	if !errors.Is(err, ErrMismatch) || fd != -1 {
		t.Fatalf("Create = %d, %v; want -1, ErrMismatch", fd, err)
	}
	if got := modeOf(t, f.root()); got != 0o555 {
		t.Fatalf("planted dir mode = %o, want untouched 0555", got)
	}
	assertExists(t, planted)
}

func TestOpenDirNoFollow(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	f := newFixture(t)
	if err := os.Symlink(f.sentinel, filepath.Join(f.base, "link")); err != nil {
		t.Fatal(err)
	}
	// With O_DIRECTORY, Linux refuses a final symlink under O_NOFOLLOW with
	// ENOTDIR rather than ELOOP; without O_NOFOLLOW this open would succeed.
	if fd, err := OpenDirNoFollow(f.parentFd, "link"); !errors.Is(err, unix.ELOOP) && !errors.Is(err, unix.ENOTDIR) {
		if err == nil {
			_ = unix.Close(fd)
		}
		t.Fatalf("OpenDirNoFollow(symlink) = %d, %v; want ELOOP/ENOTDIR", fd, err)
	}
	fd, err := OpenDirNoFollow(f.parentFd, sentinelName)
	if err != nil {
		t.Fatalf("OpenDirNoFollow(dir): %v", err)
	}
	_ = unix.Close(fd)
}

// A verified root that disappears at the recheck (renamed away, nothing put
// back) was moved, not removed: ErrMismatch, never "absent".
func TestRemoveRootVanishedAtRecheckIsMismatch(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	f := newFixture(t)
	pin := f.createTree(t)
	buildModuleCache(t, f.root())
	setRootRecheckHook(t, func(int, string) {
		if err := os.Rename(f.root(), f.root()+".orig"); err != nil {
			t.Errorf("rename: %v", err)
		}
	})

	err := Remove(f.parentFd, treeName, pin)
	if !errors.Is(err, ErrMismatch) || errors.Is(err, ErrNotExist) || Reason(err) != "mismatch" {
		t.Fatalf("Remove = %v (reason %q), want ErrMismatch", err, Reason(err))
	}
	assertExists(t, f.root()+".orig")
}

// A descendant directory deleted between its fstatat and its open is a failed
// walk (ErrIO), never ErrNotExist: only the root's first open may say "absent".
func TestRemoveDescendantVanishedBeforeOpenIsNotAbsent(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	f := newFixture(t)
	pin := f.createTree(t)
	mustMkdir(t, filepath.Join(f.root(), "gone"))
	setStatOpenHook(t, func(dirfd int, name string) {
		if name == "gone" {
			if err := unix.Unlinkat(dirfd, name, unix.AT_REMOVEDIR); err != nil {
				t.Errorf("rmdir: %v", err)
			}
		}
	})

	err := Remove(f.parentFd, treeName, pin)
	if !errors.Is(err, ErrIO) || errors.Is(err, ErrNotExist) || Reason(err) == "absent" {
		t.Fatalf("Remove = %v (reason %q), want ErrIO, not absent", err, Reason(err))
	}
	assertExists(t, f.root())
}

// A umask stripping owner bits from the mkdirat makes Create fail closed
// rather than restore bits on a directory it cannot prove it made.
func TestCreateFailsUnderOwnerStrippingUmask(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	f := newFixture(t)
	old := unix.Umask(0o100)
	fd, _, err := Create(f.parentFd, treeName, os.Geteuid())
	unix.Umask(old)
	if err == nil {
		_ = unix.Close(fd)
		t.Fatal("Create succeeded under umask 0100, want ErrMismatch")
	}
	if !errors.Is(err, ErrMismatch) {
		t.Fatalf("Create = %v, want ErrMismatch", err)
	}
}

// The root vanishing after a successful recheck and before its rmdir is
// ErrMismatch, never "absent".
func TestRemoveRootVanishedBeforeRmdirIsMismatch(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	f := newFixture(t)
	pin := f.createTree(t)
	rechecking := false
	setRootRecheckHook(t, func(int, string) { rechecking = true })
	setFstatat(t, func(real func(int, string, *unix.Stat_t, int) error, dirfd int, path string, st *unix.Stat_t, flags int) error {
		err := real(dirfd, path, st, flags)
		if rechecking && dirfd == f.parentFd && path == treeName {
			if rerr := os.Rename(f.root(), f.root()+".orig"); rerr != nil {
				t.Errorf("rename: %v", rerr)
			}
		}
		return err
	})

	err := Remove(f.parentFd, treeName, pin)
	if !errors.Is(err, ErrMismatch) || errors.Is(err, ErrNotExist) {
		t.Fatalf("Remove = %v, want ErrMismatch", err)
	}
	assertExists(t, f.root()+".orig")
}
