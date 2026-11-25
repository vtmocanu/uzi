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

func forwardProgress(t *testing.T, zoneName string, probe time.Time, d1, d2 Date, want1, want2 time.Time) {
	t.Helper()
	loc := zone(t, zoneName)
	_, end := probe.In(loc).ZoneBounds()
	affected := !end.After(probe) && !end.IsZero()
	if !affected {
		t.Logf("%s: ZoneBounds end %s is after %s; precondition does not hold, fallback not asserted", zoneName, end.UTC(), probe)
	}
	var steps1, steps2 int
	var got1, got2 time.Time
	var err1, err2 error
	withGuard(t, func() {
		got1, err1 = dayStart(d1, loc, &steps1)
		got2, err2 = dayStart(d2, loc, &steps2)
	})
	if err1 != nil || err2 != nil {
		t.Fatalf("errors: %v, %v", err1, err2)
	}
	eq(t, zoneName+" "+d1.String(), got1, want1)
	eq(t, zoneName+" "+d2.String(), got2, want2)
	t.Logf("%s fallback steps: %d and %d", zoneName, steps1, steps2)
	if affected && steps1+steps2 == 0 {
		t.Errorf("%s: precondition holds but fallback counter is zero", zoneName)
	}
	if steps1 > 273600+1000 || steps2 > 273600+1000 {
		t.Errorf("%s: fallback steps %d/%d exceed bound", zoneName, steps1, steps2)
	}
}

func TestDayStartForwardProgressNewYork2040(t *testing.T) {
	loc := zone(t, "America/New_York")
	_, end := time.Date(2040, 12, 31, 12, 0, 0, 0, time.UTC).In(loc).ZoneBounds()
	if !end.Equal(utc(2040, 12, 31, 0, 0)) {
		t.Logf("precondition: ZoneBounds end is %s, not 2040-12-31T00:00Z", end.UTC())
	}
	forwardProgress(t, "America/New_York", utc(2040, 12, 31, 12, 0),
		Date{2040, 12, 31}, Date{2041, 1, 1},
		utc(2040, 12, 31, 5, 0), utc(2041, 1, 1, 5, 0))
}

func TestDayStartForwardProgressBucharest2044(t *testing.T) {
	forwardProgress(t, "Europe/Bucharest", utc(2044, 12, 31, 12, 0),
		Date{2044, 12, 31}, Date{2045, 1, 1},
		utc(2044, 12, 30, 22, 0), utc(2044, 12, 31, 22, 0))
}

func TestAllTimeLongHistoryTerminates(t *testing.T) {
	for _, name := range []string{"America/New_York", "Europe/Bucharest"} {
		t.Run(name, func(t *testing.T) {
			loc := zone(t, name)
			earliest := utc(2030, 1, 7, 12, 0)
			now := utc(2041, 6, 15, 12, 0)
			var b Bounds
			var err error
			withGuard(t, func() { b, err = ComputeBounds(now, loc, WindowAll, &earliest) })
			if err != nil {
				t.Fatal(err)
			}
			if b.WidthWeeks == 0 || len(b.Buckets) == 0 || len(b.Buckets) > 104 {
				t.Fatalf("width %d, %d buckets", b.WidthWeeks, len(b.Buckets))
			}
			if len(b.Rolling) != len(b.Buckets) {
				t.Fatalf("rolling %d vs buckets %d", len(b.Rolling), len(b.Buckets))
			}
			checkTiling(t, b, loc)
		})
	}
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
