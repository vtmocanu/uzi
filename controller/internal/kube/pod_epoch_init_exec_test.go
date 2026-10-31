//go:build unix

package kube

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// This bounded fixture substitutes only the image helper, not the rendered shell
// protocol. Python fcntl locks inherited descriptors; it does not prove the
// production util-linux implementation (the host flock is BusyBox).
const epochFixture = `import fcntl, os, sys, time
command, root = sys.argv[1:3]
lock = root + "/.uzi-epoch.lock"
def event(value):
    with open(root + "/events", "a") as f: f.write(value + "\n")
def wait(predicate):
    deadline = time.monotonic() + 5
    while not predicate():
        if time.monotonic() >= deadline: raise RuntimeError("fixture deadline")
        time.sleep(0.01)
def acquire(fd, mode):
    def attempt():
        try:
            fcntl.flock(fd, mode | fcntl.LOCK_NB)
            return True
        except BlockingIOError:
            return False
    wait(attempt)
if command == os.environ.get("EPOCH_FIXTURE_STALL"):
    event(command + "-stalled")
    wait(lambda: os.path.exists(root + "/release"))
if command == os.environ.get("EPOCH_FIXTURE_FAIL"):
    event(command + "-failed")
    sys.exit(1)
if command == "protected":
    with open(lock) as f:
        try:
            fcntl.flock(f, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            event("body-SH")
        else:
            raise RuntimeError("shell lost SH before protected body")
elif command == "seed":
    with open(lock, "a+") as f:
        try:
            fcntl.flock(f, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            event("record-busy")
        else:
            event("record-EX")
            with open(root + "/record", "w") as r: r.write(sys.argv[3])
elif command == "prepare":
    event("prepare")
    s = os.stat(lock)
    print(str(s.st_dev) + ":" + str(s.st_ino))
    if os.environ.get("EPOCH_FIXTURE_REMOVE_ON_PREPARE") == "1":
        os.unlink(lock)
        event("prepare-unlinked")
elif command == "hold":
    event("SH-wait")
    fd = int(sys.argv[4])
    s = os.fstat(fd)
    assert sys.argv[3] == str(s.st_dev) + ":" + str(s.st_ino)
    acquire(fd, fcntl.LOCK_SH)
    event("SH-acquired")
elif command == "block":
    with open(lock, "a+") as f:
        acquire(f.fileno(), fcntl.LOCK_EX)
        event("block-EX")
        wait(lambda: os.path.exists(root + "/release"))
else:
    raise RuntimeError("unknown fixture operation")
`

type epochInitHost struct {
	t                                                    *testing.T
	data, nix, src, marker, layout, helper, python, node string
}

func newEpochInitHost(t *testing.T) *epochInitHost {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python fcntl fixture unavailable")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node unavailable for image plain-JS root validator")
	}
	root := t.TempDir()
	h := &epochInitHost{t: t, node: node, data: filepath.Join(root, "data"), nix: filepath.Join(root, "nix"),
		src: seedSource(t), marker: markerFileWith(t, "toolchain"), layout: filepath.Join(root, "layout"),
		helper: filepath.Join(root, "helper.py"), python: python}
	for _, p := range []string{h.data, h.nix} {
		if err := os.Mkdir(p, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	h.write(h.helper, epochFixture)
	h.write(h.layout, runnerLayoutMarkerValue)
	h.write(filepath.Join(h.data, ".uzi-epoch.lock"), "")
	return h
}
func (h *epochInitHost) write(path, value string) {
	h.t.Helper()
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		h.t.Fatal(err)
	}
}
func (h *epochInitHost) script(seed bool) string {
	pod := RenderDeployment(dockerTestConfig(), desiredDocker("abc"), testSpec(h.t, "base", "m")).Spec.Template.Spec
	name := runnerInitContainerName
	if seed {
		name = seedContainerName
	}
	script := containerByName(h.t, pod.InitContainers, name).Command[2]
	// Observe ownership after hold exits, at the protected body boundary.
	probe := h.python + " " + h.helper + " protected " + h.data + "\n"
	if seed {
		script = strings.Replace(script, "WANT=", probe+"WANT=", 1)
	} else {
		script = strings.Replace(script, "# 2. Create", probe+"# 2. Create", 1)
	}
	script = strings.NewReplacer(
		"/usr/local/bin/node /usr/local/lib/uzi/pod-epoch-init.cjs", h.python+" "+h.helper,
		"/usr/local/bin/node -e", h.node+" -e",
		dataSeedMountPath, h.data, nixSeedMountPath, h.nix, toolchainProfileMarker, h.marker,
		runnerLayoutMarkerFile, h.layout,
	).Replace(script)
	// Synthetic mount ownership: the unprivileged host cannot prepare gid10001.
	// Keep the real read-only posture validator, substituting only uid/gid.
	script = strings.ReplaceAll(script, "s.gid !== 10001", "s.gid !== "+strconv.Itoa(os.Getgid()))
	script = strings.ReplaceAll(script, "[0, 10001]", "[0, "+strconv.Itoa(os.Getuid())+"]")
	if seed {
		script = strings.ReplaceAll(script, "cd /nix\n", "cd "+h.src+"\n")
		// The fixture posture needs group rwx + setgid, exactly as production.
	}
	return script
}
func (h *epochInitHost) command(seed bool, uid string) (*exec.Cmd, context.CancelFunc) {
	h.t.Helper()
	if !seed && os.Getuid() == 0 {
		h.t.Skip("root runner-init requires image worker/runner accounts; covered by the worker-uid image lane")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	cmd := exec.CommandContext(ctx, "/bin/bash", "-c", h.script(seed)) //nolint:gosec // G204: fixed rendered script and test-owned fixture paths, not user input.
	cmd.Env = append(os.Environ(), "UZI_EPOCH_LOCK=1", "UZI_POD_UID="+uid)
	return cmd, cancel
}
func (h *epochInitHost) run(seed bool, uid string) (string, error) {
	h.t.Helper()
	cmd, cancel := h.command(seed, uid)
	defer cancel()
	out, err := cmd.CombinedOutput()
	return string(out), err
}
func (h *epochInitHost) events() string {
	h.t.Helper()
	b, err := os.ReadFile(filepath.Join(h.data, "events"))
	if err != nil && !os.IsNotExist(err) {
		h.t.Fatal(err)
	}
	return string(b)
}
func (h *epochInitHost) awaitEvent(event string) {
	h.t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(h.events(), event+"\n") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.t.Fatalf("waiting for %s: %q", event, h.events())
}
func (h *epochInitHost) posture() {
	h.t.Helper()
	if err := os.Chmod(h.data, os.ModeSetgid|0o770); err != nil {
		h.t.Fatal(err)
	}
}

// errEpochRaceSetup marks a missing-after-prepare run that ended before the
// fixture proved its injected unlink: execution evidence, never race evidence.
var errEpochRaceSetup = errors.New("missing-after-prepare race did not run")

// missingAfterPrepare runs the rendered shell with the lock unlinked inside
// prepare and classifies the outcome. The shell and every fixture process share
// one process group, which cancellation kills whole; CombinedOutput returns only
// after every holder of its pipe has exited, so the snapshot below cannot race a
// surviving fixture.
func (h *epochInitHost) missingAfterPrepare(ctx context.Context, seed bool, extraEnv ...string) error {
	h.t.Helper()
	script := h.script(seed)
	// Failure precedes UID-specific writes, including on a root host.
	cmd := exec.CommandContext(ctx, "/bin/bash", "-c", script) //nolint:gosec // G204: only fixed rendered source and test-owned paths.
	cmd.Env = append(append(os.Environ(), "UZI_EPOCH_LOCK=1", "UZI_POD_UID=pod-race", "EPOCH_FIXTURE_REMOVE_ON_PREPARE=1", "LC_ALL=C"), extraEnv...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	out, runErr := cmd.CombinedOutput()
	events := h.events()
	_, lockErr := os.Lstat(filepath.Join(h.data, ".uzi-epoch.lock"))
	tail := out
	if len(tail) > 4096 {
		tail = tail[len(tail)-4096:]
	}
	diag := fmt.Sprintf("err=%v ctx=%v lock=%v events=%q output=%q", runErr, ctx.Err(), lockErr, events, tail)
	if ctx.Err() != nil {
		return fmt.Errorf("%w: context ended during execution: %s", errEpochRaceSetup, diag)
	}
	if !strings.Contains(events, "prepare-unlinked\n") {
		return fmt.Errorf("%w: fixture never reached the injected unlink: %s", errEpochRaceSetup, diag)
	}
	if cmd.ProcessState != nil && cmd.ProcessState.Success() {
		return fmt.Errorf("raced open succeeded: %s", diag)
	}
	if !os.IsNotExist(lockErr) {
		return fmt.Errorf("shell created a replacement before SH: %s", diag)
	}
	if strings.Contains(events, "SH-wait") || strings.Contains(events, "SH-acquired") || strings.Contains(events, "body-SH") {
		return fmt.Errorf("protected initialization continued after raced open: %s", diag)
	}
	want := "prepare\nprepare-unlinked\n"
	if seed {
		want = "record-EX\n" + want
	}
	if events != want {
		return fmt.Errorf("unexpected fixture events (want %q): %s", want, diag)
	}
	if !strings.Contains(string(out), ".uzi-epoch.lock: No such file or directory") {
		return fmt.Errorf("shell did not refuse the missing lock: %s", diag)
	}
	return nil
}

func TestPodEpochInitExecMissingAfterPrepareNeverCreatesState(t *testing.T) {
	for _, seed := range []bool{true, false} {
		t.Run(fmt.Sprintf("seed=%v", seed), func(t *testing.T) {
			h := newEpochInitHost(t)
			h.posture()
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			if err := h.missingAfterPrepare(ctx, seed); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// An exit or cancellation before the injected unlink leaves the original lock
// in place; it must be reported as an execution failure, never as recreation.
func TestPodEpochInitExecMissingAfterPrepareDiagnosesEarlyExit(t *testing.T) {
	for _, seed := range []bool{true, false} {
		for _, fault := range []string{"fail", "cancelled", "stall"} {
			t.Run(fmt.Sprintf("seed=%v/%s", seed, fault), func(t *testing.T) {
				h := newEpochInitHost(t)
				h.posture()
				ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
				defer cancel()
				var env []string
				var want string
				var stalled chan bool
				switch fault {
				case "fail":
					env, want = []string{"EPOCH_FIXTURE_FAIL=prepare"}, "prepare-failed\n"
				case "cancelled":
					cancel()
				case "stall":
					env, want = []string{"EPOCH_FIXTURE_STALL=prepare"}, "prepare-stalled\n"
					stalled = make(chan bool, 1)
					// Cancel on the fixture's event, mid-run and before the unlink.
					go func() {
						seen := false
						for !seen && ctx.Err() == nil {
							b, _ := os.ReadFile(filepath.Join(h.data, "events"))
							if seen = strings.Contains(string(b), "prepare-stalled\n"); !seen {
								time.Sleep(10 * time.Millisecond)
							}
						}
						stalled <- seen
						cancel()
					}()
				}
				if seed && want != "" {
					want = "record-EX\n" + want
				}
				err := h.missingAfterPrepare(ctx, seed, env...)
				if !errors.Is(err, errEpochRaceSetup) {
					t.Fatalf("early exit accepted or misclassified: %v", err)
				}
				if !strings.Contains(err.Error(), "ctx=") || !strings.Contains(err.Error(), "events=") {
					t.Fatalf("diagnostics lack context state or events: %v", err)
				}
				if stalled != nil && !<-stalled {
					t.Fatalf("cancelled before the fixture stalled: %v", err)
				}
				if got := h.events(); got != want {
					t.Fatalf("events = %q, want %q", got, want)
				}
				// After termination the original lock remains: nothing unlinked it late.
				if _, err := os.Lstat(filepath.Join(h.data, ".uzi-epoch.lock")); err != nil {
					t.Fatalf("lock changed after an early exit: %v", err)
				}
			})
		}
	}
}

func TestPodEpochInitExecCompleteKubeletRootPredicate(t *testing.T) {
	for _, tc := range []struct {
		perms         os.FileMode
		setgid, valid bool
	}{
		{0o770, true, true}, {0o775, true, true}, {0o070, true, false},
		{0o670, true, false}, {0o570, true, false}, {0o370, true, false},
		{0o770, false, false}, {0o777, true, false},
	} {
		t.Run(fmt.Sprintf("mode=%o/setgid=%v", tc.perms, tc.setgid), func(t *testing.T) {
			h := newEpochInitHost(t)
			mode := tc.perms
			if tc.setgid {
				mode |= os.ModeSetgid
			}
			if err := os.Chmod(h.data, mode); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := os.Chmod(h.data, 0o700); err != nil { //nolint:gosec // G302: private test directory needs owner search permission for cleanup.
					t.Error(err)
				}
			}()
			script := h.script(true)
			at := strings.Index(script, h.python+" "+h.helper+" seed")
			if at < 0 {
				t.Fatal("posture-prefix boundary absent")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "/bin/bash", "-c", script[:at]) //nolint:gosec // G204: literal read-only validator with test-owned paths.
			out, err := cmd.CombinedOutput()
			if tc.valid && err != nil {
				t.Fatalf("matching root refused: %v %s", err, out)
			}
			if !tc.valid && (err == nil || !strings.Contains(string(out), "unsafe data root posture")) {
				t.Fatalf("nonmatching root accepted or failed after validation: %v %s", err, out)
			}
		})
	}
}

func TestPodEpochInitExecColdAndWarmSeedRecordBeforeShared(t *testing.T) {
	h := newEpochInitHost(t)
	h.posture()
	for _, uid := range []string{"pod-one", "pod-two"} {
		h.write(filepath.Join(h.data, "events"), "")
		if out, err := h.run(true, uid); err != nil {
			t.Fatalf("seed: %v\n%s", err, out)
		}
		if got := h.events(); got != "record-EX\nprepare\nSH-wait\nSH-acquired\nbody-SH\n" {
			t.Fatalf("ordering = %q", got)
		}
		b, err := os.ReadFile(filepath.Join(h.data, "record"))
		if err != nil || string(b) != uid {
			t.Fatalf("warm seed did not record pod: %q %v", b, err)
		}
		if !seeded(t, h.nix) {
			t.Fatal("cold seed did not copy image")
		}
		st, err := os.Stat(h.data)
		if err != nil || st.Mode().Perm() != 0o770 || st.Mode()&os.ModeSetgid == 0 {
			t.Fatalf("seed mutated mount root: %v %v", st, err)
		}
	}
}

func TestPodEpochInitExecBusyRecordSkipsButProtectedInitWaits(t *testing.T) {
	for _, seed := range []bool{true, false} {
		t.Run(fmt.Sprintf("seed=%v", seed), func(t *testing.T) {
			h := newEpochInitHost(t)
			h.posture()
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			blocker := exec.CommandContext(ctx, h.python, h.helper, "block", h.data) //nolint:gosec // G204: interpreter from LookPath, literal helper and paths owned by this test.
			if err := blocker.Start(); err != nil {
				t.Fatal(err)
			}
			// Always settle our exact child, including failed assertions.
			defer func() {
				h.write(filepath.Join(h.data, "release"), "")
				if err := blocker.Wait(); err != nil {
					t.Errorf("blocker: %v", err)
				}
			}()
			h.awaitEvent("block-EX")
			cmd, stop := h.command(seed, "next-pod")
			defer stop()
			done := make(chan error, 1)
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			go func() { done <- cmd.Wait() }()
			defer func() {
				stop()
				if done != nil {
					// After stop() cancels the child its exit error is expected; drain and record it.
					if err := <-done; err != nil {
						t.Logf("next-pod exit after stop: %v", err)
					}
				}
			}()
			h.awaitEvent("SH-wait")
			if seed && !strings.Contains(h.events(), "record-busy\nprepare\nSH-wait\n") {
				t.Fatalf("record did not skip EX: %q", h.events())
			}
			if strings.Contains(h.events(), "SH-acquired") {
				t.Fatal("SH passed live EX")
			}
			for _, path := range []string{filepath.Join(h.data, dindWorkdirSubPath), filepath.Join(h.nix, nixSentinel), filepath.Join(h.data, "record")} {
				if _, err := os.Lstat(path); !os.IsNotExist(err) {
					t.Fatalf("protected state accessed before SH: %s (%v)", path, err)
				}
			}
			select {
			case err := <-done:
				done = nil
				t.Fatalf("initializer exited under EX: %v", err)
			default:
			}
			h.write(filepath.Join(h.data, "release"), "")
			err := <-done
			done = nil
			if err != nil {
				t.Fatalf("initializer after EX release: %v", err)
			}
			h.awaitEvent("SH-acquired")
			// An independent nonblocking EX succeeds after shell exit: FD9 was released.
			probe := exec.CommandContext(ctx, h.python, "-c", "import fcntl,sys; f=open(sys.argv[1]); fcntl.flock(f,fcntl.LOCK_EX|fcntl.LOCK_NB)", filepath.Join(h.data, ".uzi-epoch.lock")) //nolint:gosec // G204: fixed Python source and a test-owned path.
			if out, err := probe.CombinedOutput(); err != nil {
				t.Fatalf("FD9 leaked: %v %s", err, out)
			}
		})
	}
}

func TestPodEpochInitExecRefusesRootPostureBeforeSeed(t *testing.T) {
	h := newEpochInitHost(t)
	// Missing setgid/group access refuses before helper or protected state writes.
	if out, err := h.run(true, "pod"); err == nil || !strings.Contains(out, "unsafe data root posture") {
		t.Fatalf("unsafe root: %v %s", err, out)
	}
	if got := h.events(); got != "" {
		t.Fatalf("helper ran before root validation: %q", got)
	}
	st, err := os.Stat(h.data)
	if err != nil || st.Mode().Perm() != 0o700 || st.Mode()&os.ModeSetgid != 0 {
		t.Fatalf("root posture mutated: %v %v", st, err)
	}
}

func TestPodEpochInitExecHelperFailureStopsProtectedWrites(t *testing.T) {
	for _, seed := range []bool{true, false} {
		for _, operation := range []string{"prepare", "hold"} {
			t.Run(fmt.Sprintf("seed=%v/%s", seed, operation), func(t *testing.T) {
				h := newEpochInitHost(t)
				h.posture()
				cmd, cancel := h.command(seed, "pod")
				defer cancel()
				cmd.Env = append(cmd.Env, "EPOCH_FIXTURE_FAIL="+operation)
				if out, err := cmd.CombinedOutput(); err == nil {
					t.Fatalf("helper failure admitted initialization: %s", out)
				}
				if !strings.Contains(h.events(), operation+"-failed\n") {
					t.Fatalf("did not reach injected failure: %q", h.events())
				}
				for _, path := range []string{filepath.Join(h.data, dindWorkdirSubPath), filepath.Join(h.nix, nixSentinel)} {
					if _, err := os.Lstat(path); !os.IsNotExist(err) {
						t.Fatalf("protected state written on helper failure: %s (%v)", path, err)
					}
				}
			})
		}
	}
}

func TestPodEpochInitExecRunnerImageGatePrecedesHelper(t *testing.T) {
	h := newEpochInitHost(t)
	h.write(h.layout, "old")
	if out, err := h.run(false, "pod"); err == nil || !strings.Contains(out, "worker image lacks runner-shared layout support") {
		t.Fatalf("image gate: %v %s", err, out)
	}
	if got := h.events(); got != "" {
		t.Fatalf("helper ran before image gate: %q", got)
	}
}
