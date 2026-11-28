// Package analyticsprecompute schedules the background builds of the stored analytics documents
// (issue #4771). The sweeper calls Scheduler.Pass every tick; Pass does no I/O and starts at most
// one build goroutine, which reads the shared status, takes the single-flight advisory lock,
// builds on one snapshot and publishes the generation.
//
// Failure handling: a failed build is recorded on the shared state row (so every replica backs
// off together) and delays this process's next attempt. Two outcomes are not recorded as failures:
// an invalidation (a delete committed mid-build; the data was fine, only stale) and an unresolved
// publish (the commit may have succeeded; the next tick decides from the stored status).
// Recording a failure is best effort: if it cannot be written (the database is unreachable) only
// the local backoff applies.
//
// With several sections the scheduler works them in order; one section's failure does not stop the
// next, and the next attempt waits for the latest delay any section asked for. Only one section
// exists today.
package analyticsprecompute

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/store"
)

const (
	defaultBuildBound          = 10 * time.Minute
	failureBackoffBase         = 5 * time.Minute
	invalidationBackoffBase    = time.Minute
	backoffCap                 = time.Hour
	closeTimeout               = 10 * time.Second
	recordFailureTimeout       = 5 * time.Second
	phaseStatus                = "status"
	phaseOpen                  = "open"
	phaseRecheck               = "recheck"
	phaseBuild                 = "build"
	phasePublish               = "publish"
	lockBusyRetry              = time.Minute
	analyticsPrecomputeLogName = "analytics precompute"
)

// Section is one stored set: its name, its variant keys and the builder of the whole generation.
type Section struct {
	Name  string
	Keys  []string
	Build func(ctx context.Context, s *store.AnalyticsSnapshot, now time.Time) ([]store.AnalyticsPrecomputedDoc, error)
}

// session is the part of *store.AnalyticsPrecomputeSession a build uses.
type session interface {
	RunBuild(ctx context.Context, fn func(ctx context.Context, s *store.AnalyticsSnapshot, counter int64) error) error
	Publish(ctx context.Context, section string, counter int64, generatedAt time.Time, docs []store.AnalyticsPrecomputedDoc) error
	Status(ctx context.Context, section string, keys []string) (store.AnalyticsPrecomputeState, error)
	Close(ctx context.Context) (bool, error)
}

type deps struct {
	open          func(ctx context.Context) (s session, acquired bool, err error)
	status        func(ctx context.Context, section string, keys []string) (store.AnalyticsPrecomputeState, error)
	recordFailure func(ctx context.Context, at time.Time) error
	clock         func() time.Time
}

// Scheduler decides when to build. Construct it with New, call Start with the process context,
// register Pass with the sweeper, and call Wait at shutdown after the sweeper has stopped.
type Scheduler struct {
	interval   time.Duration
	sections   []Section
	d          deps
	buildBound time.Duration

	mu            sync.Mutex
	base          context.Context
	running       bool
	nextAttempt   time.Time
	failures      int
	invalidations int
	checks        int64
	wg            sync.WaitGroup
}

// Checks returns how many background checks Pass has started. It is an observability counter:
// Pass always returns 0, so it is the only way to tell that a pass launched a check (which may
// then decide that no build is wanted).
func (s *Scheduler) Checks() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.checks
}

// New returns a Scheduler on pool that keeps each section's stored set younger than interval.
func New(pool *pgxpool.Pool, interval time.Duration, sections []Section) *Scheduler {
	return NewWithClock(pool, interval, sections, time.Now)
}

// NewWithClock is New with an injected clock, which decides freshness, the backoffs and the
// generated_at stamp. Tests against a real database use it to move time without sleeping.
func NewWithClock(pool *pgxpool.Pool, interval time.Duration, sections []Section, clock func() time.Time) *Scheduler {
	return newScheduler(interval, sections, deps{
		open: func(ctx context.Context) (session, bool, error) {
			s, ok, err := store.OpenAnalyticsPrecomputeSession(ctx, pool)
			if err != nil || !ok {
				return nil, false, err
			}
			return s, true, nil
		},
		status: func(ctx context.Context, section string, keys []string) (store.AnalyticsPrecomputeState, error) {
			return store.ReadAnalyticsPrecomputeStatus(ctx, pool, section, keys)
		},
		recordFailure: func(ctx context.Context, at time.Time) error {
			return store.RecordAnalyticsPrecomputeFailure(ctx, pool, at)
		},
		clock: clock,
	})
}

func newScheduler(interval time.Duration, sections []Section, d deps) *Scheduler {
	return &Scheduler{interval: interval, sections: sections, d: d, buildBound: defaultBuildBound, base: context.Background()}
}

// Start sets the context every build derives from: cancelling it (process shutdown) cancels a
// running build and stops Pass from starting new ones. Call it before registering Pass.
func (s *Scheduler) Start(ctx context.Context) {
	s.mu.Lock()
	s.base = ctx
	s.mu.Unlock()
}

// Wait blocks until the build goroutine, if any, has exited.
func (s *Scheduler) Wait() { s.wg.Wait() }

// Pass is the sweeper pass. It does no I/O: when no build is running, the process context is live
// and the local backoff has passed, it starts one build goroutine. It always returns 0: the sweeper
// logs only rows touched; the build logs its own outcome.
func (s *Scheduler) Pass(_ context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.base.Err() != nil || s.running || s.d.clock().Before(s.nextAttempt) {
		return 0, nil
	}
	s.running = true
	s.checks++
	s.wg.Add(1)
	go s.run(s.base)
	return 0, nil
}

type sectionOutcome struct {
	failed      bool      // recorded failure (counts toward the backoff)
	unresolved  bool      // publish commit result unknown: local backoff only
	invalidated bool      // a delete committed mid-build: short local retry
	busy        bool      // another replica holds the build lock: local retry after lockBusyRetry
	deferUntil  time.Time // shared backoff seen: do not look again before this
	at          time.Time // when the outcome was observed
}

func (s *Scheduler) run(base context.Context) {
	defer func() {
		s.mu.Lock()
		s.running = false
		s.mu.Unlock()
		s.wg.Done()
	}()
	ctx, cancel := context.WithTimeout(base, s.buildBound)
	defer cancel()

	var failed, invalidated, busy bool
	var failedAt, invalidatedAt, busyAt, until time.Time
	for _, sec := range s.sections {
		o := s.runSection(ctx, base, sec)
		switch {
		case o.failed || o.unresolved:
			failed, failedAt = true, o.at
		case o.invalidated:
			invalidated, invalidatedAt = true, o.at
		case o.busy:
			busy, busyAt = true, o.at
		}
		if o.deferUntil.After(until) {
			until = o.deferUntil
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	next := until
	switch {
	case failed:
		s.failures++
		if t := failedAt.Add(backoff(failureBackoffBase, s.failures)); t.After(next) {
			next = t
		}
	case invalidated:
		s.invalidations++
		if t := invalidatedAt.Add(backoff(invalidationBackoffBase, s.invalidations)); t.After(next) {
			next = t
		}
	case busy:
		if t := busyAt.Add(lockBusyRetry); t.After(next) {
			next = t
		}
	default:
		s.failures, s.invalidations = 0, 0
	}
	s.nextAttempt = next
}

// backoff is base doubled n-1 times, capped; it never overflows.
func backoff(base time.Duration, n int) time.Duration {
	d := base
	for i := 1; i < n && d < backoffCap; i++ {
		d *= 2
	}
	return min(d, backoffCap)
}

// due reports whether a build is wanted: a key is missing, or the oldest stored document is at
// least one interval old.
func (s *Scheduler) due(st store.AnalyticsPrecomputeState, sec Section, now time.Time) bool {
	return st.Present < int64(len(sec.Keys)) || st.OldestGeneratedAt.IsZero() || !now.Before(st.OldestGeneratedAt.Add(s.interval))
}

// backedOff reports the instant until which the shared failure status forbids a build, or the
// zero time when it does not.
func backedOff(st store.AnalyticsPrecomputeState, now time.Time) time.Time {
	if st.FailureCount <= 0 || st.LastFailureAt.IsZero() {
		return time.Time{}
	}
	if until := st.LastFailureAt.Add(backoff(failureBackoffBase, int(st.FailureCount))); now.Before(until) {
		return until
	}
	return time.Time{}
}

// runSection works one section. base is the process context; ctx is base bounded by buildBound.
func (s *Scheduler) runSection(ctx, base context.Context, sec Section) (out sectionOutcome) {
	phase := phaseStatus
	panicked := false
	fail := func(err error) {
		out = sectionOutcome{failed: true, at: s.d.clock()}
		s.logFailure(base, sec.Name, phase, err, panicked)
		s.recordFailure(base, out.at, err)
	}
	defer func() {
		if r := recover(); r != nil {
			panicked = true
			fail(fmt.Errorf("panic: %v", r))
		}
	}()

	st, err := s.d.status(ctx, sec.Name, sec.Keys)
	if err != nil {
		fail(err)
		return
	}
	if skip, until := s.skip(st, sec); skip {
		return sectionOutcome{deferUntil: until}
	}

	phase = phaseOpen
	sess, acquired, err := s.d.open(ctx)
	if err != nil {
		fail(err)
		return
	}
	if !acquired {
		return sectionOutcome{busy: true, at: s.d.clock()}
	}
	defer func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), closeTimeout)
		defer cancel()
		if unlocked, cerr := sess.Close(cctx); cerr != nil || !unlocked {
			slog.Warn(analyticsPrecomputeLogName+": close session", "section", sec.Name, "unlocked", unlocked, "error", cerr)
		}
	}()

	phase = phaseRecheck
	st, err = sess.Status(ctx, sec.Name, sec.Keys)
	if err != nil {
		fail(err)
		return
	}
	if skip, until := s.skip(st, sec); skip {
		return sectionOutcome{deferUntil: until}
	}

	phase = phaseBuild
	started := s.d.clock()
	generatedAt := started.UTC().Truncate(time.Microsecond)
	var docs []store.AnalyticsPrecomputedDoc
	var counter int64
	err = sess.RunBuild(ctx, func(bctx context.Context, snap *store.AnalyticsSnapshot, c int64) error {
		counter = c
		var berr error
		docs, berr = sec.Build(bctx, snap, generatedAt)
		return berr
	})
	if err != nil {
		fail(err)
		return
	}

	phase = phasePublish
	err = sess.Publish(ctx, sec.Name, counter, generatedAt, docs)
	switch {
	case err == nil:
		slog.Info(analyticsPrecomputeLogName+": published", "section", sec.Name, "generated_at", generatedAt,
			"variants", len(docs), "duration", s.d.clock().Sub(started))
		return sectionOutcome{}
	case errors.Is(err, store.ErrAnalyticsPrecomputedInvalidated):
		slog.Info(analyticsPrecomputeLogName+": build invalidated by a delete, will retry", "section", sec.Name)
		return sectionOutcome{invalidated: true, at: s.d.clock()}
	case errors.Is(err, store.ErrAnalyticsPrecomputePublishUnresolved):
		slog.Error(analyticsPrecomputeLogName+": publish result unknown", "section", sec.Name, "error", err)
		return sectionOutcome{unresolved: true, at: s.d.clock()}
	default:
		fail(err)
		return
	}
}

// skip reports whether the status says no build is wanted now, and until when the shared backoff
// (if that is the reason) forbids one.
func (s *Scheduler) skip(st store.AnalyticsPrecomputeState, sec Section) (bool, time.Time) {
	now := s.d.clock()
	if !s.due(st, sec, now) {
		return true, time.Time{}
	}
	if until := backedOff(st, now); !until.IsZero() {
		return true, until
	}
	return false, time.Time{}
}

// logFailure logs a failed section. A process shutdown (base cancelled) is expected, not an error.
func (s *Scheduler) logFailure(base context.Context, section, phase string, err error, panicked bool) {
	if base.Err() != nil {
		slog.Info(analyticsPrecomputeLogName+": build cancelled by shutdown", "section", section, "phase", phase, "panic", panicked, "error", err)
		return
	}
	slog.Error(analyticsPrecomputeLogName+": build failed", "section", section, "phase", phase, "panic", panicked, "error", err)
}

// recordFailure writes the failure on a fresh pooled connection under its own deadline. A process
// shutdown (base cancelled) is not a build failure and records nothing.
func (s *Scheduler) recordFailure(base context.Context, at time.Time, cause error) {
	if base.Err() != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(base), recordFailureTimeout)
	defer cancel()
	if err := s.d.recordFailure(ctx, at); err != nil {
		slog.Error(analyticsPrecomputeLogName+": record failure", "error", err, "build_error", cause)
	}
}
