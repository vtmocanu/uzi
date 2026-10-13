package releasecheck

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type checkerFunc func(context.Context) (Result, error)

func (f checkerFunc) CheckForUpdate(ctx context.Context) (Result, error) { return f(ctx) }

// Each request blocks until explicitly released or cancelled. Timeouts only bound
// broken synchronization; they never drive the simulated schedule.
type waitRequest struct {
	delay   time.Duration
	release chan struct{}
}
type manualWait struct{ requests chan waitRequest }

func newManualWait() *manualWait { return &manualWait{requests: make(chan waitRequest)} }
func (w *manualWait) wait(ctx context.Context, d time.Duration) bool {
	req := waitRequest{delay: d, release: make(chan struct{})}
	select {
	case w.requests <- req:
	case <-ctx.Done():
		return false
	}
	select {
	case <-req.release:
		return ctx.Err() == nil
	case <-ctx.Done():
		return false
	}
}
func (w *manualWait) next(t *testing.T) waitRequest {
	t.Helper()
	select {
	case req := <-w.requests:
		return req
	case <-time.After(2 * time.Second):
		t.Fatal("Runner did not request a wait")
		return waitRequest{}
	}
}
func startRunner(t *testing.T, rn *Runner) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); rn.Start(ctx) }()
	stop := func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("Runner did not stop after cancellation")
		}
	}
	t.Cleanup(stop)
	return stop
}

func TestRunnerOverdueFirstWait(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	w := newManualWait()
	set := &fakeSettings{interval: 6 * time.Hour, checkedAt: now.Add(-7 * time.Hour).Format(time.RFC3339)}
	rn := &Runner{settings: set, logger: quietLogger(), now: func() time.Time { return now }, wait: w.wait}
	startRunner(t, rn)
	if got := w.next(t).delay; got != time.Minute {
		t.Fatalf("overdue first requested wait = %v, want 1m0s", got)
	}
}
