package schedsvc

import (
	"testing"
	"time"
)

// TestNextFireDSTSpringForward locks in robfig's DST behavior across the EU
// spring-forward day (PRD #241 Decision 3, M2 DST guard).
//
// In Europe/Bucharest the 2026 transition is Sunday 29 March 2026: at 03:00 EET
// (+02:00) clocks jump to 04:00 EEST (+03:00), so the wall-clock hour 03:00–03:59
// does NOT exist that day (02:30, used below and by the PRD text, DOES exist —
// the gap is 03:xx, not 02:xx).
func TestNextFireDSTSpringForward(t *testing.T) {
	const tz = "Europe/Bucharest"
	loc, err := time.LoadLocation(tz)
	if err != nil {
		t.Fatalf("LoadLocation(%q): %v", tz, err)
	}

	// Sanity: confirm the transition really is 29 March 2026 with a +2 -> +3 jump
	// across the 03:00 wall boundary, so the assertions below rest on a verified
	// transition rather than a memorized date.
	beforeGap := time.Date(2026, 3, 29, 2, 30, 0, 0, loc)
	if _, off := beforeGap.Zone(); off != 2*3600 {
		t.Fatalf("expected +02:00 at 02:30 on transition day, got offset %ds", off)
	}
	afterGap := time.Date(2026, 3, 29, 4, 30, 0, 0, loc)
	if _, off := afterGap.Zone(); off != 3*3600 {
		t.Fatalf("expected +03:00 at 04:30 on transition day, got offset %ds", off)
	}

	// A daily 02:30 cron. 02:30 exists on the transition day, so it fires at
	// 02:30 EET = 00:30 UTC — proving the fire is stamped with the pre-switch
	// (+02:00) offset and does not double-fire.
	startOfDay := time.Date(2026, 3, 29, 0, 0, 0, 0, time.UTC)
	got, err := NextFire("30 2 * * *", tz, startOfDay)
	if err != nil {
		t.Fatalf("NextFire: %v", err)
	}
	wantTransition := time.Date(2026, 3, 29, 0, 30, 0, 0, time.UTC)
	if !got.Equal(wantTransition) {
		t.Fatalf("transition-day fire = %s, want %s", got.Format(time.RFC3339), wantTransition.Format(time.RFC3339))
	}
	if got.Location() != time.UTC {
		t.Fatalf("NextFire must return UTC, got location %s", got.Location())
	}

	// The very next daily fire is 2026-03-30 02:30 EEST = 2026-03-29 23:30 UTC —
	// only 23 hours after the previous fire, exactly the spring-forward
	// compression. This is the load-bearing DST assertion: consecutive daily
	// fires are 23h apart across the transition, not a naive 24h.
	next, err := NextFire("30 2 * * *", tz, got)
	if err != nil {
		t.Fatalf("NextFire (second): %v", err)
	}
	wantNext := time.Date(2026, 3, 29, 23, 30, 0, 0, time.UTC)
	if !next.Equal(wantNext) {
		t.Fatalf("post-transition fire = %s, want %s", next.Format(time.RFC3339), wantNext.Format(time.RFC3339))
	}
	if gap := next.Sub(got); gap != 23*time.Hour {
		t.Fatalf("gap between consecutive daily fires = %s, want 23h", gap)
	}

	// A daily 03:30 cron lands in the missing hour on the transition day.
	// robfig's documented behavior is to SKIP a non-existent wall-clock time
	// (the hour never matches after time.Date normalizes it forward), so the
	// fire moves to the NEXT day at 03:30 EEST = 00:30 UTC — it does not fire
	// twice and does not silently shift into the 04:xx slot on the same day.
	gotGap, err := NextFire("30 3 * * *", tz, startOfDay)
	if err != nil {
		t.Fatalf("NextFire (gap): %v", err)
	}
	wantGapSkip := time.Date(2026, 3, 30, 0, 30, 0, 0, time.UTC)
	if !gotGap.Equal(wantGapSkip) {
		t.Fatalf("missing-hour cron fire = %s, want %s (skip to next day)", gotGap.Format(time.RFC3339), wantGapSkip.Format(time.RFC3339))
	}
}

func TestNextFireStrictlyAfter(t *testing.T) {
	// `after` sitting exactly on a fire instant must yield the NEXT one, never
	// the same instant (Decision 8's "strictly after" claim on next_fire_at).
	after := time.Date(2026, 1, 5, 9, 0, 0, 0, time.UTC) // a 09:00 fire instant
	got, err := NextFire("0 9 * * *", "UTC", after)
	if err != nil {
		t.Fatalf("NextFire: %v", err)
	}
	want := time.Date(2026, 1, 6, 9, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("NextFire = %s, want %s", got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

func TestValidateCron(t *testing.T) {
	valid := []string{
		"30 2 * * *",
		"0 9 * * 1-5",
		"*/15 * * * *",
		"0 0 1 1 *",
	}
	for _, expr := range valid {
		if err := ValidateCron(expr); err != nil {
			t.Errorf("ValidateCron(%q) = %v, want nil", expr, err)
		}
	}

	invalid := []string{
		"",                // empty
		"not a cron",      // garbage
		"99 2 * * *",      // minute out of range
		"30 25 * * *",     // hour out of range
		"30 2 * *",        // too few fields
		"0 30 2 * * *",    // 6-field (seconds-prefixed) not accepted as standard
		"@every 1h",       // descriptor rejected: stored form is plain 5-field
		"30 2 * * MONDAY", // bad day-of-week token
	}
	for _, expr := range invalid {
		if err := ValidateCron(expr); err == nil {
			t.Errorf("ValidateCron(%q) = nil, want error", expr)
		}
	}
}

func TestNextFiresMonotonic(t *testing.T) {
	after := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	const n = 6
	fires, err := NextFires("0 */4 * * *", "UTC", after, n)
	if err != nil {
		t.Fatalf("NextFires: %v", err)
	}
	if len(fires) != n {
		t.Fatalf("NextFires returned %d fires, want %d", len(fires), n)
	}
	prev := after
	for i, f := range fires {
		if f.Location() != time.UTC {
			t.Errorf("fire[%d] location = %s, want UTC", i, f.Location())
		}
		if !f.After(prev) {
			t.Errorf("fire[%d] = %s not strictly after previous %s", i, f.Format(time.RFC3339), prev.Format(time.RFC3339))
		}
		prev = f
	}
	// "0 */4 * * *" fires every 4 hours: first fire is 04:00 UTC on Jan 1.
	want0 := time.Date(2026, 1, 1, 4, 0, 0, 0, time.UTC)
	if !fires[0].Equal(want0) {
		t.Errorf("fires[0] = %s, want %s", fires[0].Format(time.RFC3339), want0.Format(time.RFC3339))
	}
}

func TestNextFiresRejectsNonPositiveN(t *testing.T) {
	for _, n := range []int{0, -3} {
		if _, err := NextFires("0 9 * * *", "UTC", time.Now(), n); err == nil {
			t.Errorf("NextFires(n=%d) = nil error, want error", n)
		}
	}
}

func TestNextFireInvalidInputs(t *testing.T) {
	if _, err := NextFire("garbage", "UTC", time.Now()); err == nil {
		t.Error("NextFire with bad cron = nil error, want error")
	}
	if _, err := NextFire("0 9 * * *", "Not/AZone", time.Now()); err == nil {
		t.Error("NextFire with bad tz = nil error, want error")
	}
}

func TestOnceFire(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	// Future run_at, expressed in a non-UTC zone, normalizes to the same instant
	// in UTC (the timezone changes display, not the instant).
	loc, err := time.LoadLocation("Europe/Bucharest")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	runAt := time.Date(2026, 1, 2, 10, 0, 0, 0, loc) // 10:00 EET = 08:00 UTC
	got, err := OnceFire(runAt, now)
	if err != nil {
		t.Fatalf("OnceFire: %v", err)
	}
	want := time.Date(2026, 1, 2, 8, 0, 0, 0, time.UTC)
	if !got.Equal(want) || got.Location() != time.UTC {
		t.Fatalf("OnceFire = %s (%s), want %s UTC", got.Format(time.RFC3339), got.Location(), want.Format(time.RFC3339))
	}

	// A run_at in the past is rejected.
	if _, err := OnceFire(now.Add(-time.Minute), now); err == nil {
		t.Error("OnceFire with past run_at = nil error, want error")
	}
	// A run_at equal to now is rejected (strictly-after semantics).
	if _, err := OnceFire(now, now); err == nil {
		t.Error("OnceFire with run_at == now = nil error, want error")
	}
}
