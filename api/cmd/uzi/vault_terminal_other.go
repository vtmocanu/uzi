//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris && !windows

package main

import "errors"

// Refuse hidden input where echo and pending input cannot be controlled.
// x/term's Plan 9 fallback does not disable echo; z/OS has no input-flush wrapper.
func readVaultHiddenPassword(_ uintptr) ([]byte, error) {
	return nil, errors.New("hidden terminal password input is unsupported on this platform")
}
