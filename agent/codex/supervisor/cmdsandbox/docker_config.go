package main

import (
	"errors"
	"fmt"
	"strings"

	"golang.org/x/sys/unix"
)

const dockerConfigName = ".docker"

// dockerParent is what the run home (the parent of .docker) must look like:
// owned by the worker (uid) with the runner group (gid) and mode exactly
// setgid+sticky+rwxrwx--- (03770), as the trusted worker prepares it.
type dockerParent struct {
	uid, gid uint32
	mode     uint32
}

var productionDockerParent = dockerParent{uid: 10001, gid: 10002, mode: 0o3770}

// dockerConfigSys are the syscalls setupDockerConfig makes, injectable so a test
// can fake an owner it cannot create.
type dockerConfigSys struct {
	open    func(path string, flags int, mode uint32) (int, error)
	openat  func(dirfd int, path string, flags int, mode uint32) (int, error)
	mkdirat func(dirfd int, path string, mode uint32) error
	fstat   func(fd int, st *unix.Stat_t) error
	fchmod  func(fd int, mode uint32) error
}

var realDockerConfigSys = dockerConfigSys{
	open:    unix.Open,
	openat:  unix.Openat,
	mkdirat: unix.Mkdirat,
	fstat:   unix.Fstat,
	fchmod:  unix.Fchmod,
}

// setupDockerConfig prepares "<parent>/.docker" on one fd chain and returns the
// verified directory fd (close-on-exec) for the Landlock rule. Every step works
// on an fd, never a path twice: the parent is opened O_PATH|O_NOFOLLOW and its
// owner, group and mode verified; .docker is created with mkdirat (EEXIST is
// fine), opened O_NOFOLLOW|O_DIRECTORY, checked to be a directory owned by uid,
// then fchmod'ed to 0700 unconditionally (that repairs an inherited setgid bit,
// a first-command race, a crash between mkdir and chmod, or a model's chmod) and
// re-verified. On any failure the fd is closed and the error returned; its text
// is built only from fixed strings, numbers and errno text, never a path or a
// directory entry name.
func setupDockerConfig(parent string, sys dockerConfigSys, uid int, want dockerParent) (int, error) {
	pfd, err := sys.open(parent, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("open run home: %w", err)
	}
	defer unix.Close(pfd)
	var pst unix.Stat_t
	if err := sys.fstat(pfd, &pst); err != nil {
		return -1, fmt.Errorf("fstat run home: %w", err)
	}
	if pst.Mode&unix.S_IFMT != unix.S_IFDIR {
		return -1, errors.New("run home is not a directory")
	}
	if pst.Uid != want.uid || pst.Gid != want.gid {
		return -1, fmt.Errorf("run home owned by %d:%d, want %d:%d", pst.Uid, pst.Gid, want.uid, want.gid)
	}
	if m := pst.Mode & 0o7777; m != want.mode {
		return -1, fmt.Errorf("run home mode %04o, want %04o", m, want.mode)
	}
	if err := sys.mkdirat(pfd, dockerConfigName, 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
		return -1, fmt.Errorf("mkdir: %w", err)
	}
	fd, err := sys.openat(pfd, dockerConfigName, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("open: %w", err)
	}
	fail := func(err error) (int, error) {
		_ = unix.Close(fd)
		return -1, err
	}
	var st unix.Stat_t
	if err := sys.fstat(fd, &st); err != nil {
		return fail(fmt.Errorf("fstat: %w", err))
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return fail(errors.New("not a directory"))
	}
	if st.Uid != uint32(uid) {
		return fail(fmt.Errorf("owned by uid %d, want %d", st.Uid, uid))
	}
	if err := sys.fchmod(fd, 0o700); err != nil {
		return fail(fmt.Errorf("fchmod: %w", err))
	}
	if err := verifyPrivateDir(fd, uid, sys.fstat); err != nil {
		return fail(err)
	}
	return fd, nil
}

// withoutDockerConfig returns a copy of an environment list without any
// DOCKER_CONFIG entry, duplicates included.
func withoutDockerConfig(vars []string) []string {
	out := make([]string, 0, len(vars))
	for _, e := range vars {
		if !strings.HasPrefix(e, "DOCKER_CONFIG=") {
			out = append(out, e)
		}
	}
	return out
}
