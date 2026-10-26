//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package main

import (
	"errors"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/charmbracelet/x/term"
	"golang.org/x/sys/unix"
)

// vaultInterruptExitCode reports an interrupted prompt as the default signal
// action would: 128 plus the signal number.
func vaultInterruptExitCode(sig os.Signal) int {
	if sig == syscall.SIGTERM {
		return 128 + int(syscall.SIGTERM)
	}
	return 128 + int(syscall.SIGINT)
}

type vaultPasswordFD uintptr

func (fd vaultPasswordFD) Read(p []byte) (int, error) {
	return unix.Read(int(fd), p)
}

func readVaultHiddenPassword(fd uintptr) (password []byte, err error) {
	state, err := term.GetState(fd)
	if err != nil {
		return nil, err
	}
	hidden := *state
	// Match x/term v0.2.2 ReadPassword without changing editing or signals.
	hidden.Lflag &^= unix.ECHO
	hidden.Lflag |= unix.ICANON | unix.ISIG
	hidden.Iflag |= unix.ICRNL
	// Catch SIGINT/SIGTERM from before the terminal is hidden until it is
	// restored: the default action would exit with echo still off. mu
	// serializes every terminal change with the interrupt path, so an interrupt
	// can neither precede a later hide nor fall between restore and unregister.
	// A signal sent with kill does not flush the input queue the way a typed
	// INTR does, so the interrupt path discards queued secret input itself and,
	// like the overflow path, keeps echo off when that fails.
	var mu sync.Mutex
	hiddenApplied := false
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt, syscall.SIGTERM)
	stopped := make(chan struct{})
	go func() {
		select {
		case sig := <-interrupts:
			mu.Lock() // held through exit: no later terminal change can run
			if hiddenApplied && flushVaultTerminalInput(fd) == nil {
				_ = term.Restore(fd, state)
			}
			os.Exit(vaultInterruptExitCode(sig))
		case <-stopped:
		}
	}()
	unregister := func() {
		signal.Stop(interrupts)
		close(stopped)
	}
	mu.Lock()
	err = term.SetState(fd, &hidden)
	hiddenApplied = err == nil
	mu.Unlock()
	if err != nil {
		unregister()
		return nil, err
	}
	defer unregister() // runs after the restore below: protection outlives it
	defer func() {
		mu.Lock()
		defer mu.Unlock()
		if errors.Is(err, errVaultPasswordTooLong) {
			clear(password)
			password = nil
			if flushErr := flushVaultTerminalInput(fd); flushErr != nil {
				// Keep echo disabled when queued secret input could not be discarded.
				err = flushErr
				return
			}
		}
		hiddenApplied = false
		if restoreErr := term.Restore(fd, state); restoreErr != nil {
			clear(password)
			password = nil
			if err == nil {
				err = restoreErr
			}
		}
	}()
	return readVaultPasswordLine(vaultPasswordFD(fd))
}
