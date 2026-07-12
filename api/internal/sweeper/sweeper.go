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

	"gitlab.example.com/vtmocanu/uzi/api/internal/workersvc"
)

// defaultInterval is the sweep cadence. Not a PRD env var: it only bounds how
// long a dead worker's runs sit before recovery, and it must be well under the
// heartbeat-stale window (45s) so a stale worker is caught promptly.
const defaultInterval = 15 * time.Second

// Sweeper is the narrow behavior the engine needs; *workersvc.Service satisfies
// it.
type Sweeper interface {
	Sweep(ctx context.Context) (workersvc.SweepResult, error)
}

// Engine periodically sweeps.
type Engine struct {
	svc      Sweeper
	interval time.Duration
}

// New constructs an Engine. A non-positive interval falls back to the default.
func New(svc Sweeper, interval time.Duration) *Engine {
	if interval <= 0 {
		interval = defaultInterval
	}
	return &Engine{svc: svc, interval: interval}
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
	res, err := e.svc.Sweep(ctx)
	if err != nil {
		slog.Error("sweeper: sweep failed", "error", err)
		return
	}
	// Only log when the pass actually did something, to keep the log quiet on an
	// idle system.
	if res.WorkersOffline+res.ClaimedReset+res.RunningTimeout+res.StaleFailed+res.StaleRequeued+res.ChatIdleCompleted+res.ProposalsRecovered > 0 {
		slog.Info("sweeper pass",
			"workers_offline", res.WorkersOffline,
			"claimed_reset", res.ClaimedReset,
			"running_timeout", res.RunningTimeout,
			"stale_failed", res.StaleFailed,
			"stale_requeued", res.StaleRequeued,
			"chat_idle_completed", res.ChatIdleCompleted,
			"proposals_recovered", res.ProposalsRecovered,
		)
	}
}
