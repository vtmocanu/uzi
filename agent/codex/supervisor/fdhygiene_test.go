package main

import (
	"bytes"
	"errors"
	"reflect"
	"slices"
	"testing"

	"golang.org/x/sys/unix"
)

// openStrayFdAt opens /dev/null WITHOUT close-on-exec at the lowest free fd
// >= lowest, the shape of a stray descriptor the supervisor inherited from its own
// parent.
func openStrayFdAt(t *testing.T, lowest int) int {
	t.Helper()
	fd, err := unix.Open("/dev/null", unix.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	high, err := unix.FcntlInt(uintptr(fd), unix.F_DUPFD, lowest)
	_ = unix.Close(fd)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Close(high) })
	if isCloexec(t, high) {
		t.Fatalf("fixture fd %d is already close-on-exec", high)
	}
	return high
}

func openStrayFd(t *testing.T) int { return openStrayFdAt(t, 100) }

// openHighStrayFd is a stray fd at or above 1100, past the old 1024-fd
// fallback; it skips when RLIMIT_NOFILE cannot hold one.
func openHighStrayFd(t *testing.T) int {
	t.Helper()
	var r unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &r); err != nil {
		t.Fatal(err)
	}
	if cur := r.Cur; cur <= 1200 {
		t.Skipf("RLIMIT_NOFILE soft limit %d is too low for a stray fd at 1100", cur)
	}
	return openStrayFdAt(t, 1100)
}

func isCloexec(t *testing.T, fd int) bool {
	t.Helper()
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	if err != nil {
		t.Fatalf("F_GETFD %d: %v", fd, err)
	}
	return flags&unix.FD_CLOEXEC != 0
}

func failCloseRange(errno error) func(uint) error {
	return func(uint) error { return errno }
}

// TestMarkStrayFdsCloexecReal runs the production seams: close_range where the
// kernel and seccomp allow it, the fallbacks otherwise.
func TestMarkStrayFdsCloexecReal(t *testing.T) {
	fd := openStrayFd(t)
	if err := markStrayFdsCloexec(realFdHygiene); err != nil {
		t.Fatalf("markStrayFdsCloexec: %v", err)
	}
	if !isCloexec(t, fd) {
		t.Fatalf("stray fd %d is not close-on-exec after the call", fd)
	}
}

// TestMarkStrayFdsCloexecEnumeratesHighFds: with close_range failing, the
// enumeration of the open fds reaches a stray fd at or above 1024.
func TestMarkStrayFdsCloexecEnumeratesHighFds(t *testing.T) {
	for _, errno := range []error{unix.ENOSYS, unix.EINVAL, unix.EPERM} {
		fd := openHighStrayFd(t)
		h := realFdHygiene
		h.closeRange = failCloseRange(errno)
		touched := map[int]bool{}
		h.set = func(fd int) error {
			touched[fd] = true
			return setCloexec(fd)
		}
		if err := markStrayFdsCloexec(h); err != nil {
			t.Fatalf("%v: markStrayFdsCloexec = %v", errno, err)
		}
		if !isCloexec(t, fd) {
			t.Fatalf("%v: stray fd %d is not close-on-exec after the fallback", errno, fd)
		}
		for low := range firstStrayFd {
			if touched[low] {
				t.Fatalf("fallback touched fd %d", low)
			}
		}
	}
}

// TestListOpenFdsExcludesItsOwnFd: the listing contains a known open fd and
// never the directory fd it read through (which is closed by the time it
// returns).
func TestListOpenFdsExcludesItsOwnFd(t *testing.T) {
	fd := openStrayFd(t)
	fds, err := listOpenFds()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(fds, fd) {
		t.Fatalf("listing %v lacks open fd %d", fds, fd)
	}
	for _, got := range fds {
		if _, err := unix.FcntlInt(uintptr(got), unix.F_GETFD, 0); err != nil {
			t.Fatalf("listed fd %d is not open: %v", got, err)
		}
	}
}

// TestMarkStrayFdsCloexecFailsClosed: when close_range fails and the fd
// listing fails too, or a set on a listed fd fails with anything but EBADF, the
// call returns an error (so the supervisor never forks) instead of sweeping a
// numeric range that could miss a stray fd above it.
func TestMarkStrayFdsCloexecFailsClosed(t *testing.T) {
	// The listing fails.
	err := markStrayFdsCloexec(fdHygiene{
		closeRange: failCloseRange(unix.EPERM),
		listOpen:   func() ([]int, error) { return nil, unix.ENOENT },
		set:        func(int) error { t.Fatal("set ran without a listing"); return nil },
	})
	if !errors.Is(err, unix.ENOENT) {
		t.Fatalf("err = %v, want the listing error", err)
	}
	// A set on a listed fd fails with a non-EBADF error.
	calls := 0
	err = markStrayFdsCloexec(fdHygiene{
		closeRange: failCloseRange(unix.ENOSYS),
		listOpen:   func() ([]int, error) { return []int{5, 7, 9}, nil },
		set: func(fd int) error {
			calls++
			if fd == 7 {
				return unix.EIO
			}
			return nil
		},
	})
	if !errors.Is(err, unix.EIO) || calls != 2 {
		t.Fatalf("err = %v after %d calls, want EIO after 2", err, calls)
	}
}

// TestMarkStrayFdsCloexecEnumerationSkipsEBADF: a listed fd closed before its
// set (EBADF) is skipped and does not fail the call.
func TestMarkStrayFdsCloexecEnumerationSkipsEBADF(t *testing.T) {
	var set []int
	err := markStrayFdsCloexec(fdHygiene{
		closeRange: failCloseRange(unix.EPERM),
		listOpen:   func() ([]int, error) { return []int{0, 4, 5, 9, 3000}, nil },
		set: func(fd int) error {
			set = append(set, fd)
			if fd == 9 {
				return unix.EBADF
			}
			return nil
		},
	})
	if err != nil || !reflect.DeepEqual(set, []int{5, 9, 3000}) {
		t.Fatalf("err = %v, set = %v, want nil and [5 9 3000]", err, set)
	}
}

func TestMarkStrayFdsCloexecSkipsFallbackOnSuccess(t *testing.T) {
	fail := func() {
		t.Fatal("a fallback ran after close_range succeeded")
	}
	if err := markStrayFdsCloexec(fdHygiene{
		closeRange: func(uint) error { return nil },
		listOpen:   func() ([]int, error) { fail(); return nil, nil },
		set:        func(int) error { fail(); return nil },
	}); err != nil {
		t.Fatalf("err = %v", err)
	}
}

// prelaunchRecorder is a prelaunchSeams whose steps record their order.
type prelaunchRecorder struct {
	calls     []string
	hygieneOK bool
}

func (r *prelaunchRecorder) seams() prelaunchSeams {
	step := func(name string) { r.calls = append(r.calls, name) }
	return prelaunchSeams{
		fdHygiene: func() error {
			step("hygiene")
			if r.hygieneOK {
				return nil
			}
			return unix.EPERM
		},
		dropCaps: func() error { step("dropCaps"); return nil },
		profile:  func(int) (procStatus, string) { step("profile"); return procStatus{UID: 10003}, "" },
		trustFds: func() { step("trustFds") },
		setup: func(string, int) (*commandTmp, error) {
			step("setup")
			return &commandTmp{}, nil
		},
		launch: func([]string) (int, error) { step("launch"); return 42, nil },
	}
}

var prelaunchArgs = []string{"--expect-uid", "10003", "--cleanup-token", testToken, "--", "/bin/true"}

// TestPrelaunchRunsHygieneFirst: the fd hygiene runs before the profile, the
// tmp setup and the launch.
func TestPrelaunchRunsHygieneFirst(t *testing.T) {
	var buf bytes.Buffer
	r := &prelaunchRecorder{hygieneOK: true}
	tmp, pid, st, ok := prelaunch(&evidence{w: &buf}, prelaunchArgs, r.seams())
	if !ok || tmp == nil || pid != 42 || st.UID != 10003 {
		t.Fatalf("prelaunch = %v, %d, %+v, %v", tmp, pid, st, ok)
	}
	want := []string{"hygiene", "profile", "trustFds", "setup", "launch"}
	if !reflect.DeepEqual(r.calls, want) {
		t.Fatalf("steps = %v, want %v", r.calls, want)
	}
	if buf.Len() != 0 {
		t.Fatalf("unexpected evidence %q", buf.String())
	}
}

// TestPrelaunchHygieneFailureNeverLaunches: a hygiene failure emits the
// pre-fork abnormal "fd hygiene failed" and runs no later step.
func TestPrelaunchHygieneFailureNeverLaunches(t *testing.T) {
	var buf bytes.Buffer
	r := &prelaunchRecorder{}
	if _, _, _, ok := prelaunch(&evidence{w: &buf}, prelaunchArgs, r.seams()); ok {
		t.Fatal("prelaunch reported ok after a hygiene failure")
	}
	if !reflect.DeepEqual(r.calls, []string{"hygiene"}) {
		t.Fatalf("steps = %v, want only the hygiene", r.calls)
	}
	lines := decodeLines(t, &buf)
	if len(lines) != 1 || lines[0]["event"] != "abnormal" || lines[0]["reason"] != "fd hygiene failed" {
		t.Fatalf("evidence = %v, want one abnormal \"fd hygiene failed\"", lines)
	}
}

// TestPrelaunchProfileFailureNeverLaunches: a profile failure is its reason and
// neither the tmp nor the child is set up.
func TestPrelaunchProfileFailureNeverLaunches(t *testing.T) {
	var buf bytes.Buffer
	r := &prelaunchRecorder{hygieneOK: true}
	p := r.seams()
	p.profile = func(int) (procStatus, string) {
		r.calls = append(r.calls, "profile")
		return procStatus{}, "profile:uid"
	}
	if _, _, _, ok := prelaunch(&evidence{w: &buf}, prelaunchArgs, p); ok {
		t.Fatal("prelaunch reported ok after a profile failure")
	}
	if !reflect.DeepEqual(r.calls, []string{"hygiene", "profile"}) {
		t.Fatalf("steps = %v", r.calls)
	}
	lines := decodeLines(t, &buf)
	if len(lines) != 1 || lines[0]["reason"] != "profile:uid" {
		t.Fatalf("evidence = %v", lines)
	}
}
