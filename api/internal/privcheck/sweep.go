package privcheck

import (
	"context"
	"log/slog"
	"time"
)

// Sweeper is the narrow behavior the Engine schedules; *Service satisfies it.
type Sweeper interface {
	CheckAllConnections(ctx context.Context) (SweepResult, error)
}

// Engine periodically re-runs the full privilege check for every connection. It
// is modeled on the worker sweeper, not the poller: a Boot pass runs immediately
// so pre-existing/never-checked connections get a report right after deploy
// (grandfathered over-privileged tokens surface at boot, not one interval
// later), then it ticks on the interval. Single-instance, like the poller and
// worker sweeper — a multi-replica deploy needs leader election for all three.
type Engine struct {
	svc      Sweeper
	interval time.Duration
}

// NewEngine constructs a privilege-sweep Engine. The caller is responsible for
// only starting it when the interval is positive (0 disables the sweep).
func NewEngine(svc Sweeper, interval time.Duration) *Engine {
	return &Engine{svc: svc, interval: interval}
}

// Boot runs one immediate sweep at API start, back-filling reports for
// connections that predate this feature (NULL status) before the first tick.
func (e *Engine) Boot(ctx context.Context) {
	e.runOnce(ctx)
}

// Run blocks until ctx is cancelled, sweeping every interval.
func (e *Engine) Run(ctx context.Context) {
	ticker := time.NewTicker(e.interval)
	defer ticker.Stop()
	slog.Info("privilege sweeper started", "interval", e.interval.String())
	for {
		select {
		case <-ctx.Done():
			slog.Info("privilege sweeper stopped")
			return
		case <-ticker.C:
			e.runOnce(ctx)
		}
	}
}

func (e *Engine) runOnce(ctx context.Context) {
	res, err := e.svc.CheckAllConnections(ctx)
	if err != nil {
		slog.Error("privilege sweeper: pass failed", "error", err)
		return
	}
	if res.Checked+res.Errors > 0 {
		slog.Info("privilege sweeper pass",
			"checked", res.Checked,
			"ok", res.OK,
			"warnings", res.Warnings,
			"violations", res.Violations,
			"report_errors", res.ReportErrors,
			"errors", res.Errors,
		)
	}
}
