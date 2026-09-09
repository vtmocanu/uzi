// Command uzi-codex-command-sandbox applies a fail-closed Landlock filesystem
// policy before starting one model-authorized command. It is launched only as
// runner-cmd (uid 10003) beneath uzi-codex-supervisor.
package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	landlockRulePathBeneath = 1
	landlockVersionFlag     = 1
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

func main() { os.Exit(realMain(os.Args[1:])) }

func realMain(args []string) int {
	root, tmp, cwd, child, err := parseArgs(args)
	if err != nil {
		return 2
	}
	if err := os.Mkdir(tmp, 0o700); err != nil {
		return 2
	}
	defer os.RemoveAll(tmp)
	if err := os.Chmod(tmp, 0o700); err != nil {
		return 2
	}
	if err := confine(root, tmp); err != nil {
		return 2
	}
	if err := os.Chdir(cwd); err != nil {
		return 2
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
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return err
	}
	_, _, errno = syscall.Syscall(unix.SYS_LANDLOCK_RESTRICT_SELF, fd, 0, 0)
	if errno != 0 {
		return errno
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
