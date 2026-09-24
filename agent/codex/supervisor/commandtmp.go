package main

import (
	"errors"
	"path"

	"golang.org/x/sys/unix"

	"uzi.local/codex-supervisor/internal/safetree"
)

// commandTmpPrefix is the fixed path a --cleanup-token names: the token is the
// only variable part, so the argv can never point the supervisor at another path.
const commandTmpPrefix = "/tmp/uzi-codex-command-"

// Wire values of the tmpCleanup evidence field.
const (
	tmpCleanupRemoved  = "removed"
	tmpCleanupRetained = "retained"
)

// errTmpRecheck: after the lock was taken, the name no longer named the
// directory Create pinned.
var errTmpRecheck = errors.New("command tmp recheck mismatch")

// Test seams for createCommandTmp. flock takes the liveness lock;
// hookAfterLock runs after the lock is taken and before the name is rechecked.
var (
	flock         = unix.Flock
	hookAfterLock func(parentFd int, name string)
)

// tmpCleanupResult is the tmpCleanup evidence field. Reason is safetree.Reason
// of the removal error: "" when removed, a fixed short word otherwise.
type tmpCleanupResult struct {
	State  string
	Reason string
}

// commandTmp is the supervisor-owned per-command scratch directory. The
// supervisor creates it before fork, holds an exclusive flock on dirFd for its
// whole lifetime (the liveness signal the startup orphan reaper, --reap-orphans,
// tests), and removes it only after a confirmed drain. parentFd and dirFd are
// close-on-exec, so the child never inherits either; neither is closed before
// process exit.
type commandTmp struct {
	parentFd int
	name     string
	dirFd    int
	pin      safetree.Pin
	remove   func(parentFd int, name string, pin safetree.Pin) error
}

// cleanup removes the tree through the pinned identity. Any error, including
// safetree.ErrNotExist (the pinned directory vanishing is not success), is
// "retained" with its fixed reason.
func (c *commandTmp) cleanup() *tmpCleanupResult {
	if err := c.remove(c.parentFd, c.name, c.pin); err != nil {
		return &tmpCleanupResult{State: tmpCleanupRetained, Reason: safetree.Reason(err)}
	}
	return &tmpCleanupResult{State: tmpCleanupRemoved}
}

// commandTmpName is the single path component under /tmp for token.
func commandTmpName(token string) string {
	return path.Base(commandTmpPrefix + token)
}

// openCommandTmp opens /tmp without following a symlink and creates, locks and
// rechecks the token's directory under it. It is the production tmpSetup.
func openCommandTmp(token string, uid int) (*commandTmp, error) {
	return openCommandTmpIn(path.Dir(commandTmpPrefix), token, uid)
}

// openCommandTmpIn is openCommandTmp under parentDir. The parent fd is
// close-on-exec, so the child never inherits it.
func openCommandTmpIn(parentDir, token string, uid int) (*commandTmp, error) {
	parentFd, err := unix.Open(parentDir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	ct, err := createCommandTmp(parentFd, commandTmpName(token), uid)
	if err != nil {
		_ = unix.Close(parentFd)
		return nil, err
	}
	return ct, nil
}

// createCommandTmp makes name under parentFd as a fresh 0700 directory owned by
// uid, takes LOCK_EX|LOCK_NB on its fd, then re-verifies with fstatat
// (AT_SYMLINK_NOFOLLOW) that the name still names the pinned dev/ino, which
// closes the window between the mkdir and the lock. On failure it closes the
// directory fd (dropping the lock) and leaves the directory for the startup
// orphan reaper (--reap-orphans): nothing is removed here.
func createCommandTmp(parentFd int, name string, uid int) (*commandTmp, error) {
	dirFd, pin, err := safetree.Create(parentFd, name, uid)
	if err != nil {
		return nil, err
	}
	if err := flock(dirFd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = unix.Close(dirFd)
		return nil, err
	}
	if hookAfterLock != nil {
		hookAfterLock(parentFd, name)
	}
	var st unix.Stat_t
	if err := unix.Fstatat(parentFd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		_ = unix.Close(dirFd)
		return nil, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR || uint64(st.Dev) != pin.Dev || st.Ino != pin.Ino {
		_ = unix.Close(dirFd)
		return nil, errTmpRecheck
	}
	return &commandTmp{parentFd: parentFd, name: name, dirFd: dirFd, pin: pin, remove: safetree.Remove}, nil
}

// tmpSetup is the pre-fork command tmp setup seam.
type tmpSetup func(token string, uid int) (*commandTmp, error)

// setupAndLaunch runs the pre-fork command tmp setup (only when a token was
// given) and then launches the child. Any setup failure emits the pre-fork
// abnormal "command tmp setup failed" and returns ok == false WITHOUT calling
// launch. A launch failure emits "child launch failed"; the tmp is then left
// for the startup orphan reaper (--reap-orphans), because cleanup runs only
// after a confirmed drain.
func setupAndLaunch(ev *evidence, token string, uid int, setup tmpSetup, launch func([]string) (int, error), argv []string) (*commandTmp, int, bool) {
	var ct *commandTmp
	if token != "" {
		var err error
		ct, err = setup(token, uid)
		if err != nil {
			_ = ev.writeJSON(abnormalEvidence("command tmp setup failed", nil))
			return nil, 0, false
		}
	}
	pid, err := launch(argv)
	if err != nil {
		_ = ev.writeJSON(abnormalEvidence("child launch failed", nil))
		return ct, 0, false
	}
	return ct, pid, true
}

// tmpCleanupFor returns ct's cleanup as a supervisor seam, or nil when there
// is no command tmp (no token): a nil seam means the evidence carries no
// tmpCleanup field.
func tmpCleanupFor(ct *commandTmp) func() *tmpCleanupResult {
	if ct == nil {
		return nil
	}
	return ct.cleanup
}
