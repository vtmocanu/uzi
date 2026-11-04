package main

import (
	"strconv"
	"strings"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// harnessDecisionLines describes only persisted evidence, never current account meters.
func harnessDecisionLines(d *apitypes.HarnessDecisionDTO) []string {
	if d == nil {
		return []string{"not recorded"}
	}
	// Automatic rows always describe Claude as the "preferred" side and Codex as the
	// "other" side, so they are labelled by harness rather than by role.
	pref, other, prefMeasured := "preferred", "other", "pref"
	if strings.HasPrefix(d.Reason, "auto_") {
		pref, other, prefMeasured = "claude", "codex", "claude"
	}
	return []string{
		"preferred: " + cellText(d.Preferred),
		"chosen: " + cellText(d.Chosen),
		"reason: " + cellText(d.Reason),
		pref + " score: " + harnessDecisionNumber(d.PreferredScore),
		other + " score: " + harnessDecisionNumber(d.OtherScore),
		prefMeasured + " measured: " + harnessDecisionNumber(d.PreferredMeasured),
		other + " measured: " + harnessDecisionNumber(d.OtherMeasured),
		pref + " held: " + strconv.FormatBool(d.PreferredHeld),
		other + " held: " + strconv.FormatBool(d.OtherHeld),
		prefMeasured + " reset: " + harnessDecisionTime(d.PreferredWeeklyResetAt),
		other + " reset: " + harnessDecisionTime(d.OtherWeeklyResetAt),
	}
}

// harnessDecisionTime is "MM-DD HH:MMZ" (UTC): the TUI rail is 26 columns wide, and a
// weekly reset is always within the next week, so the year is implied.
func harnessDecisionTime(t *time.Time) string {
	if t == nil {
		return "unknown"
	}
	return t.UTC().Format("01-02 15:04Z")
}

func harnessDecisionNumber(n *float64) string {
	if n == nil {
		return "unknown"
	}
	return strconv.FormatFloat(*n, 'f', -1, 64) + "%"
}
