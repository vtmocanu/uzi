import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { randomBytes, randomUUID } from "node:crypto";
import fs from "node:fs";
import fsp from "node:fs/promises";
import type { FileHandle } from "node:fs/promises";
import path from "node:path";
import type { TestContext } from "node:test";
import { acquireWorkerLifetime, type WorkerLifetime } from "../src/authority-lock.js";
import { AuthorityRuntime } from "../src/authority-runtime.js";
import { openAuthorityStore, type AuthorityFsOps, type SlotSnapshot } from "../src/completion-authority.js";
import type { CompletionSourceV2 } from "../src/completion-source.js";
import { ExecutorLifetime, type ExecutorTerminalSeal } from "../src/codex/executor-lifetime.js";
import type { Executor } from "../src/executor.js";
import type { CompletedPublicationReceipt } from "../src/protocol.js";
import type { CompletionSource, RecoveryCoordinator, RecoveryRecord } from "../src/recovery.js";
import { mintAttemptId } from "../src/run-quiescence.js";
import { testLockOptions } from "./authority-test-support.js";
import { nullLogger } from "./helpers.js";
import { client, fakeGitlab, fx, git, gitlabClaim, runnerWith } from "./runner-harness.js";

// issue #3404 M2 U5 - the rig of the restart-retirement tests. The REAL RunRunner publishes a flight's
// completing source (close -> seal -> publish) over the real authority store, real recovery coordinator
// and real Git; `restart()` then builds a second worker incarnation (a fresh AuthorityRuntime and a fresh
// RunRunner over the same data directory), which owns no in-memory binding for anything the first one did.

export const TOKEN = "unit-join-" + randomBytes(12).toString("hex");
export const JOIN = "u5-retirement-join";
export const GENERATION = 29;
export const BRANCH = "agent/issue-3404";
export const ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0" };

export interface PrivateRunner {
  recovery: RecoveryCoordinator;
  liveAttempts: { isLivePath(p: string): boolean };
  buildFlight(...args: unknown[]): Record<string, unknown>;
  bindCompletionSource(flight: Record<string, unknown>): Promise<void>;
  endFlightSourceUse(flight: Record<string, unknown>, log: unknown): Promise<void>;
}

export interface Restarted {
  rt: AuthorityRuntime;
  p: PrivateRunner;
  incarnation: string;
  /** Awaited durability barriers this incarnation issued: authority store (slot + head shard) and the run directory. */
  barriers: { authority: number; runDirectory: number };
  /** One boot/live sweep over the run's receipt-bearing records. */
  sweep(): Promise<void>;
}

export interface Rig {
  lifetime: WorkerLifetime;
  rt: AuthorityRuntime;
  p: PrivateRunner;
  flight: Record<string, unknown>;
  bound: CompletionSource;
  bare: string;
  clone: string;
  skills: string;
  runId: string;
  captureIds: string[];
  executor: Executor;
  warnings: string[];
  end(): Promise<void>;
  receipt(): Promise<void>;
  restart(): Restarted;
}

export function sealingExecutor(unproven = false): Executor {
  const lifetime = new ExecutorLifetime();
  return {
    run: async ctx => ({ branch: ctx.branch }),
    executorIncarnation: lifetime.executorIncarnation,
    sealLifetime: (sourceUse: object) => lifetime.seal(sourceUse, async () => {}, () => !unproven),
    acceptsLifetimeSeal: (seal: unknown, sourceUse: object): seal is ExecutorTerminalSeal => lifetime.accepts(seal, sourceUse),
  } as Executor;
}

export const receiptOf = (runId: string, head: string): CompletedPublicationReceipt => ({
  hold_id: "11111111-1111-4111-8111-111111111111", run_id: runId,
  owner_id: "33333333-3333-4333-8333-333333333333", worker_id: "44444444-4444-4444-8444-444444444444", generation: GENERATION,
  final_head: head, repo_id: "55555555-5555-4555-8555-555555555555",
  connection_id: "66666666-6666-4666-8666-666666666666", project_id: 7, forge_type: "github",
  base_url: "https://example.invalid", branch: BRANCH, mr_iid: 3, observed_branch_head: head,
});

export async function rig(_t: TestContext, options: { codex: boolean; cohort?: number; unproven?: boolean; issue?: number;
  /** Explicit capture ids for the extra cohort members (default randomUUID()). */
  memberIds?: string[];
  /** The owner's run kind (default "issue") and clone slug (default "u5-source"); a slug the claimant would derive puts the owner at its real canonical path. */
  kind?: "issue" | "mr_rework"; slug?: string;
  /** Seed the owner at the CANONICAL clone path for its slug (no attempt id), as a legacy/unwired worker does. */
  canonical?: boolean }): Promise<Rig> {
  const dataDir = fs.realpathSync(fx.dataDir);
  const lifetime = await acquireWorkerLifetime(dataDir, testLockOptions());
  const rt = new AuthorityRuntime({ lifetime, workerToken: TOKEN, timeoutMs: 5000 });
  const executor: Executor = options.codex ? sealingExecutor(options.unproven === true) : { run: async ctx => ({ branch: ctx.branch }) };
  const claim = gitlabClaim(options.issue ?? 3404, {
    claim_generation: GENERATION, inventory_guarded: true,
    ...(options.kind ? { kind: options.kind, branch: BRANCH } : {}),
  });
  const runner = runnerWith(() => ({ executor }), fakeGitlab().gitlab, JOIN, undefined, { authority: rt });
  const p = runner as unknown as PrivateRunner;
  client.hasFeature = () => true;
  client.listRecoveryHolds = async () => ({ run_id: claim.run_id, holds: [{
    hold_id: "u5-open-hold", generation: GENERATION, inventory_guarded: true, has_available_capture: false,
  }] });
  const bare = await git.ensureClone(fx.originPath);
  fs.chmodSync(path.dirname(bare), 0o700);
  fs.chmodSync(bare, 0o700);
  fs.chmodSync(path.join(bare, "config"), 0o600);
  const attemptId = options.canonical ? undefined : mintAttemptId(GENERATION);
  const cloneInfo = await git.runnerCloneForBranch(bare, BRANCH, options.slug ?? "u5-source", {
    isLive: () => false, beforeFree: async () => { throw new Error("unexpected incumbent"); },
  }, claim.run_id, false, undefined, attemptId === undefined ? undefined : {
    attemptId, isLive: () => false, beforeSeed: async paths => assert.deepEqual(paths, []),
  });
  await git.markRecoveryCapture(bare, cloneInfo.path, BRANCH, claim.run_id, attemptId);
  const head = await git.worktreeHead(cloneInfo.path);
  assert.ok(head);
  const first = await p.recovery.pin({ runId: claim.run_id, generation: GENERATION, kind: options.kind ?? "issue", branch: BRANCH, sourceSha: head, inventoryGuarded: true });
  assert.ok(first);
  const captureIds = [first.captureId];
  for (let i = 1; i < (options.cohort ?? 1); i++) {
    const extra = { ...first, captureId: options.memberIds?.[i - 1] ?? randomUUID() };
    await (p.recovery as unknown as { writeRecordUnlocked(r: unknown): Promise<void> }).writeRecordUnlocked(extra);
    captureIds.push(extra.captureId);
  }
  captureIds.sort();
  const flight = p.buildFlight(claim, claim.run_id, executor, undefined, "", []);
  Object.assign(flight, { barePath: bare, worktreePath: cloneInfo.path, branch: BRANCH, runnerClone: cloneInfo });
  await p.bindCompletionSource(flight);
  const bound = flight.completionSourceBound as CompletionSource | undefined;
  assert.ok(bound, "the real binder bound the completing source");
  const sink = new Set<string>();
  flight.sourceUsePaths = sink;
  flight.sourceLease = await rt.recordAdmissions(sink, () => rt.bindSource(cloneInfo.path, executor.executorIncarnation));
  flight.sourceTailsJoined = true;
  const skills = path.join(path.dirname(cloneInfo.path), `.uzi-skills-${path.basename(cloneInfo.path)}`);
  fs.mkdirSync(path.join(skills, "skills"), { recursive: true });
  fs.writeFileSync(path.join(skills, "plugin.json"), "{}");
  // A bundle file per cohort member, like a capture that uploaded.
  for (const id of captureIds) fs.writeFileSync(path.join(git.recoveryRoot, claim.run_id, `${id}.bundle`), `bundle ${id}`);
  const warnings: string[] = [];
  const log = Object.assign(nullLogger(), { warn: (message: string, fields?: unknown) => { warnings.push(`${message} ${JSON.stringify(fields ?? {})}`); } });
  const self: Rig = {
    lifetime, rt, p, flight, bound, bare, clone: cloneInfo.path, skills, runId: claim.run_id, captureIds, executor, warnings,
    end: () => p.endFlightSourceUse(flight, log),
    receipt: async () => {
      assert.equal(await p.recovery.persistCompletionReceipt(receiptOf(claim.run_id, bound.expectedHead)), true);
    },
    restart: () => {
      // The production wiring of a worker: GitCache carries the authority hooks and requires them.
      const incarnation = randomUUID();
      const barriers = { authority: 0, runDirectory: 0 };
      const ops: AuthorityFsOps = {
        syncFile: async handle => { barriers.authority++; await handle.sync(); },
        syncDir: async handle => { barriers.authority++; await handle.sync(); },
        rename: (from, to) => fsp.rename(from, to),
      };
      const rt2 = new AuthorityRuntime({ lifetime, workerToken: TOKEN, workerIncarnation: incarnation, timeoutMs: 5000, ops });
      Object.assign(git, { authority: rt2.hooks, authorityRequired: true });
      const p2 = runnerWith(() => ({ executor }), fakeGitlab().gitlab, JOIN, undefined, { authority: rt2 }) as unknown as PrivateRunner;
      (p2.recovery as unknown as { publicationOps: { syncDir(h: FileHandle): Promise<void> } }).publicationOps.syncDir = async handle => { barriers.runDirectory++; await handle.sync(); };
      return {
        rt: rt2, p: p2, incarnation, barriers,
        sweep: async () => {
          const snapshot = (await p2.recovery.inspect(claim.run_id)).filter(x => x.completionReceipt);
          await p2.recovery.resumePending(undefined, snapshot);
        },
      };
    },
  };
  return self;
}

export const recordDir = (r: Rig): string => path.join(git.recoveryRoot, r.runId);
export const recordNames = (r: Rig): string[] => (fs.existsSync(recordDir(r)) ? fs.readdirSync(recordDir(r)).sort() : []);
export const readRecords = (r: Rig): Array<Record<string, unknown>> =>
  r.captureIds.filter(id => fs.existsSync(path.join(recordDir(r), `${id}.json`)))
    .map(id => JSON.parse(fs.readFileSync(path.join(recordDir(r), `${id}.json`), "utf8")) as Record<string, unknown>);
export const snapshotFiles = (r: Rig): Record<string, string> => Object.fromEntries(
  recordNames(r).map(name => [name, fs.readFileSync(path.join(recordDir(r), name), "utf8")]));
export function fenceOf(r: Rig): unknown {
  try {
    const raw = execFileSync("git", ["-C", r.bare, "config", "--get", `uzi-recovery.${BRANCH}.clone`], { encoding: "utf8", env: ENV }).trim();
    return (JSON.parse(raw) as { completionSourceFence?: unknown }).completionSourceFence;
  } catch { return undefined; }
}
export function journalRaw(r: Rig): string {
  try { return execFileSync("git", ["-C", r.bare, "config", "--get", `uzi-recovery.${BRANCH}.clone`], { encoding: "utf8", env: ENV }).trim(); }
  catch { return ""; }
}
export async function slotVia(rt: AuthorityRuntime, target: string): Promise<SlotSnapshot | null> {
  return rt.withTransaction(async tx => {
    const store = await openAuthorityStore(tx, { workerToken: TOKEN });
    return store.readSlot(tx, await store.slotKey(tx, target));
  });
}
export const slotAt = (r: Rig, target: string): Promise<SlotSnapshot | null> => slotVia(r.rt, target);
export const slotOf = (r: Rig): Promise<SlotSnapshot | null> => slotAt(r, r.clone);
export const isV2 = (record: Record<string, unknown>): boolean => (record.completionSource as { version?: number } | undefined)?.version === 2;
export const sourceOf = (record: Record<string, unknown>): CompletionSourceV2 => record.completionSource as CompletionSourceV2;

/** Rewrite every cohort record's source through the coordinator (a journal-valid record whose MAC is fresh). */
export async function rewriteSources(r: Rig, change: (source: Record<string, unknown>, index: number) => Record<string, unknown> | undefined): Promise<void> {
  const internals = r.p.recovery as unknown as { writeRecordUnlocked(rec: unknown): Promise<void> };
  const records = await r.p.recovery.inspect(r.runId);
  for (const [index, record] of records.sort((a, b) => (a.captureId < b.captureId ? -1 : 1)).entries()) {
    const next = change({ ...(record.completionSource as unknown as Record<string, unknown>) }, index);
    if (next !== undefined) await internals.writeRecordUnlocked({ ...record, completionSource: next } as unknown as RecoveryRecord);
  }
}

