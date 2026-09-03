package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// resolveUntil turns a `uzi schedule pause-all --until <when>` value into an absolute
// auto-resume instant (PRD #1093 D8). It is a PURE function of its inputs: it never
// reads time.Now() or time.Local, taking `now` and `loc` from the caller instead, so
// its table tests are machine-independent. The command wires the ambient clock and
// zone in at the call site.
//
// Accepted forms (tried in this order):
//   - "never" → indefinite=true, zero time (the "until I resume" pause; the command
//     sends until=null).
//   - RFC3339 (e.g. 2026-09-04T09:00:00Z) → parsed as-is.
//   - a Go duration (24h, 12h30m) → now.Add(d).
//   - "tomorrow" or "tomorrow HH:MM" → the next calendar day at HH:MM in loc (bare
//     tomorrow = 09:00).
//   - a weekday name, case-insensitive, full or three-letter (monday, Mon), optionally
//     " HH:MM" (default 09:00) → the next occurrence STRICTLY after now (today's date
//     when that weekday is today and the time is still ahead, else the next such
//     weekday; a matching weekday whose time already passed today rolls +7 days).
//
// Wall-clock times are built with time.Date(..., loc), so a spring-forward gap (a
// non-existent local time) normalizes FORWARD exactly the way Go's time.Date does, and
// a fall-back overlap resolves to Go's choice — no special-casing here.
func resolveUntil(input string, now time.Time, loc *time.Location) (time.Time, bool, error) {
	s := strings.TrimSpace(input)
	if s == "" {
		return time.Time{}, false, fmt.Errorf("empty value: want an RFC3339 time, a duration (24h), tomorrow[ HH:MM], a weekday[ HH:MM], or never")
	}
	if strings.EqualFold(s, "never") {
		return time.Time{}, true, nil
	}

	// RFC3339 absolute instant, parsed as-is (it carries its own offset).
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, false, nil
	}

	// A Go duration added to now. Tried before the word forms because a duration never
	// collides with "tomorrow"/a weekday name (time.ParseDuration rejects those).
	if d, err := time.ParseDuration(s); err == nil {
		if d <= 0 {
			// Rejected here, not by the server's 422: "-1h" or "0s" is a usage error the
			// CLI can name precisely, and a past instant would only round-trip to fail.
			return time.Time{}, false, fmt.Errorf("--until duration must be positive, got %q", s)
		}
		return now.Add(d), false, nil
	}

	// Word forms: an optional " HH:MM" suffix, default 09:00.
	head, hh, mm, err := splitWordAndTime(s)
	if err != nil {
		return time.Time{}, false, err
	}

	if strings.EqualFold(head, "tomorrow") {
		// Day+1 with time.Date normalizes both the date rollover and any DST gap forward.
		return time.Date(now.Year(), now.Month(), now.Day()+1, hh, mm, 0, 0, loc), false, nil
	}

	if wd, ok := parseWeekday(head); ok {
		base := time.Date(now.Year(), now.Month(), now.Day(), hh, mm, 0, 0, loc)
		daysAhead := (int(wd) - int(base.Weekday()) + 7) % 7
		cand := base.AddDate(0, 0, daysAhead)
		if !cand.After(now) {
			// Today's weekday but the time already passed (or exactly now): roll +7.
			cand = cand.AddDate(0, 0, 7)
		}
		return cand, false, nil
	}

	return time.Time{}, false, fmt.Errorf("unrecognized value %q: want an RFC3339 time, a duration (24h), tomorrow[ HH:MM], a weekday[ HH:MM], or never", input)
}

// splitWordAndTime splits a "word" or "word HH:MM" form into the leading word and the
// resolved hour/minute (defaulting to 09:00 when no time is given). A malformed HH:MM
// is a hard error rather than a silent default.
func splitWordAndTime(s string) (word string, hh, mm int, err error) {
	fields := strings.Fields(s)
	switch len(fields) {
	case 1:
		return fields[0], 9, 0, nil
	case 2:
		h, m, terr := parseHHMM(fields[1])
		if terr != nil {
			return "", 0, 0, terr
		}
		return fields[0], h, m, nil
	default:
		return "", 0, 0, fmt.Errorf("unrecognized value %q: want a single word optionally followed by HH:MM", s)
	}
}

// parseHHMM parses a 24-hour "HH:MM" clock time, rejecting out-of-range values.
func parseHHMM(s string) (int, int, error) {
	parts := strings.Split(s, ":")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid time %q: want HH:MM", s)
	}
	h, err := strconv.Atoi(parts[0])
	if err != nil || h < 0 || h > 23 {
		return 0, 0, fmt.Errorf("invalid hour in %q: want 00-23", s)
	}
	m, err := strconv.Atoi(parts[1])
	if err != nil || m < 0 || m > 59 {
		return 0, 0, fmt.Errorf("invalid minute in %q: want 00-59", s)
	}
	return h, m, nil
}

// weekdayNames maps every accepted weekday token (case-insensitive, full and
// three-letter) to its time.Weekday.
var weekdayNames = map[string]time.Weekday{
	"sunday": time.Sunday, "sun": time.Sunday,
	"monday": time.Monday, "mon": time.Monday,
	"tuesday": time.Tuesday, "tue": time.Tuesday,
	"wednesday": time.Wednesday, "wed": time.Wednesday,
	"thursday": time.Thursday, "thu": time.Thursday,
	"friday": time.Friday, "fri": time.Friday,
	"saturday": time.Saturday, "sat": time.Saturday,
}

// parseWeekday resolves a full or three-letter weekday name (case-insensitive).
func parseWeekday(s string) (time.Weekday, bool) {
	wd, ok := weekdayNames[strings.ToLower(s)]
	return wd, ok
}
