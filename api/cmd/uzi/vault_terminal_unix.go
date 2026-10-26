//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package main

import (
	"errors"
	"os"
	"os/signal"
	"syscall"

	"github.com/charmbracelet/x/term"
	"golang.org/x/sys/unix"
)

// Interrupt hooks for the hidden prompt; tests replace them to observe an
// interruption without delivering a real signal or exiting.
var (
	vaultNotifyInterrupt = func(c chan<- os.Signal) { signal.Notify(c, os.Interrupt, syscall.SIGTERM) }
	vaultStopInterrupt   = func(c chan<- os.Signal) { signal.Stop(c) }
	vaultInterruptExit   = func(sig os.Signal) {
		code := 130 // 128 + SIGINT, as the default action would report
		if sig == syscall.SIGTERM {
			code = 143
		}
		os.Exit(code)
	}
)

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
	// Catch SIGINT/SIGTERM for the prompt's lifetime: the default action would
	// exit before the deferred restore below, leaving the terminal without echo.
	// ISIG makes the driver flush queued input on INTR, so nothing is echoed back.
	interrupts := make(chan os.Signal, 1)
	vaultNotifyInterrupt(interrupts)
	stopped := make(chan struct{})
	go func() {
		select {
		case sig := <-interrupts:
			_ = term.Restore(fd, state)
			vaultInterruptExit(sig)
		case <-stopped:
		}
	}()
	if err := term.SetState(fd, &hidden); err != nil {
		vaultStopInterrupt(interrupts)
		close(stopped)
		return nil, err
	}
	defer func() {
		if errors.Is(err, errVaultPasswordTooLong) {
			clear(password)
			password = nil
			if flushErr := flushVaultTerminalInput(fd); flushErr != nil {
				// Keep echo disabled when queued secret input could not be discarded.
				err = flushErr
				return
			}
		}
		if restoreErr := term.Restore(fd, state); restoreErr != nil {
			clear(password)
			password = nil
			if err == nil {
				err = restoreErr
			}
		}
	}()
	defer func() {
		vaultStopInterrupt(interrupts)
		close(stopped)
	}()
	return readVaultPasswordLine(vaultPasswordFD(fd))
}
