//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Exercise the production reader against an owned Linux PTY, including its
// library's restoration path on a controlled read error.
func TestVaultTerminalPassword(t *testing.T) {
	for _, readError := range []bool{false, true} {
		t.Run(fmt.Sprintf("readError=%t", readError), func(t *testing.T) {
			fd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
			if err != nil {
				t.Fatal(err)
			}
			master := os.NewFile(uintptr(fd), "vault-test-master")
			defer func() {
				if err := master.Close(); err != nil {
					t.Errorf("close PTY master: %v", err)
				}
			}()
			if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
				t.Fatal(err)
			}
			n, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
			if err != nil {
				t.Fatal(err)
			}
			slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|unix.O_NOCTTY, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := slave.Close(); err != nil {
					t.Errorf("close PTY slave: %v", err)
				}
			}()
			before, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
			if err != nil {
				t.Fatal(err)
			}
			if before.Lflag&unix.ECHO == 0 {
				t.Fatal("PTY starts without echo; invalid control")
			}
			type result struct {
				password []byte
				err      error
			}
			done := make(chan result, 1)
			go func() {
				password, err := readVaultTerminalPassword(context.Background(), slave)
				done <- result{password, err}
			}()
			deadline := time.NewTimer(3 * time.Second)
			defer deadline.Stop()
			poll := time.NewTicker(time.Millisecond)
			defer poll.Stop()
			ready := false
			for !ready {
				state, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
				if err != nil {
					t.Fatal(err)
				}
				ready = state.Lflag&unix.ECHO == 0
				if !ready {
					select {
					case <-poll.C:
					case <-deadline.C:
						t.Fatal("reader did not disable ECHO")
					}
				}
			}
			input := []byte(vaultTestSecret + "\n")
			if readError {
				// Wake the blocking read with canonical EOF. The next read sees EAGAIN,
				// forcing the library's error return while the slave stays inspectable.
				if err := unix.SetNonblock(int(slave.Fd()), true); err != nil {
					t.Fatal(err)
				}
				input = []byte{before.Cc[unix.VEOF]}
			}
			if n, err := unix.Write(fd, input); err != nil || n != len(input) {
				t.Fatalf("PTY write n=%d err=%v", n, err)
			}
			var got result
			select {
			case got = <-done:
			case <-deadline.C:
				t.Fatal("terminal read did not return")
			}
			if readError {
				if got.err == nil {
					t.Fatal("controlled read error succeeded")
				}
				if len(got.password) != 0 {
					t.Fatal("EOF returned a password")
				}
				t.Logf("controlled read-error outcome: %v", got.err)
			} else if got.err != nil || string(got.password) != vaultTestSecret {
				t.Fatalf("password read failed: %v", got.err)
			}
			after, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
			if err != nil {
				t.Fatal(err)
			}
			if *after != *before {
				t.Fatal("terminal state was not restored")
			}
			// The result synchronizes completion of all password input processing.
			// Read directly on the nonblocking fd so an empty echo queue cannot hang.
			var output [256]byte
			n, err = unix.Read(fd, output[:])
			if err != nil && !errors.Is(err, unix.EAGAIN) {
				t.Fatal(err)
			}
			if n > 0 {
				t.Fatalf("terminal echoed %d bytes with ECHO disabled", n)
			}
			clear(got.password)
		})
	}
}
