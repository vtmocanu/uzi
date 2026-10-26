//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris || zos

package main

import (
	"github.com/charmbracelet/x/term"
	"golang.org/x/sys/unix"
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
	if err := term.SetState(fd, &hidden); err != nil {
		return nil, err
	}
	defer func() {
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
