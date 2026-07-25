package workersvc

// Guarded auto-stop for a confirmed per-run persistence loop (PRD #108 M5).
//
// WHY THIS IS A SEPARATE SWEEP STEP AND NOT A FIFTH ARM OF detectRunHealth.
// Three independent code facts force it, and together they are also what makes
// "ship M4, hold M5" a real option rather than a hope:
//
//  1. Decision 8 — detectRunHealth returns 0 immediately when HealthEnabled is
//     false. An arm inside it would make an admin's health toggle silently disable
//     loop protection. The FLAG may ride that toggle; the availability fix must not.
//  2. Chat coverage — ListActiveRunsForHealth ends `AND kind <> 'chat'`, so
//     detectRunHealth can never see a chat run, while agent/src/chat-runner.ts
//     builds the same MessageBatcher against the same POST /messages route. A chat
//     run wedges identically and the PRD requires it covered. THE ASYMMETRY THAT
//     FOLLOWS IS REAL AND DOCUMENTED: a wedged chat run is auto-stopped but never
//     flagged. Widening that query would flag every legitimately-parked chat.
//  3. Cost — the candidate set is the in-process map, which is normally empty, so
//     this step costs ZERO queries in the common case. An arm inside the health
//     loop would run per active run per tick.
//
// HONEST SCOPE, and it must not be oversold in review: THIS WOULD NOT HAVE FIRED ON
// THE INCIDENT THAT MOTIVATED IT. There was one active run, so G4's comparison set
// was empty, so the rule degrades to flag-and-notify permanently. That is the
// correct behaviour on insufficient evidence and is tested as such. On a
// single-active-run deployment M4 is the value and this is insurance — for
// multi-run instances and for workers on pre-0.10.1 images with none of Phase 1's
// client-side protection.

import (
	"context"
	"log/slog"
	"time"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// Auto-stop KILL thresholds (PRD #108 §4), code constants rather than settings.
//
// Not settings on purpose: Decision 8 forbids the kill riding the health toggle,
// and putting its thresholds in the same settings cache that owns health_enabled
// would re-create exactly the coupling that decision exists to prevent. The
// operator control is UZI_AUTOSTOP_ENABLED, which is boot-read env.
//
// They sit strictly ABOVE the flag thresholds and detectRunHealth runs first in
// Sweep, so "health first, kill second" is ordered by construction: the flag lands
// at least three sweep ticks before a kill can.
const (
	// autoStopStreak is ~10s of failures at the incident's observed ~2 Hz.
	autoStopStreak = 20
	// autoStopWindow rides out a pool exhaustion or a pod eviction.
	autoStopWindow = 60 * time.Second
	// autoStopEscalateAfter bounds the live-poller half. There is no acknowledgement
	// channel for a steering input (route's cancel arm reports nothing back), so a
	// worker whose steering loop is itself wedged would otherwise ride to RUN_TIMEOUT
	// — 2 hours, against this PRD's ~2 minute goal. 60s is 20 steering polls for an
	// issue run (runner.ts pollMs ?? 3_000) and 60 for a chat (WORKER_CHAT_POLL_MS).
	//
	// It escalates UNCONDITIONALLY rather than re-testing liveness: if the worker
	// honoured the cancel it has already reported terminal, and FailRunAutoStop's
	// status scope makes the write a no-op. The SQL is the guard, not the timing.
	autoStopEscalateAfter = 60 * time.Second
)

// stopKindAutoStopped is the CONTRACT (00082). failure_reason is not: on the
// live-poller half the worker's own SetRunFailed overwrites it unconditionally with
// REASON_CANCELLED ("run cancelled"), so the two halves genuinely carry different
// strings and only this value survives both. Nothing may parse failure_reason —
// which is the same lesson 00050's own comment records.
const stopKindAutoStopped = "auto_stopped"

// autoStopReason is human prose, like every other failure_reason in this codebase
// ("worker lost; exceeded re-queue budget", "run exceeded RUN_TIMEOUT", "run
// cancelled"). The PRD's `message_persist_permanent` would have been the only
// identifier-shaped value in a column both the CLI (FAILURE_REASON) and the web
// render verbatim to humans — and per the paragraph above it is decoration anyway.
const autoStopReason = "uzi stopped this run: its updates could not be saved"

// autoStopBody is written into the synthetic cancel input for AUDIT ONLY. The
// worker never reads it: SteeringChannel.route's `case "cancel"` calls abort() and
// ignores the body, in both the issue and chat steering classes.
const autoStopBody = "stopped automatically: this run's messages could not be persisted"

// autoStopWedgedRuns evaluates every run whose failure streak has reached the kill
// thresholds and stops the ones that clear all the guards. Returns how many runs it
// acted on. Best-effort in the sweep's style: every failure is logged and skipped,
// never returned, so an auto-stop hiccup cannot fail the sweep.
func (s *Service) autoStopWedgedRuns(ctx context.Context, now time.Time) int64 {
	// G0 — the operator kill switch, read once at boot. Deliberately NOT
	// health_enabled (Decision 8), and deliberately not a settings key: an automatic
	// destructive behaviour needs an off switch that does not depend on the database
	// it might be misbehaving against. Mirrors UZI_HOME_RECLAIM, the precedent Phase 1
	// set for exactly this shape.
	if !s.p.AutoStopEnabled {
		return 0
	}
	cands := s.persistFail.candidates(now, autoStopStreak, autoStopWindow)
	if len(cands) == 0 {
		return 0
	}
	var stopped int64
	for _, c := range cands {
		if s.evaluateAutoStop(ctx, now, c) {
			stopped++
		}
	}
	return stopped
}

// evaluateAutoStop runs the remaining guards for one candidate and takes the
// action they allow. Returns whether the run was stopped or a stop was requested.
func (s *Service) evaluateAutoStop(ctx context.Context, now time.Time, c persistFailCandidate) bool {
	// G6 — still non-terminal, re-read from Postgres at decision time. Double-guarded
	// on purpose: this read closes the ordinary case and FailRunAutoStop's status
	// scope closes the race between the read and the write, the same pattern
	// SetRunHealth's Status param uses for the exit race.
	run, err := s.q.GetRunByID(ctx, c.runID)
	if err != nil {
		slog.Error("workersvc: auto-stop could not re-read the run", "run_id", c.runID.String(), "error", err)
		return false
	}
	if terminalStatuses[run.Status] {
		s.persistFail.evict(c.runID)
		return false
	}

	// G4 — other runs are succeeding on THIS instance inside the window. When the
	// answer is zero we flag and do not kill, permanently: a rule that cannot
	// distinguish "this run is poisoned" from "the database is down" must not kill
	// runs (Decision 5).
	peers := s.persistFail.peersSucceeding(c.runID, now, autoStopWindow)
	if peers == 0 {
		// The DECLINED line, and it is the one that earns its keep: a run satisfying
		// G1-G3 but blocked by G4 is the incident's own shape, and without this an
		// operator sees a `looping` flag and nothing else, forever.
		if s.persistFail.shouldLogDecline(c.runID, now, autoStopWindow) {
			slog.Warn("workersvc: a run's messages cannot be persisted, but auto-stop is holding",
				"run_id", c.runID.String(),
				"run_kind", run.Kind,
				"streak", c.streak,
				"window_seconds", int(now.Sub(c.firstAt).Seconds()),
				"failure_class", c.kind.String(),
				"peers_succeeding", 0,
				"decision", "no_comparison_set")
		}
		return false
	}

	live, err := s.hasLivePoller(ctx, run)
	if err != nil {
		slog.Error("workersvc: auto-stop could not resolve the run's poller", "run_id", c.runID.String(), "error", err)
		return false
	}

	switch {
	case c.stopReqAt.IsZero() && live:
		// Half A. The verdict rides kind='cancel' — the path the incident's own
		// 3-second abort proved — so version skew is impossible by construction: the
		// only thing on the wire is a cancel every worker version has always
		// understood. Its data-modifying CTE stamps runs.stop_kind in the same
		// statement, so the identity cannot be lost independently of the request.
		if _, err := s.q.CreateStopVerdictInput(ctx, store.CreateStopVerdictInputParams{
			RunID:    c.runID,
			Kind:     "cancel",
			Body:     pgText(autoStopBody),
			StopKind: pgText(stopKindAutoStopped),
		}); err != nil {
			slog.Error("workersvc: auto-stop could not enqueue the stop verdict", "run_id", c.runID.String(), "error", err)
			return false
		}
		s.persistFail.markStopRequested(c.runID, now)
		s.logAutoStop("verdict_enqueued", run, c, now, peers, live)
		return true

	case c.stopReqAt.IsZero():
		// Half B. No live poller, so no one will ever consume a verdict: take the
		// terminal transition server-side.
		return s.failRunAutoStop(ctx, run, c, now, peers, live, "server_side_failed")

	case now.Sub(c.stopReqAt) >= autoStopEscalateAfter:
		// The escalation. See autoStopEscalateAfter — the status-scoped write is what
		// makes this safe to fire without re-testing whether the worker complied.
		return s.failRunAutoStop(ctx, run, c, now, peers, live, "escalated")
	}
	return false
}

// failRunAutoStop performs the server-side terminal transition and the side effects
// every other server-side failed path performs. Omit either and the run rots in the
// wrong board column with no judge.
func (s *Service) failRunAutoStop(ctx context.Context, run store.Run, c persistFailCandidate, now time.Time, peers int, live bool, action string) bool {
	rows, err := s.q.FailRunAutoStop(ctx, store.FailRunAutoStopParams{
		ID:            c.runID,
		FailureReason: pgText(autoStopReason),
	})
	if err != nil {
		slog.Error("workersvc: auto-stop could not fail the run", "run_id", c.runID.String(), "error", err)
		return false
	}
	if rows == 0 {
		// The run reached terminal between the evaluator's read and this write. Nothing
		// happened, which is the correct outcome and the reason the escalation above is
		// unconditional.
		s.persistFail.evict(c.runID)
		return false
	}
	s.publishSwept(c.runID, "failed")
	// PRD #46 Decision 2: a committed-terminal run is worth judging. Best-effort,
	// gated (kind/toggles/token) inside.
	s.maybeEnqueueJudgeByID(ctx, c.runID)
	s.persistFail.evict(c.runID)
	s.logAutoStop(action, run, c, now, peers, live)
	return true
}

// logAutoStop is M7: ONE structured line per auto-stop decision, carrying every
// input an operator needs to reconstruct it without the process.
//
// There is no metrics surface in this api to put a counter on — re-measured at
// 6be9f542, `grep -rn 'promhttp|prometheus|/metrics' --include='*.go' api/`
// (excluding _test) returns zero lines and `grep -n prometheus api/go.mod` returns
// nothing. No import, no route, no dependency, so there is no dashboard and no
// alert; a metrics endpoint is its own PRD (Decision 10). These log lines are what
// an operator keys on instead, and docs/ names them verbatim.
//
// NEVER logged here: the payload, the SQLSTATE Message/Detail (worker-controlled
// bytes — the comment in appendMessages explains why it logs pgErr.Code only), or
// the run's failure_reason. Nothing else in this design logs per FAILURE either:
// appendMessages already emits one WARN per unstorable insert, and at 2 Hz a second
// per-failure line would be 7,200 an hour.
func (s *Service) logAutoStop(action string, run store.Run, c persistFailCandidate, now time.Time, peers int, live bool) {
	slog.Warn("workersvc: auto-stopping a run whose messages cannot be persisted",
		"run_id", c.runID.String(),
		"run_kind", run.Kind,
		"action", action,
		"streak", c.streak,
		"window_seconds", int(now.Sub(c.firstAt).Seconds()),
		"failure_class", c.kind.String(),
		"last_seq", c.lastSeq,
		// The comparison-set SIZE — the guard operators doubt most, and the one that
		// explains a kill that did NOT happen.
		"peers_succeeding", peers,
		"live_poller", live,
	)
}
