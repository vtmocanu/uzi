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
// subreaper/dumpable posture, verify the container profile BEFORE fork, launch
// the child with fd3/fd4 closed at its execve, then run the control loop. Every
// pre-fork failure emits a sanitized abnormal event on fd 4 and exits non-zero
// WITHOUT forking.
func realMain(args []string) int {
	ev := &evidence{w: os.NewFile(4, "evidence")}

	expectUID, childArgv, err := parseArgs(args)
	if err != nil {
		_ = ev.writeJSON(abnormalEvidence("invalid arguments", nil))
		return 2
	}

	// (1) Establish + verify subreaper and dumpable BEFORE fork. PR_SET_DUMPABLE
	// 0 makes the supervisor's /proc/<pid> root-owned, so a same-uid child cannot
	// open /proc/<sup>/fd/3|4 and forge the trusted channels.
	subreaper := establishSubreaper()
	dumpable := establishDumpable()

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
	if ok, field := evaluateProfile(st, expectUID, subreaper, dumpable); !ok {
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

	sup := &supervisor{
		ev:      ev,
		control: &controlReader{r: os.NewFile(3, "control")},
		seams: seams{
			supervisorPid:  os.Getpid(),
			directChildren: selfDirectChildren,
			kill:           realKill,
			reap:           realReap,
			snapshot:       func() ([]procRow, error) { return walkDescendants(os.Getpid()) },
			now:            time.Now,
			sleep:          func() { time.Sleep(2 * time.Millisecond) },
		},
	}
	return sup.run(childPid, st.UID)
}

var errBadArgs = errors.New("invalid arguments")

// parseArgs parses the trusted, caller-supplied argv:
//
//	--expect-uid <N> -- <child-exec-abspath> [child args...]
//
// The child exec path must be absolute (never a model-selected relative target),
// and --expect-uid is mandatory.
func parseArgs(args []string) (expectUID int, childArgv []string, err error) {
	seenUID := false
	i := 0
	for i < len(args) {
		switch args[i] {
		case "--expect-uid":
			if i+1 >= len(args) {
				return 0, nil, errBadArgs
			}
			n, cerr := strconv.Atoi(args[i+1])
			if cerr != nil || n < 0 {
				return 0, nil, errBadArgs
			}
			expectUID, seenUID = n, true
			i += 2
		case "--":
			child := args[i+1:]
			if !seenUID || len(child) == 0 || !strings.HasPrefix(child[0], "/") {
				return 0, nil, errBadArgs
			}
			return expectUID, child, nil
		default:
			return 0, nil, errBadArgs
		}
	}
	return 0, nil, errBadArgs
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

// establishDumpable sets PR_SET_DUMPABLE 0 then CONFIRMS it via
// PR_GET_DUMPABLE == 0 (that get returns the value as the syscall result).
func establishDumpable() bool {
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
