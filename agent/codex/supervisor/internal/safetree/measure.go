package safetree

import (
	"errors"
	"fmt"
	"math"
	"time"

	"golang.org/x/sys/unix"
)

// Measure's limit failures. Their Reason words are "entries", "depth" and
// "bytes"; an expired deadline is ErrDeadline.
var (
	// ErrEntryLimit: the tree holds more entries than Limits.MaxEntries.
	ErrEntryLimit = errors.New("safetree: entry limit")
	// ErrDepthLimit: the tree is deeper than Limits.MaxDepth.
	ErrDepthLimit = errors.New("safetree: depth limit")
	// ErrByteLimit: the byte total would overflow uint64 (a peer can make
	// sparse files with huge apparent sizes). Usage.Bytes is then saturated at
	// math.MaxUint64, never a wrapped small value.
	ErrByteLimit = errors.New("safetree: byte limit")
)

// Limits bounds one Measure. MaxEntries counts every entry below the root
// (files, symlinks, directories); MaxDepth is the deepest directory level
// below the root that is entered (the root's children are level 1); a zero
// Deadline means none. A MaxEntries of zero or less means unlimited entries;
// a MaxDepth of zero or less forbids any subdirectory (ErrDepthLimit at the
// first one).
type Limits struct {
	MaxEntries int
	MaxDepth   int
	Deadline   time.Time
}

// Usage is what Measure counted. On an error it is the partial count up to the
// point the walk stopped.
type Usage struct {
	Bytes   uint64
	Entries uint64
}

// Measure sums the on-disk size of the tree name under parentFd without
// changing anything: no chmod, unlink, rename or lock, and every directory is
// opened through openDir with readOnlyOpen, so a 0555 directory (a Go module
// cache) measures and a directory lacking owner r or x is ErrMismatch. It is
// an advisory estimate, never an authority for removal.
//
// Each entry below the root is charged max(st_size, st_blocks*512) from an
// fstatat with AT_SYMLINK_NOFOLLOW (a symlink is charged for its own inode
// only and is never followed; only directories are opened). The root directory
// itself is not charged and not counted. Entries must sit on the root's
// filesystem (ErrMismatch) and be owned by uid (ErrOwner).
//
// An entry that vanished before its fstatat, and a directory that vanished
// before its first open or between its two opens (including one renamed
// away), is skipped, so a tree with concurrent writers can be undercounted.
// A directory swapped for a different object between those opens is
// ErrMismatch, not skipped. A byte total that would overflow uint64 stops the
// walk with ErrByteLimit and Usage.Bytes saturated at math.MaxUint64. A missing root is ErrNotExist. The walk is depth-first
// and fd-relative with a single forward getdents pass per directory; it holds
// at most MaxDepth+2 fds at once. It stops at the first error, returning the
// partial Usage.
func Measure(parentFd int, name string, uid int, lim Limits) (Usage, error) {
	if !validName(name) {
		return Usage{}, ErrName
	}
	fd, st, err := openDir(parentFd, name, ident{}, uid, readOnlyOpen|rootOpen, "measure root")
	if err != nil {
		return Usage{}, err
	}
	m := &measurer{uid: uid, dev: uint64(st.Dev), lim: lim}
	err = m.walkDir(fd, 0)
	_ = unix.Close(fd)
	return m.use, err
}

type measurer struct {
	uid int
	dev uint64
	lim Limits
	use Usage
}

func (m *measurer) checkDeadline() error {
	if !m.lim.Deadline.IsZero() && !now().Before(m.lim.Deadline) {
		return ErrDeadline
	}
	return nil
}

// walkDir lists the verified directory fd (at depth levels below the root) in
// one forward getdents pass and measures every entry.
func (m *measurer) walkDir(fd int, depth int) error {
	buf := make([]byte, max(direntBufSize, minDirentBufSize))
	var names []string
	for {
		if err := m.checkDeadline(); err != nil {
			return err
		}
		n, err := getdents(fd, buf)
		if err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return ioErr("getdents", err)
		}
		if n <= 0 {
			return nil
		}
		// ParseDirent already drops "." and "..".
		_, _, names = unix.ParseDirent(buf[:n], -1, names[:0])
		for _, name := range names {
			if err := m.checkDeadline(); err != nil {
				return err
			}
			if err := m.entry(fd, name, depth); err != nil {
				return err
			}
		}
	}
}

func (m *measurer) entry(dirfd int, name string, depth int) error {
	var st unix.Stat_t
	if err := fstatat(dirfd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil // vanished after getdents listed it
		}
		return ioErr("fstatat", err)
	}
	if uint64(st.Dev) != m.dev {
		return fmt.Errorf("%w: entry on another filesystem", ErrMismatch)
	}
	if int(st.Uid) != m.uid {
		return ErrOwner
	}
	if m.lim.MaxEntries > 0 && m.use.Entries >= uint64(m.lim.MaxEntries) {
		return ErrEntryLimit
	}
	m.use.Entries++
	c := charge(&st)
	// >= so a total that reaches the saturated value is never reported complete.
	if c >= math.MaxUint64-m.use.Bytes {
		m.use.Bytes = math.MaxUint64
		return ErrByteLimit
	}
	m.use.Bytes += c
	if !isDir(&st) {
		return nil
	}

	if depth+1 > m.lim.MaxDepth {
		return ErrDepthLimit
	}
	child, _, err := openDir(dirfd, name, ident{dev: uint64(st.Dev), ino: st.Ino, set: true}, m.uid, readOnlyOpen, "measure open")
	if err != nil {
		// Relies on openDir keeping the errno wrapped: ENOENT of the O_PATH
		// open sits inside ErrIO, and ENOENT of the reopen inside ErrMismatch.
		if errors.Is(err, unix.ENOENT) {
			return nil // the directory vanished before or between its opens
		}
		return err
	}
	err = m.walkDir(child, depth+1)
	_ = unix.Close(child)
	return err
}

// charge is max(st_size, st_blocks*512): a sparse file costs its apparent
// size and a fully allocated one its blocks. Over-reporting is the safe
// direction: it can only trigger rotation, never hide growth.
// The blocks product saturates rather than wrap.
func charge(st *unix.Stat_t) uint64 {
	size, blocks := uint64(max(st.Size, 0)), uint64(max(st.Blocks, 0))
	if blocks > math.MaxUint64/512 {
		return math.MaxUint64
	}
	return max(size, blocks*512)
}
