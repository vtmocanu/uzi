package releasecheck

import (
	"context"
	"log/slog"
	"time"
)

// releaseCheckRunnerFloor is the fallback cadence for non-positive intervals.
// settings.Cache separately floors stored values at one minute.
const releaseCheckRunnerFloor = time.Hour

// flooredInterval substitutes an hour for non-positive readings to prevent a busy
// loop. Every positive interval passes through unchanged; settings.Cache applies
// its own one-minute floor to stored values.
func flooredInterval(d time.Duration) time.Duration {
	if d <= 0 {
		return releaseCheckRunnerFloor
	}
	return d
}

// updateChecker is the check seam the Runner drives, satisfied by *Reconciler. It is
// an interface so a unit test can inject a counting or panicking fake without a live
// HTTP endpoint.
type updateChecker interface {
	CheckForUpdate(ctx context.Context) (Result, error)
}

// Runner schedules upstream-release checks. The initial wait uses the persisted
// checked-at timestamp with a positive boot delay, so an overdue check runs soon
// without blocking startup. Later waits use the full interval after each attempt.
// CheckForUpdate owns the enable gate and short-circuits disabled checks without
// egress; tick recovers panics and logs errors.
type Runner struct {
	check     updateChecker
	settings  SettingsReader
	logger    *slog.Logger
	now       func() time.Time
	bootDelay time.Duration
	wait      func(context.Context, time.Duration) bool
}

// NewRunner builds the interval trigger around a Reconciler.
func NewRunner(rec *Reconciler, set SettingsReader, logger *slog.Logger) *Runner {
	if logger == nil {
		logger = slog.Default()
	}
	return &Runner{check: rec, settings: set, logger: logger, now: time.Now, bootDelay: time.Minute, wait: waitForReleaseCheck}
}

// Start reads checked-at exactly once. After each completed attempt, including a
// disabled, failed or panicking check, it rereads the interval and waits it in full.
// Cancellation interrupts either wait and is passed through to the check.
func (rn *Runner) Start(ctx context.Context) {
	wait := rn.wait
	if wait == nil {
		wait = waitForReleaseCheck
	}
	now := rn.now
	if now == nil {
		now = time.Now
	}
	bootDelay := rn.bootDelay
	if bootDelay <= 0 {
		bootDelay = time.Minute
	}
	configured, _ := rn.settings.ReleaseCheckInterval(ctx)
	interval := flooredInterval(configured)
	checkedAt, err := rn.settings.ReleaseCheckedAt(ctx)
	delay := bootDelay
	if checked, parseErr := time.Parse(time.RFC3339, checkedAt); err == nil && parseErr == nil {
		age := now().Sub(checked)
		if age < 0 {
			delay = interval
		} else if age < interval {
			delay = max(interval-age, bootDelay)
		}
	}
	for {
		if !wait(ctx, delay) || ctx.Err() != nil {
			return
		}
		rn.tick(ctx)
		if ctx.Err() != nil {
			return
		}
		configured, _ = rn.settings.ReleaseCheckInterval(ctx)
		delay = flooredInterval(configured)
	}
}

// waitForReleaseCheck reports whether the timer elapsed before cancellation.
func waitForReleaseCheck(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return ctx.Err() == nil
	}
}

// tick runs one check with a panic guard so a bug in the fetch/parse/derive path can
// never take down the api process. CheckForUpdate records every failure in its Result
// (and returns a nil error today), so tick logs the recorded status/message rather than
// treating a "disabled"/"error" outcome as fatal. A token is never logged.
func (rn *Runner) tick(ctx context.Context) {
	logger := rn.logger
	if logger == nil {
		logger = slog.Default()
	}
	defer func() {
		if p := recover(); p != nil {
			logger.Error("releasecheck: check panic recovered", "panic", p)
		}
	}()
	res, err := rn.check.CheckForUpdate(ctx)
	if err != nil {
		logger.Error("releasecheck: check", "error", err)
		return
	}
	switch res.Status {
	case statusOK:
		logger.Info("releasecheck: checked", "status", res.Status, "latest_tag", res.Facts.LatestTag)
	case statusError:
		logger.Warn("releasecheck: check reported error", "message", res.Message)
	default:
		logger.Debug("releasecheck: checked", "status", res.Status)
	}
}
