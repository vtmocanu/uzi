package main

import (
	"strconv"
	"strings"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// weeklyLabelCap bounds an owner-authored credential label in a summary cell. The FIX command cell
// is never bounded: a truncated command is one that names another credential.
const weeklyLabelCap = 60

// weeklyFixEditReserve is the server's edit_reserve fix kind (apitypes.WeeklyLimitFixDTO.Kind). It
// carries no command; the CLI offers the Settings route and `uzi settings reserve` for it.
const weeklyFixEditReserve = "edit_reserve"

const weeklyEditReserveText = "raise or turn off the weekly reserve in Settings → Run defaults " +
	"(or `uzi settings reserve --threshold <1-100|off>`)"

// weeklyLimitCell is the WEEKLY_LIMIT row (PRD #4123): the credential, its weekly use against the
// effective limit, the reset time, the server-built reason and whether the run waits. Empty for a
// run without the block (emit-only-when-set). Labels are owner-authored, so they go through cellText
// and a bound; the reason and wait text are fixed prose.
func weeklyLimitCell(r apitypes.RunDTO) string {
	w := r.WeeklyLimit
	if w == nil {
		return ""
	}
	label := capCell(cellText(w.CredentialLabel), weeklyLabelCap)
	var b strings.Builder
	b.WriteString(label)
	if w.Pct != nil {
		b.WriteString(" at " + strconv.FormatFloat(*w.Pct, 'f', -1, 64) + "% of the " +
			strconv.FormatFloat(w.EffectiveLimit, 'f', -1, 64) + "% weekly limit")
	} else {
		b.WriteString(" weekly usage unknown (limit " + strconv.FormatFloat(w.EffectiveLimit, 'f', -1, 64) + "%)")
	}
	if w.ResetsAt != nil {
		b.WriteString(", resets " + w.ResetsAt.UTC().Format("2006-01-02T15:04:05Z"))
	}
	if reason := weeklyReasonText(w); reason != "" {
		b.WriteString("; " + reason)
	}
	if wait := weeklyWaitText(w.Wait, weeklyLimitParked(r)); wait != "" {
		b.WriteString("; " + wait)
	}
	return b.String()
}

// weeklyReasonText is ReasonText with its one label-bearing arm (the alternative's label) folded
// and bounded, so the server sentence cannot widen a cell or smuggle a control.
func weeklyReasonText(w *apitypes.WeeklyLimitDTO) string {
	c := *w
	c.AlternativeLabel = capCell(cellText(c.AlternativeLabel), weeklyLabelCap)
	c.CredentialLabel = capCell(cellText(c.CredentialLabel), weeklyLabelCap)
	return c.ReasonText()
}

// weeklyLimitParked mirrors the web's weeklyLimitParked: the run is parked on its weekly limit
// (limit_wait on the seven_day window) or held by the weekly reserve. Any other run with a block
// is a live or queued run near its limit, so what happens at the limit is conditional.
func weeklyLimitParked(r apitypes.RunDTO) bool {
	switch r.Status {
	case "limit_wait":
		return r.RateLimitType != nil && *r.RateLimitType == "seven_day"
	case "paused":
		return r.HoldReason != nil && *r.HoldReason == "weekly_reserve"
	}
	return false
}

// weeklyWaitText says what the run does at (parked) or if it reaches (not parked) the limit. A run
// that is not parked is never described as paused or waiting.
func weeklyWaitText(wait string, parked bool) string {
	if parked {
		switch wait {
		case "waits":
			return "the run waits for the reset"
		case "fails":
			return "the run fails instead of waiting"
		case "reserve_pause":
			return "the run stays paused until usage drops below the reserve or you resume it"
		case "unknown":
			return "whether the run waits is not known"
		}
		return ""
	}
	switch wait {
	case "waits":
		return "if it reaches the limit, it waits for the reset"
	case "fails":
		return "if it reaches the limit, it fails"
	case "reserve_pause":
		return "if usage reaches the reserve, the run pauses until usage drops below it or you resume it"
	case "unknown":
		return "whether the run waits at the limit is not known"
	}
	return ""
}

// weeklyLimitFixRows are the FIX rows: one per run-level fix that has a command, then the same for
// future-run advice, marked as such. A fix whose label is not command-safe says how to get a
// copyable command instead. The command cell is sanitized but never truncated.
func weeklyLimitFixRows(w *apitypes.WeeklyLimitDTO) [][]string {
	if w == nil {
		return nil
	}
	var rows [][]string
	add := func(key string, fixes []apitypes.WeeklyLimitFixDTO) {
		for _, f := range fixes {
			switch {
			case f.Kind == weeklyFixEditReserve:
				rows = append(rows, []string{key, weeklyEditReserveText})
			case f.Command != "":
				rows = append(rows, []string{key, uzicli.CellText(f.Command)})
			case f.CommandUnavailable != "":
				rows = append(rows, []string{key, capCell(cellText(f.TargetLabel), weeklyLabelCap) +
					": rename this token in Settings to get a copyable command"})
			}
		}
	}
	add("FIX", w.Fix)
	add("FIX_FUTURE_RUNS", w.FutureRuns)
	return rows
}

// weeklyLimitTUILine is the one run-detail line: credential, use against the limit and the reason,
// pointing at `uzi run get` for the fix rather than drawing a command a width clamp could cut.
func weeklyLimitTUILine(r apitypes.RunDTO) string {
	w := r.WeeklyLimit
	if w == nil {
		return ""
	}
	line := "weekly limit: " + weeklyLimitCell(r)
	// Only when `uzi run get` would print at least one run-level FIX row (future-run advice does not count).
	if len(weeklyLimitFixRows(&apitypes.WeeklyLimitDTO{Fix: w.Fix})) > 0 {
		line += " (fix: uzi run get " + cellText(r.ID) + ")"
	}
	return line
}
