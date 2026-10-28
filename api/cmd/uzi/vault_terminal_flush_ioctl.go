//go:build aix || linux || solaris

package main

import "golang.org/x/sys/unix"

func flushVaultTerminalInput(fd uintptr) error {
	return unix.IoctlSetInt(int(fd), unix.TCFLSH, unix.TCIFLUSH)
}
