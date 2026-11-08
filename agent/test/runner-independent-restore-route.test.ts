// Issue #3644 Gate B (m4a): the independent-restore route of a retained claim whose predecessor's authority slot is still
// active under a dead worker incarnation. Production RunRunner + real Git + the durable authority, driven through
// RunRunner.execute(). The predecessor P is a real attempt clone with its journal, ledger entry, pin record and an ACTIVE slot
// owned by another incarnation; the successor S is the runner's own AuthorityRuntime with a different incarnation.
//
// m4a only DECIDES: every refusal, and the eligible verdict, ends in a nonterminal stop that writes nothing to P (journal,
// ledger, slot, clone, HOME, provision directory) and never reaches the reservation, the capture, or the admission.
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { createHash, randomBytes, randomUUID } from "node:crypto";
import fs from "node:fs";
import fsp from "node:fs/promises";
import { syncBuiltinESMExports } from "node:module";
import os from "node:os";
import path from "node:path";
import { afterEach, beforeEach, describe, it, mock } from "node:test";
import { acquireWorkerLifetime } from "../src/authority-lock.js";
import { AuthorityRuntime } from "../src/authority-runtime.js";
import { CacheTargetBinding } from "../src/cache-target.js";
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
import { api, client, fakeGitlab, fx, git, gitlabClaim, homeDir, installHarness, runnerWith } from "./runner-harness.js";

installHarness();

const AUTH_TOKEN = "unit-join-" + randomBytes(12).toString("hex");
const IID = 3644;
const BRANCH = `agent/issue-${IID}`;
const CID = "ab".repeat(32);
const BOOT = "11111111-2222-3333-4444-555555555555";
const FEATURES = ["active_run_snapshot", "claim_generation_fence", "recovery_completed_publication_v1"];
const GIT_ENV: NodeJS.ProcessEnv = { GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", PATH: process.env.PATH };
const host = (): string => CID.slice(0, 12);

const gitIn = (dir: string, args: string[]): string => execFileSync("git", ["-C", dir, ...args], { env: GIT_ENV, encoding: "utf8", stdio: "pipe" }).trim();

/** Names, modes, sizes, mtimes and bytes of a tree: a directory's byte identity. */
function treeDigest(root: string): string {
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

interface ProcOpts { ticks?: string; boot?: string; hidden?: boolean }
async function makeProc(dir: string, o: ProcOpts = {}): Promise<void> {
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

interface Rig {
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
}

let tmpRoots: string[] = [];

interface RigOptions {
  /** Claim overrides. */
  claim?: Partial<ClaimResponse>;
  features?: string[];
  /** Default true: write the early pin of the default tip (bound as on_default). */
  pin?: boolean;
  /** Default "ok". */
  pBoundary?: "ok" | "missing" | "invalid" | "hosted" | "failed" | "slot_mismatch" | "same_execution";
  /** Default "ok": S's process boundary. */
  sBoundary?: "ok" | "none" | "missing_file";
  /** Default "clean". */
  sLive?: "clean" | "hidden";
  governor?: boolean;
  runnerExtra?: Parameters<typeof runnerWith>[4];
}

async function buildRig(o: RigOptions = {}): Promise<Rig> {
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
  const clone = await git.createOrAttachRunnerClone(bare, IID, noProofReseed, runId, false, undefined,
    { attemptId: pAttempt, isLive: () => false, beforeSeed: async () => {} });
  fs.writeFileSync(path.join(clone.path, "ONLY_COPY.txt"), "work that only the predecessor's clone holds\n");
  await git.markRecoveryCapture(bare, clone.path, BRANCH, runId, clone.attemptId);

  // The authority: P's active slot under a dead incarnation, S as the restarted worker.
  const lifetime = await acquireWorkerLifetime(dataDir, testLockOptions());
  const pInc = randomUUID();
  const sInc = randomUUID();
  const pRt = new AuthorityRuntime({ lifetime, workerToken: AUTH_TOKEN, workerIncarnation: pInc, timeoutMs: 5000 });
  const sRt = new AuthorityRuntime({ lifetime, workerToken: AUTH_TOKEN, workerIncarnation: sInc, timeoutMs: 5000 });
  await pRt.withTransaction(() => pRt.hooks.admitSourceUse(clone.path, "seed-attempt"));

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
    const w = await writeBoundaryRecord({ ...common, procRoot: pProc, incarnation: pInc, podUid: "123e4567-e89b-12d3-a456-426614174000" });
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
    const w = await establishProcessBoundary({ ...common, procRoot: sProc, incarnation: sInc });
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
  const executor: Executor = {
    run: async () => { counters.ran++; throw new Error("a refused or deferred restore runs no model"); },
    reclaimCachesInPlace: async (): Promise<RunCacheDropResult> => { counters.reclaims++; return { dropped: [], absent: [], failed: [], skipped: [] }; },
  };
  const config: CacheCapConfig = { capEnabled: true, capFraction: 0.5, lowWater: 0.6, maxConcurrentRuns: 2, hardStopEnabled: true, hardMargin: 0.03 };
  class SpyGovernor extends DiskGovernor {
    override register(id: string, run: GovernedRun): void { governor.run = run; super.register(id, run); }
  }
  const diskGovernor = o.governor ? new SpyGovernor({
    config, log: logs.logger, volumeTotalBytes: () => 1 << 30, thresholdOf: () => 0.9,
    measure: async home => { governor.measures.push(home); return { cacheBytes: 1, goBuild: 0, npm: 0, goModules: 0 } as never; },
    trim: async home => { governor.trims.push(home); return {} as never; },
  }) : undefined;
  const runner = runnerWith(() => ({ executor, homeDir: runHome }), fakeGitlab().gitlab, AUTH_TOKEN, logs.logger, {
    authority: sRt, codexBoundaryDeadlineMs: 5000, recoveryRetryMs: 5,
    restoreIdentity: { procRoot: sProc, hostname: host },
    ...(diskGovernor ? { diskGovernor } : {}),
    ...o.runnerExtra,
  });
  const recovery = (): RecoveryCoordinator => (runner as unknown as { recovery: RecoveryCoordinator }).recovery;
  const rig: Rig = {
    claim, runId, bare, dataDir, pAttempt, pPath: clone.path, pInc, sInc, pRt, sRt, sProc, pProc, tmp, pinSha,
    journalKey: `uzi-recovery.${BRANCH}.clone`, ledgerKey: `uzi-attempts.${BRANCH}.entry`, runHome, provisionDir,
    recovery, runner, logs, counters, governor,
    pin: over => recovery().pin({ runId, sourceSha: pinSha, kind: "issue", branch: BRANCH, generation: 1, ...over }),
  };
  if (o.pin !== false) {
    const pinned = await rig.pin();
    assert.ok(pinned, "the early pin record is written");
  }
  return rig;
}

const cfgGet = (r: Rig, key: string): string | undefined => { try { return gitIn(r.bare, ["config", "--local", "--get", key]) || undefined; } catch { return undefined; } };
const cfgAll = (r: Rig, key: string): string[] => { try { return gitIn(r.bare, ["config", "--local", "--get-all", key]).split("\n").filter(Boolean); } catch { return []; } };
const cfgSet = (r: Rig, key: string, value: string): void => { gitIn(r.bare, ["config", "--local", key, value]); };
const cfgAdd = (r: Rig, key: string, value: string): void => { gitIn(r.bare, ["config", "--local", "--add", key, value]); };
const journalOf = (r: Rig): Record<string, unknown> => JSON.parse(cfgGet(r, r.journalKey)!) as Record<string, unknown>;
const setJournal = (r: Rig, extra: Record<string, unknown>): void => cfgSet(r, r.journalKey, JSON.stringify({ ...journalOf(r), ...extra }));

async function slotOf(r: Rig, p: string): Promise<SlotSnapshot | null> { return r.sRt.peekSlot(p); }

interface Snapshot {
  config: string; journal: string | undefined; ledger: string[]; slot: SlotSnapshot | null; bareSlot: SlotSnapshot | null;
  clone: string; home: string; provision: string;
}
async function snapshot(r: Rig): Promise<Snapshot> {
  return {
    config: fs.readFileSync(path.join(r.bare, "config"), "utf8"), journal: cfgGet(r, r.journalKey), ledger: cfgAll(r, r.ledgerKey),
    slot: await slotOf(r, r.pPath), bareSlot: await slotOf(r, r.bare),
    clone: treeDigest(r.pPath), home: treeDigest(r.runHome), provision: treeDigest(r.provisionDir),
  };
}

/** Replaces the runner's authority peek for the FIRST call made from a frame named `marker` (everything else, the test's own
 *  snapshots included, reads through). */
function peekOnceFrom(r: Rig, marker: string, impl: () => SlotSnapshot | null): void {
  let done = false;
  const original = r.sRt.peekSlot.bind(r.sRt);
  mock.method(r.sRt, "peekSlot", async (p: string) => {
    if (!done && (new Error().stack ?? "").includes(marker)) { done = true; return impl(); }
    return original(p);
  });
}

const ownLedger = (r: Rig): Record<string, unknown> => JSON.parse(cfgAll(r, r.ledgerKey)[0]!) as Record<string, unknown>;
const olderLedgerEntry = (r: Rig, state: string): string => {
  const id = mintAttemptId(0);
  return JSON.stringify({ attemptId: id, runId: r.runId, clonePath: `${r.pPath.replace(/\.attempt-.*$/, "")}.attempt-${id}`, state });
};

// ---- traps: nothing may reach P's path or the legacy P-touching steps ------------------------------------------------

interface Traps { hits: string[]; calls: Record<string, number>; armed: boolean }

/** Arm, from now until the test ends: fs calls naming P's path (the authority's slot-key lstat walk excepted), and calls of the
 *  steps that read, capture, reserve or admit the predecessor. */
function armTraps(r: Rig): Traps {
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

afterEach(() => {
  mock.restoreAll();
  syncBuiltinESMExports();
  for (const dir of tmpRoots) fs.rmSync(dir, { recursive: true, force: true });
  tmpRoots = [];
  resetProcessBoundaryForTests();
});

const failed = (r: Rig) => api.states.filter(s => s.runId === r.runId && s.body.status === "failed");
const logged = (r: Rig, msg: string) => r.logs.lines.filter(l => (l as { msg: string }).msg === msg) as Array<Record<string, unknown>>;

/** The route's common contract: a nonterminal stop that touched nothing, with every P-touching step never reached. */
async function assertStoppedUntouched(r: Rig, before: Snapshot, traps: Traps): Promise<void> {
  assert.equal(failed(r).length, 0, "no failed report");
  assert.equal(api.states.filter(s => s.runId === r.runId && s.body.status === "completed").length, 0, "no completed report");
  assert.deepEqual(await snapshot(r), before, "P's journal, ledger, slots, clone, HOME and provision directory are byte-identical");
  assert.deepEqual(traps.hits.slice(0, 5), [], "no fs call named P's path");
  assert.deepEqual(traps.calls, {}, "no reservation, episode block, capture, admission, legacy discovery or family match");
  assert.equal(r.counters.ran, 0, "no model turn");
  assert.equal(fs.existsSync(r.runHome) && fs.existsSync(r.provisionDir), true, "agent-home and provision survive");
}

async function drive(r: Rig): Promise<{ before: Snapshot; traps: Traps }> {
  const before = await snapshot(r);
  const traps = armTraps(r);
  try { await r.runner.execute(r.claim); } finally { traps.armed = false; }
  return { before, traps };
}

const refusals = (r: Rig) => logged(r, "independent restore refused; predecessor retained");

// ---- tests ------------------------------------------------------------------------------------------------------------------

describe("the independent restore route (issue #3644 m4a)", () => {
  beforeEach(() => { tmpRoots = []; });

  it("an eligible foreign-active predecessor stops nonterminally, writes nothing, and says seeding is not enabled", async () => {
    const r = await buildRig();
    const { before, traps } = await drive(r);
    const eligible = logged(r, "independent restore eligible; seeding not enabled in this build");
    assert.equal(eligible.length, 1, JSON.stringify(r.logs.lines.filter(l => /restore/i.test(String((l as { msg: string }).msg)))));
    const verdict = eligible[0]!.verdict as { predecessorIncarnation: string; boot: string };
    assert.equal(verdict.predecessorIncarnation, r.pInc);
    assert.equal(verdict.boot, "same_boot");
    assert.equal(eligible[0]!.pin_generation, 1);
    assert.equal(eligible[0]!.source_sha, r.pinSha);
    assert.equal(refusals(r).length, 0);
    await assertStoppedUntouched(r, before, traps);
  });

  it("scope control: a foreign-active slot of an unrelated bare does not block", async () => {
    const r = await buildRig();
    const other = path.join(r.dataDir, "runner", "some-other-repo", "issue-5");
    fs.mkdirSync(other, { recursive: true });
    await r.pRt.withTransaction(() => r.pRt.hooks.admitSourceUse(other, "seed-canonical"));
    const { before, traps } = await drive(r);
    assert.equal(logged(r, "independent restore eligible; seeding not enabled in this build").length, 1);
    assert.equal(refusals(r).length, 0);
    await assertStoppedUntouched(r, before, traps);
  });

  it("without active_run_snapshot the legacy path is unchanged: the admission refuses and the run fails preservation_failed", async () => {
    const r = await buildRig({ features: ["claim_generation_fence"] });
    const spies = { reserve: 0, discover: 0 };
    const reserve = git.reserveRecoveryIteration.bind(git);
    git.reserveRecoveryIteration = ((...args: Parameters<typeof reserve>) => { spies.reserve++; return reserve(...args); }) as typeof git.reserveRecoveryIteration;
    const discover = git.discoverRetainedRecovery.bind(git);
    git.discoverRetainedRecovery = ((...args: Parameters<typeof discover>) => { spies.discover++; return discover(...args); }) as typeof git.discoverRetainedRecovery;
    await r.runner.execute(r.claim);
    assert.ok(spies.discover >= 1, "legacy discovery ran");
    assert.ok(spies.reserve >= 1, "the legacy path reserved an iteration");
    assert.equal(logged(r, "retained predecessor refused by source-use admission; capture not started").length, 1);
    assert.equal(failed(r).length, 1, "today's outcome: the run is failed");
    assert.equal(logged(r, "independent restore eligible; seeding not enabled in this build").length, 0);
    assert.equal(refusals(r).length, 0);
  });

  it("a claim generation of 1 never takes the restore route", async () => {
    const r = await buildRig({ claim: { claim_generation: 1 } });
    await r.runner.execute(r.claim);
    assert.equal(logged(r, "independent restore eligible; seeding not enabled in this build").length, 0);
    assert.equal(refusals(r).length, 0);
    assert.equal(logged(r, "retained predecessor is foreign-owned; taking the independent restore route").length, 0);
  });

  describe("every refusal stops nonterminally with no effects and logs its reason", () => {
    const cases: Array<{ name: string; reason: string; rig?: RigOptions; arrange?: (r: Rig) => Promise<void> | void }> = [
      { name: "no pin record", reason: "no_eligible_pin", rig: { pin: false } },
      { name: "a pin of another generation", reason: "no_eligible_pin", rig: { pin: false }, arrange: async r => { await r.pin({ generation: 2 }); } },
      { name: "a pin of another branch", reason: "no_eligible_pin", rig: { pin: false }, arrange: async r => { await r.pin({ branch: "agent/issue-9999" }); } },
      { name: "a coverage-only record", reason: "no_eligible_pin", rig: { pin: false }, arrange: async r => {
        const rec = (await r.pin())!;
        const rc = r.recovery() as unknown as { writeRecordUnlocked(rec: RecoveryRecord): Promise<void> };
        await rc.writeRecordUnlocked({ ...rec, coverageDigest: "a".repeat(64) });
      } },
      { name: "a record of another version", reason: "no_eligible_pin", rig: { pin: false }, arrange: async r => {
        const rec = (await r.pin())!;
        const rc = r.recovery() as unknown as { writeRecordUnlocked(rec: RecoveryRecord): Promise<void> };
        await rc.writeRecordUnlocked({ ...rec, version: 2 as never });
      } },
      { name: "a pin that already carries a final request", reason: "no_eligible_pin", rig: { pin: false }, arrange: async r => {
        const rec = (await r.pin())!;
        const rc = r.recovery() as unknown as { writeRecordUnlocked(rec: RecoveryRecord): Promise<void> };
        await rc.writeRecordUnlocked({ ...rec, finalRequest: { disposition: "published" } as never });
      } },
      { name: "two ordinary pin records of the generation", reason: "pin_ambiguous", arrange: async r => {
        const rec = (await r.recovery().inspect(r.runId))[0]!;
        const rc = r.recovery() as unknown as { writeRecordUnlocked(rec: RecoveryRecord): Promise<void> };
        await rc.writeRecordUnlocked({ ...rec, captureId: randomUUID() });
      } },
      { name: "a finalization pin taken in another bare", reason: "pin_bare_mismatch", rig: { pin: false }, arrange: async r => {
        await r.pin({ finalizationPin: true, bareDir: "another-bare.git", defaultBranch: "main" });
      } },
      { name: "a pin that is not bound in the bare", reason: "pin_unbound", rig: { pin: false }, arrange: async r => {
        const tree = gitIn(r.bare, ["rev-parse", `${r.pinSha}^{tree}`]);
        const orphan = gitIn(r.bare, ["-c", "user.email=t@t", "-c", "user.name=t", "commit-tree", "-p", r.pinSha, "-m", "unbound", tree]);
        await r.pin({ sourceSha: orphan });
      } },
      { name: "a checkpoint tip that is not an ancestor of the pin", reason: "checkpoint_not_ancestor", arrange: r => {
        const tree = gitIn(r.bare, ["rev-parse", `${r.pinSha}^{tree}`]);
        const side = gitIn(r.bare, ["-c", "user.email=t@t", "-c", "user.name=t", "commit-tree", "-p", r.pinSha, "-m", "side", tree]);
        (r.claim as { checkpoint_tip?: string }).checkpoint_tip = side;
      } },
      { name: "a pending footprint on the predecessor's slot", reason: "predecessor_pending", arrange: async r => {
        await r.pRt.withTransaction(() => r.pRt.hooks.beginDestructive([r.pPath], "unsettled-destruction"));
      } },
      { name: "a completion source fence on the journal", reason: "uncertain_publication", arrange: r => {
        setJournal(r, { completionSourceFence: { version: 1, sourceUseEpoch: randomUUID(), sourceCoreDigest: "b".repeat(64) } });
      } },
      { name: "an episode on the journal", reason: "predecessor_not_plain", arrange: r => {
        setJournal(r, { recovery: { version: 1, source: { runId: r.runId, clonePath: r.pPath, attemptId: r.pAttempt }, attempts: 1,
          startedAt: 1000, deadline: 301_000, claimGeneration: 1, backoffMs: 0, stage: "capturing" } });
      } },
      { name: "a restore tip on the journal", reason: "predecessor_not_plain", arrange: r => { setJournal(r, { restoreTip: "c".repeat(40) }); } },
      { name: "a protected descriptor of the run", reason: "conflicting_attribution", arrange: r => {
        cfgSet(r, `uzi-retained.${r.runId}.journal`, JSON.stringify({ version: 1, branch: BRANCH, key: `issue-${IID}`, journal: journalOf(r) }));
      } },
      { name: "a stopped marker of the run", reason: "conflicting_attribution", arrange: r => { cfgAdd(r, `uzi-stopped.${r.runId}.entry`, "{}"); } },
      { name: "another live ledger entry of the run", reason: "conflicting_attribution", arrange: r => {
        const id = mintAttemptId(2);
        cfgAdd(r, r.ledgerKey, JSON.stringify({ attemptId: id, runId: r.runId, clonePath: `${r.pPath.replace(/\.attempt-.*$/, "")}.attempt-${id}`, state: "live" }));
      } },
      { name: "another key naming the predecessor's path", reason: "conflicting_attribution", arrange: r => {
        cfgSet(r, "uzi-recovery.agent/issue-9998.clone", JSON.stringify({ runId: randomUUID(), clonePath: r.pPath }));
      } },
      { name: "a running pending record on the bare", reason: "bare_busy", arrange: async r => {
        await r.pRt.withTransaction(() => r.pRt.hooks.beginBareMutation(r.bare, "unsettled-ref-deletion", ["refs/uzi-test/x"]));
      } },
      { name: "another run's foreign-active source in the same bare", reason: "census_foreign_active", arrange: async r => {
        const otherRun = randomUUID();
        const id = mintAttemptId(1);
        const other = path.join(path.dirname(r.pPath), `issue-77.attempt-${id}`);
        fs.mkdirSync(other, { recursive: true });
        await r.pRt.withTransaction(() => r.pRt.hooks.admitSourceUse(other, "seed-attempt"));
        cfgAdd(r, "uzi-attempts.agent/issue-77.entry", JSON.stringify({ attemptId: id, runId: otherRun, clonePath: other, state: "live" }));
      } },
      { name: "P's own ledger entry retired", reason: "predecessor_ledger", arrange: r => { cfgSet(r, r.ledgerKey, JSON.stringify({ ...ownLedger(r), state: "retired" })); } },
      { name: "P's own ledger entry reclaimed", reason: "predecessor_ledger", arrange: r => { cfgSet(r, r.ledgerKey, JSON.stringify({ ...ownLedger(r), state: "reclaimed" })); } },
      { name: "an older-generation live ledger entry of the run", reason: "conflicting_attribution", arrange: r => { cfgAdd(r, r.ledgerKey, olderLedgerEntry(r, "live")); } },
      { name: "an incomplete attribution config read", reason: "attribution_unverifiable", arrange: () => {
        mock.method(git, "readRestoreAttribution", async () => ({ entries: [], complete: false }));
      } },
      { name: "a slot that changed owner between classification and evaluation", reason: "predecessor_changed", arrange: r => {
        peekOnceFrom(r, "evaluateRestoreRoute", () => null);
      } },
      { name: "a slot peek that throws during classification (a slot one write ahead of its head)", reason: "slot_unreadable", arrange: r => {
        peekOnceFrom(r, "opaquePreDiscovery", () => { throw new Error("slot ahead of its head record"); });
      } },
      { name: "a slot peek that throws during evaluation", reason: "slot_unreadable", arrange: r => {
        peekOnceFrom(r, "evaluateRestoreRoute", () => { throw new Error("unreadable"); });
      } },
      { name: "a hosted predecessor", reason: "hosted_restore_not_delivered", rig: { pBoundary: "hosted" } },
      { name: "a predecessor with a failed cross-check", reason: "cross_check_failed", rig: { pBoundary: "failed" } },
      { name: "a predecessor of another slot", reason: "slot_mismatch", rig: { pBoundary: "slot_mismatch" } },
      { name: "a missing predecessor boundary record", reason: "predecessor_record_missing", rig: { pBoundary: "missing" } },
      { name: "an invalid predecessor boundary record", reason: "predecessor_record_invalid", rig: { pBoundary: "invalid" } },
      { name: "the same execution", reason: "same_execution", rig: { pBoundary: "same_execution" } },
      { name: "a hidden procfs", reason: "procfs_hidden", rig: { sLive: "hidden" } },
      { name: "no process boundary", reason: "self_record_invalid", rig: { sBoundary: "none" } },
      { name: "this process's record missing on disk", reason: "self_record_invalid", rig: { sBoundary: "missing_file" } },
      { name: "certified completion not negotiated", reason: "certified_completion_unavailable", rig: { features: ["active_run_snapshot", "claim_generation_fence"] } },
      { name: "an unguarded inventory", reason: "certified_completion_unavailable", rig: { claim: { inventory_guarded: false } } },
    ];
    for (const c of cases) {
      it(`${c.name}: ${c.reason}`, async () => {
        const r = await buildRig(c.rig);
        await c.arrange?.(r);
        const { before, traps } = await drive(r);
        const logs = refusals(r);
        assert.equal(logs.length, 1, JSON.stringify(r.logs.lines.filter(l => /restore|retained/i.test(String((l as { msg: string }).msg)))));
        assert.equal(logs[0]!.reason, c.reason);
        assert.equal(logged(r, "independent restore eligible; seeding not enabled in this build").length, 0);
        const feed = api.messages(r.runId).map(m => JSON.stringify(m.payload)).filter(t => t.includes("independent restore refused"));
        assert.equal(feed.length, 1, "one bounded feed line");
        assert.ok(feed[0]!.includes(c.reason) && feed[0]!.length < 400);
        await assertStoppedUntouched(r, before, traps);
      });
    }
  });

  describe("positive controls", () => {
    it("an older retired ledger entry of the run is history, not attribution", async () => {
      const r = await buildRig();
      cfgAdd(r, r.ledgerKey, olderLedgerEntry(r, "retired"));
      const { before, traps } = await drive(r);
      assert.equal(logged(r, "independent restore eligible; seeding not enabled in this build").length, 1);
      assert.equal(refusals(r).length, 0);
      await assertStoppedUntouched(r, before, traps);
    });

    it("a journal with another key order is classified by shape, from the exact raw value discovery read", async () => {
      const r = await buildRig();
      const j = journalOf(r);
      const raw = JSON.stringify({ runId: j.runId, attemptId: j.attemptId, clonePath: j.clonePath });
      assert.notEqual(raw, cfgGet(r, r.journalKey), "the reordered value differs byte-wise");
      cfgSet(r, r.journalKey, raw);
      const { before, traps } = await drive(r);
      assert.equal(refusals(r).length, 0, JSON.stringify(refusals(r)));
      assert.equal(logged(r, "independent restore eligible; seeding not enabled in this build").length, 1);
      await assertStoppedUntouched(r, before, traps);
    });
  });

  describe("a blocked or memory-parked foreign-owned predecessor keeps its legacy outcome", () => {
    it("a blocked recovery stage is the unchanged blocked-claim failure", async () => {
      const r = await buildRig();
      setJournal(r, { recovery: { version: 1, source: { runId: r.runId, clonePath: r.pPath, attemptId: r.pAttempt }, attempts: 3,
        startedAt: 1000, deadline: 301_000, claimGeneration: 1, backoffMs: 0, stage: "blocked", blocker: "capture_failed" } });
      await r.runner.execute(r.claim);
      assert.equal(logged(r, "independent restore eligible; seeding not enabled in this build").length, 0);
      assert.equal(refusals(r).length, 0);
      assert.equal(logged(r, "retained predecessor is foreign-owned; taking the independent restore route").length, 0);
      assert.equal(failed(r).length, 1, "the legacy blocked path fails the run");
    });
  });

  describe("a memory-parked foreign-owned predecessor keeps its legacy outcome", () => {
    it("a journal with a memoryPark is not routed to the restore route", async () => {
      const r = await buildRig();
      const slot = (await slotOf(r, r.pPath))!;
      setJournal(r, { memoryPark: { version: 1, binding: { runId: r.runId, claimGeneration: 1, workerId: "worker-1", registerNonce: "nonce-1", memoryEpisode: 0 },
        ack: "not_sent", origin: { flightId: randomUUID(), attemptId: r.pAttempt, executorIncarnation: null, authorityIncarnation: r.pInc, sourceEpoch: slot.epoch } } });
      let discovered = 0;
      const discover = git.discoverRetainedRecovery.bind(git);
      git.discoverRetainedRecovery = ((...args: Parameters<typeof discover>) => { discovered++; return discover(...args); }) as typeof git.discoverRetainedRecovery;
      await r.runner.execute(r.claim);
      assert.ok(discovered >= 1, "legacy discovery ran");
      assert.equal(logged(r, "independent restore eligible; seeding not enabled in this build").length, 0);
      assert.equal(refusals(r).length, 0);
      assert.equal(logged(r, "retained predecessor is foreign-owned; taking the independent restore route").length, 0);
    });
  });

  describe("composite and invalid restore entries", () => {
    it("a restore entry of the branch that is not a valid composite stops the claim, not a generic failure", async () => {
      const r = await buildRig();
      cfgSet(r, `uzi-restore.${BRANCH}.journal`, "not a restore entry");
      const before = await snapshot(r);
      const traps = armTraps(r);
      try { await r.runner.execute(r.claim); } finally { traps.armed = false; }
      assert.equal(logged(r, "restore entry present; claim stopped").length, 1);
      assert.equal(failed(r).length, 0, "no generic failure report");
      assert.equal(refusals(r).length, 0);
      assert.deepEqual(traps.hits, []);
      assert.deepEqual(traps.calls, {});
      assert.equal(fs.readFileSync(path.join(r.bare, "config"), "utf8"), before.config);
      assert.equal(treeDigest(r.pPath), before.clone);
    });
  });

  describe("the disk governor's cache target", () => {
    it("a retained re-claim starts unbound: a forced reclaim before classification and after a refusal touches nothing of the predecessor's", async () => {
      const r = await buildRig({ governor: true, pBoundary: "hosted" });
      const homeBefore = treeDigest(r.runHome);
      let midRun: Promise<unknown> | undefined;
      const opaque = git.discoverOpaque.bind(git);
      git.discoverOpaque = (async (...args: Parameters<typeof opaque>) => {
        const found = await opaque(...args);
        // Classification has not run yet: the registered callbacks must find no target.
        midRun = Promise.all([r.governor.run!.reclaimInPlace(), r.governor.run!.withHome!(async h => h), Promise.resolve(r.governor.run!.home)]);
        return found;
      }) as typeof git.discoverOpaque;
      await r.runner.execute(r.claim);
      assert.equal(refusals(r)[0]?.reason, "hosted_restore_not_delivered");
      const [dropped, held] = await (midRun as Promise<[unknown, unknown]>);
      assert.equal(dropped, undefined);
      assert.equal(held, undefined);
      const run = r.governor.run!;
      assert.equal(typeof run.home === "function" ? (run.home as () => string | undefined)() : run.home, undefined, "still unbound after the refusal");
      assert.equal(await run.reclaimInPlace(), undefined, "a reclaim after the refusal drops nothing");
      assert.equal(await run.withHome!(async h => h), undefined);
      assert.equal(r.counters.reclaims, 0, "the predecessor's executor was never asked to reclaim");
      assert.deepEqual(r.governor.measures, []);
      assert.deepEqual(r.governor.trims, []);
      assert.equal(treeDigest(r.runHome), homeBefore, "the HOME is byte-identical");
    });

    const homeOf = (run: GovernedRun): string | undefined => typeof run.home === "function" ? run.home() : run.home;

    it("a generation-1 claim binds at registration: the registered callbacks reach its executor and HOME", async () => {
      const r = await buildRig({ governor: true, claim: { claim_generation: 1 } });
      let homeAtDiscovery: string | undefined;
      let reclaimed: RunCacheDropResult | undefined;
      const opaque = git.discoverOpaque.bind(git);
      git.discoverOpaque = (async (...args: Parameters<typeof opaque>) => {
        const found = await opaque(...args);
        homeAtDiscovery = homeOf(r.governor.run!);
        reclaimed = await r.governor.run!.reclaimInPlace();
        return found;
      }) as typeof git.discoverOpaque;
      await r.runner.execute(r.claim);
      assert.equal(homeAtDiscovery, r.runHome, "bound at registration, before any discovery");
      assert.ok(reclaimed, "the reclaim ran");
      assert.equal(r.counters.reclaims, 1, "through the claim's executor");
    });

    it("the legacy route binds the initial executor and HOME before any HOME use", async () => {
      // No active_run_snapshot: the same retained predecessor takes today's route.
      const r = await buildRig({ governor: true, features: ["claim_generation_fence"] });
      let before: string | undefined | null = null;
      let after: string | undefined | null = null;
      const opaque = git.discoverOpaque.bind(git);
      git.discoverOpaque = (async (...args: Parameters<typeof opaque>) => {
        const found = await opaque(...args);
        before = homeOf(r.governor.run!);
        return found;
      }) as typeof git.discoverOpaque;
      const reserve = git.reserveRecoveryIteration.bind(git);
      git.reserveRecoveryIteration = (async (...args: Parameters<typeof reserve>) => {
        after = homeOf(r.governor.run!);
        await r.governor.run!.reclaimInPlace();
        return reserve(...args);
      }) as typeof git.reserveRecoveryIteration;
      await r.runner.execute(r.claim);
      assert.equal(before, undefined, "a retained re-claim starts unbound");
      assert.equal(after, r.runHome, "legacy routing bound it before the recovery reserved an iteration");
      assert.equal(r.counters.reclaims, 1);
    });
  });
});

describe("CacheTargetBinding", () => {
  const target = (name: string) => ({ executor: { name } as unknown as Executor, home: `/h/${name}` });
  it("is unbound until bound, and refuses to run anything while unbound", async () => {
    const b = new CacheTargetBinding<Executor>();
    assert.equal(b.home(), undefined);
    assert.equal(await b.use(async () => "ran"), undefined);
    b.bind(target("a"));
    assert.equal(b.home(), "/h/a");
    assert.equal(await b.use(async t => t.home), "/h/a");
    b.unbind();
    assert.equal(await b.use(async () => "ran"), undefined);
  });
  it("one snapshot per operation: a rebind during an operation does not change what it sees", async () => {
    const b = new CacheTargetBinding<Executor>();
    b.bind(target("a"));
    let release!: () => void;
    const gate = new Promise<void>(r => { release = r; });
    const op = b.use(async t => { await gate; return [t.home, t.home]; });
    assert.equal(b.inFlight, 1);
    b.bind(target("b"));
    release();
    assert.deepEqual(await op, ["/h/a", "/h/a"]);
    assert.equal(b.home(), "/h/b");
  });
  it("drainThenBind waits for in-flight operations, refuses new ones meanwhile, then binds", async () => {
    const b = new CacheTargetBinding<Executor>();
    b.bind(target("a"));
    let release!: () => void;
    const gate = new Promise<void>(r => { release = r; });
    const op = b.use(async t => { await gate; return t.home; });
    let bound = false;
    const swap = b.drainThenBind(target("b")).then(() => { bound = true; });
    await new Promise(r => setImmediate(r));
    assert.equal(bound, false, "still draining");
    assert.equal(b.home(), undefined, "the governor sees no target while draining");
    assert.equal(await b.use(async () => "late"), undefined, "a new operation is refused during the drain");
    release();
    assert.equal(await op, "/h/a");
    await swap;
    assert.equal(b.home(), "/h/b");
    assert.equal(await b.use(async t => t.home), "/h/b");
  });
  it("an abort while draining restores the previous binding and rejects", async () => {
    const b = new CacheTargetBinding<Executor>();
    b.bind(target("a"));
    let release!: () => void;
    const gate = new Promise<void>(r => { release = r; });
    const op = b.use(async () => { await gate; });
    const ac = new AbortController();
    const swap = b.drainThenBind(target("b"), ac.signal);
    ac.abort(new Error("stop"));
    await assert.rejects(swap, /stop/);
    assert.equal(b.home(), "/h/a");
    release();
    await op;
    assert.equal(b.inFlight, 0);
  });
});

