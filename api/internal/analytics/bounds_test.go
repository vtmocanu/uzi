package analytics

import (
	"testing"
	"time"
)

func findBucket(t *testing.T, b Bounds, label Date) (int, Bucket) {
	t.Helper()
	for i, bk := range b.Buckets {
		if bk.Label == label {
			return i, bk
		}
	}
	t.Fatalf("no bucket labelled %s", label)
	return 0, Bucket{}
}

func TestParseWindow(t *testing.T) {
	if w, err := ParseWindow(""); err != nil || w != Window30d {
		t.Errorf("default: %v %v", w, err)
	}
	for _, s := range []string{"7d", "30d", "90d", "all"} {
		if w, err := ParseWindow(s); err != nil || string(w) != s {
			t.Errorf("%s: %v %v", s, w, err)
		}
	}
	for _, s := range []string{"1d", "ALL", "30", " 7d"} {
		if _, err := ParseWindow(s); err == nil {
			t.Errorf("%q accepted", s)
		}
	}
}

func TestBoundedWindowBounds(t *testing.T) {
	now := utc(2026, 11, 25, 12, 0)
	cases := []struct {
		w          Window
		buckets    int
		from, prev time.Time
	}{
		{Window7d, 7, utc(2026, 11, 19, 0, 0), utc(2026, 11, 12, 0, 0)},
		{Window30d, 30, utc(2026, 10, 27, 0, 0), utc(2026, 9, 27, 0, 0)},
		{Window90d, 90, utc(2026, 8, 28, 0, 0), utc(2026, 5, 30, 0, 0)},
	}
	for _, c := range cases {
		b, err := ComputeBounds(now, time.UTC, c.w, nil)
		if err != nil {
			t.Fatal(err)
		}
		eq(t, string(c.w)+" from", b.From, c.from)
		if b.PreviousFrom == nil {
			t.Fatalf("%s: no previous_from", c.w)
		}
		eq(t, string(c.w)+" previous", *b.PreviousFrom, c.prev)
		eq(t, string(c.w)+" fold", b.FoldFrom, c.prev)
		if len(b.Buckets) != c.buckets || b.WidthWeeks != 0 || b.HistoryStart != nil {
			t.Errorf("%s: %d buckets width %d history %v", c.w, len(b.Buckets), b.WidthWeeks, b.HistoryStart)
		}
		if b.Today != (Date{2026, 11, 25}) {
			t.Errorf("today %s", b.Today)
		}
		checkTiling(t, b, time.UTC)
		if len(b.BucketStarts) != c.buckets || len(b.RollEnds) != c.buckets {
			t.Errorf("%s: convenience arrays", c.w)
		}
	}
}

func TestAllEmptyScope(t *testing.T) {
	now := utc(2026, 11, 25, 12, 0)
	b, err := ComputeBounds(now, time.UTC, WindowAll, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Buckets) != 0 || len(b.Rolling) != 0 || b.PreviousFrom != nil || b.HistoryStart != nil {
		t.Errorf("empty scope not empty: %+v", b)
	}
	eq(t, "from", b.From, now)
	eq(t, "fold", b.FoldFrom, now)
	if b.WidthWeeks != 1 {
		t.Errorf("empty all-time scope width %d weeks, want 1", b.WidthWeeks)
	}
}

func TestAllWidthCoarsening(t *testing.T) {
	now := utc(2026, 11, 25, 12, 0)
	cases := []struct {
		earliest time.Time
		width    int
		buckets  int
	}{
		{utc(2026, 9, 9, 8, 0), 1, 12},
		{utc(2023, 1, 2, 9, 0), 2, 102},
		{utc(2020, 1, 1, 0, 0), 4, 0}, // bucket count asserted loosely below
	}
	for _, c := range cases {
		e := c.earliest
		b, err := ComputeBounds(now, time.UTC, WindowAll, &e)
		if err != nil {
			t.Fatal(err)
		}
		if b.WidthWeeks != c.width {
			t.Errorf("%s: width %d, want %d", e, b.WidthWeeks, c.width)
		}
		if c.buckets != 0 && len(b.Buckets) != c.buckets {
			t.Errorf("%s: %d buckets, want %d", e, len(b.Buckets), c.buckets)
		}
		if len(b.Buckets) > 104 {
			t.Errorf("%s: %d buckets over cap", e, len(b.Buckets))
		}
		if b.PreviousFrom != nil {
			t.Error("all has no previous window")
		}
		checkTiling(t, b, time.UTC)
	}
}

func TestRollingFixtureA_FullOneWeekBucket(t *testing.T) {
	earliest := utc(2026, 9, 9, 8, 0) // history_start = Monday 2026-09-07
	b, err := ComputeBounds(utc(2026, 11, 25, 12, 0), time.UTC, WindowAll, &earliest)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "history_start", *b.HistoryStart, utc(2026, 9, 7, 0, 0))
	if b.WidthWeeks != 1 {
		t.Fatalf("width %d", b.WidthWeeks)
	}
	i, bk := findBucket(t, b, Date{2026, 10, 12})
	eq(t, "bucket start", bk.Start, utc(2026, 10, 12, 0, 0))
	eq(t, "bucket end", bk.End, utc(2026, 10, 19, 0, 0))
	r := b.Rolling[i]
	eq(t, "R0 start", r.UnclippedStart, utc(2026, 10, 12, 0, 0))
	eq(t, "R start", r.Start, utc(2026, 10, 12, 0, 0))
	eq(t, "R end", r.End, utc(2026, 10, 19, 0, 0))
	if r.Partial {
		t.Error("partial")
	}
}

func TestRollingFixtureB_MultiweekBucket(t *testing.T) {
	earliest := utc(2023, 1, 2, 9, 0)
	b, err := ComputeBounds(utc(2026, 11, 25, 12, 0), time.UTC, WindowAll, &earliest)
	if err != nil {
		t.Fatal(err)
	}
	if b.WidthWeeks != 2 || len(b.Buckets) != 102 {
		t.Fatalf("width %d buckets %d", b.WidthWeeks, len(b.Buckets))
	}
	i, bk := findBucket(t, b, Date{2025, 2, 24})
	eq(t, "bucket end", bk.End, utc(2025, 3, 10, 0, 0))
	r := b.Rolling[i]
	eq(t, "R0 start", r.UnclippedStart, utc(2025, 3, 3, 0, 0))
	eq(t, "R end", r.End, utc(2025, 3, 10, 0, 0))
	if r.Partial {
		t.Error("partial")
	}
	r = b.Rolling[0]
	eq(t, "first R0 start", r.UnclippedStart, utc(2023, 1, 9, 0, 0))
	eq(t, "first R end", r.End, utc(2023, 1, 16, 0, 0))
	if r.Partial {
		t.Error("first bucket partial")
	}
}

func TestRollingFixtureC_SkippedDate(t *testing.T) {
	apia := zone(t, "Pacific/Apia")
	earliest := utc(2011, 1, 5, 0, 0)
	b, err := ComputeBounds(utc(2012, 1, 3, 0, 0), apia, Window7d, &earliest)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "from", b.From, utc(2011, 12, 28, 10, 0))
	i29, b29 := findBucket(t, b, Date{2011, 12, 29})
	eq(t, "b29 start", b29.Start, utc(2011, 12, 29, 10, 0))
	eq(t, "b29 end", b29.End, utc(2011, 12, 30, 10, 0))
	r := b.Rolling[i29]
	eq(t, "b29 R0 start", r.UnclippedStart, utc(2011, 12, 23, 10, 0))
	eq(t, "b29 R0 end", r.End, utc(2011, 12, 30, 10, 0))
	if r.Partial {
		t.Error("b29 partial")
	}
	for _, bk := range b.Buckets {
		if bk.Label == (Date{2011, 12, 30}) {
			t.Error("empty skipped-date bucket was emitted")
		}
	}
	i31, b31 := findBucket(t, b, Date{2011, 12, 31})
	eq(t, "b31 start", b31.Start, utc(2011, 12, 30, 10, 0))
	eq(t, "b31 end", b31.End, utc(2011, 12, 31, 10, 0))
	r = b.Rolling[i31]
	eq(t, "b31 R0 start", r.UnclippedStart, utc(2011, 12, 25, 10, 0))
	eq(t, "b31 R0 end", r.End, utc(2011, 12, 31, 10, 0))
	if r.End.Sub(r.Start) != 144*time.Hour {
		t.Errorf("b31 R0 spans %s, want 144h", r.End.Sub(r.Start))
	}
	if r.Partial {
		t.Error("b31 partial")
	}
	if last := b.Rolling[len(b.Rolling)-1]; !last.Partial {
		t.Error("today's point not partial")
	}
	checkTiling(t, b, apia)
}

func TestRollingFixtureD_PointEndingAtNow(t *testing.T) {
	earliest := utc(2025, 1, 1, 0, 0)
	b, err := ComputeBounds(utc(2026, 11, 25, 12, 0), time.UTC, Window30d, &earliest)
	if err != nil {
		t.Fatal(err)
	}
	r := b.Rolling[len(b.Rolling)-1]
	eq(t, "R0 start", r.UnclippedStart, utc(2026, 11, 19, 0, 0))
	eq(t, "R0 end", r.End, utc(2026, 11, 25, 12, 0))
	if !r.Partial {
		t.Error("today's point not partial")
	}
}

func TestRollingFixtureE_NowAtLocalMidnight(t *testing.T) {
	buch := zone(t, "Europe/Bucharest")
	earliest := utc(2025, 1, 1, 0, 0)
	now := utc(2026, 11, 24, 22, 0)
	b, err := ComputeBounds(now, buch, Window30d, &earliest)
	if err != nil {
		t.Fatal(err)
	}
	if b.Today != (Date{2026, 11, 25}) {
		t.Fatalf("today %s", b.Today)
	}
	n := len(b.Buckets)
	today, prev := b.Buckets[n-1], b.Buckets[n-2]
	if today.Label != (Date{2026, 11, 25}) || !today.Start.Equal(now) || !today.End.Equal(now) {
		t.Fatalf("today's bucket not the empty [now,now): %+v", today)
	}
	r := b.Rolling[n-1]
	eq(t, "today R0 start", r.UnclippedStart, utc(2026, 11, 18, 22, 0))
	eq(t, "today R end", r.End, now)
	if !r.Partial {
		t.Error("today's point not partial")
	}
	if prev.Label != (Date{2026, 11, 24}) || !prev.End.Equal(now) {
		t.Fatalf("previous bucket %+v", prev)
	}
	r = b.Rolling[n-2]
	eq(t, "prev R0 start", r.UnclippedStart, utc(2026, 11, 17, 22, 0))
	eq(t, "prev R end", r.End, now)
	if r.Partial {
		t.Error("previous bucket's point partial although its seven days are complete")
	}
	checkTiling(t, b, buch)
}

func TestRollingFixtureF_HistoryClippedPartial(t *testing.T) {
	earliest := utc(2026, 11, 10, 12, 0) // Tuesday; history_start = Monday 2026-11-09
	b, err := ComputeBounds(utc(2026, 11, 25, 12, 0), time.UTC, Window30d, &earliest)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "history_start", *b.HistoryStart, utc(2026, 11, 9, 0, 0))
	i, _ := findBucket(t, b, Date{2026, 11, 12})
	r := b.Rolling[i]
	if !r.Partial {
		t.Error("clipped point not partial")
	}
	eq(t, "R start", r.Start, utc(2026, 11, 9, 0, 0))
	eq(t, "R end", r.End, utc(2026, 11, 13, 0, 0))
	eq(t, "R0 start", r.UnclippedStart, utc(2026, 11, 6, 0, 0))
	i, _ = findBucket(t, b, Date{2026, 11, 16})
	if b.Rolling[i].Partial {
		t.Error("later point partial")
	}
}
