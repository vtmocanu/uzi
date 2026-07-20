// The steering channel (PRD #4 §Workflow: plan gate, follow-ups, cancel).
//
// One run has exactly one poller of `GET /inputs`, because that endpoint is
// consume-on-read (each GET marks its inputs consumed, FIFO — see the M1
// ConsumeInputs contract). A second concurrent poller would eat inputs the first
// needs, so this single loop consumes every input and ROUTES it by kind:
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

import type { WorkerClient } from "./client.js";
import type { Logger } from "./log.js";
import { parseAgentSelection, type AgentSelectionParse } from "./protocol.js";
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

export interface SteeringOptions {
  /** Injectable sleep so tests can drive the poll loop deterministically. */
  sleep?: (ms: number, signal?: AbortSignal) => Promise<void>;
  /** PRD #41: emit a short feed notice (as a worker `status` message) when a verdict
   *  or revision is DISCARDED for being stamped against a stale plan version. Injected
   *  by the runner (wired to the batcher) so the channel never reaches into runner
   *  internals; optional so tests can omit it. */
  notify?: (text: string) => void;
}

/** Feed notices for events discarded because they were written against a plan version
 *  that has since been revised (PRD #41 Decision 3). */
const STALE_APPROVE_NOTICE = "Approval ignored — the plan changed; re-send if you still want it.";
const STALE_REJECT_NOTICE = "Rejection ignored — the plan changed; re-send if you still want it.";
const STALE_REVISE_NOTICE = "Feedback ignored — it was written against an older plan version; re-send it.";

export class SteeringChannel {
  private stopped = false;
  private loop: Promise<void> | undefined;
  private readonly followUps: string[] = [];
  /** The current gate epoch (PRD #41): bumped at each awaiting_approval (re-)report.
   *  Every buffered verdict / queued revise is stamped with the epoch it arrived at, so
   *  a verdict written against a superseded plan version is detectable. Starts at 0 so a
   *  gate that never bumps (backward-compat awaitVerdict) still matches epoch-0 inputs. */
  private gateEpoch = 0;
  /** An approve/reject that arrived before the executor asked for one (no lost wakeup),
   *  stamped with the epoch it landed under. Latest-wins if several land before a read. */
  private bufferedVerdict: { verdict: PlanVerdict; epoch: number } | undefined;
  /** Cancel is sticky and epoch-exempt: once seen it always wins, at any epoch. */
  private cancelled = false;
  /** FIFO queue of revision feedback (PRD #41), each stamped with its arrival epoch. */
  private readonly reviseQueue: { feedback: string; epoch: number }[] = [];
  /** The gate waiter parked on awaitGateEvent, with the epoch it is waiting for. */
  private gateWaiter: { epoch: number; resolve: (v: PlanVerdict) => void } | undefined;
  private readonly sleepFn: (ms: number, signal?: AbortSignal) => Promise<void>;
  private readonly notify: ((text: string) => void) | undefined;

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
    return new Promise<PlanVerdict>((resolve) => {
      this.gateWaiter = { epoch, resolve };
    });
  }

  /** Backward-compat entry: await a verdict at the CURRENT epoch (PRD #41). A caller
   *  that never bumps (epoch 0) matches epoch-0 inputs, preserving pre-revision
   *  behaviour for callers that only ever gate once. */
  awaitVerdict(): Promise<PlanVerdict> {
    return this.awaitGateEvent(this.gateEpoch);
  }

  /** Dequeue the oldest un-consumed follow-up, or undefined if none. */
  pullFollowUp(): string | undefined {
    return this.followUps.shift();
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
      this.notify?.(verdict.kind === "reject" ? STALE_REJECT_NOTICE : STALE_APPROVE_NOTICE);
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

  private route(kind: string, body: string | null | undefined): void {
    switch (kind) {
      case "approve_plan":
        // The body carries the JSON-encoded agent selection (PRD #37). Parse it
        // here; the executor resolves it against the detected roster. A malformed
        // body parses to `invalid`, which the executor sends to `own`, never repo.
        this.bufferedVerdict = { verdict: { kind: "approve", selection: parseAgentSelection(body) }, epoch: this.gateEpoch };
        break;
      case "reject_plan":
        this.bufferedVerdict = { verdict: { kind: "reject", reason: body?.trim() || "plan rejected" }, epoch: this.gateEpoch };
        break;
      case "revise_plan":
        // PRD #41: enqueue the feedback for a revision round. Empty feedback is ignored
        // (like follow_up). The gate is serviced once per poll batch (pollLoop), AFTER all
        // inputs route, so precedence (a current-epoch revise beats a buffered approve) is
        // applied across the WHOLE batch — a [approve, revise] batch yields the revision
        // round, not a silent drop of the trailing revise.
        if (body && body.trim()) this.reviseQueue.push({ feedback: body.trim(), epoch: this.gateEpoch });
        break;
      case "cancel":
        if (!this.cancel.signal.aborted) this.cancel.abort();
        this.cancelled = true;
        break;
      case "follow_up":
        if (body && body.trim()) this.followUps.push(body.trim());
        break;
      default:
        this.log.warn("steering: ignoring unknown input kind", { kind });
    }
  }

  private async pollLoop(): Promise<void> {
    while (!this.stopped) {
      try {
        const inputs = await this.client.getInputs(this.runId);
        for (const inp of inputs) this.route(inp.kind, inp.body ?? undefined);
        // Route the WHOLE batch, THEN service once: whatever landed may now satisfy a
        // parked gate. Servicing per-input would let an approve at the head of a
        // [approve, revise] batch resolve the gate before the revise routes — the revise
        // would then sit un-consumed (a silent drop that still burned a server cap slot).
        // One service call per batch evaluates full precedence (revise beats approve) once.
        this.serviceGate();
      } catch (err) {
        this.log.warn("steering: input poll failed", { run_id: this.runId, error: errMessage(err) });
      }
      if (this.stopped) break;
      await this.sleepFn(this.pollMs);
    }
  }
}

/** What the chat park loop should do next: answer a message, idle-complete, or end
 *  (an explicit End chat, or worker shutdown). */
export type ChatInput =
  | { kind: "message"; text: string }
  | { kind: "idle" }
  | { kind: "ended" };

/** The input source a ChatRunner parks on between turns (PRD #39 Decision 2). The
 *  real one is ChatSteering; tests inject a fake that yields scripted ChatInputs. */
export interface ChatInputSource {
  start(): void;
  stop(): Promise<void>;
  awaitFollowUp(idleMs: number): Promise<ChatInput>;
}

export interface ChatSteeringOptions {
  /** Injectable sleep so tests drive the poll loop deterministically. */
  sleep?: (ms: number, signal?: AbortSignal) => Promise<void>;
  /** Injectable clock so the idle window is provable without real time. */
  now?: () => number;
}

/**
 * The chat steering channel (PRD #39 Decision 2). Like SteeringChannel it is the
 * SOLE poller of the consume-on-read `GET /inputs`, but a chat only ever sees two
 * input kinds: `follow_up` (a user turn — including the seeded first message) and
 * `cancel` (End chat). It also OWNS the idle clock, and that ownership is the
 * load-bearing fix for the drop-on-idle race (team task #8):
 *
 *   The poll loop consumes inputs, buffers any follow_up, and THEN — in the SAME
 *   iteration, after routing — services the parked waiter, delivering a buffered
 *   message before it ever tests idle. There is no separate idle timer that could
 *   fire in the window between the server consuming a follow_up (consume-on-read
 *   removes it server-side) and the worker buffering it. So a message that races the
 *   idle tick is delivered, never lost — the only correct design under consume-on-read.
 *
 * A `cancel` aborts the shared controller (which is the executor's ctx.signal), so
 * End chat also aborts a turn in flight, not just a parked wait.
 */
export class ChatSteering implements ChatInputSource {
  private stopped = false;
  private loop: Promise<void> | undefined;
  private readonly followUps: string[] = [];
  private waiter: { resolve: (i: ChatInput) => void; idleMs: number; parkedAt: number } | undefined;
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
  ) {
    this.sleepFn = opts.sleep ?? sleep;
    this.now = opts.now ?? Date.now;
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
    if (this.followUps.length) return Promise.resolve<ChatInput>({ kind: "message", text: this.followUps.shift()! });
    if (this.cancel.signal.aborted || this.stopped) return Promise.resolve<ChatInput>({ kind: "ended" });
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
        this.log.warn("chat steering: ignoring unexpected input kind", { kind });
    }
  }

  /** After each poll+route, decide what a parked waiter gets: cancel/stop wins, then
   *  a buffered message, then idle — so a just-consumed follow_up is always delivered
   *  before idle can complete the chat. */
  private serviceWaiter(): void {
    if (!this.waiter) return;
    if (this.cancel.signal.aborted || this.stopped) return this.settle({ kind: "ended" });
    if (this.followUps.length) return this.settle({ kind: "message", text: this.followUps.shift()! });
    if (this.now() - this.waiter.parkedAt >= this.waiter.idleMs) this.settle({ kind: "idle" });
  }

  private async pollLoop(): Promise<void> {
    while (!this.stopped) {
      try {
        const inputs = await this.client.getInputs(this.runId);
        for (const inp of inputs) this.route(inp.kind, inp.body ?? undefined);
      } catch (err) {
        this.log.warn("chat steering: input poll failed", { run_id: this.runId, error: errMessage(err) });
      }
      // Route THEN service: a follow_up consumed this cycle is buffered above and
      // delivered here, before idle is tested (task #8).
      this.serviceWaiter();
      // Once cancelled (End chat / shutdown), stop polling — serviceWaiter already
      // delivered `ended` to any waiter; continuing would busy-spin on the aborted
      // sleep and hammer /inputs until stop() lands.
      if (this.stopped || this.cancel.signal.aborted) break;
      await this.sleepFn(this.pollMs, this.cancel.signal);
    }
    this.settle({ kind: "ended" });
  }
}
