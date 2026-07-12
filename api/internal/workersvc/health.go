package workersvc

// Run-health detector (PRD #47). Called once per sweep tick, it computes a
// non-terminal, self-clearing flag for every run in a flaggable status from
// telemetry already in Postgres, and writes it through the single SetRunHealth
// writer. It NEVER kills a run — the existing RUN_TIMEOUT / idle / iteration caps
// remain the only liveness backstops; this is early warning only. It is also
// deliberately NOT a guardrail: a hostile worker can suppress `stalled` with junk
// messages or evade `looping` by varying inputs, but it cannot forge health state
// (these columns are sweeper-written server-side) or touch another user's runs.

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// Settings is the narrow read surface the health detector needs from the instance
// settings cache (PRD #47 Decision 5). *settings.Cache satisfies it; tests use a
// small fake. Declared here rather than importing settings' concrete type so
// workersvc keeps no dependency on that package.
type Settings interface {
	HealthEnabled(ctx context.Context) (bool, error)
	HealthStallSeconds(ctx context.Context) (int, error)
	HealthSlowSeconds(ctx context.Context) (int, error)
	HealthQueuedSeconds(ctx context.Context) (int, error)
	HealthApprovalSeconds(ctx context.Context) (int, error)
}

// Health flag values. These mirror the CHECK constraint on runs.health; keep them
// in sync with migration 00057.
const (
	healthOK            = "ok"
	healthStalled       = "stalled"
	healthLooping       = "looping"
	healthSlow          = "slow"
	healthWaitingWorker = "waiting_worker"
	healthApprovalIdle  = "approval_idle"
)

// Reason templates. FIXED, server-controlled strings: they never carry a tool name,
// a tool input, or repo content (run_messages are secret-scrubbed by the worker but
// NOT scrubbed of repo content — Decision 4), and never a live duration (the UI
// recomputes elapsed from health_since, so a stored duration would go stale). The
// owner sees these; non-owners never see the reason at all (owner-gated in M3).
const (
	reasonStalled       = "the agent stopped sending updates"
	reasonLooping       = "the agent keeps repeating the same action"
	reasonSlow          = "this run is taking longer than usual"
	reasonApprovalIdle  = "waiting for the plan to be approved"
	reasonVaultLocked   = "your vault is locked, so this run can't start"
	reasonNoWorker      = "no worker is online to pick up this run"
	reasonWaitingWorker = "waiting for a worker to pick up this run"
)

// toolWindowFetch bounds the per-run tool-window fetch (Decisions 4 & 9). It sits
// well above the 12-wide loop window so the newest 12 tool_use rows plus their
// interleaved tool_results all fit (each tool_use has at most one result).
const toolWindowFetch = 40

// healthThresholds are the per-tick resolved thresholds. A zero duration means the
// signal is disabled (the admin set 0, or a read failed and defaulted to 0).
type healthThresholds struct {
	stall    time.Duration
	slow     time.Duration
	queued   time.Duration
	approval time.Duration
}

// detectRunHealth runs one health pass over every active run and returns the number
// of runs whose flag it wrote (raised, changed, or cleared). Best-effort: every
// failure is logged and skipped, never returned — a health hiccup must not fail the
// sweep. A nil settings disables the detector entirely.
func (s *Service) detectRunHealth(ctx context.Context, now time.Time) int64 {
	if s.settings == nil {
		return 0
	}
	enabled, err := s.settings.HealthEnabled(ctx)
	if err != nil {
		slog.Error("health: read health_enabled", "error", err)
		return 0
	}
	if !enabled {
		return 0
	}
	runs, err := s.q.ListActiveRunsForHealth(ctx)
	if err != nil {
		slog.Error("health: list active runs", "error", err)
		return 0
	}
	if len(runs) == 0 {
		return 0
	}
	th := s.healthThresholds(ctx)

	var changed int64
	for _, r := range runs {
		target, reason := s.healthTargetFor(ctx, now, r, th)
		if r.Health == target && textEq(r.HealthReason, reason) {
			continue // nothing changed — skip the write (and, later, its broadcast)
		}
		n, err := s.q.SetRunHealth(ctx, store.SetRunHealthParams{
			Health:       target,
			HealthReason: pgText(reason), // "" → NULL
			HealthSince:  sinceParam(now, target),
			ID:           r.ID,
			Status:       r.Status, // exit-race scope: no-ops if the run left this status
		})
		if err != nil {
			slog.Error("health: write run health", "run_id", r.ID, "error", err)
			continue
		}
		changed += n
	}
	return changed
}

// healthTargetFor computes the (flag, reason) a run should carry now. It returns
// (healthOK, "") when the run looks healthy, which the caller writes as a
// self-clear. Exactly one flag per run; the running-run priority is inside
// runningTarget.
func (s *Service) healthTargetFor(ctx context.Context, now time.Time, r store.ListActiveRunsForHealthRow, th healthThresholds) (string, string) {
	switch r.Status {
	case "queued":
		// A wedged 'claimed' checkout is handled by SweepClaimedNeverStarted, not
		// here (Decision 8); 'queued' gains only a flag, timed from the last
		// transition into queued (updated_at).
		if th.queued == 0 || !olderThan(now, r.UpdatedAt, th.queued) {
			return healthOK, ""
		}
		return healthWaitingWorker, s.queuedReason(ctx, r.UserID)
	case "awaiting_approval":
		// auto_approve runs self-resolve their gate and must never nudge anyone to
		// approve them (Decision 8).
		if r.AutoApprove || th.approval == 0 || !olderThan(now, r.UpdatedAt, th.approval) {
			return healthOK, ""
		}
		return healthApprovalIdle, reasonApprovalIdle
	case "running":
		return s.runningTarget(ctx, now, r, th)
	default:
		return healthOK, ""
	}
}

// runningTarget computes the flag for a running run, priority looping > stalled >
// slow (Decision 3). M1 implements stalled (with in-flight suppression) and slow;
// looping is added in M2.
func (s *Service) runningTarget(ctx context.Context, now time.Time, r store.ListActiveRunsForHealthRow, th healthThresholds) (string, string) {
	stats := s.toolWindow(ctx, r.ID)

	// stalled: silence past the threshold, suppressed while a tool call is in flight
	// (Decision 9). A long build/test-suite emits one tool_use then nothing until its
	// result — that is working, not stalled; the wall-clock slow signal still covers a
	// pathological single call.
	if th.stall > 0 && !stats.inFlight {
		if base := stallBaseline(r); !base.IsZero() && now.Sub(base) >= th.stall {
			return healthStalled, reasonStalled
		}
	}

	// slow: wall clock since start, regardless of activity or in-flight state.
	if th.slow > 0 && r.StartedAt.Valid && now.Sub(r.StartedAt.Time) >= th.slow {
		return healthSlow, reasonSlow
	}

	return healthOK, ""
}

// toolWindowStats are the run-health signals derivable from the tool-call window.
type toolWindowStats struct {
	// inFlight is true when the newest tool_use has no matching tool_result yet.
	inFlight bool
}

// toolWindow fetches and analyzes a running run's recent tool activity. A fetch
// error degrades to the zero value (not in flight), so a transient DB blip biases
// toward the stalled/slow checks rather than crashing the pass.
func (s *Service) toolWindow(ctx context.Context, runID uuid.UUID) toolWindowStats {
	rows, err := s.q.ListRunToolWindow(ctx, store.ListRunToolWindowParams{
		RunID: runID,
		Lim:   toolWindowFetch,
	})
	if err != nil {
		slog.Error("health: read tool window", "run_id", runID, "error", err)
		return toolWindowStats{}
	}
	return analyzeToolWindow(rows)
}

// analyzeToolWindow computes the tool-window signals from rows ordered newest-first.
// M1 computes only in-flight; M2 adds the loop hash.
//
// In-flight (Decision 9): the newest tool_use has no matching tool_result. Because
// rows are seq-desc and a completed call's result has a higher seq than its
// tool_use, that result is seen before the tool_use here, so a lookup in the
// collected result-id set answers the question with no extra query.
func analyzeToolWindow(rows []store.ListRunToolWindowRow) toolWindowStats {
	resultIDs := make(map[string]bool)
	var newestUseID string
	var haveUse bool
	for _, row := range rows {
		switch row.Kind {
		case "tool_result":
			if id := toolResultID(row.Payload); id != "" {
				resultIDs[id] = true
			}
		case "tool_use":
			if !haveUse {
				newestUseID = toolUseID(row.Payload)
				haveUse = true
			}
		}
	}
	return toolWindowStats{
		inFlight: haveUse && newestUseID != "" && !resultIDs[newestUseID],
	}
}

// stallBaseline is GREATEST(last_activity_at, started_at): the later of the last
// message and the run start (Decision 2). started_at guards the checkout window
// before the first message, when last_activity_at is still NULL.
func stallBaseline(r store.ListActiveRunsForHealthRow) time.Time {
	var base time.Time
	if r.StartedAt.Valid {
		base = r.StartedAt.Time
	}
	if r.LastActivityAt.Valid && r.LastActivityAt.Time.After(base) {
		base = r.LastActivityAt.Time
	}
	return base
}

// queuedReason resolves the human reason a queued run past its threshold is not
// running, most-actionable first (Decision 8): a locked owner vault (they unlock and
// it claims within a poll), then no online worker, else a plain wait.
func (s *Service) queuedReason(ctx context.Context, userID uuid.UUID) string {
	if s.vlt != nil && !s.vlt.Unlocked(userID) {
		return reasonVaultLocked
	}
	n, err := s.q.CountOnlineWorkersForUser(ctx, userID)
	if err != nil {
		slog.Error("health: count online workers", "error", err)
		return reasonWaitingWorker
	}
	if n == 0 {
		return reasonNoWorker
	}
	return reasonWaitingWorker
}

// healthThresholds reads all thresholds once per tick (the settings cache is a 5s
// read-through, so this is cheap). Each accessor already falls back to its default
// on a read error; healthDur maps a non-positive value to a disabled (zero) signal.
func (s *Service) healthThresholds(ctx context.Context) healthThresholds {
	return healthThresholds{
		stall:    healthDur(s.settings.HealthStallSeconds(ctx)),
		slow:     s.slowThreshold(ctx),
		queued:   healthDur(s.settings.HealthQueuedSeconds(ctx)),
		approval: healthDur(s.settings.HealthApprovalSeconds(ctx)),
	}
}

// healthDur turns a settings accessor's (seconds, err) into a duration, mapping a
// non-positive value (0 = disabled, or a read that fell back to a bad value) to a
// zero duration the caller reads as "signal off". The error is already logged by
// the accessor's default fallback path; a strict re-log here would be noise.
func healthDur(secs int, _ error) time.Duration {
	if secs <= 0 {
		return 0
	}
	return time.Duration(secs) * time.Second
}

// slowThreshold reads health_slow_seconds and applies the read-time RUN_TIMEOUT
// clamp (Decision 5): a slow threshold at or beyond RUN_TIMEOUT would never fire —
// the run is failed by the timeout first — so it is clamped just under and logged.
// RUN_TIMEOUT is an env value that can change across restarts, which is why this
// lives here and not in the pure settings.Validate().
func (s *Service) slowThreshold(ctx context.Context) time.Duration {
	slow := healthDur(s.settings.HealthSlowSeconds(ctx))
	if slow == 0 {
		return 0
	}
	if s.p.RunTimeout > 0 && slow >= s.p.RunTimeout {
		clamped := s.p.RunTimeout - time.Minute
		if clamped <= 0 {
			clamped = s.p.RunTimeout / 2
		}
		slog.Warn("health: health_slow_seconds >= RUN_TIMEOUT; clamping so the slow flag can fire before the run is timed out",
			"configured", slow.String(), "clamped", clamped.String(), "run_timeout", s.p.RunTimeout.String())
		slow = clamped
	}
	return slow
}

// -------------------------------------------------------------------------
// small helpers
// -------------------------------------------------------------------------

// olderThan reports whether ts is valid and at least d in the past relative to now.
func olderThan(now time.Time, ts pgtype.Timestamptz, d time.Duration) bool {
	return ts.Valid && now.Sub(ts.Time) >= d
}

// sinceParam stamps health_since with now for a flag, or NULL when clearing to ok.
func sinceParam(now time.Time, target string) pgtype.Timestamptz {
	if target == healthOK {
		return pgtype.Timestamptz{}
	}
	return pgTime(now)
}

// textEq compares a nullable text column against a want string, where "" means the
// column should be NULL. Used to skip a no-op health write when neither the flag nor
// its reason changed.
func textEq(t pgtype.Text, want string) bool {
	if want == "" {
		return !t.Valid
	}
	return t.Valid && t.String == want
}

// toolUseID / toolResultID extract the block ids from a run_message payload. The
// shapes come from the worker's sdk-messages mapping (tool_use carries `id`,
// tool_result carries `tool_use_id`). A malformed payload yields "" — treated as
// "no id", which the caller handles conservatively.
func toolUseID(payload []byte) string {
	var p struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return ""
	}
	return p.ID
}

func toolResultID(payload []byte) string {
	var p struct {
		ToolUseID string `json:"tool_use_id"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return ""
	}
	return p.ToolUseID
}
