// Package schedsvc is the pure, DB-free cron / next-fire engine behind PRD #241
// (scheduled runs). It knows how to validate a cron string, compute the next
// fire instant(s) for a recurring schedule in a named IANA timezone
// (DST-correct), and normalize a one-time run_at — plus the preset<->cron
// translation shared by web, CLI, and api so all three agree byte-for-byte.
//
// Scope discipline (PRD #241 Decision 3): this package uses ONLY the
// parser/schedule types of github.com/robfig/cron/v3 (cron.ParseStandard,
// Schedule.Next) — never its in-process cron.Cron runner. The durable
// next_fire_at column is the real runner (Decision 1); the library is just a
// spec parser and a next-instant calculator. Storage is always the raw cron
// string + IANA timezone, never a preset label (Decision 6), so there is one
// canonical form to parse and to compute next_fire_at from.
//
// Timezone / DST mechanism (Decision 3, review N3): a cron string is
// interpreted in its schedule's IANA timezone. We build the probe time in that
// location (time.LoadLocation + time.Time.In) before calling Schedule.Next, so
// robfig computes wall-clock advances in the target zone; the result is then
// persisted/returned as UTC. (The alternative — robfig's CRON_TZ= spec prefix —
// is deliberately NOT used; we keep the stored cron string free of that prefix
// so it round-trips cleanly through the presets and the CLI's --cron flag.)
package schedsvc

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

// ErrNoFire is returned when a schedule has no next fire within robfig's search
// horizon (e.g. an impossible day-of-month/month combination). It is distinct
// from a parse error so callers can tell "bad cron" from "valid but never
// fires".
var ErrNoFire = errors.New("schedsvc: cron expression has no next fire")

// ValidateCron reports whether expr is a valid 5-field standard cron string
// (minute hour day-of-month month day-of-week). It rejects 6-field
// (seconds-prefixed) expressions and robfig's @-descriptors, because the stored
// form for a recurring schedule is always plain 5-field cron (Decision 6).
func ValidateCron(expr string) error {
	if _, err := parseStandard(expr); err != nil {
		return err
	}
	return nil
}

// parseStandard wraps cron.ParseStandard so every entry point rejects the same
// set of expressions. cron.ParseStandard accepts the 5-field standard form (no
// seconds field) AND robfig's @every/@daily descriptors; we require exactly 5
// whitespace-separated fields up front so the descriptors are rejected and the
// raw stored form is always a plain cron string the presets can round-trip
// (Decision 6). This field-count check is the mechanism — it is verified by
// TestValidateCron's "@every 1h" case.
func parseStandard(expr string) (cron.Schedule, error) {
	if n := len(strings.Fields(expr)); n != 5 {
		return nil, fmt.Errorf("schedsvc: invalid cron %q: want 5 fields, got %d", expr, n)
	}
	sched, err := cron.ParseStandard(expr)
	if err != nil {
		return nil, fmt.Errorf("schedsvc: invalid cron %q: %w", expr, err)
	}
	return sched, nil
}

// NextFire computes the first fire STRICTLY after `after`, interpreting
// cronExpr in the IANA timezone tz, and returns it in UTC. DST is handled by
// robfig via the wall-clock advance in tz (see the package doc); a wall-clock
// time that does not exist on a spring-forward day resolves to the next valid
// instant, and one that occurs twice on a fall-back day fires once.
func NextFire(cronExpr, tz string, after time.Time) (time.Time, error) {
	sched, loc, err := parseInLocation(cronExpr, tz)
	if err != nil {
		return time.Time{}, err
	}
	next := sched.Next(after.In(loc))
	if next.IsZero() {
		return time.Time{}, ErrNoFire
	}
	return next.UTC(), nil
}

// NextFires returns the next n fires strictly after `after`, each in UTC, for
// the modal / CLI "next fires" preview (Decision 6). The slice is strictly
// increasing. It stops early (returning fewer than n) if the schedule has no
// further fires within robfig's horizon.
func NextFires(cronExpr, tz string, after time.Time, n int) ([]time.Time, error) {
	if n <= 0 {
		return nil, fmt.Errorf("schedsvc: NextFires n must be positive, got %d", n)
	}
	sched, loc, err := parseInLocation(cronExpr, tz)
	if err != nil {
		return nil, err
	}
	out := make([]time.Time, 0, n)
	probe := after.In(loc)
	for i := 0; i < n; i++ {
		next := sched.Next(probe)
		if next.IsZero() {
			break
		}
		out = append(out, next.UTC())
		probe = next
	}
	return out, nil
}

// parseInLocation parses the cron string and loads the timezone together so the
// three next-fire entry points share one validation path.
func parseInLocation(cronExpr, tz string) (cron.Schedule, *time.Location, error) {
	sched, err := parseStandard(cronExpr)
	if err != nil {
		return nil, nil, err
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, nil, fmt.Errorf("schedsvc: invalid timezone %q: %w", tz, err)
	}
	return sched, loc, nil
}

// OnceFire validates and normalizes a one-time run_at (Decision 5's soft-terminal
// `once` schedules). A run_at is an absolute instant, so a timezone changes only
// its display, not the instant; this canonicalizes to UTC and confirms the fire
// is strictly after `after` (pass the current time). Recurring schedules use
// NextFire instead. The future-check lives here rather than in the caller so
// web, CLI, and api reject a past run_at identically.
func OnceFire(runAt, after time.Time) (time.Time, error) {
	fire := runAt.UTC()
	if !fire.After(after.UTC()) {
		return time.Time{}, fmt.Errorf("schedsvc: run_at %s is not after %s", fire, after.UTC())
	}
	return fire, nil
}
