package main

import (
	"strconv"
	"strings"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// harnessDecisionLines is the human CLI form (uzi run get): it describes only persisted
// evidence, never current account meters, and renders resets as RFC3339 UTC.
func harnessDecisionLines(d *apitypes.HarnessDecisionDTO) []string {
	return harnessDecisionLinesWith(d, harnessDecisionTime)
}

// railHarnessDecisionLines is the TUI rail form: identical rows, compact reset times.
func railHarnessDecisionLines(d *apitypes.HarnessDecisionDTO) []string {
	return harnessDecisionLinesWith(d, harnessDecisionRailTime)
}

func harnessDecisionLinesWith(d *apitypes.HarnessDecisionDTO, fmtTime func(*time.Time) string) []string {
	if d == nil {
		return []string{"not recorded"}
	}
	// off and excluded decisions return before any usage is read, so every reading
	// would print as unknown/false; show what happened instead. Each note fits the
	// 26-column TUI rail. --json still carries every field.
	if note, ok := harnessDecisionUnmeasured[d.Reason]; ok {
		return []string{
			"preferred: " + cellText(d.Preferred),
			"chosen: " + cellText(d.Chosen),
			"reason: " + cellText(d.Reason),
			note,
		}
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
		prefMeasured + " reset: " + fmtTime(d.PreferredWeeklyResetAt),
		other + " reset: " + fmtTime(d.OtherWeeklyResetAt),
	}
}

// harnessDecisionUnmeasured names the reasons recorded before any usage reading.
var harnessDecisionUnmeasured = map[string]string{
	"off":      "overflow off in settings",
	"excluded": "overflow not considered",
}

// harnessDecisionTime is the CLI reset form: RFC3339 in UTC, or "unknown".
func harnessDecisionTime(t *time.Time) string {
	if t == nil {
		return "unknown"
	}
	return t.UTC().Format(time.RFC3339)
}

// harnessDecisionRailTime is "MM-DD HH:MMZ" (UTC). The TUI rail is only 26 columns wide and a
// full RFC3339 stamp after "claude reset: " would be clipped. The year is omitted: a recorded
// weekly reset is a future reset at the run's creation, so it falls within about a week of the
// run's creation time, which dates it.
func harnessDecisionRailTime(t *time.Time) string {
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
