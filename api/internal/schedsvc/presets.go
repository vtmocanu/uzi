package schedsvc

import (
	"fmt"
	"strconv"
	"strings"
)

// Preset identifiers (Decision 6). These are the UI-friendly cadences the modal
// offers on top of the raw cron field. Storage is ALWAYS the cron string + tz,
// never one of these labels — these constants exist only so web, CLI, and api
// name the same preset when translating to/from cron. The set matches the
// approved mock exactly: Weekdays / Every day / Every week / Every N hours /
// Custom.
const (
	// PresetWeekdays fires Monday–Friday at a chosen hour:minute → "M H * * 1-5".
	PresetWeekdays = "weekdays"
	// PresetDaily fires every day at a chosen hour:minute → "M H * * *".
	PresetDaily = "daily"
	// PresetWeekly fires every Monday at a chosen hour:minute → "M H * * 1".
	PresetWeekly = "weekly"
	// PresetEveryNHours fires every N hours on the hour → "0 */N * * *".
	// Its parameter is the interval N, not an hour:minute, so it is produced by
	// EveryNHoursCron rather than PresetToCron.
	PresetEveryNHours = "every_n_hours"
	// PresetCustom is the sentinel for a raw cron string that matches no preset;
	// the modal flips its dropdown to "Custom" and shows the Advanced field.
	PresetCustom = "custom"
)

// PresetToCron renders the cron string for a time-of-day preset (weekdays /
// daily / weekly) at hour:minute in 24h time (Decision 6). It deliberately does
// NOT handle PresetEveryNHours — that cadence has an interval, not a time — use
// EveryNHoursCron for it; and PresetCustom has no canonical cron by definition.
func PresetToCron(preset string, hour, minute int) (string, error) {
	if err := validClock(hour, minute); err != nil {
		return "", err
	}
	switch preset {
	case PresetWeekdays:
		return fmt.Sprintf("%d %d * * 1-5", minute, hour), nil
	case PresetDaily:
		return fmt.Sprintf("%d %d * * *", minute, hour), nil
	case PresetWeekly:
		return fmt.Sprintf("%d %d * * 1", minute, hour), nil
	case PresetEveryNHours:
		return "", fmt.Errorf("schedsvc: preset %q takes an interval; use EveryNHoursCron", preset)
	case PresetCustom:
		return "", fmt.Errorf("schedsvc: preset %q has no canonical cron", preset)
	default:
		return "", fmt.Errorf("schedsvc: unknown preset %q", preset)
	}
}

// EveryNHoursCron renders the "every N hours" preset → "0 */N * * *" (Decision
// 6). N must be 1..23: a cron hour step is only meaningful within the 0–23 hour
// range, and "*/24" would never fire.
func EveryNHoursCron(n int) (string, error) {
	if n < 1 || n > 23 {
		return "", fmt.Errorf("schedsvc: every_n_hours interval must be 1..23, got %d", n)
	}
	return fmt.Sprintf("0 */%d * * *", n), nil
}

// CronToPreset recognizes a cron string that a preset produced and reports which
// preset plus its parameters (Decision 6). This drives the modal's "flip to
// Custom when the raw field diverges" behavior: any cron string not produced by
// a preset returns ok=false with preset=PresetCustom.
//
// Return encoding:
//   - weekdays / daily / weekly → (preset, hour, minute, true)
//   - every_n_hours → (PresetEveryNHours, N, 0, true): the interval N is carried
//     in the `hour` return value (minute is always 0 for this cadence). Callers
//     that need N should read `hour` when preset==PresetEveryNHours.
//   - anything else → (PresetCustom, 0, 0, false)
func CronToPreset(cronExpr string) (preset string, hour, minute int, ok bool) {
	fields := strings.Fields(cronExpr)
	if len(fields) != 5 {
		return PresetCustom, 0, 0, false
	}
	min, hr, dom, mon, dow := fields[0], fields[1], fields[2], fields[3], fields[4]

	// every_n_hours: "0 */N * * *" (checked first — its hour field is a step,
	// not a number, so the numeric-hour presets below never match it).
	if min == "0" && dom == "*" && mon == "*" && dow == "*" && strings.HasPrefix(hr, "*/") {
		if n, err := strconv.Atoi(strings.TrimPrefix(hr, "*/")); err == nil && n >= 1 && n <= 23 {
			return PresetEveryNHours, n, 0, true
		}
		return PresetCustom, 0, 0, false
	}

	// The remaining presets share "M H * * <dow>" with numeric M and H.
	mi, err1 := strconv.Atoi(min)
	hi, err2 := strconv.Atoi(hr)
	if err1 != nil || err2 != nil || dom != "*" || mon != "*" {
		return PresetCustom, 0, 0, false
	}
	if validClock(hi, mi) != nil {
		return PresetCustom, 0, 0, false
	}
	switch dow {
	case "*":
		return PresetDaily, hi, mi, true
	case "1-5":
		return PresetWeekdays, hi, mi, true
	case "1":
		return PresetWeekly, hi, mi, true
	default:
		return PresetCustom, 0, 0, false
	}
}

// validClock bounds an hour:minute in 24h time.
func validClock(hour, minute int) error {
	if hour < 0 || hour > 23 {
		return fmt.Errorf("schedsvc: hour must be 0..23, got %d", hour)
	}
	if minute < 0 || minute > 59 {
		return fmt.Errorf("schedsvc: minute must be 0..59, got %d", minute)
	}
	return nil
}
