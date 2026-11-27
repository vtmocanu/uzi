package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// testDockerParent builds a run home the way the trusted worker prepares it
// (setgid+sticky, rwxrwx---) but owned by the test's own uid and gid, and the
// expectation struct that accepts it.
func testDockerParent(t *testing.T) (string, dockerParent) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "runhome")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := unix.Chmod(dir, 0o3770); err != nil {
		t.Fatal(err)
	}
	return dir, dockerParent{uid: uint32(os.Getuid()), gid: uint32(os.Getgid()), mode: 0o3770}
}

func modeOf(t *testing.T, path string) uint32 {
	t.Helper()
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		t.Fatal(err)
	}
	return st.Mode & 0o7777
}

func TestParseArgsDockerConfig(t *testing.T) {
	cacheA := "/var/cache/uzi-codex-cmd/" + runTokenA
	const docker = "/data/runs/r1/.docker"
	base := []string{"--root", "/data/run", "--tmp", "/tmp/run", "--cwd", "/data/run"}
	with := func(extra ...string) []string { return append(append([]string{}, base...), extra...) }

	_, _, _, cache, dc, mode, null, child, err := parseArgs(with("--cache", cacheA, "--docker-config", docker, "--mode", "off", "--stdin", "null", "--", "/bin/true"))
	if err != nil || cache != cacheA || dc != docker || mode != modeOff || !null || !reflect.DeepEqual(child, []string{"/bin/true"}) {
		t.Fatalf("all flags: cache=%q dc=%q mode=%q null=%v child=%v err=%v", cache, dc, mode, null, child, err)
	}
	if _, _, _, _, dc, _, _, _, err := parseArgs(with("--docker-config", docker, "--", "/bin/true")); err != nil || dc != docker {
		t.Fatalf("without cache: dc=%q err=%v", dc, err)
	}
	if _, _, _, _, dc, _, _, _, err := parseArgs(with("--", "/bin/true")); err != nil || dc != "" {
		t.Fatalf("absent: dc=%q err=%v", dc, err)
	}
	// After the separator the token is child argv.
	_, _, _, _, dc, _, _, child, err = parseArgs(with("--", "/bin/sh", "--docker-config", "/x/.docker"))
	if err != nil || dc != "" || !reflect.DeepEqual(child, []string{"/bin/sh", "--docker-config", "/x/.docker"}) {
		t.Fatalf("after --: dc=%q child=%v err=%v", dc, child, err)
	}

	for name, args := range map[string][]string{
		"relative":       with("--docker-config", "run/.docker", "--", "/bin/true"),
		"unclean":        with("--docker-config", "/data/runs/../runs/.docker", "--", "/bin/true"),
		"trailing slash": with("--docker-config", docker+"/", "--", "/bin/true"),
		"wrong basename": with("--docker-config", "/data/runs/r1/docker", "--", "/bin/true"),
		"nested":         with("--docker-config", docker+"/x", "--", "/bin/true"),
		"root":           with("--docker-config", "/", "--", "/bin/true"),
		"missing value":  with("--docker-config", "--", "/bin/true"),
		"repeated":       with("--docker-config", docker, "--docker-config", docker, "--", "/bin/true"),
		"before cache":   with("--docker-config", docker, "--cache", cacheA, "--", "/bin/true"),
		"after mode":     with("--mode", "off", "--docker-config", docker, "--", "/bin/true"),
		"after stdin":    with("--stdin", "null", "--docker-config", docker, "--", "/bin/true"),
	} {
		if _, _, _, _, _, _, _, _, err := parseArgs(args); err == nil {
			t.Errorf("%s: accepted %v", name, args)
		}
	}
}

func TestSetupDockerConfigCreatesWhenAbsent(t *testing.T) {
	parent, want := testDockerParent(t)
	fd, err := setupDockerConfig(parent, realDockerConfigSys, os.Getuid(), want)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(fd) }()
	dir := filepath.Join(parent, ".docker")
	if m := modeOf(t, dir); m != 0o700 {
		t.Fatalf("created mode %04o, want 0700 (the parent's setgid must not be inherited)", m)
	}
	flags, ferr := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	if ferr != nil || flags&unix.FD_CLOEXEC == 0 {
		t.Fatalf("fd not close-on-exec (flags=%d err=%v)", flags, ferr)
	}
	var viaFd, viaPath unix.Stat_t
	if err := unix.Fstat(fd, &viaFd); err != nil {
		t.Fatal(err)
	}
	if err := unix.Stat(dir, &viaPath); err != nil || viaFd.Ino != viaPath.Ino {
		t.Fatalf("fd does not name the created dir: %v", err)
	}
}

func TestSetupDockerConfigRepairsExistingModes(t *testing.T) {
	for name, mode := range map[string]uint32{
		"inherited setgid 02700": 0o2700,
		"self-owned 0755":        0o755,
		"already 0700":           0o700,
		"model widened 0777":     0o777,
	} {
		t.Run(name, func(t *testing.T) {
			parent, want := testDockerParent(t)
			dir := filepath.Join(parent, ".docker")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := unix.Chmod(dir, mode); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte("{}"), 0o600); err != nil {
				t.Fatal(err)
			}
			fd, err := setupDockerConfig(parent, realDockerConfigSys, os.Getuid(), want)
			if err != nil {
				t.Fatal(err)
			}
			_ = unix.Close(fd)
			if m := modeOf(t, dir); m != 0o700 {
				t.Fatalf("mode %04o after setup, want 0700", m)
			}
			if _, err := os.Stat(filepath.Join(dir, "config.json")); err != nil {
				t.Fatalf("existing content must survive: %v", err)
			}
		})
	}
}

func TestSetupDockerConfigDegrades(t *testing.T) {
	t.Run("symlink .docker", func(t *testing.T) {
		parent, want := testDockerParent(t)
		target := privateTmp(t)
		if err := os.Symlink(target, filepath.Join(parent, ".docker")); err != nil {
			t.Fatal(err)
		}
		if fd, err := setupDockerConfig(parent, realDockerConfigSys, os.Getuid(), want); err == nil {
			_ = unix.Close(fd)
			t.Fatal("followed a symlinked .docker")
		}
		if m := modeOf(t, target); m != 0o700 {
			t.Fatalf("symlink target was chmodded to %04o", m)
		}
	})
	t.Run("non-directory .docker", func(t *testing.T) {
		parent, want := testDockerParent(t)
		if err := os.WriteFile(filepath.Join(parent, ".docker"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if fd, err := setupDockerConfig(parent, realDockerConfigSys, os.Getuid(), want); err == nil {
			_ = unix.Close(fd)
			t.Fatal("accepted a regular file")
		}
	})
	t.Run("foreign-owner .docker via stat seam", func(t *testing.T) {
		parent, want := testDockerParent(t)
		dir := filepath.Join(parent, ".docker")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := unix.Chmod(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		var dirIno uint64
		var st unix.Stat_t
		if err := unix.Stat(dir, &st); err != nil {
			t.Fatal(err)
		}
		dirIno = st.Ino
		sys := realDockerConfigSys
		sys.fstat = func(fd int, st *unix.Stat_t) error {
			if err := unix.Fstat(fd, st); err != nil {
				return err
			}
			if st.Ino == dirIno {
				st.Uid++
			}
			return nil
		}
		fd, err := setupDockerConfig(parent, sys, os.Getuid(), want)
		if err == nil || !strings.Contains(err.Error(), "owned by uid") {
			if err == nil {
				_ = unix.Close(fd)
			}
			t.Fatalf("foreign owner = %v", err)
		}
		if m := modeOf(t, dir); m != 0o755 {
			t.Fatalf("foreign dir was chmodded to %04o", m)
		}
	})
	t.Run("fchmod failure closes the fd", func(t *testing.T) {
		parent, want := testDockerParent(t)
		var opened int = -1
		sys := realDockerConfigSys
		sys.openat = func(dirfd int, p string, flags int, mode uint32) (int, error) {
			fd, err := unix.Openat(dirfd, p, flags, mode)
			opened = fd
			return fd, err
		}
		boom := errors.New("boom")
		sys.fchmod = func(int, uint32) error { return boom }
		if _, err := setupDockerConfig(parent, sys, os.Getuid(), want); !errors.Is(err, boom) {
			t.Fatalf("err = %v", err)
		}
		if err := unix.Fstat(opened, &unix.Stat_t{}); !errors.Is(err, unix.EBADF) {
			t.Fatalf("fd %d still open after failure: %v", opened, err)
		}
	})
	t.Run("fchmod that does not take effect is caught by the re-check", func(t *testing.T) {
		parent, want := testDockerParent(t)
		dir := filepath.Join(parent, ".docker")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := unix.Chmod(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		sys := realDockerConfigSys
		sys.fchmod = func(int, uint32) error { return nil }
		if fd, err := setupDockerConfig(parent, sys, os.Getuid(), want); err == nil {
			_ = unix.Close(fd)
			t.Fatal("accepted a 0755 directory without the 0700 re-check")
		}
	})
	t.Run("bad parent", func(t *testing.T) {
		for name, tc := range map[string]func(dockerParent) dockerParent{
			"wrong mode":  func(w dockerParent) dockerParent { w.mode = 0o770; return w },
			"wrong uid":   func(w dockerParent) dockerParent { w.uid++; return w },
			"wrong gid":   func(w dockerParent) dockerParent { w.gid++; return w },
			"plain 0700 ": func(w dockerParent) dockerParent { w.mode = 0o700; return w },
		} {
			parent, want := testDockerParent(t)
			if fd, err := setupDockerConfig(parent, realDockerConfigSys, os.Getuid(), tc(want)); err == nil {
				_ = unix.Close(fd)
				t.Errorf("%s: accepted", name)
			}
			if _, err := os.Lstat(filepath.Join(parent, ".docker")); err == nil {
				t.Errorf("%s: created .docker under a refused parent", name)
			}
		}
		// The real parent mode really is refused when it lacks setgid+sticky.
		parent, want := testDockerParent(t)
		if err := unix.Chmod(parent, 0o770); err != nil {
			t.Fatal(err)
		}
		if fd, err := setupDockerConfig(parent, realDockerConfigSys, os.Getuid(), want); err == nil || !strings.Contains(err.Error(), "run home mode 0770") {
			if err == nil {
				_ = unix.Close(fd)
			}
			t.Fatalf("0770 parent = %v", err)
		}
	})
	t.Run("symlinked parent", func(t *testing.T) {
		parent, want := testDockerParent(t)
		link := filepath.Join(t.TempDir(), "link")
		if err := os.Symlink(parent, link); err != nil {
			t.Fatal(err)
		}
		if fd, err := setupDockerConfig(link, realDockerConfigSys, os.Getuid(), want); err == nil {
			_ = unix.Close(fd)
			t.Fatal("followed a symlinked run home")
		}
		if _, err := os.Lstat(filepath.Join(parent, ".docker")); err == nil {
			t.Fatal("created .docker through a symlinked run home")
		}
	})
	t.Run("missing parent", func(t *testing.T) {
		_, want := testDockerParent(t)
		if fd, err := setupDockerConfig(filepath.Join(t.TempDir(), "absent"), realDockerConfigSys, os.Getuid(), want); err == nil {
			_ = unix.Close(fd)
			t.Fatal("accepted a missing run home")
		}
	})
}

// TestDegradeReasonNamesNothingFromTheFilesystem: the reasons are built from
// fixed strings, numbers and errno text only.
func TestDegradeReasonNamesNothingFromTheFilesystem(t *testing.T) {
	parent, want := testDockerParent(t)
	secret := "attacker-chosen-name"
	target := filepath.Join(t.TempDir(), secret)
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(parent, ".docker")); err != nil {
		t.Fatal(err)
	}
	_, err := setupDockerConfig(parent, realDockerConfigSys, os.Getuid(), want)
	if err == nil {
		t.Fatal("expected refusal")
	}
	for _, leak := range []string{secret, parent, target} {
		if strings.Contains(err.Error(), leak) {
			t.Fatalf("reason %q leaks %q", err, leak)
		}
	}
}

func TestWithoutDockerConfig(t *testing.T) {
	in := []string{"A=1", "DOCKER_CONFIG=/a/.docker", "B=2", "DOCKER_CONFIG=/b/.docker", "DOCKER_CONFIG=", "XDOCKER_CONFIG=keep", "DOCKER_CONFIG_X=keep"}
	got := withoutDockerConfig(in)
	want := []string{"A=1", "B=2", "XDOCKER_CONFIG=keep", "DOCKER_CONFIG_X=keep"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if len(in) != 7 || in[1] != "DOCKER_CONFIG=/a/.docker" {
		t.Fatal("input was mutated")
	}
}

func TestAddRulesDockerGrant(t *testing.T) {
	const ruleset, handled = 7, uint64(0xabc)
	collect := func(fds grantFds) []int {
		var got []int
		err := addRules(ruleset, "/checkout", fds, handled, ruleAdders{
			path: func(int, string, uint64) error { return nil },
			fd: func(_ int, fd int, access uint64) error {
				if access != handled {
					t.Errorf("fd %d granted %#x, want %#x", fd, access, handled)
				}
				got = append(got, fd)
				return nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := collect(grantFds{tmp: 3, cache: -1}); !reflect.DeepEqual(got, []int{3}) {
		t.Fatalf("zero-value docker granted %v (must never zero-value into fd 0)", got)
	}
	if got := collect(grantFds{tmp: 3, cache: -1, docker: 0}); !reflect.DeepEqual(got, []int{3}) {
		t.Fatalf("docker=0 without hasDocker granted %v", got)
	}
	if got := collect(grantFds{tmp: 3, cache: -1, docker: 5, hasDocker: true}); !reflect.DeepEqual(got, []int{3, 5}) {
		t.Fatalf("docker only: %v", got)
	}
	if got := collect(grantFds{tmp: 3, cache: 4, docker: 5, hasDocker: true}); !reflect.DeepEqual(got, []int{3, 4, 5}) {
		t.Fatalf("cache and docker: %v", got)
	}
	if got := collect(grantFds{tmp: 3, cache: -1, docker: 5, hasDocker: true, readOnly: true}); !reflect.DeepEqual(got, []int{3}) {
		t.Fatalf("read-only policy granted docker: %v", got)
	}
	boom := errors.New("boom")
	err := addRules(ruleset, "/checkout", grantFds{tmp: 3, cache: -1, docker: 5, hasDocker: true}, handled, ruleAdders{
		path: func(int, string, uint64) error { return nil },
		fd: func(_ int, fd int, _ uint64) error {
			if fd == 5 {
				return boom
			}
			return nil
		},
	})
	if !errors.Is(err, boom) {
		t.Fatalf("docker rule failure = %v", err)
	}
}

// TestAddRulesDockerGrantsThroughTheSetupFd: the rule lands on the inode the
// setup verified, not on a path swapped in afterwards.
func TestAddRulesDockerGrantsThroughTheSetupFd(t *testing.T) {
	parent, want := testDockerParent(t)
	fd, err := setupDockerConfig(parent, realDockerConfigSys, os.Getuid(), want)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(fd) }()
	var adopted unix.Stat_t
	if err := unix.Fstat(fd, &adopted); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(parent, ".docker")
	if err := os.Rename(dir, dir+".moved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	var paths []string
	var inos []uint64
	err = addRules(7, t.TempDir(), grantFds{tmp: 3, cache: -1, docker: fd, hasDocker: true}, 0xabc, ruleAdders{
		path: func(_ int, p string, _ uint64) error { paths = append(paths, p); return nil },
		fd: func(_ int, f int, _ uint64) error {
			if f == fd {
				var st unix.Stat_t
				if err := unix.Fstat(f, &st); err != nil {
					t.Error(err)
				}
				inos = append(inos, st.Ino)
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(inos) != 1 || inos[0] != adopted.Ino {
		t.Fatalf("docker rule inodes = %v, want [%d]", inos, adopted.Ino)
	}
	for _, p := range paths {
		if strings.HasPrefix(p, parent) {
			t.Fatalf("a path rule named the run home or its docker dir: %q", p)
		}
	}
}

// TestCrossCheckNeverGrantsDocker: the checker policy is built with no docker
// grant and ignores one even if set.
func TestCrossCheckNeverGrantsDocker(t *testing.T) {
	handled := baseRights | unix.LANDLOCK_ACCESS_FS_REFER | unix.LANDLOCK_ACCESS_FS_TRUNCATE
	var got []int
	err := addRules(1, "/checkout", grantFds{tmp: 9, cache: -1, docker: 11, hasDocker: true, readOnly: true}, handled, ruleAdders{
		path: func(int, string, uint64) error { return nil },
		fd:   func(_ int, fd int, _ uint64) error { got = append(got, fd); return nil },
	})
	if err != nil || !reflect.DeepEqual(got, []int{9}) {
		t.Fatalf("cross-check fd grants = %v err=%v", got, err)
	}
}

const dockerHelperEnv = "CMDSANDBOX_TEST_DOCKER_ARGS"

// TestRealMainDockerDegrade runs realMain in a re-exec'd copy of this binary
// (mode off, so no Landlock is needed) with a --docker-config whose parent is
// not the production worker home. The command must still run (exit 0), every
// DOCKER_CONFIG must be gone from its variables, and stderr must carry the one
// degrade line.
func TestRealMainDockerDegrade(t *testing.T) {
	if raw := os.Getenv(dockerHelperEnv); raw != "" {
		var args []string
		if err := json.Unmarshal([]byte(raw), &args); err != nil {
			os.Exit(90)
		}
		os.Exit(runOnLockedThread(args, runtime.LockOSThread, realMain))
	}
	if os.Getuid() == int(productionDockerParent.uid) {
		t.Skip("running as the production worker uid; the parent could be valid")
	}
	root := t.TempDir()
	parent := filepath.Join(root, "runhome")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	tmp := privateTmp(t)
	out := filepath.Join(root, "seen")
	script := `printf '%s' "${DOCKER_CONFIG-unset}" > "$OUT"`
	args, err := json.Marshal([]string{"--root", root, "--tmp", tmp, "--cwd", root, "--docker-config", filepath.Join(parent, ".docker"), "--mode", "off", "--", "/bin/sh", "-c", script})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestRealMainDockerDegrade$")
	cmd.Env = append(os.Environ(), dockerHelperEnv+"="+string(args), "DOCKER_CONFIG=/planted/.docker", "OUT="+out)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("helper failed: %v\n%s", err, stderr.String())
	}
	if got, err := os.ReadFile(out); err != nil || string(got) != "unset" {
		t.Fatalf("child saw DOCKER_CONFIG=%q err=%v, want it removed", got, err)
	}
	const prefix = "uzi-codex-command-sandbox: Docker config directory unavailable ("
	const suffix = "); this command runs without a persistent Docker login\n"
	line := ""
	for _, l := range strings.SplitAfter(stderr.String(), "\n") {
		if strings.HasPrefix(l, prefix) {
			if line != "" {
				t.Fatalf("more than one degrade line:\n%s", stderr.String())
			}
			line = l
		}
	}
	if line == "" || !strings.HasSuffix(line, suffix) {
		t.Fatalf("degrade line missing or malformed:\n%s", stderr.String())
	}
	if strings.Contains(line, root) {
		t.Fatalf("degrade line names a path: %q", line)
	}
	if _, err := os.Lstat(filepath.Join(parent, ".docker")); err == nil {
		t.Fatal("created .docker under a refused parent")
	}
}
