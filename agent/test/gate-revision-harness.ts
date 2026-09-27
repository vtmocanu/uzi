// PRD #1795 M3: shared helpers for the runner-level gate-revision suites. The FakeApi models a
// revision-allocating api (gateRevisions) that stamps verdict rows like the api's M2 inserts
// (stampGateBindings); the client negotiated gate_revision_v1 (and the claim-generation fence the
// id-bearing report needs) unless a test says otherwise.
import assert from "node:assert/strict";
import { PlanRejectedError, type Executor, type RunContext } from "../src/executor.js";
import type { PlanVerdict } from "../src/steering.js";
import type { ClaimResponse, Milestone, StateRequest, UserInput } from "../src/protocol.js";
import { api, client, gitlabClaim, simulateCommittedWork } from "./runner-harness.js";

const tick = (ms = 5): Promise<void> => new Promise((r) => setTimeout(r, ms));

/** Poll `pred` until true or `ms` elapses; returns whether it became true (never throws). */
export async function until(pred: () => boolean, ms = 8_000): Promise<boolean> {
  const end = Date.now() + ms;
  while (!pred()) {
    if (Date.now() > end) return false;
    await tick();
  }
  return true;
}

/** Turn on the new api and the negotiated feature set (or only the fence, `negotiated: false`). */
export function newApi(opts: { negotiated?: boolean } = {}): void {
  simulateCommittedWork();
  api.gateRevisions = true;
  api.stampGateBindings = true;
  api.strictReceiptGenerations = true;
  api.stampInputCreatedAt = true;
  client.protocolFeatures = opts.negotiated === false ? ["claim_generation_fence"] : ["claim_generation_fence", "gate_revision_v1"];
}

let nextIid = 17950;
let nextId = 9_000;

/** A fresh issue-run claim at generation `generation`, receipts keyed to it. */
export function freshClaim(generation = 1, overrides: Partial<ClaimResponse> = {}): ClaimResponse {
  const claim = gitlabClaim(nextIid++, { claim_generation: generation, ...overrides });
  api.setInputClaimGeneration(claim.run_id, generation);
  return claim;
}

export function row(kind: UserInput["kind"], body: string | null = null, extra: Partial<UserInput> = {}): UserInput {
  return { id: nextId++, kind, body, ...extra };
}

/** Send rows now, stamped by the fake api from the run's current state. Returns the stored rows. */
export function send(runId: string, ...rows: UserInput[]): UserInput[] {
  api.appendInputs(runId, rows);
  const ids = new Set(rows.map((r) => r.id));
  return api.inputRows(runId).filter((r) => ids.has(r.id));
}

export function gates(runId: string): StateRequest[] {
  return api.states.filter((s) => s.runId === runId && s.body.status === "awaiting_approval").map((s) => s.body);
}

export function statuses(runId: string): string[] {
  return api.states.filter((s) => s.runId === runId).map((s) => s.body.status);
}

export function feed(runId: string): string[] {
  return api
    .messages(runId)
    .filter((m) => m.kind === "status")
    .map((m) => String((m.payload as { text?: unknown }).text ?? ""));
}

/** Wait until the worker ACKed input `id` (it is routed on that same poll tick). */
export async function routed(runId: string, id: number): Promise<void> {
  assert.ok(await until(() => api.isAcked(runId, id)), `input ${id} was ACKed (routed)`);
  await tick(30);
}

export const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

/**
 * A plan-gate executor driven through the REAL runner gate (ctx.gatePlan): plan `plans[0]`, and
 * on each revise the next plan (settling the revise, as the SDK executor does). With
 * `resumesAtGate` it behaves like the SDK executor on a resumed, unapproved claim: it takes the
 * resumed gate event first, then re-presents the claim's persisted plan (or revises it).
 */
export class GateExecutor implements Executor {
  readonly verdicts: PlanVerdict[] = [];
  readonly resumesAtGate: boolean | undefined;
  /** Resolves when the executor is between gates, i.e. running the revision turn for round n. */
  private readonly revisionHolds = new Map<number, Promise<void>>();

  constructor(
    private readonly plans: string[],
    opts: { resumesAtGate?: boolean; milestones?: Milestone[] } = {},
  ) {
    this.resumesAtGate = opts.resumesAtGate;
    this.milestones = opts.milestones;
  }

  private readonly milestones: Milestone[] | undefined;

  /** Hold the revision turn that produces plan round `n` (1-based) until `release` settles. */
  holdRevision(n: number, release: Promise<void>): void {
    this.revisionHolds.set(n, release);
  }

  async run(ctx: RunContext): Promise<{ branch: string }> {
    let round = 0;
    let plan = this.plans[0]!;
    let settles: number | undefined;
    if (ctx.planApproved !== true && ctx.approvedPlan?.trim() && ctx.takeResumedGateEvent) {
      const pending = await ctx.takeResumedGateEvent(ctx.signal);
      if (pending) this.verdicts.push(pending);
      if (pending?.kind === "cancel") throw new Error("run cancelled");
      if (pending?.kind === "reject") throw new PlanRejectedError(pending.reason);
      if (pending?.kind === "revise") {
        round++;
        plan = this.plans[round] ?? `# PLAN round ${round}`;
        settles = pending.inputId;
      } else if (this.resumesAtGate && ctx.resumePhase === "awaiting_approval") {
        plan = ctx.approvedPlan!;
      }
    }
    const milestones = this.milestones ?? (ctx.frozenMilestones ?? undefined);
    for (;;) {
      const v = await ctx.gatePlan!(plan, milestones, undefined, settles);
      this.verdicts.push(v);
      if (v.kind === "revise") {
        round++;
        await this.revisionHolds.get(round);
        plan = this.plans[round] ?? `# PLAN round ${round}`;
        settles = v.inputId;
        continue;
      }
      if (v.kind === "approve") return { branch: ctx.branch };
      if (v.kind === "reject") throw new PlanRejectedError(v.reason);
      throw new Error("run cancelled");
    }
  }
}

export interface Held {
  release: () => void;
}

/** Persist the next awaiting_approval report carrying `plan` and hold its ACK until released. */
export function holdGateAck(runId: string, plan: string): Held {
  let release!: () => void;
  const held = new Promise<void>((r) => (release = r));
  api.afterPersistState(runId, (b) => b.status === "awaiting_approval" && b.plan_md === plan, held);
  return { release };
}
