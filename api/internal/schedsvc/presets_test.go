package schedsvc

import "testing"

func TestPresetToCronMappings(t *testing.T) {
	cases := []struct {
		preset string
		hour   int
		minute int
		want   string
	}{
		{PresetWeekdays, 2, 0, "0 2 * * 1-5"},
		{PresetDaily, 9, 30, "30 9 * * *"},
		{PresetWeekly, 14, 15, "15 14 * * 1"},
	}
	for _, c := range cases {
		got, err := PresetToCron(c.preset, c.hour, c.minute)
		if err != nil {
			t.Errorf("PresetToCron(%q,%d,%d): %v", c.preset, c.hour, c.minute, err)
			continue
		}
		if got != c.want {
			t.Errorf("PresetToCron(%q,%d,%d) = %q, want %q", c.preset, c.hour, c.minute, got, c.want)
		}
		// The produced cron must itself be valid standard cron.
		if err := ValidateCron(got); err != nil {
			t.Errorf("ValidateCron(%q) = %v, want nil", got, err)
		}
	}
}

func TestPresetToCronErrors(t *testing.T) {
	// every_n_hours needs an interval, not a time.
	if _, err := PresetToCron(PresetEveryNHours, 0, 0); err == nil {
		t.Error("PresetToCron(every_n_hours) = nil error, want error")
	}
	// Custom has no canonical cron.
	if _, err := PresetToCron(PresetCustom, 9, 0); err == nil {
		t.Error("PresetToCron(custom) = nil error, want error")
	}
	// Unknown preset.
	if _, err := PresetToCron("nonsense", 9, 0); err == nil {
		t.Error("PresetToCron(nonsense) = nil error, want error")
	}
	// Out-of-range clock.
	if _, err := PresetToCron(PresetDaily, 24, 0); err == nil {
		t.Error("PresetToCron(daily, hour=24) = nil error, want error")
	}
	if _, err := PresetToCron(PresetDaily, 9, 60); err == nil {
		t.Error("PresetToCron(daily, minute=60) = nil error, want error")
	}
}

func TestEveryNHoursCron(t *testing.T) {
	got, err := EveryNHoursCron(6)
	if err != nil {
		t.Fatalf("EveryNHoursCron(6): %v", err)
	}
	if got != "0 */6 * * *" {
		t.Fatalf("EveryNHoursCron(6) = %q, want %q", got, "0 */6 * * *")
	}
	if err := ValidateCron(got); err != nil {
		t.Fatalf("ValidateCron(%q) = %v", got, err)
	}
	for _, n := range []int{0, -1, 24, 100} {
		if _, err := EveryNHoursCron(n); err == nil {
			t.Errorf("EveryNHoursCron(%d) = nil error, want error", n)
		}
	}
}

func TestPresetRoundTrip(t *testing.T) {
	cases := []struct {
		preset string
		hour   int
		minute int
	}{
		{PresetWeekdays, 2, 0},
		{PresetDaily, 9, 30},
		{PresetWeekly, 14, 15},
		{PresetDaily, 0, 0},
		{PresetWeekly, 23, 59},
	}
	for _, c := range cases {
		expr, err := PresetToCron(c.preset, c.hour, c.minute)
		if err != nil {
			t.Fatalf("PresetToCron(%q,%d,%d): %v", c.preset, c.hour, c.minute, err)
		}
		preset, hour, minute, ok := CronToPreset(expr)
		if !ok {
			t.Errorf("CronToPreset(%q) ok=false, want true", expr)
			continue
		}
		if preset != c.preset || hour != c.hour || minute != c.minute {
			t.Errorf("round-trip %q,%d,%d -> %q -> (%q,%d,%d)", c.preset, c.hour, c.minute, expr, preset, hour, minute)
		}
	}
}

func TestEveryNHoursRoundTrip(t *testing.T) {
	for _, n := range []int{1, 2, 6, 12, 23} {
		expr, err := EveryNHoursCron(n)
		if err != nil {
			t.Fatalf("EveryNHoursCron(%d): %v", n, err)
		}
		preset, hour, minute, ok := CronToPreset(expr)
		if !ok || preset != PresetEveryNHours {
			t.Errorf("CronToPreset(%q) = (%q,ok=%v), want every_n_hours", expr, preset, ok)
			continue
		}
		// For every_n_hours the interval N is carried in `hour`; minute is 0.
		if hour != n || minute != 0 {
			t.Errorf("CronToPreset(%q) = (hour=%d,minute=%d), want (N=%d,0)", expr, hour, minute, n)
		}
	}
}

func TestCronToPresetCustom(t *testing.T) {
	// Hand-written crons that no preset produces must return ok=false ("Custom").
	custom := []string{
		"30 2 15 * *",   // day-of-month set
		"30 2 * 6 *",    // month set
		"30 2 * * 2",    // Tuesday (not 1, 1-5, or *)
		"30 2 * * 0,3",  // weekday list
		"*/15 * * * *",  // minute step, no preset
		"0 0 * * 6",     // Saturday
		"0 */24 * * *",  // interval out of the 1..23 range -> not every_n_hours
		"5 */6 * * *",   // every_n_hours only fires on minute 0
		"0 9 * * 1-5 x", // wrong field count
		"",              // empty
	}
	for _, expr := range custom {
		preset, hour, minute, ok := CronToPreset(expr)
		if ok {
			t.Errorf("CronToPreset(%q) = (%q,%d,%d,ok=true), want ok=false", expr, preset, hour, minute)
		}
		if preset != PresetCustom {
			t.Errorf("CronToPreset(%q) preset = %q, want %q", expr, preset, PresetCustom)
		}
	}
}
