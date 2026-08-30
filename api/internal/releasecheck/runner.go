package releasecheck

import (
	"context"
	"log/slog"
	"time"
)

// releaseCheckRunnerFloor is the minimum interval the Runner will ever sleep. It
// guards against a non-positive or otherwise nonsensical value leaking through so a
// bad read can never make the loop hammer github.com. (settings.Cache already floors
// the stored value at 1m; this is the Runner's own belt-and-braces default.)
const releaseCheckRunnerFloor = time.Hour

// updateChecker is the check seam the Runner drives, satisfied by *Reconciler. It is
// an interface so a unit test can inject a counting or panicking fake without a live
// HTTP endpoint.
type updateChecker interface {
	CheckForUpdate(ctx context.Context) (Result, error)
}

// Runner is the interval trigger for the upstream-release check (PRD #836 M2),
// mirroring agentsource.Runner. It sleeps ReleaseCheckInterval() and then runs one
// CheckForUpdate — recovering from any panic and logging (never crashing the process)
// on error. It is boot-safe: main.go starts it as a non-blocking background goroutine
// BEFORE the listener starts, and the first tick only fires after one interval, so a
// first check against an unreachable github.com never delays boot. The master enable
// gate is read INSIDE CheckForUpdate (which short-circuits to "disabled" with no
// egress), so the Runner itself needs only the interval.
type Runner struct {
	check    updateChecker
	settings SettingsReader
	logger   *slog.Logger
}

// NewRunner builds the interval trigger around a Reconciler.
func NewRunner(rec *Reconciler, set SettingsReader, logger *slog.Logger) *Runner {
	if logger == nil {
		logger = slog.Default()
	}
	return &Runner{check: rec, settings: set, logger: logger}
}

// Start loops until ctx is cancelled: read the configured interval (floored), sleep it
// via a timer, then run one panic-recovered check. It never panics the process. On ctx
// cancellation it stops the timer and returns.
func (rn *Runner) Start(ctx context.Context) {
	for {
		interval, _ := rn.settings.ReleaseCheckInterval(ctx)
		if interval <= 0 {
			interval = releaseCheckRunnerFloor
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		rn.tick(ctx)
	}
}

// tick runs one check with a panic guard so a bug in the fetch/parse/derive path can
// never take down the api process. CheckForUpdate records every failure in its Result
// (and returns a nil error today), so tick logs the recorded status/message rather than
// treating a "disabled"/"error" outcome as fatal. A token is never logged.
func (rn *Runner) tick(ctx context.Context) {
	defer func() {
		if p := recover(); p != nil {
			rn.logger.Error("releasecheck: check panic recovered", "panic", p)
		}
	}()
	res, err := rn.check.CheckForUpdate(ctx)
	if err != nil {
		rn.logger.Error("releasecheck: check", "error", err)
		return
	}
	switch res.Status {
	case statusOK:
		rn.logger.Info("releasecheck: checked", "status", res.Status, "latest_tag", res.Facts.LatestTag)
	case statusError:
		rn.logger.Warn("releasecheck: check reported error", "message", res.Message)
	default:
		rn.logger.Debug("releasecheck: checked", "status", res.Status)
	}
}
