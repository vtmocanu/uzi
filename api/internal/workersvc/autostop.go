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
// THE INCIDENT THAT MOTIVATED IT. There was one active run, so the comparison set
// was empty, so the rule degrades to flag-and-notify permanently. That is the
// correct behaviour on insufficient evidence and is tested as such. On a
// single-active-run deployment M4 is the value and this is insurance — for
// multi-run instances and for workers on pre-0.10.1 images with none of Phase 1's
// client-side protection.

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
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

// 🔴 THE GUARDS ARE CITED BY WHAT THEY ASK, NOT BY ORDINAL. This file used to
// number them G0-G6 — and defined only G0, G4, G5 and G6 while its prose cited G3,
// so the scheme asserted seven slots and named four. That is the guard-tally defect
// one layer in: an ordinal scheme with gaps is a count, and a count cannot detect
// its own referent disappearing (see 00082 and specs/ai.md §367). It also collided
// with a THIRD numbering in docs/run-auto-stopped.md, whose "guard 6" was this
// file's G4 — an operator following it landed on the non-terminal re-read.
//
// autoStopKillableKinds is the set of failure classes that may reach the kill.
// The MEMBERSHIP DECISION LIVES HERE AND NOWHERE ELSE — everything downstream
// tests this set, so adding or removing a class is one line plus its test.
//
// The test that decides membership: COULD A CORRECT PRE-0.10.1 WORKER HAVE
// PRODUCED THIS IN ORDINARY OPERATION?
//
//   - unstorable — YES, unavoidably. A headless Chromium's HarfBuzz spew puts raw
//     NULs in a tool_result. That is the world, and a pre-0.10.1 worker has no
//     sanitizer and no bisector to answer it with.
//   - oversize — YES, unavoidably. A chatty run riding out a transient outage grows
//     its batch past the 1 MiB cap. No byte cap, no splitter, on that image.
//   - invalid — NO, never. Every path into it is seq<=0, an empty kind, an empty or
//     non-JSON payload, or an all-NUL kind. `kind` comes from a fixed SDK-frame
//     vocabulary, `seq` from the batcher's own accounting, the payload from
//     JSON.stringify. A streak of it means the CLIENT IS BROKEN.
//   - store — NO. 500 means "try again", which is the contract Phase 1 exists to
//     make honest, and classifyStoreError is deliberately statement-level so a 500
//     from InsertRunMessage and one from foldRunUsage are literally the same value.
//     Killing on it would destroy a run over a telemetry side-table.
//
// AUTO-STOP EXISTS TO PROTECT A CORRECT OLD CLIENT FROM THE WORLD. `invalid` is
// not the world. The same test explains for free why a 0.10.1+ worker is never
// killed by either surviving class: it sanitizes, caps, splits and bisects.
//
// 🔴 DO NOT record "excluded because a worker can choose it" as the reason for
// `invalid`. That justification does not discriminate — `{"n":1e1000000}` is
// json.Valid, survives every class the sanitizer strips, and is permanently
// unstorable (sanitize.go's own comment says so), and a worker POSTs an oversized
// body whenever it likes. Applied consistently it empties this set and deletes M5.
// The premise is false anyway: /state is mounted beside /messages and SetState's
// `failed` arm takes a worker-supplied reason, so a worker has ALWAYS been able to
// end its own run in one call. M5 adds no capability; the only delta is the label,
// and a label problem is answered by M7's failure_class field, not by a guard.
// health.go's own header already says this machinery is not a guardrail.
//
// Why the residual is acceptable: a buggy old worker looping on invalid batches is
// FLAGGED by M4 and bounded by RUN_TIMEOUT, which is the PRD's own accepted
// baseline (on a single-active-run instance M5 permanently never kills, so flag +
// RUN_TIMEOUT is already the documented outcome for the motivating incident
// itself). And decisively: a worker defect is per-BUILD, not per-RUN. Every run
// that worker touches fails identically, and the flag makes that visible across all
// of them in ~10 seconds — the signal that says roll the image. Auto-stopping them
// one at a time would make the affected runs DISAPPEAR while the broken build keeps
// claiming new ones, converting a loud, correlated, diagnosable fleet symptom into
// a trickle of individually-explained deaths. Worse than doing nothing.
var autoStopKillableKinds = map[persistFailKind]bool{
	persistFailUnstorable: true,
	persistFailOversize:   true,
}

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
	// THE OPERATOR KILL SWITCH, read once at boot. Deliberately NOT
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
	// STILL NON-TERMINAL, re-read from Postgres at decision time. Double-guarded
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
	// The kill's status set is IDENTICAL to the flag's. runningTarget is reached only
	// from healthTargetFor's "running" arm, so a run in any other status was never
	// flagged — and killing a run that was never flagged breaks the "health first,
	// kill second" ordering this whole step is placed after Sweep's detector to
	// guarantee. Without this, a run could reach the approval gate on /state (a
	// different route, which does not wedge) and be killed at ~75s while a human was
	// reading the plan, with no flag ever shown.
	//
	// AND IT EVICTS, which is the half that matters more. A STREAK IS EVIDENCE ABOUT
	// ONE RUNNING ATTEMPT; leaving `running` ends that attempt's claim on it. The
	// case that forced this: RequeueRunsOfStaleWorkers writes status='queued' and
	// KEEPS worker_id (affinity), so a wedged run whose worker died — the likely
	// shape, since a pre-0.10.1 worker's retry batch grows and OOMs — would come back
	// as a fresh attempt carrying the dead one's 20-failure streak and be killed
	// before the new worker persisted a byte. uzi had just decided that run deserved
	// another try and spent budget saying so. It also defeats the no-progress guard's
	// own purpose: a
	// 0.10.1+ worker would have bisected the poison out on the retry.
	//
	// Sweep evicts at the requeue sites directly, which is immediate; this is the
	// catch-all for every other path that resets status without a hook (Register's
	// RequeueWorkerRuns returns no ids, so it has none), bounded by one sweep tick.
	//
	// Chat coverage survives this: chat-runner.ts reports `running` before it does any
	// work, so a wedged chat run is `running` exactly like an issue run.
	if run.Status != "running" {
		s.persistFail.evict(c.runID)
		return false
	}

	// THE STREAK'S STABLE CLASS must be one auto-stop is allowed to kill on
	// (autoStopKillableKinds).
	// Checked here rather than in candidates() so the DECLINE below still fires: for
	// a non-killable class this log line is the only operator signal that a worker
	// BUILD is broken, since these runs are never stopped and their flag is erased
	// by the exit contract when RUN_TIMEOUT finally ends them.
	if !autoStopKillableKinds[c.kind] {
		if s.persistFail.shouldLogDecline(c.runID, now, autoStopWindow) {
			slog.Warn("workersvc: a run's messages cannot be persisted, but auto-stop is holding",
				"run_id", c.runID.String(),
				"run_kind", run.Kind,
				"streak", c.streak,
				"window_seconds", int(now.Sub(c.firstAt).Seconds()),
				"failure_class", c.kind.String(),
				"decision", "class_not_killable")
		}
		return false
	}

	// OTHER RUNS ARE SUCCEEDING on THIS instance inside the window (peersSucceeding).
	// When the
	// answer is zero we flag and do not kill, permanently: a rule that cannot
	// distinguish "this run is poisoned" from "the database is down" must not kill
	// runs (Decision 5).
	peers := s.persistFail.peersSucceeding(c.runID, now, autoStopWindow)
	if peers == 0 {
		// The DECLINED line, and it is the one that earns its keep: a run satisfying
		// the streak, window and no-progress guards but blocked by the comparison set is
		// the incident's own shape, and without this an
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
			Body:     pgconv.TextOrNull(autoStopBody),
			StopKind: pgconv.TextOrNull(stopKindAutoStopped),
			// PRD #503 M3: NULL — auto-stop carries no operator reason; its
			// identity is stop_kind='auto_stopped'.
			StopReason: pgtype.Text{},
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
		FailureReason: pgconv.TextOrNull(autoStopReason),
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
