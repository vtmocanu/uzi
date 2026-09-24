package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"golang.org/x/sys/unix"
)

// The three standalone modes. Each is selected by its flag as the FIRST
// argument, has its own strict fixed-order parser, needs no fd 3/4 and forks
// nothing, and writes its JSON lines to stdout.
const (
	modeReapOrphans = "--reap-orphans"
	modeHoldCache   = "--hold-cache"
	modeRemoveCache = "--remove-cache"
)

// Exit codes of --reap-orphans.
const (
	reapExitDone  = 0
	reapExitSetup = 2
)

// modeArgs is a parsed mode argv.
type modeArgs struct {
	mode      string
	expectUID int
	cacheRoot string
	token     string // hold/remove only
}

// isModeFlag reports whether arg selects one of the standalone modes.
func isModeFlag(arg string) bool {
	return arg == modeReapOrphans || arg == modeHoldCache || arg == modeRemoveCache
}

// parseModeArgs parses exactly:
//
//	--reap-orphans --expect-uid <N> --cache-root <abs dir>
//	--hold-cache   --expect-uid <N> --cache-root <abs dir> --cache-token <lowercase uuid>
//	--remove-cache --expect-uid <N> --cache-root <abs dir> --cache-token <lowercase uuid>
//
// in that order. The cache root must be an absolute, already-clean path other
// than "/".
func parseModeArgs(args []string) (modeArgs, error) {
	if len(args) == 0 || !isModeFlag(args[0]) {
		return modeArgs{}, errBadArgs
	}
	a := modeArgs{mode: args[0]}
	want := 5
	if a.mode != modeReapOrphans {
		want = 7
	}
	if len(args) != want || args[1] != "--expect-uid" || args[3] != "--cache-root" {
		return modeArgs{}, errBadArgs
	}
	n, err := strconv.Atoi(args[2])
	if err != nil || n < 0 || strconv.Itoa(n) != args[2] {
		return modeArgs{}, errBadArgs
	}
	a.expectUID = n
	root := args[4]
	if !filepath.IsAbs(root) || filepath.Clean(root) != root || root == "/" {
		return modeArgs{}, errBadArgs
	}
	a.cacheRoot = root
	if a.mode != modeReapOrphans {
		if args[5] != "--cache-token" || !validCleanupToken(args[6]) {
			return modeArgs{}, errBadArgs
		}
		a.token = args[6]
	}
	return a, nil
}

// modeEnv is the standalone modes' dependencies; realModeEnv wires the real
// ones.
type modeEnv struct {
	stdin  io.Reader
	stdout io.Writer
	// hygiene marks every inherited fd from firstStrayFd upward close-on-exec.
	hygiene func() error
	// nondumpable makes the process nondumpable (PR_SET_DUMPABLE 0, confirmed),
	// so a same-uid peer cannot reopen its fds (its stdin, the release
	// channel) through the proc fd directory; --hold-cache runs it first.
	nondumpable func() bool
	// uids returns the real and effective uid.
	uids func() (int, int)
	// tmpDir is the directory holding the command tmps.
	tmpDir string
	// procs is the process table for the no-user proof; selfPid is this
	// process.
	procs   procTable
	selfPid int
	now     func() time.Time
}

func realModeEnv() modeEnv {
	pid := os.Getpid()
	return modeEnv{
		stdin:       os.Stdin,
		stdout:      os.Stdout,
		hygiene:     func() error { return markStrayFdsCloexec(realFdHygiene) },
		nondumpable: establishNondumpable,
		uids:        func() (int, int) { return os.Getuid(), os.Geteuid() },
		tmpDir:      "/tmp",
		procs:       procFS{root: procRootPath, self: pid},
		selfPid:     pid,
		now:         time.Now,
	}
}

// reapErrorLine is --reap-orphans' line when it exits 2 without a scan.
type reapErrorLine struct {
	Event  string `json:"event"`
	Reason string `json:"reason"`
}

// runMode runs the standalone mode args[0] names: for --hold-cache the
// nondumpable step first, then fd hygiene, then the strict argv, then the uid
// check (real and effective uid must both be --expect-uid), then the mode. It
// returns the exit code.
func runMode(args []string, env modeEnv) int {
	fail := func(reason string) int {
		if len(args) > 0 && args[0] == modeReapOrphans {
			writeLine(env.stdout, reapErrorLine{Event: "reap_error", Reason: reason})
			return reapExitSetup
		}
		return cacheError(env.stdout, reason)
	}
	if len(args) > 0 && args[0] == modeHoldCache && !env.nondumpable() {
		return fail("dumpable")
	}
	if err := env.hygiene(); err != nil {
		return fail("fd_hygiene")
	}
	a, err := parseModeArgs(args)
	if err != nil {
		return fail("args")
	}
	if ruid, euid := env.uids(); ruid != a.expectUID || euid != a.expectUID {
		return fail("uid")
	}

	if a.mode == modeReapOrphans {
		return runReap(a, env)
	}
	rootFd, err := openCacheRoot(a.cacheRoot, a.expectUID)
	if err != nil {
		return fail("root")
	}
	defer func() { _ = unix.Close(rootFd) }()
	if a.mode == modeHoldCache {
		return holdCache(rootFd, a.cacheRoot, a.token, a.expectUID, env.stdin, env.stdout)
	}
	return removeCache(rootFd, a.token, a.expectUID, env.stdout)
}

// runReap opens the two roots and runs one pass. A missing cache root is
// skipped; a cache root that exists but is not a 0700 directory owned by the
// command uid, or an unopenable tmp dir, is a reap_error.
func runReap(a modeArgs, env modeEnv) int {
	fail := func(reason string) int {
		writeLine(env.stdout, reapErrorLine{Event: "reap_error", Reason: reason})
		return reapExitSetup
	}
	tmpFd, err := unix.Open(env.tmpDir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fail("tmp_root")
	}
	defer func() { _ = unix.Close(tmpFd) }()
	cacheFd, err := openCacheRoot(a.cacheRoot, a.expectUID)
	switch {
	case errors.Is(err, unix.ENOENT):
		cacheFd = -1
	case err != nil:
		return fail("cache_root")
	default:
		defer func() { _ = unix.Close(cacheFd) }()
	}
	res, err := reap(tmpFd, cacheFd, reapConfig{
		uid:   a.expectUID,
		prove: func() string { return proveNoUser(env.procs, a.expectUID, env.selfPid) },
		now:   env.now,
	})
	if err != nil {
		return fail("list")
	}
	writeLine(env.stdout, res)
	return reapExitDone
}
