//go:build linux

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Exercise the production reader against an owned Linux PTY, including its
// restoration path on a controlled read error.
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

// Canonical EOF delivers each sub-line-cap chunk without terminating the
// password. Overflow must return without waiting for a final newline.
func TestVaultTerminalPasswordBoundPTY(t *testing.T) {
	for _, overflow := range []bool{true, false} {
		t.Run(fmt.Sprintf("overflow=%t", overflow), func(t *testing.T) {
			fd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
			if err != nil {
				t.Fatal(err)
			}
			master := os.NewFile(uintptr(fd), "vault-bound-master")
			defer func() {
				if err := master.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
					t.Errorf("close PTY master: %v", err)
				}
			}()
			// Bound all writer attempts, including cleanup if the reader regresses.
			if err := master.SetWriteDeadline(time.Now().Add(15 * time.Second)); err != nil {
				t.Fatal(err)
			}
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
			type result struct {
				password []byte
				err      error
			}
			done := make(chan result, 1)
			go func() {
				password, err := readVaultTerminalPassword(context.Background(), slave)
				done <- result{password, err}
			}()
			joined := false
			var writerDone chan error
			writerJoined := false
			defer func() {
				// Closing our master also releases a writer blocked by a failed reader.
				if err := master.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
					t.Errorf("close PTY master during cleanup: %v", err)
				}
				if writerDone != nil && !writerJoined {
					if writerErr := <-writerDone; writerErr != nil {
						t.Logf("PTY writer released during cleanup: %v", writerErr)
					}
				}
				if !joined {
					got := <-done
					clear(got.password)
				}
			}()
			deadline := time.NewTimer(15 * time.Second)
			defer deadline.Stop()
			poll := time.NewTicker(time.Millisecond)
			defer poll.Stop()
			for {
				state, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
				if err != nil {
					t.Fatal(err)
				}
				if state.Lflag&unix.ECHO == 0 {
					break
				}
				select {
				case <-poll.C:
				case <-deadline.C:
					t.Fatal("reader did not disable ECHO")
				}
			}
			writerDone = make(chan error, 1)
			go func() {
				remaining := maxVaultPasswordBytes
				for remaining > 0 {
					size := min(1024, remaining)
					chunk := bytes.Repeat([]byte{'x'}, size)
					chunk = append(chunk, before.Cc[unix.VEOF])
					if _, err := master.Write(chunk); err != nil {
						writerDone <- err
						return
					}
					remaining -= size
				}
				if !overflow {
					_, err := master.Write([]byte("\n"))
					writerDone <- err
					return
				}
				// Queue the overflow byte and its same-line tail in one write while
				// echo is off. The next consumer must never receive this tail.
				_, err := master.Write(append([]byte("xqueued_secret_tail"), before.Cc[unix.VEOF]))
				writerDone <- err
			}()
			var got result
			select {
			case got = <-done:
				joined = true
			case <-deadline.C:
				// Hang up our PTY before joining either goroutine. Cleanup cannot
				// block trying to submit more input to a stalled reader.
				if err := master.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
					t.Errorf("close PTY master after timeout: %v", err)
				}
				got = <-done
				joined = true
				if writerErr := <-writerDone; writerErr != nil {
					t.Logf("PTY writer released after timeout: %v", writerErr)
				}
				writerJoined = true
				clear(got.password)
				t.Fatal("reader waited for newline instead of rejecting overflow")
			}
			defer clear(got.password)
			if err := <-writerDone; err != nil {
				writerJoined = true
				t.Fatal(err)
			}
			writerJoined = true
			if overflow {
				if !errors.Is(got.err, errVaultPasswordTooLong) || got.password != nil {
					t.Fatalf("overflow returned length=%d err=%v", len(got.password), got.err)
				}
			} else if got.err != nil || len(got.password) != maxVaultPasswordBytes || !bytes.Equal(got.password, bytes.Repeat([]byte{'x'}, maxVaultPasswordBytes)) {
				t.Fatalf("exact limit returned length=%d err=%v", len(got.password), got.err)
			}
			after, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
			if err != nil {
				t.Fatal(err)
			}
			if *before != *after {
				t.Fatal("full terminal state was not restored")
			}
			// Submit only a newline after return, exposing any queued tail as data.
			// A nonblocking literal read has no shell and needs no cleanup goroutine.
			if _, err := master.Write([]byte("\n")); err != nil {
				t.Fatal(err)
			}
			if err := unix.SetNonblock(int(slave.Fd()), true); err != nil {
				t.Fatal(err)
			}
			var next [128]byte
			for {
				n, err := unix.Read(int(slave.Fd()), next[:])
				if err == nil {
					if n != 1 || next[0] != '\n' {
						t.Fatalf("next terminal consumer received queued input (%d bytes)", n)
					}
					break
				}
				if !errors.Is(err, unix.EAGAIN) {
					t.Fatal(err)
				}
				select {
				case <-poll.C:
				case <-deadline.C:
					t.Fatal("next terminal consumer did not receive newline")
				}
			}
		})
	}
}

// TestVaultUnlockSignalHelper is the child process for
// TestVaultUnlockSignalSubprocess; it runs the real CLI on its terminal stdin.
func TestVaultUnlockSignalHelper(t *testing.T) {
	if os.Getenv("UZI_VAULT_SIGNAL_HELPER") != "1" {
		t.Skip("subprocess helper")
	}
	root := newRootCmd(DefaultEnv())
	root.SetArgs([]string{"vault", "unlock"})
	_ = root.Execute()
	os.Exit(0)
}

// A real SIGINT or SIGTERM delivered to `uzi vault unlock` while its hidden
// prompt holds a partly typed password must discard that input, restore the
// full terminal state, exit with 128+signal and send no unlock request.
// Default signal termination would skip the restore, and a signal sent with
// kill (unlike a typed INTR) leaves the partial secret queued for the shell.
func TestVaultUnlockSignalSubprocess(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			var requests atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasPrefix(r.URL.Path, "/api/vault/unlock") {
					requests.Add(1)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte("{}"))
			}))
			defer srv.Close()

			fd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
			if err != nil {
				t.Fatal(err)
			}
			master := os.NewFile(uintptr(fd), "vault-signal-master")
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

			home := t.TempDir()
			cmd := exec.Command(os.Args[0], "-test.run=^TestVaultUnlockSignalHelper$") //nolint:gosec // G204: re-executes this test binary with a fixed argument.
			cmd.Env = append(os.Environ(),
				"UZI_VAULT_SIGNAL_HELPER=1",
				"UZI_URL="+srv.URL,
				"UZI_TOKEN=uzc_signaltest",
				"UZI_VERSION_CHECK=0",
				"HOME="+home,
				"XDG_CONFIG_HOME="+home,
			)
			cmd.Stdin = slave
			var stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stderr, &stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			waited := make(chan error, 1)
			go func() { waited <- cmd.Wait() }()
			exited := false
			defer func() {
				if !exited {
					_ = cmd.Process.Kill()
					if err := <-waited; err != nil {
						t.Logf("helper stopped during cleanup: %v", err)
					}
				}
			}()

			deadline := time.NewTimer(15 * time.Second)
			defer deadline.Stop()
			poll := time.NewTicker(time.Millisecond)
			defer poll.Stop()
			for {
				state, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
				if err != nil {
					t.Fatal(err)
				}
				if state.Lflag&unix.ECHO == 0 {
					break
				}
				select {
				case <-poll.C:
				case err := <-waited:
					exited = true
					t.Fatalf("helper exited before prompting: %v\n%s", err, stderr.String())
				case <-deadline.C:
					t.Fatalf("helper did not disable ECHO\n%s", stderr.String())
				}
			}
			const partial = "partial-secret-sentinel"
			if _, err := unix.Write(fd, []byte(partial)); err != nil {
				t.Fatal(err)
			}
			// A canonical-mode queue reports no bytes until a full line arrives
			// (TIOCINQ counts completed lines), so no readiness event exists for
			// a partial line. Give the PTY flip buffer time to reach the line
			// discipline; a late arrival could only make the test pass vacuously,
			// which the mutation run against the unfixed reader rules out.
			time.Sleep(100 * time.Millisecond)
			if err := cmd.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-waited:
				exited = true
				var exitErr *exec.ExitError
				if err != nil && !errors.As(err, &exitErr) {
					t.Fatalf("wait for helper: %v", err)
				}
			case <-deadline.C:
				t.Fatalf("helper did not exit on %v", sig)
			}
			if got, want := cmd.ProcessState.ExitCode(), 128+int(sig); got != want {
				t.Errorf("exit code = %d (%v), want %d", got, cmd.ProcessState, want)
			}
			after, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
			if err != nil {
				t.Fatal(err)
			}
			if *after != *before {
				t.Error("terminal state was not restored after the signal")
			}
			if got := requests.Load(); got != 0 {
				t.Errorf("unlock requests = %d, want 0", got)
			}
			// The next terminal reader must not receive the partial secret:
			// terminate the pending line and read what the slave delivers.
			if _, err := unix.Write(fd, []byte("\n")); err != nil {
				t.Fatal(err)
			}
			if err := unix.SetNonblock(int(slave.Fd()), true); err != nil {
				t.Fatal(err)
			}
			var line []byte
			for !bytes.Contains(line, []byte("\n")) {
				var buf [256]byte
				n, err := unix.Read(int(slave.Fd()), buf[:])
				if n > 0 {
					line = append(line, buf[:n]...)
					continue
				}
				if err != nil && !errors.Is(err, unix.EAGAIN) {
					t.Fatal(err)
				}
				select {
				case <-poll.C:
				case <-deadline.C:
					t.Fatalf("next reader got no line; read %q", line)
				}
			}
			if bytes.Contains(line, []byte(partial)) {
				t.Errorf("queued secret input survived the signal: next reader got %q", line)
			}
		})
	}
}
