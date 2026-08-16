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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
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
	// HealthNudgeCooldownSeconds bounds how often a single run may DM its owner
	// (PRD #47 M4). 0 means no cooldown (nudge on every ok→flagged transition).
	HealthNudgeCooldownSeconds(ctx context.Context) (int, error)
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
	// reasonAllWorkersBusy (PRD #216) distinguishes a saturated fleet from an idle
	// queue: every online worker is at its advertised run-lane cap, so this run is
	// waiting for a SLOT to free — not for a worker to come online — and the
	// fleet-aware claim (migration 00113) may be deferring it to a peer that is itself
	// full. Maps to the SAME healthWaitingWorker enum (no migration owed —
	// runs.health_reason is free text, only runs.health is CHECK-constrained). A NEW
	// const rather than reusing reasonWaitingWorker so the message tells the owner
	// "add capacity" apart from "start a worker".
	reasonAllWorkersBusy = "all your workers are busy; this run is waiting for a free slot"
	// reasonPersistFailing is the PRD #108 M4 reason: the run's messages cannot be
	// written, so the agent keeps re-sending them. Same contract as its siblings —
	// fixed, server-controlled, no tool name, no repo content, no live duration.
	//
	// It names the MECHANISM rather than the symptom, unlike the `slow` this
	// replaces: the incident reported "this run is taking longer than usual" for a
	// run that was neither slow nor working. No migration is owed — runs.health_reason
	// is plain text with no CHECK (00057), and only runs.health is constrained, where
	// 'looping' already exists.
	reasonPersistFailing = "the agent's updates can't be saved, so it keeps resending them"
	// reasonVerdictUndelivered is issue #182's reason: the owner answered the approval
	// gate and the worker has not acted on it yet. Same contract as its siblings — no
	// tool name, no repo content, no live duration.
	//
	// A NEW const rather than a reuse of reasonWaitingWorker, despite both mapping to
	// the healthWaitingWorker enum: that one ("waiting for a worker to pick up this
	// run") belongs to the queued arm and describes an UNCLAIMED run, while this
	// describes a run whose worker already holds it. "response" rather than "approval"
	// or "revision" so one string covers all four verdict kinds without leaking which
	// decision the owner made.
	reasonVerdictUndelivered = "the worker hasn't picked up your response yet"
)

// Persistence-failure FLAG thresholds (PRD #108 M4), code constants for the same
// reason loopWindow/loopThreshold are: they describe a mechanism, not an operator
// preference.
//
// They sit strictly BELOW M5's kill thresholds (autoStopStreak/autoStopWindow), and
// detectRunHealth runs before the auto-stop step in Sweep, so the PRD's "health
// first, kill second" ordering holds by construction rather than by timing — the
// flag lands at least three sweep ticks before a kill can.
//
// 5 failures is ~2.5s at the incident's observed ~2 Hz — enough not to be one blip;
// the 10s window rides out a single slow query.
const (
	persistFlagStreak = 5
	persistFlagWindow = 10 * time.Second
)

// Loop-detection window (Decision 4), code constants (not settings): flag when any
// tool call recurs at least loopThreshold times among the newest loopWindow
// tool_use. loopThreshold=4 means an A/B-alternating loop still trips once the
// window has filled (4 A's and 4 B's), while a healthy edit→test cycle whose
// distinct edits keep any single call under 4 does not.
const (
	loopWindow    = 12
	loopThreshold = 4
)

// toolWindowFetch bounds the per-run tool-window fetch (Decisions 4 & 9). It sits
// well above the loop window so the newest loopWindow tool_use rows plus their
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
	if s.healthSettings == nil {
		return 0
	}
	enabled, err := s.healthSettings.HealthEnabled(ctx)
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
	cooldown := healthDur(s.healthSettings.HealthNudgeCooldownSeconds(ctx))

	var changed int64
	for _, r := range runs {
		target, reason := s.healthTargetFor(ctx, now, r, th)
		if r.Health == target && textEq(r.HealthReason, reason) {
			continue // nothing changed — skip the write (and, later, its broadcast)
		}
		// Nudge-worthiness (Decision 7): only the ok→flagged transition (a new
		// episode) DMs the owner, and only if the cooldown has elapsed since the last
		// nudge. The sweeper is the single writer of health_notified_at, so it stamps
		// it here — in the same SetRunHealth write — exactly when it emits a nudge.
		nudge := target != healthOK && r.Health == healthOK &&
			(cooldown == 0 || !r.HealthNotifiedAt.Valid || now.Sub(r.HealthNotifiedAt.Time) >= cooldown)
		notifiedAt := pgtype.Timestamptz{}
		if nudge {
			notifiedAt = pgTime(now)
		}
		n, err := s.q.SetRunHealth(ctx, store.SetRunHealthParams{
			Health:           target,
			HealthReason:     pgText(reason), // "" → NULL
			HealthSince:      healthSince(now, target, r),
			HealthNotifiedAt: notifiedAt, // NULL → COALESCE preserves the existing stamp
			ID:               r.ID,
			Status:           r.Status, // exit-race scope: no-ops if the run left this status
		})
		if err != nil {
			slog.Error("health: write run health", "run_id", r.ID, "error", err)
			continue
		}
		if n == 0 {
			continue // status-scoped write hit the exit race — no change landed, don't broadcast
		}
		changed += n
		// Fan the change out AFTER it is durable (PRD #47 M3): the live hub prompts a
		// browser re-read within a tick, the Slack notifier (M4) re-renders/nudges.
		// Best-effort and non-blocking by the Broadcaster contract.
		if s.bcast != nil {
			s.bcast.PublishHealth(r.ID, target, reason, nudge)
		}
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
		// The gate is old — but "old" alone does not mean the OWNER is the one holding
		// it up (issue #182). The revise path deliberately never touches runs (see
		// CreateRunReviseInputIfUnderCap), so a user who requested changes at t+50m
		// still hits this arm at t+60m and, before this check existed, was nudged to
		// approve a plan they had already responded to. Between their response and the
		// worker delivering the next plan version the run is waiting on the WORKER.
		//
		// Placed BEHIND the three guards above on purpose: that is what makes a per-run
		// lookup affordable here (see the query's own comment for the indexing
		// argument). It is also why this cannot become a projection on
		// ListActiveRunsForHealth.
		if s.verdictUndelivered(ctx, r) {
			return healthWaitingWorker, reasonVerdictUndelivered
		}
		return healthApprovalIdle, reasonApprovalIdle
	case "running":
		return s.runningTarget(ctx, now, r, th)
	default:
		return healthOK, ""
	}
}

// runningTarget computes the flag for a running run, priority persist-looping >
// tool-looping > stalled > slow (Decision 3, extended by PRD #108 M4): looping is
// the strongest evidence of pathology, and slow is a wall-clock backstop that must
// not mask a more specific signal.
func (s *Service) runningTarget(ctx context.Context, now time.Time, r store.ListActiveRunsForHealthRow, th healthThresholds) (string, string) {
	// looping, persistence flavour (PRD #108 M4). Checked FIRST, and deliberately
	// NOT from run_messages: the arm below reads ListRunToolWindow, and this wedge IS
	// a failure to persist run_messages, so that evidence source is blind BY
	// CONSTRUCTION rather than by threshold. This arm reads the api's own in-process
	// count of AppendMessages failures for this run instead — evidence produced by
	// the thing that failed, which the wedge cannot suppress because the wedge is the
	// event being counted.
	//
	// Above the tool-window arm because both map to the same `looping` enum and this
	// is the more specific truth: below it, a run that is BOTH repeating a call and
	// failing to persist would report the repeat and hide the wedge. Same enum means
	// nothing downstream (slacksvc, web badge, CLI) changes shape.
	//
	// Bonus worth keeping: returning here skips the per-tick ListRunToolWindow query
	// for exactly the runs whose message stream is broken.
	//
	// NO CLASS NARROWING HERE, and that is deliberate — autoStopKillableKinds narrows
	// only the KILL (PRD #108 §16). The flag is early warning and any repeated
	// persistence failure is worth warning about. The consequence, stated so nobody
	// meets it by surprise: a fault lasting >= persistFlagWindow that hits
	// run_messages inserts specifically will flag EVERY actively-appending run
	// `looping` and nudge each owner, subject to PRD #47's cooldown. A whole-database
	// outage does not reach this — SetRunHealth fails too, and detectRunHealth logs
	// and skips.
	// No IsZero guard on firstAt: `streak >= persistFlagStreak` already implies an
	// entry exists, and recordFailure sets firstAt on every path that creates or
	// resets one. It was here and it was INERT — measured by folding it to `true`,
	// which left the whole suite green. An inert conjunct in a kill's ancestry reads
	// as a guard and defends nothing.
	if fs := s.persistFail.stats(r.ID); fs.streak >= persistFlagStreak && now.Sub(fs.firstAt) >= persistFlagWindow {
		return healthLooping, reasonPersistFailing
	}

	stats := s.toolWindow(ctx, r.ID)

	// looping: the same tool call recurred past the threshold in the window. Not
	// settings-gated (the window/threshold are code constants) and not suppressed by
	// in-flight — a run repeating the same call is pathological even mid-call.
	if stats.looping {
		return healthLooping, reasonLooping
	}

	// stalled: silence past the threshold, suppressed while a tool call is in flight
	// (Decision 9). A long build/test-suite emits one tool_use then nothing until its
	// result — that is working, not stalled; the wall-clock slow signal still covers a
	// pathological single call.
	if th.stall > 0 && !stats.inFlight {
		if base := stallBaseline(r); !base.IsZero() && now.Sub(base) >= th.stall {
			return healthStalled, reasonStalled
		}
	}

	// slow: wall clock since start, regardless of activity or in-flight state. The RAW
	// threshold (th.slow) is clamped PER-RUN against this run's EFFECTIVE timeout (PRD
	// #122 M2, Decision 5b): the global RUN_TIMEOUT unless the run froze a scaled budget,
	// in which case its persisted budget_wall_seconds. Without this a scaled run would
	// render "slow" for its whole extended life (the global-clamped threshold fires ~2h
	// into an 8h run). Byte-for-byte today for an unscaled run (NULL budget → eff = global).
	effTimeout := s.p.RunTimeout
	if r.BudgetWallSeconds.Valid && r.BudgetWallSeconds.Int32 > 0 {
		effTimeout = time.Duration(r.BudgetWallSeconds.Int32) * time.Second
	}
	if slow := clampSlow(th.slow, effTimeout); slow > 0 && r.StartedAt.Valid && now.Sub(r.StartedAt.Time) >= slow {
		return healthSlow, reasonSlow
	}

	return healthOK, ""
}

// toolWindowStats are the run-health signals derivable from the tool-call window.
type toolWindowStats struct {
	// inFlight is true when the newest tool_use has no matching tool_result yet.
	inFlight bool
	// looping is true when some tool call recurs at least loopThreshold times among
	// the newest loopWindow tool_use.
	looping bool
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
//
// In-flight (Decision 9): the newest tool_use has no matching tool_result. Because
// rows are seq-desc and a completed call's result has a higher seq than its
// tool_use, that result is seen before the tool_use here, so a lookup in the
// collected result-id set answers the question with no extra query.
//
// Looping (Decision 4): hash each of the newest loopWindow tool_use as
// sha256(name + canonical-JSON(input)) and flag when any hash count reaches
// loopThreshold. The in-flight (possibly newest) call is included in the window.
// The hash is compared transiently and NEVER surfaced — not in health_reason, logs,
// or Slack.
func analyzeToolWindow(rows []store.ListRunToolWindowRow) toolWindowStats {
	resultIDs := make(map[string]bool)
	counts := make(map[string]int)
	var newestUseID string
	var haveUse bool
	var hashed int
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
			if hashed < loopWindow {
				counts[toolCallHash(row.Payload)]++
				hashed++
			}
		}
	}
	maxRepeat := 0
	for _, c := range counts {
		if c > maxRepeat {
			maxRepeat = c
		}
	}
	return toolWindowStats{
		inFlight: haveUse && newestUseID != "" && !resultIDs[newestUseID],
		looping:  maxRepeat >= loopThreshold,
	}
}

// toolCallHash is the loop-detection fingerprint of a tool_use payload:
// sha256(name + NUL + canonical-JSON(input)). Two calls with the same tool and
// semantically-equal input hash alike; a malformed payload hashes its raw bytes so
// it participates deterministically without grouping with a well-formed call. The
// digest is only ever compared for equality — never logged or stored.
func toolCallHash(payload []byte) string {
	var p struct {
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		sum := sha256.Sum256(payload)
		return hex.EncodeToString(sum[:])
	}
	h := sha256.New()
	h.Write([]byte(p.Name))
	h.Write([]byte{0})
	h.Write(canonicalJSON(p.Input))
	return hex.EncodeToString(h.Sum(nil))
}

// canonicalJSON re-encodes a JSON value so equal values compare equal regardless of
// object-key order (Go's json.Marshal sorts map keys). Array order is preserved (it
// is semantic). An absent or unparseable input degrades to a stable literal / the
// raw bytes so the hash stays deterministic.
func canonicalJSON(raw json.RawMessage) []byte {
	if len(raw) == 0 {
		return []byte("null")
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return raw
	}
	out, err := json.Marshal(v)
	if err != nil {
		return raw
	}
	return out
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
// running, most-actionable first (Decision 8, extended by PRD #216): a locked owner
// vault (they unlock and it claims within a poll), then no online worker at all, then
// a saturated fleet where every online worker is at its cap (add capacity), else a
// plain wait for an idle worker to claim.
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
	free, err := s.q.CountOnlineWorkersWithFreeSlotForUser(ctx, userID)
	if err != nil {
		slog.Error("health: count online workers with free slot", "error", err)
		return reasonWaitingWorker
	}
	if free == 0 {
		return reasonAllWorkersBusy
	}
	return reasonWaitingWorker
}

// verdictUndelivered reports whether the run carries a gate verdict submitted AT OR AFTER
// this gate opened — i.e. the owner has answered and the worker has not acted on it yet
// (issue #182). The predicate, its `>=` boundary and the four-of-six kind list all live in
// RunHasVerdictSinceGateOpened; read that comment before changing either side.
//
// r.UpdatedAt is passed as the episode boundary rather than re-read in SQL, so this asks
// about exactly the gate the caller's threshold guard just aged.
//
// A read error degrades to false, which yields the pre-#182 approval_idle. That is the
// conservative direction for a best-effort detector: a nudge the owner has already
// answered is noise, while suppressing one on a gate nobody has touched would hide the
// signal this arm exists to raise. Same shape as toolWindow's degrade-to-zero-value.
func (s *Service) verdictUndelivered(ctx context.Context, r store.ListActiveRunsForHealthRow) bool {
	ok, err := s.q.RunHasVerdictSinceGateOpened(ctx, store.RunHasVerdictSinceGateOpenedParams{
		RunID:        r.ID,
		GateOpenedAt: r.UpdatedAt,
	})
	if err != nil {
		slog.Error("health: read gate verdict", "run_id", r.ID, "error", err)
		return false
	}
	return ok
}

// healthThresholds reads all thresholds once per tick (the settings cache is a 5s
// read-through, so this is cheap). Each accessor already falls back to its default
// on a read error; healthDur maps a non-positive value to a disabled (zero) signal.
func (s *Service) healthThresholds(ctx context.Context) healthThresholds {
	return healthThresholds{
		stall:    healthDur(s.healthSettings.HealthStallSeconds(ctx)),
		slow:     s.slowThreshold(ctx),
		queued:   healthDur(s.healthSettings.HealthQueuedSeconds(ctx)),
		approval: healthDur(s.healthSettings.HealthApprovalSeconds(ctx)),
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

// slowThreshold reads health_slow_seconds and returns it RAW (unclamped) — the clamp
// against a run's effective timeout is applied PER-RUN in runningTarget (PRD #122 M2,
// Decision 5b), not here, because a scaled run's ceiling is its persisted
// budget_wall_seconds, not the global RUN_TIMEOUT, and this pass has no run in hand.
//
// The once-per-value operator-misconfig WARN stays here: a health_slow_seconds at or
// beyond the GLOBAL RUN_TIMEOUT is a misconfiguration worth a log, since the common
// (unscaled) run is still failed by the global timeout first and the flag would never
// fire for it. RUN_TIMEOUT is an env value that can change across restarts, which is
// why this lives here and not in the pure settings.Validate().
func (s *Service) slowThreshold(ctx context.Context) time.Duration {
	slow := healthDur(s.healthSettings.HealthSlowSeconds(ctx))
	if slow == 0 {
		s.lastSlowClampWarn = 0 // reset so a later re-break warns again
		return 0
	}
	if s.p.RunTimeout > 0 && slow >= s.p.RunTimeout {
		// Log once per distinct misconfigured value, not on every 15s sweep: the check
		// is re-evaluated each pass, so an unconditional Warn would spam the log for as
		// long as the misconfiguration stands.
		if s.lastSlowClampWarn != slow {
			slog.Warn("health: health_slow_seconds >= RUN_TIMEOUT; the slow flag will be clamped per-run so it can fire before a run is timed out",
				"configured", slow.String(), "run_timeout", s.p.RunTimeout.String())
			s.lastSlowClampWarn = slow
		}
		return slow
	}
	s.lastSlowClampWarn = 0 // configured below the timeout now; a later re-break warns again
	return slow
}

// clampSlow clamps a RAW slow threshold against a run's EFFECTIVE timeout (PRD #122
// M2, Decision 5b): a threshold at or beyond the effective timeout would never fire —
// the run is failed by the timeout first — so it is pulled just under. Returns raw
// unchanged when raw < eff (the common case, byte-for-byte today for an unscaled run),
// eff - 1m otherwise (or eff/2 when that would be non-positive), and 0 when raw is 0.
func clampSlow(raw, eff time.Duration) time.Duration {
	if raw == 0 {
		return 0
	}
	if eff <= 0 || raw < eff {
		return raw
	}
	clamped := eff - time.Minute
	if clamped <= 0 {
		clamped = eff / 2
	}
	return clamped
}

// -------------------------------------------------------------------------
// small helpers
// -------------------------------------------------------------------------

// olderThan reports whether ts is valid and at least d in the past relative to now.
func olderThan(now time.Time, ts pgtype.Timestamptz, d time.Duration) bool {
	return ts.Valid && now.Sub(ts.Time) >= d
}

// healthSince decides the health_since to write. It stamps now when a flag is raised
// or its enum CHANGES (a genuinely new episode), PRESERVES the existing timestamp
// when only the reason changes within the same enum (so a queued run flipping
// no-worker → waiting keeps the UI's "stuck for Xm" counting from the original flag),
// and clears it when the run returns to ok.
func healthSince(now time.Time, target string, r store.ListActiveRunsForHealthRow) pgtype.Timestamptz {
	switch {
	case target == healthOK:
		return pgtype.Timestamptz{}
	case r.Health == target:
		return r.HealthSince
	default:
		return pgTime(now)
	}
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
