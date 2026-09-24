package main

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

func TestParseArgs(t *testing.T) {
	root, tmp, cwd, cache, mode, child, err := parseArgs([]string{
		"--root", "/data/run", "--tmp", "/tmp/run", "--cwd", "/data/run/sub", "--", "/bin/sh", "-c", "true",
	})
	if err != nil || root != "/data/run" || tmp != "/tmp/run" || cwd != "/data/run/sub" || cache != "" || mode != modeRequired || !reflect.DeepEqual(child, []string{"/bin/sh", "-c", "true"}) {
		t.Fatalf("unexpected parse: root=%q tmp=%q cwd=%q mode=%q child=%v err=%v", root, tmp, cwd, mode, child, err)
	}
}

func TestParseArgsMode(t *testing.T) {
	// (present) an explicit --mode before -- is honored.
	for _, want := range []sandboxMode{modeRequired, modeBestEffort} {
		root, tmp, cwd, _, mode, child, err := parseArgs([]string{
			"--root", "/data/run", "--tmp", "/tmp/run", "--cwd", "/data/run", "--mode", string(want), "--", "/bin/true",
		})
		if err != nil || mode != want || root != "/data/run" || tmp != "/tmp/run" || cwd != "/data/run" || !reflect.DeepEqual(child, []string{"/bin/true"}) {
			t.Fatalf("--mode %q: mode=%q child=%v err=%v", want, mode, child, err)
		}
	}

	// (absent) defaults to required.
	if _, _, _, _, mode, _, err := parseArgs([]string{
		"--root", "/data/run", "--tmp", "/tmp/run", "--cwd", "/data/run", "--", "/bin/true",
	}); err != nil || mode != modeRequired {
		t.Fatalf("absent --mode should default to required: mode=%q err=%v", mode, err)
	}

	// (after --) a --mode token in the CHILD command is NOT parsed as the sandbox
	// mode — the trust property: the mode comes only from the trusted worker argv.
	root, _, _, _, mode, child, err := parseArgs([]string{
		"--root", "/data/run", "--tmp", "/tmp/run", "--cwd", "/data/run", "--", "/bin/sh", "--mode", "best-effort",
	})
	if err != nil || root != "/data/run" || mode != modeRequired {
		t.Fatalf("a malicious --mode after -- must not change the mode: mode=%q err=%v", mode, err)
	}
	if !reflect.DeepEqual(child, []string{"/bin/sh", "--mode", "best-effort"}) {
		t.Fatalf("a --mode after -- must stay part of the child command: child=%v", child)
	}

	// (invalid) an unknown mode value is rejected (fail closed).
	if _, _, _, _, _, _, err := parseArgs([]string{
		"--root", "/data/run", "--tmp", "/tmp/run", "--cwd", "/data/run", "--mode", "loose", "--", "/bin/true",
	}); err == nil {
		t.Fatal("an unknown --mode value must be rejected")
	}
}

func TestRunOnLockedThreadPinsBeforePolicyWork(t *testing.T) {
	order := make([]string, 0, 2)
	got := runOnLockedThread(
		[]string{"arg"},
		func() { order = append(order, "lock") },
		func(args []string) int {
			order = append(order, "run")
			if !reflect.DeepEqual(args, []string{"arg"}) {
				t.Fatalf("unexpected args: %v", args)
			}
			return 7
		},
	)
	if got != 7 || !reflect.DeepEqual(order, []string{"lock", "run"}) {
		t.Fatalf("runOnLockedThread: code=%d order=%v", got, order)
	}
}

func TestParseArgsRejectsEscape(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "sibling", args: []string{"--root", "/data/run", "--tmp", "/tmp/run", "--cwd", "/data/other", "--", "/bin/true"}},
		{name: "prefix collision", args: []string{"--root", "/data/run", "--tmp", "/tmp/run", "--cwd", "/data/runner", "--", "/bin/true"}},
		{name: "filesystem root", args: []string{"--root", "/", "--tmp", "/tmp/run", "--cwd", "/", "--", "/bin/true"}},
		{name: "relative root", args: []string{"--root", "data/run", "--tmp", "/tmp/run", "--cwd", "/data/run", "--", "/bin/true"}},
		{name: "relative tmp", args: []string{"--root", "/data/run", "--tmp", "tmp/run", "--cwd", "/data/run", "--", "/bin/true"}},
		{name: "relative cwd", args: []string{"--root", "/data/run", "--tmp", "/tmp/run", "--cwd", "data/run", "--", "/bin/true"}},
		{name: "relative child", args: []string{"--root", "/data/run", "--tmp", "/tmp/run", "--cwd", "/data/run", "--", "bin/true"}},
		{name: "malformed flag order", args: []string{"--tmp", "/tmp/run", "--root", "/data/run", "--cwd", "/data/run", "--", "/bin/true"}},
		{name: "missing separator", args: []string{"--root", "/data/run", "--tmp", "/tmp/run", "--cwd", "/data/run", "/bin/true"}},
		{name: "invalid mode", args: []string{"--root", "/data/run", "--tmp", "/tmp/run", "--cwd", "/data/run", "--mode", "off", "--", "/bin/true"}},
		{name: "too short", args: []string{"--root", "/data/run", "--tmp", "/tmp/run"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, _, _, _, _, _, err := parseArgs(test.args); err == nil {
				t.Fatal("expected argument rejection")
			}
		})
	}
}

// fakeProbe builds a versionProbe seam returning a fixed ABI/errno. When errno is
// non-zero the abi is ignored (matching the real syscall).
func fakeProbe(abi uintptr, errno syscall.Errno) versionProbe {
	return func() (uintptr, syscall.Errno) { return abi, errno }
}

func TestClassifyLandlockErrnoSeam(t *testing.T) {
	tests := []struct {
		name  string
		probe versionProbe
		want  landlockAvailability
	}{
		{name: "ENOSYS is unavailable", probe: fakeProbe(0, syscall.ENOSYS), want: landlockUnavailable},
		{name: "EOPNOTSUPP is unavailable", probe: fakeProbe(0, syscall.EOPNOTSUPP), want: landlockUnavailable},
		{name: "EPERM is error", probe: fakeProbe(0, syscall.EPERM), want: landlockError},
		{name: "EACCES is error", probe: fakeProbe(0, syscall.EACCES), want: landlockError},
		{name: "EINVAL is error", probe: fakeProbe(0, syscall.EINVAL), want: landlockError},
		{name: "ABI 0 is error", probe: fakeProbe(0, 0), want: landlockError},
		{name: "ABI 1 is available", probe: fakeProbe(1, 0), want: landlockAvailable},
		{name: "ABI 6 is available", probe: fakeProbe(6, 0), want: landlockAvailable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, abi, err := classifyLandlock(test.probe)
			if got != test.want {
				t.Fatalf("classify = %v, want %v (abi=%d err=%v)", got, test.want, abi, err)
			}
			if test.want == landlockError && err == nil {
				t.Fatal("an error classification must carry a diagnostic error")
			}
			if test.want == landlockAvailable && abi < 1 {
				t.Fatalf("an available classification must report the ABI, got %d", abi)
			}
		})
	}
}

func TestDecidePolicyByModeAndErrno(t *testing.T) {
	tests := []struct {
		name  string
		probe versionProbe
		mode  sandboxMode
		want  policyAction
	}{
		// ENOSYS/EOPNOTSUPP: fatal in required, degrade in best-effort.
		{name: "ENOSYS required is fatal", probe: fakeProbe(0, syscall.ENOSYS), mode: modeRequired, want: actionFatal},
		{name: "ENOSYS best-effort degrades", probe: fakeProbe(0, syscall.ENOSYS), mode: modeBestEffort, want: actionUnconfined},
		{name: "EOPNOTSUPP required is fatal", probe: fakeProbe(0, syscall.EOPNOTSUPP), mode: modeRequired, want: actionFatal},
		{name: "EOPNOTSUPP best-effort degrades", probe: fakeProbe(0, syscall.EOPNOTSUPP), mode: modeBestEffort, want: actionUnconfined},
		// Any other errno / ABI 0: fatal in BOTH modes.
		{name: "EPERM required is fatal", probe: fakeProbe(0, syscall.EPERM), mode: modeRequired, want: actionFatal},
		{name: "EPERM best-effort is fatal", probe: fakeProbe(0, syscall.EPERM), mode: modeBestEffort, want: actionFatal},
		{name: "EINVAL best-effort is fatal", probe: fakeProbe(0, syscall.EINVAL), mode: modeBestEffort, want: actionFatal},
		{name: "ABI 0 required is fatal", probe: fakeProbe(0, 0), mode: modeRequired, want: actionFatal},
		{name: "ABI 0 best-effort is fatal", probe: fakeProbe(0, 0), mode: modeBestEffort, want: actionFatal},
		// Available: apply in both modes.
		{name: "available required applies", probe: fakeProbe(1, 0), mode: modeRequired, want: actionApply},
		{name: "available best-effort applies", probe: fakeProbe(3, 0), mode: modeBestEffort, want: actionApply},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, abi, err := decidePolicy(test.probe, test.mode)
			if got != test.want {
				t.Fatalf("decidePolicy = %v, want %v (abi=%d err=%v)", got, test.want, abi, err)
			}
			if test.want == actionFatal && err == nil {
				t.Fatal("a fatal decision must carry an error")
			}
			if test.want == actionApply && abi < 1 {
				t.Fatalf("an apply decision must carry the ABI, got %d", abi)
			}
		})
	}
}

func TestProbeExitCodes(t *testing.T) {
	tests := []struct {
		name  string
		probe versionProbe
		want  int
	}{
		{name: "available", probe: fakeProbe(1, 0), want: probeExitAvailable},
		{name: "ENOSYS unavailable", probe: fakeProbe(0, syscall.ENOSYS), want: probeExitUnavailable},
		{name: "EOPNOTSUPP unavailable", probe: fakeProbe(0, syscall.EOPNOTSUPP), want: probeExitUnavailable},
		{name: "EPERM error", probe: fakeProbe(0, syscall.EPERM), want: probeExitError},
		{name: "ABI 0 error", probe: fakeProbe(0, 0), want: probeExitError},
	}
	// The three codes must be distinct so the caller can discriminate.
	if probeExitAvailable == probeExitUnavailable || probeExitAvailable == probeExitError || probeExitUnavailable == probeExitError {
		t.Fatalf("probe exit codes must be distinct: %d/%d/%d", probeExitAvailable, probeExitUnavailable, probeExitError)
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := probeExitCode(test.probe); got != test.want {
				t.Fatalf("probeExitCode = %d, want %d", got, test.want)
			}
		})
	}
}

// TestApplyPolicyDispatch pins applyPolicy's mapping of a decidePolicy action onto
// the actual enforcement primitives — the most safety-critical wiring. confine()
// needs a real kernel, so the two primitives are injected as recording fakes; the
// assertions are which primitive ran, never that it succeeded. This reddens on a
// branch swap (actionApply calling the unconfined path, or vice versa) and on
// dropping applyNoNewPrivs() from the unconfined branch.
func TestApplyPolicyDispatch(t *testing.T) {
	const wantABI = 3
	tests := []struct {
		name           string
		probe          versionProbe
		mode           sandboxMode
		wantConfine    bool
		wantNoNewPrivs bool
		wantErr        bool
	}{
		{
			// actionApply → confine() runs (required path is NEVER unconfined).
			name:        "actionApply confines and never runs the unconfined-only path",
			probe:       fakeProbe(wantABI, 0),
			mode:        modeRequired,
			wantConfine: true,
		},
		{
			// actionUnconfined → confine() does NOT run, but no_new_privs is STILL set.
			name:           "actionUnconfined skips confine but still sets no_new_privs",
			probe:          fakeProbe(0, syscall.ENOSYS),
			mode:           modeBestEffort,
			wantNoNewPrivs: true,
		},
		{
			// actionFatal → neither enforcement runs and the fatal error propagates.
			name:    "actionFatal runs neither enforcement and propagates the error",
			probe:   fakeProbe(0, syscall.ENOSYS),
			mode:    modeRequired,
			wantErr: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var confineCalled, noNewPrivsCalled bool
			var gotRoot string
			var gotFds grantFds
			var gotABI int
			confineFake := func(root string, fds grantFds, abi int) error {
				confineCalled = true
				gotRoot, gotFds, gotABI = root, fds, abi
				return nil
			}
			noNewPrivsFake := func() error {
				noNewPrivsCalled = true
				return nil
			}
			err := applyPolicy("/data/run", grantFds{tmp: 42, cache: 43}, test.mode, test.probe, confineFake, noNewPrivsFake)
			switch {
			case test.wantErr && err == nil:
				t.Fatal("actionFatal must propagate the fatal error")
			case !test.wantErr && err != nil:
				t.Fatalf("unexpected error: %v", err)
			}
			if confineCalled != test.wantConfine {
				t.Fatalf("confine() invoked = %v, want %v", confineCalled, test.wantConfine)
			}
			if noNewPrivsCalled != test.wantNoNewPrivs {
				t.Fatalf("applyNoNewPrivs() invoked = %v, want %v", noNewPrivsCalled, test.wantNoNewPrivs)
			}
			if test.wantConfine {
				// confine() must receive the worktree/tmp roots and the ABI
				// decidePolicy resolved — proof this is the real confine path, not
				// the unconfined one.
				if gotRoot != "/data/run" || gotFds != (grantFds{tmp: 42, cache: 43}) || gotABI != wantABI {
					t.Fatalf("confine() args: root=%q fds=%+v abi=%d, want /data/run {42 43} %d", gotRoot, gotFds, gotABI, wantABI)
				}
			}
		})
	}
}

func TestConfinementDenyProbe(t *testing.T) {
	readable := func(string) (*os.File, error) { return os.Open(os.DevNull) }
	denied := func(string) (*os.File, error) { return nil, os.ErrPermission }
	failed := func(string) (*os.File, error) { return nil, errors.New("probe failed") }

	if err := requireProbeReadable(landlockDenyProbe, readable); err != nil {
		t.Fatalf("readable pre-probe rejected: %v", err)
	}
	if err := requireProbeReadable(landlockDenyProbe, denied); err == nil {
		t.Fatal("pre-probe denial must fail closed")
	}
	if err := requireProbeDenied(landlockDenyProbe, denied); err != nil {
		t.Fatalf("post-probe permission denial rejected: %v", err)
	}
	if err := requireProbeDenied(landlockDenyProbe, readable); err == nil {
		t.Fatal("readable post-probe must fail closed")
	}
	if err := requireProbeDenied(landlockDenyProbe, failed); err == nil {
		t.Fatal("unexpected post-probe error must fail closed")
	}
}

// privateTmp makes a real 0700 directory in a t.TempDir parent.
func privateTmp(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "uzi-codex-command-x")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestAdoptPrivateTmpAcceptsOwned0700Dir(t *testing.T) {
	dir := privateTmp(t)
	fd, err := adoptPrivateTmp(dir, os.Getuid(), unix.Open, unix.Fstat)
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	defer func() { _ = unix.Close(fd) }()
	// The adopted fd stays open, is close-on-exec, and is the directory itself.
	flags, ferr := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	if ferr != nil || flags&unix.FD_CLOEXEC == 0 {
		t.Fatalf("adopted fd is not close-on-exec (flags=%d err=%v)", flags, ferr)
	}
	var byFd, byPath unix.Stat_t
	if err := unix.Fstat(fd, &byFd); err != nil {
		t.Fatal(err)
	}
	if err := unix.Lstat(dir, &byPath); err != nil {
		t.Fatal(err)
	}
	if byFd.Dev != byPath.Dev || byFd.Ino != byPath.Ino {
		t.Fatal("adopted fd does not refer to the tmp directory")
	}
	// Adoption never removes or chmods: the dir is still there, still 0700.
	fi, err := os.Lstat(dir)
	if err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Fatalf("after adopt: %v %v", fi, err)
	}
}

func TestAdoptPrivateTmpRefusesSymlink(t *testing.T) {
	target := privateTmp(t)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := adoptPrivateTmp(link, os.Getuid(), unix.Open, unix.Fstat); err == nil {
		t.Fatal("adopted a symlink to an owned 0700 directory")
	}
}

func TestAdoptPrivateTmpRefusesForeignOwner(t *testing.T) {
	dir := privateTmp(t)
	foreign := func(fd int, st *unix.Stat_t) error {
		if err := unix.Fstat(fd, st); err != nil {
			return err
		}
		st.Uid++
		return nil
	}
	_, err := adoptPrivateTmp(dir, os.Getuid(), unix.Open, foreign)
	if err == nil || !strings.Contains(err.Error(), "owned by uid") {
		t.Fatalf("adopt with a foreign owner = %v, want an owner refusal", err)
	}
	// The uid seam alone must also refuse: the dir is not owned by another uid.
	if _, err := adoptPrivateTmp(dir, os.Getuid()+1, unix.Open, unix.Fstat); err == nil {
		t.Fatal("adopted a dir owned by another uid")
	}
}

func TestAdoptPrivateTmpRefusesWrongMode(t *testing.T) {
	dir := privateTmp(t)
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := adoptPrivateTmp(dir, os.Getuid(), unix.Open, unix.Fstat)
	if err == nil || !strings.Contains(err.Error(), "mode 0755") {
		t.Fatalf("adopt of a 0755 dir = %v, want a mode refusal", err)
	}
}

func TestAdoptPrivateTmpRefusesNonDirectory(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := adoptPrivateTmp(file, os.Getuid(), unix.Open, unix.Fstat); err == nil {
		t.Fatal("adopted a regular file")
	}
	// A stat that reports a non-directory is refused even if the open passed.
	notDir := func(fd int, st *unix.Stat_t) error {
		if err := unix.Fstat(fd, st); err != nil {
			return err
		}
		st.Mode = unix.S_IFREG | 0o700
		return nil
	}
	_, err := adoptPrivateTmp(privateTmp(t), os.Getuid(), unix.Open, notDir)
	if err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("adopt of a non-dir stat = %v", err)
	}
}

func TestAdoptPrivateTmpRefusesMissing(t *testing.T) {
	if _, err := adoptPrivateTmp(filepath.Join(t.TempDir(), "absent"), os.Getuid(), unix.Open, unix.Fstat); err == nil {
		t.Fatal("adopted a missing path: the sandbox must never create it")
	}
}

// TestAdoptPrivateTmpRefusesNonEmpty: a tmp planted with content (here a
// .gitconfig) is not fresh and is refused, and its fd is not leaked.
func TestAdoptPrivateTmpRefusesNonEmpty(t *testing.T) {
	for _, plant := range []func(dir string) error{
		func(dir string) error {
			return os.WriteFile(filepath.Join(dir, ".gitconfig"), []byte("[core]\n"), 0o600)
		},
		func(dir string) error { return os.Mkdir(filepath.Join(dir, "sub"), 0o700) },
		func(dir string) error { return os.Symlink("/etc", filepath.Join(dir, "link")) },
	} {
		dir := privateTmp(t)
		if err := plant(dir); err != nil {
			t.Fatal(err)
		}
		var opened int
		open := func(p string, flags int, mode uint32) (int, error) {
			fd, err := unix.Open(p, flags, mode)
			opened = fd
			return fd, err
		}
		fd, err := adoptPrivateTmp(dir, os.Getuid(), open, unix.Fstat)
		if err == nil || !strings.Contains(err.Error(), "not empty") {
			t.Fatalf("adopt of a planted tmp = %d/%v, want a not-empty refusal", fd, err)
		}
		if _, ferr := unix.FcntlInt(uintptr(opened), unix.F_GETFD, 0); !errors.Is(ferr, unix.EBADF) {
			t.Fatalf("refused tmp fd %d still open: %v", opened, ferr)
		}
	}
}

// TestAddFdRuleUsesTheFd pins that addFdRule works on the fd itself: with a
// real Landlock ruleset it accepts the adopted directory fd, and an fd that can
// never be open (-1) is refused with EBADF rather than resolved by any path.
func TestAddFdRuleUsesTheFd(t *testing.T) {
	avail, _, _ := classifyLandlock(realVersionProbe)
	if avail != landlockAvailable {
		t.Skip("Landlock is not available on this kernel")
	}
	attr := unix.LandlockRulesetAttr{Access_fs: baseRights}
	rs, _, errno := syscall.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, uintptr(unsafe.Pointer(&attr)), unsafe.Sizeof(attr), 0)
	if errno != 0 {
		t.Fatalf("create ruleset: %v", errno)
	}
	defer func() { _ = unix.Close(int(rs)) }()
	fd, err := adoptPrivateTmp(privateTmp(t), os.Getuid(), unix.Open, unix.Fstat)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(fd) }()
	if err := addFdRule(int(rs), fd, baseRights); err != nil {
		t.Fatalf("addFdRule on the adopted fd: %v", err)
	}
	if err := addFdRule(int(rs), -1, baseRights); !errors.Is(err, unix.EBADF) {
		t.Fatalf("addFdRule on fd -1 = %v, want EBADF", err)
	}
}

// TestAddRulesGrantsTmpThroughTheAdoptedFd: confine's rule set grants the tmp
// rule through the ADOPTED fd, never by reopening a path, so swapping the
// --tmp path for another directory after adoption does not move the rule.
func TestAddRulesGrantsTmpThroughTheAdoptedFd(t *testing.T) {
	tmp := privateTmp(t)
	fd, err := adoptPrivateTmp(tmp, os.Getuid(), unix.Open, unix.Fstat)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(fd) }()
	var adopted unix.Stat_t
	if err := unix.Fstat(fd, &adopted); err != nil {
		t.Fatal(err)
	}
	// Swap the path after adoption: the adopted dir moves away and a fresh
	// directory takes its name.
	if err := os.Rename(tmp, tmp+".moved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(tmp, 0o700); err != nil {
		t.Fatal(err)
	}

	const ruleset, handled = 99, uint64(0x1234)
	root := t.TempDir()
	var paths []string
	type fdRule struct {
		fd     int
		access uint64
		ino    uint64
	}
	var fdRules []fdRule
	err = addRules(ruleset, root, grantFds{tmp: fd, cache: -1}, handled, ruleAdders{
		path: func(rs int, path string, _ uint64) error {
			if rs != ruleset {
				t.Errorf("path rule on ruleset %d", rs)
			}
			paths = append(paths, path)
			return nil
		},
		fd: func(rs int, got int, access uint64) error {
			if rs != ruleset {
				t.Errorf("fd rule on ruleset %d", rs)
			}
			var st unix.Stat_t
			if err := unix.Fstat(got, &st); err != nil {
				t.Errorf("fstat of the rule fd %d: %v", got, err)
			}
			fdRules = append(fdRules, fdRule{fd: got, access: access, ino: st.Ino})
			return nil
		},
	})
	if err != nil {
		t.Fatalf("addRules: %v", err)
	}
	if len(fdRules) != 1 || fdRules[0].fd != fd || fdRules[0].access != handled || fdRules[0].ino != adopted.Ino {
		t.Fatalf("fd rules = %+v, want one on the adopted fd %d (ino %d) with %#x", fdRules, fd, adopted.Ino, handled)
	}
	for _, p := range paths {
		if strings.HasPrefix(p, filepath.Dir(tmp)) {
			t.Fatalf("a path rule named the tmp (%q); the tmp must go through its fd", p)
		}
	}
	if len(paths) == 0 || paths[len(paths)-1] != root {
		t.Fatalf("path rules %v do not end with the root %q", paths, root)
	}
}
