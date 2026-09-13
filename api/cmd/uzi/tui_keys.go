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
	// Forge-view navigation (PRD #1255 D1). tab cycles the list screens (floor → pulls →
	// ci → floor; ci lands in M4b, so for now it cycles floor ↔ pulls); 1/2/3 jump directly;
	// R cycles the scoped repo. R is shift+r (distinct from r = refresh) so it does not
	// collide with the board/list refresh key.
	keyViewFloor = "1"
	keyViewPulls = "2"
	keyViewCI    = "3"
	keyRepoCycle = "R"
	// Forge actions (PRD #1255 M5, D1/D12), bound on BOTH the pulls list row and the PR drill-in:
	// u opens the PR's linked uzi run in the run view, w reworks that run, f queues a CI-fix run
	// for the PR's head branch. keyPRView (m) is the run view → PR view cross-link.
	keyRunLink = "u"
	keyRework  = "w"
	keyFixCI   = "f"
	keyPRView  = "m"
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

// helpLines is the `?` overlay content, per the focused screen. It lists what each
// milestone actually binds (D13: a key appears in the legend of the milestone that binds
// it), so the pulls screen shows its own navigation legend rather than the board's.
func helpLines(v tuiView) []string {
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
	switch v {
	case viewDetail:
		return append([]string{
			"← / →      focus the crew rail / the transcript",
			"tab        cycle the focused pane",
			"↑ / ↓      move within the focused pane (agents · scroll)",
			"g          follow live: re-attach and jump to newest (live runs)",
			"c          collapse the crew list (keeps the milestone block in view)",
			"m          open the PR view for this run's merge request (when it has one)",
		}, common...)
	case viewPulls:
		return append([]string{
			"enter / →  open the selected PR (checks · reviews · merge)",
			"u          open the PR's linked uzi run (when one exists)",
			"w          rework the linked run",
			"f          fix ci: queue a CI-fix run for the PR's branch",
			"tab        switch screen (floor · pulls · ci)",
			"1 / 2 / 3  jump to the floor / pulls / ci",
			"R          cycle the scoped repo (when several are enabled)",
		}, common...)
	case viewCI:
		return append([]string{
			"enter / →  open the selected run's jobs (jobs · steps)",
			"tab        switch screen (floor · pulls · ci)",
			"1 / 2 / 3  jump to the floor / pulls / ci",
			"R          cycle the scoped repo (when several are enabled)",
		}, common...)
	case viewPR:
		return append([]string{
			"↑ / ↓      move the cursor over the checks",
			"↗          the selected check's URL is a clickable link",
			"u          open the PR's linked uzi run (when one exists)",
			"w          rework the linked run (fix review findings)",
			"f          fix ci: queue a CI-fix run for the PR's branch",
		}, common...)
	case viewCIRun:
		return append([]string{
			"↑ / ↓      move the cursor over the jobs (the selected job expands its steps)",
			"↗          the selected job's URL is a clickable link",
			"f          fix ci: queue a CI-fix run for this run's branch",
		}, common...)
	default:
		return append([]string{
			"a          toggle the factory-wide admin board (needs a uza_ token)",
			"h          hide finished runs (completed/failed/cancelled); keeps active + needs-you",
			"tab        switch screen (floor · pulls · ci)",
		}, common...)
	}
}
