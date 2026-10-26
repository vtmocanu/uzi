package main

import (
	"os"

	"github.com/charmbracelet/x/term"
	"golang.org/x/sys/windows"
)

func readVaultHiddenPassword(fd uintptr) (password []byte, err error) {
	state, err := term.GetState(fd)
	if err != nil {
		return nil, err
	}
	hidden := *state
	hidden.Mode &^= windows.ENABLE_ECHO_INPUT | windows.ENABLE_LINE_INPUT
	hidden.Mode |= windows.ENABLE_PROCESSED_OUTPUT | windows.ENABLE_PROCESSED_INPUT
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

	var handle windows.Handle
	process, _ := windows.GetCurrentProcess()
	if err := windows.DuplicateHandle(process, windows.Handle(fd), process, &handle, 0, false, windows.DUPLICATE_SAME_ACCESS); err != nil {
		return nil, err
	}
	input := os.NewFile(uintptr(handle), "stdin")
	defer func() {
		if closeErr := input.Close(); closeErr != nil && err == nil {
			clear(password)
			password = nil
			err = closeErr
		}
	}()
	return readVaultPasswordLine(input)
}
