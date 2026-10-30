package safetree

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// newMeasureTree makes an empty, owner-only tree root in the fixture. The mode
// is set explicitly: a setgid parent would otherwise leave S_ISGID on it.
func newMeasureTree(t *testing.T, f *fixture) string {
	t.Helper()
	root := f.root()
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	mustChmod(t, root, 0o700)
	return root
}

func mkdirMode(t *testing.T, p string, mode os.FileMode) {
	t.Helper()
	if err := os.Mkdir(p, 0o700); err != nil {
		t.Fatal(err)
	}
	mustChmod(t, p, mode)
}

func writeSized(t *testing.T, p string, n int, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(p, make([]byte, n), 0o600); err != nil {
		t.Fatal(err)
	}
	mustChmod(t, p, mode)
}

// expectedUsage charges every entry below root max(size, blocks*512) from
// os.Lstat, independently of Measure.
func expectedUsage(t *testing.T, root string) Usage {
	t.Helper()
	var u Usage
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == root {
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
		u.Bytes += c
		u.Entries++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func measureTree(t *testing.T, f *fixture, lim Limits) (Usage, error) {
	t.Helper()
	return Measure(f.parentFd, treeName, os.Geteuid(), lim)
}

var wideLimits = Limits{MaxEntries: 1 << 20, MaxDepth: 64}

func TestMeasureChargesEveryEntry(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	f := newFixture(t)
	root := newMeasureTree(t, f)
	writeSized(t, filepath.Join(root, "one"), 1, 0o600)
	writeSized(t, filepath.Join(root, "big"), 100000, 0o600)
	writeSized(t, filepath.Join(root, "empty"), 0, 0o600)
	sparse := filepath.Join(root, "sparse")
	writeSized(t, sparse, 0, 0o600)
	if err := os.Truncate(sparse, 5<<20); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(root, "d1", "d2")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	writeSized(t, filepath.Join(nested, "deep"), 4097, 0o600)
	if err := os.Symlink("one", filepath.Join(root, "ln")); err != nil {
		t.Fatal(err)
	}

	want := expectedUsage(t, root)
	if want.Entries != 8 {
		t.Fatalf("fixture has %d entries, want 8", want.Entries)
	}
	got, err := measureTree(t, f, wideLimits)
	if err != nil {
		t.Fatalf("Measure: %v", err)
	}
	if got != want {
		t.Fatalf("Measure = %+v, want %+v", got, want)
	}
	if got.Bytes < 5<<20 {
		t.Fatalf("sparse file charged %d bytes, want at least its apparent size", got.Bytes)
	}
}

func TestMeasureReadOnlyModuleCacheModes(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	f := newFixture(t)
	root := newMeasureTree(t, f)
	mod := filepath.Join(root, moduleCacheRel)
	if err := os.MkdirAll(filepath.Join(mod, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeSized(t, filepath.Join(mod, "a.go"), 300, 0o444)
	writeSized(t, filepath.Join(mod, "sub", "b.go"), 700, 0o444)
	// 0555 directories, deepest first so each chmod happens after its content.
	// newFixture's cleanup restores write bits before TempDir's RemoveAll.
	for p := filepath.Join(mod, "sub"); ; p = filepath.Dir(p) {
		mustChmod(t, p, 0o555)
		if p == root {
			break
		}
	}

	before := snapshotTree(t, root)
	got, err := measureTree(t, f, wideLimits)
	if err != nil {
		t.Fatalf("Measure of a 0555/0444 tree: %v", err)
	}
	if want := expectedUsage(t, root); got != want {
		t.Fatalf("Measure = %+v, want %+v", got, want)
	}
	assertSnapshot(t, before, snapshotTree(t, root))
}

func TestMeasureDoesNotFollowSymlinks(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	f := newFixture(t)
	root := newMeasureTree(t, f)
	outside := t.TempDir()
	big := filepath.Join(outside, "big")
	writeSized(t, big, 1<<20, 0o600)
	dirTarget := filepath.Join(outside, "dir")
	mkdirMode(t, dirTarget, 0o700)
	for i := range 20 {
		writeSized(t, filepath.Join(dirTarget, fmt.Sprint(i)), 1000, 0o600)
	}
	if err := os.Symlink(big, filepath.Join(root, "tobig")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(dirTarget, filepath.Join(root, "todir")); err != nil {
		t.Fatal(err)
	}
	// A dangling link is charged too and does not fail the walk.
	if err := os.Symlink("/nonexistent/target", filepath.Join(root, "dangling")); err != nil {
		t.Fatal(err)
	}

	got, err := measureTree(t, f, wideLimits)
	if err != nil {
		t.Fatalf("Measure: %v", err)
	}
	if got.Entries != 3 || got.Bytes >= 1<<20 {
		t.Fatalf("Measure = %+v: symlink targets were followed", got)
	}
	if want := expectedUsage(t, root); got != want {
		t.Fatalf("Measure = %+v, want %+v", got, want)
	}
}

func TestMeasureOwnerBitsMissingIsMismatch(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	for _, mode := range []os.FileMode{0o000, 0o100, 0o200, 0o300, 0o400} {
		t.Run(fmt.Sprintf("child_%04o", mode), func(t *testing.T) {
			f := newFixture(t)
			root := newMeasureTree(t, f)
			writeSized(t, filepath.Join(root, "first"), 10, 0o600)
			bad := filepath.Join(root, "bad")
			mkdirMode(t, bad, 0o700)
			writeSized(t, filepath.Join(bad, "x"), 10, 0o600)
			mustChmod(t, bad, mode)

			got, err := measureTree(t, f, wideLimits)
			if !errors.Is(err, ErrMismatch) || Reason(err) != "mismatch" {
				t.Fatalf("Measure = %+v, %v; want ErrMismatch", got, err)
			}
			var st unix.Stat_t
			if err := unix.Lstat(bad, &st); err != nil || os.FileMode(st.Mode&0o777) != mode {
				t.Fatalf("mode of the rejected directory changed: %o %v", st.Mode&0o777, err)
			}
		})
	}
	t.Run("root_0300", func(t *testing.T) {
		f := newFixture(t)
		root := newMeasureTree(t, f)
		mustChmod(t, root, 0o300)
		if _, err := measureTree(t, f, wideLimits); !errors.Is(err, ErrMismatch) {
			t.Fatalf("Measure = %v, want ErrMismatch", err)
		}
		var st unix.Stat_t
		if err := unix.Lstat(root, &st); err != nil || st.Mode&0o777 != 0o300 {
			t.Fatalf("root mode changed: %o %v", st.Mode&0o777, err)
		}
	})
	t.Run("owner_r_x_only", func(t *testing.T) {
		f := newFixture(t)
		root := newMeasureTree(t, f)
		sub := filepath.Join(root, "ro")
		mkdirMode(t, sub, 0o700)
		writeSized(t, filepath.Join(sub, "x"), 10, 0o400)
		mustChmod(t, sub, 0o500)
		if _, err := measureTree(t, f, wideLimits); err != nil {
			t.Fatalf("0500 directory: %v", err)
		}
	})
}

func TestMeasureAbsentAndForeignRoot(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	f := newFixture(t)
	if u, err := measureTree(t, f, wideLimits); !errors.Is(err, ErrNotExist) || Reason(err) != "absent" || u != (Usage{}) {
		t.Fatalf("absent: %+v, %v", u, err)
	}
	newMeasureTree(t, f)
	if _, err := Measure(f.parentFd, treeName, os.Geteuid()+1, wideLimits); !errors.Is(err, ErrOwner) {
		t.Fatalf("foreign root owner: %v", err)
	}
	if _, err := Measure(f.parentFd, "a/b", os.Geteuid(), wideLimits); !errors.Is(err, ErrName) {
		t.Fatalf("bad name: %v", err)
	}
	// A symlink at the root name is never followed.
	f2 := newFixture(t)
	if err := os.Symlink(f2.sentinel, f2.root()); err != nil {
		t.Fatal(err)
	}
	if _, err := measureTree(t, f2, wideLimits); !errors.Is(err, ErrMismatch) {
		t.Fatalf("symlinked root: %v", err)
	}
}

// withGetdents installs g for one test.
func withGetdents(t *testing.T, g func(fd int, buf []byte) (int, error)) {
	t.Helper()
	prev := getdents
	getdents = g
	t.Cleanup(func() { getdents = prev })
}

func withFstatat(t *testing.T, g func(dirfd int, path string, st *unix.Stat_t, flags int) error) {
	t.Helper()
	prev := fstatat
	fstatat = g
	t.Cleanup(func() { fstatat = prev })
}

// withFstatatFor wraps the real fstatat and lets edit alter the result for the
// named entry (the root is never named by Measure's fstatat).
func withFstatatFor(t *testing.T, name string, edit func(dirfd int, st *unix.Stat_t)) {
	t.Helper()
	withFstatat(t, func(dirfd int, p string, st *unix.Stat_t, flags int) error {
		err := unix.Fstatat(dirfd, p, st, flags)
		if err == nil && p == name {
			edit(dirfd, st)
		}
		return err
	})
}

func TestMeasureChildEntryChecks(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	setup := func(t *testing.T) (*fixture, string) {
		f := newFixture(t)
		root := newMeasureTree(t, f)
		writeSized(t, filepath.Join(root, "x"), 10, 0o600)
		return f, root
	}
	t.Run("foreign_owner_child", func(t *testing.T) {
		f, _ := setup(t)
		withFstatatFor(t, "x", func(_ int, st *unix.Stat_t) { st.Uid++ })
		if _, err := measureTree(t, f, wideLimits); !errors.Is(err, ErrOwner) || Reason(err) != "owner" {
			t.Fatalf("err = %v, want ErrOwner", err)
		}
	})
	t.Run("other_device_child", func(t *testing.T) {
		f, _ := setup(t)
		withFstatatFor(t, "x", func(_ int, st *unix.Stat_t) { st.Dev++ })
		if _, err := measureTree(t, f, wideLimits); !errors.Is(err, ErrMismatch) || Reason(err) != "mismatch" {
			t.Fatalf("err = %v, want ErrMismatch", err)
		}
	})
	t.Run("fstatat_eio_is_not_skipped", func(t *testing.T) {
		f, _ := setup(t)
		withFstatat(t, func(dirfd int, p string, st *unix.Stat_t, flags int) error {
			if p == "x" {
				return unix.EIO
			}
			return unix.Fstatat(dirfd, p, st, flags)
		})
		got, err := measureTree(t, f, wideLimits)
		if !errors.Is(err, ErrIO) || Reason(err) != "io" || got.Entries != 0 {
			t.Fatalf("got %+v, %v; want ErrIO with nothing counted", got, err)
		}
	})
}

// A directory replaced by a different directory is ErrMismatch and its
// content is not measured, whichever of the two opens it lands between.
func TestMeasureChildDirectorySwapIsMismatch(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	for _, tc := range []string{"between_fstatat_and_path_open", "between_the_two_opens"} {
		t.Run(tc, func(t *testing.T) {
			f := newFixture(t)
			root := newMeasureTree(t, f)
			orig := filepath.Join(root, "d")
			mkdirMode(t, orig, 0o700)
			repl := root + "-repl"
			mkdirMode(t, repl, 0o700)
			writeSized(t, filepath.Join(repl, "inner"), 10, 0o600)
			swapped := false
			swap := func() {
				if swapped {
					return
				}
				swapped = true
				if err := os.Rename(orig, orig+".old"); err != nil {
					t.Error(err)
				}
				if err := os.Rename(repl, orig); err != nil {
					t.Error(err)
				}
			}
			if tc == "between_the_two_opens" {
				prev := hookBetweenPathAndReadOpen
				hookBetweenPathAndReadOpen = func(_ int, name string) {
					if name == "d" {
						swap()
					}
				}
				t.Cleanup(func() { hookBetweenPathAndReadOpen = prev })
			} else {
				withFstatatFor(t, "d", func(int, *unix.Stat_t) { swap() })
			}
			got, err := measureTree(t, f, wideLimits)
			if !swapped || !errors.Is(err, ErrMismatch) || Reason(err) != "mismatch" {
				t.Fatalf("got %+v, %v (swapped=%v); want ErrMismatch", got, err, swapped)
			}
			if got.Entries != 1 {
				t.Fatalf("Entries = %d: the swapped-in directory was measured", got.Entries)
			}
		})
	}
}

// A child directory that fails to open for a reason other than ENOENT is an
// error, never a skipped vanish.
func TestMeasureChildDirectoryOpenFailureIsError(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	newTree := func(t *testing.T) *fixture {
		f := newFixture(t)
		root := newMeasureTree(t, f)
		mkdirMode(t, filepath.Join(root, "d"), 0o700)
		writeSized(t, filepath.Join(root, "d", "inner"), 10, 0o600)
		return f
	}
	t.Run("reopen_eacces", func(t *testing.T) {
		f := newTree(t)
		prev := hookBetweenPathAndReadOpen
		hookBetweenPathAndReadOpen = func(_ int, name string) {
			if name == "d" {
				mustChmod(t, filepath.Join(f.root(), "d"), 0o000)
			}
		}
		t.Cleanup(func() {
			hookBetweenPathAndReadOpen = prev
			mustChmod(t, filepath.Join(f.root(), "d"), 0o700)
		})
		got, err := measureTree(t, f, wideLimits)
		if err == nil || !errors.Is(err, ErrIO) || errors.Is(err, unix.ENOENT) {
			t.Fatalf("got %+v, %v; want ErrIO", got, err)
		}
	})
	t.Run("path_fstat_eio", func(t *testing.T) {
		f := newTree(t)
		calls := 0
		prev := fstat
		// Root openDir: the O_PATH fstat and the reopen fstat; the third call
		// is the child's O_PATH fstat.
		fstat = func(fd int, st *unix.Stat_t) error {
			calls++
			if calls == 3 {
				return unix.EIO
			}
			return unix.Fstat(fd, st)
		}
		t.Cleanup(func() { fstat = prev })
		got, err := measureTree(t, f, wideLimits)
		if !errors.Is(err, ErrIO) || Reason(err) != "io" || calls < 3 {
			t.Fatalf("got %+v, %v (fstat calls=%d); want ErrIO", got, err, calls)
		}
	})
}

// Sparse files with huge apparent sizes must not wrap the byte total to a
// small number: the walk stops, incomplete "bytes", saturated at MaxUint64.
func TestMeasureByteTotalOverflowIsIncomplete(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	f := newFixture(t)
	root := newMeasureTree(t, f)
	for _, n := range []string{"a", "b", "c"} {
		writeSized(t, filepath.Join(root, n), 1, 0o600)
	}
	withFstatat(t, func(dirfd int, p string, st *unix.Stat_t, flags int) error {
		err := unix.Fstatat(dirfd, p, st, flags)
		if err == nil {
			st.Size = math.MaxInt64
		}
		return err
	})
	got, err := measureTree(t, f, wideLimits)
	if !errors.Is(err, ErrByteLimit) || Reason(err) != "bytes" || got.Bytes != math.MaxUint64 {
		t.Fatalf("got %+v, %v; want ErrByteLimit with saturated bytes", got, err)
	}
}

func TestMeasureBlocksProductOverflowIsIncomplete(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	f := newFixture(t)
	root := newMeasureTree(t, f)
	for _, n := range []string{"a", "b", "c"} {
		writeSized(t, filepath.Join(root, n), 1, 0o600)
	}
	withFstatat(t, func(dirfd int, p string, st *unix.Stat_t, flags int) error {
		err := unix.Fstatat(dirfd, p, st, flags)
		if err == nil && !isDir(st) {
			st.Size, st.Blocks = 0, 1<<55
		}
		return err
	})
	got, err := measureTree(t, f, wideLimits)
	if !errors.Is(err, ErrByteLimit) || got.Bytes != math.MaxUint64 || got.Entries != 1 {
		t.Fatalf("got %+v, %v; want ErrByteLimit at the first wrapped entry", got, err)
	}
}

func TestMeasureSkipsEntryGoneBeforeFstatat(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	f := newFixture(t)
	root := newMeasureTree(t, f)
	writeSized(t, filepath.Join(root, "keep"), 1000, 0o600)
	writeSized(t, filepath.Join(root, "victim"), 1000, 0o600)
	writeSized(t, filepath.Join(root, "victim2"), 1000, 0o600)

	removed := false
	withGetdents(t, func(fd int, buf []byte) (int, error) {
		n, err := unix.Getdents(fd, buf)
		if n > 0 && !removed {
			removed = true
			// Between getdents and fstatat.
			_ = os.Remove(filepath.Join(root, "victim"))
			_ = os.Remove(filepath.Join(root, "victim2"))
		}
		return n, err
	})
	got, err := measureTree(t, f, wideLimits)
	if err != nil {
		t.Fatalf("Measure: %v", err)
	}
	if want := expectedUsage(t, root); !removed || got != want || got.Entries != 1 {
		t.Fatalf("Measure = %+v (removed=%v), want %+v: exactly the surviving entry", got, removed, want)
	}
}

func TestMeasureSkipsDirectoryGoneBeforeOpen(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, f *fixture)
	}{
		{"before_path_open", func(t *testing.T, f *fixture) {
			// After fstatat reported the directory, before its O_PATH open.
			withFstatat(t, func(dirfd int, p string, st *unix.Stat_t, flags int) error {
				err := unix.Fstatat(dirfd, p, st, flags)
				if err == nil && p == "gone" {
					_ = os.RemoveAll(filepath.Join(f.root(), "gone"))
				}
				return err
			})
		}},
		{"between_the_two_opens", func(t *testing.T, f *fixture) {
			prev := hookBetweenPathAndReadOpen
			hookBetweenPathAndReadOpen = func(dirfd int, name string) {
				if name == "gone" {
					_ = os.RemoveAll(filepath.Join(f.root(), "gone"))
				}
			}
			t.Cleanup(func() { hookBetweenPathAndReadOpen = prev })
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !requireNonRootCommandUID(t) {
				return
			}
			f := newFixture(t)
			root := newMeasureTree(t, f)
			writeSized(t, filepath.Join(root, "keep"), 10, 0o600)
			gone := filepath.Join(root, "gone")
			mkdirMode(t, gone, 0o700)
			writeSized(t, filepath.Join(gone, "inner"), 10, 0o600)
			tc.setup(t, f)
			got, err := measureTree(t, f, wideLimits)
			if err != nil {
				t.Fatalf("Measure = %+v, %v; want a complete skip", got, err)
			}
			// keep, plus the directory's own entry (charged when it was listed);
			// its content is skipped.
			if got.Entries != 2 {
				t.Fatalf("Entries = %d, want 2", got.Entries)
			}
			if _, err := os.Lstat(gone); !os.IsNotExist(err) {
				t.Fatalf("the vanish hook did not run: %v", err)
			}
		})
	}
}

func TestMeasureLimits(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	t.Run("entries", func(t *testing.T) {
		f := newFixture(t)
		root := newMeasureTree(t, f)
		for i := range 10 {
			writeSized(t, filepath.Join(root, fmt.Sprint("f", i)), 1, 0o600)
		}
		got, err := measureTree(t, f, Limits{MaxEntries: 5, MaxDepth: 64})
		if !errors.Is(err, ErrEntryLimit) || Reason(err) != "entries" {
			t.Fatalf("err = %v", err)
		}
		if got.Entries != 5 || got.Bytes == 0 {
			t.Fatalf("partial = %+v, want 5 entries", got)
		}
		// Exactly at the cap is complete.
		if _, err := measureTree(t, f, Limits{MaxEntries: 10, MaxDepth: 64}); err != nil {
			t.Fatalf("at the cap: %v", err)
		}
	})
	t.Run("depth", func(t *testing.T) {
		f := newFixture(t)
		root := newMeasureTree(t, f)
		if err := os.MkdirAll(filepath.Join(root, "a", "b", "c"), 0o700); err != nil {
			t.Fatal(err)
		}
		_, err := measureTree(t, f, Limits{MaxEntries: 100, MaxDepth: 2})
		if !errors.Is(err, ErrDepthLimit) || Reason(err) != "depth" {
			t.Fatalf("depth 2: %v", err)
		}
		if _, err := measureTree(t, f, Limits{MaxEntries: 100, MaxDepth: 3}); err != nil {
			t.Fatalf("depth 3: %v", err)
		}
	})
	t.Run("deadline_expired", func(t *testing.T) {
		f := newFixture(t)
		root := newMeasureTree(t, f)
		writeSized(t, filepath.Join(root, "f"), 1, 0o600)
		got, err := measureTree(t, f, Limits{MaxEntries: 100, MaxDepth: 64, Deadline: time.Now().Add(-time.Second)})
		if !errors.Is(err, ErrDeadline) || Reason(err) != "deadline" || got != (Usage{}) {
			t.Fatalf("got %+v, %v", got, err)
		}
	})
	t.Run("deadline_mid_walk", func(t *testing.T) {
		f := newFixture(t)
		root := newMeasureTree(t, f)
		for i := range 10 {
			writeSized(t, filepath.Join(root, fmt.Sprint("f", i)), 1, 0o600)
		}
		base := time.Now()
		calls := 0
		prev := now
		now = func() time.Time {
			calls++
			if calls > 4 {
				return base.Add(time.Hour)
			}
			return base
		}
		t.Cleanup(func() { now = prev })
		got, err := measureTree(t, f, Limits{MaxEntries: 100, MaxDepth: 64, Deadline: base.Add(time.Minute)})
		if !errors.Is(err, ErrDeadline) || got.Entries == 0 || got.Entries >= 10 {
			t.Fatalf("got %+v, %v; want a partial count and ErrDeadline", got, err)
		}
	})
}

// openFds counts the open descriptors below 256 by probing F_GETFD.
func openFds() int {
	n := 0
	for fd := range 256 {
		if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err == nil {
			n++
		}
	}
	return n
}

func TestMeasureHoldsAtMostDepthPlusTwoFds(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	f := newFixture(t)
	root := newMeasureTree(t, f)
	const depth = 12
	p := root
	for i := range depth {
		p = filepath.Join(p, fmt.Sprint("d", i))
	}
	if err := os.MkdirAll(p, 0o700); err != nil {
		t.Fatal(err)
	}
	writeSized(t, filepath.Join(p, "leaf"), 1, 0o600)

	base := openFds()
	peak := 0
	withGetdents(t, func(fd int, buf []byte) (int, error) {
		peak = max(peak, openFds()-base)
		return unix.Getdents(fd, buf)
	})
	got, err := measureTree(t, f, wideLimits)
	if err != nil {
		t.Fatal(err)
	}
	if got.Entries != depth+1 {
		t.Fatalf("Entries = %d", got.Entries)
	}
	// Observed at a getdents call: the root plus depth directory fds.
	if peak > depth+1 || peak < depth {
		t.Fatalf("peak extra fds = %d for depth %d", peak, depth)
	}
	if n := openFds(); n != base {
		t.Fatalf("fds leaked: %d open, %d before", n, base)
	}
}

type fileState struct {
	mode  os.FileMode
	size  int64
	mtime time.Time
	ino   uint64
}

func snapshotTree(t *testing.T, root string) map[string]fileState {
	t.Helper()
	snap := map[string]fileState{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		snap[p] = fileState{fi.Mode(), fi.Size(), fi.ModTime(), fi.Sys().(*syscall.Stat_t).Ino}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

func assertSnapshot(t *testing.T, before, after map[string]fileState) {
	t.Helper()
	if len(before) != len(after) {
		t.Fatalf("entry count changed: %d -> %d", len(before), len(after))
	}
	for p, b := range before {
		if a, ok := after[p]; !ok || a != b {
			t.Fatalf("%s changed: %+v -> %+v (present=%v)", p, b, a, ok)
		}
	}
}

func BenchmarkMeasure(b *testing.B) {
	base := b.TempDir()
	root := filepath.Join(base, treeName)
	for d := range 20 {
		dir := filepath.Join(root, fmt.Sprint("d", d), "sub")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			b.Fatal(err)
		}
		for i := range 100 {
			if err := os.WriteFile(filepath.Join(dir, fmt.Sprint("f", i)), []byte("benchmark"), 0o600); err != nil {
				b.Fatal(err)
			}
		}
	}
	fd, err := unix.Open(base, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = unix.Close(fd) }()
	b.ResetTimer()
	for range b.N {
		u, err := Measure(fd, treeName, os.Geteuid(), wideLimits)
		if err != nil || u.Entries != 2040 {
			b.Fatalf("Measure = %+v, %v", u, err)
		}
	}
}
