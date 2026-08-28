// Package sweeper runs the run-liveness watchdog: on a fixed interval it invokes
// workersvc.Sweep, which enforces the timeouts and worker-loss recovery the
// workers themselves cannot be trusted to report. It is the sibling of the
// poller — a thin goroutine over a service method, so the run-lifecycle rules
// stay in workersvc and this file only owns scheduling.
package sweeper

import (
	"context"
	"log/slog"
	"time"

	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// defaultInterval is the fallback sweep cadence used when New is given a
// non-positive interval. The cadence is operator-tunable via SWEEP_INTERVAL
// (PRD #97 M6 / #100), which config.Load parses and main.go passes here;
// unset/0 falls back to this value. It only bounds how long a dead worker's
// runs sit before recovery, and it should stay well under the heartbeat-stale
// window (45s) so a stale worker is caught promptly — not clamped, matching the
// lenient-parse treatment of the sibling worker-liveness knobs.
const defaultInterval = 15 * time.Second

// Sweeper is the narrow behavior the engine needs; *workersvc.Service satisfies
// it.
type Sweeper interface {
	Sweep(ctx context.Context) (workersvc.SweepResult, error)
}

// Pass is an extra sweep the engine runs on the same tick as the run-liveness
// one, reporting how many rows it touched. It exists so a periodic cleanup that
// is NOT about run lifecycle (PRD #58's pending-join-token expiry) can ride this
// ticker instead of spawning a goroutine of its own, and without pulling its
// concern into workersvc, which owns runs and should not learn about hosted
// workers.
//
// A failing pass is logged and does not abort the others: these are independent
// cleanups, and one erroring is no reason to skip run recovery.
type Pass struct {
	// Name labels the pass in logs (e.g. "hosted_tokens_expired").
	Name string
	Run  func(ctx context.Context) (int64, error)
}

// Engine periodically sweeps.
type Engine struct {
	svc      Sweeper
	interval time.Duration
	passes   []Pass
}

// New constructs an Engine. A non-positive interval falls back to the default.
// Extra passes run on every tick alongside the run-liveness sweep.
func New(svc Sweeper, interval time.Duration, passes ...Pass) *Engine {
	if interval <= 0 {
		interval = defaultInterval
	}
	return &Engine{svc: svc, interval: interval, passes: passes}
}

// Boot runs one immediate sweep — the orphan sweep on API boot (bottega). It
// recovers runs left non-terminal by workers that died while the API was down
// (their heartbeats are already stale) before the ticker's first interval
// elapses. A failure is logged, not fatal: the next tick retries.
func (e *Engine) Boot(ctx context.Context) {
	e.runOnce(ctx)
}

// Run blocks until ctx is cancelled, sweeping every interval.
func (e *Engine) Run(ctx context.Context) {
	ticker := time.NewTicker(e.interval)
	defer ticker.Stop()
	slog.Info("sweeper started", "interval", e.interval.String())
	for {
		select {
		case <-ctx.Done():
			slog.Info("sweeper stopped")
			return
		case <-ticker.C:
			e.runOnce(ctx)
		}
	}
}

func (e *Engine) runOnce(ctx context.Context) {
	// Extra passes run first and independently of the run-liveness sweep's outcome:
	// they are unrelated cleanups, and a DB blip in one must not silently skip the
	// other (PRD #58's token expiry is an at-rest secret bound — it should not be
	// hostage to a failure in run recovery).
	for _, p := range e.passes {
		n, err := p.Run(ctx)
		if err != nil {
			slog.Error("sweeper: pass failed", "pass", p.Name, "error", err)
			continue
		}
		// Quiet on an idle system, like the run-liveness pass below.
		if n > 0 {
			slog.Info("sweeper extra pass", "pass", p.Name, "rows", n)
		}
	}

	res, err := e.svc.Sweep(ctx)
	if err != nil {
		slog.Error("sweeper: sweep failed", "error", err)
		return
	}
	// Only log when the pass actually did something, to keep the log quiet on an
	// idle system.
	if res.WorkersOffline+res.ClaimedReset+res.RunningTimeout+res.StaleFailed+res.StaleRequeued+res.ChatIdleCompleted+res.ProposalsRecovered+res.HealthChanged+res.AutoStopped+res.PoolResumed > 0 {
		slog.Info("sweeper pass",
			"workers_offline", res.WorkersOffline,
			"claimed_reset", res.ClaimedReset,
			"running_timeout", res.RunningTimeout,
			"stale_failed", res.StaleFailed,
			"stale_requeued", res.StaleRequeued,
			"chat_idle_completed", res.ChatIdleCompleted,
			"proposals_recovered", res.ProposalsRecovered,
			"health_changed", res.HealthChanged,
			// PRD #108 M5. In the sum above as well as emitted here: a field documented
			// as observability that nothing observes is the wrong shape for an M7
			// milestone, and without it an auto-stop does not raise this line at all.
			"auto_stopped", res.AutoStopped,
			// PRD #754 M5: same reasoning — in the sum above as well as emitted here, so a
			// resume-only tick (nothing else changed) still raises this line rather than
			// resuming a held run invisibly.
			"pool_resumed", res.PoolResumed,
		)
	}
}
