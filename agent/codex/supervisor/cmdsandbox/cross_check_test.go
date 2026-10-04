package main

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func crossCheckFixture(t *testing.T) (string, string, []string) {
	t.Helper()
	scratch, err := filepath.Abs("../../../../.uzi/scratch")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		t.Fatal(err)
	}
	base, err := os.MkdirTemp(scratch, "checker-unit-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	root, state := filepath.Join(base, "checkout"), filepath.Join(base, "private")
	for _, p := range []string{root, state} {
		if err := os.Mkdir(p, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return root, state, []string{"--root", root, "--state", state, "--cwd", root, "--", "/bin/true"}
}

func TestCrossCheckRequiredUnavailableRefuses(t *testing.T) {
	_, _, args := crossCheckFixture(t)
	for _, errno := range []syscall.Errno{unix.ENOSYS, unix.EOPNOTSUPP, unix.EPERM} {
		t.Run(errno.Error(), func(t *testing.T) {
			called := false
			rc := crossCheckRun(args, func() (uintptr, syscall.Errno) { return 0, errno },
				func(string, grantFds, int) error { called = true; return nil })
			if rc == 0 || called {
				t.Fatalf("refusal rc=%d confine=%v", rc, called)
			}
		})
	}
}

func TestCrossCheckApplyFailureRefuses(t *testing.T) {
	_, _, args := crossCheckFixture(t)
	rc := crossCheckRun(args, func() (uintptr, syscall.Errno) { return 6, 0 },
		func(_ string, fds grantFds, abi int) error {
			if !fds.readOnly || fds.cache != -1 || fds.tmp < 0 || abi != 6 {
				t.Fatal("wrong checker posture")
			}
			return errors.New("restrict failed")
		})
	if rc == 0 {
		t.Fatal("application error launched child")
	}
}

func TestCrossCheckStateCustody(t *testing.T) {
	for _, tc := range []struct {
		name string
		uid  uint32
		mode uint32
		ok   bool
	}{
		{"private", 123, unix.S_IFDIR | 0o700, true},
		{"session traverse", 123, unix.S_IFDIR | 0o710, true},
		{"foreign", 124, unix.S_IFDIR | 0o700, false},
		{"world read", 123, unix.S_IFDIR | 0o755, false},
		{"group write", 123, unix.S_IFDIR | 0o770, false},
		{"setgid", 123, unix.S_IFDIR | 0o2700, false},
		{"file", 123, unix.S_IFREG | 0o700, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := verifyCrossCheckState(9, 123, func(_ int, st *unix.Stat_t) error { st.Uid = tc.uid; st.Mode = tc.mode; return nil })
			if (err == nil) != tc.ok {
				t.Fatalf("custody: %v", err)
			}
		})
	}
}

func TestCrossCheckArgsRejectWidening(t *testing.T) {
	root, state, args := crossCheckFixture(t)
	link := filepath.Join(filepath.Dir(root), "link")
	if err := os.Symlink(state, link); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		index int
		value string
	}{
		{1, "/"}, {3, root}, {3, link}, {5, state}, {1, "/usr"}, {3, "/etc"}, {7, "relative"},
	} {
		altered := append([]string(nil), args...)
		altered[tc.index] = tc.value
		if _, _, _, _, err := parseCrossCheckArgs(altered); err == nil {
			t.Fatalf("accepted %v", altered)
		}
	}
	altered := append([]string{"--mode", "off"}, args...)
	if _, _, _, _, err := parseCrossCheckArgs(altered); err == nil {
		t.Fatal("accepted optional enforcement")
	}
}

func TestCrossCheckTruncateRequiresABI3(t *testing.T) {
	for _, abi := range []int{1, 2} {
		if err := confine("/checkout", grantFds{readOnly: true}, abi); err == nil || err.Error() != "checker confinement requires Landlock ABI 3 for truncate denial" {
			t.Fatalf("ABI %d: expected truncate guard refusal, got %v", abi, err)
		}
	}
}

func TestCrossCheckRulesNarrow(t *testing.T) {
	paths := map[string]uint64{}
	fds := map[int]uint64{}
	handled := baseRights | unix.LANDLOCK_ACCESS_FS_REFER | unix.LANDLOCK_ACCESS_FS_TRUNCATE
	err := addRules(1, "/checkout", grantFds{tmp: 9, cache: -1, readOnly: true}, handled, ruleAdders{
		path: func(_ int, p string, rights uint64) error { paths[p] = rights; return nil },
		fd:   func(_ int, fd int, rights uint64) error { fds[fd] = rights; return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	read := uint64(unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_READ_DIR)
	if paths["/checkout"] != read || fds[9] != handled || len(fds) != 1 {
		t.Fatal("incorrect checkout/private grants")
	}
	for _, p := range []string{"/", "/etc", "/dev", "/dev/shm", "/tmp", "/data", "/checkout-sibling"} {
		if _, ok := paths[p]; ok {
			t.Fatalf("broad grant: %s", p)
		}
	}
	for _, p := range []string{"/usr", "/nix", "/opt/uzi-toolchain", "/opt/uzi-codex", "/etc/ssl", "/etc/codex/requirements.toml"} {
		rights, ok := paths[p]
		if !ok || rights&unix.LANDLOCK_ACCESS_FS_WRITE_FILE != 0 {
			t.Fatalf("system rights %s: %x", p, rights)
		}
	}
}
