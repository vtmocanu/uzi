// Command uzi-codex-command-sandbox applies a fail-closed Landlock filesystem
// policy before starting one model-authorized command. It is launched only as
// runner-cmd (uid 10003) beneath uzi-codex-supervisor.
//
// PRD #1493 M3 — Landlock is optional through an operator-chosen mode carried in
// argv by the trusted worker (never model input):
//
//   - required (the default when --mode is absent): behaves exactly as before —
//     Landlock is applied, and any probe error OR an unavailable kernel is fatal.
//   - best-effort: Landlock is applied whenever the kernel offers it (still
//     failing closed if applying it fails); ONLY a probe result of "unavailable"
//     (ENOSYS/EOPNOTSUPP) runs the command WITHOUT filesystem confinement,
//     relying on the uid split. It never relaxes no_new_privs, the private 0700
//     tmp, the cwd-inside-root check or exit-status passthrough.
//
// "Unavailable" is defined by errno (D8): a version probe returning ENOSYS or
// EOPNOTSUPP is unavailable; any other errno, an ABI below 1, and every later
// create/add-rule/restrict/deny-probe failure is an ERROR and stays fatal in
// BOTH modes.
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

// The distinct exit codes for the `--probe` sub-invocation. `--probe` performs
// exactly ONE landlock_create_ruleset version probe (no directory created, no
// policy applied) and exits with one of these so the trusted worker's
// advertisement wrapper can distinguish the three outcomes:
//
//	0  → Landlock is available (ABI >= 1).
//	10 → Landlock is unavailable (ENOSYS / EOPNOTSUPP).
//	11 → the probe hit an error (any other errno, or an ABI below 1).
const (
	probeExitAvailable   = 0
	probeExitUnavailable = 10
	probeExitError       = 11
)

// sandboxMode is the worker-supplied enforcement mode. It comes ONLY from the
// trusted argv built by the worker; a --mode token appearing AFTER the `--`
// separator belongs to the child command and is never read as the mode.
type sandboxMode string

const (
	modeRequired   sandboxMode = "required"
	modeBestEffort sandboxMode = "best-effort"
)

// landlockAvailability is the typed probe result shared by `--probe` and the
// enforcement path (D8).
type landlockAvailability int

const (
	landlockAvailable   landlockAvailability = iota // ABI >= 1
	landlockUnavailable                             // ENOSYS or EOPNOTSUPP
	landlockError                                   // any other errno, or an ABI < 1
)

// policyAction is the decision the mode + probe result imply.
type policyAction int

const (
	actionApply      policyAction = iota // apply Landlock (both modes, when available)
	actionUnconfined                     // best-effort degrade: run without Landlock
	actionFatal                          // fail closed (required-unavailable, or any error)
)

// versionProbe performs the single landlock_create_ruleset version probe and
// returns the ABI value plus its errno. It is a seam so tests can drive the
// errno/ABI classification without a real kernel.
type versionProbe func() (abi uintptr, errno syscall.Errno)

// realVersionProbe issues the real landlock_create_ruleset(NULL, 0,
// LANDLOCK_CREATE_RULESET_VERSION) syscall.
func realVersionProbe() (uintptr, syscall.Errno) {
	abi, _, errno := syscall.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, landlockVersionFlag)
	return abi, errno
}

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
	// `--probe` is a standalone, no-policy version probe (see probeExit* codes).
	if len(os.Args) == 2 && os.Args[1] == "--probe" {
		os.Exit(probeExitCode(realVersionProbe))
	}
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
	root, tmp, cwd, mode, child, err := parseArgs(args)
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
	if err := applyPolicy(root, tmp, mode, realVersionProbe, confine, applyNoNewPrivs); err != nil {
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

// parseArgs reads the trusted worker-built argv. Grammar:
//
//	--root R --tmp T --cwd C [--mode required|best-effort] -- CMD...
//
// The three path flags stay positional (as before); the mode flag is OPTIONAL
// and, when present, sits immediately before the `--` separator. Everything
// after `--` is the child command verbatim, so a `--mode` token there is part of
// the CHILD and is NEVER read as the sandbox mode (the trust property: the mode
// comes only from this argv, built by the trusted worker).
func parseArgs(args []string) (root, tmp, cwd string, mode sandboxMode, child []string, err error) {
	mode = modeRequired
	if len(args) < 7 || args[0] != "--root" || args[2] != "--tmp" || args[4] != "--cwd" {
		return "", "", "", "", nil, errors.New("invalid arguments")
	}
	root, tmp, cwd = args[1], args[3], args[5]
	rest := args[6:]
	// An OPTIONAL `--mode <value>` may precede the separator. Parsed only here,
	// before `--`, so a child argument spelled `--mode` can never reach it.
	if len(rest) >= 2 && rest[0] == "--mode" {
		parsed, modeErr := parseMode(rest[1])
		if modeErr != nil {
			return "", "", "", "", nil, modeErr
		}
		mode = parsed
		rest = rest[2:]
	}
	if len(rest) < 1 || rest[0] != "--" {
		return "", "", "", "", nil, errors.New("missing -- separator")
	}
	child = rest[1:]
	if !filepath.IsAbs(root) || !filepath.IsAbs(tmp) || !filepath.IsAbs(cwd) || len(child) == 0 || !filepath.IsAbs(child[0]) {
		return "", "", "", "", nil, errors.New("paths must be absolute")
	}
	if filepath.Clean(root) == string(filepath.Separator) {
		return "", "", "", "", nil, errors.New("root sandbox is forbidden")
	}
	rel, relErr := filepath.Rel(root, cwd)
	if relErr != nil || rel == ".." || (len(rel) >= 3 && rel[:3] == ".."+string(filepath.Separator)) {
		return "", "", "", "", nil, errors.New("cwd escapes root")
	}
	return root, tmp, cwd, mode, child, nil
}

// parseMode strictly maps a mode token to a sandboxMode; an unknown value is
// rejected (fail closed).
func parseMode(value string) (sandboxMode, error) {
	switch sandboxMode(value) {
	case modeRequired:
		return modeRequired, nil
	case modeBestEffort:
		return modeBestEffort, nil
	default:
		return "", fmt.Errorf("invalid sandbox mode %q (expected %q or %q)", value, modeRequired, modeBestEffort)
	}
}

// classifyLandlock runs the version probe ONCE and classifies the outcome (D8).
// The returned abi is meaningful only when the result is landlockAvailable.
func classifyLandlock(probe versionProbe) (avail landlockAvailability, abi int, err error) {
	value, errno := probe()
	if errno != 0 {
		if errno == unix.ENOSYS || errno == unix.EOPNOTSUPP {
			return landlockUnavailable, 0, nil
		}
		return landlockError, 0, fmt.Errorf("landlock version probe failed: %w", errno)
	}
	if value < 1 {
		return landlockError, 0, fmt.Errorf("landlock ABI %d is below 1", int(value))
	}
	return landlockAvailable, int(value), nil
}

// probeExitCode maps the classification onto a `--probe` exit code.
func probeExitCode(probe versionProbe) int {
	avail, _, err := classifyLandlock(probe)
	switch avail {
	case landlockAvailable:
		return probeExitAvailable
	case landlockUnavailable:
		return probeExitUnavailable
	default:
		if err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "uzi-codex-command-sandbox: --probe: %v\n", err)
		}
		return probeExitError
	}
}

// decidePolicy resolves the action for a mode + probe result WITHOUT applying
// any policy, so it is fully unit-testable through the injected seam. The abi it
// returns is meaningful only for actionApply.
func decidePolicy(probe versionProbe, mode sandboxMode) (action policyAction, abi int, err error) {
	avail, abiValue, classifyErr := classifyLandlock(probe)
	switch avail {
	case landlockAvailable:
		return actionApply, abiValue, nil
	case landlockUnavailable:
		if mode == modeBestEffort {
			return actionUnconfined, 0, nil
		}
		return actionFatal, 0, errors.New("landlock unavailable")
	default:
		// landlockError — fatal in BOTH modes (a seccomp/implementation fault
		// must never be mistaken for "no Landlock").
		return actionFatal, 0, classifyErr
	}
}

// confineFunc and noNewPrivsFunc are injectable seams for applyPolicy's two
// enforcement primitives (mirroring the probeOpen/versionProbe seams in this
// file), so the mode→enforcement dispatch is hermetically testable without a real
// kernel. Production always passes the real confine/applyNoNewPrivs, so shipped
// behaviour is byte-identical; confine() still calls the real applyNoNewPrivs
// internally regardless of these seams.
type confineFunc func(root, tmp string, abi int) error

type noNewPrivsFunc func() error

// applyPolicy decides from mode + probe what to do, then does it. actionApply
// applies Landlock and fails closed on any apply error (both modes);
// actionUnconfined (best-effort, kernel without Landlock) runs the child without
// filesystem confinement but STILL sets no_new_privs; actionFatal returns the
// error so realMain fails closed. The two enforcement primitives are injected
// (confineFn/noNewPrivsFn) so the dispatch itself is unit-testable; realMain
// passes the real confine/applyNoNewPrivs.
func applyPolicy(root, tmp string, mode sandboxMode, probe versionProbe, confineFn confineFunc, noNewPrivsFn noNewPrivsFunc) error {
	action, abi, err := decidePolicy(probe, mode)
	switch action {
	case actionApply:
		return confineFn(root, tmp, abi)
	case actionUnconfined:
		// Degraded best-effort: no worktree confinement, but the uid split and
		// no_new_privs still hold (the private 0700 tmp and cwd-inside-root check
		// are enforced by realMain/parseArgs regardless of this branch).
		return noNewPrivsFn()
	default:
		return err
	}
}

// applyNoNewPrivs sets PR_SET_NO_NEW_PRIVS on the calling thread. Kept in both
// the confined and the degraded paths so a child can never regain privileges via
// a set-uid/fcap execve.
func applyNoNewPrivs() error {
	return unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0)
}

// confine applies the full Landlock policy for an available kernel. `abi` selects
// the access rights this kernel understands (from the version probe). EVERY
// failure here (real create, add-rule, restrict_self, deny-probe) is an ERROR
// that fails closed in BOTH modes; the caller only reaches confine when the probe
// classified the kernel as available.
func confine(root, tmp string, abi int) error {
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
	if err := applyNoNewPrivs(); err != nil {
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
