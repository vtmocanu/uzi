package dbdiskfull

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func pgErr(code string) error { return &pgconn.PgError{Code: code} }

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newClock() *clock { return &clock{t: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)} }

func TestIsAndObserve(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"direct 53100", pgErr("53100"), true},
		{"wrapped 53100", fmt.Errorf("commit: %w", pgErr("53100")), true},
		{"out of memory 53200", pgErr("53200"), false},
		{"too many connections 53300", pgErr("53300"), false},
		{"temp file limit 53400", pgErr("53400"), false},
		{"division by zero 22012", pgErr("22012"), false},
		{"plain error", errors.New("boom"), false},
		{"nil", nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Is(tc.err); got != tc.want {
				t.Fatalf("Is = %v, want %v", got, tc.want)
			}
			c := newClock()
			s := New(c.now)
			if got := s.Observe(tc.err); got != tc.want {
				t.Fatalf("Observe = %v, want %v", got, tc.want)
			}
			if got, _, _ := s.Snapshot(c.now()); got != tc.want {
				t.Fatalf("active = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestActiveClearsAfterWindow(t *testing.T) {
	c := newClock()
	s := New(c.now)
	s.Observe(pgErr(CodeDiskFull))
	if active, _, _ := s.Snapshot(c.t.Add(Window)); !active {
		t.Fatal("want active at the window edge")
	}
	active, last, _ := s.Snapshot(c.t.Add(Window + time.Second))
	if active {
		t.Fatal("want inactive after the window")
	}
	if !last.Equal(c.t) {
		t.Fatalf("last = %v, want %v", last, c.t)
	}
}

func TestGenerationBumpsOnlyAfterQuietGap(t *testing.T) {
	c := newClock()
	s := New(c.now)
	if s.Generation() != 0 {
		t.Fatal("fresh generation must be 0")
	}
	s.Observe(pgErr(CodeDiskFull))
	if s.Generation() != 1 {
		t.Fatalf("gen = %d, want 1", s.Generation())
	}
	for i := 0; i < 5; i++ {
		c.t = c.t.Add(Window - time.Second)
		s.Observe(pgErr(CodeDiskFull))
	}
	if s.Generation() != 1 {
		t.Fatalf("repeated sightings inside the window bumped gen to %d", s.Generation())
	}
	c.t = c.t.Add(Window + time.Second)
	s.Observe(pgErr(CodeDiskFull))
	if s.Generation() != 2 {
		t.Fatalf("gen = %d, want 2 after a quiet gap", s.Generation())
	}
}

func TestNilSignalIsSafe(t *testing.T) {
	var s *Signal
	if s.Observe(pgErr(CodeDiskFull)) || s.Generation() != 0 {
		t.Fatal("nil signal must be inert")
	}
	if active, last, start := s.Snapshot(time.Now()); active || !last.IsZero() || !start.IsZero() {
		t.Fatalf("nil signal snapshot = %v %v %v", active, last, start)
	}
	(&Tracer{}).TraceQueryEnd(context.Background(), nil, pgx.TraceQueryEndData{Err: pgErr(CodeDiskFull)})
}

func TestTracerActivates(t *testing.T) {
	c := newClock()
	s := New(c.now)
	tr := &Tracer{Signal: s}
	ctx := context.Background()
	if got := tr.TraceQueryStart(ctx, nil, pgx.TraceQueryStartData{}); got != ctx {
		t.Fatal("TraceQueryStart must return ctx unchanged")
	}
	tr.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{Err: fmt.Errorf("w: %w", pgErr("53200"))})
	if active, _, _ := s.Snapshot(c.now()); active {
		t.Fatal("53200 must not activate")
	}
	tr.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{Err: fmt.Errorf("w: %w", pgErr(CodeDiskFull))})
	if active, _, _ := s.Snapshot(c.now()); !active {
		t.Fatal("wrapped 53100 must activate")
	}
}

func TestIncidentStartHoldsWithinWindowAndResetsAfterGap(t *testing.T) {
	c := newClock()
	s := New(c.now)
	if _, _, st := s.Snapshot(c.t); !st.IsZero() {
		t.Fatal("no incident start before any sighting")
	}
	first := c.t
	s.Observe(pgErr(CodeDiskFull))
	c.t = c.t.Add(Window - time.Second)
	s.Observe(pgErr(CodeDiskFull))
	if _, last, st := s.Snapshot(c.t); !st.Equal(first) || !last.Equal(c.t) {
		t.Fatalf("start = %v last = %v; want %v and %v", st, last, first, c.t)
	}
	if s.Generation() != 1 {
		t.Fatalf("gen = %d, want 1", s.Generation())
	}
	c.t = c.t.Add(Window + time.Second)
	s.Observe(pgErr(CodeDiskFull))
	if _, _, st := s.Snapshot(c.t); !st.Equal(c.t) {
		t.Fatalf("start = %v, want reset to %v", st, c.t)
	}
	if s.Generation() != 2 {
		t.Fatalf("gen = %d, want 2", s.Generation())
	}
}

func TestSnapshotStartNeverAfterLast(t *testing.T) {
	c := newClock()
	s := New(c.now)
	if active, last, start := s.Snapshot(c.t); active || !last.IsZero() || !start.IsZero() {
		t.Fatalf("empty snapshot = %v %v %v", active, last, start)
	}
	s.Observe(pgErr(CodeDiskFull))
	first := c.t
	// The clock steps backwards for the next sighting; lastSeen must not follow it.
	c.t = first.Add(-time.Second)
	s.Observe(pgErr(CodeDiskFull))
	active, last, start := s.Snapshot(first)
	if !active {
		t.Fatal("want active")
	}
	if !last.Equal(first) {
		t.Errorf("last = %v moved from %v", last, first)
	}
	if start.After(last) {
		t.Errorf("start %v after last %v", start, last)
	}
	var nilSig *Signal
	if a, _, _ := nilSig.Snapshot(first); a {
		t.Error("nil Signal active")
	}
}
