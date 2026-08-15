// Command demo boots the PROPOSED "factory shift board" redesign of the uzi TUI in a real
// bubbletea event loop, over seeded fixtures — no API, no DB, no auth, no network. It runs
// in your terminal's own theme (light or dark).
//
//	cd api/cmd/uzi/uxlab && go run ./demo      # or: devbox run demo
//
// It is DEMO-ONLY: the shipped TUI (api/cmd/uzi/tui_*.go) is unchanged. The redesign lives
// in ../factoryui and is the reference for a later port into the real views.
package main

import (
	"fmt"
	"os"

	tea "charm.land/bubbletea/v2"

	"gitlab.example.com/vtmocanu/uzi/api/cmd/uzi/uxlab/factoryui"
)

func main() {
	if !isatty(os.Stdout) {
		fmt.Fprintln(os.Stderr, "uzi demo: needs an interactive terminal (stdout is not a TTY).")
		os.Exit(1)
	}
	// AltScreen is set on the View, not as a program option, in bubbletea v2.
	p := tea.NewProgram(factoryui.New())
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "uzi demo:", err)
		os.Exit(1)
	}
}

// isatty reports whether f is a character device (a terminal), with no extra dependency.
func isatty(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
