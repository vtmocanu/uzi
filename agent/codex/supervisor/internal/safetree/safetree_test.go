package safetree

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
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

		if err := Remove(f.parentFd, treeName, pin); !errors.Is(err, ErrMismatch) {
			t.Fatalf("Remove = %v, want ErrMismatch", err)
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
}
