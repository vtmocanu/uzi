package workersvc

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/autoselect"
	"github.com/vtmocanu/uzi/api/internal/autoselectrow"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// wallParkGraceSeconds is the fixed grace (PRD #1497 D11) a live, capable worker gets to
// finish its own capture-first wall park before ParkRunsAtWall parks the row from under it.
// A constant, not a setting: it exists only to let a live worker finish its capture, and a
// knob would be a way to reintroduce a de facto timeout.
const wallParkGraceSeconds = 600

// Sweep enforces the liveness rules the workers cannot: stale workers go offline
// and their non-terminal runs are re-queued (or failed past the re-queue cap);
// claimed-but-never-started runs are reclaimed; runs past their wall-clock deadline are
// PARKED (paused with hold_reason='budget_exhausted'), never failed (PRD #1497 M1).
// It is called on a ticker and once immediately at boot (the orphan sweep).
func (s *Service) Sweep(ctx context.Context) (SweepResult, error) {
	now := s.now()
	staleCutoff := pgconv.Time(now.Add(-s.p.WorkerHeartbeatStale))
	// PRD #1390 M1 (D9): the over-cap FAIL waits for TWO consecutive stale windows, so a run
	// requeued once then hit by a partition just over one window is not terminated before a
	// heartbeat can re-adopt it. The REQUEUE keeps the single window (staleCutoff).
	failCutoff := pgconv.Time(now.Add(-2 * s.p.WorkerHeartbeatStale))
	claimCutoff := pgconv.Time(now.Add(-s.p.ClaimGrace))
	// issue #1367: the undispatched-handoff reaper cutoff — a kind='task' run queued with
	// dispatched_at NULL and created before this is past its dispatch grace window. Mirrors
	// claimCutoff, but off DispatchGrace (a separate, longer window; see Params.DispatchGrace).
	dispatchCutoff := pgconv.Time(now.Add(-s.p.DispatchGrace))
	max := int32(s.p.RunMaxRequeues) //nolint:gosec // G115: RunMaxRequeues is a small bounded config int (env RUN_MAX_REQUEUES), never near int32 range

	// PRD #1390 M1 (D1): the boot grace. While active, the three stale-worker passes
	// (MarkStaleWorkersOffline, FailRunsOfStaleWorkersOverCap, RequeueRunsOfStaleWorkers) are
	// skipped — an api that was unreachable must not declare every worker dead the instant it
	// returns, before any worker could reconnect. Every other pass below runs as today. The
	// grace is anchored on listener-ready (SetReadyAt), not process start; readyAt still zero
	// (before bind) is treated as active. SWEEPER_BOOT_GRACE=0 makes this always false.
	graceActive := bootGraceActive(s.p.SweeperBootGrace, s.readyAtTime(), now)
	if graceActive {
		slog.Info("sweeper: boot grace active, skipping stale-worker passes",
			"boot_grace", s.p.SweeperBootGrace, "ready_at", s.readyAtTime())
	} else if s.p.SweeperBootGrace > 0 && s.bootGraceFirstRunLogged.CompareAndSwap(false, true) {
		slog.Info("sweeper: boot grace elapsed, running stale-worker passes",
			"boot_grace", s.p.SweeperBootGrace, "ready_at", s.readyAtTime())
	}

	var res SweepResult
	var err error

	if !graceActive {
		if res.WorkersOffline, err = s.q.MarkStaleWorkersOffline(ctx, staleCutoff); err != nil {
			return res, fmt.Errorf("mark stale workers offline: %w", err)
		}
	}

	claimed, err := s.q.SweepClaimedNeverStarted(ctx, claimCutoff)
	if err != nil {
		return res, fmt.Errorf("sweep claimed-never-started: %w", err)
	}
	res.ClaimedReset = int64(len(claimed))
	for _, r := range claimed {
		s.publishSwept(r.ID, r.Status)
		// A fresh attempt starts with no evidence against it (PRD #108 M5). See the
		// requeue loop below for the argument; this reset is the same event.
		s.persistFail.evict(r.ID)
	}

	// Undispatched-handoff reaper (issue #1367): a kind='task' run left queued with
	// dispatched_at NULL past DispatchGrace — the CLI never landed its push/dispatch — is
	// terminalized (fail_origin='task_undispatched'). Modelled on the claimed-never-started
	// block above (broadcast the transition), NOT on the running-timeout block below: an
	// undispatched row was never claimed, so it has no agent attempt/trace and MUST NOT be
	// handed to the judge (belt-and-braces: task_undispatched is also in neverJudgeFailOrigins).
	undispatched, err := s.q.SweepTaskNeverDispatched(ctx, store.SweepTaskNeverDispatchedParams{
		FailureReason: pgconv.TextOrNull("handoff was not dispatched before its setup deadline"),
		Cutoff:        dispatchCutoff,
	})
	if err != nil {
		return res, fmt.Errorf("sweep task-never-dispatched: %w", err)
	}
	res.TaskUndispatchedFailed = int64(len(undispatched))
	for _, r := range undispatched {
		s.publishSwept(r.ID, r.Status)
	}

	// PRD #1497 M1: the wall-clock sweep PARKS a run at its deadline, it does not fail it. Two passes
	// over the SAME per-run three-term deadline (budget_wall_seconds|global + extension + finalize,
	// plus banked pause), both honouring each run's persisted budget so a scaled run is never parked
	// at the global 2h. RequestWallParks files a system 'wall' pause request on a run whose worker is
	// alive and speaks wall_park_v1 (the worker drops its turn and captures); ParkRunsAtWall parks the
	// row server-side when the worker is dead, incapable, or unresponsive past the grace. Neither
	// fails the run, so a parked row is NOT enqueued to the judge (it is not finished). ParkRunsAtWall
	// is a stale-worker classification pass, so it lives in the boot-grace-gated block below (PRD #1390
	// M1) — it must not server-park a returning worker's run on a transiently-stale heartbeat — and it
	// runs FIRST there, BEFORE both the stale-worker fail-over-cap and requeue (D5), so a dead worker's
	// out-of-time run parks (preserving its work) instead of being failed over cap or requeued and
	// re-cloned only to park.
	//
	// worker_stale_cutoff is the SAME staleCutoff the stale-worker passes use, so "live" means exactly
	// "not yet swept as a stale worker" — both for the #1226 carve-out (a live post-attempt run is
	// left for StampCompletionBudgetExhausted) and for the ParkRunsAtWall stale classification.
	requested, err := s.q.RequestWallParks(ctx, store.RequestWallParksParams{
		Now:                  pgconv.Time(now),
		GlobalTimeoutSeconds: int32(s.p.RunTimeout.Seconds()),
		WorkerStaleCutoff:    staleCutoff,
	})
	if err != nil {
		return res, fmt.Errorf("request wall parks: %w", err)
	}
	res.WallParkRequested = int64(len(requested))
	for _, r := range requested {
		// The run stays 'running' (the worker will park it); publish so live surfaces show the
		// pending pause. No judge fan-out — the run is not finished.
		s.publishSwept(r.ID, r.Status)
	}

	// PRD #1497 review (Fix 2): ParkRunsAtWall is the SERVER-side park — it fires precisely on a
	// worker classified stale/incapable/unresponsive, so it belongs with the other stale-worker
	// passes below, INSIDE the boot grace (PRD #1390 M1). RequestWallParks stays here, ungated: it
	// only touches LIVE, capable workers (a transiently-stale returning worker cannot match it), so
	// it is safe during grace and lets a live worker self-park.

	// PRD #1226 M4 (D3): the served `budget_exhausted` steer, run RIGHT AFTER the sweep with the
	// SAME now/global_timeout_seconds/worker_stale_cutoff, so it stamps EXACTLY the post-attempt
	// live-worker interlocked rows the carve-out just spared — arming the one-shot flag the worker
	// reads off its running-report ACK to enter the completion hold. No broadcast/judge fan-out:
	// the run stays `running` (a live worker still owns it), so this only sets a flag the worker
	// acts on; nothing transitions here.
	if res.CompletionBudgetExhausted, err = s.q.StampCompletionBudgetExhausted(ctx, store.StampCompletionBudgetExhaustedParams{
		Now:                  pgconv.Time(now),
		GlobalTimeoutSeconds: int32(s.p.RunTimeout.Seconds()),
		WorkerStaleCutoff:    staleCutoff,
	}); err != nil {
		return res, fmt.Errorf("stamp completion budget exhausted: %w", err)
	}

	// Fail-over-cap before re-queue: the two are disjoint on requeue_count, but
	// failing first keeps a run that just hit the cap from being re-queued. Both are
	// stale-worker passes, so both are suppressed inside the boot grace (PRD #1390 M1).
	if !graceActive {
		// PRD #1497 M1 (D5), gated by boot grace (Fix 2): server-park each past-deadline run whose
		// worker is stale/incapable/unresponsive-past-grace. It runs FIRST in this block — BEFORE both
		// FailRunsOfStaleWorkersOverCap and RequeueRunsOfStaleWorkers — so a dead worker's out-of-time
		// run PARKS (preserving its work, the PRD #1497 invariant) instead of being failed over cap or
		// requeued and re-cloned only to park. It is inside `if !graceActive` for the same reason
		// MarkStaleWorkersOffline is: while the api is in its post-outage boot grace (PRD #1390 M1) a
		// returning worker's last_heartbeat_at is transiently stale, and server-parking its run then
		// would drop same-worker affinity before the worker can self-park — the exact misclassification
		// the boot grace exists to prevent.
		parked, err := s.q.ParkRunsAtWall(ctx, store.ParkRunsAtWallParams{
			Now:                  pgconv.Time(now),
			GlobalTimeoutSeconds: int32(s.p.RunTimeout.Seconds()),
			WorkerStaleCutoff:    staleCutoff,
			// D11: the grace a live capable worker gets to finish its own capture before the server
			// parks the row from under it. A fixed 10-minute constant, deliberately not a setting.
			GraceSeconds: int32(wallParkGraceSeconds),
		})
		if err != nil {
			return res, fmt.Errorf("park runs at wall: %w", err)
		}
		res.WallParked = int64(len(parked))
		for _, r := range parked {
			// The run transitioned to paused; fan the transition out. NOT enqueued to the judge —
			// unlike the old fail-at-the-wall, a parked run is not finished.
			s.publishSwept(r.ID, r.Status)
		}

		failed, err := s.q.FailRunsOfStaleWorkersOverCap(ctx, store.FailRunsOfStaleWorkersOverCapParams{
			FailureReason: pgconv.TextOrNull("worker lost; exceeded re-queue budget"),
			MaxRequeues:   max,
			// PRD #1390 M1 (D9): the two-window cutoff — the over-cap fail requires the worker
			// to have been stale for 2*WORKER_HEARTBEAT_STALE, unlike the single-window requeue.
			FailCutoff: failCutoff,
		})
		if err != nil {
			return res, fmt.Errorf("fail stale-worker runs over cap: %w", err)
		}
		res.StaleFailed = int64(len(failed))
		for _, r := range failed {
			s.publishSwept(r.ID, r.Status)
			// PRD #46 Decision 2: a swept-to-failed run (worker lost, over re-queue budget)
			// is committed-terminal and worth judging. Best-effort, gated inside.
			s.maybeEnqueueJudgeByID(ctx, r.ID)
		}

		requeued, err := s.q.RequeueRunsOfStaleWorkers(ctx, store.RequeueRunsOfStaleWorkersParams{
			MaxRequeues: max,
			Cutoff:      staleCutoff,
		})
		if err != nil {
			return res, fmt.Errorf("re-queue stale-worker runs: %w", err)
		}
		res.StaleRequeued = int64(len(requeued))
		for _, r := range requeued {
			s.publishSwept(r.ID, r.Status)
			// 🔴 A REQUEUE GRANTS A FRESH ATTEMPT, SO IT MUST CLEAR THE DEAD ATTEMPT'S
			// EVIDENCE (PRD #108 M5). This query writes status='queued' but KEEPS
			// worker_id for affinity, so without this the run returns to `running` under a
			// new attempt still carrying the old one's 20-failure streak and is
			// auto-stopped before the new worker persists a byte — uzi killing a run one
			// tick after deciding it deserved another try and spending re-queue budget to
			// say so. Likely rather than theoretical for the population M5 exists to
			// protect: a pre-0.10.1 worker's retry batch GROWS, so a worker wedged at 2 Hz
			// is a prime OOM candidate, and OOM is exactly what puts it here.
			//
			// The window is wide, and uzi's own configuration is the calibration:
			// defaultClaimGrace budgets FIVE MINUTES for claimed→started, while the sweeper
			// gives this 15 seconds. The whole of the new attempt's checkout sits inside it
			// — ensureClone branches on isBareRepo, so a fresh container from a NEW image
			// has an empty cache and takes the cold cloneBare path, and that clone runs
			// between the worker's reportState({status:"running"}) and its first flush
			// (runner.ts; batcher.emit only buffers and then waits for a tick). The claim
			// is about that ORDERING, not a stopwatched duration.
			s.persistFail.evict(r.ID)
		}
	}

	// Chat idle backstop (PRD #39 Decision 3): a chat run whose last message is
	// older than ChatIdleTimeout is completed even though its worker is alive (so no
	// stale-worker sweep above fired for it). Disabled when ChatIdleTimeout is 0.
	if s.p.ChatIdleTimeout > 0 {
		idleChats, err := s.q.SweepIdleChatRuns(ctx, pgconv.Time(now.Add(-s.p.ChatIdleTimeout)))
		if err != nil {
			return res, fmt.Errorf("sweep idle chat runs: %w", err)
		}
		res.ChatIdleCompleted = int64(len(idleChats))
		for _, r := range idleChats {
			s.publishSwept(r.ID, r.Status)
		}
	}

	// Recover issue proposals stranded in 'confirming' by a confirm handler killed
	// mid-flight (M3): revert them to pending so the user retries/dismisses. Disabled
	// when ProposalConfirmStuckTimeout is 0. No broadcast — proposals have no live
	// channel; the browser re-reads on its next proposal fetch.
	if s.p.ProposalConfirmStuckTimeout > 0 {
		recovered, err := s.q.SweepStuckConfirmingProposals(ctx, pgconv.Time(now.Add(-s.p.ProposalConfirmStuckTimeout)))
		if err != nil {
			return res, fmt.Errorf("sweep stuck confirming proposals: %w", err)
		}
		res.ProposalsRecovered = int64(len(recovered))
	}

	// Usage-limit promotion (PRD #35 M2): limit_wait → queued once retry_not_before
	// has elapsed. Placed here — after the status transitions, before the
	// prune/detector/auto-stop observability block — because Sweep's shape is
	// "transitions first, enforcement second", and because a run promoted before the
	// detector runs is health-visible in THIS tick rather than the next. Ordering is
	// otherwise free: every other pass is disjoint from limit_wait as a source and
	// from queued as a target.
	//
	// No persistFail.evict here, unlike the stale-worker requeue above. autoStopWedgedRuns
	// already evicts on `run.Status != "running"`, which a parked run satisfied for the
	// whole park, so the streak is long gone by the time this fires.
	//
	// Duration-time auto-failover re-evaluation (PRD #1247 M3, D8): BEFORE the promote
	// pass, re-ask every still-parked `auto` run whether a pooled alternative became
	// spendable after its park, and LOWER retry_not_before to now() for those that did —
	// Decision 6e extended from park-time to park-duration. Placed immediately before
	// PromoteLimitWaitRuns so the lowered stamp is presented to THIS tick's promote pass,
	// which resumes it the same tick once the pass's clock has reached the stamp (the
	// live-DB tests pin that) and otherwise the very next tick. It only lowers stamps; the
	// resume mutation set stays PromoteLimitWaitRuns' alone.
	if res.LimitReevaluated, err = s.reEvaluateParkedLimitWaitRuns(ctx, now); err != nil {
		return res, fmt.Errorf("re-evaluate parked limit-wait runs: %w", err)
	}

	promoted, err := s.q.PromoteLimitWaitRuns(ctx, pgconv.Time(now))
	if err != nil {
		return res, fmt.Errorf("promote limit-wait runs: %w", err)
	}
	res.LimitPromoted = int64(len(promoted))
	for _, r := range promoted {
		// Same fan-out as every other sweep transition: the broadcaster tells live
		// browsers, and notify moves the board card to In Progress for "queued" —
		// identical to a requeue, which is exactly what a resume looks like from the
		// board's point of view.
		s.publishSwept(r.ID, r.Status)
	}

	// Transient-recovery promotion (issue #1197): recovery_wait → queued once
	// recovery_retry_not_before has elapsed. Placed beside the limit-wait promote for the
	// same "transitions first, enforcement second" reason, and it is a distinct clock-based
	// hold — never folded into PromoteLimitWaitRuns. Unlike the limit park there is no
	// lifetime cap, so this pass keeps auto-promoting a recovering run at the capped cadence
	// until it succeeds or the owner cancels.
	recoveryPromoted, err := s.q.PromoteRecoveryWaitRuns(ctx, pgconv.Time(now))
	if err != nil {
		return res, fmt.Errorf("promote recovery-wait runs: %w", err)
	}
	res.RecoveryPromoted = int64(len(recoveryPromoted))
	for _, r := range recoveryPromoted {
		s.publishSwept(r.ID, r.Status)
	}

	// Reactive pool resume (PRD #754 M5): a pool_wait hold is released the moment its
	// owner's Anthropic token pool becomes non-empty again. Scoped to pool_wait ONLY —
	// never folded into PromoteLimitWaitRuns, whose clock-based predicate is a different
	// hold. Placed here for the same reason the limit promote is: transitions first, a
	// run resumed before the detector runs is health-visible in THIS tick.
	if res.PoolResumed, err = s.resumePoolWaitRuns(ctx); err != nil {
		return res, fmt.Errorf("resume pool-wait runs: %w", err)
	}

	// Custody-release reconciler (PRD #1296 M4, D3): the boot/periodic backstop that
	// settles a recorded successful publication whose best-effort terminal release
	// (SetState) failed, leaving an open hold's live FKs set and its worker un-reapable.
	// It releases ONLY holds whose disposition is durable (a completed run, or a ready
	// capture) — never inferring success from a failed/cancelled/partial/running status —
	// so a failed run with no ready capture keeps its custody. Placed before the reap
	// backstop (ephemeral reap is a separate sweeper.Pass in main.go, run each tick) so a
	// hold released this tick lets the SAME tick's/next tick's reap delete the worker.
	// Partial best-effort: inside ReconcileCustodyReleases a per-hold ReleaseCustodyHold
	// error is logged and skipped (one stuck hold does not sink the reconcile), but a
	// candidate-LIST read error fails the pass and surfaces here — see that method's doc.
	if res.CustodyReleased, err = s.ReconcileCustodyReleases(ctx); err != nil {
		return res, fmt.Errorf("reconcile custody releases: %w", err)
	}

	// Upload-retry-window sweep (PRD #1296 D3/D4): the LIVE consumer of
	// UZI_RECOVERY_UPLOAD_RETRY_WINDOW. A reserved capture advances to 'available' within
	// seconds of a healthy upload, so one still non-terminal (preparing/uploading) past the
	// window has genuinely stalled — surface it as needs_action so the owner sees a
	// retain-and-decide artifact. The custody hold is NOT released (needs_action is a
	// non-releasing state), so the source is RETAINED until capture succeeds or the owner
	// explicitly discards. Disabled when the window is non-positive, matching the
	// ChatIdleTimeout/ProposalConfirmStuckTimeout "0 disables" convention above.
	if s.p.RecoveryUploadRetryWindow > 0 {
		if res.RecoveryStalled, err = s.q.ExpireStalledUploads(ctx, pgtype.Interval{
			Microseconds: s.p.RecoveryUploadRetryWindow.Microseconds(), Valid: true,
		}); err != nil {
			return res, fmt.Errorf("expire stalled recovery uploads: %w", err)
		}
	}

	// Ready-artifact retention sweep (PRD #1296 D4): the LIVE enforcement of
	// UZI_RECOVERY_READY_RETENTION. Each capture's expires_at is baked in at durable capture
	// (MarkCaptureReady) from the retention window; this pass flips every 'available' capture
	// now past its expires_at to 'expired' AND deletes its encrypted chunk rows in one atomic
	// statement, so a retention-expired artifact stops costing byte storage while its metadata
	// row stays visible in state 'expired'. Custody is NOT touched — an expired capture whose
	// hold is still open stays retained per D3. Disabled when the retention is non-positive,
	// matching the RecoveryUploadRetryWindow "0 disables" guard above.
	if s.p.RecoveryReadyRetention > 0 {
		if res.RecoveryExpired, err = s.q.ExpireReadyCaptures(ctx, pgconv.Time(now)); err != nil {
			return res, fmt.Errorf("expire ready recovery captures: %w", err)
		}
	}

	// Bound the in-process persistence-failure tracker (PRD #108 M4). This is the
	// memory bound for the one case no other eviction path reaches: a run whose
	// worker vanished without the run ever reaching terminal. Pruned BEFORE the
	// detector so a flag is never raised off an entry this tick was going to expire.
	s.persistFail.prune(now)

	// Bound the in-process outbox-depth tracker (PRD #1391 M5), the same memory bound
	// persistFail.prune above is: a worker that vanished (or went offline and stopped
	// heartbeating) without a graceful delete has its stale depth age out here, which
	// is also how the "queued on the worker" health reason clears for an offline worker.
	s.outbox.prune(now)

	// Run-health detector (PRD #47): flag/clear slow, stalled, looping, stuck-queued,
	// and approval-idle runs from telemetry already in Postgres. Best-effort and
	// non-terminal — it never kills a run and never fails the sweep (it logs and
	// returns a count); a nil settings (tests) disables it entirely.
	res.HealthChanged = s.detectRunHealth(ctx, now)

	// Auto-stop confirmed per-run persistence loops (PRD #108 M5). Deliberately NOT
	// inside detectRunHealth — Decision 8 (it must not ride health_enabled), and
	// because ListActiveRunsForHealth excludes chat runs, which wedge identically.
	// Runs AFTER the detector so the flag always lands first ("health first, kill
	// second"); its own thresholds sit above the flag's, so that ordering is
	// belt-and-braces rather than the mechanism.
	res.AutoStopped = s.autoStopWedgedRuns(ctx, now)

	// Codex subscription-refresh survivor (issue #1532): reap any account left wedged by an
	// interrupted refresh (in_progress + expired lease, or an orphaned rotating intent) →
	// quarantine + resolve its intents, so a re-link can recover it via reconcileTuple's
	// quarantined arm. Folded into Sweep — the always-on seam the sweeper Engine calls every
	// tick and on Boot — rather than an optional sweeper.Pass, so it cannot be silently
	// unregistered from main.go. Best-effort and non-terminal: a failure is logged and never
	// aborts run recovery, matching detectRunHealth/autoStopWedgedRuns above.
	if n, cerr := s.SweepUnresolvedCodexRefresh(ctx); cerr != nil {
		slog.Error("sweeper: codex refresh survivor failed", "error", cerr)
	} else {
		res.CodexRefreshRecovered = n
	}
	return res, nil
}

// resumePoolWaitRuns is the reactive half of the pool_wait lifecycle (PRD #754 M5): it
// promotes held runs back to 'queued' once their owner's Anthropic token pool is
// non-empty again. Returns the number promoted this tick and fans each out through
// publishSwept, exactly like the limit promote above.
//
// 🔴 AT MOST ONE HELD RUN PER OWNER PER TICK. The live case had three runs held on one
// user's single token; promoting all of them the instant a token pools would thundering-
// herd that one credential — every resumed run would re-claim, and all but one would find
// the pool empty again and re-hold, a churn the hold exists to avoid. So the pass promotes
// only the OLDEST held run for each user with a now-non-empty pool, and lets the next tick
// (~15s later) take the next one once the first has actually claimed a token. ListPoolWaitRuns
// returns oldest-first, so the FIRST run seen for a user is the one to promote.
//
// The candidate query is issued at most once per distinct held-run owner per tick (users
// are deduped as the list is walked), bounding its cost regardless of how many runs a user
// holds. A per-user candidate-query error is logged and skipped — one user's read fault must
// not fail the whole sweep — mirroring how the sweep treats its other best-effort sub-steps.
// A ListPoolWaitRuns error, by contrast, is returned to fail the pass, matching the limit
// promote's own read.
func (s *Service) resumePoolWaitRuns(ctx context.Context) (int64, error) {
	held, err := s.q.ListPoolWaitRuns(ctx)
	if err != nil {
		return 0, fmt.Errorf("list pool-wait runs: %w", err)
	}
	// seen records users already handled this tick: the first (oldest) held run for a user
	// is the one considered, and no user's candidate pool is read twice.
	seen := make(map[uuid.UUID]bool, len(held))
	var resumed int64
	for _, r := range held {
		if seen[r.UserID] {
			continue
		}
		seen[r.UserID] = true

		rows, err := s.q.ListAutoSelectCandidates(ctx, r.UserID)
		if err != nil {
			// Best-effort: skip this user, keep sweeping the rest. The run stays held and
			// the next tick retries it.
			slog.Error("sweeper: pool-resume candidate read failed", "user", r.UserID, "error", err)
			continue
		}
		cands := make([]autoselect.Candidate, 0, len(rows))
		for _, row := range rows {
			cands = append(cands, autoselectrow.FromCandidateRow(row))
		}
		// Resume iff there is a pooled token spendable NOW — autoselect.Floor(cands,
		// claimExclude(run)).ok — the SAME question the re-claim asks, counting AFTER the
		// run's dead-credential exclude (PRD #1247 M3, the pool-promoter fix). This
		// REPLACES the old exclude-blind PoolNonEmpty loop, which relied on the invariant
		// "a pool_wait run can never carry a future retry_not_before". Early promotion (the
		// set-token verb and D8) breaks that invariant: an `auto` run switched early whose
		// only pooled token is its own dead credential is held with a FUTURE stamp, so
		// claimExclude keeps excluding that sole token and Floor.ok is false — the run must
		// NOT resume (it would only re-hold, churning every tick while SetRunPoolWait never
		// counts against RUN_LIMIT_MAX_WAITS). Behaviour is identical in the no-future-stamp
		// case (claimExclude relaxes to Nil, so Floor.ok == the old PoolNonEmpty there); it
		// differs only for a future-stamp pool_wait run, which it correctly holds.
		if _, ok := autoselect.Floor(cands, s.claimExcludeFor(r.LimitDeadSecretID, r.RetryNotBefore), s.now()); !ok {
			continue
		}
		promoted, err := s.q.PromotePoolWaitRun(ctx, store.PromotePoolWaitRunParams{ID: r.ID, UserID: r.UserID})
		if err != nil {
			// Same best-effort stance: a single promote fault does not sink the sweep.
			slog.Error("sweeper: pool-resume promote failed", "run", r.ID, "user", r.UserID, "error", err)
			continue
		}
		if promoted == 0 {
			// The run moved out of pool_wait between the list and the promote (e.g. a
			// concurrent cancel). Nothing to resume; do not broadcast a transition that
			// did not happen.
			continue
		}
		resumed++
		// Same fan-out as the limit promote: broadcast the queued transition so live
		// browsers and the board's In-Progress column follow the resume.
		s.publishSwept(r.ID, "queued")
	}
	return resumed, nil
}

// reEvaluateParkedLimitWaitRuns is the duration-time auto-failover pass (PRD #1247 M3,
// D8): the second limit-wait promoter beside PromoteLimitWaitRuns. For each run STILL
// parked in limit_wait whose next claim resolves through the `auto` selector, it re-asks
// autoselect.NextAvailable over the owner's CURRENT pool and, when a pooled alternative
// is now spendable at or before now, LOWERS retry_not_before to now() so the following
// PromoteLimitWaitRuns pass resumes it (the same tick once that pass's clock has reached
// the lowered stamp, else the next). This is Decision 6e extended from park-time to
// park-duration, reusing the same NextAvailable policy and the same promoter. Returns the
// number lowered.
//
// It does NOT transition status and does NOT fan out publishSwept — the lowered run is
// promoted (and broadcast) by PromoteLimitWaitRuns, which runs immediately after this in
// Sweep. Duplicating either would double-count the resume.
//
// 🔴 AT MOST ONE RUN PER OWNER PER TICK, OLDEST PARK FIRST. This pass bypasses the
// park-time jitter that is the ONLY thing staggering a promoted wave (ADR-35 D4), so
// lowering several of one owner's runs in a single tick would thundering-herd their pool
// the instant PromoteLimitWaitRuns fires — every resumed run re-claims, all but one find
// the alternative already spent and re-park. So `seen` records an owner the moment one of
// its runs is LOWERED, and every later run of that owner is skipped THIS tick; the next
// tick (~15s later) takes the next one once the first has actually claimed. seen is set
// on the LOWERING, not on mere consideration, so a pinned/default/unknown run (skipped
// below) never consumes an `auto` sibling's slot. ListLimitWaitReeval returns oldest park
// first, matching resumePoolWaitRuns' stagger.
//
// Per-owner reads are deduped: ListAutoSelectCandidates and the judge-binding read fire at
// most once per distinct owner per tick. A per-owner read fault is logged and skipped
// (best-effort, like resumePoolWaitRuns); a ListLimitWaitReeval error fails the pass.
func (s *Service) reEvaluateParkedLimitWaitRuns(ctx context.Context, now time.Time) (int64, error) {
	rows, err := s.q.ListLimitWaitReeval(ctx, pgconv.Time(now))
	if err != nil {
		return 0, fmt.Errorf("list limit-wait reeval runs: %w", err)
	}
	seen := make(map[uuid.UUID]bool, len(rows))
	candsByOwner := make(map[uuid.UUID][]autoselect.Candidate, len(rows))
	judgeByOwner := make(map[uuid.UUID]string, len(rows))
	var lowered int64
	for _, r := range rows {
		if seen[r.UserID] {
			continue
		}
		// The minimal Run/Worker the two pure policies read — nothing else is projected.
		run := store.Run{
			Kind:                       r.Kind,
			UserID:                     r.UserID,
			WorkerID:                   r.WorkerID,
			CredentialOverrideMode:     r.CredentialOverrideMode,
			CredentialOverrideSecretID: r.CredentialOverrideSecretID,
			LimitDeadSecretID:          r.LimitDeadSecretID,
			RetryNotBefore:             r.RetryNotBefore,
		}
		var wkr store.Worker
		if r.WorkerBindMode.Valid {
			// The LEFT JOIN matched, so the recorded worker exists and this is its bind
			// mode. A NULL worker_bind_mode (absent/deleted worker) leaves wkr zero-valued,
			// which effectiveNextClaimMode maps to `unknown` — skipped below, the safe way.
			wkr = store.Worker{ID: uuid.UUID(r.WorkerID.Bytes), AnthropicBindMode: r.WorkerBindMode.String}
		}
		// self_improve follows the owner's judge binding; every other kind ignores it. The
		// read is deduped per owner. A read fault skips this run (best-effort) without
		// consuming the owner's slot.
		ownerJudgeMode := ""
		if run.Kind == runkind.SelfImprove {
			m, cached := judgeByOwner[r.UserID]
			if !cached {
				var jerr error
				if m, jerr = s.ownerJudgeBindMode(ctx, r.UserID); jerr != nil {
					slog.Error("sweeper: limit-wait reeval judge-binding read failed", "user", r.UserID, "error", jerr)
					continue
				}
				judgeByOwner[r.UserID] = m
			}
			ownerJudgeMode = m
		}
		// Skip unless the NEXT claim is `auto`: a pinned/default/unknown resume would ignore
		// a pooled alternative and re-park, so lowering it early only burns the wait budget.
		// A per-run skip, NOT a per-owner one — it must not mark the owner seen.
		if effectiveNextClaimMode(run, ownerJudgeMode, wkr) != BindModeAuto {
			continue
		}
		// The dead credential to exclude from its own replacement (window still closed while
		// parked). NextAvailable refuses a uuid.Nil exclude, so a relaxed one skips.
		dead := s.claimExcludeFor(r.LimitDeadSecretID, r.RetryNotBefore)
		if dead == uuid.Nil {
			continue
		}
		cands, cached := candsByOwner[r.UserID]
		if !cached {
			candRows, cerr := s.q.ListAutoSelectCandidates(ctx, r.UserID)
			if cerr != nil {
				slog.Error("sweeper: limit-wait reeval candidate read failed", "user", r.UserID, "error", cerr)
				continue
			}
			cands = make([]autoselect.Candidate, 0, len(candRows))
			for _, cr := range candRows {
				cands = append(cands, autoselectrow.FromCandidateRow(cr))
			}
			candsByOwner[r.UserID] = cands
		}
		// The same floor Decision 6e computes at park time — a lower bound on when this user
		// can spend something OTHER than the dead credential. Lower only when it is spendable
		// NOW (floor <= now); a future floor means nothing pooled is spendable yet.
		if floor, ok := autoselect.NextAvailable(cands, dead, s.p.Autoselect, now); !ok || floor.After(now) {
			continue
		}
		lrows, lerr := s.q.LowerLimitWaitRetryNow(ctx, store.LowerLimitWaitRetryNowParams{ID: r.ID, UserID: r.UserID})
		if lerr != nil {
			// Best-effort: a single lowering fault does not sink the sweep, mirroring the
			// pool-resume promote.
			slog.Error("sweeper: limit-wait reeval lower failed", "run", r.ID, "user", r.UserID, "error", lerr)
			continue
		}
		if lrows == 0 {
			// The run moved out of limit_wait between the list and the lower (a concurrent
			// claim/cancel). Nothing lowered; do not consume the owner's one-per-tick slot.
			continue
		}
		seen[r.UserID] = true
		lowered++
	}
	return lowered, nil
}

// ReconcileCustodyReleases is the boot/periodic custody-release backstop (PRD #1296 M4,
// D3). It finds OPEN custody holds whose release is now WARRANTED but was never applied —
// the best-effort terminal release in SetState failed after a successful publication, or a
// worker uploaded a ready capture for a still-open hold without releasing it — and applies
// the idempotent release, so the hold's ON DELETE RESTRICT live FKs are nulled and normal
// reap can then delete the worker.
//
// Release is warranted ONLY on a durable disposition for THIS hold, decided in SQL by
// ListReleasableCustodyHolds: the hold's run is 'completed' AND the hold is the completed
// generation (h.generation = runs.claim_generation), or a READY capture ('available') exists
// for THIS hold. It NEVER infers success from an arbitrary terminal status — a
// 'failed'/'cancelled'/future 'partial' run with no ready capture keeps its custody for
// capture or explicit discard.
//
// It releases EXACTLY the selected hold via ReleaseCustodyHold(h.ID), NOT the whole run: a
// run can carry MORE THAN ONE open hold (a cross-worker re-claim after a transient worker loss
// opens a generation-2 hold while the crashed worker's generation-1 hold stays open), whose
// committed work is a different, uncaptured copy. Releasing per-run would null that sibling
// orphan's live FKs and let its worker be reaped, dropping the only copy of its work — so
// selection and release must agree PER HOLD. Returns the number of holds released this tick
// (summed from the per-hold execrows). A ReleaseCustodyHold error is logged and skipped so one
// stuck hold does not sink the whole reconcile — the same best-effort stance the pool-resume
// pass takes; a candidate-list read error, by contrast, fails the pass like the sibling reads.
func (s *Service) ReconcileCustodyReleases(ctx context.Context) (int64, error) {
	holds, err := s.q.ListReleasableCustodyHolds(ctx)
	if err != nil {
		return 0, fmt.Errorf("list releasable custody holds: %w", err)
	}
	var released int64
	for _, h := range holds {
		// PRD #1392 M1 (D3): stamp the per-hold release-evidence class the candidate query
		// computed alongside each hold — 'publication' (a completed run published this
		// generation's head) or 'archive' (a ready capture covers this hold's source) — so the
		// stored evidence matches the qualifier that selected the hold for release.
		n, err := s.q.ReleaseCustodyHold(ctx, store.ReleaseCustodyHoldParams{
			ID:              h.ID,
			ReleaseEvidence: pgconv.TextOrNull(h.Reason),
		})
		if err != nil {
			// Best-effort: skip this hold, keep reconciling the rest. The hold stays open and
			// the next tick retries it.
			slog.Error("sweeper: custody release failed", "hold", h.ID, "run", h.RunID, "error", err)
			continue
		}
		released += n
	}
	return released, nil
}

// publishSwept fans a sweeper-driven run transition out to the same seams a
// worker-reported transition uses: the live WS hub (PublishState) and, once the
// Slack notifier is wired behind the fan-out, the per-owner DM. Before PRD #25 M3
// these bulk transitions returned counts only and never reached the Broadcaster,
// so timeout/worker-loss failures — exactly the "failed" events a user most wants
// pushed — were silently missed. Best-effort and non-blocking (the Broadcaster
// contract), so a slow consumer never delays the sweep.
func (s *Service) publishSwept(runID uuid.UUID, status string) {
	if s.bcast != nil {
		s.bcast.PublishState(runID, status)
	}
	s.notify(runID, status)
}
