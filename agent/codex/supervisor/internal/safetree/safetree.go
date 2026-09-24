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
// Caller-supplied names must be a single path component: empty, ".", "..", a
// "/" or a NUL byte is ErrName.
//
// Every directory is opened in two steps relative to its parent's fd. First
// an O_PATH|O_NOFOLLOW|O_DIRECTORY open, which needs no permission on the
// directory itself and refuses a symlink with ELOOP/ENOTDIR; that fd is
// fstat'ed and must be a directory owned by the pinned uid with the dev/ino the
// parent's fstatat reported (the root: the Pin taken at Create). Second, the
// directory is opened O_RDONLY|O_NOFOLLOW|O_DIRECTORY and its fstat must show
// the same dev/ino, so an entry swapped between the two opens (renamed away,
// replaced by another directory or by a symlink) is ErrMismatch.
//
// During Remove only, between those two opens, missing owner rwx bits (modes
// like 0000, 0100, 0300, 0555) are added on that verified inode through its
// /proc/self/fd magic link, which resolves to the already-opened inode and not
// to a pathname. The chmod only adds the owner bits (old mode | 0700); the fd
// is then fstat'ed again and must be the same directory with exactly that mode,
// or the walk stops with ErrMismatch, which catches a chmod misdirected by a
// /proc that is not procfs. If the magic link cannot be resolved (no /proc)
// the chmod fails and the walk stops with ErrIO. Remove reports ErrNotExist
// only when the root's first O_PATH open finds no entry; the root vanishing
// after that is ErrMismatch.
//
// Every entry is lstat'ed (fstatat AT_SYMLINK_NOFOLLOW), must sit on the pinned
// root's filesystem (st_dev == Pin.Dev), and must be owned by the pinned uid; a
// non-directory is removed with unlinkat, which never follows. An entry that
// vanished after getdents listed it (ENOENT on its fstatat, or on the unlinkat
// of a non-directory) is skipped: nothing was followed and nothing is left. The
// first mismatch, foreign owner, bound breach or other syscall error aborts the
// whole walk and retains whatever remains: nothing continues to siblings.
//
// Create never chmods a directory before it has proven it made it. It opens
// the name it just mkdirat'ed with the same two steps but restores no bits: a
// directory that is not owned by uid, lacks any owner rwx bit (mkdirat asked
// for 0700), or is not empty (getdents shows more than "." and "..") is
// rejected, so a peer that exchanged its own directory into the name between
// the mkdirat and the open gets neither a pin nor a chmod on it (ErrMismatch,
// or ErrOwner for a foreign owner). Only after those checks pass does Create
// fchmod the directory to exactly 0700. A umask that strips owner bits from
// the mkdirat therefore also makes Create fail.
//
// # Documented residual
//
// There is an instant between the last check on an entry and the unlinkat that
// removes it. A same-uid peer swapping the entry in that instant can at most make
// Remove rmdir an EMPTY directory that the peer could itself rename into place
// (and so could itself remove), or unlink a NAME in a directory the walk already
// verified. It can never make Remove follow a link or chmod an unverified inode:
// every open is O_NOFOLLOW, every chmod goes through an fd whose identity and
// owner were checked first (Create's only after the emptiness check too), and
// rmdir refuses a non-empty directory.
package safetree

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// Typed failures. Callers branch with errors.Is and log only Reason(err).
var (
	// ErrMismatch: an object's identity (dev/ino/type) changed between checks,
	// a symlink sits where a directory was verified, an entry is on another
	// filesystem, or Create found a directory it did not make.
	ErrMismatch = errors.New("safetree: identity mismatch")
	// ErrOwner: an object is not owned by the pinned uid.
	ErrOwner = errors.New("safetree: foreign owner")
	// ErrBound: the tree exceeds maxDepth, maxEntries or maxNameBytes.
	ErrBound = errors.New("safetree: bound exceeded")
	// ErrIO: a syscall failed; the underlying errno is wrapped.
	ErrIO = errors.New("safetree: io")
	// ErrNotExist: Remove's first O_PATH open of the root found no entry. An
	// ENOENT at any later step (the reopen, the chmod, a descendant) is not
	// this error.
	ErrNotExist = errors.New("safetree: absent")
	// ErrName: a caller-supplied name is not a single path component.
	ErrName = errors.New("safetree: invalid name")
)

// Walk bounds. Package vars so tests can lower them. maxNameBytes caps the
// name bytes buffered for one directory.
var (
	maxDepth     = 256
	maxEntries   = 1_000_000
	maxNameBytes = 64 << 20
)

// getdentsBufSize bounds each getdents read; the loop continues until 0.
const getdentsBufSize = 8192

const (
	dirOpenFlags  = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
	pathOpenFlags = unix.O_PATH | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
)

// Test seams. hookBetweenStatAndOpen runs after an entry's fstatat and before
// its openat (directory entries only); hookBeforeRootRecheck runs after the
// contents are gone and before the final root fstatat;
// hookCreateBetweenMkdirAndOpen runs in Create after mkdirat and before the
// open; hookBetweenPathAndReadOpen runs between openDir's O_PATH and O_RDONLY
// opens, after any owner-bit restore. fstatat is the lstat used for every entry
// and for the root re-check; fstat is used on every opened fd. procFdPrefix is
// the magic-link directory the owner-bit restore chmods through.
var (
	hookBetweenStatAndOpen        func(dirfd int, name string)
	hookBeforeRootRecheck         func(parentFd int, name string)
	hookCreateBetweenMkdirAndOpen func(parentFd int, name string)
	hookBetweenPathAndReadOpen    func(dirfd int, name string)
	fstatat                       = unix.Fstatat
	fstat                         = unix.Fstat
	procFdPrefix                  = "/proc/self/fd/"
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
	case errors.Is(err, ErrName):
		return "name"
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
// non-directory in the verified slot is a swap, not an I/O fault. Both forms
// keep the errno wrapped.
func openErr(op string, err error) error {
	if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
		return fmt.Errorf("%w: %s: %w", ErrMismatch, op, err)
	}
	return ioErr(op, err)
}

// validName reports whether name is one path component that cannot resolve
// outside its parent.
func validName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/\x00")
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
	if err := fstat(fd, &st); err != nil {
		return Pin{}, ioErr("fstat", err)
	}
	if err := checkDirOwned(&st, uid); err != nil {
		return Pin{}, err
	}
	return pinOf(&st), nil
}

// ident is an expected dev/ino. The zero value (set == false) accepts any.
type ident struct {
	dev, ino uint64
	set      bool
}

// openFlag selects openDir's behaviour for its caller.
type openFlag int

const (
	// restoreOwnerBits adds missing owner rwx bits on the verified inode (the
	// Remove walk). Without it a directory lacking any owner bit is ErrMismatch.
	restoreOwnerBits openFlag = 1 << iota
	// rootOpen maps ENOENT at the first O_PATH open, and only there, to
	// ErrNotExist.
	rootOpen
)

// openDir opens name under dirfd as a readable directory owned by uid (and, when
// want is set, with want's dev/ino). It returns the fd and its fstat.
func openDir(dirfd int, name string, want ident, uid int, flags openFlag, op string) (int, unix.Stat_t, error) {
	var st unix.Stat_t
	pfd, err := unix.Openat(dirfd, name, pathOpenFlags, 0)
	if err != nil {
		if flags&rootOpen != 0 && errors.Is(err, unix.ENOENT) {
			return -1, st, fmt.Errorf("%w: %s: %w", ErrNotExist, op, err)
		}
		return -1, st, openErr(op, err)
	}
	defer func() { _ = unix.Close(pfd) }()
	if err := fstat(pfd, &st); err != nil {
		return -1, st, ioErr(op+": fstat", err)
	}
	if !isDir(&st) || (want.set && (uint64(st.Dev) != want.dev || st.Ino != want.ino)) {
		return -1, st, fmt.Errorf("%w: %s", ErrMismatch, op)
	}
	if int(st.Uid) != uid {
		return -1, st, ErrOwner
	}
	if st.Mode&0o700 != 0o700 {
		if flags&restoreOwnerBits == 0 {
			return -1, st, fmt.Errorf("%w: %s: owner bits missing", ErrMismatch, op)
		}
		if err := addOwnerBits(pfd, &st, op); err != nil {
			return -1, st, err
		}
	}

	if hookBetweenPathAndReadOpen != nil {
		hookBetweenPathAndReadOpen(dirfd, name)
	}
	fd, err := unix.Openat(dirfd, name, dirOpenFlags, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			// The verified entry left the name between the two opens.
			return -1, st, fmt.Errorf("%w: %s reopen: %w", ErrMismatch, op, err)
		}
		return -1, st, openErr(op+" reopen", err)
	}
	var fst unix.Stat_t
	if err := fstat(fd, &fst); err != nil {
		_ = unix.Close(fd)
		return -1, st, ioErr(op+": fstat", err)
	}
	// Same dev/ino as the owner-checked O_PATH inode means the same inode, whose
	// owner only root could change.
	if !isDir(&fst) || fst.Dev != st.Dev || fst.Ino != st.Ino {
		_ = unix.Close(fd)
		return -1, st, fmt.Errorf("%w: %s reopen", ErrMismatch, op)
	}
	return fd, fst, nil
}

// addOwnerBits sets the owner rwx bits, and only those, on the directory pfd
// holds, then re-fstats pfd and requires the same directory with exactly the
// old mode plus 0700; st is updated to that stat. fchmod refuses an O_PATH fd
// and kernel 6.1 has no fchmodat2 with AT_EMPTY_PATH, so the chmod goes through
// the magic link naming the inode pfd already holds.
func addOwnerBits(pfd int, st *unix.Stat_t, op string) error {
	want := (st.Mode & 0o7777) | 0o700
	magic := procFdPrefix + strconv.Itoa(pfd)
	if err := unix.Fchmodat(unix.AT_FDCWD, magic, want, 0); err != nil {
		return ioErr(op+": chmod", err)
	}
	var after unix.Stat_t
	if err := fstat(pfd, &after); err != nil {
		return ioErr(op+": fstat after chmod", err)
	}
	if !isDir(&after) || after.Dev != st.Dev || after.Ino != st.Ino || after.Mode&0o7777 != want {
		return fmt.Errorf("%w: %s: chmod did not reach the verified inode", ErrMismatch, op)
	}
	*st = after
	return nil
}

// Create makes name under parentFd as a fresh 0700 directory owned by uid and
// returns an open fd on it (the caller keeps it, and may flock it) plus its pin.
// An existing name is an error, and so is finding a non-empty directory at the
// name after the mkdirat. A failure after mkdir closes the fd and reports; it
// does not try to rmdir.
func Create(parentFd int, name string, uid int) (int, Pin, error) {
	if !validName(name) {
		return -1, Pin{}, ErrName
	}
	if err := unix.Mkdirat(parentFd, name, 0o700); err != nil {
		return -1, Pin{}, ioErr("mkdirat", err)
	}
	if hookCreateBetweenMkdirAndOpen != nil {
		hookCreateBetweenMkdirAndOpen(parentFd, name)
	}
	// No restoreOwnerBits: nothing is chmodded until requireFresh has passed.
	fd, st, err := openDir(parentFd, name, ident{}, uid, 0, "create open")
	if err != nil {
		return -1, Pin{}, err
	}
	if err := requireFresh(fd); err != nil {
		_ = unix.Close(fd)
		return -1, Pin{}, err
	}
	if err := unix.Fchmod(fd, 0o700); err != nil {
		_ = unix.Close(fd)
		return -1, Pin{}, ioErr("fchmod", err)
	}
	return fd, pinOf(&st), nil
}

// requireFresh verifies the directory fd is empty: one getdents shows only "."
// and "..". st_nlink is not checked: btrfs reports 1 for every directory.
func requireFresh(fd int) error {
	buf := make([]byte, getdentsBufSize)
	n, err := unix.Getdents(fd, buf)
	for errors.Is(err, unix.EINTR) {
		n, err = unix.Getdents(fd, buf)
	}
	if err != nil {
		return ioErr("create getdents", err)
	}
	_, _, names := unix.ParseDirent(buf[:max(n, 0)], -1, nil)
	for _, name := range names {
		if name != "." && name != ".." {
			return fmt.Errorf("%w: create: not empty", ErrMismatch)
		}
	}
	return nil
}

// Remove deletes the tree name under parentFd, provided its root still matches
// pin. It fails closed on the first mismatch, foreign owner, bound breach or
// syscall error, and retains whatever remains.
func Remove(parentFd int, name string, pin Pin) error {
	if !validName(name) {
		return ErrName
	}
	fd, _, err := openDir(parentFd, name, ident{dev: pin.Dev, ino: pin.Ino, set: true}, pin.UID, restoreOwnerBits|rootOpen, "open root")
	if err != nil {
		return err
	}

	w := &walker{uid: pin.UID, dev: pin.Dev}
	err = w.emptyDir(fd, 0)
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
	dev     uint64
	entries int
}

// emptyDir removes every entry of the verified directory fd, which openDir
// already made owner-rwx.
func (w *walker) emptyDir(fd int, depth int) error {
	if depth > maxDepth {
		return ErrBound
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
// walk-wide entry bound and its bytes to the per-directory name bound before it
// is retained.
func (w *walker) readNames(fd int) ([]string, error) {
	buf := make([]byte, getdentsBufSize)
	var names []string
	nameBytes := 0
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
			nameBytes += len(name)
			if nameBytes > maxNameBytes {
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
		if errors.Is(err, unix.ENOENT) {
			return nil // removed concurrently after getdents; nothing followed
		}
		return ioErr("fstatat", err)
	}
	if uint64(st.Dev) != w.dev {
		return fmt.Errorf("%w: entry on another filesystem", ErrMismatch)
	}
	if int(st.Uid) != w.uid {
		return ErrOwner
	}
	if !isDir(&st) {
		if err := unix.Unlinkat(dirfd, name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
			return ioErr("unlinkat", err)
		}
		return nil
	}

	if hookBetweenStatAndOpen != nil {
		hookBetweenStatAndOpen(dirfd, name)
	}
	child, _, err := openDir(dirfd, name, ident{dev: uint64(st.Dev), ino: st.Ino, set: true}, w.uid, restoreOwnerBits, "openat")
	if err != nil {
		return err
	}
	err = w.emptyDir(child, depth+1)
	_ = unix.Close(child)
	if err != nil {
		return err
	}
	if err := unix.Unlinkat(dirfd, name, unix.AT_REMOVEDIR); err != nil {
		return ioErr("rmdir", err)
	}
	return nil
}
