// Command uzi-codex-command-sandbox applies a fail-closed Landlock filesystem
// policy before starting one model-authorized command. It is launched only as
// runner-cmd (uid 10003) beneath uzi-codex-supervisor.
package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	landlockRulePathBeneath = 1
	landlockVersionFlag     = 1
	landlockDenyProbe       = "/app/package.json"
)

var baseRights = uint64(
	unix.LANDLOCK_ACCESS_FS_EXECUTE |
		unix.LANDLOCK_ACCESS_FS_WRITE_FILE |
		unix.LANDLOCK_ACCESS_FS_READ_FILE |
		unix.LANDLOCK_ACCESS_FS_READ_DIR |
		unix.LANDLOCK_ACCESS_FS_REMOVE_DIR |
		unix.LANDLOCK_ACCESS_FS_REMOVE_FILE |
		unix.LANDLOCK_ACCESS_FS_MAKE_CHAR |
		unix.LANDLOCK_ACCESS_FS_MAKE_DIR |
		unix.LANDLOCK_ACCESS_FS_MAKE_REG |
		unix.LANDLOCK_ACCESS_FS_MAKE_SOCK |
		unix.LANDLOCK_ACCESS_FS_MAKE_FIFO |
		unix.LANDLOCK_ACCESS_FS_MAKE_BLOCK |
		unix.LANDLOCK_ACCESS_FS_MAKE_SYM)

func main() {
	os.Exit(runOnLockedThread(os.Args[1:], runtime.LockOSThread, realMain))
}

func runOnLockedThread(args []string, lock func(), run func([]string) int) int {
	// Landlock ABI 6 restricts only the calling OS thread and its descendants. Pin
	// this goroutine before any policy setup and never unlock, so Go cannot migrate
	// it between restrict_self, the deny probe and the child fork/exec.
	lock()
	return run(args)
}

func setupFailure(stage string, err error) int {
	_, _ = fmt.Fprintf(os.Stderr, "uzi-codex-command-sandbox: %s: %v\n", stage, err)
	return 2
}

func realMain(args []string) int {
	root, tmp, cwd, child, err := parseArgs(args)
	if err != nil {
		return setupFailure("invalid arguments", err)
	}
	if err := os.Mkdir(tmp, 0o700); err != nil {
		return setupFailure("create private tmp", err)
	}
	defer os.RemoveAll(tmp)
	if err := os.Chmod(tmp, 0o700); err != nil {
		return setupFailure("harden private tmp", err)
	}
	if err := confine(root, tmp); err != nil {
		return setupFailure("apply Landlock policy", err)
	}
	if err := os.Chdir(cwd); err != nil {
		return setupFailure("enter command cwd", err)
	}
	cmd := exec.Command(child[0], child[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = os.Environ()
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			if status, ok := exit.Sys().(syscall.WaitStatus); ok {
				if status.Exited() {
					return status.ExitStatus()
				}
				if status.Signaled() {
					return 128 + int(status.Signal())
				}
			}
		}
		return 1
	}
	return 0
}

func parseArgs(args []string) (root, tmp, cwd string, child []string, err error) {
	if len(args) < 8 || args[0] != "--root" || args[2] != "--tmp" || args[4] != "--cwd" || args[6] != "--" {
		return "", "", "", nil, errors.New("invalid arguments")
	}
	root, tmp, cwd, child = args[1], args[3], args[5], args[7:]
	if !filepath.IsAbs(root) || !filepath.IsAbs(tmp) || !filepath.IsAbs(cwd) || len(child) == 0 || !filepath.IsAbs(child[0]) {
		return "", "", "", nil, errors.New("paths must be absolute")
	}
	if filepath.Clean(root) == string(filepath.Separator) {
		return "", "", "", nil, errors.New("root sandbox is forbidden")
	}
	rel, relErr := filepath.Rel(root, cwd)
	if relErr != nil || rel == ".." || (len(rel) >= 3 && rel[:3] == ".."+string(filepath.Separator)) {
		return "", "", "", nil, errors.New("cwd escapes root")
	}
	return root, tmp, cwd, child, nil
}

func confine(root, tmp string) error {
	abi, _, errno := syscall.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, landlockVersionFlag)
	if errno != 0 || abi < 1 {
		return errors.New("landlock unavailable")
	}
	handled := baseRights
	if abi >= 2 {
		handled |= unix.LANDLOCK_ACCESS_FS_REFER
	}
	if abi >= 3 {
		handled |= unix.LANDLOCK_ACCESS_FS_TRUNCATE
	}
	attr := unix.LandlockRulesetAttr{Access_fs: handled}
	fd, _, errno := syscall.Syscall(
		unix.SYS_LANDLOCK_CREATE_RULESET,
		uintptr(unsafe.Pointer(&attr)),
		unsafe.Sizeof(attr),
		0,
	)
	if errno != 0 {
		return errno
	}
	defer unix.Close(int(fd))
	read := uint64(unix.LANDLOCK_ACCESS_FS_EXECUTE | unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_READ_DIR)
	for _, path := range []string{"/bin", "/sbin", "/usr", "/lib", "/lib64", "/etc", "/nix", "/opt/uzi-toolchain"} {
		if err := addPathRule(int(fd), path, read&handled); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	dev := read | unix.LANDLOCK_ACCESS_FS_WRITE_FILE
	if err := addPathRule(int(fd), "/dev", dev&handled); err != nil {
		return err
	}
	if err := addPathRule(int(fd), root, handled); err != nil {
		return err
	}
	if err := addPathRule(int(fd), tmp, handled); err != nil {
		return err
	}
	if err := requireProbeReadable(landlockDenyProbe, os.Open); err != nil {
		return err
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return err
	}
	_, _, errno = syscall.Syscall(unix.SYS_LANDLOCK_RESTRICT_SELF, fd, 0, 0)
	if errno != 0 {
		return errno
	}
	if err := requireProbeDenied(landlockDenyProbe, os.Open); err != nil {
		return err
	}
	return nil
}

type probeOpen func(string) (*os.File, error)

// requireProbeReadable proves the deny sentinel exists and ordinary DAC/LSM policy
// allows it before Landlock is applied, so the post-restriction denial is discriminating.
func requireProbeReadable(path string, open probeOpen) error {
	file, err := open(path)
	if err != nil {
		return errors.New("Landlock deny probe was not readable before restriction")
	}
	if err := file.Close(); err != nil {
		return errors.New("Landlock deny probe could not be closed")
	}
	return nil
}

// requireProbeDenied positively observes that the policy attached to the locked OS
// thread before any model command is forked. The file is baked, world-readable and
// outside every allowlist; its pre-restriction check prevents DAC or another LSM from
// producing a false green.
func requireProbeDenied(path string, open probeOpen) error {
	file, err := open(path)
	if err == nil {
		_ = file.Close()
		return errors.New("Landlock deny probe unexpectedly readable")
	}
	if !errors.Is(err, os.ErrPermission) {
		return errors.New("Landlock deny probe returned an unexpected error")
	}
	return nil
}

func addPathRule(ruleset int, path string, access uint64) error {
	fd, err := unix.Open(path, unix.O_PATH|unix.O_CLOEXEC, 0)
	if err != nil {
		return &os.PathError{Op: "open", Path: path, Err: err}
	}
	defer unix.Close(fd)
	attr := unix.LandlockPathBeneathAttr{Allowed_access: access, Parent_fd: int32(fd)}
	_, _, errno := syscall.Syscall6(
		unix.SYS_LANDLOCK_ADD_RULE,
		uintptr(ruleset),
		landlockRulePathBeneath,
		uintptr(unsafe.Pointer(&attr)),
		0, 0, 0,
	)
	if errno != 0 {
		return errno
	}
	return nil
}
