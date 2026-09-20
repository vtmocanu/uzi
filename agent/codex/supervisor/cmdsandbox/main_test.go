package main

import (
	"errors"
	"os"
	"reflect"
	"syscall"
	"testing"
)

func TestParseArgs(t *testing.T) {
	root, tmp, cwd, mode, child, err := parseArgs([]string{
		"--root", "/data/run", "--tmp", "/tmp/run", "--cwd", "/data/run/sub", "--", "/bin/sh", "-c", "true",
	})
	if err != nil || root != "/data/run" || tmp != "/tmp/run" || cwd != "/data/run/sub" || mode != modeRequired || !reflect.DeepEqual(child, []string{"/bin/sh", "-c", "true"}) {
		t.Fatalf("unexpected parse: root=%q tmp=%q cwd=%q mode=%q child=%v err=%v", root, tmp, cwd, mode, child, err)
	}
}

func TestParseArgsMode(t *testing.T) {
	// (present) an explicit --mode before -- is honored.
	for _, want := range []sandboxMode{modeRequired, modeBestEffort} {
		root, tmp, cwd, mode, child, err := parseArgs([]string{
			"--root", "/data/run", "--tmp", "/tmp/run", "--cwd", "/data/run", "--mode", string(want), "--", "/bin/true",
		})
		if err != nil || mode != want || root != "/data/run" || tmp != "/tmp/run" || cwd != "/data/run" || !reflect.DeepEqual(child, []string{"/bin/true"}) {
			t.Fatalf("--mode %q: mode=%q child=%v err=%v", want, mode, child, err)
		}
	}

	// (absent) defaults to required.
	if _, _, _, mode, _, err := parseArgs([]string{
		"--root", "/data/run", "--tmp", "/tmp/run", "--cwd", "/data/run", "--", "/bin/true",
	}); err != nil || mode != modeRequired {
		t.Fatalf("absent --mode should default to required: mode=%q err=%v", mode, err)
	}

	// (after --) a --mode token in the CHILD command is NOT parsed as the sandbox
	// mode — the trust property: the mode comes only from the trusted worker argv.
	root, _, _, mode, child, err := parseArgs([]string{
		"--root", "/data/run", "--tmp", "/tmp/run", "--cwd", "/data/run", "--", "/bin/sh", "--mode", "best-effort",
	})
	if err != nil || root != "/data/run" || mode != modeRequired {
		t.Fatalf("a malicious --mode after -- must not change the mode: mode=%q err=%v", mode, err)
	}
	if !reflect.DeepEqual(child, []string{"/bin/sh", "--mode", "best-effort"}) {
		t.Fatalf("a --mode after -- must stay part of the child command: child=%v", child)
	}

	// (invalid) an unknown mode value is rejected (fail closed).
	if _, _, _, _, _, err := parseArgs([]string{
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
			if _, _, _, _, _, err := parseArgs(test.args); err == nil {
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
