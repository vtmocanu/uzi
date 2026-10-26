package main

import (
	"errors"
	"io"
	"runtime"
)

var errVaultPasswordTooLong = errors.New("vault password input is too long")

// readVaultPasswordLine preserves x/term's byte editing and line endings.
// Every read consumes the budget, including edits, ignored bytes and empty reads.
// A blocked read still waits for terminal input; no background reader is started.
func readVaultPasswordLine(reader io.Reader) (password []byte, err error) {
	storage := make([]byte, maxVaultPasswordBytes)
	defer func() {
		if err != nil {
			clear(storage)
			password = nil
		}
	}()
	var buf [1]byte
	defer clear(buf[:])
	length := 0
	for attempts := 0; attempts <= maxVaultPasswordBytes; attempts++ {
		n, readErr := reader.Read(buf[:])
		if n > 0 {
			// Check termination before the budget so an exact-limit line fits.
			if (buf[0] == '\n' && runtime.GOOS != "windows") ||
				(buf[0] == '\r' && runtime.GOOS == "windows") {
				return storage[:length], nil
			}
			switch buf[0] {
			case '\b':
				if length > 0 {
					length--
					storage[length] = 0
				}
			case '\r', '\n':
				// Ignore the other platform's line-ending byte.
			default:
				if length == len(storage) {
					return nil, errVaultPasswordTooLong
				}
				storage[length] = buf[0]
				length++
			}
			continue
		}
		if readErr != nil {
			if readErr == io.EOF && length > 0 {
				return storage[:length], nil
			}
			return nil, readErr
		}
	}
	return nil, errVaultPasswordTooLong
}
