package main

import "strconv"

// issueRef renders an issue reference: the issue tracker key ("ABC-12") on a repo linked
// to an issue tracker, else "#<iid>". The key is server/issue-tracker-authored text, so it
// goes through cellText (control-byte strip, fold, cap) before it reaches a terminal.
// Fire candidates in schedule_render.go deliberately keep "#<iid>" (PRD #4173 M6).
func issueRef(iid int64, key string) string {
	if k := cellText(key); k != "" {
		return k
	}
	return "#" + strconv.FormatInt(iid, 10)
}

// tuiIssueKeyCells caps an issue tracker key drawn in a TUI cell (through m.renderer.Plain,
// the D7 rule for untrusted cells): real keys ("ABC-12") are
// short, and the cap keeps a hostile 200-rune key from blowing out a row or the crumb.
// The cap counts runes, not columns: a wide rune takes two columns.
const tuiIssueKeyCells = 24
