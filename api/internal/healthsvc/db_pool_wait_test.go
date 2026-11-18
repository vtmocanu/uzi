package healthsvc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/dbdiskfull"
)

// poolFixture is a db check wired to a mutable fake probe.
type poolFixture struct {
	svc *Service
	mu  sync.Mutex
	st  dbStat
}

func newPoolFixture() *poolFixture {
	f := &poolFixture{st: dbStat{pingDur: time.Millisecond, acquiredConns: 7, maxConns: 8, schemaAtHead: true}}
	f.svc = newSvc(&fakeStore{}, &fakeSettings{})
	f.svc.probeDB = func(context.Context) dbStat {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.st
	}
	return f
}

func (f *poolFixture) set(fn func(*dbStat)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(&f.st)
}

func (f *poolFixture) check(now time.Time) apitypes.HealthCheckDTO {
	return f.svc.checkDB(context.Background(), now)
}

func poolEvidenceValue(c apitypes.HealthCheckDTO, label string) (string, bool) {
	for _, e := range c.Evidence {
		if e.Label == label {
			return e.Value, true
		}
	}
	return "", false
}

func wantEvidence(t *testing.T, c apitypes.HealthCheckDTO, label, want string) {
	t.Helper()
	got, ok := poolEvidenceValue(c, label)
	if !ok || got != want {
		t.Fatalf("evidence %q = %q (present %v), want %q; all %+v", label, got, ok, want, c.Evidence)
	}
}

func TestDBPoolWaitConstants(t *testing.T) {
	if dbPoolWaitWindow%dbPoolWaitBudget != 0 {
		t.Fatalf("window %v is not a multiple of budget %v", dbPoolWaitWindow, dbPoolWaitBudget)
	}
	if dbPoolWaitRatio != 240 {
		t.Fatalf("ratio = %d, want 240", dbPoolWaitRatio)
	}
}

// 7 of 8 connections in use with no waits is healthy: occupancy alone never warns.
func TestDBPoolOccupancyWithoutWaitsRegression(t *testing.T) {
	f := newPoolFixture()
	t0 := fixedNow
	f.check(t0)
	c := f.check(t0.Add(dbPoolWaitWindow))
	if c.Severity != sevOK {
		t.Fatalf("severity = %q (%q), want ok", c.Severity, c.Summary)
	}
	wantEvidence(t, c, "In use", "7 of 8")
	wantEvidence(t, c, "Waits", "0")
}

func TestDBPoolFirstEvaluationCollecting(t *testing.T) {
	f := newPoolFixture()
	c := f.check(fixedNow)
	if c.Severity != sevOK {
		t.Fatalf("severity = %q", c.Severity)
	}
	wantEvidence(t, c, "In use", "7 of 8")
	wantEvidence(t, c, "Wait window", "collecting (first window not complete)")
	if _, ok := poolEvidenceValue(c, "Waits"); ok {
		t.Fatalf("unexpected Waits row: %+v", c.Evidence)
	}
}

func TestDBPoolWaitWindowThreshold(t *testing.T) {
	run := func(wait time.Duration) apitypes.HealthCheckDTO {
		f := newPoolFixture()
		f.check(fixedNow)
		f.set(func(s *dbStat) { s.emptyAcquireCount = 5; s.emptyAcquireWait = wait })
		return f.check(fixedNow.Add(60 * time.Second))
	}
	c := run(300 * time.Millisecond)
	if c.Severity != sevWarn {
		t.Fatalf("300ms: severity = %q (%q)", c.Severity, c.Summary)
	}
	for _, sub := range []string{"5 waits", "300ms", "60s"} {
		if !strings.Contains(c.Summary, sub) {
			t.Fatalf("summary %q lacks %q", c.Summary, sub)
		}
	}
	if c.Action == nil || !strings.Contains(*c.Action, "DB_MAX_CONNS") {
		t.Fatalf("action = %v", c.Action)
	}
	wantEvidence(t, c, "Waits", "5")
	wantEvidence(t, c, "Wait time", "300ms")
	wantEvidence(t, c, "Interval", "60s")
	if c := run(100 * time.Millisecond); c.Severity != sevOK {
		t.Fatalf("100ms: severity = %q (%q)", c.Severity, c.Summary)
	}
	// The budget is inclusive: exactly 250ms over 60s warns, 249ms does not.
	if c := run(250 * time.Millisecond); c.Severity != sevWarn {
		t.Fatalf("250ms: severity = %q (%q), want warn at the boundary", c.Severity, c.Summary)
	}
	if c := run(249 * time.Millisecond); c.Severity != sevOK {
		t.Fatalf("249ms: severity = %q (%q), want ok below the boundary", c.Severity, c.Summary)
	}
}

func TestDBPoolWaitBudgetScalesWithElapsed(t *testing.T) {
	f := newPoolFixture()
	f.check(fixedNow)
	f.set(func(s *dbStat) { s.emptyAcquireCount = 9; s.emptyAcquireWait = 600 * time.Millisecond })
	c := f.check(fixedNow.Add(180 * time.Second))
	if c.Severity != sevOK {
		t.Fatalf("severity = %q (%q)", c.Severity, c.Summary)
	}
	wantEvidence(t, c, "Interval", "180s")

	g := newPoolFixture()
	g.check(fixedNow)
	g.set(func(s *dbStat) { s.emptyAcquireCount = 9; s.emptyAcquireWait = 800 * time.Millisecond })
	c = g.check(fixedNow.Add(180 * time.Second))
	if c.Severity != sevWarn || !strings.Contains(c.Summary, "180s") {
		t.Fatalf("severity = %q (%q)", c.Severity, c.Summary)
	}
}

func TestDBPoolVerdictCachedInsideWindow(t *testing.T) {
	f := newPoolFixture()
	t0 := fixedNow
	f.check(t0)
	f.set(func(s *dbStat) { s.emptyAcquireCount = 5; s.emptyAcquireWait = 300 * time.Millisecond })
	first := f.check(t0.Add(60 * time.Second))
	if first.Severity != sevWarn {
		t.Fatalf("setup: %q", first.Severity)
	}
	// Counters move, but nothing is consumed inside the window.
	f.set(func(s *dbStat) { s.emptyAcquireCount = 50; s.emptyAcquireWait = 5 * time.Second })
	for _, d := range []time.Duration{10, 30, 59} {
		c := f.check(t0.Add(60*time.Second + d*time.Second))
		if c.Severity != first.Severity || c.Summary != first.Summary {
			t.Fatalf("+%ds: %q / %q differs from cached %q / %q", d, c.Severity, c.Summary, first.Severity, first.Summary)
		}
	}
	// The next window measures from the reset baseline, so the counters accumulated while
	// cached count here: 45 waits / 4700ms.
	c := f.check(t0.Add(120 * time.Second))
	if c.Severity != sevWarn || !strings.Contains(c.Summary, "45 waits") || !strings.Contains(c.Summary, "4700ms") {
		t.Fatalf("next window: %q / %q", c.Severity, c.Summary)
	}
}

func TestDBPoolVerdictChangesOnlyAfterNextWindow(t *testing.T) {
	f := newPoolFixture()
	t0 := fixedNow
	f.check(t0)
	f.set(func(s *dbStat) { s.emptyAcquireCount = 5; s.emptyAcquireWait = 300 * time.Millisecond })
	if c := f.check(t0.Add(60 * time.Second)); c.Severity != sevWarn {
		t.Fatalf("warn window: %q", c.Severity)
	}
	// Quiet from here on: no further waits.
	if c := f.check(t0.Add(119 * time.Second)); c.Severity != sevWarn {
		t.Fatalf("still inside cached window: %q", c.Severity)
	}
	if c := f.check(t0.Add(120 * time.Second)); c.Severity != sevOK {
		t.Fatalf("quiet window: %q (%q)", c.Severity, c.Summary)
	}
}

func TestDBPoolConcurrentEvaluationsInsideWindowAgree(t *testing.T) {
	f := newPoolFixture()
	t0 := fixedNow
	f.check(t0)
	// The first window (t0..+60s) is quiet; the waits land in the second window, and every
	// goroutine evaluates exactly at its boundary, so they race to compute and cache it.
	if c := f.check(t0.Add(60 * time.Second)); c.Severity != sevOK {
		t.Fatalf("quiet first window: severity = %q (%q)", c.Severity, c.Summary)
	}
	f.set(func(s *dbStat) { s.emptyAcquireCount = 5; s.emptyAcquireWait = 300 * time.Millisecond })
	now := t0.Add(120 * time.Second)
	const n = 16
	out := make([]apitypes.HealthCheckDTO, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = f.check(now)
		}()
	}
	wg.Wait()
	for i := 1; i < n; i++ {
		if out[i].Severity != out[0].Severity || out[i].Summary != out[0].Summary {
			t.Fatalf("goroutine %d: %q / %q vs %q / %q", i, out[i].Severity, out[i].Summary, out[0].Severity, out[0].Summary)
		}
	}
	if out[0].Severity != sevWarn {
		t.Fatalf("severity = %q", out[0].Severity)
	}
	wantEvidence(t, out[0], "Waits", "5")
	wantEvidence(t, out[0], "Interval", "60s")
	// Exactly one goroutine consumed the delta: later waits inside the new window do not
	// change the cached verdict.
	f.set(func(s *dbStat) { s.emptyAcquireCount = 50; s.emptyAcquireWait = 5 * time.Second })
	later := f.check(t0.Add(150 * time.Second))
	if later.Summary != out[0].Summary {
		t.Fatalf("+150s summary %q, want cached %q", later.Summary, out[0].Summary)
	}
}

func TestDBPoolCancellationsNeverWarnAlone(t *testing.T) {
	f := newPoolFixture()
	f.check(fixedNow)
	f.set(func(s *dbStat) { s.canceledAcquireCount = 40 })
	c := f.check(fixedNow.Add(60 * time.Second))
	if c.Severity != sevOK {
		t.Fatalf("severity = %q (%q)", c.Severity, c.Summary)
	}
	wantEvidence(t, c, "Cancelled acquires", "40")
	wantEvidence(t, c, "Waits", "0")
}

func TestDBPoolCounterResetClampsToZero(t *testing.T) {
	f := newPoolFixture()
	f.set(func(s *dbStat) { s.emptyAcquireCount = 100; s.emptyAcquireWait = 10 * time.Second })
	f.check(fixedNow)
	f.set(func(s *dbStat) { s.emptyAcquireCount = 1; s.emptyAcquireWait = time.Millisecond })
	c := f.check(fixedNow.Add(60 * time.Second))
	if c.Severity != sevOK {
		t.Fatalf("severity = %q (%q)", c.Severity, c.Summary)
	}
	wantEvidence(t, c, "Waits", "0")
	wantEvidence(t, c, "Wait time", "0ms")
}

func TestDBPoolWaitSaturatingMultiply(t *testing.T) {
	f := newPoolFixture()
	f.check(fixedNow)
	f.set(func(s *dbStat) { s.emptyAcquireCount = 1; s.emptyAcquireWait = time.Duration(1 << 62) })
	if c := f.check(fixedNow.Add(60 * time.Second)); c.Severity != sevWarn {
		t.Fatalf("severity = %q", c.Severity)
	}
}

// A cached warn verdict never masks the higher-precedence db conditions.
func TestDBPoolWarnDoesNotMaskPrecedence(t *testing.T) {
	setup := func() (*poolFixture, time.Time) {
		f := newPoolFixture()
		f.check(fixedNow)
		f.set(func(s *dbStat) { s.emptyAcquireCount = 5; s.emptyAcquireWait = 300 * time.Millisecond })
		now := fixedNow.Add(60 * time.Second)
		if c := f.check(now); c.Severity != sevWarn {
			t.Fatalf("setup: %q", c.Severity)
		}
		return f, now.Add(10 * time.Second)
	}
	t.Run("slow ping", func(t *testing.T) {
		f, now := setup()
		f.set(func(s *dbStat) { s.pingDur = 400 * time.Millisecond })
		if c := f.check(now); c.Severity != sevWarn || !strings.Contains(c.Summary, "ping is slow") {
			t.Fatalf("%q / %q", c.Severity, c.Summary)
		}
	})
	t.Run("ping error", func(t *testing.T) {
		f, now := setup()
		f.set(func(s *dbStat) { s.pingErr = errors.New("boom") })
		if c := f.check(now); c.Severity != sevDanger || !strings.Contains(c.Summary, "ping failed") {
			t.Fatalf("%q / %q", c.Severity, c.Summary)
		}
	})
	t.Run("schema mismatch", func(t *testing.T) {
		f, now := setup()
		f.set(func(s *dbStat) { s.schemaAtHead = false; s.schemaApplied = 1; s.schemaHead = 2 })
		if c := f.check(now); c.Severity != sevDanger || !strings.Contains(c.Summary, "differs from the embedded head") {
			t.Fatalf("%q / %q", c.Severity, c.Summary)
		}
	})
	t.Run("schema read error", func(t *testing.T) {
		f, now := setup()
		f.set(func(s *dbStat) { s.schemaAtHead = false; s.schemaErr = errors.New("no goose") })
		if c := f.check(now); c.Severity != sevDanger || !strings.Contains(c.Summary, "Could not read the applied migration") {
			t.Fatalf("%q / %q", c.Severity, c.Summary)
		}
	})
	t.Run("disk full", func(t *testing.T) {
		f, now := setup()
		sig := dbdiskfull.New(func() time.Time { return now })
		sig.Observe(fmt.Errorf("write: %w", &pgconn.PgError{Code: "53100"}))
		f.svc.cfg.DiskFull = sig
		if c := f.check(now); c.Severity != sevDanger || c.Summary != emergencyDiskFullLine {
			t.Fatalf("%q / %q", c.Severity, c.Summary)
		}
	})
}
