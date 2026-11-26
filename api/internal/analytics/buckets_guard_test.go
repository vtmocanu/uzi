package analytics

import (
	"errors"
	"testing"
	"time"
)

func TestValidateBucketsAcceptsComputedCalendars(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	earliest := func(tt time.Time) *time.Time { return &tt }
	cases := []struct {
		name     string
		now      time.Time
		loc      *time.Location
		w        Window
		earliest *time.Time
	}{
		{"7d", utc(2026, 11, 25, 12, 0), time.UTC, Window7d, nil},
		{"30d", utc(2026, 11, 25, 12, 0), time.UTC, Window30d, nil},
		{"90d", utc(2026, 11, 25, 12, 0), time.UTC, Window90d, nil},
		{"empty all-time", utc(2026, 11, 25, 12, 0), time.UTC, WindowAll, nil},
		{"ordinary all-time", utc(2026, 11, 25, 12, 0), time.UTC, WindowAll, earliest(utc(2026, 9, 9, 8, 0))},
		{"coarsened all-time", utc(2026, 11, 25, 12, 0), time.UTC, WindowAll, earliest(utc(2020, 1, 1, 0, 0))},
		// 05:00Z on 2026-11-25 is local midnight in New York: today's bucket is [now, now).
		{"local midnight now", utc(2026, 11, 25, 5, 0), ny, Window7d, nil},
		{"local midnight now all-time", utc(2026, 11, 25, 5, 0), ny, WindowAll, earliest(utc(2026, 10, 1, 12, 0))},
		// the DST fall-back day (25h) lies inside the window.
		{"dst fall back", utc(2026, 11, 3, 15, 0), ny, Window30d, nil},
	}
	for _, c := range cases {
		b, err := ComputeBounds(c.now, c.loc, c.w, c.earliest)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if err := ValidateBuckets(b); err != nil {
			t.Errorf("%s: %v", c.name, err)
		}
	}
}

func TestValidateBucketsFinalBucketMayBeZeroDurationAtMidnight(t *testing.T) {
	ny, _ := time.LoadLocation("America/New_York")
	b, err := ComputeBounds(utc(2026, 11, 25, 5, 0), ny, Window7d, nil)
	if err != nil {
		t.Fatal(err)
	}
	n := len(b.BucketStarts)
	if !b.BucketStarts[n-1].Equal(b.BucketEnds[n-1]) {
		t.Fatalf("fixture: final bucket is not zero-duration: %s..%s", b.BucketStarts[n-1], b.BucketEnds[n-1])
	}
	if err := ValidateBuckets(b); err != nil {
		t.Fatal(err)
	}
	// An earlier zero-duration bucket is never valid.
	b.BucketStarts[1] = b.BucketEnds[1]
	if err := ValidateBuckets(b); !errors.Is(err, ErrBucketTiling) {
		t.Fatalf("zero-duration interior bucket accepted: %v", err)
	}
}

func TestValidateBucketsZeroBuckets(t *testing.T) {
	now := utc(2026, 11, 25, 12, 0)
	if err := ValidateBuckets(Bounds{Now: now, From: now}); err != nil {
		t.Errorf("empty range: %v", err)
	}
	if err := ValidateBuckets(Bounds{Now: now, From: now.Add(-time.Hour)}); !errors.Is(err, ErrBucketTiling) {
		t.Errorf("zero buckets over a non-empty range accepted: %v", err)
	}
}

func TestValidateBucketsRejectsBrokenTilings(t *testing.T) {
	now := utc(2026, 11, 25, 12, 0)
	good := func() Bounds {
		b, err := ComputeBounds(now, time.UTC, Window7d, nil)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	cases := []struct {
		name   string
		mutate func(b *Bounds)
	}{
		{"gap", func(b *Bounds) { b.BucketEnds[2] = b.BucketEnds[2].Add(-time.Hour) }},
		{"overlap", func(b *Bounds) { b.BucketEnds[2] = b.BucketEnds[2].Add(time.Hour) }},
		{"wrong final end", func(b *Bounds) { b.BucketEnds[len(b.BucketEnds)-1] = now.Add(time.Minute) }},
		{"early final end", func(b *Bounds) { b.BucketEnds[len(b.BucketEnds)-1] = now.Add(-time.Minute) }},
		{"wrong first start", func(b *Bounds) { b.BucketStarts[0] = b.BucketStarts[0].Add(time.Hour) }},
		{"duplicate start", func(b *Bounds) { b.BucketStarts[3] = b.BucketStarts[2] }},
		{"decreasing end", func(b *Bounds) { b.BucketEnds[3] = b.BucketEnds[1]; b.BucketStarts[4] = b.BucketEnds[3] }},
		{"end before start", func(b *Bounds) { b.BucketEnds[0] = b.BucketStarts[0].Add(-time.Hour) }},
		{"length mismatch", func(b *Bounds) { b.BucketEnds = b.BucketEnds[:len(b.BucketEnds)-1] }},
	}
	for _, c := range cases {
		b := good()
		c.mutate(&b)
		if err := ValidateBuckets(b); !errors.Is(err, ErrBucketTiling) {
			t.Errorf("%s: accepted (err=%v)", c.name, err)
		}
	}
}
