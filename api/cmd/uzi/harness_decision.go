package main

import (
	"strconv"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// harnessDecisionLines describes only persisted evidence, never current account meters.
func harnessDecisionLines(d *apitypes.HarnessDecisionDTO) []string {
	if d == nil {
		return []string{"not recorded"}
	}
	return []string{
		"preferred: " + cellText(d.Preferred),
		"chosen: " + cellText(d.Chosen),
		"reason: " + cellText(d.Reason),
		"preferred score: " + harnessDecisionNumber(d.PreferredScore),
		"other score: " + harnessDecisionNumber(d.OtherScore),
		"pref measured: " + harnessDecisionNumber(d.PreferredMeasured),
		"other measured: " + harnessDecisionNumber(d.OtherMeasured),
		"preferred held: " + strconv.FormatBool(d.PreferredHeld),
		"other held: " + strconv.FormatBool(d.OtherHeld),
	}
}

func harnessDecisionNumber(n *float64) string {
	if n == nil {
		return "unknown"
	}
	return strconv.FormatFloat(*n, 'f', -1, 64) + "%"
}
