//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris && !zos && !windows

package main

import "github.com/charmbracelet/x/term"

// Plan 9's library reader uses a fixed 128-byte buffer; other platforms return
// the library's unsupported-platform error.
func readVaultHiddenPassword(fd uintptr) ([]byte, error) {
	return term.ReadPassword(fd)
}
