package main

import (
	"errors"
	"testing"

	"golang.org/x/sys/unix"
)

// openStrayFd opens /dev/null WITHOUT close-on-exec at an fd >= 100, the shape
// of a stray descriptor the supervisor inherited from its own parent.
func openStrayFd(t *testing.T) int {
	t.Helper()
	fd, err := unix.Open("/dev/null", unix.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	high, err := unix.FcntlInt(uintptr(fd), unix.F_DUPFD, 100)
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

func isCloexec(t *testing.T, fd int) bool {
	t.Helper()
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	if err != nil {
		t.Fatalf("F_GETFD %d: %v", fd, err)
	}
	return flags&unix.FD_CLOEXEC != 0
}

// TestMarkStrayFdsCloexecReal runs the production pair: close_range where the
// kernel and seccomp allow it, the per-fd fallback otherwise.
func TestMarkStrayFdsCloexecReal(t *testing.T) {
	fd := openStrayFd(t)
	if err := markStrayFdsCloexec(closeRangeCloexec, setCloexec); err != nil {
		t.Fatalf("markStrayFdsCloexec: %v", err)
	}
	if !isCloexec(t, fd) {
		t.Fatalf("stray fd %d is not close-on-exec after the call", fd)
	}
}

func TestMarkStrayFdsCloexecFallback(t *testing.T) {
	for _, errno := range []error{unix.ENOSYS, unix.EINVAL, unix.EPERM} {
		fd := openStrayFd(t)
		var gotFirst uint
		touched := map[int]bool{}
		set := func(fd int) error {
			touched[fd] = true
			return setCloexec(fd)
		}
		err := markStrayFdsCloexec(func(first uint) error { gotFirst = first; return errno }, set)
		if err != nil {
			t.Fatalf("%v: fallback = %v", errno, err)
		}
		if gotFirst != firstStrayFd {
			t.Fatalf("close_range from %d, want %d", gotFirst, firstStrayFd)
		}
		if !isCloexec(t, fd) {
			t.Fatalf("%v: stray fd %d is not close-on-exec after the fallback", errno, fd)
		}
		for _, low := range []int{0, 1, 2, 3, 4} {
			if touched[low] {
				t.Fatalf("fallback touched fd %d", low)
			}
		}
		if !touched[firstStrayFd] || !touched[fallbackFdLimit-1] || touched[fallbackFdLimit] {
			t.Fatalf("fallback range wrong: 5=%v 1023=%v 1024=%v", touched[firstStrayFd], touched[fallbackFdLimit-1], touched[fallbackFdLimit])
		}
	}
}

func TestMarkStrayFdsCloexecFallbackErrors(t *testing.T) {
	// EBADF (an unopened fd) is skipped; any other fallback error is returned.
	calls := 0
	err := markStrayFdsCloexec(func(uint) error { return unix.ENOSYS }, func(fd int) error {
		calls++
		if fd == 7 {
			return unix.EIO
		}
		return unix.EBADF
	})
	if !errors.Is(err, unix.EIO) || calls != 3 {
		t.Fatalf("err = %v after %d calls, want EIO after 3", err, calls)
	}
}

func TestMarkStrayFdsCloexecSkipsFallbackOnSuccess(t *testing.T) {
	if err := markStrayFdsCloexec(func(uint) error { return nil }, func(int) error {
		t.Fatal("fallback ran after close_range succeeded")
		return nil
	}); err != nil {
		t.Fatalf("err = %v", err)
	}
}
