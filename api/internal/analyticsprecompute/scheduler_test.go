package analyticsprecompute

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/store"
)

var t0 = time.Date(2060, 5, 1, 12, 0, 0, 0, time.UTC)

const interval = 4 * time.Hour

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) set(t time.Time)     { c.mu.Lock(); c.t = t; c.mu.Unlock() }
func (c *fakeClock) add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

// fakeSession records calls; each method's error and the optional hooks are set per test.
type fakeSession struct {
	env       *env
	statusErr error
	recheck   *store.AnalyticsPrecomputeState // what the session's own status read sees, when set
	buildErr  error
	publish   error
	panicIn   bool
	closes    int
	publishes int
	published []store.AnalyticsPrecomputedDoc
	counter   int64
	closeErr  error // ctx.Err() the Close call saw on entry
}

func (s *fakeSession) RunBuild(ctx context.Context, fn func(ctx context.Context, s *store.AnalyticsSnapshot, counter int64) error) error {
	if s.buildErr != nil {
		return s.buildErr
	}
	return fn(ctx, nil, 7)
}

func (s *fakeSession) Publish(_ context.Context, _ string, counter int64, _ time.Time, docs []store.AnalyticsPrecomputedDoc) error {
	s.publishes++
	s.counter = counter
	s.published = docs
	return s.publish
}

func (s *fakeSession) Status(context.Context, string, []string) (store.AnalyticsPrecomputeState, error) {
	if s.statusErr != nil {
		return store.AnalyticsPrecomputeState{}, s.statusErr
	}
	if s.recheck != nil {
		return *s.recheck, nil
	}
	return s.env.state, nil
}

func (s *fakeSession) Close(ctx context.Context) (bool, error) {
	s.closes++
	s.closeErr = ctx.Err()
	return true, nil
}

type env struct {
	clock    fakeClock
	state    store.AnalyticsPrecomputeState
	statusEr error
	openErr  error
	busy     bool
	sess     *fakeSession
	opens    int
	builds   int
	recorded []time.Time
	recErr   error
	statuses atomic.Int64 // calls of the pool status read: one per started build goroutine
	recCtx   error        // ctx.Err() recordFailure saw on entry
	recDl    time.Time    // the deadline recordFailure's ctx carried
	build    func(ctx context.Context) error
}

var keys = []string{"a", "b"}

func newEnv(t *testing.T) (*env, *Scheduler) {
	t.Helper()
	e := &env{}
	e.clock.set(t0)
	e.sess = &fakeSession{env: e}
	sec := Section{Name: "spend", Keys: keys, Build: func(ctx context.Context, _ *store.AnalyticsSnapshot, _ time.Time) ([]store.AnalyticsPrecomputedDoc, error) {
		e.builds++
		if e.sess.panicIn {
			panic("boom")
		}
		if e.build != nil {
			if err := e.build(ctx); err != nil {
				return nil, err
			}
		}
		return []store.AnalyticsPrecomputedDoc{{Key: "a", Document: []byte(`{"a":1}`)}, {Key: "b", Document: []byte(`{"b":1}`)}}, nil
	}}
	s := newScheduler(interval, []Section{sec}, deps{
		open: func(context.Context) (session, bool, error) {
			e.opens++
			if e.openErr != nil {
				return nil, false, e.openErr
			}
			if e.busy {
				return nil, false, nil
			}
			return e.sess, true, nil
		},
		status: func(context.Context, string, []string) (store.AnalyticsPrecomputeState, error) {
			e.statuses.Add(1)
			return e.state, e.statusEr
		},
		recordFailure: func(ctx context.Context, at time.Time) error {
			e.recCtx = ctx.Err()
			e.recDl, _ = ctx.Deadline()
			e.recorded = append(e.recorded, at)
			return e.recErr
		},
		clock: e.clock.now,
	})
	return e, s
}

// cycle runs one Pass, waits for its goroutine and reports whether a build goroutine was started
// (Pass itself always returns 0, so a start is seen as a status read).
func cycle(t *testing.T, s *Scheduler, e *env) bool {
	t.Helper()
	before := e.statuses.Load()
	n, err := s.Pass(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("Pass = %d, want 0", n)
	}
	s.Wait()
	return e.statuses.Load() > before
}

func fresh(at time.Time) store.AnalyticsPrecomputeState {
	return store.AnalyticsPrecomputeState{Present: 2, OldestGeneratedAt: at}
}

func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func TestPassIsNonBlockingAndSingleFlight(t *testing.T) {
	e, s := newEnv(t)
	release, started := make(chan struct{}), make(chan struct{})
	e.build = func(ctx context.Context) error { close(started); <-release; return nil }

	if n, _ := s.Pass(context.Background()); n != 0 {
		t.Fatalf("first Pass = %d, want 0 (starting a check is not a row)", n)
	}
	<-started
	opens, statuses := e.opens, e.statuses.Load()
	if n, _ := s.Pass(context.Background()); n != 0 {
		t.Fatalf("Pass while running = %d, want 0", n)
	}
	if e.opens != opens || e.statuses.Load() != statuses {
		t.Fatal("a second Pass started a second build")
	}
	close(release)
	s.Wait()
	if e.sess.publishes != 1 || e.sess.closes != 1 {
		t.Fatalf("publishes %d closes %d, want 1 and 1", e.sess.publishes, e.sess.closes)
	}
}

func TestShutdownCancelsRunningBuild(t *testing.T) {
	e, s := newEnv(t)
	base, cancel := context.WithCancel(context.Background())
	s.Start(base)
	started := make(chan struct{})
	e.build = func(ctx context.Context) error { close(started); <-ctx.Done(); return ctx.Err() }
	logs := captureLogs(t)
	if _, err := s.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-started
	cancel()
	s.Wait()
	if !strings.Contains(logs.String(), "build cancelled by shutdown") || strings.Contains(logs.String(), "level=ERROR") {
		t.Fatalf("shutdown must log at info, not error: %s", logs)
	}
	if len(e.recorded) != 0 {
		t.Fatalf("a shutdown was recorded as a build failure: %v", e.recorded)
	}
	if e.sess.publishes != 0 || e.sess.closes != 1 {
		t.Fatalf("publishes %d closes %d", e.sess.publishes, e.sess.closes)
	}
	if cycle(t, s, e) {
		t.Fatal("Pass started a build after shutdown")
	}
}

func TestBackoffCountsFromObservedFailureTime(t *testing.T) {
	e, s := newEnv(t)
	logs := captureLogs(t)
	e.build = func(context.Context) error { e.clock.add(11 * time.Minute); return errors.New("slow failure") }
	if !cycle(t, s, e) {
		t.Fatal("no build started")
	}
	failAt := t0.Add(11 * time.Minute)
	if len(e.recorded) != 1 || !e.recorded[0].Equal(failAt) {
		t.Fatalf("recorded %v, want the observed failure time %v", e.recorded, failAt)
	}
	if e.sess.publishes != 0 {
		t.Fatal("a failed build published")
	}
	if !strings.Contains(logs.String(), "analytics precompute: build failed") || !strings.Contains(logs.String(), "phase=build") {
		t.Fatalf("failure log missing: %s", logs)
	}
	e.clock.set(failAt.Add(5*time.Minute - time.Nanosecond))
	if cycle(t, s, e) {
		t.Fatal("Pass before failure+5m started a build")
	}
	e.clock.set(failAt.Add(5 * time.Minute))
	e.build = nil
	if !cycle(t, s, e) {
		t.Fatal("Pass at failure+5m did not start")
	}
}

func TestBackoffDoublesAndCaps(t *testing.T) {
	e, s := newEnv(t)
	e.build = func(context.Context) error { return errors.New("fail") }
	want := []time.Duration{5 * time.Minute, 10 * time.Minute, 20 * time.Minute, 40 * time.Minute, time.Hour, time.Hour}
	for i, d := range want {
		if !cycle(t, s, e) {
			t.Fatalf("attempt %d did not start", i)
		}
		at := e.clock.now()
		e.clock.set(at.Add(d - time.Nanosecond))
		if cycle(t, s, e) {
			t.Fatalf("attempt %d: started %v early", i, d)
		}
		e.clock.set(at.Add(d))
	}
	if got := backoff(failureBackoffBase, 1<<30); got != time.Hour {
		t.Fatalf("backoff(huge) = %v", got)
	}
}

func TestPreCaptureErrorsAreRecordedAndBackedOff(t *testing.T) {
	for name, set := range map[string]func(e *env){
		"open":   func(e *env) { e.openErr = errors.New("no conn") },
		"status": func(e *env) { e.statusEr = errors.New("read status") },
	} {
		t.Run(name, func(t *testing.T) {
			e, s := newEnv(t)
			set(e)
			cycle(t, s, e)
			if len(e.recorded) != 1 || !e.recorded[0].Equal(t0) {
				t.Fatalf("recorded %v, want one at %v", e.recorded, t0)
			}
			if e.builds != 0 {
				t.Fatal("built after a pre-capture error")
			}
			e.clock.add(5*time.Minute - time.Nanosecond)
			if cycle(t, s, e) {
				t.Fatal("not backed off")
			}
			e.clock.add(time.Nanosecond)
			if !cycle(t, s, e) {
				t.Fatal("not retried after the backoff")
			}
			s.Wait()
		})
	}
}

func TestUnresolvedPublishIsNotRecordedAndNextTickDecidesFromStatus(t *testing.T) {
	e, s := newEnv(t)
	logs := captureLogs(t)
	e.sess.publish = fmt.Errorf("%w: lost ack", store.ErrAnalyticsPrecomputePublishUnresolved)
	cycle(t, s, e)
	if len(e.recorded) != 0 {
		t.Fatalf("an unresolved publish was recorded: %v", e.recorded)
	}
	if !strings.Contains(logs.String(), "level=ERROR") {
		t.Fatalf("no error log: %s", logs)
	}
	if e.sess.closes != 1 {
		t.Fatalf("closes = %d", e.sess.closes)
	}
	e.clock.add(5*time.Minute - time.Nanosecond)
	if cycle(t, s, e) {
		t.Fatal("no local backoff after an unresolved publish")
	}
	e.clock.add(time.Nanosecond)
	// The commit had landed: status is fresh, so no build.
	e.state = fresh(e.clock.now())
	builds, opens := e.builds, e.opens
	cycle(t, s, e)
	if e.builds != builds || e.opens != opens {
		t.Fatal("rebuilt although the stored set is fresh")
	}
	// Had it not landed (missing keys), the next tick builds.
	e.state = store.AnalyticsPrecomputeState{}
	e.sess.publish = nil
	cycle(t, s, e)
	if e.builds != builds+1 || e.sess.publishes != 2 {
		t.Fatalf("builds %d publishes %d, want a new build and publish", e.builds, e.sess.publishes)
	}
}

func TestInvalidationRetriesSoonWithoutRecording(t *testing.T) {
	e, s := newEnv(t)
	e.sess.publish = store.ErrAnalyticsPrecomputedInvalidated
	for i, d := range []time.Duration{time.Minute, 2 * time.Minute} {
		if !cycle(t, s, e) {
			t.Fatalf("attempt %d did not start", i)
		}
		at := e.clock.now()
		e.clock.set(at.Add(d - time.Nanosecond))
		if cycle(t, s, e) {
			t.Fatalf("attempt %d: retried before %v", i, d)
		}
		e.clock.set(at.Add(d))
	}
	if len(e.recorded) != 0 {
		t.Fatalf("an invalidation was recorded as a failure: %v", e.recorded)
	}
}

func TestFreshSetIsSkippedWithoutOpening(t *testing.T) {
	e, s := newEnv(t)
	e.state = fresh(t0.Add(-interval + time.Nanosecond))
	cycle(t, s, e)
	if e.opens != 0 || e.builds != 0 {
		t.Fatalf("opens %d builds %d for a fresh set", e.opens, e.builds)
	}
}

func TestBuildTriggers(t *testing.T) {
	for name, st := range map[string]store.AnalyticsPrecomputeState{
		"missing key":      {Present: 1, OldestGeneratedAt: t0},
		"empty":            {},
		"age equals":       fresh(t0.Add(-interval)),
		"age beyond":       fresh(t0.Add(-interval - time.Hour)),
		"present no stamp": {Present: 2},
	} {
		t.Run(name, func(t *testing.T) {
			e, s := newEnv(t)
			e.state = st
			cycle(t, s, e)
			if e.builds != 1 || e.sess.publishes != 1 {
				t.Fatalf("builds %d publishes %d, want 1 and 1", e.builds, e.sess.publishes)
			}
			if e.sess.counter != 7 || len(e.sess.published) != 2 {
				t.Fatalf("published counter %d docs %d", e.sess.counter, len(e.sess.published))
			}
		})
	}
}

func TestLockBusySkipsWithoutRecording(t *testing.T) {
	e, s := newEnv(t)
	e.busy = true
	cycle(t, s, e)
	if e.opens != 1 || e.builds != 0 || len(e.recorded) != 0 {
		t.Fatalf("opens %d builds %d recorded %v", e.opens, e.builds, e.recorded)
	}
	// Another replica is building: wait exactly lockBusyRetry before checking again.
	e.clock.set(t0.Add(time.Minute - time.Nanosecond))
	if cycle(t, s, e) {
		t.Fatal("a busy lock must delay the next attempt by a minute")
	}
	e.clock.set(t0.Add(time.Minute))
	if !cycle(t, s, e) {
		t.Fatal("not retried one minute after a busy lock")
	}
}

func TestSharedBackoffSkipsUntilFiveMinutes(t *testing.T) {
	e, s := newEnv(t)
	e.state = store.AnalyticsPrecomputeState{FailureCount: 1, LastFailureAt: t0}
	e.clock.set(t0.Add(5*time.Minute - time.Nanosecond))
	cycle(t, s, e)
	if e.opens != 0 || len(e.recorded) != 0 {
		t.Fatalf("opens %d recorded %v inside the shared backoff", e.opens, e.recorded)
	}
	// The local state now knows the shared deadline: no goroutine until it passes.
	if cycle(t, s, e) {
		t.Fatal("Pass inside the shared backoff started a goroutine")
	}
	e.clock.add(time.Nanosecond)
	cycle(t, s, e)
	if e.builds != 1 {
		t.Fatalf("builds = %d after the shared backoff", e.builds)
	}
}

func TestSessionRecheckSkipsAndCloses(t *testing.T) {
	e, s := newEnv(t)
	// The pool read says due; the re-check on the locked session sees another replica's publish.
	recheck := fresh(t0)
	e.sess.recheck = &recheck
	cycle(t, s, e)
	if e.builds != 0 || e.sess.closes != 1 {
		t.Fatalf("builds %d closes %d, want a skip and one close", e.builds, e.sess.closes)
	}
}

func TestCloseRunsOnEveryExitAfterAcquisition(t *testing.T) {
	cases := map[string]func(e *env){
		"recheck error": func(e *env) { e.sess.statusErr = errors.New("x") },
		"build error":   func(e *env) { e.sess.buildErr = errors.New("x") },
		"builder error": func(e *env) { e.build = func(context.Context) error { return errors.New("x") } },
		"publish error": func(e *env) { e.sess.publish = errors.New("x") },
		"invalidated":   func(e *env) { e.sess.publish = store.ErrAnalyticsPrecomputedInvalidated },
		"unresolved":    func(e *env) { e.sess.publish = store.ErrAnalyticsPrecomputePublishUnresolved },
		"panic":         func(e *env) { e.sess.panicIn = true },
		"success":       func(e *env) {},
	}
	for name, set := range cases {
		t.Run(name, func(t *testing.T) {
			e, s := newEnv(t)
			set(e)
			cycle(t, s, e)
			if e.sess.closes != 1 {
				t.Fatalf("closes = %d, want 1", e.sess.closes)
			}
		})
	}
}

func TestPanicIsRecoveredAndRecorded(t *testing.T) {
	e, s := newEnv(t)
	logs := captureLogs(t)
	e.sess.panicIn = true
	cycle(t, s, e)
	if len(e.recorded) != 1 || e.sess.publishes != 0 {
		t.Fatalf("recorded %v publishes %d", e.recorded, e.sess.publishes)
	}
	if !strings.Contains(logs.String(), "phase=build") || !strings.Contains(logs.String(), "panic=true") || strings.Contains(logs.String(), "phase=panic") {
		t.Fatalf("panic must log the phase it happened in plus panic=true: %s", logs)
	}
	if cycle(t, s, e) {
		t.Fatal("no backoff after a panic")
	}
}

func TestRecordFailureErrorIsLoggedAndLocalBackoffApplies(t *testing.T) {
	e, s := newEnv(t)
	logs := captureLogs(t)
	e.openErr = errors.New("db down")
	e.recErr = errors.New("cannot record")
	cycle(t, s, e)
	if !strings.Contains(logs.String(), "record failure") {
		t.Fatalf("record failure error not logged: %s", logs)
	}
	if cycle(t, s, e) {
		t.Fatal("no local backoff when recording fails")
	}
}

func TestSuccessResetsBackoff(t *testing.T) {
	e, s := newEnv(t)
	e.sess.buildErr = errors.New("x")
	cycle(t, s, e)
	e.clock.add(5 * time.Minute)
	e.sess.buildErr = nil
	cycle(t, s, e)
	e.state = store.AnalyticsPrecomputeState{}
	e.sess.buildErr = errors.New("x")
	e.clock.add(interval)
	cycle(t, s, e)
	at := e.clock.now()
	e.clock.set(at.Add(5*time.Minute - time.Nanosecond))
	if cycle(t, s, e) {
		t.Fatal("expected the base backoff after a reset")
	}
	e.clock.set(at.Add(5 * time.Minute))
	if !cycle(t, s, e) {
		t.Fatal("backoff did not reset to 5m after a success (it doubled)")
	}
	s.Wait()
}

// runExpiredBuild runs one build whose bound expires while the builder blocks on its context.
func runExpiredBuild(t *testing.T) (*env, *Scheduler, time.Time) {
	t.Helper()
	e, s := newEnv(t)
	s.buildBound = 20 * time.Millisecond
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	e.build = func(ctx context.Context) error {
		e.clock.add(11 * time.Minute)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-stop:
			return errors.New("test ended before the build bound")
		}
	}
	done := make(chan struct{})
	go func() { cycle(t, s, e); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the build was not bounded: Wait did not return")
	}
	return e, s, t0.Add(11 * time.Minute)
}

func TestBuildBoundExpiryRecordsOneFailureOnADetachedContext(t *testing.T) {
	logs := captureLogs(t)
	e, s, failAt := runExpiredBuild(t)
	if len(e.recorded) != 1 || !e.recorded[0].Equal(failAt) {
		t.Fatalf("recorded %v, want one at %v", e.recorded, failAt)
	}
	if !strings.Contains(logs.String(), "phase=build") {
		t.Fatalf("failure not logged in the build phase: %s", logs)
	}
	s.mu.Lock()
	next := s.nextAttempt
	s.mu.Unlock()
	if want := failAt.Add(failureBackoffBase); !next.Equal(want) {
		t.Fatalf("nextAttempt %v, want %v", next, want)
	}
	if e.recCtx != nil {
		t.Fatalf("recordFailure got a done context (%v): it must not inherit the build bound", e.recCtx)
	}
	if left := time.Until(e.recDl); left < 3*time.Second || left > recordFailureTimeout {
		t.Fatalf("recordFailure deadline %v away, want about %v", left, recordFailureTimeout)
	}
}

func TestSessionCloseIsNotCancelledByAnExpiredBuild(t *testing.T) {
	e, _, _ := runExpiredBuild(t)
	if e.sess.closes != 1 {
		t.Fatalf("closes = %d", e.sess.closes)
	}
	if e.sess.closeErr != nil {
		t.Fatalf("Close got a done context (%v): it must outlive the build context", e.sess.closeErr)
	}
}
