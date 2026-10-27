//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package main

import "golang.org/x/sys/unix"

func flushVaultTerminalInput(fd uintptr) error {
	// x/sys does not export FREAD; the BSD TIOCFLUSH input selector is 1.
	const fread = 1
	return unix.IoctlSetPointerInt(int(fd), unix.TIOCFLUSH, fread)
}
