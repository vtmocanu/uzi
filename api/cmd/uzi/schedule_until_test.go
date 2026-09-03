package main

import (
	"testing"
	"time"
)

// mustLoc loads an IANA zone or fails the test (tzdata must be present, as it is in CI).
func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("LoadLocation(%q): %v", name, err)
	}
	return loc
}

// TestResolveUntil covers every accepted --until form with a FIXED now/loc, so the
// table is machine-independent (the resolver never reads time.Now()/time.Local).
func TestResolveUntil(t *testing.T) {
	utc := time.UTC
	// A fixed reference: 2026-09-02 is a Wednesday; 14:00 UTC.
	now := time.Date(2026, 9, 2, 14, 0, 0, 0, utc)

	cases := []struct {
		name       string
		input      string
		now        time.Time
		loc        *time.Location
		want       time.Time // ignored when indefinite
		indefinite bool
	}{
		{
			name:  "rfc3339 as-is",
			input: "2026-09-04T09:00:00Z",
			now:   now, loc: utc,
			want: time.Date(2026, 9, 4, 9, 0, 0, 0, utc),
		},
		{
			name:  "duration hours",
			input: "24h",
			now:   now, loc: utc,
			want: now.Add(24 * time.Hour),
		},
		{
			name:  "duration compound",
			input: "12h30m",
			now:   now, loc: utc,
			want: now.Add(12*time.Hour + 30*time.Minute),
		},
		{
			name:  "tomorrow bare defaults 09:00",
			input: "tomorrow",
			now:   now, loc: utc,
			want: time.Date(2026, 9, 3, 9, 0, 0, 0, utc),
		},
		{
			name:  "tomorrow with time",
			input: "tomorrow 14:30",
			now:   now, loc: utc,
			want: time.Date(2026, 9, 3, 14, 30, 0, 0, utc),
		},
		{
			name:  "weekday full name default time",
			input: "monday",
			now:   now, loc: utc, // next Monday after Wed 09-02 is 09-07
			want: time.Date(2026, 9, 7, 9, 0, 0, 0, utc),
		},
		{
			name:  "weekday three-letter mixed case",
			input: "Mon",
			now:   now, loc: utc,
			want: time.Date(2026, 9, 7, 9, 0, 0, 0, utc),
		},
		{
			name:  "weekday today time still ahead is today",
			input: "wednesday 18:00", // now is Wed 14:00, 18:00 still ahead
			now:   now, loc: utc,
			want: time.Date(2026, 9, 2, 18, 0, 0, 0, utc),
		},
		{
			name:  "weekday today time already passed rolls +7d",
			input: "wednesday 09:00", // now is Wed 14:00, 09:00 already passed
			now:   now, loc: utc,
			want: time.Date(2026, 9, 9, 9, 0, 0, 0, utc),
		},
		{
			name:  "never is indefinite",
			input: "never",
			now:   now, loc: utc,
			indefinite: true,
		},
		{
			name:  "never case-insensitive",
			input: "Never",
			now:   now, loc: utc,
			indefinite: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, indefinite, err := resolveUntil(tc.input, tc.now, tc.loc)
			if err != nil {
				t.Fatalf("resolveUntil(%q) error: %v", tc.input, err)
			}
			if indefinite != tc.indefinite {
				t.Fatalf("indefinite = %v, want %v", indefinite, tc.indefinite)
			}
			if tc.indefinite {
				if !got.IsZero() {
					t.Errorf("indefinite result should be zero time, got %v", got)
				}
				return
			}
			if !got.Equal(tc.want) {
				t.Errorf("resolveUntil(%q) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}

// TestResolveUntilSpringForwardGap: a non-existent local wall time on a spring-forward
// day normalizes forward exactly the way Go's time.Date does. 2026-03-08 02:30 in
// America/New_York falls in the DST gap; the resolver must return the same instant
// time.Date yields (NOT a hand-rolled UTC-offset arithmetic).
func TestResolveUntilSpringForwardGap(t *testing.T) {
	ny := mustLoc(t, "America/New_York")
	// now is the Saturday before; tomorrow = 2026-03-08 (the spring-forward Sunday).
	now := time.Date(2026, 3, 7, 12, 0, 0, 0, ny)
	got, indefinite, err := resolveUntil("tomorrow 02:30", now, ny)
	if err != nil || indefinite {
		t.Fatalf("resolveUntil error=%v indefinite=%v", err, indefinite)
	}
	want := time.Date(2026, 3, 8, 2, 30, 0, 0, ny)
	if !got.Equal(want) {
		t.Fatalf("gap-day result = %v, want the Go-normalized %v", got, want)
	}
	// Observable normalization: the gap wall time 02:30 is not real, so Go renders the
	// same instant as 01:30 EST (offset -5, before the 02:00→03:00 jump).
	if got.Hour() != 1 || got.Minute() != 30 {
		t.Errorf("normalized wall clock = %02d:%02d, want 01:30 (Go's gap normalization)", got.Hour(), got.Minute())
	}
}

// TestResolveUntilFallBackOverlap: a fall-back overlap day (01:30 occurs twice) resolves
// to Go's choice, machine-independently. 2026-11-01 01:30 in America/New_York.
func TestResolveUntilFallBackOverlap(t *testing.T) {
	ny := mustLoc(t, "America/New_York")
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, ny)
	got, indefinite, err := resolveUntil("tomorrow 01:30", now, ny)
	if err != nil || indefinite {
		t.Fatalf("resolveUntil error=%v indefinite=%v", err, indefinite)
	}
	want := time.Date(2026, 11, 1, 1, 30, 0, 0, ny)
	if !got.Equal(want) {
		t.Fatalf("overlap-day result = %v, want the Go-normalized %v", got, want)
	}
	if got.Hour() != 1 || got.Minute() != 30 {
		t.Errorf("overlap wall clock = %02d:%02d, want 01:30", got.Hour(), got.Minute())
	}
}

// TestResolveUntilErrors: unrecognized and malformed values are errors, not silent
// defaults.
func TestResolveUntilErrors(t *testing.T) {
	now := time.Date(2026, 9, 2, 14, 0, 0, 0, time.UTC)
	for _, in := range []string{"", "someday", "tomorrow 25:00", "monday 09:99", "monday tuesday", "notaday 09:00"} {
		if _, _, err := resolveUntil(in, now, time.UTC); err == nil {
			t.Errorf("resolveUntil(%q) = nil error, want an error", in)
		}
	}
}

// TestResolveUntilRejectsNonPositiveDuration: a zero or negative duration is a usage
// error the CLI names itself, never a past instant round-tripped to the server's 422.
func TestResolveUntilRejectsNonPositiveDuration(t *testing.T) {
	now := time.Date(2026, 9, 2, 14, 0, 0, 0, time.UTC)
	for _, in := range []string{"-1h", "0s", "-30m"} {
		if _, _, err := resolveUntil(in, now, time.UTC); err == nil {
			t.Errorf("resolveUntil(%q) = nil error, want a positive-duration usage error", in)
		}
	}
	if _, _, err := resolveUntil("1h", now, time.UTC); err != nil {
		t.Errorf("resolveUntil(1h) control errored: %v", err)
	}
}
