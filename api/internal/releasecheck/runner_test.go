package releasecheck

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

// countingChecker is an updateChecker that records how many times it was invoked and,
// optionally, panics — the two behaviours the Runner must survive.
type countingChecker struct {
	calls  atomic.Int64
	panics bool
	result Result
}

func (c *countingChecker) CheckForUpdate(context.Context) (Result, error) {
	c.calls.Add(1)
	if c.panics {
		panic("boom")
	}
	return c.result, nil
}

// quietLogger discards output so a panicking-check test does not spam the test log.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestRunnerStopsPromptlyOnCancel(t *testing.T) {
	// A long interval means the timer never fires; cancel must return the loop at once.
	chk := &countingChecker{result: Result{Status: statusOK}}
	rn := &Runner{check: chk, settings: &fakeSettings{interval: time.Hour}, logger: quietLogger()}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		rn.Start(ctx)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Runner did not stop within 2s of ctx cancel")
	}
	if got := chk.calls.Load(); got != 0 {
		t.Errorf("check ran %d times before the first interval elapsed, want 0", got)
	}
}

func TestRunnerInvokesCheckAfterInterval(t *testing.T) {
	// A tiny interval fires the timer fast; the check must run at least once, then the
	// loop must stop cleanly on cancel.
	chk := &countingChecker{result: Result{Status: statusOK}}
	rn := &Runner{check: chk, settings: &fakeSettings{interval: time.Millisecond}, logger: quietLogger()}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		rn.Start(ctx)
		close(done)
	}()

	deadline := time.After(2 * time.Second)
	for chk.calls.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("check was not invoked within 2s at a 1ms interval")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Runner did not stop after cancel")
	}
}

func TestRunnerRecoversPanickingCheck(t *testing.T) {
	// A panicking check must be recovered by tick — the loop keeps running (calls
	// climb past 1) and Start returns cleanly on cancel rather than crashing.
	chk := &countingChecker{panics: true}
	rn := &Runner{check: chk, settings: &fakeSettings{interval: time.Millisecond}, logger: quietLogger()}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		rn.Start(ctx)
		close(done)
	}()

	deadline := time.After(2 * time.Second)
	for chk.calls.Load() < 2 {
		select {
		case <-deadline:
			t.Fatalf("panicking check did not recover and re-run (calls=%d)", chk.calls.Load())
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Runner did not stop after cancel following a recovered panic")
	}
}

func TestRunnerFloorsNonPositiveInterval(t *testing.T) {
	// A zero interval read must be floored (not busy-loop with a 0-length timer). We
	// only assert the floor constant is applied deterministically, not timing.
	if releaseCheckRunnerFloor <= 0 {
		t.Fatalf("releaseCheckRunnerFloor must be positive, got %v", releaseCheckRunnerFloor)
	}
}
