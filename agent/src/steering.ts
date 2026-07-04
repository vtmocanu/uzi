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
import { errMessage, sleep } from "./util.js";

/** The outcome of the plan-approval gate. */
export type PlanVerdict =
  | { kind: "approve" }
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
        this.deliverVerdict({ kind: "approve" });
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
