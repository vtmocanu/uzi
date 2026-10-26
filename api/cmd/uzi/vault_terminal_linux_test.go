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

// An interrupt during the hidden prompt restores the terminal before exiting
// and submits no password; the default signal action would skip the restore.
func TestVaultTerminalPasswordInterruptPTY(t *testing.T) {
	fd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	master := os.NewFile(uintptr(fd), "vault-interrupt-master")
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

	notified := make(chan chan<- os.Signal, 1)
	exited := make(chan *unix.Termios, 1)
	oldNotify, oldStop, oldExit := vaultNotifyInterrupt, vaultStopInterrupt, vaultInterruptExit
	vaultNotifyInterrupt = func(c chan<- os.Signal) { notified <- c }
	vaultStopInterrupt = func(chan<- os.Signal) {}
	vaultInterruptExit = func(os.Signal) {
		state, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
		if err != nil {
			t.Errorf("termios at exit: %v", err)
		}
		exited <- state
	}
	t.Cleanup(func() { vaultNotifyInterrupt, vaultStopInterrupt, vaultInterruptExit = oldNotify, oldStop, oldExit })

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
	var interrupts chan<- os.Signal
	select {
	case interrupts = <-notified:
	case <-deadline.C:
		t.Fatal("reader did not register for interrupts")
	}
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
	interrupts <- os.Interrupt
	select {
	case state := <-exited:
		if state == nil || *state != *before {
			t.Fatal("terminal state was not restored before the interrupt exit")
		}
	case <-deadline.C:
		t.Fatal("interrupt did not exit the prompt")
	}
	// Release the reader the stubbed exit left blocked; the prompt must still
	// not have produced a password before this test input.
	select {
	case got := <-done:
		t.Fatalf("reader returned before input: err=%v", got.err)
	default:
	}
	if _, err := unix.Write(fd, []byte("\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		clear(got.password)
	case <-deadline.C:
		t.Fatal("reader did not return after release")
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
// prompt waits must restore the full terminal state, exit with 128+signal and
// send no unlock request. Default signal termination would skip the restore.
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
			cmd := exec.Command(os.Args[0], "-test.run=^TestVaultUnlockSignalHelper$")
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
			defer func() {
				if cmd.ProcessState == nil {
					_ = cmd.Process.Kill()
					<-waited
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
					t.Fatalf("helper exited before prompting: %v\n%s", err, stderr.String())
				case <-deadline.C:
					t.Fatalf("helper did not disable ECHO\n%s", stderr.String())
				}
			}
			if err := cmd.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			select {
			case <-waited:
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
		})
	}
}
