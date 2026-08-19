package main

import tea "charm.land/bubbletea/v2"

// The keymap. Deliberately NOT the PRD's, in one place: the PRD gives `[a]` two
// meanings — admin-toggle on the board and approve in the detail — which puts an
// approval one keystroke from `[x]` cancel-a-live-run. Approve/reject are y/n (M4
// owns the actions; the binding is settled here so it never moves), `a` is the admin
// toggle only, `r` is refresh only.
const (
	keyQuit         = "q"
	keyCtrlC        = "ctrl+c"
	keyEnter        = "enter"
	keyEsc          = "esc"
	keyUp           = "up"
	keyDown         = "down"
	keyLeft         = "left"
	keyRight        = "right"
	keyTab          = "tab"
	keyFilter       = "/"
	keyHelp         = "?"
	keyAdmin        = "a"
	keyHideDone     = "h" // board: hide terminal (completed/failed/cancelled) runs, keeping active + needs-you
	keyRefresh      = "r"
	keyConfirmY     = "y"
	keyConfirmN     = "n"
	keyGoLive       = "g" // M5: re-attach the transcript follow (f is already follow-up)
	keyCollapseCrew = "c" // fold the crew list to a summary so the milestone block is reachable
	keyPageUp       = "pgup"
	keyPageDown     = "pgdown"
	keyHome         = "home"
	keyEnd          = "end"
	keySpaceName    = "space" // v2 names the space key "space", never " "
)

// keyString normalizes a v2 key press to the string form the switches below compare
// against. In v2 the message is KeyPressMsg (not KeyMsg) and its String() already
// folds modifiers in, so this is a single seam rather than scattered field access —
// and it is what lets the model be driven from a test without a terminal.
func keyString(msg tea.KeyPressMsg) string {
	return msg.String()
}

// isMotionKey maps both the vi keys and the arrows onto a direction, matching the
// prior art rather than inventing a scheme. Returns 0 when the key is not motion.
func motionDelta(k string) int {
	switch k {
	case "j", keyDown:
		return 1
	case "k", keyUp:
		return -1
	case keyPageDown:
		return 10
	case keyPageUp:
		return -10
	}
	return 0
}

// helpLines is the `?` overlay content. It lists what M3 actually binds; M4 adds the
// mutation keys to the same list rather than a second one.
func helpLines(inDetail bool) []string {
	common := []string{
		"j / ↓      down",
		"k / ↑      up",
		"enter      open",
		"esc        back / dismiss",
		"/          filter",
		"r          refresh",
		"?          this help",
		"q          quit immediately (ctrl+c asks to confirm; twice quits at once)",
	}
	if inDetail {
		return append([]string{
			"← / →      focus the crew rail / the transcript",
			"tab        cycle the focused pane",
			"↑ / ↓      move within the focused pane (agents · scroll)",
			"g          follow live: re-attach and jump to newest (live runs)",
			"c          collapse the crew list (keeps the milestone block in view)",
		}, common...)
	}
	return append([]string{
		"a          toggle the factory-wide admin board (needs a uza_ token)",
		"h          hide finished runs (completed/failed/cancelled); keeps active + needs-you",
	}, common...)
}
