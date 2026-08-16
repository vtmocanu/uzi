package sweeper

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

type fakeSweeper struct {
	calls atomic.Int64
	err   error
}

func (f *fakeSweeper) Sweep(context.Context) (workersvc.SweepResult, error) {
	f.calls.Add(1)
	return workersvc.SweepResult{}, f.err
}

// Boot runs the extra passes alongside the run-liveness sweep. This is what makes
// PRD #58's pending-token expiry ride the existing ticker instead of a goroutine
// of its own.
func TestBootRunsExtraPasses(t *testing.T) {
	var ran int64
	sw := &fakeSweeper{}
	New(sw, 0, Pass{Name: "hosted_tokens_expired", Run: func(context.Context) (int64, error) {
		ran++
		return 2, nil
	}}).Boot(context.Background())

	if ran != 1 {
		t.Fatalf("extra pass ran %d times, want 1", ran)
	}
	if sw.calls.Load() != 1 {
		t.Fatalf("run-liveness sweep ran %d times, want 1", sw.calls.Load())
	}
}

// The passes are independent cleanups. A failing extra pass must not skip the run
// sweep (an expiry DB blip should never hold up worker-loss recovery), and a
// failing run sweep must not skip the expiry (an at-rest secret bound should never
// be hostage to run recovery).
func TestPassAndSweepFailuresAreIndependent(t *testing.T) {
	boom := errors.New("db exploded")

	t.Run("failing pass still sweeps", func(t *testing.T) {
		sw := &fakeSweeper{}
		New(sw, 0, Pass{Name: "boom", Run: func(context.Context) (int64, error) {
			return 0, boom
		}}).Boot(context.Background())
		if sw.calls.Load() != 1 {
			t.Fatal("a failing extra pass skipped the run-liveness sweep")
		}
	})

	t.Run("failing sweep still runs the pass", func(t *testing.T) {
		var ran int64
		New(&fakeSweeper{err: boom}, 0, Pass{Name: "expiry", Run: func(context.Context) (int64, error) {
			ran++
			return 0, nil
		}}).Boot(context.Background())
		if ran != 1 {
			t.Fatal("a failing run-liveness sweep skipped the extra pass")
		}
	})
}

// No passes is the pre-PRD-58 shape and must stay valid (variadic, so existing
// callers are untouched).
func TestEngineWithNoPasses(t *testing.T) {
	sw := &fakeSweeper{}
	New(sw, 0).Boot(context.Background())
	if sw.calls.Load() != 1 {
		t.Fatalf("sweep ran %d times, want 1", sw.calls.Load())
	}
}
