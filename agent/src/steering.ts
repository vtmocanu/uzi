// The steering channel (PRD #4 §Workflow: plan gate, follow-ups, cancel).
//
// One run has exactly one poller of `GET /inputs`, because that endpoint is
// consume-on-read (each GET marks its inputs consumed, FIFO — see the M1
// ConsumeInputs contract). A second concurrent poller would eat inputs the first
// needs, so this single loop consumes every input and ROUTES it by kind:
//
//   approve_plan / reject_plan  → resolves the plan-gate verdict the executor awaits
//   cancel                      → aborts the run's AbortController (the SDK executor's
//                                 watchdog trips on ctx.signal → the turn ends with
//                                 "run cancelled") AND resolves any pending verdict
//   follow_up                   → queued FIFO, injected into the NEXT loop turn via
//                                 SDK session resume (bottega's model — no mid-turn
//                                 injection)
//
// The verdict is buffered if it arrives before the executor reaches the gate, so
// there is no lost-wakeup race between "post awaiting_approval" and "await verdict".

import type { WorkerClient } from "./client.js";
import type { Logger } from "./log.js";
import { parseAgentSelection, type AgentSelectionParse } from "./protocol.js";
import { errMessage, sleep } from "./util.js";

/** The outcome of the plan-approval gate. On approve, `selection` is the parsed
 *  `approve_plan` body (PRD #37): the executor resolves it against the run's
 *  detected roster (absent → run default; malformed → own, never repo). */
export type PlanVerdict =
  | { kind: "approve"; selection: AgentSelectionParse }
  | { kind: "reject"; reason: string }
  | { kind: "cancel" };

export interface SteeringOptions {
  /** Injectable sleep so tests can drive the poll loop deterministically. */
  sleep?: (ms: number, signal?: AbortSignal) => Promise<void>;
}

export class SteeringChannel {
  private stopped = false;
  private loop: Promise<void> | undefined;
  private readonly followUps: string[] = [];
  /** A verdict that arrived before the executor asked for one (no lost wakeup). */
  private bufferedVerdict: PlanVerdict | undefined;
  private verdictResolve: ((v: PlanVerdict) => void) | undefined;
  private readonly sleepFn: (ms: number, signal?: AbortSignal) => Promise<void>;

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

  /**
   * Resolve once the next plan verdict (approve/reject/cancel) is known. If one
   * already arrived it resolves immediately (buffered). Only one gate is awaited
   * at a time (there is exactly one plan per run).
   */
  awaitVerdict(): Promise<PlanVerdict> {
    if (this.bufferedVerdict) {
      const v = this.bufferedVerdict;
      this.bufferedVerdict = undefined;
      return Promise.resolve(v);
    }
    return new Promise<PlanVerdict>((resolve) => {
      this.verdictResolve = resolve;
    });
  }

  /** Dequeue the oldest un-consumed follow-up, or undefined if none. */
  pullFollowUp(): string | undefined {
    return this.followUps.shift();
  }

  private deliverVerdict(v: PlanVerdict): void {
    if (this.verdictResolve) {
      const r = this.verdictResolve;
      this.verdictResolve = undefined;
      r(v);
    } else {
      // Latest verdict wins if several land before the gate reads one.
      this.bufferedVerdict = v;
    }
  }

  private route(kind: string, body: string | null | undefined): void {
    switch (kind) {
      case "approve_plan":
        // The body carries the JSON-encoded agent selection (PRD #37). Parse it
        // here; the executor resolves it against the detected roster. A malformed
        // body parses to `invalid`, which the executor sends to `own`, never repo.
        this.deliverVerdict({ kind: "approve", selection: parseAgentSelection(body) });
        break;
      case "reject_plan":
        this.deliverVerdict({ kind: "reject", reason: body?.trim() || "plan rejected" });
        break;
      case "cancel":
        if (!this.cancel.signal.aborted) this.cancel.abort();
        this.deliverVerdict({ kind: "cancel" });
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
