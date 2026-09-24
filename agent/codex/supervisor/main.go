package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

func main() {
	os.Exit(realMain(os.Args[1:]))
}

// realMain is the whole flow: the pre-fork sequence (prelaunch: fd hygiene,
// argv, the verified profile, the command tmp, the launch) wired to the real
// syscalls, then the control loop.
func realMain(args []string) int {
	// os.NewFile opens nothing; prelaunch's first step is the fd hygiene.
	ev := &evidence{w: os.NewFile(4, "evidence")}
	tmp, childPid, st, ok := prelaunch(ev, args, prelaunchSeams{
		fdHygiene: func() error { return markStrayFdsCloexec(realFdHygiene) },
		dropCaps:  clearAllCaps,
		profile:   verifyProfile,
		trustFds:  markTrustedFdsCloexec,
		setup:     openCommandTmp,
		launch:    launchChild,
	})
	if !ok {
		return 2
	}
	sup := &supervisor{
		ev:      ev,
		control: &controlReader{r: os.NewFile(3, "control")},
		seams: seams{
			supervisorPid:  os.Getpid(),
			directChildren: selfDirectChildren,
			kill:           realKill,
			reap:           realReap,
			reapChild:      realReapChild,
			snapshot:       func() ([]procRow, error) { return walkDescendants(os.Getpid()) },
			now:            time.Now,
			sleep:          func() { time.Sleep(2 * time.Millisecond) },
			tmpCleanup:     tmpCleanupFor(tmp),
		},
	}
	// The supervised child inherited stdio 0/1/2. Drop the supervisor's copies so
	// EOF/backpressure describe the child tree rather than this long-lived control
	// process; control/evidence remain isolated on fd3/fd4.
	_ = unix.Close(0)
	_ = unix.Close(1)
	_ = unix.Close(2)
	// A child-watch failure goes through sup.abnormal, which touches the tmp only
	// after a confirmed drain. The lock fd held in tmp stays open until process
	// exit, after any cleanup.
	return sup.runWatched(childPid, st, watchChild)
}

// prelaunchSeams are prelaunch's steps; realMain wires the real ones.
type prelaunchSeams struct {
	// fdHygiene marks every fd from firstStrayFd upward close-on-exec.
	fdHygiene func() error
	// dropCaps clears every capability (--drop-controller-caps only).
	dropCaps func() error
	// profile establishes and verifies the pre-fork posture; a non-empty
	// reason is the abnormal reason.
	profile func(expectUID int) (st procStatus, reason string)
	// trustFds marks the trusted fd3/fd4 close-on-exec.
	trustFds func()
	setup    tmpSetup
	launch   func([]string) (int, error)
}

// prelaunch is the pre-fork sequence, in this order: fd hygiene (before
// anything is opened, so no fd above the trusted 3/4 inherited from our own
// parent can reach the child), the trusted argv, the optional cap drop, the
// profile verification, then the command tmp and the launch. Every failure
// emits one sanitized abnormal event on ev and returns ok == false WITHOUT
// running any later step, so nothing is forked.
func prelaunch(ev *evidence, args []string, p prelaunchSeams) (tmp *commandTmp, childPid int, st procStatus, ok bool) {
	if err := p.fdHygiene(); err != nil {
		_ = ev.writeJSON(abnormalEvidence("fd hygiene failed", nil))
		return nil, 0, st, false
	}
	expectUID, cleanupToken, dropControllerCaps, childArgv, err := parseArgs(args)
	if err != nil {
		_ = ev.writeJSON(abnormalEvidence("invalid arguments", nil))
		return nil, 0, st, false
	}
	if dropControllerCaps {
		if err := p.dropCaps(); err != nil {
			_ = ev.writeJSON(abnormalEvidence("profile:capDrop", nil))
			return nil, 0, st, false
		}
	}
	st, reason := p.profile(expectUID)
	if reason != "" {
		_ = ev.writeJSON(abnormalEvidence(reason, nil))
		return nil, 0, st, false
	}
	// Command tmp + launch. With a cleanup token the supervisor alone creates
	// /tmp/uzi-codex-command-<token>, flocks it for its whole lifetime (the fd is
	// close-on-exec, so the child never inherits the lock) and rechecks the name,
	// all before fork. The trusted descriptors are close-on-exec so they
	// auto-close at the child's execve (the child inherits only stdio 0/1/2),
	// then ForkExec runs into a fresh session.
	p.trustFds()
	tmp, childPid, ok = setupAndLaunch(ev, cleanupToken, expectUID, p.setup, p.launch, childArgv)
	return tmp, childPid, st, ok
}

// markTrustedFdsCloexec marks the control/evidence pair close-on-exec.
func markTrustedFdsCloexec() {
	unix.CloseOnExec(3)
	unix.CloseOnExec(4)
}

// verifyProfile establishes and verifies subreaper and nondumpability, then
// verifies the container profile, all BEFORE fork. PR_SET_DUMPABLE 0 makes the
// supervisor's /proc/<pid> root-owned, so a same-uid child cannot open
// /proc/<sup>/fd/3|4 and forge the trusted channels. It returns "" or the
// abnormal reason.
func verifyProfile(expectUID int) (procStatus, string) {
	subreaper := establishSubreaper()
	nondumpable := establishNondumpable()
	statusText, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return procStatus{}, "profile:parse"
	}
	st, err := parseProcStatus(string(statusText))
	if err != nil {
		return procStatus{}, "profile:parse"
	}
	if ok, field := evaluateProfile(st, expectUID, subreaper, nondumpable); !ok {
		return st, "profile:" + field
	}
	return st, ""
}

// watchChild returns a one-shot readiness channel backed by a pidfd. Readiness
// means the primary child is waitable; it does not reap. The supervisor's main
// loop therefore remains the sole wait authority and never races drain's
// Wait4(-1, __WALL) path.
func watchChild(pid int) (<-chan error, error) {
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return nil, err
	}
	ready := make(chan error, 1)
	go func() {
		defer unix.Close(fd)
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		for {
			_, pollErr := unix.Poll(fds, -1)
			if pollErr == unix.EINTR {
				continue
			}
			ready <- pollErr
			return
		}
	}()
	return ready, nil
}

// realReapChild reaps only the supervised primary child after its pidfd became
// readable. Detached/background descendants remain adopted by the subreaper and
// are accounted for by the later ECHILD+__WALL drain.
func realReapChild(pid int) (int, error) {
	var ws unix.WaitStatus
	got, err := unix.Wait4(pid, &ws, unix.WNOHANG|unix.WALL, nil)
	if err != nil {
		return 0, err
	}
	if got != pid {
		return 0, errors.New("child was not waitable after pidfd readiness")
	}
	if ws.Exited() {
		return ws.ExitStatus(), nil
	}
	if ws.Signaled() {
		return 128 + int(ws.Signal()), nil
	}
	return 1, nil
}

var errBadArgs = errors.New("invalid arguments")

// parseArgs parses the trusted, caller-supplied argv:
//
//	--expect-uid <N> [--cleanup-token <lowercase-uuid>] [--drop-controller-caps] -- <child-exec-abspath> [child args...]
//
// The child exec path must be absolute (never a model-selected relative target),
// and --expect-uid is mandatory.
func parseArgs(args []string) (expectUID int, cleanupToken string, dropControllerCaps bool, childArgv []string, err error) {
	seenUID := false
	i := 0
	for i < len(args) {
		switch args[i] {
		case "--expect-uid":
			if i+1 >= len(args) {
				return 0, "", false, nil, errBadArgs
			}
			n, cerr := strconv.Atoi(args[i+1])
			if cerr != nil || n < 0 {
				return 0, "", false, nil, errBadArgs
			}
			expectUID, seenUID = n, true
			i += 2
		case "--cleanup-token":
			if i+1 >= len(args) || cleanupToken != "" || !validCleanupToken(args[i+1]) {
				return 0, "", false, nil, errBadArgs
			}
			cleanupToken = args[i+1]
			i += 2
		case "--drop-controller-caps":
			if dropControllerCaps {
				return 0, "", false, nil, errBadArgs
			}
			dropControllerCaps = true
			i++
		case "--":
			child := args[i+1:]
			if !seenUID || len(child) == 0 || !strings.HasPrefix(child[0], "/") {
				return 0, "", false, nil, errBadArgs
			}
			return expectUID, cleanupToken, dropControllerCaps, child, nil
		default:
			return 0, "", false, nil, errBadArgs
		}
	}
	return 0, "", false, nil, errBadArgs
}

func clearAllCaps() error {
	hdr := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3, Pid: 0}
	data := [2]unix.CapUserData{}
	return unix.Capset(&hdr, &data[0])
}

func validCleanupToken(token string) bool {
	if len(token) != 36 {
		return false
	}
	for i, c := range token {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// firstStrayFd is the lowest fd markStrayFdsCloexec touches: 0/1/2 are the
// child's stdio and 3/4 the trusted control/evidence pair.
const firstStrayFd = 5

// procSelfFdDir lists this process's open fds.
const procSelfFdDir = "/proc/self/fd"

// fdHygiene is markStrayFdsCloexec's seams.
type fdHygiene struct {
	// closeRange marks every fd from first upward close-on-exec at once.
	closeRange func(first uint) error
	// listOpen returns the fds open in this process, excluding its own.
	listOpen func() ([]int, error)
	// set marks one fd close-on-exec.
	set func(fd int) error
}

var realFdHygiene = fdHygiene{
	closeRange: closeRangeCloexec,
	listOpen:   listOpenFds,
	set:        setCloexec,
}

// listOpenFds reads the open fd numbers from procSelfFdDir through one
// O_CLOEXEC directory fd, which it leaves out of the result. A name that is
// not a number is an error.
func listOpenFds() ([]int, error) {
	dfd, err := unix.Open(procSelfFdDir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = unix.Close(dfd) }()
	buf := make([]byte, 4096)
	var fds []int
	var names []string
	for {
		n, err := unix.Getdents(dfd, buf)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if n <= 0 {
			return fds, nil
		}
		_, _, names = unix.ParseDirent(buf[:n], -1, names[:0])
		for _, name := range names {
			fd, err := strconv.Atoi(name)
			if err != nil {
				return nil, fmt.Errorf("fd entry %q is not a number", name)
			}
			if fd != dfd {
				fds = append(fds, fd)
			}
		}
	}
}

// closeRangeCloexec sets close-on-exec on every fd from first upward in one
// close_range(CLOSE_RANGE_CLOEXEC) call, without closing any.
func closeRangeCloexec(first uint) error {
	return unix.CloseRange(first, ^uint(0), unix.CLOSE_RANGE_CLOEXEC)
}

// setCloexec sets FD_CLOEXEC on one fd (the only fd flag).
func setCloexec(fd int) error {
	_, err := unix.FcntlInt(uintptr(fd), unix.F_SETFD, unix.FD_CLOEXEC)
	return err
}

// markStrayFdsCloexec makes every fd from firstStrayFd upward close-on-exec,
// so an fd the supervisor itself inherited without close-on-exec can never
// reach the child, whatever its number. It tries, in order:
//
//  1. close_range(CLOSE_RANGE_CLOEXEC) over the whole range. It fails with
//     ENOSYS or EINVAL on a kernel without it or without CLOSE_RANGE_CLOEXEC,
//     and with EPERM where a seccomp profile denies it (as measured in this
//     repo's dev sandbox);
//  2. set on every fd procSelfFdDir lists (the open ones, at any number).
//
// An EBADF from set is an fd that is not (or no longer) open and is skipped.
// There is deliberately no third, numeric sweep: RLIMIT_NOFILE does not bound
// the fds already open (lowering it closes nothing), so a sweep to any limit
// could miss a stray fd above it. When both steps fail the error is returned
// and the caller must not fork (fail closed).
func markStrayFdsCloexec(h fdHygiene) error {
	crErr := h.closeRange(firstStrayFd)
	if crErr == nil {
		return nil
	}
	fds, err := h.listOpen()
	if err != nil {
		return fmt.Errorf("close_range: %v; fd listing: %w", crErr, err)
	}
	if err := setEach(fds, h.set); err != nil {
		return fmt.Errorf("close_range: %v; fd listing set: %w", crErr, err)
	}
	return nil
}

// setEach runs set on every fd in fds from firstStrayFd upward, skipping EBADF.
func setEach(fds []int, set func(fd int) error) error {
	for _, fd := range fds {
		if fd < firstStrayFd {
			continue
		}
		if err := set(fd); err != nil && !errors.Is(err, unix.EBADF) {
			return err
		}
	}
	return nil
}

// establishSubreaper sets PR_SET_CHILD_SUBREAPER then CONFIRMS it via
// PR_GET_CHILD_SUBREAPER == 1. A failed set or an unconfirmed get returns false,
// which the profile decision surfaces as "profile:subreaper".
func establishSubreaper() bool {
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		return false
	}
	// PR_GET_CHILD_SUBREAPER returns the setting via an int* in arg2. Passing the
	// stack address as a uintptr directly to syscall.Syscall is the sanctioned
	// unsafe.Pointer pattern (the runtime keeps &v pinned across this call).
	var v int32
	_, _, errno := syscall.Syscall(uintptr(unix.SYS_PRCTL), uintptr(unix.PR_GET_CHILD_SUBREAPER), uintptr(unsafe.Pointer(&v)), 0)
	if errno != 0 {
		return false
	}
	return v == 1
}

// establishNondumpable sets PR_SET_DUMPABLE 0 then CONFIRMS it via
// PR_GET_DUMPABLE == 0 (that get returns the value as the syscall result).
func establishNondumpable() bool {
	if err := unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
		return false
	}
	v, err := unix.PrctlRetInt(unix.PR_GET_DUMPABLE, 0, 0, 0, 0)
	if err != nil {
		return false
	}
	return v == 0
}

// launchChild forks and execs the child with only stdio 0/1/2 in its file table
// (fd3/fd4 are close-on-exec) and Setsid so it leads a fresh session. The child
// INHERITS the supervisor's own environment (os.Environ()): the launcher already
// replaced this process's env with its explicit allowlist (fresh HOME/CODEX_HOME/
// XDG/TMPDIR/PATH + the provider credential for a provider root) and setpriv passed
// it through unchanged, so forwarding it verbatim delivers exactly that allowlist
// to the app-server. A nil Env would ForkExec an EMPTY envp and start Codex with no
// CODEX_HOME/PATH/credential — the M0 fixture's os.execv inherited env implicitly.
func launchChild(childArgv []string) (int, error) {
	attr := &syscall.ProcAttr{
		Env:   os.Environ(),
		Files: []uintptr{0, 1, 2},
		Sys:   &syscall.SysProcAttr{Setsid: true},
	}
	return syscall.ForkExec(childArgv[0], childArgv, attr)
}

// selfDirectChildren lists the supervisor's current direct children.
func selfDirectChildren() []int {
	return childrenOf(os.Getpid())
}

// realKill pidfd-opens the pid and delivers SIGKILL. A vanished process (ESRCH)
// is returned as an error so the drain does not record it as killed.
func realKill(pid int) error {
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	return unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0)
}

// realReap is one non-blocking Wait4 over ALL children including differently-
// grouped/threaded descendants (__WALL). ECHILD (no children remain) is the
// drain authority.
func realReap() (int, error) {
	var ws unix.WaitStatus
	pid, err := unix.Wait4(-1, &ws, unix.WNOHANG|unix.WALL, nil)
	return pid, err
}
