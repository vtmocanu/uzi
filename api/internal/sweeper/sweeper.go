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
	// beat is the admin-health loop-beat callback (PRD #1484 M2): main.go injects a
	// func(){ registry.Beat("sweeper") } so the `loops` health check can see this loop is
	// still ticking. Optional and nil-safe. A plain func — this package must NOT import
	// healthsvc.
	beat func()
}

// New constructs an Engine. A non-positive interval falls back to the default.
// Extra passes run on every tick alongside the run-liveness sweep.
func New(svc Sweeper, interval time.Duration, passes ...Pass) *Engine {
	if interval <= 0 {
		interval = defaultInterval
	}
	return &Engine{svc: svc, interval: interval, passes: passes}
}

// SetBeat wires the admin-health loop-beat callback (PRD #1484 M2). Call once at startup,
// before Run. A nil beat (the default) disables it, so tests that never wire one behave
// exactly as before. The callback fires once per Run tick (not on the Boot pass).
func (e *Engine) SetBeat(beat func()) { e.beat = beat }

// Interval reports the sweep cadence the loop actually ticks at (after New's clamp), so
// main.go registers the loop-beat with the effective interval (PRD #1484 M2).
func (e *Engine) Interval() time.Duration { return e.interval }

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
			if e.beat != nil {
				e.beat() // admin-health loop-beat: this loop ticked (PRD #1484 M2)
			}
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
	if res.WorkersOffline+res.ClaimedReset+res.WallParkRequested+res.WallParked+res.StaleFailed+res.StaleRequeued+res.ChatIdleCompleted+res.ProposalsRecovered+res.HealthChanged+res.AutoStopped+res.LimitPromoted+res.PoolResumed+res.LimitReevaluated+res.RecoveryPromoted+res.CompletionBudgetExhausted+res.CustodyReleased+res.RecoveryStalled+res.RecoveryExpired+res.TaskUndispatchedFailed+res.CodexRefreshRecovered+res.CodexAccountParked+res.CodexAccountPromoted+res.CodexAccountFailed > 0 {
		slog.Info("sweeper pass",
			"workers_offline", res.WorkersOffline,
			"claimed_reset", res.ClaimedReset,
			// PRD #1497 M1: the wall no longer fails a run — it parks it. wall_park_requested counts
			// runs asked to self-park (live capable worker); wall_parked counts runs parked
			// server-side (dead/incapable/unresponsive worker). Both in the sum above and emitted
			// here, so a park-only tick still raises this line.
			"wall_park_requested", res.WallParkRequested,
			"wall_parked", res.WallParked,
			"stale_failed", res.StaleFailed,
			"stale_requeued", res.StaleRequeued,
			"chat_idle_completed", res.ChatIdleCompleted,
			"proposals_recovered", res.ProposalsRecovered,
			"health_changed", res.HealthChanged,
			// PRD #108 M5. In the sum above as well as emitted here: a field documented
			// as observability that nothing observes is the wrong shape for an M7
			// milestone, and without it an auto-stop does not raise this line at all.
			"auto_stopped", res.AutoStopped,
			// PRD #35: same reasoning — in the sum above as well as emitted here, so a
			// tick that only promotes a run out of limit_wait (retry_not_before elapsed)
			// still raises this line rather than resuming a held run invisibly.
			"limit_promoted", res.LimitPromoted,
			// PRD #754 M5: same reasoning — in the sum above as well as emitted here, so a
			// resume-only tick (nothing else changed) still raises this line rather than
			// resuming a held run invisibly.
			"pool_resumed", res.PoolResumed,
			// PRD #1247 M3 (D8): same reasoning — in the sum above as well as emitted here, so a
			// re-eval-only tick (a still-parked run LOWERED to now() because its auto next claim
			// gained a spendable pooled alternative) still raises this line. Its own promotion
			// usually lands a tick later — the D8 pass writes DB now(), a hair ahead of the sweep's
			// captured now — so without this the early promotion is otherwise invisible.
			"limit_reevaluated", res.LimitReevaluated,
			// issue #1197: same reasoning — in the sum above as well as emitted here, so a
			// tick that only promotes a run out of recovery_wait (recovery_retry_not_before
			// elapsed) still raises this line rather than resuming a held run invisibly.
			"recovery_promoted", res.RecoveryPromoted,
			// PRD #1226 M4 (D3): same reasoning — in the sum above as well as emitted here, so a
			// tick that only arms the served budget_exhausted steer (a spared post-attempt
			// live-worker run past its wall) still raises this line rather than steering invisibly.
			"completion_budget_exhausted", res.CompletionBudgetExhausted,
			// PRD #1296 M4 (D3): same reasoning — in the sum above as well as emitted here, so a
			// tick that only releases a stuck custody hold (a completed run whose best-effort
			// terminal release failed) still raises this line rather than unblocking teardown
			// invisibly.
			"custody_released", res.CustodyReleased,
			// PRD #1296 D3/D4: same reasoning — in the sum above as well as emitted here, so a
			// tick that only flips a stalled durable-archive upload to needs_action (past the
			// UZI_RECOVERY_UPLOAD_RETRY_WINDOW, source retained) still raises this line rather
			// than surfacing the needs_action transition invisibly.
			"recovery_stalled", res.RecoveryStalled,
			// PRD #1296 D4: same reasoning — in the sum above as well as emitted here, so a tick
			// that only expires a ready durable-archive capture past its retention (flipping it
			// to expired and reclaiming its bytes) still raises this line rather than reclaiming
			// storage invisibly.
			"recovery_expired", res.RecoveryExpired,
			// issue #1367: same reasoning — in the sum above as well as emitted here, so a
			// tick that only reaps undispatched handoff runs (a kind='task' run left queued
			// with dispatched_at NULL past DispatchGrace) still raises this line rather than
			// failing an orphaned reservation invisibly.
			"task_undispatched_failed", res.TaskUndispatchedFailed,
			// issue #1532: same reasoning — in the sum above as well as emitted here, so a
			// tick that only reaps a wedged Codex account (survivor pass) still raises this
			// line rather than recovering an account invisibly.
			"codex_refresh_recovered", res.CodexRefreshRecovered,
			// PRD #1590 M2: same reasoning, so a tick that only parks queued Codex runs on an
			// unavailable account still raises this line.
			"codex_account_parked", res.CodexAccountParked,
			// PRD #1590 M3: likewise, so a tick that only resumes held Codex runs is visible.
			"codex_account_promoted", res.CodexAccountPromoted,
			// PRD #1590 M4: re-admissions are a subset of the promotions above (so not summed);
			// a terminal failure of a held run is summed so it is never silent.
			"codex_account_readmitted", res.CodexAccountReadmitted,
			"codex_account_failed", res.CodexAccountFailed,
		)
	}
}
