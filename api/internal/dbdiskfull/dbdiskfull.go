// Package dbdiskfull detects PostgreSQL "disk full" write failures (SQLSTATE 53100) seen
// on the api's own pool and remembers the most recent sighting, so the admin health
// check can surface a full database volume. It is a leaf package: it imports only
// pgx/pgconn and the standard library.
package dbdiskfull

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Window is how long after the last sighting the signal stays active.
const Window = 5 * time.Minute

// CodeDiskFull is SQLSTATE 53100 (disk_full). Other class 53 codes (53200 out of
// memory, 53300 too many connections, 53400 configuration limit exceeded) are not it.
const CodeDiskFull = "53100"

// Is reports whether err, or any error it wraps, is a PostgreSQL error with SQLSTATE
// exactly 53100.
func Is(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == CodeDiskFull
}

// Signal remembers the last disk-full sighting. The zero value is not usable; build one
// with New. A nil *Signal is safe to use and never active.
type Signal struct {
	now func() time.Time

	mu       sync.Mutex
	lastSeen int64  // unix nanoseconds, the newest sighting; 0 means never seen
	start    int64  // unix nanoseconds of the current incident's first sighting
	gen      uint64 // distinct incidents observed so far
}

// New returns a Signal reading time from now (nil means time.Now).
func New(now func() time.Time) *Signal {
	if now == nil {
		now = time.Now
	}
	return &Signal{now: now}
}

// Observe records err when it is a disk-full error and reports whether it recorded.
// The generation advances when this sighting starts a new incident: no earlier
// sighting, or the earlier one is older than Window. lastSeen never moves backwards,
// and lastSeen, start and generation change together under one lock (Observe runs on
// the error path only).
func (s *Signal) Observe(err error) bool {
	if s == nil || !Is(err) {
		return false
	}
	t := s.now().UnixNano()
	s.mu.Lock()
	defer s.mu.Unlock()
	prev := s.lastSeen
	if prev == 0 || t-prev > int64(Window) {
		s.start = t
		s.gen++
	}
	if t > prev {
		s.lastSeen = t
	}
	return true
}

// Snapshot returns the active state, the newest sighting and the current incident's start
// read under one lock, so start <= last whenever active. last and start are zero when
// nothing was ever seen.
func (s *Signal) Snapshot(now time.Time) (active bool, last, start time.Time) {
	if s == nil {
		return false, time.Time{}, time.Time{}
	}
	s.mu.Lock()
	l, st := s.lastSeen, s.start
	s.mu.Unlock()
	if l == 0 {
		return false, time.Time{}, time.Time{}
	}
	if st == 0 || st > l {
		st = l
	}
	return now.UnixNano()-l <= int64(Window), time.Unix(0, l), time.Unix(0, st)
}

// Generation counts distinct incidents observed so far.
func (s *Signal) Generation() uint64 {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gen
}

// Tracer is a pgx.QueryTracer that feeds query errors to Signal. pgx v5 traces Exec,
// Query and QueryRow (at rows close), and transaction Commit (which runs Exec("commit")),
// so a 53100 raised at commit time is seen too.
//
// SendBatch, CopyFrom and Prepare use separate pgx tracer interfaces and are not traced
// (the api uses none of them today).
//
// Not covered: the goose migration connection (a separate database/sql connection opened
// in the store package's migrate.go), connect-time errors, and pool.Ping.
type Tracer struct {
	Signal *Signal
}

var _ pgx.QueryTracer = (*Tracer)(nil)

// TraceQueryStart returns ctx unchanged.
func (t *Tracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return ctx
}

// TraceQueryEnd records a disk-full error; it never logs or panics and takes only a brief
// lock on the error path.
func (t *Tracer) TraceQueryEnd(_ context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	if t == nil {
		return
	}
	t.Signal.Observe(data.Err)
}
