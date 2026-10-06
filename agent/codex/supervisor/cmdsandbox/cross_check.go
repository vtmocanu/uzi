package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// crossCheckMain is a separate required-only policy. The trusted launcher supplies
// one canonical checkout and one private launch tree; neither comes from a callback.
// There is no optional enforcement mode and no command-cache grant.
func crossCheckMain(args []string) int {
	return crossCheckRun(args, realVersionProbe, confine)
}

func crossCheckRun(args []string, probe versionProbe, confineFn confineFunc) int {
	root, state, cwd, child, err := parseCrossCheckArgs(args)
	if err != nil {
		return setupFailure("cross-check arguments", err)
	}
	stateFd, err := unix.Open(state, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return setupFailure("cross-check state", err)
	}
	defer unix.Close(stateFd)
	if err := verifyCrossCheckState(stateFd, os.Getuid(), unix.Fstat); err != nil {
		return setupFailure("cross-check state", err)
	}
	if err := applyPolicy(root, grantFds{tmp: stateFd, cache: -1, readOnly: true}, modeRequired, probe, confineFn, applyNoNewPrivs); err != nil {
		return setupFailure("cross-check required confinement", err)
	}
	if err := os.Chdir(cwd); err != nil {
		return setupFailure("cross-check cwd", err)
	}
	cmd := exec.Command(child[0], child[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr, cmd.Env = os.Stdin, os.Stdout, os.Stderr, os.Environ()
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

// verifyCrossCheckState checks the opened inode, including caller uid and all
// permission bits. Managed auth uses 0710 for the session-reader traverse path.
func verifyCrossCheckState(fd, uid int, fstat func(int, *unix.Stat_t) error) error {
	var st unix.Stat_t
	if err := fstat(fd, &st); err != nil {
		return err
	}
	mode := st.Mode & 0o7777
	if st.Mode&unix.S_IFMT != unix.S_IFDIR || st.Uid != uint32(uid) || (mode != 0o700 && mode != 0o710) {
		return errors.New("private launch tree ownership or mode")
	}
	return nil
}

func parseCrossCheckArgs(args []string) (root, state, cwd string, child []string, err error) {
	fail := func() (string, string, string, []string, error) {
		return "", "", "", nil, errors.New("expected --root R --state S --cwd C -- absolute-child")
	}
	if len(args) < 8 || args[0] != "--root" || args[2] != "--state" || args[4] != "--cwd" || args[6] != "--" {
		return fail()
	}
	root, state, cwd, child = args[1], args[3], args[5], args[7:]
	for _, p := range []string{root, state, cwd} {
		real, e := filepath.EvalSymlinks(p)
		if e != nil || !filepath.IsAbs(p) || filepath.Clean(p) != p || real != p || p == "/" {
			return fail()
		}
	}
	inside := func(parent, p string) bool { return p == parent || strings.HasPrefix(p, parent+"/") }
	if !inside(root, cwd) || inside(root, state) || inside(state, root) || !filepath.IsAbs(child[0]) {
		return fail()
	}
	// Broad system grants must never overlap a checkout or its private state.
	for _, p := range []string{"/bin", "/sbin", "/usr", "/lib", "/lib64", "/etc", "/dev", "/nix", "/opt/uzi-toolchain", "/opt/uzi-codex"} {
		if inside(p, root) || inside(root, p) || inside(p, state) || inside(state, p) {
			return fail()
		}
	}
	return root, state, cwd, child, nil
}
