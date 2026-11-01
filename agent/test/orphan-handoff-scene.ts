import { randomUUID } from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import type { TestContext } from "node:test";
import { ActiveRunRegistry } from "../src/active-run-registry.js";
import type { AuthorityRuntime } from "../src/authority-runtime.js";
import type { SlotSnapshot } from "../src/completion-authority.js";
import type { HandoffRetirement, HandoffRetirementInput } from "../src/recovery.js";
import type { RunRunner } from "../src/runner.js";
import { recordingLogger } from "./helpers.js";
import { api, git, gitlabClaim } from "./runner-harness.js";
import {
  BRANCH, GENERATION, fenceOf, journalRaw, recordNames, rig, slotVia, snapshotFiles,
  type PrivateRunner, type Rig,
} from "./completion-retirement-fixture.js";

// issue #3645 M3 - the runner-level scene of the terminal-owner handoff tests: a REAL RunRunner claims a successor run
// on the branch of a just-completed owner whose completed source is still published (fence-carrying journal). The
// owner-classification fake answers `completed`. Real authority store, real recovery coordinator, real Git; the
// production wiring (authorityRequired) throughout. Lifted from the preserved #3646 attempt's successor-claim scene,
// without its owner-tail wait, boolean fallback or seed bypass.

export const STOPPED = "stop after successor admission";
export const OWNER_FILE = "OWNER-ONLY.txt";

export interface Row {
  name: string;
  /** The completed owner's kind and clone slug (its REAL canonical path). */
  owner: { kind: "issue" | "mr_rework"; slug: string };
  /** The successor's kind; it derives agent-issue-3404 for mr_rework, issue-3404 for issue. */
  successor: "issue" | "mr_rework";
  samePath: boolean;
  /** Seed the owner at its CANONICAL path regardless of `samePath` (the cross-kind Case A with a canonical owner). Default: `samePath`. */
  ownerCanonical?: boolean;
  codex: boolean;
  unproven?: boolean;
  restart: boolean;
  /** Attempt paths enabled on the claimant: it selects its own distinct attempt path. */
  attemptPaths?: boolean;
}

export const ROWS: Row[] = [];
for (const restart of [false, true]) {
  const when = restart ? "after restart" : "same incarnation";
  ROWS.push({ name: `Case A cross-path (issue owner, mr_rework successor), ${when}`, owner: { kind: "issue", slug: "issue-3404" },
    successor: "mr_rework", samePath: false, codex: false, restart });
  ROWS.push({ name: `Case B same-path (mr_rework owner, mr_rework successor), ${when}`, owner: { kind: "mr_rework", slug: "agent-issue-3404" },
    successor: "mr_rework", samePath: true, codex: false, restart });
}

/** Case A with the owner at its canonical path: a canonical `issue-3404` owner, an `mr_rework` claimant at `agent-issue-3404`. */
for (const restart of [false, true]) {
  ROWS.push({ name: `Case A cross-path with a canonical owner (issue owner at issue-3404, mr_rework successor), ${restart ? "after restart" : "same incarnation"}`,
    owner: { kind: "issue", slug: "issue-3404" }, successor: "mr_rework", samePath: false, ownerCanonical: true, codex: false, restart });
}

/** Attempt shape: the claimant has attempt paths enabled and selects its own distinct attempt path. */
export const ATTEMPT_ROWS: Row[] = [false, true].map(restart => ({
  name: `attempt shape (issue owner at an attempt path, mr_rework successor with attempt paths), ${restart ? "after restart" : "same incarnation"}`,
  owner: { kind: "issue" as const, slug: "issue-3404" }, successor: "mr_rework" as const, samePath: false, codex: false, restart, attemptPaths: true,
}));

export interface Scene {
  r: Rig;
  runner: RunRunner;
  recovery: PrivateRunner["recovery"];
  rt: AuthorityRuntime;
  /** The owner's slot as published, before the successor claims. */
  ownerSlot: SlotSnapshot | null;
  /** What the successor saw when its executor was entered. */
  started?: { worktree: string; slot: SlotSnapshot | null };
  /** Errors thrown by the successor's runnerCloneForBranch seed attempts. */
  foreign: Error[];
  claim: ReturnType<typeof gitlabClaim>;
  /** Every log record the claimant runner emitted. */
  lines: Array<Record<string, unknown>>;
  /** The claimant runner's active-run registry (the heartbeat snapshot source). */
  registry: ActiveRunRegistry;
  /** retireCompletedSourceForHandoff calls, with the durable state observed the moment each returned. */
  handoffs: Array<{ input: HandoffRetirementInput; outcome: HandoffRetirement | undefined; error?: unknown;
    slot: SlotSnapshot | null; fence: unknown; cloneExists: boolean }>;
  /** Awaited with the claimant's handoff input right before the real coordinator entry; may return a replacement input. */
  beforeHandoff?: (input: HandoffRetirementInput) => Promise<HandoffRetirementInput | void>;
  /** Awaited after the real entry returned or threw. */
  afterHandoff?: () => Promise<void>;
  /** The claimant runner's settleUnadoptedGuardedClaim (calls through), to prove a retained stop never settles the claim hold. */
  settle: { mock: { callCount(): number } };
  /** Every flight the claimant runner built, in order. */
  flights: FlightHandle[];
}

/** The slice of a claimant flight the tests drive: its steering channel and cancel controller. */
export interface FlightHandle {
  runId: string;
  worktreePath?: string;
  executionTail?: Promise<unknown>;
  cancel: AbortController;
  steering: { markCancelled(): void; claimFence(): string | undefined; endFencedFlight(reason: string): void };
}

export async function scene(t: TestContext, row: Row, options: { receipt?: boolean; ownerStatus?: string; cohort?: number } = {}): Promise<Scene> {
  const r = await rig(t, { codex: row.codex, unproven: row.unproven, kind: row.owner.kind, slug: row.owner.slug, canonical: row.ownerCanonical ?? row.samePath, cohort: options.cohort });
  // The production wiring: the git cache carries the authority hooks and requires them.
  const restore = { authority: (git as unknown as { authority?: unknown }).authority, required: (git as unknown as { authorityRequired?: boolean }).authorityRequired };
  t.after(() => { Object.assign(git, { authority: restore.authority, authorityRequired: restore.required }); });
  fs.writeFileSync(path.join(r.clone, OWNER_FILE), "owner-only bytes\n");
  await r.end();
  if (options.receipt !== false) await r.receipt();
  let rt = r.rt;
  let privateRunner = r.p;
  if (row.restart) {
    const second = r.restart();
    rt = second.rt;
    privateRunner = second.p;
  } else {
    Object.assign(git, { authority: rt.hooks, authorityRequired: true });
  }
  const runner = privateRunner as unknown as RunRunner;
  const ownerSlot = await slotVia(rt, r.clone);
  const claim = gitlabClaim(3404, {
    run_id: randomUUID(), kind: row.successor, claim_generation: GENERATION + 1, inventory_guarded: true,
    ...(row.successor === "mr_rework" ? { branch: BRANCH } : {}),
  });
  api.setOrphanClassification(r.runId, {
    status: options.ownerStatus ?? "completed", repo_id: "r1", kind: row.owner.kind, issue_iid: 3404,
    branch: null, pipeline_ref: row.owner.kind === "mr_rework" ? BRANCH : null, pipeline_id: null,
  });
  const recorded = recordingLogger();
  Object.assign(runner, { log: recorded.logger });
  const registry = new ActiveRunRegistry();
  Object.assign(runner, { snapshotRegistry: registry });
  if (row.attemptPaths) Object.assign(runner, { attemptPaths: true });
  const flights: FlightHandle[] = [];
  const buildFlight = (privateRunner as unknown as { buildFlight(...a: unknown[]): FlightHandle }).buildFlight.bind(privateRunner);
  (privateRunner as unknown as { buildFlight: unknown }).buildFlight = (...args: unknown[]) => {
    const flight = buildFlight(...args);
    flights.push(flight);
    return flight;
  };
  const s: Scene = { r, runner, recovery: privateRunner.recovery, rt, ownerSlot, foreign: [], claim, lines: recorded.lines as Array<Record<string, unknown>>, registry, flights, handoffs: [], settle: t.mock.method(runner as unknown as Record<string, (...a: unknown[]) => unknown>, "settleUnadoptedGuardedClaim") };
  const recovery = privateRunner.recovery;
  const realHandoff = recovery.retireCompletedSourceForHandoff.bind(recovery);
  recovery.retireCompletedSourceForHandoff = async input => {
    const seen = async (outcome: HandoffRetirement | undefined, error?: unknown) => s.handoffs.push({
      input, outcome, error, slot: await slotVia(rt, r.clone), fence: fenceOf(r), cloneExists: fs.existsSync(path.join(r.clone, ".git")),
    });
    const effective = (await s.beforeHandoff?.(input)) ?? input;
    let outcome: HandoffRetirement | undefined;
    let thrown: { error: unknown } | undefined;
    try { outcome = await realHandoff(effective); } catch (error) { thrown = { error }; }
    // The after-hook (releasing a held A) runs before the durable-state observation, which itself takes A.
    await s.afterHandoff?.();
    await seen(outcome, thrown?.error);
    if (thrown) throw thrown.error;
    return outcome!;
  };
  // Observe the refusals the successor's canonical seed raises (the real method still runs).
  const realSeed = git.runnerCloneForBranch.bind(git);
  git.runnerCloneForBranch = (async (...args: Parameters<typeof realSeed>) => {
    try { return await realSeed(...args); } catch (err) { if (err instanceof Error) s.foreign.push(err); throw err; }
  }) as typeof git.runnerCloneForBranch;
  t.after(() => { delete (git as unknown as Record<string, unknown>).runnerCloneForBranch; });
  (r.executor as unknown as { run: unknown }).run = async (ctx: { worktreePath: string }) => {
    s.started = { worktree: ctx.worktreePath, slot: await slotVia(rt, ctx.worktreePath) };
    throw new Error(STOPPED);
  };
  return s;
}

/** The failed report (its reason) the claimant sent, if any. */
export const failure = (s: Scene): unknown =>
  api.states.filter(state => state.runId === s.claim.run_id && state.body.status === "failed").at(-1)?.body.failure_reason;

/** Every status the claimant reported. */
export const reported = (s: Scene): string[] =>
  api.states.filter(state => state.runId === s.claim.run_id).map(state => String(state.body.status));

/** The orphan diagnostics the claimant runner emitted. */
export const diagnostics = (s: Scene): Array<Record<string, unknown>> =>
  s.lines.filter(l => typeof l["event"] === "string" && String(l["event"]).startsWith("orphan_"));

/** Everything the fail-closed and held controls require to stay byte-identical. */
export function durable(s: Scene) {
  return {
    journal: journalRaw(s.r),
    records: snapshotFiles(s.r),
    names: recordNames(s.r),
    owner: fs.existsSync(path.join(s.r.clone, OWNER_FILE)) ? fs.readFileSync(path.join(s.r.clone, OWNER_FILE), "utf8") : null,
    head: fs.existsSync(path.join(s.r.clone, ".git")) ? fs.readFileSync(path.join(s.r.clone, ".git", "HEAD"), "utf8") : null,
    skills: fs.existsSync(s.r.skills),
  };
}

export async function assertUnchanged(s: Scene, before: ReturnType<typeof durable>, assert: typeof import("node:assert/strict")): Promise<void> {
  assert.equal(s.started, undefined, "the model never starts");
  assert.deepEqual(durable(s), before, "journal, records and the owner clone are byte-identical");
  assert.ok(fenceOf(s.r), "the owner's fence is untouched");
  assert.equal((await slotVia(s.rt, s.r.clone))?.state, s.ownerSlot?.state, "the owner's slot is untouched");
}

/** Hold the generation's capture cycle lock C from another actor (a pin whose cycle blocks); `release()` frees it. */
export async function holdCycle(t: TestContext, s: Scene, kind: "issue" | "mr_rework" = "issue"): Promise<{ release(): Promise<void> }> {
  let open!: () => void;
  const gate = new Promise<void>(resolve => { open = resolve; });
  let entered!: () => void;
  const inside = new Promise<void>(resolve => { entered = resolve; });
  t.mock.method(s.recovery as unknown as { pinCycle(): Promise<undefined> }, "pinCycle", async () => { entered(); await gate; return undefined; });
  const head = (await s.recovery.inspect(s.r.runId))[0]!.sourceSha;
  const holder = s.recovery.pin({ runId: s.r.runId, generation: GENERATION, kind, branch: BRANCH, sourceSha: head, inventoryGuarded: true });
  await inside;
  return { release: async () => { open(); await holder; } };
}
