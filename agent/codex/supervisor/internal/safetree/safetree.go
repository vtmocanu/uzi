// Package safetree creates and removes a per-run directory tree through file
// descriptors only, so that a same-uid peer cannot turn cleanup into a write
// against an object it chose.
//
// # Why pathname removal is not enough
//
// The supervisor's command scratch dir holds Go's module cache, whose dirs are
// 0555 and whose files are 0444. os.RemoveAll cannot descend a 0555 dir as its
// non-root owner, and the obvious fix, chmod-walking by pathname, is unsafe here:
// several processes run under the same uid at once, and the command sandbox's
// Landlock confinement is best-effort (it may run without it). A same-uid peer
// can therefore rename a directory or plant a symlink at any moment, and a
// pathname walk would follow the swap into a tree it was never meant to touch.
//
// # The mechanism
//
// Every directory is opened with O_NOFOLLOW|O_DIRECTORY relative to its parent's
// fd, fstat'ed, and compared against the dev/ino that the parent's fstatat
// reported (the root against the Pin taken at Create). Owner rwx is restored
// with fchmod on that verified fd, never by path. Every entry is lstat'ed
// (fstatat AT_SYMLINK_NOFOLLOW) and must be owned by the pinned uid; a
// non-directory is removed with unlinkat, which never follows. The first
// mismatch, foreign owner, bound breach or syscall error aborts the whole walk
// and retains whatever remains: nothing is skipped and nothing continues to
// siblings.
//
// # Documented residual
//
// There is an instant between the last check on an entry and the unlinkat that
// removes it. A same-uid peer swapping the entry in that instant can at most make
// Remove rmdir an EMPTY directory owned by the same uid, or unlink a NAME in a
// directory the walk already verified. It can never make Remove follow a link,
// chmod a foreign inode, or delete a non-empty foreign tree: rmdir refuses a
// non-empty dir, and every chmod goes through an fd whose identity was checked.
package safetree

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// Typed failures. Callers branch with errors.Is and log only Reason(err).
var (
	// ErrMismatch: an object's identity (dev/ino/type) changed between checks,
	// or a symlink sits where a directory was verified.
	ErrMismatch = errors.New("safetree: identity mismatch")
	// ErrOwner: an object is not owned by the pinned uid.
	ErrOwner = errors.New("safetree: foreign owner")
	// ErrBound: the tree exceeds maxDepth or maxEntries.
	ErrBound = errors.New("safetree: bound exceeded")
	// ErrIO: a syscall failed; the underlying errno is wrapped.
	ErrIO = errors.New("safetree: io")
	// ErrNotExist: the root was absent at the first open.
	ErrNotExist = errors.New("safetree: absent")
)

// Walk bounds. Package vars so tests can lower them.
var (
	maxDepth   = 256
	maxEntries = 1_000_000
)

// getdentsBufSize bounds each getdents read; the loop continues until 0.
const getdentsBufSize = 8192

const dirOpenFlags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC

// Test seams. hookBetweenStatAndOpen runs after an entry's fstatat and before
// its openat (directory entries only); hookBeforeRootRecheck runs after the
// contents are gone and before the final root fstatat. fstatat is the lstat
// used for every entry and for the root re-check.
var (
	hookBetweenStatAndOpen func(dirfd int, name string)
	hookBeforeRootRecheck  func(parentFd int, name string)
	fstatat                = unix.Fstatat
)

// Pin is the identity a tree root must still have when it is removed.
type Pin struct {
	Dev uint64
	Ino uint64
	UID int
}

// Reason maps err to a short fixed string for evidence lines. It never carries a
// path or errno text.
func Reason(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrNotExist):
		return "absent"
	case errors.Is(err, ErrMismatch):
		return "mismatch"
	case errors.Is(err, ErrOwner):
		return "owner"
	case errors.Is(err, ErrBound):
		return "bound"
	default:
		return "io"
	}
}

func ioErr(op string, err error) error {
	return fmt.Errorf("%w: %s: %w", ErrIO, op, err)
}

// openErr classifies an O_NOFOLLOW|O_DIRECTORY open failure: a symlink or a
// non-directory in the verified slot is a swap, not an I/O fault.
func openErr(op string, err error) error {
	if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
		return fmt.Errorf("%w: %s", ErrMismatch, op)
	}
	return ioErr(op, err)
}

func isDir(st *unix.Stat_t) bool { return st.Mode&unix.S_IFMT == unix.S_IFDIR }

func pinOf(st *unix.Stat_t) Pin {
	return Pin{Dev: uint64(st.Dev), Ino: st.Ino, UID: int(st.Uid)}
}

// checkDirOwned verifies st is a directory owned by uid.
func checkDirOwned(st *unix.Stat_t, uid int) error {
	if !isDir(st) {
		return fmt.Errorf("%w: not a directory", ErrMismatch)
	}
	if int(st.Uid) != uid {
		return ErrOwner
	}
	return nil
}

// OpenDirNoFollow opens name under parentFd as a directory without following a
// final symlink.
func OpenDirNoFollow(parentFd int, name string) (int, error) {
	return unix.Openat(parentFd, name, dirOpenFlags, 0)
}

// PinFromFd pins an already-open directory fd, which must be owned by uid.
func PinFromFd(fd int, uid int) (Pin, error) {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return Pin{}, ioErr("fstat", err)
	}
	if err := checkDirOwned(&st, uid); err != nil {
		return Pin{}, err
	}
	return pinOf(&st), nil
}

// Create makes name under parentFd as a fresh 0700 directory owned by uid and
// returns an open fd on it (the caller keeps it, and may flock it) plus its pin.
// An existing name is an error. A failure after mkdir closes the fd and reports;
// it does not try to rmdir.
func Create(parentFd int, name string, uid int) (int, Pin, error) {
	if err := unix.Mkdirat(parentFd, name, 0o700); err != nil {
		return -1, Pin{}, ioErr("mkdirat", err)
	}
	fd, err := OpenDirNoFollow(parentFd, name)
	if err != nil {
		return -1, Pin{}, openErr("openat", err)
	}
	if err := unix.Fchmod(fd, 0o700); err != nil {
		_ = unix.Close(fd)
		return -1, Pin{}, ioErr("fchmod", err)
	}
	pin, err := PinFromFd(fd, uid)
	if err != nil {
		_ = unix.Close(fd)
		return -1, Pin{}, err
	}
	return fd, pin, nil
}

// Remove deletes the tree name under parentFd, provided its root still matches
// pin. It fails closed on the first mismatch, foreign owner, bound breach or
// syscall error, and retains whatever remains.
func Remove(parentFd int, name string, pin Pin) error {
	fd, err := OpenDirNoFollow(parentFd, name)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return ErrNotExist
		}
		return openErr("open root", err)
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		_ = unix.Close(fd)
		return ioErr("fstat root", err)
	}
	if !isDir(&st) || uint64(st.Dev) != pin.Dev || st.Ino != pin.Ino {
		_ = unix.Close(fd)
		return fmt.Errorf("%w: root", ErrMismatch)
	}
	if int(st.Uid) != pin.UID {
		_ = unix.Close(fd)
		return ErrOwner
	}

	w := &walker{uid: pin.UID}
	err = w.emptyDir(fd, &st, 0)
	_ = unix.Close(fd)
	if err != nil {
		return err
	}

	if hookBeforeRootRecheck != nil {
		hookBeforeRootRecheck(parentFd, name)
	}
	var re unix.Stat_t
	if err := fstatat(parentFd, name, &re, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return ioErr("recheck root", err)
	}
	if !isDir(&re) || uint64(re.Dev) != pin.Dev || re.Ino != pin.Ino {
		return fmt.Errorf("%w: root recheck", ErrMismatch)
	}
	if int(re.Uid) != pin.UID {
		return ErrOwner
	}
	if err := unix.Unlinkat(parentFd, name, unix.AT_REMOVEDIR); err != nil {
		return ioErr("rmdir root", err)
	}
	return nil
}

type walker struct {
	uid     int
	entries int
}

// emptyDir removes every entry of the verified directory fd. st is fd's own
// fstat, already checked as a same-uid directory with the expected identity.
func (w *walker) emptyDir(fd int, st *unix.Stat_t, depth int) error {
	if depth > maxDepth {
		return ErrBound
	}
	if st.Mode&0o700 != 0o700 {
		if err := unix.Fchmod(fd, (st.Mode&0o7777)|0o700); err != nil {
			return ioErr("fchmod", err)
		}
	}
	names, err := w.readNames(fd)
	if err != nil {
		return err
	}
	for _, name := range names {
		if err := w.removeEntry(fd, name, depth); err != nil {
			return err
		}
	}
	return nil
}

// readNames lists fd's entries (without "." and ".."), charging each to the
// walk-wide entry bound before it is retained.
func (w *walker) readNames(fd int) ([]string, error) {
	buf := make([]byte, getdentsBufSize)
	var names []string
	for {
		n, err := unix.Getdents(fd, buf)
		if err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return nil, ioErr("getdents", err)
		}
		if n <= 0 {
			return names, nil
		}
		before := len(names)
		_, _, names = unix.ParseDirent(buf[:n], -1, names)
		kept := names[:before]
		for _, name := range names[before:] {
			if name == "." || name == ".." {
				continue
			}
			w.entries++
			if w.entries > maxEntries {
				return nil, ErrBound
			}
			kept = append(kept, name)
		}
		names = kept
	}
}

func (w *walker) removeEntry(dirfd int, name string, depth int) error {
	var st unix.Stat_t
	if err := fstatat(dirfd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return ioErr("fstatat", err)
	}
	if int(st.Uid) != w.uid {
		return ErrOwner
	}
	if !isDir(&st) {
		if err := unix.Unlinkat(dirfd, name, 0); err != nil {
			return ioErr("unlinkat", err)
		}
		return nil
	}

	if hookBetweenStatAndOpen != nil {
		hookBetweenStatAndOpen(dirfd, name)
	}
	child, err := OpenDirNoFollow(dirfd, name)
	if err != nil {
		return openErr("openat", err)
	}
	var cst unix.Stat_t
	if err := unix.Fstat(child, &cst); err != nil {
		_ = unix.Close(child)
		return ioErr("fstat", err)
	}
	if !isDir(&cst) || cst.Dev != st.Dev || cst.Ino != st.Ino {
		_ = unix.Close(child)
		return fmt.Errorf("%w: entry", ErrMismatch)
	}
	if int(cst.Uid) != w.uid {
		_ = unix.Close(child)
		return ErrOwner
	}
	err = w.emptyDir(child, &cst, depth+1)
	_ = unix.Close(child)
	if err != nil {
		return err
	}
	if err := unix.Unlinkat(dirfd, name, unix.AT_REMOVEDIR); err != nil {
		return ioErr("rmdir", err)
	}
	return nil
}
