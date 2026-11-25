package analytics

import (
	"fmt"
	"testing"
	"time"
)

func zone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("load %s: %v", name, err)
	}
	return loc
}

func utc(y int, m time.Month, d, h, mi int) time.Time {
	return time.Date(y, m, d, h, mi, 0, 0, time.UTC)
}

func mustDayStart(t *testing.T, d Date, loc *time.Location) time.Time {
	t.Helper()
	s, err := DayStart(d, loc)
	if err != nil {
		t.Fatalf("DayStart(%s, %s): %v", d, loc, err)
	}
	return s
}

func eq(t *testing.T, what string, got, want time.Time) {
	t.Helper()
	if !got.Equal(want) {
		t.Errorf("%s: got %s, want %s", what, got.UTC().Format(time.RFC3339), want.UTC().Format(time.RFC3339))
	}
}

func TestDayStartPinnedInstants(t *testing.T) {
	cases := []struct {
		zone string
		date Date
		want time.Time
	}{
		{"Europe/Bucharest", Date{2026, 10, 25}, utc(2026, 10, 24, 21, 0)},
		{"Europe/Bucharest", Date{2026, 10, 26}, utc(2026, 10, 25, 22, 0)},
		{"America/Santiago", Date{2022, 9, 11}, utc(2022, 9, 11, 4, 0)},
		{"America/Havana", Date{2022, 11, 6}, utc(2022, 11, 6, 4, 0)},
		{"America/Sao_Paulo", Date{2018, 2, 18}, utc(2018, 2, 18, 3, 0)},
		{"Pacific/Apia", Date{2011, 12, 30}, utc(2011, 12, 30, 10, 0)},
		{"Pacific/Apia", Date{2011, 12, 31}, utc(2011, 12, 30, 10, 0)},
	}
	for _, c := range cases {
		eq(t, c.zone+" "+c.date.String(), mustDayStart(t, c.date, zone(t, c.zone)), c.want)
	}
}

// ParseInLocation runs backwards across Apia's skipped date, which is why
// dayStart never builds instants with it (or time.Date) in loc.
func TestConstructionRuleParseInLocationRunsBackwards(t *testing.T) {
	apia := zone(t, "Pacific/Apia")
	p, err := time.ParseInLocation("2006-01-02", "2011-12-30", apia)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "ParseInLocation(2011-12-30, Apia)", p, utc(2011, 12, 29, 10, 0))
	eq(t, "DayStart(2011-12-30, Apia)", mustDayStart(t, Date{2011, 12, 30}, apia), utc(2011, 12, 30, 10, 0))
}

// withGuard fails the test if f takes longer than 5s.
func withGuard(t *testing.T, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); f() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("did not finish within 5s (no forward progress?)")
	}
}

// fpCase is one dayStart call in a forward-progress test. fallback is true when
// the call must take fallback steps and false when it must take none.
type fpCase struct {
	date     Date
	want     time.Time
	fallback bool
}

// zoneBoundsDefect reports whether Go's ZoneBounds still returns the known-wrong segment end
// for probe: wantEnd is the (too early) end the defect produces. The fallback-step assertions
// hold only under that precondition; if Go fixes it, the dayStart values are still asserted.
func zoneBoundsDefect(t *testing.T, loc *time.Location, probe, wantEnd time.Time) bool {
	t.Helper()
	_, end := probe.In(loc).ZoneBounds()
	if end.Equal(wantEnd) {
		return true
	}
	t.Logf("ZoneBounds(%s) end = %s, not the known defect %s: fallback-step assertions skipped", probe, end.UTC(), wantEnd)
	return false
}

func forwardProgress(t *testing.T, zoneName string, defect func(*testing.T, *time.Location) bool, cases []fpCase) {
	t.Helper()
	loc := zone(t, zoneName)
	checkSteps := defect(t, loc)
	for _, c := range cases {
		var steps int
		var got time.Time
		var err error
		withGuard(t, func() { got, err = dayStart(c.date, loc, &steps) })
		if err != nil {
			t.Fatalf("%s %s: %v", zoneName, c.date, err)
		}
		eq(t, zoneName+" "+c.date.String(), got, c.want)
		t.Logf("%s %s fallback steps: %d", zoneName, c.date, steps)
		if !checkSteps {
			continue
		}
		if c.fallback && steps == 0 {
			t.Errorf("%s %s: expected fallback steps, got 0", zoneName, c.date)
		}
		if !c.fallback && steps != 0 {
			t.Errorf("%s %s: expected no fallback steps, got %d", zoneName, c.date, steps)
		}
		if steps > 273600+1000 {
			t.Errorf("%s %s: fallback steps %d exceed bound", zoneName, c.date, steps)
		}
	}
}

func TestDayStartForwardProgressNewYork2040(t *testing.T) {
	forwardProgress(t, "America/New_York", func(t *testing.T, loc *time.Location) bool {
		return zoneBoundsDefect(t, loc, utc(2040, 12, 31, 12, 0), utc(2040, 12, 31, 0, 0))
	}, []fpCase{
		{Date{2040, 12, 31}, utc(2040, 12, 31, 5, 0), true},
		{Date{2041, 1, 1}, utc(2041, 1, 1, 5, 0), true},
	})
}

func TestDayStartForwardProgressBucharest2044(t *testing.T) {
	forwardProgress(t, "Europe/Bucharest", func(t *testing.T, loc *time.Location) bool {
		return zoneBoundsDefect(t, loc, utc(2044, 12, 31, 12, 0), utc(2044, 12, 31, 0, 0))
	}, []fpCase{
		// East of UTC the search segment for 2044-12-31 (starting at
		// 2044-12-29T22:00Z) is still valid, so no fallback is needed.
		{Date{2044, 12, 31}, utc(2044, 12, 30, 22, 0), false},
		{Date{2045, 1, 1}, utc(2044, 12, 31, 22, 0), true},
	})
}

func TestAllTimeLongHistoryTerminates(t *testing.T) {
	earliest := utc(2030, 1, 7, 12, 0)
	type lhCase struct {
		zone   string
		now    time.Time
		dates  []Date // dates whose dayStart must take fallback steps (7d, 90d)
		window Window
	}
	var cases []lhCase
	for _, w := range []Window{WindowAll, Window7d, Window90d} {
		cases = append(cases,
			lhCase{"America/New_York", utc(2041, 1, 3, 12, 0), []Date{{2040, 12, 31}, {2041, 1, 1}}, w},
			lhCase{"Europe/Bucharest", utc(2045, 1, 3, 12, 0), []Date{{2045, 1, 1}}, w},
		)
	}
	for _, c := range cases {
		t.Run(c.zone+"/"+string(c.window), func(t *testing.T) {
			loc := zone(t, c.zone)
			steps := map[Date]int{}
			dayStartObserver = func(d Date, n int) { steps[d] += n }
			defer func() { dayStartObserver = nil }()
			var b Bounds
			var err error
			withGuard(t, func() { b, err = ComputeBounds(c.now, loc, c.window, &earliest) })
			if err != nil {
				t.Fatal(err)
			}
			if len(b.Buckets) == 0 || len(b.Buckets) > 104 {
				t.Fatalf("%d buckets", len(b.Buckets))
			}
			if c.window == WindowAll && b.WidthWeeks == 0 {
				t.Fatal("all window has no bucket width")
			}
			checkTiling(t, b, loc)
			// The all window only evaluates week starts, so the defect dates
			// need not be hit there; the day windows must hit them.
			if c.window != WindowAll {
				for _, d := range c.dates {
					if steps[d] == 0 {
						t.Errorf("dayStart(%s) took no fallback steps: fallback path not exercised", d)
					}
				}
			}
		})
	}
}

func TestAllWindowEarliestAfterNowIsClamped(t *testing.T) {
	loc := zone(t, "America/New_York")
	// Sunday 2026-03-15 23:30 local; earliest is Monday 2026-03-16 01:30 local, so the
	// unclamped Monday-of-week would be 03-16, after now (review: a same-week pair could
	// not detect a missing clamp).
	now := utc(2026, 3, 16, 3, 30)
	earliest := now.Add(2 * time.Hour)
	if DateOf(earliest, loc) == DateOf(now, loc) {
		t.Fatal("test setup: earliest must be on a later local date than now")
	}
	if DateOf(now, loc).Weekday() != time.Sunday || DateOf(earliest, loc).Weekday() != time.Monday {
		t.Fatal("test setup: now must be a Sunday and earliest the following Monday, local")
	}
	b, err := ComputeBounds(now, loc, WindowAll, &earliest)
	if err != nil {
		t.Fatal(err)
	}
	if b.HistoryStart == nil || b.HistoryStart.After(now) {
		t.Fatalf("HistoryStart %v after now", b.HistoryStart)
	}
	if len(b.Buckets) != 1 {
		t.Fatalf("got %d buckets, want 1", len(b.Buckets))
	}
	bk := b.Buckets[0]
	if bk.Start.After(bk.End) || !bk.End.Equal(now) || !bk.Partial {
		t.Errorf("malformed bucket %+v", bk)
	}
	checkTiling(t, b, loc)
}

// checkTiling asserts the structural calendar invariants for one Bounds.
func checkTiling(t *testing.T, b Bounds, loc *time.Location) {
	t.Helper()
	if err := tilingError(b, loc); err != nil {
		t.Fatal(err)
	}
}

// tilingError returns the first violated calendar invariant, or nil.
func tilingError(b Bounds, loc *time.Location) error {
	if len(b.Buckets) == 0 {
		return fmt.Errorf("no buckets")
	}
	if !b.Buckets[0].Start.Equal(b.From) {
		return fmt.Errorf("first start %s != from %s", b.Buckets[0].Start, b.From)
	}
	for i, bk := range b.Buckets {
		if bk.End.Before(bk.Start) {
			return fmt.Errorf("bucket %d (%s) ends before it starts", i, bk.Label)
		}
		if i > 0 && !bk.Start.Equal(b.Buckets[i-1].End) {
			return fmt.Errorf("bucket %d start %s != previous end %s", i, bk.Start, b.Buckets[i-1].End)
		}
	}
	last := b.Buckets[len(b.Buckets)-1]
	if !last.End.Equal(b.Now) || !last.Partial {
		return fmt.Errorf("last bucket end %s partial=%v, now %s", last.End, last.Partial, b.Now)
	}
	// Half-open [start, now) excludes now: a run created at exactly now is in
	// no bucket (and, by created_at < @to, in no total).
	if b.Now.Before(last.End) {
		return fmt.Errorf("now inside last bucket")
	}
	ts, tn := b.TodayStart, b.NextDayStart
	if ts.After(b.Now) || !b.Now.Before(tn) {
		return fmt.Errorf("today's day [%s,%s) does not contain now %s", ts, tn, b.Now)
	}
	if b.WidthWeeks == 0 && !last.Start.Equal(ts) {
		return fmt.Errorf("last day bucket starts %s, want %s", last.Start, ts)
	}
	if b.PreviousFrom != nil && b.PreviousFrom.After(b.From) {
		return fmt.Errorf("previous_from after from")
	}
	if len(b.Rolling) != len(b.Buckets) {
		return fmt.Errorf("rolling %d vs buckets %d", len(b.Rolling), len(b.Buckets))
	}
	for i, r := range b.Rolling {
		if r.End.Before(r.Start) || !r.End.Equal(b.Buckets[i].End) {
			return fmt.Errorf("rolling %d malformed", i)
		}
	}
	return nil
}

func TestDaysUntilBeyondDurationRange(t *testing.T) {
	// 1700-01-01 to 2026-11-25 is far beyond the ~292 years a time.Duration holds.
	a, b := Date{1700, 1, 1}, Date{2026, 11, 25}
	want := int(b.utcMidnight().Unix()-a.utcMidnight().Unix()) / 86400
	if want != 119_397 {
		t.Fatalf("test setup: expected day count %d", want)
	}
	if got := a.DaysUntil(b); got != 119_397 {
		t.Errorf("DaysUntil = %d, want 119397", got)
	}
	if got := b.DaysUntil(a); got != -119_397 {
		t.Errorf("reverse DaysUntil = %d, want -119397", got)
	}
	if got := (Date{2024, 2, 28}).DaysUntil(Date{2024, 3, 1}); got != 2 {
		t.Errorf("leap-year span = %d, want 2", got)
	}
	if got := a.AddDays(119_397); got != b {
		t.Errorf("AddDays = %s, want %s", got, b)
	}
}

func TestComputeBoundsAllWindowEarliest1700(t *testing.T) {
	loc := zone(t, "UTC")
	now := utc(2026, 11, 25, 12, 0)
	earliest := utc(1700, 1, 1, 0, 0)
	b, err := ComputeBounds(now, loc, WindowAll, &earliest)
	if err != nil {
		t.Fatal(err)
	}
	if b.WidthWeeks < 1 || len(b.Buckets) == 0 || len(b.Buckets) > 104 {
		t.Fatalf("width %d weeks, %d buckets", b.WidthWeeks, len(b.Buckets))
	}
	// 1700-01-01 is a Friday; buckets are Monday-anchored, so the first starts 1699-12-28.
	if first := b.Buckets[0].Start; first.After(earliest) || earliest.Sub(first) >= 7*24*time.Hour {
		t.Errorf("first bucket start %s does not anchor the earliest run %s to its Monday", first, earliest)
	}
	span := DateOf(b.Buckets[0].Start, loc).DaysUntil(DateOf(now, loc))
	if span < 119_000 {
		t.Errorf("span %d days, want about 119397", span)
	}
	for i, bk := range b.Buckets[:len(b.Buckets)-1] {
		if got := DateOf(bk.Start, loc).DaysUntil(DateOf(bk.End, loc)); got != 7*b.WidthWeeks {
			t.Errorf("bucket %d is %d days wide, want %d", i, got, 7*b.WidthWeeks)
		}
	}
	checkTiling(t, b, loc)
}
