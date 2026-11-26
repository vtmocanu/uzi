package analytics

import (
	"errors"
	"fmt"
	"time"
)

// Date is a civil (calendar) date with no zone attached. Date arithmetic is
// pure calendar arithmetic (done in UTC only so Go normalises month lengths).
type Date struct {
	Year  int
	Month time.Month
	Day   int
}

// DateOf returns the local civil date of t in loc.
func DateOf(t time.Time, loc *time.Location) Date {
	y, m, d := t.In(loc).Date()
	return Date{y, m, d}
}

func (d Date) utcMidnight() time.Time {
	return time.Date(d.Year, d.Month, d.Day, 0, 0, 0, 0, time.UTC)
}

func fromUTC(t time.Time) Date {
	y, m, dd := t.Date()
	return Date{y, m, dd}
}

// AddDays returns the date n calendar days after d (n may be negative).
func (d Date) AddDays(n int) Date { return fromUTC(d.utcMidnight().AddDate(0, 0, n)) }

// Before reports whether d is strictly earlier than o.
func (d Date) Before(o Date) bool { return d.utcMidnight().Before(o.utcMidnight()) }

// Weekday returns the day of the week of d.
func (d Date) Weekday() time.Weekday { return d.utcMidnight().Weekday() }

// DaysUntil returns the number of calendar days from d to o.
// It is integer civil arithmetic: a time.Duration saturates at about 292 years.
func (d Date) DaysUntil(o Date) int { return daysFromCivil(o) - daysFromCivil(d) }

// daysFromCivil is the day count of d from 1970-01-01 (proleptic Gregorian, Hinnant's algorithm).
func daysFromCivil(d Date) int {
	y, m := d.Year, int(d.Month)
	if m <= 2 {
		y--
	}
	era := y / 400
	if y < 0 {
		era = (y - 399) / 400
	}
	yoe := y - era*400
	mp := (m + 9) % 12
	doy := (153*mp+2)/5 + d.Day - 1
	doe := yoe*365 + yoe/4 - yoe/100 + doy
	return era*146097 + doe - 719468
}

// String formats d as YYYY-MM-DD.
func (d Date) String() string { return d.utcMidnight().Format("2006-01-02") }

// mondayOf returns the Monday of the ISO week containing d.
func mondayOf(d Date) Date { return d.AddDays(-((int(d.Weekday()) + 6) % 7)) }

var errNoDayStart = errors.New("analytics: no day start found")

// DayStart returns the earliest instant whose local date in loc is >= d
// (#4684 decision D-A). A wholly skipped date therefore has the same start as
// the next date. It is exported so the store tests can cross-check PostgreSQL.
func DayStart(d Date, loc *time.Location) (time.Time, error) {
	return dayStart(d, loc, nil)
}

// zoneOffset returns the UTC offset in seconds at t.
func zoneOffset(t time.Time) int {
	_, o := t.Zone()
	return o
}

// segmentEnd returns the end of the constant-offset segment containing
// cursor (in loc), normalised against hi, and whether the reported bounds are
// usable: the end must be after cursor and the offset at end-1s must equal
// the offset at cursor (the late-end guard). Go's ZoneBounds can report an end
// before the queried instant for far-future years (e.g. America/New_York at
// 2040-12-31T12:00Z), so the bounds cannot be trusted blindly.
func segmentEnd(cursor time.Time, loc *time.Location, hi time.Time) (end time.Time, ok bool) {
	t := cursor.In(loc)
	_, e := t.ZoneBounds()
	if e.IsZero() || e.After(hi) {
		e = hi
	}
	if !e.After(cursor) {
		return e, false
	}
	last := e.Add(-time.Second)
	if last.Before(cursor) {
		last = cursor
	}
	if zoneOffset(last.In(loc)) != zoneOffset(t) {
		return e, false
	}
	return e, true
}

// dayStart implements the bounded traversal. It never builds an instant with
// time.Date or ParseInLocation in loc: ParseInLocation("2011-12-30",
// Pacific/Apia) yields 2011-12-29T10:00Z, which runs backwards. steps, if
// non-nil, counts one-second fallback steps (tests).
func dayStart(d Date, loc *time.Location, steps *int) (time.Time, error) {
	mid := d.utcMidnight()
	lo := mid.Add(-26 * time.Hour)
	hi := d.AddDays(1).utcMidnight().Add(26 * time.Hour)
	cursor := lo
	for cursor.Before(hi) {
		e, ok := segmentEnd(cursor, loc, hi)
		if ok {
			o := zoneOffset(cursor.In(loc))
			cand := mid.Add(-time.Duration(o) * time.Second)
			if cand.Before(cursor) {
				cand = cursor
			}
			if cand.Before(e) {
				return cand, nil
			}
			cursor = e
			continue
		}
		// Invalid bounds: scan whole seconds in order until a date >= d
		// appears or usable bounds return. Offsets and transitions are whole
		// seconds, so the first qualifying second is the earliest instant.
		resumed := false
		for tp := cursor; tp.Before(hi); tp = tp.Add(time.Second) {
			if steps != nil {
				*steps++
			}
			if !DateOf(tp, loc).Before(d) {
				return tp, nil
			}
			if _, ok := segmentEnd(tp, loc, hi); ok {
				cursor = tp
				resumed = true
				break
			}
		}
		if !resumed {
			cursor = hi
		}
	}
	return time.Time{}, errNoDayStart
}

// Window is a requested analytics window.
type Window string

// Supported windows.
const (
	Window7d  Window = "7d"
	Window30d Window = "30d"
	Window90d Window = "90d"
	WindowAll Window = "all"
)

// ErrInvalidWindow is returned by ParseWindow for an unknown value.
var ErrInvalidWindow = errors.New("analytics: invalid window")

// ParseWindow parses a window name; the empty string defaults to 30d.
func ParseWindow(s string) (Window, error) {
	switch Window(s) {
	case "":
		return Window30d, nil
	case Window7d, Window30d, Window90d, WindowAll:
		return Window(s), nil
	}
	return "", fmt.Errorf("%w: %q", ErrInvalidWindow, s)
}

func (w Window) days() int {
	switch w {
	case Window7d:
		return 7
	case Window30d:
		return 30
	case Window90d:
		return 90
	}
	return 0
}

// maxBuckets caps the all-time bucket count.
const maxBuckets = 104

// Bucket is one half-open interval [Start, End) labelled by a civil date.
type Bucket struct {
	Label   Date
	Start   time.Time
	End     time.Time
	Partial bool // true for today's bucket (it ends at now, not at a midnight)
}

// RollingPoint is the 7-day rolling range of one bucket. Start is
// UnclippedStart clipped at HistoryStart; Partial is computed on the
// unclipped range.
type RollingPoint struct {
	Start          time.Time
	End            time.Time
	UnclippedStart time.Time
	Partial        bool
}

// Bounds is the Go-authoritative calendar for one request (decision D-A).
// SQL only does half-open membership against these instants.
type Bounds struct {
	Now   time.Time
	Today Date
	// TodayStart and NextDayStart are dayStart(Today) and dayStart(Today+1):
	// today's natural day, which contains Now.
	TodayStart, NextDayStart time.Time
	From                     time.Time
	PreviousFrom             *time.Time // nil for the all window
	FoldFrom                 time.Time
	// HistoryStart is dayStart(Monday of the ISO week of the earliest run's
	// local date); nil when the scope has no runs (earliest == nil).
	HistoryStart *time.Time
	WidthWeeks   int // 0 for day buckets
	Buckets      []Bucket
	Rolling      []RollingPoint

	BucketStarts, BucketEnds []time.Time
	RollStarts, RollEnds     []time.Time
}

// ComputeBounds computes the buckets, windows and rolling ranges for a
// request. earliest is the scope's earliest run creation instant, or nil for
// an empty scope. An all-window with an empty scope has no buckets and no
// rolling points, and From == FoldFrom == Now (an empty range).
// dayStarter memoises DayStart per date for one ComputeBounds call: the
// traversal is the expensive part and the same dates recur across buckets,
// rolling ranges and the today search.
// dayStartObserver, when non-nil, is called by dayStarter.at with each computed
// date and its fallback step count. Tests only; not safe for parallel tests.
var dayStartObserver func(d Date, steps int)

type dayStarter struct {
	loc  *time.Location
	memo map[Date]time.Time
}

func (s *dayStarter) at(d Date) (time.Time, error) {
	if t, ok := s.memo[d]; ok {
		return t, nil
	}
	var steps int
	t, err := dayStart(d, s.loc, &steps)
	if dayStartObserver != nil {
		dayStartObserver(d, steps)
	}
	if err != nil {
		return time.Time{}, err
	}
	s.memo[d] = t
	return t, nil
}

func ComputeBounds(now time.Time, loc *time.Location, window Window, earliest *time.Time) (Bounds, error) {
	b := Bounds{Now: now}
	ds := &dayStarter{loc: loc, memo: map[Date]time.Time{}}

	// today = max{D : dayStart(D) <= now}, searched around now's local date.
	d0 := DateOf(now, loc)
	found := false
	for _, cand := range []Date{d0.AddDays(1), d0, d0.AddDays(-1), d0.AddDays(-2)} {
		s, err := ds.at(cand)
		if err != nil {
			return Bounds{}, err
		}
		if !s.After(now) {
			b.Today = cand
			found = true
			break
		}
	}
	if !found {
		return Bounds{}, errNoDayStart
	}

	var histDate Date
	if earliest != nil {
		// Clock skew: an earliest run after Today is clamped to Today so no
		// bucket starts after now.
		ed := DateOf(*earliest, loc)
		if b.Today.Before(ed) {
			ed = b.Today
		}
		histDate = mondayOf(ed)
		hs, err := ds.at(histDate)
		if err != nil {
			return Bounds{}, err
		}
		b.HistoryStart = &hs
	}

	var err error
	if b.TodayStart, err = ds.at(b.Today); err != nil {
		return Bounds{}, err
	}
	if b.NextDayStart, err = ds.at(b.Today.AddDays(1)); err != nil {
		return Bounds{}, err
	}
	if window == WindowAll {
		if earliest == nil {
			b.From, b.FoldFrom = now, now
			b.WidthWeeks = 1 // weekly granularity, so the envelope reports 7 days, not the day-window 1
			return b, nil
		}
		b.From, b.FoldFrom = *b.HistoryStart, *b.HistoryStart
		span := histDate.DaysUntil(b.Today)
		w := 1
		for span/(7*w)+1 > maxBuckets {
			w *= 2
		}
		b.WidthWeeks = w
		n := span/(7*w) + 1
		for k := 0; k < n; k++ {
			label := histDate.AddDays(k * w * 7)
			start, err := ds.at(label)
			if err != nil {
				return Bounds{}, err
			}
			last := k == n-1
			end := now
			if !last {
				if end, err = ds.at(histDate.AddDays((k + 1) * w * 7)); err != nil {
					return Bounds{}, err
				}
				if !start.Before(end) {
					continue
				}
			}
			b.Buckets = append(b.Buckets, Bucket{Label: label, Start: start, End: end, Partial: last})
		}
	} else {
		n := window.days()
		if n == 0 {
			return Bounds{}, fmt.Errorf("%w: %q", ErrInvalidWindow, string(window))
		}
		fromDate := b.Today.AddDays(-(n - 1))
		if b.From, err = ds.at(fromDate); err != nil {
			return Bounds{}, err
		}
		pf, err := ds.at(b.Today.AddDays(-(2*n - 1)))
		if err != nil {
			return Bounds{}, err
		}
		b.PreviousFrom, b.FoldFrom = &pf, pf
		for dd := fromDate; !b.Today.Before(dd); dd = dd.AddDays(1) {
			start, err := ds.at(dd)
			if err != nil {
				return Bounds{}, err
			}
			last := dd == b.Today
			end := now
			if !last {
				if end, err = ds.at(dd.AddDays(1)); err != nil {
					return Bounds{}, err
				}
				if !start.Before(end) {
					continue // wholly skipped date: empty bucket omitted
				}
			}
			b.Buckets = append(b.Buckets, Bucket{Label: dd, Start: start, End: end, Partial: last})
		}
	}

	for _, bk := range b.Buckets {
		l := b.Today
		if !bk.Partial {
			if l, err = lastCoveredDate(bk.End, loc, ds); err != nil {
				return Bounds{}, err
			}
		}
		r0, err := ds.at(l.AddDays(-6))
		if err != nil {
			return Bounds{}, err
		}
		rp := RollingPoint{Start: r0, End: bk.End, UnclippedStart: r0, Partial: bk.Partial}
		if b.HistoryStart != nil && r0.Before(*b.HistoryStart) {
			rp.Partial = true
			rp.Start = *b.HistoryStart
			if rp.Start.After(rp.End) {
				rp.Start = rp.End
			}
		}
		b.Rolling = append(b.Rolling, rp)
		b.BucketStarts = append(b.BucketStarts, bk.Start)
		b.BucketEnds = append(b.BucketEnds, bk.End)
		b.RollStarts = append(b.RollStarts, rp.Start)
		b.RollEnds = append(b.RollEnds, rp.End)
	}
	return b, nil
}

// lastCoveredDate returns the unique D with dayStart(D) < e <= dayStart(D+1).
func lastCoveredDate(e time.Time, loc *time.Location, ds *dayStarter) (Date, error) {
	d0 := DateOf(e.Add(-time.Second), loc)
	for _, cand := range []Date{d0, d0.AddDays(-1), d0.AddDays(1), d0.AddDays(-2), d0.AddDays(2)} {
		s, err := ds.at(cand)
		if err != nil {
			return Date{}, err
		}
		n, err := ds.at(cand.AddDays(1))
		if err != nil {
			return Date{}, err
		}
		if s.Before(e) && !e.After(n) {
			return cand, nil
		}
	}
	return Date{}, errNoDayStart
}
