// The steering channel (PRD #4 §Workflow: plan gate, follow-ups, cancel).
//
// One run has one poller of the read-only `GET /inputs`. It holds each batch until
// ACK, routing and applied receipt settle, then reads again. It routes by kind:
//
//   approve_plan / reject_plan  → resolves the plan-gate verdict the executor awaits
//   revise_plan                 → (PRD #41) enqueues the user's feedback for a plan
//                                 revision round and wakes the gate; the executor runs
//                                 a fresh plan turn and gates again
//   cancel                      → aborts the run's AbortController (the SDK executor's
//                                 watchdog trips on ctx.signal → the turn ends with
//                                 "run cancelled") AND resolves any pending verdict
//   follow_up                   → queued FIFO, injected into the NEXT loop turn via
//                                 SDK session resume (bottega's model — no mid-turn
//                                 injection)
//
// The verdict is buffered if it arrives before the executor reaches the gate, so
// there is no lost-wakeup race between "post awaiting_approval" and "await verdict".
//
// PRD #41 plan revision (Decisions 2 & 3): the gate can be re-entered N times (one per
// revision round) under a SINGLE approval budget. Every buffered verdict and queued
// revise carries the GATE EPOCH it arrived under — the monotonic counter the gate bumps
// (bumpEpoch) at each awaiting_approval (re-)report. awaitGateEvent(epoch) applies only
// events stamped at that epoch (cancel is epoch-exempt), so a verdict written against a
// stale plan version is discarded with a feed notice rather than acting on the wrong
// plan. A current-epoch revise beats a current-epoch approve/reject, so a batched
// [revise, approve] yields one revision round and a fresh gate — never an approve of the
// pre-feedback plan.

import { RequestError, type InputReceipt, type WorkerClient } from "./client.js";
import type { FollowUpOutcome } from "./executor.js";
import type { Logger } from "./log.js";
import { parseAgentSelection, type AgentSelectionParse, type UserInput } from "./protocol.js";
import { errMessage, sleep } from "./util.js";

/** The outcome of the plan-approval gate. On approve, `selection` is the parsed
 *  `approve_plan` body (PRD #37): the executor resolves it against the run's
 *  detected roster (absent → run default; malformed → own, never repo). A `revise`
 *  (PRD #41) carries the user's feedback: the executor runs a fresh plan turn with it
 *  and re-enters the gate (approve/reject/cancel are terminal; revise is not). */
export type PlanVerdict =
  | { kind: "approve"; selection: AgentSelectionParse }
  | { kind: "reject"; reason: string }
  | { kind: "cancel" }
  | { kind: "revise"; feedback: string };

/** The resolution of an ask_user park (PRD #88 M1). `cancel` is the same abort the
 *  plan gate sees; it is question-identity-exempt and always wins. */
export type AnswerVerdict =
  { kind: "answer"; answers: string[] } | { kind: "cancel" };

export interface SteeringOptions {
  /** Injectable sleep so tests can drive the poll loop deterministically. */
  sleep?: (ms: number, signal?: AbortSignal) => Promise<void>;
  /** PRD #41: emit a short feed notice (as a worker `status` message) when a verdict
   *  or revision is DISCARDED for being stamped against a stale plan version. Injected
   *  by the runner (wired to the batcher) so the channel never reaches into runner
   *  internals; optional so tests can omit it. */
  notify?: (text: string) => void;
  /** PRD #517 M3: injectable clock so the interactive follow-up park's idle window is
   *  provable without real time (mirrors ChatSteeringOptions.now). Default Date.now. */
  now?: () => number;
  /** PRD #1247 M5b: the claim-lane generation THIS claim holds. A `credential_switch {generation}`
   *  signal on an /inputs poll is acted on ONLY when its generation === this value — a signal for a
   *  superseded (already-reclaimed) claim is ignored. Absent ⇒ 0, which no real fenced claim uses,
   *  so a channel constructed without it never trips a switch (the behaviour of every test and
   *  chat path that omits it). Threaded from the flight by the runner. */
  claimGeneration?: number;
  /** Issue #1673: how long a routed batch may wait for its applied receipt on the active claim
   *  before the channel gives up (default ACTIVE_APPLY_DEADLINE_MS). Injectable for tests. */
  receiptDeadlineMs?: number;
}

/** Feed notices for events discarded because they were written against a plan version
 *  that has since been revised (PRD #41 Decision 3). */
const STALE_APPROVE_NOTICE =
  "Approval ignored — the plan changed; re-send if you still want it.";
const STALE_REJECT_NOTICE =
  "Rejection ignored — the plan changed; re-send if you still want it.";
const STALE_REVISE_NOTICE =
  "Feedback ignored — it was written against an older plan version; re-send it.";
/** PRD #88: an answer naming a question that is no longer the open one. The common
 *  cause is benign — a Slack reply to question N arriving after the lead asked N+1. */
const STALE_ANSWER_NOTICE =
  "Answer ignored — it was written against an earlier question; re-send it.";

/**
 * Parse an `answer` input body (PRD #88 M1): JSON `{ question_id, answers }`.
 *
 * Rejects rather than defaulting, deliberately unlike parseAgentSelection's
 * fallback-to-`own`. There, a fallback picks a genuinely safe default. Here the
 * payload's entire job is to say WHICH question is being answered, so an
 * unidentifiable answer has no safe reading — accepting one would resolve whatever
 * question happened to be open with text written for a different one.
 */
function parseAnswerBody(
  body: string | null | undefined,
): { answers: string[]; questionId: string } | undefined {
  if (!body || !body.trim()) return undefined;
  let raw: unknown;
  try {
    raw = JSON.parse(body);
  } catch {
    return undefined;
  }
  if (!raw || typeof raw !== "object") return undefined;
  const rec = raw as Record<string, unknown>;
  const questionId =
    typeof rec["question_id"] === "string" ? rec["question_id"].trim() : "";
  if (questionId === "") return undefined;
  const rawAnswers = rec["answers"];
  const answers = Array.isArray(rawAnswers)
    ? rawAnswers.filter((a): a is string => typeof a === "string")
    : [];
  return { answers, questionId };
}

/**
 * PRD #1190 M2: the abort reason a `now` pause uses to drop the in-flight turn. DISTINCT
 * from the steering cancel (which aborts with the default AbortError and makes the executor
 * throw Error(REASON_CANCELLED) → a `failed`/`cancelled` terminal): a pause-now must PARK the
 * run, not fail it. `route("pause","now")` aborts the shared controller WITH this as the abort
 * reason; the executor's cancel listener reads `signal.reason instanceof PauseNowSignal` and
 * trips the turn so driveTurn throws a fresh PauseNowSignal, which the implement loop's turn
 * catch takes to the pause-park path instead of the terminal cancel/failure path.
 *
 * Modeled on LimitReachedError (exported; constructed here, `instanceof`-tested and thrown in
 * sdk-executor.ts, and `instanceof`-tested in runner.ts) so it crosses files and knip sees a
 * live consumer of the export.
 */
export class PauseNowSignal extends Error {
  constructor() {
    super("run paused (now)");
    this.name = "PauseNowSignal";
  }
}

/**
 * PRD #1247 M5b: the abort/interrupt a held-state CREDENTIAL SWITCH uses to release the claim.
 * A pending switch for THIS claim's generation is delivered on an /inputs poll
 * (`credential_switch {generation}`); the poll loop trips the shared controller with this as the
 * abort reason (so the executor's cancel listener maps it to REASON_CREDENTIAL_SWITCH → a fresh
 * CredentialSwitchSignal from tripError, DISTINCT from both the cancel path and the pause path),
 * invokes the re-armable onCredentialSwitch interrupt, AND rejects any parked gate/answer/
 * follow-up waiter with this error so a run idling at the plan gate / a question / a follow-up
 * (no live SDK turn to abort) is released too. It reaches executeClaim's catch chain, which enters
 * the two-phase local release (enterCredentialSwitch) instead of failing the run — the claim is
 * handed back to the server for a reclaim on the newly-chosen token.
 *
 * Modeled on PauseNowSignal (exported; constructed here, `instanceof`-tested and thrown in
 * sdk-executor.ts, and `instanceof`-tested in runner.ts) so it crosses files and knip sees a live
 * consumer of the export.
 */
export class CredentialSwitchSignal extends Error {
  constructor() {
    super("run released for a credential switch");
    this.name = "CredentialSwitchSignal";
  }
}

/** Issue #1673: the most ids one /inputs/ack or /inputs/applied accepts (validInputIDs). */
const MAX_INPUT_BATCH = 1000;
/** Issue #1673: applied attempts a stopping channel makes for a routed batch before it leaves
 *  the batch to the next claim (which replays it: at-least-once across claims). */
const STOP_APPLY_ATTEMPTS = 3;
/** Issue #1673: consecutive failed applied attempts on an ACTIVE claim before the channel gives
 *  up. A state report waits on the applied receipt, so an unbounded retry would hold it forever. */
const ACTIVE_APPLY_ATTEMPTS = 30;
/** Issue #1673: the same bound in elapsed time, from routing. An applied request can itself take
 *  a full HTTP timeout, so the attempt count alone could hold a report for many minutes. */
const ACTIVE_APPLY_DEADLINE_MS = 60_000;

/** Issue #1673: the applied receipt for a routed batch could not be confirmed. Thrown to every
 *  state report still waiting on it and to the parked waiters, so the run fails instead of
 *  reporting a resume the server's applied-only guards would not recognise. */
class InputReceiptError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "InputReceiptError";
  }
}

/** Issue #1673: a receipt said this claim was released or superseded (or the run is terminal).
 *  The old flight's GETs would never see its inputs again, so it ends; its reports are refused
 *  by the server's claim fence. */
class ClaimFencedSignal extends Error {
  constructor(readonly reason: string) {
    super(`input claim is no longer active (${reason})`);
    this.name = "ClaimFencedSignal";
  }
}

/** Issue #1673: the claim fence a failed receipt names: a 409's `reason` ("" for a row-state
 *  conflict), a typed 404's `reason` ("stale": the run is not this worker's), undefined for
 *  anything else. Only the new route's typed body fences a claim: an untyped 404 (an api pod that
 *  predates the route, mid-roll) is an availability error and is retried. */
function receiptFenceReason(err: unknown): string | undefined {
  if (!(err instanceof RequestError) || (err.status !== 409 && err.status !== 404)) return undefined;
  let reason: unknown;
  try {
    reason = (JSON.parse(err.body) as { reason?: unknown }).reason;
  } catch {
    reason = undefined;
  }
  if (err.status === 404) return reason === "stale" ? "stale" : undefined;
  return typeof reason === "string" ? reason : "";
}

/** Issue #1673: a state report was interrupted (cancel, pause, switch or its own abort) while a
 *  routed input's applied receipt was still uncertain. Thrown instead of letting the report go
 *  out, since the server's guard would not yet see the input as applied. */
class ReceiptWaitInterrupted extends Error {
  constructor(readonly interruption: unknown) {
    super(`state report interrupted while an operator input's applied receipt was uncertain: ${errMessage(interruption)}`);
    this.name = "ReceiptWaitInterrupted";
  }
}

/** A 4xx that no retry of the same ids can fix. 404/405 (a route an older api pod lacks), 408
 *  and 429 are availability errors and retry like a 5xx. */
function definitiveReceiptError(err: unknown): boolean {
  return err instanceof RequestError && err.status >= 400 && err.status < 500 &&
    ![404, 405, 408, 429].includes(err.status);
}

/** A fenced claim ends the old flight, except a pending credential switch, whose own signal
 *  (on the next GET) releases the claim, and which may yet fail and leave the claim active. */
function endsFlight(reason: string | undefined): boolean {
  return reason === "released" || reason === "stale";
}

/** Issue #1673: a receipt response is trusted only when every row is well formed and the ids
 *  are exactly the held batch; anything else is retried like a lost reply. */
function validInput(input: unknown): input is UserInput {
  if (!input || typeof input !== "object") return false;
  const row = input as Partial<UserInput>;
  return Number.isSafeInteger(row.id) && row.id! > 0 && typeof row.kind === "string" &&
    (row.body === undefined || row.body === null || typeof row.body === "string");
}

function validBatch(inputs: unknown): inputs is UserInput[] {
  return Array.isArray(inputs) && inputs.every(validInput) &&
    new Set(inputs.map((input) => input.id)).size === inputs.length;
}

function validReceipt(receipt: InputReceipt | null | undefined, ids: number[]): boolean {
  if (typeof receipt?.active !== "boolean" || !validBatch(receipt.inputs) || receipt.inputs.length !== ids.length)
    return false;
  const expected = new Set(ids);
  return receipt.inputs.every((input) => expected.delete(input.id)) && expected.size === 0;
}

export class SteeringChannel {
  private stopped = false;
  private loop: Promise<void> | undefined;
  private readonly followUps: { id: number; body: string }[] = [];
  /** Issue #1660: the run's operator constraints, oldest first: the follow-ups earlier claims
   *  consumed (seedOperatorConstraints, from GET /follow-ups, before start), then every
   *  follow-up this claim receives (recorded on route, after an active ACK receipt). Never shifted, so the lead's FIFO delivery (followUps) is untouched and each
   *  later subagent dispatch gets the whole set (the Agent guard reads it via
   *  operatorConstraints). All follow-ups, not a subset: the operator has no way to tag a
   *  safety constraint today. */
  private readonly receivedFollowUps: { id: number; body: string }[] = [];
  /** Issue #1660: ids seeded from earlier claims, so a row the live drain also returns is
   *  recorded once. */
  private readonly seededFollowUpIds = new Set<number>();
  /** IDs queued for the lead on this claim; replayed ACK responses route once. */
  private readonly leadQueuedIds = new Set<number>();
  /** Issue #1673: every input id this channel has routed. A batch replayed on the same claim
   *  (its applied receipt was given up) is ACKed and applied again but never routed twice. */
  private readonly routedIds = new Set<number>();
  /** Issue #1660: set when the claim-time reload of earlier follow-ups failed. The earlier
   *  constraints are then unknown, so operatorConstraints reports null and the Agent guard
   *  denies every dispatch for this claim. */
  private operatorConstraintsLost = false;
  /** Issue #1673: the one input batch in flight. No newer GET replaces it until its ACK and
   *  applied receipts settle: "ack" retries the same ids, "routed" is serviced by the waiters
   *  on the tick that routed it, "applied" retries the applied receipt without rerouting. */
  private held: { ids: number[]; phase: "ack" | "routed" | "applied"; hasFollowUp: boolean; heldAt: number; routedAt?: number } | undefined;
  /** Consecutive failed ACK attempts for the held batch; bounded like the applied receipt. */
  private ackFailures = 0;
  /** Applied attempts made since stop(); bounded by STOP_APPLY_ATTEMPTS. */
  private stopApplyAttempts = 0;
  /** Consecutive failed applied attempts on the active claim; bounded by ACTIVE_APPLY_ATTEMPTS. */
  private applyFailures = 0;
  /** Set once the applied receipt is given up on an active claim; every later wait throws it. */
  private receiptFailure: InputReceiptError | undefined;
  /** Why the flight ended from this channel (a given-up receipt or a fenced claim): a park
   *  armed after that rejects at once instead of waiting on a poll loop that has stopped. */
  private flightEnded: Error | undefined;
  private readonly receiptWaiters: Array<{ resolve: () => void; reject: (err: Error) => void }> = [];
  private readonly receiptDeadlineMs: number;
  /** Why this flight was fenced (released | stale), once it was; the runner then stops quietly. */
  private fence: string | undefined;

  /** Issue #1673: resolves once no routed input awaits its applied receipt. The runner holds
   *  every non-terminal state report behind it, so the server's resume guards (which count only
   *  APPLIED inputs) see the follow-up, verdict or answer that caused the report. Rejects with
   *  InputReceiptError once the receipt is given up, so no guarded report goes out as if the
   *  input were applied. */
  async awaitReceiptSettlement(signal?: AbortSignal): Promise<void> {
    if (this.receiptFailure) throw this.receiptFailure;
    if (!this.receiptPending()) return;
    // Abort-aware: a cancel, pause or switch trip (the shared controller) or the report's own
    // signal ends the wait by REJECTING, so the report does not go out without the applied receipt
    // its server guard needs. Only the explicit `failed` path skips this wait (the runner).
    // The shared controller aborts ONCE and stays aborted: a declined `now` park restarts the turn
    // and a given-up credential switch continues in place, so its `aborted` flag says nothing about
    // this report. React to it only when it aborts DURING the wait (a listener on an already-aborted
    // signal never fires); a sticky cancel is the one earlier abort that still refuses the report.
    if (signal?.aborted) throw new ReceiptWaitInterrupted(signal.reason);
    if (this.cancelled) throw new ReceiptWaitInterrupted(this.cancel.signal.reason);
    const signals = [this.cancel.signal, signal].filter((s): s is AbortSignal => s !== undefined && !s.aborted);
    await new Promise<void>((resolve, reject) => {
      const done = (): void => {
        clearTimeout(timer);
        for (const s of signals) s.removeEventListener("abort", onAbort);
      };
      const waiter = {
        resolve: () => { done(); resolve(); },
        reject: (err: Error) => { done(); reject(err); },
      };
      const onAbort = (): void => {
        const i = this.receiptWaiters.indexOf(waiter);
        if (i >= 0) this.receiptWaiters.splice(i, 1);
        waiter.reject(new ReceiptWaitInterrupted(signals.find((s) => s.aborted)?.reason));
      };
      // Deadline-bound even while an applied request is still in flight (a slow or hung reply).
      const timer = setTimeout(() => {
        if (this.receiptPending())
          this.failReceipts(new InputReceiptError(
            `could not confirm ${this.held!.ids.length} applied operator input(s) within ${this.receiptDeadlineMs} ms`,
          ));
      }, this.receiptRemainingMs());
      timer.unref?.();
      for (const s of signals) s.addEventListener("abort", onAbort, { once: true });
      this.receiptWaiters.push(waiter);
    });
  }

  /** Why this flight was fenced by a receipt (released | stale), or undefined. */
  claimFence(): string | undefined {
    return this.fence;
  }

  private receiptRemainingMs(): number {
    const since = this.held?.routedAt ?? this.now();
    return Math.max(0, this.receiptDeadlineMs - (this.now() - since));
  }

  private receiptOverdue(): boolean {
    return this.receiptRemainingMs() === 0;
  }

  private receiptPending(): boolean {
    return this.held !== undefined && this.held.phase !== "ack";
  }

  private releaseHeld(): void {
    this.held = undefined;
    this.applyFailures = 0;
    this.ackFailures = 0;
    for (const w of this.receiptWaiters.splice(0)) w.resolve();
  }

  /** Give up the applied receipt of a routed batch on an active claim: stop polling, fail every
   *  report waiting on it and every parked waiter. The rows stay unapplied for a later claim. */
  private failReceipts(err: InputReceiptError): void {
    this.log.error("steering: giving up the applied receipt; failing the run", { run_id: this.runId, error: err.message });
    this.receiptFailure = err;
    this.flightEnded = err;
    this.held = undefined;
    this.stopped = true;
    for (const w of this.receiptWaiters.splice(0)) w.reject(err);
    this.rejectParkedWaiters(err);
  }

  /** The claim is released or superseded: route nothing more and end the old flight. A waiting
   *  report is let go; the server refuses it as a stale claim. */
  private endFencedFlight(reason: string): void {
    this.log.warn("steering: input receipt claim is fenced; ending this flight", { run_id: this.runId, reason });
    const signal = new ClaimFencedSignal(reason);
    this.fence = reason;
    this.flightEnded = signal;
    this.releaseHeld();
    this.stopped = true;
    if (!this.cancel.signal.aborted) this.cancel.abort(signal);
    this.rejectParkedWaiters(signal);
  }

  /** Issue #1673: a failed receipt request. A fence reason ends the flight or drops the batch;
   *  any other 4xx drops it; a retryable failure of the applied receipt counts toward the
   *  active-claim bound. */
  private onReceiptFailure(err: unknown): void {
    if (!this.held) return;
    const reason = receiptFenceReason(err);
    if (endsFlight(reason)) return this.endFencedFlight(reason!);
    if (reason !== undefined || definitiveReceiptError(err)) {
      // After routing, a definitive refusal (a row-state conflict and the like) means the applied
      // receipt will never land: fail the waiting reports like a give-up instead of releasing
      // them unapplied. A pending switch still just drops the batch; its own path releases the claim.
      if (this.held.phase !== "ack" && reason !== "switch_pending")
        return this.failReceipts(new InputReceiptError(
          `the applied receipt for ${this.held.ids.length} routed operator input(s) was refused: ${errMessage(err)}`,
        ));
      return this.releaseHeld();
    }
    if (this.held.phase === "ack") {
      // Nothing is routed yet, so giving up only drops the batch: it blocks no report, but it
      // would block every newer GET. The next GET reads the rows again.
      const acks = ++this.ackFailures;
      if (acks >= ACTIVE_APPLY_ATTEMPTS || this.now() - this.held.heldAt >= this.receiptDeadlineMs) {
        this.log.warn("steering: giving up the ACK of a held batch; the next GET reads it again", {
          run_id: this.runId,
          attempts: acks,
          error: errMessage(err),
        });
        this.releaseHeld();
      }
      return;
    }
    if (this.held.phase !== "applied" || this.stopped) return;
    const attempts = ++this.applyFailures;
    if (attempts >= ACTIVE_APPLY_ATTEMPTS || this.receiptOverdue())
      this.failReceipts(new InputReceiptError(
        `could not confirm ${this.held.ids.length} applied operator input(s) after ${attempts} attempts: ${errMessage(err)}`,
      ));
  }

  /** issue #559 M2: the highest `follow_up` input id this channel has already handed to the
   *  executor — via pullFollowUp or awaitFollowUp/serviceFollowUp. This is the wake-guard
   *  watermark the runner reports at the interactive park (open_followup_id). Buffering a
   *  follow-up (route → push) does NOT advance it; only DELIVERY (takeFollowUp shift) does,
   *  which is what makes it race-free across the park report's DB round-trip: the value is
   *  stable while the report is in flight because the follow-up waiter is armed only AFTER
   *  the report returns. Monotone (Math.max). */
  private lastDeliveredFollowUpIdValue = 0;
  /** PRD #1416 M2: a WORKER-AUTHORITATIVE safety steer, entirely separate from the follow-up
   *  machinery. It originates IN-PROCESS (the runner's divergence detection arms it via
   *  pushSafetySteer), NOT from a server input — so it is deliberately NOT wired into route(),
   *  does NOT carry a server input id, does NOT advance the follow-up wake-guard watermark
   *  (lastDeliveredFollowUpIdValue), and is NOT consulted by hasPendingFollowUpOutcome() or
   *  awaitFollowUp() (D3). Both executors drain it with priority at their loop top and render it
   *  as worker guidance (NOT untrusted <follow_up> user text). Single slot, latest-wins: a later
   *  arm overwrites an unconsumed one (the body is regenerated from the current tip, so the newest
   *  is the truthful one). undefined ⇒ nothing armed. */
  private safetySteer: string | undefined;
  /** The current gate epoch (PRD #41): bumped at each awaiting_approval (re-)report.
   *  Every buffered verdict / queued revise is stamped with the epoch it arrived at, so
   *  a verdict written against a superseded plan version is detectable. Starts at 0 so a
   *  gate that never bumps (backward-compat awaitVerdict) still matches epoch-0 inputs. */
  private gateEpoch = 0;
  /** An approve/reject that arrived before the executor asked for one (no lost wakeup),
   *  stamped with the epoch it landed under. Latest-wins if several land before a read. */
  private bufferedVerdict: { verdict: PlanVerdict; epoch: number } | undefined;
  /** Cancel is sticky and epoch-exempt: once seen it always wins, at any epoch. Also read by the
   *  executor's loop-top cancel re-check (PRD #1190 rework, via ctx.cancelRequested → isCancelled):
   *  a cancel that arrives AFTER the shared abort controller was already spent by a declined
   *  `now`-park cannot re-fire the turn abort (an AbortController fires once and its once-listener
   *  is gone), so the implement loop re-reads this durable flag each iteration and takes the
   *  terminal cancel path. Before the rework such a late cancel was silently dropped. */
  private cancelled = false;
  /** PRD #517 M4: a graceful `stop` is sticky. Once seen it ends an interactive park with
   *  { kind:"ended", reason:"stopped" }, serviced AHEAD of a buffered follow-up (an explicit
   *  stop wins over a queued turn). DISTINCT from `this.stopped` (~:112), which means "the
   *  poll loop should stop" — reusing that would kill the poll loop before the park resolves. */
  private stopRequested = false;
  /** PRD #1190 M2: the sticky owner-requested pause mode, or null when none is pending. Set by
   *  a `pause` input (route), cleared by `pause_cancel`, and seeded from the claim on a resume
   *  (seedPauseRequested). Sticky like stopRequested (~:137): it survives across turns until the
   *  worker parks or the owner withdraws it. The actual park BOUNDARY is decided server-side
   *  (the running-report ACK's pause_requested); this flag RECORDS the pending mode so the
   *  executor can honour a seeded/steered pause at its FIRST loop boundary as an ACK-independent
   *  fallback (getPauseMode → ctx.pauseModeRequested, PRD #1190 rework N1). The immediate turn-drop
   *  of a `now` pause is done by the abort + the re-armable interrupt in route(), NOT by this flag.
   *  DISTINCT slot from stopRequested — a stop and a pause are independent.
   *
   *  PRD #1497 M2: `wall` joins the union — the SYSTEM-authored pause the sweep files when a run
   *  reaches its wall-clock limit (a `pause` input whose body is "wall"). It aborts the in-flight
   *  turn exactly like `now` (route below), but it is STICKY-STICKY: `pause_cancel` cannot clear it
   *  (only the owner extending — cleared via clearWallMode after a REFUSED wall_park — or the park
   *  landing does), because it is the server's involuntary park, not an owner request the owner may
   *  withdraw. The mode is read via getPauseMode()/ctx.pauseModeRequested(); the PauseNowSignal that
   *  drops the turn carries NO mode, so the executor branches on getPauseMode() to route a `wall`
   *  abort to the capture-first wall park instead of handlePausePark. */
  private pauseMode: "milestone" | "now" | "wall" | null = null;
  /** PRD #1190 rework (N2): a RE-ARMABLE interrupt the executor registers (via ctx.onPauseNow) so a
   *  `now` pause can drop the in-flight turn EVERY time — not only the first. The shared cancel
   *  AbortController fires 'abort' exactly once, so a SECOND `now` after a declined park (which
   *  already aborted it) cannot re-fire it; this callback is invoked on every `now` pause instead,
   *  and the executor's trip() is first-wins-per-turn, so re-calling it after the loop cleared the
   *  prior trip re-arms the drop. The shared-controller abort is KEPT alongside it as the
   *  pre-registration safety net for the very first `now` (before the executor registers this). */
  private pauseNowInterrupt: (() => void) | undefined;
  /** PRD #1247 M5b: the generation of a held-state credential switch pending for THIS claim, or
   *  undefined when none is pending. Set ONCE by maybeTripCredentialSwitch on the first matching
   *  `credential_switch` poll (idempotent — every later tick with the same pending switch is a
   *  no-op), read by the runner (pendingCredentialSwitch) so enterCredentialSwitch knows which
   *  generation it is releasing. Never reset within a flight: the flight ends (release or give-up)
   *  the moment it is set. */
  private pendingSwitchGeneration: number | undefined;
  /** PRD #1247 M5b (MAJOR-6 rework): while > 0, a matching credential_switch signal is DEFERRED —
   *  maybeTripCredentialSwitch returns WITHOUT setting pendingSwitchGeneration and WITHOUT aborting,
   *  so the signal (which rides every poll) is honored at the NEXT trip point after the window ends.
   *  The executor opens this window around a plan-REVISION planning turn, whose new plan is not yet
   *  persisted: releasing mid-turn would leave the run row on the OLD plan_md, so a reclaim would
   *  re-present the superseded plan. Deferring lets the switch trip at the following gate wait
   *  instead, AFTER gatePlan has persisted the revised plan. A COUNTER (not a bool) so nested/
   *  re-entrant windows compose. */
  private switchDeferDepth = 0;
  /** PRD #1247 M5b: a RE-ARMABLE interrupt the executor registers (via ctx.onCredentialSwitch),
   *  the exact analog of pauseNowInterrupt — invoked on the matching switch so a switch drops the
   *  in-flight turn even after the shared cancel controller has already fired once (an
   *  AbortController fires 'abort' only once). The executor's callback trips the current turn with
   *  REASON_CREDENTIAL_SWITCH. */
  private credentialSwitchInterrupt: (() => void) | undefined;
  /** FIFO queue of revision feedback (PRD #41), each stamped with its arrival epoch. */
  private readonly reviseQueue: { feedback: string; epoch: number }[] = [];
  /** The gate waiter parked on awaitGateEvent, with the epoch it is waiting for. `reject` releases
   *  it with a CredentialSwitchSignal when a held-state switch trips (PRD #1247 M5b): a run idling
   *  at the plan gate has no live SDK turn to abort, so the waiter itself must reject. */
  private gateWaiter:
    { epoch: number; resolve: (v: PlanVerdict) => void; reject: (err: Error) => void } | undefined;
  /** An answer that arrived before the executor asked for one (no lost wakeup),
   *  stamped with the QUESTION ID it named. Latest-wins if several land before a read.
   *
   *  Deliberately a SEPARATE slot from bufferedVerdict, and the waiter below is a
   *  separate slot from gateWaiter (PRD #88). The plan gate and a mid-run question are
   *  mutually exclusive today, but M4 puts a pre-run ask_user adjacent to the plan
   *  gate, and one shared slot would then have two owners. Nothing about the answer
   *  path touches gateEpoch either: Runner.gatePlan decides "first gate does not bump"
   *  from its own gatedRuns set rather than from the epoch value, so a question that
   *  bumped the gate epoch would shift the first plan gate off epoch 0 while that set
   *  still said "first gate", and a verdict already queued when the gate opened would
   *  go stale. */
  private answerBuffer: { answers: string[]; questionId: string } | undefined;
  /** The answer waiter parked on awaitAnswer, with the question id it is waiting for. `reject`
   *  releases it with a CredentialSwitchSignal when a held-state switch trips (PRD #1247 M5b). */
  private answerWaiter:
    { questionId: string; resolve: (v: AnswerVerdict) => void; reject: (err: Error) => void } | undefined;
  /** PRD #517 M3: the interactive-task follow-up park waiter (single outstanding), with
   *  the idle bound and the clock reading it armed at. A DISTINCT slot from gateWaiter /
   *  answerWaiter for the same reason those are distinct (one shared slot would have two
   *  owners): an interactive task parks HERE between turns, never at the plan gate or an
   *  ask_user question, so the three are mutually exclusive and each owns its own slot. */
  private followUpWaiter:
    | { resolve: (o: FollowUpOutcome) => void; reject: (err: Error) => void; idleMs: number; parkedAt: number }
    | undefined;
  private readonly sleepFn: (ms: number, signal?: AbortSignal) => Promise<void>;
  private readonly notify: ((text: string) => void) | undefined;
  /** PRD #517 M3: clock the follow-up park's idle window measures against. */
  private readonly now: () => number;
  /** PRD #1247 M5b: the claim-lane generation this channel guards a credential switch against. */
  private readonly claimGeneration: number;

  constructor(
    private readonly client: WorkerClient,
    private readonly runId: string,
    private readonly pollMs: number,
    private readonly log: Logger,
    /** Aborted when a `cancel` input arrives; wired to the executor's ctx.signal. */
    private readonly cancel: AbortController,
    opts: SteeringOptions = {},
  ) {
    this.sleepFn = opts.sleep ?? sleep;
    this.notify = opts.notify;
    this.now = opts.now ?? Date.now;
    this.claimGeneration = opts.claimGeneration ?? 0;
    this.receiptDeadlineMs = opts.receiptDeadlineMs ?? ACTIVE_APPLY_DEADLINE_MS;
  }

  /** Seed the sticky `stop` state at construction time (issue #552 M3), before the poll
   *  loop starts. A graceful `uzi run stop` is stamped durably as runs.stop_kind='stopped'
   *  (PRD #517 M4) but the steering input may already have been applied, so a worker
   *  that dies before winding the park down loses the in-memory flag and the input never
   *  re-delivers. The claim re-delivers the durable fact as `stop_pending`; seeding it here
   *  reconstructs the same state a live `stop` input would have set (~:454), so the next
   *  interactive park resolves { kind:"ended", reason:"stopped" } immediately instead of
   *  waiting out the idle timeout. Idempotent with a later live `stop` input. */
  seedStopRequested(): void {
    this.stopRequested = true;
  }

  /** Seed the sticky pause state at construction time (PRD #1190 M2), before the poll loop
   *  starts — the direct analog of seedStopRequested. A pause request lives durably in the
   *  runs.pause_* columns and survives every requeue, but the steering input that carried it is
   *  applied on a prior claim, so a resumed worker's fresh channel starts pauseMode=null. The claim
   *  re-delivers the durable fact (pause_pending + pause_mode); seeding it here reconstructs the
   *  same state a live `pause` input would have set. The executor then reads this seeded mode at
   *  its FIRST loop boundary (ctx.pauseModeRequested → getPauseMode) and honours it as an initial
   *  pause request there, so a resumed pause PARKS at its first boundary as an ACK-INDEPENDENT
   *  fallback — a real safety net if the running-report ACK's pause_requested regresses (an
   *  older/buggy server). In the normal case the ACK also re-fires pause_requested, so the seed is
   *  redundant; making it load-bearing means a resumed pause no longer depends solely on the ACK.
   *  (It PARKS at the boundary; it does not abort a turn — there is no in-flight turn at a loop
   *  boundary. The immediate turn-drop of a live `now` pause is route()'s job.) Only seeds a
   *  recognised mode; ignores a garbage value (leaves pauseMode null). Idempotent with a later
   *  live `pause` input. */
  seedPauseRequested(mode: string | undefined): void {
    // PRD #1497 M2: `wall` is seeded too — a run re-claimed with pause_pending + pause_mode='wall'
    // (its worker died with the sweep's wall request pending, or it was resumed after a server-side
    // park) parks at its FIRST loop boundary before spending a turn (the executor reads getPauseMode
    // → routes to the wall seam), an ACK-independent fallback beside the running-report's pauseRequested.
    if (mode === "milestone" || mode === "now" || mode === "wall") this.pauseMode = mode;
  }

  /** The sticky owner-requested pause mode, or null when none is pending (PRD #1190 M2). Read in
   *  PRODUCTION by the executor at its first loop boundary (via the runner's ctx.pauseModeRequested
   *  wiring) so a seeded/steered pause parks even if the running-report ACK's pauseRequested
   *  regressed — see seedPauseRequested. Also read by the M2 tests. */
  getPauseMode(): "milestone" | "now" | "wall" | null {
    return this.pauseMode;
  }

  /** PRD #1497 M2: clear a STICKY `wall` pause mode. Called by the executor after a REFUSED
   *  wall_park (the server answered a non-`paused` status because the owner extended in the
   *  request-then-park window): the run continues, so the sticky wall mode must be cleared or the
   *  next loop boundary would route to the wall seam again. Deliberately clears ONLY when the mode
   *  is `wall` — a concurrent owner `now`/`milestone` pause set after the wall was refused must
   *  survive. Idempotent; a no-op when no wall is pending. */
  clearWallMode(): void {
    if (this.pauseMode === "wall") this.pauseMode = null;
  }

  /** True once a `cancel` input has been seen (sticky, PRD #1190 rework N2). The executor reads it
   *  at each loop boundary (ctx.cancelRequested) so a cancel that arrives after the shared abort
   *  controller was already consumed by a `now` pause is still honored — the controller cannot
   *  re-fire, so this sticky flag is the durable record the loop re-checks. */
  isCancelled(): boolean {
    return this.cancelled;
  }

  /** Register the re-armable pause-now interrupt (see pauseNowInterrupt, PRD #1190 rework N2).
   *  Last-wins; the runner wires it to the executor's per-run trip so a `now` pause drops the
   *  current turn even after the shared abort controller has been spent by an earlier `now`. */
  onPauseNow(cb: () => void): void {
    this.pauseNowInterrupt = cb;
  }

  /** Register the re-armable credential-switch interrupt (see credentialSwitchInterrupt, PRD #1247
   *  M5b). Last-wins; the runner wires it to the executor's per-run trip so a held-state switch
   *  drops the current turn even after the shared abort controller has been spent. */
  onCredentialSwitch(cb: () => void): void {
    this.credentialSwitchInterrupt = cb;
  }

  /** PRD #1247 M5b: the generation of a held-state credential switch pending for THIS claim, or
   *  undefined when none is pending. Read by the runner's CredentialSwitchSignal catch arm so
   *  enterCredentialSwitch knows which generation it is releasing (it equals this claim's
   *  generation by construction — maybeTripCredentialSwitch fires only on a generation match). */
  pendingCredentialSwitch(): number | undefined {
    return this.pendingSwitchGeneration;
  }

  /** PRD #1247 M5b (BLOCKING-3 rework): re-arm the credential-switch trip after a give-up whose
   *  stamp-clear the server POSITIVELY confirmed. Clears pendingSwitchGeneration so a LATER
   *  same-generation switch signal — a re-request the owner makes on the STILL-OPEN claim, whose
   *  generation is pinned to claim_generation for the claim's lifetime — can trip again. Without
   *  it the once-only guard in maybeTripCredentialSwitch permanently drops every subsequent signal
   *  for that claim. Call it ONLY after a CONFIRMED clear on a give-up-continue, never on a
   *  retain-and-stop (where the server stamp may still be pending). */
  rearmCredentialSwitch(): void {
    this.pendingSwitchGeneration = undefined;
  }

  /** PRD #1247 M5b (MINOR-7): the PUBLIC entry the runner's reportState closure calls to feed the
   *  state-ack's credential_switch signal into the SAME trigger the /inputs poll uses. It reuses
   *  maybeTripCredentialSwitch's guards verbatim — the generation match, the once-only idempotency,
   *  and the defer window — so the two transports compose (a switch trips at most once, whichever the
   *  ack or a poll observes first) and a failing /inputs poll can no longer disable the advertised
   *  secondary transport. */
  tripCredentialSwitch(generation: number): void {
    this.maybeTripCredentialSwitch(generation);
  }

  /** PRD #1247 M5b (MAJOR-6): open a defer window — a matching credential_switch signal is held
   *  (not tripped, not aborting) until the window closes. Used around a plan-REVISION planning turn
   *  so a switch never releases the claim before gatePlan has persisted the revised plan (which
   *  would strand the run row on the OLD plan_md and re-present the superseded plan on a reclaim).
   *  Counted, so a defer window that itself nests one composes; balanced by endCredentialSwitchDefer. */
  beginCredentialSwitchDefer(): void {
    this.switchDeferDepth++;
  }

  /** PRD #1247 M5b (MAJOR-6): close a defer window opened by beginCredentialSwitchDefer. The pending
   *  signal is NOT re-tripped here — it rides every poll, so the next poll after the window closes
   *  (the gate wait) trips it, by which point the revised plan is persisted. */
  endCredentialSwitchDefer(): void {
    if (this.switchDeferDepth > 0) this.switchDeferDepth--;
  }

  /** Start the poll loop (idempotent). Runs until stop(). */
  start(): void {
    if (this.loop) return;
    this.loop = this.pollLoop();
  }

  /** Stop polling and await the loop's exit. */
  async stop(): Promise<void> {
    this.stopped = true;
    if (this.loop) await this.loop;
  }

  /** The current gate epoch — the plan version verdicts/revises are stamped against.
   *  The runner awaits this at each round; a buffered event at a LOWER epoch is stale. */
  currentEpoch(): number {
    return this.gateEpoch;
  }

  /**
   * Advance to a new revision round (PRD #41): increment the epoch and return it. The
   * runner's gatePlan calls this AT THE awaiting_approval RE-report — immediately AFTER
   * reporting the next plan version and before it awaits the new round — NOT when the
   * revise is dequeued. The revision planning turn runs BETWEEN rounds; bumping when the
   * revise is taken would leave that whole window at the new epoch, so an approve clicked
   * mid-revision would be accepted at the v2 gate (a plan no human saw). Bumping at the
   * re-report stamps such a mid-revision approve at the PRIOR epoch, so it goes stale.
   * The FIRST round is not bumped (epoch 0), so a verdict already sitting in the consume-
   * on-read /inputs queue when the gate opens still applies to the initial plan. The
   * executor drives the revision loop through ctx.gatePlan:
   *
   *   let v = await gatePlan(planMd);          // runner: awaitGateEvent at current epoch
   *   while (v.kind === "revise") {            // feedback → run a fresh plan turn
   *     const newPlan = await revise(v.feedback);
   *     v = await gatePlan(newPlan);           // runner bumps the epoch at the v2 re-report
   *   }
   */
  bumpEpoch(): number {
    return ++this.gateEpoch;
  }

  /**
   * Resolve once the next actionable gate event for `epoch` is known (PRD #41). If one
   * already arrived it resolves immediately (buffered). Precedence, evaluated together:
   *   - a sticky `cancel` is epoch-exempt and always wins immediately;
   *   - a CURRENT-epoch revise beats a buffered current-epoch approve/reject (so a
   *     batched [revise, approve] yields the revision round, never the stale approve);
   *   - a current-epoch approve/reject applies when no current-epoch revise is pending;
   *   - PRIOR-epoch approve/reject/revise are discarded with a feed notice (they were
   *     written against a plan version that has since been revised).
   * Only one gate is awaited at a time (one plan turn is in flight per run).
   */
  awaitGateEvent(epoch: number): Promise<PlanVerdict> {
    const v = this.takeGateEvent(epoch);
    if (v) return Promise.resolve(v);
    // Issue #1673: the poll loop has stopped for good, so a new park would never be serviced.
    if (this.flightEnded) return Promise.reject(this.flightEnded);
    return new Promise<PlanVerdict>((resolve, reject) => {
      this.gateWaiter = { epoch, resolve, reject };
    });
  }

  /** Backward-compat entry: await a verdict at the CURRENT epoch (PRD #41). A caller
   *  that never bumps (epoch 0) matches epoch-0 inputs, preserving pre-revision
   *  behaviour for callers that only ever gate once. */
  awaitVerdict(): Promise<PlanVerdict> {
    return this.awaitGateEvent(this.gateEpoch);
  }

  /**
   * Resolve once the answer to question `questionId` is known (PRD #88 M1). If one
   * already arrived it resolves immediately (buffered — the lost-wakeup case is real
   * here, because the poll loop may ACK and route the
   * answer between the park being decided and this being called).
   *
   * Keyed on the question's IDENTITY, not on an epoch or an arrival ordinal, and that
   * is the whole point rather than a detail. A worker death re-queues the run; the
   * resumed worker re-parks on the same question with the same id, so an answer the
   * user submitted before the death still matches and is honoured. Every
   * when-based alternative rejects it — silently, and through an event the user
   * neither caused nor can see.
   */
  awaitAnswer(questionId: string): Promise<AnswerVerdict> {
    const v = this.takeAnswerEvent(questionId);
    if (v) return Promise.resolve(v);
    // Issue #1673: the poll loop has stopped for good, so a new park would never be serviced.
    if (this.flightEnded) return Promise.reject(this.flightEnded);
    return new Promise<AnswerVerdict>((resolve, reject) => {
      this.answerWaiter = { questionId, resolve, reject };
    });
  }

  /** Consume the answer for `questionId` if one is due, discarding a buffered answer
   *  that names a different question (with a feed notice). */
  private takeAnswerEvent(questionId: string): AnswerVerdict | undefined {
    // Cancel is sticky and question-exempt, exactly as it is for the plan gate.
    if (this.cancelled) return { kind: "cancel" };
    if (this.answerBuffer) {
      const { answers, questionId: qid } = this.answerBuffer;
      this.answerBuffer = undefined;
      if (qid === questionId) return { kind: "answer", answers };
      this.notify?.(STALE_ANSWER_NOTICE);
    }
    return undefined;
  }

  /** After routing an input, deliver to a parked answer waiter if one is now due. */
  private serviceAnswer(): void {
    if (!this.answerWaiter) return;
    const v = this.takeAnswerEvent(this.answerWaiter.questionId);
    if (!v) return;
    const w = this.answerWaiter;
    this.answerWaiter = undefined;
    w.resolve(v);
  }

  /** Shift the oldest buffered follow-up and advance the last-delivered watermark to its id
   *  (issue #559 M2). The SINGLE delivery site: every place that hands a follow-up to the
   *  executor routes through here so none can forget to advance the watermark — the same
   *  sibling-clear hazard the SQL wake-guard comments guard against. Monotone via Math.max. */
  private takeFollowUp(): { id: number; body: string } | undefined {
    const f = this.followUps.shift();
    if (f)
      this.lastDeliveredFollowUpIdValue = Math.max(
        this.lastDeliveredFollowUpIdValue,
        f.id,
      );
    return f;
  }

  /** issue #559 M2: the highest `follow_up` input id already delivered to the executor —
   *  the wake-guard watermark the runner reports as `open_followup_id` on the interactive
   *  park. Named to avoid colliding with the `lastDeliveredFollowUpIdValue` field. */
  getLastDeliveredFollowUpId(): number {
    return this.lastDeliveredFollowUpIdValue;
  }

  /** Issue #1660: the run's operator constraints, every follow-up received so far in arrival
   *  order, or null when the earlier ones could not be loaded this claim. A copy, so a caller
   *  cannot rewrite the record. */
  operatorConstraints(): readonly string[] | null {
    if (this.operatorConstraintsLost) return null;
    return this.receivedFollowUps.map((f) => f.body);
  }

  /** Issue #1660: the claim-time reload failed; report the constraints unavailable (null). */
  markOperatorConstraintsUnavailable(): void {
    this.operatorConstraintsLost = true;
  }

  /** Issue #1660: seed the constraints earlier claims consumed (GET /follow-ups, already in id
   *  order), before start(). follow_up only, blanks skipped, ahead of anything received live, and
   *  de-duplicated by input id. They are constraints only: the lead is NOT re-delivered them. */
  seedOperatorConstraints(inputs: readonly UserInput[]): void {
    // Atomic: build the whole batch first and touch channel state only once it is complete, so
    // a row that throws leaves nothing half-seeded for a retry to skip as already known.
    const known = new Set([...this.seededFollowUpIds, ...this.receivedFollowUps.map((f) => f.id)]);
    const seeded: { id: number; body: string }[] = [];
    for (const input of inputs) {
      const body = input.body?.trim();
      if (input.kind !== "follow_up" || !body || known.has(input.id)) continue;
      known.add(input.id);
      seeded.push({ id: input.id, body });
    }
    for (const f of seeded) this.seededFollowUpIds.add(f.id);
    this.receivedFollowUps.unshift(...seeded);
  }

  /** Dequeue the oldest un-consumed follow-up, or undefined if none. */
  pullFollowUp(): string | undefined {
    return this.takeFollowUp()?.body;
  }

  /** PRD #1416 M2: arm (set/replace) the worker-authoritative safety steer. Called IN-PROCESS by
   *  the runner's divergence detection, never from route(). Latest-wins: a later arm overwrites an
   *  unconsumed one — the body is regenerated from the current tip, so the newest is the truthful
   *  one. Deliberately touches NONE of the follow-up machinery (no watermark advance, no wake
   *  guard), so it can never be mistaken for a server follow-up (D3). */
  pushSafetySteer(text: string): void {
    this.safetySteer = text;
  }

  /** PRD #1416 M2: consume the worker-authoritative safety steer — return it and clear the slot,
   *  so it is delivered exactly once per arm. Both executors call this at their loop top ahead of
   *  pullFollowUp, so the steer is rendered before any follow-up. undefined ⇒ none armed. */
  pullSafetySteer(): string | undefined {
    const steer = this.safetySteer;
    this.safetySteer = undefined;
    return steer;
  }

  /**
   * True when awaitFollowUp would resolve SYNCHRONOUSLY — a cancel or stop is pending, or a
   * follow-up is already buffered from mid-turn — i.e. the run is NOT actually going idle.
   *
   * issue #552 M1: the interactive park calls this BEFORE reporting `awaiting_followup`. That
   * report stamps the open_followup_id wake-guard watermark to MAX(consumed follow_up id); a
   * follow-up consumed MID-TURN (by the poll loop, while the agent worked) is already consumed
   * but NOT yet applied, so folding it into the watermark would make its own wake `running`
   * report fail the `id > watermark` guard and strand a live run at awaiting_followup. When an
   * outcome is already in hand the park is skipped and the outcome serviced directly, so the
   * watermark is stamped only when the run genuinely idles (MAX(consumed) == last APPLIED).
   * Mirrors awaitFollowUp's own precedence (cancelled → stop → buffered follow-up) as a
   * read-only peek that consumes nothing.
   */
  hasPendingFollowUpOutcome(): boolean {
    return this.cancelled || this.stopRequested || this.followUps.length > 0;
  }

  /**
   * Park until the next follow-up for an INTERACTIVE task run (PRD #517 M3), or until the
   * park ENDS: idle after `idleMs` with no follow-up, or a cancel. Modeled on
   * ChatSteering.awaitFollowUp + serviceWaiter (route-then-service, drain-after-arm, single
   * outstanding park, channel-owned idle clock) — NOT on pullFollowUp. The waiter resolves
   * ONLY from serviceFollowUp, called by pollLoop AFTER the entire ACK batch routes.
   * The runner awaits awaitReceiptSettlement before reporting `running`, so the applied
   * receipt settles before the server's wake guard is checked.
   *
   * DRAIN-AFTER-ARM: a follow-up already buffered when this is called (it arrived in the
   * window between the executor deciding to break and arming the waiter) is returned
   * immediately, so a follow-up that races the park boundary is never lost — the drop-on-idle
   * race.
   *
   * Single outstanding park only: the executor parks exactly once per `signal_done`, and an
   * interactive park is mutually exclusive with the plan gate / an ask_user question. A
   * double-arm is a programming error, not a runtime condition — throw rather than silently
   * clobber the earlier waiter.
   */
  awaitFollowUp(idleMs: number): Promise<FollowUpOutcome> {
    if (this.followUpWaiter)
      throw new Error("steering: awaitFollowUp is already parked (double-arm)");
    // Cancel is sticky and always wins immediately, exactly as it does for the plan gate
    // and the answer waiter.
    if (this.cancelled)
      return Promise.resolve<FollowUpOutcome>({
        kind: "ended",
        reason: "cancelled",
      });
    // PRD #517 M4: a `stop` input resolves { kind:"ended", reason:"stopped" } and is serviced
    // AHEAD of a buffered follow-up (an explicit stop wins over a queued turn, Decision 5).
    // Precedence: cancelled (above) → stop (here) → buffered follow-up → idle.
    if (this.stopRequested)
      return Promise.resolve<FollowUpOutcome>({
        kind: "ended",
        reason: "stopped",
      });
    if (this.followUps.length) {
      const f = this.takeFollowUp()!;
      return Promise.resolve<FollowUpOutcome>({
        kind: "followup",
        body: f.body,
      });
    }
    // Issue #1673: the poll loop has stopped for good, so a new park would never be serviced.
    if (this.flightEnded) return Promise.reject(this.flightEnded);
    return new Promise<FollowUpOutcome>((resolve, reject) => {
      this.followUpWaiter = { resolve, reject, idleMs, parkedAt: this.now() };
    });
  }

  /** After routing an input batch, deliver to a parked follow-up waiter if one is now due
   *  (PRD #517 M3). Precedence: a cancel ends it (`cancelled`); a pending `stop` ends it with
   *  reason "stopped" AHEAD of the follow-up drain below (PRD #517 M4, Decision 5); then a
   *  buffered follow-up; then idle once `idleMs` has elapsed since it armed. Same route-THEN-
   *  service discipline as serviceGate / serviceAnswer / ChatSteering.serviceWaiter — resolving
   *  from here (post-route) delivers the full ACK batch before apply starts. */
  private serviceFollowUp(): void {
    const w = this.followUpWaiter;
    if (!w) return;
    if (this.cancelled) {
      this.followUpWaiter = undefined;
      w.resolve({ kind: "ended", reason: "cancelled" });
      return;
    }
    // PRD #517 M4: resolve { kind:"ended", reason:"stopped" } here — BEFORE the follow-up
    // drain — when a `stop` input is pending, so an explicit stop wins over a queued turn.
    // Precedence: cancelled (above) → stop (here) → buffered follow-up → idle.
    if (this.stopRequested) {
      this.followUpWaiter = undefined;
      w.resolve({ kind: "ended", reason: "stopped" });
      return;
    }
    if (this.followUps.length) {
      const f = this.takeFollowUp()!;
      this.followUpWaiter = undefined;
      w.resolve({ kind: "followup", body: f.body });
      return;
    }
    if (!this.held?.hasFollowUp && this.now() - w.parkedAt >= w.idleMs) {
      this.followUpWaiter = undefined;
      w.resolve({ kind: "ended", reason: "idle" });
    }
  }

  /** Consume the next actionable gate event for `epoch`, applying the precedence rule
   *  and discarding stale events (with a feed notice) as a side effect. Returns the
   *  verdict to deliver, or undefined when nothing is (yet) actionable. */
  private takeGateEvent(epoch: number): PlanVerdict | undefined {
    // Cancel is epoch-exempt and sticky — it always wins, at any epoch.
    if (this.cancelled) return { kind: "cancel" };
    // Drop any stale (prior-epoch) revises at the head, FIFO, noting each.
    while (this.reviseQueue.length && this.reviseQueue[0]!.epoch !== epoch) {
      this.reviseQueue.shift();
      this.notify?.(STALE_REVISE_NOTICE);
    }
    // A current-epoch revise beats a buffered current-epoch approve/reject.
    if (this.reviseQueue.length) {
      const r = this.reviseQueue.shift()!;
      return { kind: "revise", feedback: r.feedback };
    }
    // A buffered approve/reject: apply it at its own epoch, else discard as stale.
    if (this.bufferedVerdict) {
      const { verdict, epoch: e } = this.bufferedVerdict;
      this.bufferedVerdict = undefined;
      if (e === epoch) return verdict;
      // Verdict-specific notice so the feed reads correctly for either kind.
      this.notify?.(
        verdict.kind === "reject" ? STALE_REJECT_NOTICE : STALE_APPROVE_NOTICE,
      );
    }
    return undefined;
  }

  /** After routing an input, deliver to a parked gate waiter if one is now due. */
  private serviceGate(): void {
    if (!this.gateWaiter) return;
    const v = this.takeGateEvent(this.gateWaiter.epoch);
    if (!v) return;
    const w = this.gateWaiter;
    this.gateWaiter = undefined;
    w.resolve(v);
  }

  /**
   * PRD #1247 M5b: act on a `credential_switch {generation}` signal surfaced by an /inputs poll.
   *
   * ONLY when the generation matches THIS claim's (`this.claimGeneration`): a signal carrying a
   * DIFFERENT generation targets a superseded claim (the run was already reclaimed under a newer
   * generation) and MUST be ignored — acting on it would release a claim that is not this flight's.
   * IDEMPOTENT: fires exactly once per pending switch (the `pendingSwitchGeneration` guard), so the
   * continuous idle poll does not re-abort every tick.
   *
   * On a match it does three things, covering every shape a held-state run can be in when the
   * switch lands:
   *   1. records the pending switch (read later by the runner's release state machine);
   *   2. trips the shared cancel controller with a CredentialSwitchSignal reason (the mid-turn
   *      case — the executor's cancel listener maps the reason to REASON_CREDENTIAL_SWITCH) and
   *      invokes the re-armable interrupt (a turn that outlived a spent controller);
   *   3. rejects any parked gate/answer/follow-up waiter with a CredentialSwitchSignal (the idle
   *      case — a run at the plan gate / a question / a follow-up park has NO live SDK turn to
   *      abort, so the awaited promise itself must reject to release the flight).
   */
  private maybeTripCredentialSwitch(generation: number): void {
    if (generation !== this.claimGeneration) return; // a stale signal for a superseded claim
    // PRD #1247 M5b (MAJOR-6): inside a defer window (a plan-revision planning turn), DEFER — do NOT
    // set pendingSwitchGeneration and do NOT abort. The signal rides every poll, so it trips at the
    // next poll after the window closes (the gate wait), by which point the revised plan is persisted.
    if (this.switchDeferDepth > 0) return;
    if (this.pendingSwitchGeneration !== undefined) return; // already tripped once — idempotent
    this.pendingSwitchGeneration = generation;
    // Mid-turn: trip the shared controller (guarded on !aborted so we don't double-abort a
    // controller a cancel/pause already spent) and the re-armable interrupt, exactly like a `now`
    // pause. The abort reason is a CredentialSwitchSignal so the executor's cancel listener routes
    // it to REASON_CREDENTIAL_SWITCH, not REASON_CANCELLED.
    if (!this.cancel.signal.aborted) this.cancel.abort(new CredentialSwitchSignal());
    this.credentialSwitchInterrupt?.();
    // Idle at a waiter: reject it so a gate/question/follow-up park (no live turn) is released.
    this.rejectParkedWaiters(new CredentialSwitchSignal());
  }

  /** Reject every parked gate/answer/follow-up waiter with `err`. */
  private rejectParkedWaiters(err: Error): void {
    if (this.gateWaiter) {
      const w = this.gateWaiter;
      this.gateWaiter = undefined;
      w.reject(err);
    }
    if (this.answerWaiter) {
      const w = this.answerWaiter;
      this.answerWaiter = undefined;
      w.reject(err);
    }
    if (this.followUpWaiter) {
      const w = this.followUpWaiter;
      this.followUpWaiter = undefined;
      w.reject(err);
    }
  }

  private route(kind: string, body: string | null | undefined, id: number): void {
    switch (kind) {
      case "approve_plan":
        // The body carries the JSON-encoded agent selection (PRD #37). Parse it
        // here; the executor resolves it against the detected roster. A malformed
        // body parses to `invalid`, which the executor sends to `own`, never repo.
        this.bufferedVerdict = {
          verdict: { kind: "approve", selection: parseAgentSelection(body) },
          epoch: this.gateEpoch,
        };
        break;
      case "reject_plan":
        this.bufferedVerdict = {
          verdict: { kind: "reject", reason: body?.trim() || "plan rejected" },
          epoch: this.gateEpoch,
        };
        break;
      case "revise_plan":
        // PRD #41: enqueue the feedback for a revision round. Empty feedback is ignored
        // (like follow_up). The gate is serviced once per poll batch (pollLoop), AFTER all
        // inputs route, so precedence (a current-epoch revise beats a buffered approve) is
        // applied across the WHOLE batch — a [approve, revise] batch yields the revision
        // round, not a silent drop of the trailing revise.
        if (body && body.trim())
          this.reviseQueue.push({
            feedback: body.trim(),
            epoch: this.gateEpoch,
          });
        break;
      case "cancel":
        if (!this.cancel.signal.aborted) this.cancel.abort();
        this.cancelled = true;
        break;
      case "stop":
        // PRD #517 M4: a graceful wind-down of an interactive task. Sticky, like cancel, but
        // it does NOT abort ctx.signal — the current turn finishes and the park resolves
        // { kind:"ended", reason:"stopped" }, which sdk-executor finalizes normally (push +
        // MR iff open_mr → completed). serviceFollowUp/awaitFollowUp check this AHEAD of the
        // follow-up drain so an explicit stop beats a queued follow-up (Decision 5).
        this.stopRequested = true;
        break;
      case "pause": {
        // PRD #1190 M2: an owner-requested pause. The body is the mode; default to "milestone"
        // for an absent/garbage body (the safe, non-destructive mode). Set the sticky flag, and
        // for "now" ALSO drop the in-flight turn — the server decides the milestone boundary
        // (the running-report ACK) but cannot drop a turn, so only the worker can. Two mechanisms,
        // both invoked for "now" (PRD #1190 rework N2):
        //  - the shared cancel controller, aborted with a PauseNowSignal reason (DISTINCT from
        //    cancel, which uses the default AbortError) so the executor's cancel listener trips the
        //    turn as a pause, not a cancel. Guarded on !aborted so a `now` after a `cancel` does
        //    not double-abort (cancel already won, and it is terminal). This is the pre-registration
        //    safety net for the very FIRST `now` (before the executor registers the interrupt).
        //  - the re-armable pauseNowInterrupt, invoked on EVERY `now`. The shared controller fires
        //    'abort' exactly once, so a SECOND `now` after a declined park (which already aborted
        //    it) cannot re-fire it; the interrupt trips the executor's current turn each time, so
        //    the second `now` still drops the (restarted) turn instead of degrading to a
        //    milestone-boundary park. Before the rework a second `now` silently degraded.
        //
        // PRD #1497 M2: `wall` (the sweep's system-authored wall-clock park) is treated EXACTLY
        // like `now` for the turn-drop mechanism — it must abort the in-flight turn so the run parks
        // in seconds rather than at the next milestone (D3). The mode recorded is `wall`, which the
        // executor reads (getPauseMode) to route the abort to the capture-first wall park seam
        // instead of handlePausePark. Any non-"now"/"wall" body is the safe "milestone" default.
        const trimmed = body?.trim();
        const mode = trimmed === "now" ? "now" : trimmed === "wall" ? "wall" : "milestone";
        this.pauseMode = mode;
        if (mode === "now" || mode === "wall") {
          if (!this.cancel.signal.aborted)
            this.cancel.abort(new PauseNowSignal());
          this.pauseNowInterrupt?.();
        }
        break;
      }
      case "pause_cancel":
        // PRD #1190 M2: withdraw a pending pause. Clears the sticky flag; the server clears its
        // own columns via CancelPauseInput. Does NOT un-abort a turn already dropped by a prior
        // `now` (an AbortController cannot be reset) — a pause_cancel after a `now` is a rare
        // race the server's boundary ACK settles.
        //
        // PRD #1497 M2: an owner `pause_cancel` CANNOT clear a `wall` park — the server's
        // involuntary wall park is not an owner request the owner may withdraw (the M1 server-side
        // CancelPauseInput has the matching `pause_mode IS DISTINCT FROM 'wall'` guard, so the
        // columns stay set too). Only clearWallMode (a refused wall_park) or the park landing clears
        // it. A `wall` mode therefore SURVIVES a pause_cancel here.
        if (this.pauseMode !== "wall") this.pauseMode = null;
        break;
      case "follow_up":
        // issue #559 M2: carry the input id alongside the body so a delivery (takeFollowUp)
        // can advance the wake-guard watermark. The other kinds ignore the id.
        if (body && body.trim()) {
          // An ACK retry may replay this row; queue it once.
          if (!this.leadQueuedIds.has(id)) {
            this.leadQueuedIds.add(id);
            this.followUps.push({ id, body: body.trim() });
          }
          if (!this.seededFollowUpIds.has(id) && !this.receivedFollowUps.some((f) => f.id === id))
            this.receivedFollowUps.push({ id, body: body.trim() });
        }
        break;
      case "answer": {
        // PRD #88. Reaching the default arm instead would DESTROY the answer: /inputs
        // is receipt based, so this row is ACKed before routing. There would be no error, no retry, and no
        // symptom other than a run that appears to have been ignored by its user.
        //
        // The body is the JSON the API validated and re-encoded (it never stores the
        // client's raw text), so a parse failure here means a wire-shape mismatch
        // rather than user input — log it and drop, the same way a malformed
        // approve_plan selection degrades rather than throwing inside the poll loop.
        const parsed = parseAnswerBody(body);
        if (!parsed) {
          this.log.warn("steering: ignoring malformed answer body", {
            run_id: this.runId,
          });
          break;
        }
        this.answerBuffer = parsed;
        break;
      }
      default:
        this.log.warn("steering: ignoring unknown input kind", { kind });
    }
  }

  /** Issue #1673: one receipt step for the held batch. ACK first; route an active ACK's rows
   *  once, in id order; the applied receipt goes out on a later tick, after the waiters were
   *  serviced. A failed request keeps the batch and retries the same ids. */
  private async advanceHeld(): Promise<void> {
    const held = this.held!;
    if (held.phase === "applied") {
      const receipt = await this.client.applyInputs(this.runId, held.ids, this.claimGeneration);
      if (!validReceipt(receipt, held.ids)) throw new Error("invalid input applied response");
      // Settled either way: active, or already applied before the claim was fenced, in which
      // case a released or superseded claim still ends this flight.
      if (!receipt.active && endsFlight(receipt.reason)) return this.endFencedFlight(receipt.reason!);
      this.releaseHeld();
      return;
    }
    if (held.phase !== "ack") return;
    const receipt = await this.client.ackInputs(this.runId, held.ids, this.claimGeneration);
    if (!validReceipt(receipt, held.ids)) throw new Error("invalid input ACK response");
    if (!receipt.active) {
      // Route nothing; the rows belong to the next claim. A released or superseded claim ends
      // this flight. A pending credential switch keeps polling, so its signal still trips (a
      // local cancel would report the run cancelled mid-switch) and a failed switch resumes.
      if (endsFlight(receipt.reason ?? "stale")) return this.endFencedFlight(receipt.reason ?? "stale");
      this.log.warn("steering: input receipt claim has a switch pending; leaving the batch to the next claim", {
        run_id: this.runId,
        count: held.ids.length,
      });
      this.releaseHeld();
      return;
    }
    for (const input of [...receipt.inputs].sort((a, b) => a.id - b.id)) {
      if (this.routedIds.has(input.id)) continue;
      this.routedIds.add(input.id);
      this.route(input.kind, input.body ?? undefined, input.id);
    }
    held.phase = "routed";
    held.routedAt = this.now();
  }

  private async pollLoop(): Promise<void> {
    while (!this.stopped || this.held) {
      if (this.stopped && this.held) {
        // Stopping: an unrouted batch is simply left for the next claim; a routed one gets a
        // bounded number of applied attempts, never an unbounded wait on a failing API.
        if (this.held.phase === "ack" || this.stopApplyAttempts >= STOP_APPLY_ATTEMPTS) {
          if (this.held.phase !== "ack")
            this.log.warn("steering: giving up the applied receipt at stop; the next claim replays it", {
              run_id: this.runId,
            });
          this.releaseHeld();
          break;
        }
        this.held.phase = "applied";
        this.stopApplyAttempts++;
      }
      try {
        if (!this.held && !this.stopped) {
          const { inputs, credentialSwitch, receipts } = await this.client.getInputs(this.runId);
          if (!validBatch(inputs)) throw new Error("invalid input GET response");
          if (!receipts) {
            // Issue #1673: no receipt marker, so an older api pod consumed these on read; nothing
            // replays them. Route them now, in the server's FIFO order, with no ACK or APPLIED.
            for (const input of inputs) {
              if (this.routedIds.has(input.id)) continue;
              this.routedIds.add(input.id);
              this.route(input.kind, input.body ?? undefined, input.id);
            }
          } else if (inputs.length) {
            const batch = inputs.slice(0, MAX_INPUT_BATCH);
            this.held = {
              ids: batch.map((input) => input.id),
              phase: "ack",
              hasFollowUp: batch.some((input) => input.kind === "follow_up" && !!input.body?.trim()),
              heldAt: this.now(),
            };
          }
          // PRD #1247 M5b: the credential-switch signal rides EVERY inputs response (incl. an
          // empty one). It is not an input row — it is the server's "a switch is pending for the
          // current claim" fact — and acting on it releases the claim.
          if (credentialSwitch) this.maybeTripCredentialSwitch(credentialSwitch.generation);
        }
        if (this.held) await this.advanceHeld();
      } catch (err) {
        // Issue #1673: a fenced claim ends the flight; any other 4xx is definitive for these ids
        // (drop the batch, the server replays whatever is unapplied); a failing applied receipt
        // on the active claim is bounded.
        this.onReceiptFailure(err);
        // The loop continues on a failure (HTTP >=400 / timeout) — but only the request is
        // skipped, not the service step below. PRD #517 M5: serviceFollowUp()
        // evaluates the interactive park's idle clock and is called ONLY from this loop, so
        // if the service step lived inside this try a PERSISTENT run-scoped getInputs outage
        // (a 500 on ConsumeInputs, a not-owned 404 flip) concurrent with a healthy worker
        // heartbeat would starve the idle finalize forever — pinning the park at
        // awaiting_followup as a permanent zombie the heartbeat-keyed stale-worker requeue
        // never sees. Log and fall through; the service step runs regardless.
        this.log.warn("steering: input poll failed", {
          run_id: this.runId,
          error: errMessage(err),
        });
      }
      // Service the parked waiters on EVERY tick, OUTSIDE the try above, so a getInputs
      // failure cannot starve them (PRD #517 M5). serviceGate/serviceAnswer/serviceFollowUp
      // operate PURELY on in-memory state — the buffered verdict/answer/follow-up, the
      // sticky cancel/stop flags, and the channel-owned idle clock — never on the getInputs
      // result, so running them on a failed tick is safe and delivers nothing NEW: a
      // follow-up (and a verdict/answer/cancel/stop) still only ENTERS via route() from a
      // SUCCESSFUL batch, so follow-up/stop/cancel delivery semantics are unchanged. What a
      // failed tick still evaluates is the idle bound (serviceFollowUp) and any event
      // already buffered by a prior successful batch. (This mirrors ChatSteering.pollLoop,
      // which likewise services its waiter after the catch.)
      //
      // Route the WHOLE batch, THEN service once: whatever landed may now satisfy a parked
      // gate. Servicing per-input would let an approve at the head of a [approve, revise]
      // batch resolve the gate before the revise routes — the revise would then sit
      // un-consumed (a silent drop that still burned a server cap slot). One service call
      // per batch evaluates full precedence (revise beats approve) once.
      this.serviceGate();
      // Same batch-then-service-once position, for the same reason (PRD #88): an
      // answer that lands in this batch may satisfy a parked question.
      this.serviceAnswer();
      // PRD #517 M3: same position, same reason — a follow-up that lands in this batch may
      // satisfy a parked interactive-task waiter. The wake guard needs it APPLIED before the
      // resulting `running` report, which the runner holds behind awaitReceiptSettlement. Also
      // re-evaluated every idle tick so a park with no follow-up ends on its idle bound —
      // including on a tick where getInputs threw (PRD #517 M5).
      this.serviceFollowUp();
      // Issue #1673: the applied receipt starts on the next tick with these same ids, after
      // every waiter has seen the routed batch. A failed reply retries without GET or route.
      if (this.held?.phase === "routed") this.held.phase = "applied";
      if (this.stopped && !this.held) break;
      await this.sleepFn(this.pollMs);
    }
  }
}

/** What the chat park loop should do next: answer a message, idle-complete, or end
 *  (an explicit End chat, or worker shutdown). */
export type ChatInput =
  { kind: "message"; text: string } | { kind: "idle" } | { kind: "ended" };

/** The input source a ChatRunner parks on between turns (PRD #39 Decision 2). The
 *  real one is ChatSteering; tests inject a fake that yields scripted ChatInputs. */
export interface ChatInputSource {
  start(): void;
  stop(): Promise<void>;
  awaitFollowUp(idleMs: number): Promise<ChatInput>;
  /** True when a receipt definitively fences this claim's old worker. */
  claimLost?(): boolean;
}

export interface ChatSteeringOptions {
  /** Injectable sleep so tests drive the poll loop deterministically. */
  sleep?: (ms: number, signal?: AbortSignal) => Promise<void>;
  /** Injectable clock so the idle window is provable without real time. */
  now?: () => number;
}

/**
 * The chat steering channel (PRD #39 Decision 2). Like SteeringChannel it is the
 * SOLE poller of the read-only `GET /inputs`, but a chat only ever sees two
 * input kinds: `follow_up` (a user turn — including the seeded first message) and
 * `cancel` (End chat). It also OWNS the idle clock, and that ownership is the
 * load-bearing fix for the drop-on-idle race (team task #8):
 *
 *   The poll loop consumes inputs, buffers any follow_up, and THEN — in the SAME
 *   iteration, after routing — services the parked waiter, delivering a buffered
 *   message before it ever tests idle. There is no separate idle timer that could
 *   fire in the window between an ACK and the worker buffering a follow_up. A message that races the
 *   idle tick is delivered before the applied request starts.
 *
 * A `cancel` aborts the shared controller (which is the executor's ctx.signal), so
 * End chat also aborts a turn in flight, not just a parked wait.
 */
/** The server has fenced this chat claim; its old worker must not report a terminal state. */
class ChatClaimLostError extends Error {
  constructor() {
    super("chat claim is no longer active");
    this.name = "ChatClaimLostError";
  }
}

export class ChatSteering implements ChatInputSource {
  private stopped = false;
  private lost = false;
  private loop: Promise<void> | undefined;
  private readonly followUps: string[] = [];
  /** Issue #1673: the one input batch in flight, as in SteeringChannel ("ack" retries the ACK,
   *  "applied" retries the applied receipt without rerouting). */
  private held: { ids: number[]; phase: "ack" | "applied" } | undefined;
  /** Every input id routed on this claim, so a replayed batch never routes twice. */
  private readonly routedIds = new Set<number>();
  private stopApplyAttempts = 0;
  private applyFailures = 0;
  private waiter:
    | { resolve: (i: ChatInput) => void; idleMs: number; parkedAt: number }
    | undefined;
  private readonly sleepFn: (ms: number, signal?: AbortSignal) => Promise<void>;
  private readonly now: () => number;

  constructor(
    private readonly client: WorkerClient,
    private readonly runId: string,
    private readonly pollMs: number,
    private readonly log: Logger,
    /** Aborted when a `cancel` (End chat) input arrives; this IS the executor's
     *  ctx.signal, so a cancel also aborts a turn in flight. */
    private readonly cancel: AbortController,
    opts: ChatSteeringOptions = {},
    private readonly claimGeneration = 0,
  ) {
    this.sleepFn = opts.sleep ?? sleep;
    this.now = opts.now ?? Date.now;
  }

  claimLost(): boolean {
    return this.lost;
  }

  start(): void {
    if (this.loop) return;
    this.loop = this.pollLoop();
  }

  async stop(): Promise<void> {
    this.stopped = true;
    this.settle({ kind: "ended" });
    if (this.loop) await this.loop;
  }

  /**
   * Park until the next user message, or idle after `idleMs` with no message, or end
   * (cancel/stop). A follow_up already buffered (consumed during the previous turn)
   * is returned immediately. Only one park is outstanding at a time — the executor
   * parks exactly once per turn.
   */
  awaitFollowUp(idleMs: number): Promise<ChatInput> {
    if (this.cancel.signal.aborted || this.stopped)
      return Promise.resolve<ChatInput>({ kind: "ended" });
    if (this.followUps.length)
      return Promise.resolve<ChatInput>({
        kind: "message",
        text: this.followUps.shift()!,
      });
    return new Promise<ChatInput>((resolve) => {
      this.waiter = { resolve, idleMs, parkedAt: this.now() };
    });
  }

  private settle(i: ChatInput): void {
    const w = this.waiter;
    if (!w) return;
    this.waiter = undefined;
    w.resolve(i);
  }

  private route(kind: string, body: string | null | undefined): void {
    switch (kind) {
      case "follow_up":
        if (body && body.trim()) this.followUps.push(body.trim());
        break;
      case "cancel":
        if (!this.cancel.signal.aborted) this.cancel.abort();
        break;
      default:
        // approve_plan / reject_plan never occur for a chat (no plan gate).
        this.log.warn("chat steering: ignoring unexpected input kind", {
          kind,
        });
    }
  }

  /** After each poll+route, decide what a parked waiter gets: cancel/stop wins, then
   *  a buffered message, then idle — so a just-consumed follow_up is always delivered
   *  before idle can complete the chat. */
  private serviceWaiter(): void {
    if (!this.waiter) return;
    if (this.cancel.signal.aborted || this.stopped)
      return this.settle({ kind: "ended" });
    if (this.followUps.length)
      return this.settle({ kind: "message", text: this.followUps.shift()! });
    if (!this.held && this.now() - this.waiter.parkedAt >= this.waiter.idleMs)
      this.settle({ kind: "idle" });
  }

  /** The server fenced this chat claim (inactive receipt, or a 404/409): route nothing more,
   *  end the chat, and let ChatRunner skip its terminal report. */
  private loseClaim(): void {
    this.held = undefined;
    this.lost = true;
    this.stopped = true;
    if (!this.cancel.signal.aborted) this.cancel.abort(new ChatClaimLostError());
  }

  private async pollLoop(): Promise<void> {
    while (!this.stopped || this.held) {
      if ((this.stopped || this.cancel.signal.aborted) && this.held) {
        // Same bounded drain as SteeringChannel: an unrouted batch is left for the next claim.
        if (this.held.phase === "ack") {
          this.held = undefined;
          break;
        }
        if (this.stopApplyAttempts >= STOP_APPLY_ATTEMPTS) {
          // A routed follow-up is still unapplied: completing the chat would block its replay, so
          // treat the claim as lost (no terminal report) and let the next claim replay it.
          this.loseClaim();
          break;
        }
        this.stopApplyAttempts++;
      }
      try {
        if (!this.held && !this.stopped && !this.cancel.signal.aborted) {
          const { inputs, receipts } = await this.client.getInputs(this.runId);
          if (!validBatch(inputs)) throw new Error("invalid input GET response");
          if (!receipts) {
            // Issue #1673: a consume-on-read reply (an older api pod): route now, no receipts.
            for (const input of inputs) {
              if (this.routedIds.has(input.id)) continue;
              this.routedIds.add(input.id);
              this.route(input.kind, input.body ?? undefined);
            }
          } else if (inputs.length) this.held = { ids: inputs.slice(0, MAX_INPUT_BATCH).map((input) => input.id), phase: "ack" };
        }
        if (this.held?.phase === "ack") {
          const receipt = await this.client.ackInputs(this.runId, this.held.ids, this.claimGeneration);
          if (!validReceipt(receipt, this.held.ids)) throw new Error("invalid input ACK response");
          if (!receipt.active) {
            // A chat has no credential switch; any inactive claim is another worker's.
            this.loseClaim();
          } else {
            for (const input of [...receipt.inputs].sort((a, b) => a.id - b.id)) {
              if (this.routedIds.has(input.id)) continue;
              this.routedIds.add(input.id);
              this.route(input.kind, input.body ?? undefined);
            }
            // The waiter is serviced below; the applied receipt goes out on the next tick.
            this.held.phase = "applied";
            this.applyFailures = 0;
          }
        } else if (this.held?.phase === "applied") {
          const receipt = await this.client.applyInputs(this.runId, this.held.ids, this.claimGeneration);
          if (!validReceipt(receipt, this.held.ids)) throw new Error("invalid input applied response");
          // Settled. An inactive claim (already applied, then released) is another worker's now:
          // this chat flight must not report completion.
          if (!receipt.active) this.loseClaim();
          this.held = undefined;
          this.applyFailures = 0;
        }
      } catch (err) {
        const reason = receiptFenceReason(err);
        if (this.held && (endsFlight(reason) || reason === "switch_pending")) this.loseClaim();
        else if (this.held && (reason !== undefined || definitiveReceiptError(err)))
          this.held = undefined;
        else if (this.held?.phase === "ack" && ++this.applyFailures >= ACTIVE_APPLY_ATTEMPTS) {
          // Nothing routed yet: drop the batch so newer GETs flow; the next GET reads it again.
          this.held = undefined;
          this.applyFailures = 0;
        } else if (this.held?.phase === "applied" && ++this.applyFailures >= ACTIVE_APPLY_ATTEMPTS) {
          // Nothing waits on a chat's applied receipt, so giving it up only drops the batch: the
          // next GET replays the rows, which are ACKed and applied again but not re-routed.
          this.held = undefined;
          this.applyFailures = 0;
        }
        this.log.warn("chat steering: input poll failed", {
          run_id: this.runId,
          error: errMessage(err),
        });
      }
      // Route THEN service: a follow_up in the ACK is available before applied
      // starts on the next tick. Held IDs prevent another GET while it is uncertain.
      this.serviceWaiter();
      // Cancellation stops fresh GETs but cannot abandon a held receipt.
      if ((this.stopped || this.cancel.signal.aborted) && !this.held) break;
      // Wake early on a cancel only when nothing is held; a held receipt keeps its poll pace.
      await this.sleepFn(this.pollMs, this.held ? undefined : this.cancel.signal);
    }
    this.settle({ kind: "ended" });
  }
}
