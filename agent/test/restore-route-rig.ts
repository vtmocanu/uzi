// Issue #3644 Gate B (m4a/m4b): the shared rig of the independent-restore tests. Production RunRunner + real Git + the durable
// authority, driven through RunRunner.execute(). The predecessor P is a real attempt clone with its journal, ledger entry, pin
// record and an ACTIVE slot owned by another incarnation; the successor S is the runner's own AuthorityRuntime with a different
// incarnation. Each test file calls installHarness() itself and cleanupRig() from its afterEach.
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { createHash, randomBytes, randomUUID } from "node:crypto";
import fs from "node:fs";
import fsp from "node:fs/promises";
import { syncBuiltinESMExports } from "node:module";
import os from "node:os";
import path from "node:path";
import { mock } from "node:test";
import { acquireWorkerLifetime } from "../src/authority-lock.js";
import { AuthorityRuntime } from "../src/authority-runtime.js";
import { DiskGovernor, type CacheCapConfig, type GovernedRun } from "../src/cache-cap.js";
import type { SlotSnapshot } from "../src/completion-authority.js";
import type { Executor } from "../src/executor.js";
import type { ClaimResponse } from "../src/protocol.js";
import type { RecoveryCoordinator, RecoveryRecord } from "../src/recovery.js";
import type { RunCacheDropResult } from "../src/run-caches.js";
import { mintAttemptId } from "../src/run-quiescence.js";
import {
  boundaryFileName, establishProcessBoundary, resetProcessBoundaryForTests, sealBoundaryRecord, writeBoundaryRecord,
  type BoundaryRecord,
} from "../src/restore-boundary.js";
import { noProofReseed, recordingLogger } from "./helpers.js";
import { testLockOptions } from "./authority-test-support.js";
import { api, client, fakeGitlab, fx, git, gitlabClaim, homeDir, runnerWith } from "./runner-harness.js";
import { runDirKey } from "../src/util.js";


export const AUTH_TOKEN = "unit-join-" + randomBytes(12).toString("hex");
export const IID = 3644;
export const BRANCH = `agent/issue-${IID}`;
export const CID = "ab".repeat(32);
export const BOOT = "11111111-2222-3333-4444-555555555555";
export const FEATURES = ["active_run_snapshot", "claim_generation_fence", "recovery_completed_publication_v1"];
/** The api advertises the owner-hold cause the unsupported-predecessor park reports (RETAINED_ADOPTION_FAILED_FEATURE). */
export const PARK_FEATURE = "recovery_cause_retained_recovery_adoption_failed";
/** The pod uid of a hosted slot in the rig (a UUID means hosted). */
const POD_UID = "123e4567-e89b-12d3-a456-426614174000";
export const GIT_ENV: NodeJS.ProcessEnv = { GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", PATH: process.env.PATH };
export const host = (): string => CID.slice(0, 12);

export const gitIn = (dir: string, args: string[]): string => execFileSync("git", ["-C", dir, ...args], { env: GIT_ENV, encoding: "utf8", stdio: "pipe" }).trim();

/** Names, modes, sizes, mtimes and bytes of a tree: a directory's byte identity. */
export function treeDigest(root: string): string {
  const h = createHash("sha256");
  const walk = (dir: string): void => {
    for (const name of fs.readdirSync(dir).sort()) {
      const p = path.join(dir, name);
      const st = fs.lstatSync(p);
      h.update(`${path.relative(root, p)}\0${st.mode}\0${st.mtimeMs}\0${st.size}\0`);
      if (st.isDirectory()) walk(p); else if (st.isFile()) h.update(fs.readFileSync(p));
    }
  };
  walk(root);
  return h.digest("hex");
}

// ---- a fake procfs, as restore-boundary.test.ts builds it ----------------------------------------------------------------

export interface ProcOpts { ticks?: string; boot?: string; hidden?: boolean }
export async function makeProc(dir: string, o: ProcOpts = {}): Promise<void> {
  await fsp.mkdir(path.join(dir, "self/ns"), { recursive: true });
  await fsp.mkdir(path.join(dir, "1"), { recursive: true });
  await fsp.mkdir(path.join(dir, "sys/kernel/random"), { recursive: true });
  await fsp.writeFile(path.join(dir, "self/ns/pid"), "");
  await fsp.writeFile(path.join(dir, "1/stat"), `1 (init (x)) S 0 1 1 0 -1 4194560 1 0 0 0 0 0 0 0 20 0 1 0 ${o.ticks ?? "1000"} 1 1 18446744073709551615\n`);
  await fsp.writeFile(path.join(dir, "sys/kernel/random/boot_id"), `${o.boot ?? BOOT}\n`);
  await fsp.writeFile(path.join(dir, "self/timens_offsets"), "monotonic           0         0\nboottime            0         0\n");
  const options = o.hidden ? "rw,hidepid=2" : "rw";
  await fsp.writeFile(path.join(dir, "self/mountinfo"),
    `20 10 0:30 / / rw - overlay overlay rw\n31 20 8:1 /var/lib/docker/containers/${CID}/hostname /etc/hostname rw,relatime - ext4 /dev/sda1 rw\n` +
    `41 20 0:5 / ${dir} rw,nosuid,nodev,noexec,relatime - proc proc ${options}\n`);
}

// ---- the rig --------------------------------------------------------------------------------------------------------------

/** Defaults a test file may change once at load (the m4a file stops at an eligible verdict; the seed file restores). */
export const rigDefaults: { independentRestore: boolean } = { independentRestore: true };

export interface Rig {
  claim: ClaimResponse;
  runId: string;
  bare: string;
  dataDir: string;
  pAttempt: string;
  pPath: string;
  pInc: string;
  sInc: string;
  pRt: AuthorityRuntime;
  sRt: AuthorityRuntime;
  sProc: string;
  pProc: string;
  tmp: string;
  pinSha: string;
  journalKey: string;
  ledgerKey: string;
  runHome: string;
  provisionDir: string;
  /** Writes the pin record through the runner's own coordinator. */
  pin(over?: Partial<Parameters<RecoveryCoordinator["pin"]>[0]>): Promise<RecoveryRecord | undefined>;
  recovery(): RecoveryCoordinator;
  runner: ReturnType<typeof runnerWith>;
  logs: ReturnType<typeof recordingLogger>;
  counters: { ran: number; reclaims: number };
  governor: { run?: GovernedRun; measures: string[]; trims: string[] };
  /** The executor handed out for the claim's initial build, and for the successor build (the same object unless overridden). */
  executor: Executor;
  successorExecutor: Executor;
  /** Every `makeExecutor` call the runner made: the address it asked for and the HOME it was given. */
  factoryCalls: Array<{ generation: number | undefined; home: string | undefined }>;
  lifetime: Awaited<ReturnType<typeof acquireWorkerLifetime>>;
  /** The agent-home root: `<homeRoot>/<runId>` is P's HOME, `<homeRoot>/<runId>.g<N>` a restored successor's. */
  homeRoot: string;
}

let tmpRoots: string[] = [];

export interface RigOptions {
  /** Claim overrides. */
  claim?: Partial<ClaimResponse>;
  features?: string[];
  /** Default true: write the early pin of the default tip (bound as on_default). */
  pin?: boolean;
  /** Default "ok". */
  pBoundary?: "ok" | "missing" | "invalid" | "hosted" | "failed" | "slot_mismatch" | "same_execution";
  /** Default "ok": S's process boundary. "hosted": S is a hosted pod (its record and live identity name a pod uid). */
  sBoundary?: "ok" | "none" | "missing_file" | "hosted";
  /** Default "attempt". "canonical": P is an attempt-less source at the canonical clone path, journaled `{runId, clonePath}` with
   *  no ledger entry (what `markRecoveryCapture(..., undefined)` writes). */
  pShape?: "attempt" | "canonical";
  /** Default "clean". */
  sLive?: "clean" | "hidden";
  governor?: boolean;
  runnerExtra?: Parameters<typeof runnerWith>[4];
  independentRestore?: boolean;
  /** The forge client the runner publishes through (default: a fresh fake). */
  gitlab?: Parameters<typeof runnerWith>[1];
  /** The factory honours the per-attempt address (HOME `<runId>.g<N>`), as main.ts does. Default false: it ignores it. */
  addrAware?: boolean;
  /** Replaces the stub executor handed to the initial build. */
  executor?: Executor;
  /** Replaces the executor handed to the successor build (an addr-aware factory only). */
  successorExecutor?: Executor;
  /** Replaces the factory entirely (called with the rig, whose `executor`s are set). */
  factory?: (rig: Rig, runId: string, codex: unknown, addr: { generation?: number } | undefined) => { executor: Executor; homeDir?: string };
}

export async function buildRig(o: RigOptions = {}): Promise<Rig> {
  resetProcessBoundaryForTests();
  const tmp = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), "restore-route-")));
  tmpRoots.push(tmp);
  const dataDir = fs.realpathSync(fx.dataDir);
  const claim: ClaimResponse = { ...gitlabClaim(IID, { claim_generation: 2, inventory_guarded: true }), ...o.claim };
  const runId = claim.run_id;
  api.setOwnershipStatus(runId, "running", claim.claim_generation ?? 2);
  client.protocolFeatures = o.features ?? FEATURES;

  const bare = await git.ensureClone(fx.originPath);
  fs.chmodSync(path.dirname(bare), 0o700);
  fs.chmodSync(bare, 0o700);
  const pinSha = gitIn(bare, ["rev-parse", "refs/remotes/origin/main"]);

  // P: a real attempt clone of generation 1 with unsaved work, its journal and its `live` ledger entry.
  const pAttempt = mintAttemptId(1);
  const canonicalP = o.pShape === "canonical";
  const clone = canonicalP
    ? await git.createOrAttachRunnerClone(bare, IID, noProofReseed, runId, false)
    : await git.createOrAttachRunnerClone(bare, IID, noProofReseed, runId, false, undefined,
      { attemptId: pAttempt, isLive: () => false, beforeSeed: async () => {} });
  fs.writeFileSync(path.join(clone.path, "ONLY_COPY.txt"), "work that only the predecessor's clone holds\n");
  await git.markRecoveryCapture(bare, clone.path, BRANCH, runId, canonicalP ? undefined : clone.attemptId);

  // The authority: P's active slot under a dead incarnation, S as the restarted worker.
  const lifetime = await acquireWorkerLifetime(dataDir, testLockOptions());
  const pInc = randomUUID();
  const sInc = randomUUID();
  const pRt = new AuthorityRuntime({ lifetime, workerToken: AUTH_TOKEN, workerIncarnation: pInc, timeoutMs: 5000 });
  const sRt = new AuthorityRuntime({ lifetime, workerToken: AUTH_TOKEN, workerIncarnation: sInc, timeoutMs: 5000 });
  await pRt.withTransaction(() => pRt.hooks.admitSourceUse(clone.path, canonicalP ? "seed-canonical" : "seed-attempt"));

  // The boundary records: same compose slot, P's execution strictly older than S's.
  const pProc = path.join(tmp, "proc-p");
  const sProc = path.join(tmp, "proc-s");
  await makeProc(pProc, { ticks: o.pBoundary === "same_execution" ? "2000" : "1000" });
  await makeProc(sProc, { ticks: "2000" });
  fs.mkdirSync(path.join(dataDir, ".uzi-authority"), { recursive: true, mode: 0o700 });
  const common = { dataDir, workerToken: AUTH_TOKEN, hostname: host };
  const pMode = o.pBoundary ?? "ok";
  if (pMode === "ok" || pMode === "same_execution") {
    const w = await writeBoundaryRecord({ ...common, procRoot: pMode === "same_execution" ? sProc : pProc, incarnation: pInc });
    assert.ok(w.ok);
  } else if (pMode === "hosted") {
    const w = await writeBoundaryRecord({ ...common, procRoot: pProc, incarnation: pInc, podUid: POD_UID });
    assert.ok(w.ok);
  } else if (pMode === "failed" || pMode === "slot_mismatch") {
    const record: BoundaryRecord = { version: 1, incarnation: pInc,
      slot: { kind: "compose", hostname: pMode === "slot_mismatch" ? "ffffffffffff" : host(), containerId: pMode === "slot_mismatch" ? "ff".repeat(32) : CID },
      crossCheck: pMode === "failed" ? "failed" : "ok",
      execution: { nsDev: "1", nsIno: "10", initStartTicks: "1000", bootId: BOOT } };
    const dir = path.join(dataDir, ".uzi-authority", "boundaries");
    fs.mkdirSync(dir, { recursive: true, mode: 0o700 });
    fs.writeFileSync(path.join(dir, boundaryFileName(pInc)), sealBoundaryRecord(AUTH_TOKEN, record), { mode: 0o600 });
  } else if (pMode === "invalid") {
    const dir = path.join(dataDir, ".uzi-authority", "boundaries");
    fs.mkdirSync(dir, { recursive: true, mode: 0o700 });
    fs.writeFileSync(path.join(dir, boundaryFileName(pInc)), "{ not a record", { mode: 0o600 });
  }
  const sMode = o.sBoundary ?? "ok";
  if (sMode !== "none") {
    const w = await establishProcessBoundary({ ...common, procRoot: sProc, incarnation: sInc, ...(sMode === "hosted" ? { podUid: POD_UID } : {}) });
    assert.ok(w.ok);
    if (sMode === "missing_file") fs.unlinkSync(path.join(dataDir, ".uzi-authority", "boundaries", boundaryFileName(sInc)));
  }
  if ((o.sLive ?? "clean") === "hidden") await makeProc(sProc, { ticks: "2000", hidden: true });

  // A HOME with rebuildable caches and a provision directory: the bytes the route must never touch.
  const runHome = path.join(homeDir, runId);
  fs.mkdirSync(path.join(runHome, ".npm", "_cacache"), { recursive: true });
  fs.writeFileSync(path.join(runHome, ".npm", "_cacache", "blob"), "cached bytes");
  fs.writeFileSync(path.join(runHome, "session-proof"), "original session");
  const provisionDir = path.join(dataDir, "provision", runId);
  fs.mkdirSync(provisionDir, { recursive: true });
  fs.writeFileSync(path.join(provisionDir, "marker"), "provision bytes");

  const logs = recordingLogger();
  const counters = { ran: 0, reclaims: 0 };
  const governor: Rig["governor"] = { measures: [], trims: [] };
  const executor: Executor = o.executor ?? {
    run: async () => { counters.ran++; throw new Error("a refused or deferred restore runs no model"); },
    reclaimCachesInPlace: async (): Promise<RunCacheDropResult> => { counters.reclaims++; return { dropped: [], absent: [], failed: [], skipped: [] }; },
  };
  const successorExecutor: Executor = o.successorExecutor ?? executor;
  const factoryCalls: Rig["factoryCalls"] = [];
  const config: CacheCapConfig = { capEnabled: true, capFraction: 0.5, lowWater: 0.6, maxConcurrentRuns: 2, hardStopEnabled: true, hardMargin: 0.03 };
  class SpyGovernor extends DiskGovernor {
    override register(id: string, run: GovernedRun): void { governor.run = run; super.register(id, run); }
  }
  const diskGovernor = o.governor ? new SpyGovernor({
    config, log: logs.logger, volumeTotalBytes: () => 1 << 30, thresholdOf: () => 0.9,
    measure: async home => { governor.measures.push(home); return { cacheBytes: 1, goBuild: 0, npm: 0, goModules: 0 } as never; },
    trim: async home => { governor.trims.push(home); return {} as never; },
  }) : undefined;
  let rigRef: Rig | undefined;
  const runner = runnerWith((id, codex, addr) => {
    const made = o.factory
      ? o.factory(rigRef!, id, codex, addr)
      : o.addrAware && addr?.generation !== undefined
        ? { executor: successorExecutor, homeDir: path.join(homeDir, runDirKey(id, addr.generation)) }
        : { executor, homeDir: runHome };
    factoryCalls.push({ generation: addr?.generation, home: made.homeDir });
    return made;
  }, o.gitlab ?? fakeGitlab().gitlab, AUTH_TOKEN, logs.logger, {
    authority: sRt, codexBoundaryDeadlineMs: 5000, recoveryRetryMs: 5,
    restoreIdentity: { procRoot: sProc, hostname: host, ...(sMode === "hosted" ? { podUid: POD_UID } : {}) },
    independentRestore: o.independentRestore ?? rigDefaults.independentRestore,
    ...(diskGovernor ? { diskGovernor } : {}),
    ...o.runnerExtra,
  });
  const recovery = (): RecoveryCoordinator => (runner as unknown as { recovery: RecoveryCoordinator }).recovery;
  const rig: Rig = {
    claim, runId, bare, dataDir, pAttempt, pPath: clone.path, pInc, sInc, pRt, sRt, sProc, pProc, tmp, pinSha,
    journalKey: `uzi-recovery.${BRANCH}.clone`, ledgerKey: `uzi-attempts.${BRANCH}.entry`, runHome, provisionDir,
    recovery, runner, logs, counters, governor, executor, successorExecutor, factoryCalls, homeRoot: homeDir, lifetime,
    pin: over => recovery().pin({ runId, sourceSha: pinSha, kind: "issue", branch: BRANCH, generation: 1, ...over }),
  };
  rigRef = rig;
  if (o.pin !== false) {
    const pinned = await rig.pin();
    assert.ok(pinned, "the early pin record is written");
  }
  return rig;
}

export const cfgGet = (r: Rig, key: string): string | undefined => { try { return gitIn(r.bare, ["config", "--local", "--get", key]) || undefined; } catch { return undefined; } };
export const cfgAll = (r: Rig, key: string): string[] => { try { return gitIn(r.bare, ["config", "--local", "--get-all", key]).split("\n").filter(Boolean); } catch { return []; } };
export const cfgSet = (r: Rig, key: string, value: string): void => { gitIn(r.bare, ["config", "--local", key, value]); };
export const cfgAdd = (r: Rig, key: string, value: string): void => { gitIn(r.bare, ["config", "--local", "--add", key, value]); };
export const journalOf = (r: Rig): Record<string, unknown> => JSON.parse(cfgGet(r, r.journalKey)!) as Record<string, unknown>;
export const setJournal = (r: Rig, extra: Record<string, unknown>): void => cfgSet(r, r.journalKey, JSON.stringify({ ...journalOf(r), ...extra }));

export async function slotOf(r: Rig, p: string): Promise<SlotSnapshot | null> { return r.sRt.peekSlot(p); }

export interface Snapshot {
  config: string; journal: string | undefined; ledger: string[]; slot: SlotSnapshot | null; bareSlot: SlotSnapshot | null;
  clone: string; home: string; provision: string;
}
export async function snapshot(r: Rig): Promise<Snapshot> {
  return {
    config: fs.readFileSync(path.join(r.bare, "config"), "utf8"), journal: cfgGet(r, r.journalKey), ledger: cfgAll(r, r.ledgerKey),
    slot: await slotOf(r, r.pPath), bareSlot: await slotOf(r, r.bare),
    clone: treeDigest(r.pPath), home: treeDigest(r.runHome), provision: treeDigest(r.provisionDir),
  };
}

/** Replaces the runner's authority peek for the FIRST call made from a frame named `marker` (everything else, the test's own
 *  snapshots included, reads through). */
export function peekOnceFrom(r: Rig, marker: string, impl: () => SlotSnapshot | null): void {
  let done = false;
  const original = r.sRt.peekSlot.bind(r.sRt);
  mock.method(r.sRt, "peekSlot", async (p: string) => {
    if (!done && (new Error().stack ?? "").includes(marker)) { done = true; return impl(); }
    return original(p);
  });
}

export const ownLedger = (r: Rig): Record<string, unknown> => JSON.parse(cfgAll(r, r.ledgerKey)[0]!) as Record<string, unknown>;
export const olderLedgerEntry = (r: Rig, state: string): string => {
  const id = mintAttemptId(0);
  return JSON.stringify({ attemptId: id, runId: r.runId, clonePath: `${r.pPath.replace(/\.attempt-.*$/, "")}.attempt-${id}`, state });
};

// ---- traps: nothing may reach P's path or the legacy P-touching steps ------------------------------------------------

export interface Traps { hits: string[]; calls: Record<string, number>; armed: boolean }

/** Arm, from now until the test ends: fs calls naming P's path (the authority's slot-key lstat walk excepted), and calls of the
 *  steps that read, capture, reserve or admit the predecessor. */
export function armTraps(r: Rig): Traps {
  const traps: Traps = { hits: [], calls: {}, armed: true };
  const under = (arg: unknown): boolean => typeof arg === "string" && (arg === r.pPath || arg.startsWith(r.pPath + path.sep));
  const wrap = (target: object, names: string[], label: string): void => {
    for (const name of names) {
      const original = (target as Record<string, unknown>)[name];
      if (typeof original !== "function") continue;
      mock.method(target as Record<string, (...a: unknown[]) => unknown>, name, function (this: unknown, ...args: unknown[]) {
        const stack = new Error().stack ?? "";
        if (traps.armed && under(args[0]) && !stack.includes("completion-authority")) traps.hits.push(`${label}.${name} ${String(args[0])}`);
        return (original as (...a: unknown[]) => unknown).apply(this, args);
      });
    }
  };
  wrap(fsp, ["lstat", "stat", "readdir", "readFile", "open", "opendir", "access", "realpath", "readlink", "rm", "rename", "writeFile", "mkdir", "utimes", "chmod"], "fsp");
  wrap(fs, ["lstatSync", "statSync", "readdirSync", "readFileSync", "openSync", "accessSync", "realpathSync", "existsSync", "rmSync", "renameSync"], "fs");
  syncBuiltinESMExports();
  const count = (target: object, name: string, label = name): void => {
    const original = (target as Record<string, unknown>)[name];
    if (typeof original !== "function") return;
    mock.method(target as Record<string, (...a: unknown[]) => unknown>, name, function (this: unknown, ...args: unknown[]) {
      if (traps.armed) traps.calls[label] = (traps.calls[label] ?? 0) + 1;
      return (original as (...a: unknown[]) => unknown).apply(this, args);
    });
  };
  for (const name of ["reserveRecoveryIteration", "blockRecoveryEpisode", "checkedRecovery", "discoverRetainedRecovery", "matchMaintenanceSourceFamily",
    "terminalRetainedSnapshot", "recoveryReservationSnapshot"]) count(git, name);
  for (const name of ["captureRecoveryRestorePoint", "admitPredecessorSource", "logPredecessorMemorySettlement"]) count(r.runner, name);
  for (const name of ["runGit", "runGitAsRunner"]) {
    const original = (git as unknown as Record<string, unknown>)[name];
    if (typeof original !== "function") continue;
    mock.method(git as unknown as Record<string, (...a: unknown[]) => unknown>, name, function (this: unknown, ...args: unknown[]) {
      if (traps.armed && under(args[0])) traps.hits.push(`git.${name} ${String(args[0])}`);
      return (original as (...a: unknown[]) => unknown).apply(this, args);
    });
  }
  return traps;
}

/** Per-test cleanup, called from the importing file's afterEach. */
export function cleanupRig(): void {
  mock.restoreAll();
  syncBuiltinESMExports();
  for (const dir of tmpRoots) fs.rmSync(dir, { recursive: true, force: true });
  tmpRoots = [];
  resetProcessBoundaryForTests();
}
export function resetRigRoots(): void { tmpRoots = []; }

export const failed = (r: Rig) => api.states.filter(s => s.runId === r.runId && s.body.status === "failed");
export const logged = (r: Rig, msg: string) => r.logs.lines.filter(l => (l as { msg: string }).msg === msg) as Array<Record<string, unknown>>;

/** The route's common contract: a nonterminal stop that touched nothing, with every P-touching step never reached. */
export async function assertStoppedUntouched(r: Rig, before: Snapshot, traps: Traps): Promise<void> {
  assert.equal(failed(r).length, 0, "no failed report");
  assert.equal(api.states.filter(s => s.runId === r.runId && s.body.status === "completed").length, 0, "no completed report");
  assert.deepEqual(await snapshot(r), before, "P's journal, ledger, slots, clone, HOME and provision directory are byte-identical");
  assert.deepEqual(traps.hits.slice(0, 5), [], "no fs call named P's path");
  assert.deepEqual(traps.calls, {}, "no reservation, episode block, capture, admission, legacy discovery or family match");
  assert.equal(r.counters.ran, 0, "no model turn");
  assert.equal(fs.existsSync(r.runHome) && fs.existsSync(r.provisionDir), true, "agent-home and provision survive");
}

export async function drive(r: Rig): Promise<{ before: Snapshot; traps: Traps }> {
  const before = await snapshot(r);
  const traps = armTraps(r);
  try { await r.runner.execute(r.claim); } finally { traps.armed = false; }
  return { before, traps };
}

export const refusals = (r: Rig) => logged(r, "independent restore refused; predecessor retained");
