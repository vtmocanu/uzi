package workersvc

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/autoselectrow"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Sweep enforces the liveness rules the workers cannot: stale workers go offline
// and their non-terminal runs are re-queued (or failed past the re-queue cap);
// claimed-but-never-started runs are reclaimed; runs past RUN_TIMEOUT are failed.
// It is called on a ticker and once immediately at boot (the orphan sweep).
func (s *Service) Sweep(ctx context.Context) (SweepResult, error) {
	now := s.now()
	staleCutoff := pgconv.Time(now.Add(-s.p.WorkerHeartbeatStale))
	claimCutoff := pgconv.Time(now.Add(-s.p.ClaimGrace))
	max := int32(s.p.RunMaxRequeues) //nolint:gosec // G115: RunMaxRequeues is a small bounded config int (env RUN_MAX_REQUEUES), never near int32 range

	var res SweepResult
	var err error

	if res.WorkersOffline, err = s.q.MarkStaleWorkersOffline(ctx, staleCutoff); err != nil {
		return res, fmt.Errorf("mark stale workers offline: %w", err)
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

	// PRD #122 M2 (Decision 5b): a PER-RUN cutoff now — the sweep honours each run's
	// persisted budget_wall_seconds, falling back to the global RUN_TIMEOUT for a
	// NULL-budget run, so a scaled run is not failed at the global 2h.
	timedOut, err := s.q.SweepRunningTimeout(ctx, store.SweepRunningTimeoutParams{
		FailureReason:        pgconv.TextOrNull("run exceeded RUN_TIMEOUT"),
		Now:                  pgconv.Time(now),
		GlobalTimeoutSeconds: int32(s.p.RunTimeout.Seconds()),
	})
	if err != nil {
		return res, fmt.Errorf("sweep running-timeout: %w", err)
	}
	res.RunningTimeout = int64(len(timedOut))
	for _, r := range timedOut {
		s.publishSwept(r.ID, r.Status)
		// PRD #46 Decision 2: a swept-to-failed run (timed out) is committed-terminal
		// and worth judging. Best-effort, gated (kind/toggles/token) inside.
		s.maybeEnqueueJudgeByID(ctx, r.ID)
	}

	// Fail-over-cap before re-queue: the two are disjoint on requeue_count, but
	// failing first keeps a run that just hit the cap from being re-queued.
	failed, err := s.q.FailRunsOfStaleWorkersOverCap(ctx, store.FailRunsOfStaleWorkersOverCapParams{
		FailureReason: pgconv.TextOrNull("worker lost; exceeded re-queue budget"),
		MaxRequeues:   max,
		Cutoff:        staleCutoff,
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

	// Reactive pool resume (PRD #754 M5): a pool_wait hold is released the moment its
	// owner's Anthropic token pool becomes non-empty again. Scoped to pool_wait ONLY —
	// never folded into PromoteLimitWaitRuns, whose clock-based predicate is a different
	// hold. Placed here for the same reason the limit promote is: transitions first, a
	// run resumed before the detector runs is health-visible in THIS tick.
	if res.PoolResumed, err = s.resumePoolWaitRuns(ctx); err != nil {
		return res, fmt.Errorf("resume pool-wait runs: %w", err)
	}

	// Bound the in-process persistence-failure tracker (PRD #108 M4). This is the
	// memory bound for the one case no other eviction path reaches: a run whose
	// worker vanished without the run ever reaching terminal. Pruned BEFORE the
	// detector so a flag is never raised off an entry this tick was going to expire.
	s.persistFail.prune(now)

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
		// The pool is "non-empty / resumable" iff at least one candidate is AutoEligible.
		// This is Select's PoolNonEmpty (membership counted with NO exclude), whereas the
		// re-claim decides with autoselect.Floor(cands, claimExclude(run)) — Floor.ok, counted
		// AFTER the run's dead-credential exclude. They can only diverge when a run's SOLE
		// AutoEligible token is its own still-excluded dead credential, i.e. claimExclude
		// returns non-Nil. But claimExclude excludes only while retry_not_before is in the
		// FUTURE, and a pool_wait run can never carry a future stamp: SetRunPoolWait does not
		// set retry_not_before, and the claim it came from was itself claimable, so the run's
		// stamp was already NULL (never parked) or in the past (promoted out of limit_wait at
		// retry_not_before <= now). So claimExclude relaxes to Nil at every real resume, making
		// PoolNonEmpty here exactly Floor.ok at re-claim — this trigger never resumes a run that
		// would immediately re-hold.
		poolNonEmpty := false
		for _, row := range rows {
			if autoselectrow.FromCandidateRow(row).AutoEligible {
				poolNonEmpty = true
				break
			}
		}
		if !poolNonEmpty {
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
