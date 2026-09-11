package sweeper

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

type fakeSweeper struct {
	calls atomic.Int64
	err   error
	res   workersvc.SweepResult
}

func (f *fakeSweeper) Sweep(context.Context) (workersvc.SweepResult, error) {
	f.calls.Add(1)
	return f.res, f.err
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

// recordingHandler is a minimal slog.Handler that captures every record so a test
// can assert on the structured log the sweeper emits.
type recordingHandler struct {
	mu      *sync.Mutex
	records *[]slog.Record
}

func (h recordingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	*h.records = append(*h.records, r.Clone())
	return nil
}

func (h recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h recordingHandler) WithGroup(string) slog.Handler      { return h }

func findRecord(recs []slog.Record, msg string) *slog.Record {
	for i := range recs {
		if recs[i].Message == msg {
			return &recs[i]
		}
	}
	return nil
}

func attrInt(r *slog.Record, key string) (int64, bool) {
	var (
		out   int64
		found bool
	)
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			out, found = a.Value.Int64(), true
			return false
		}
		return true
	})
	return out, found
}

// A tick that ONLY promotes a run out of limit_wait or recovery_wait is still a
// resume the operator log must show. The guard and the emit list must therefore
// include LimitPromoted and RecoveryPromoted, exactly as they include PoolResumed
// (sweeper.go documents this invariant for pool_resumed at the emit site). A
// resume-only tick that raised no "sweeper pass" line would resume a held run
// invisibly. Note: this test swaps slog.SetDefault (process-global), so it must not
// run in parallel with other tests that touch the default logger.
func TestSweeperPassLogsResumeOnlyTicks(t *testing.T) {
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })

	cases := []struct {
		name string
		res  workersvc.SweepResult
		attr string // "" => expect NO "sweeper pass" record
	}{
		{name: "limit_wait promotion alone", res: workersvc.SweepResult{LimitPromoted: 1}, attr: "limit_promoted"},
		{name: "recovery_wait promotion alone", res: workersvc.SweepResult{RecoveryPromoted: 1}, attr: "recovery_promoted"},
		// PRD #1226 M4 (D3): a tick that ONLY arms the served budget_exhausted steer must raise the
		// line too — the guard sum AND emit list both include CompletionBudgetExhausted, else a
		// steer-only tick logs nothing and the steer is invisible.
		{name: "completion budget exhausted alone", res: workersvc.SweepResult{CompletionBudgetExhausted: 1}, attr: "completion_budget_exhausted"},
		{name: "idle tick logs nothing", res: workersvc.SweepResult{}, attr: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var (
				mu   sync.Mutex
				recs []slog.Record
			)
			slog.SetDefault(slog.New(recordingHandler{mu: &mu, records: &recs}))

			New(&fakeSweeper{res: tc.res}, 0).Boot(context.Background())

			rec := findRecord(recs, "sweeper pass")
			if tc.attr == "" {
				if rec != nil {
					t.Fatal(`an idle sweep result raised a "sweeper pass" line; want none`)
				}
				return
			}
			if rec == nil {
				t.Fatalf("a resume-only tick (%s) raised no \"sweeper pass\" line — the run was resumed invisibly", tc.name)
			}
			got, ok := attrInt(rec, tc.attr)
			if !ok {
				t.Fatalf("%q attribute missing from the \"sweeper pass\" line (emit list not updated?)", tc.attr)
			}
			if got != 1 {
				t.Fatalf("%q = %d, want 1", tc.attr, got)
			}
		})
	}
}
