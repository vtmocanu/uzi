package main

import (
	"errors"
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

// realMain is the whole flow: parse the trusted argv, establish + verify the
// subreaper/nondumpable posture, verify the container profile BEFORE fork, launch
// the child with fd3/fd4 closed at its execve, then run the control loop. Every
// pre-fork failure emits a sanitized abnormal event on fd 4 and exits non-zero
// WITHOUT forking.
func realMain(args []string) int {
	ev := &evidence{w: os.NewFile(4, "evidence")}

	expectUID, cleanupToken, dropControllerCaps, childArgv, err := parseArgs(args)
	if err != nil {
		_ = ev.writeJSON(abnormalEvidence("invalid arguments", nil))
		return 2
	}
	if dropControllerCaps {
		if err := clearAllCaps(); err != nil {
			_ = ev.writeJSON(abnormalEvidence("profile:capDrop", nil))
			return 2
		}
	}

	// (1) Establish + verify subreaper and nondumpability BEFORE fork. PR_SET_DUMPABLE
	// 0 makes the supervisor's /proc/<pid> root-owned, so a same-uid child cannot
	// open /proc/<sup>/fd/3|4 and forge the trusted channels.
	subreaper := establishSubreaper()
	nondumpable := establishNondumpable()

	// (2) Profile verification, fail-before-fork.
	statusText, rerr := os.ReadFile("/proc/self/status")
	if rerr != nil {
		_ = ev.writeJSON(abnormalEvidence("profile:parse", nil))
		return 2
	}
	st, perr := parseProcStatus(string(statusText))
	if perr != nil {
		_ = ev.writeJSON(abnormalEvidence("profile:parse", nil))
		return 2
	}
	if ok, field := evaluateProfile(st, expectUID, subreaper, nondumpable); !ok {
		_ = ev.writeJSON(abnormalEvidence("profile:"+field, nil))
		return 2
	}

	// (3) Launch. Mark the trusted descriptors close-on-exec so they auto-close
	// at the child's execve (the child inherits only stdio 0/1/2), then ForkExec
	// into a fresh session.
	unix.CloseOnExec(3)
	unix.CloseOnExec(4)
	childPid, lerr := launchChild(childArgv)
	if lerr != nil {
		_ = ev.writeJSON(abnormalEvidence("child launch failed", nil))
		return 2
	}
	// The supervised child inherited stdio 0/1/2. Drop the supervisor's copies so
	// EOF/backpressure describe the child tree rather than this long-lived control
	// process; control/evidence remain isolated on fd3/fd4.
	_ = unix.Close(0)
	_ = unix.Close(1)
	_ = unix.Close(2)
	childReady, readyErr := watchChild(childPid)
	if readyErr != nil {
		deadline := time.Now().Add(time.Duration(defaultDisposeTimeoutMs) * time.Millisecond)
		cleanup := drain(deadline, time.Now, func() { time.Sleep(2 * time.Millisecond) }, selfDirectChildren, realKill, realReap)
		_ = ev.writeJSON(abnormalEvidence("child watch failed", &cleanup))
		removeCommandTmp(cleanupToken)
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
			childReady:     childReady,
			reapChild:      realReapChild,
			snapshot:       func() ([]procRow, error) { return walkDescendants(os.Getpid()) },
			now:            time.Now,
			sleep:          func() { time.Sleep(2 * time.Millisecond) },
		},
	}
	code := sup.run(childPid, st)
	removeCommandTmp(cleanupToken)
	return code
}

func removeCommandTmp(token string) {
	if token != "" {
		_ = os.RemoveAll("/tmp/uzi-codex-command-" + token)
	}
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
//	--expect-uid <N> [--drop-controller-caps] -- <child-exec-abspath> [child args...]
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
