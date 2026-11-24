// issue #4632 U4: the retained-recovery inventory proof is serialized against same-key attempt seeds, end to end.
//
// Real bare repo, real attempt clones, real detached unmarked processes (a double fork, so they are not descendants of
// the scanning process) and the REAL observe-only helper scan. Only the proof's git (`worktreeStatus`) and the inventory
// read are scripted, so the test controls how long the proof's "git" stays in scope of the retained clone. The seed side
// is the real GitCache seed with a `beforeSeed` that runs the runner's own seed-mode sweep (`quiesceRunAttempt`).
import { after, afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { randomUUID } from "node:crypto";
import { GitCache, type AttemptSeedOptions, type InventorySection, type InventorySourceLockOptions } from "../src/git.js";
import { InventorySourceExclusionRefusal } from "../src/inventory-exclusion.js";
import { cloneKeyOf } from "../src/attempt-path.js";
import { RunResidueBlockedError } from "../src/residue-quarantine.js";
import { restoreJournalKey } from "../src/restore-journal.js";
import {
  LiveAttemptRegistry,
  mintAttemptId,
  inventoryClearVerdict,
  newRunAttempt,
  procfsTableAt,
  quiesceRunAttempt,
  reapProcesses,
  setQuiescenceViewForTests,
  type ProcTable,
  type ProcessQuiescence,
  type QuiesceRunOutcome,
  type QuiesceRunRequest,
  type ScanRequest,
} from "../src/run-quiescence.js";
import { acquireWorkerLifetime, withAuthorityTransaction } from "../src/authority-lock.js";
import { MaintenanceBudget } from "../src/maintenance-budget.js";
import { testLockOptions } from "./authority-test-support.js";
import { RUN_ATTEMPT_ENV, RUN_CLONE_KEY_ENV, workerSpawnNonce } from "../src/worker-spawn-mark.js";
import type { RunContext, ExecutorResult } from "../src/executor.js";
import type { ExecutorFactory } from "../src/runner.js";
import { noProofReseed, nullLogger, recordingLogger } from "./helpers.js";
import { makeFakeProcRoot, plantFakeProc, plantUnreadableUnattributed, scopedRealView } from "./fake-proc.js";
import { restoreHermeticView } from "./setup/hermetic-proc.js";
import { realProcfsSkip } from "./real-procfs.js";
import { api, fakeGitlab, fx, git, gitlabClaim, homeDir, installHarness, runnerWith } from "./runner-harness.js";

installHarness();

const GIT_ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0" };
const SKIP: string | false = process.platform !== "linux" ? "reads procfs (Linux only)" : realProcfsSkip("runner-inventory-seed-serialization");
const LATENCY_CAP_MS = 12_000;

let executed = 0;
after(() => { console.log(`serialization: ${executed} real-process cases executed${SKIP ? ` (skipped: ${SKIP})` : ""}`); });

// ─── process helpers ───────────────────────────────────────────────────────────────────────

const tick = (ms: number): Promise<void> => new Promise((r) => setTimeout(r, ms));
function deferred<T = void>(): { promise: Promise<T>; resolve: (v: T) => void } {
  let resolve!: (v: T) => void;
  const promise = new Promise<T>((r) => { resolve = r; });
  return { promise, resolve };
}
async function until(cond: () => boolean, ms = 10_000, what = "condition"): Promise<void> {
  const end = Date.now() + ms;
  while (!cond()) {
    if (Date.now() > end) throw new Error(`timed out waiting for ${what}`);
    await tick(10);
  }
}
/** Gone: no such process, or only a zombie awaiting its reaper. */
function gone(pid: number): boolean {
  try {
    const stat = fs.readFileSync(`/proc/${pid}/stat`, "utf8");
    return stat.slice(stat.lastIndexOf(")") + 2).startsWith("Z");
  } catch { return true; }
}

let scratch: string;
let pidFile: string;
const spawned: number[] = [];
let live: LiveAttemptRegistry;

/** A double-forked, unmarked (or `env`-marked) process with cwd `cwd`: setsid + background + the intermediate shell exits, so
 *  the process is reparented away and is NOT a descendant of this process. Returns its pid once the intermediate shell is gone. */
function detach(cwd: string, cmd: string, env: Record<string, string> = {}): number {
  fs.mkdirSync(cwd, { recursive: true });
  const before = fs.existsSync(pidFile) ? fs.readFileSync(pidFile, "utf8").split("\n").filter(Boolean).length : 0;
  execFileSync("setsid", ["sh", "-c", `${cmd} </dev/null >/dev/null 2>&1 & echo $! >> '${pidFile}'`], {
    cwd, env: { PATH: process.env.PATH ?? "/usr/bin:/bin", ...env }, stdio: ["ignore", "ignore", "ignore"],
  });
  const pids = fs.readFileSync(pidFile, "utf8").split("\n").filter(Boolean).map(Number);
  assert.equal(pids.length, before + 1, "the intermediate shell recorded exactly one new pid");
  const pid = pids.at(-1)!;
  spawned.push(pid);
  return pid;
}
const alive = (pid: number): boolean => !gone(pid);
function killAndWait(pid: number): Promise<void> {
  try { process.kill(pid, "SIGKILL"); } catch { /* gone */ }
  return until(() => gone(pid), 5_000, `pid ${pid} to exit`);
}
function mkfifo(name: string): string {
  const p = path.join(scratch, name);
  execFileSync("mkfifo", [p]);
  return p;
}
/** Unblocks a `cat <fifo>`: a read-write open counts as a writer, so the reader's open completes and the close ends it. */
function release(fifo: string): void { fs.closeSync(fs.openSync(fifo, "r+")); }

// ─── repo helpers ──────────────────────────────────────────────────────────────────────────

const bare = (): string => git.barePathFor(fx.originPath);
const branchOf = (iid: number): string => `agent/issue-${iid}`;
const canonicalOf = (iid: number): string => git.runnerClonePath(bare(), `issue-${iid}`);
const keyOf = (iid: number): string => cloneKeyOf(canonicalOf(iid)).cloneKey;
const attemptDirs = (iid: number): string[] => fs.readdirSync(path.dirname(canonicalOf(iid))).filter((n) => n.startsWith(`issue-${iid}.attempt-`));

/** A failed predecessor's retained attempt clone: seeded, journaled, then released in place as `abandoned` (non-live). */
async function retainedClone(iid: number): Promise<{ clonePath: string; runId: string; branch: string }> {
  const b = await git.ensureClone(fx.originPath);
  const runId = randomUUID();
  const clone = await git.createOrAttachRunnerClone(b, iid, noProofReseed, runId, false, undefined,
    { attemptId: mintAttemptId(1), isLive: () => false, beforeSeed: async () => {} });
  await git.markRecoveryCapture(b, clone.path, branchOf(iid), runId, clone.attemptId);
  await git.releaseAttemptInPlace(b, clone.path, branchOf(iid), runId, "abandoned");
  return { clonePath: clone.path, runId, branch: branchOf(iid) };
}
/** A planted abandoned sibling attempt dir of the key (with its ledger value), as a verified capture leaves it. */
async function plantSibling(iid: number): Promise<string> {
  const b = await git.ensureClone(fx.originPath);
  const dir = `${canonicalOf(iid)}.attempt-${mintAttemptId(1)}`;
  fs.mkdirSync(dir, { recursive: true });
  fs.writeFileSync(path.join(dir, "WORK.txt"), "sibling");
  execFileSync("git", ["-C", b, "config", "--local", "--add", `uzi-attempts.${branchOf(iid)}.entry`,
    JSON.stringify({ attemptId: path.basename(dir).split(".attempt-")[1], runId: randomUUID(), clonePath: dir, state: "abandoned" })],
  { env: GIT_ENV, stdio: "pipe" });
  return dir;
}

/** The runner's own seed-site sweep: a real seed-mode scan over the key's non-live paths; survivors block the seed. */
async function sweep(nonLive: string[], canonical: string): Promise<void> {
  const out = await quiesceRunAttempt({
    mode: "seed", attempt: undefined, cloneKey: cloneKeyOf(canonical).cloneKey, targetPaths: nonLive, processes: true,
    dockerHost: undefined, registry: live, site: "attempt_seed",
  }, { docker: { intervalMs: 5, deadlineMs: 1_000, requestTimeoutMs: 500 } });
  if (out.process?.state !== "quiescent") throw new RunResidueBlockedError(out.process?.detail ?? "not quiescent");
}
/** The reasons a direct seed-mode sweep over `paths` gives the processes it finds. */
async function classify(canonical: string, paths: string[]): Promise<string[]> {
  const out = await quiesceRunAttempt({
    mode: "seed", attempt: undefined, cloneKey: cloneKeyOf(canonical).cloneKey, targetPaths: paths, processes: true,
    dockerHost: undefined, registry: live, site: "attempt_seed:classify",
  });
  return (out.process?.processes ?? []).map((p) => p.reason);
}

interface SeedProbe { beforeSeedAt?: number; opts: AttemptSeedOptions }
function seedOptions(beforeSeed: (nonLive: string[], canonical: string) => Promise<void> = sweep): SeedProbe {
  const probe: SeedProbe = {
    opts: {
      attemptId: mintAttemptId(2), isLive: (p) => live.isLivePath(p),
      beforeSeed: async (nonLive, canonical) => { probe.beforeSeedAt = Date.now(); await beforeSeed(nonLive, canonical); },
    },
  };
  return probe;
}
const seed = (iid: number, probe: SeedProbe): Promise<{ path: string }> =>
  git.createOrAttachRunnerClone(bare(), iid, noProofReseed, randomUUID(), false, undefined, probe.opts);

// ─── the runner side: the real proof, with scripted git ───────────────────────────────────

interface QuiesceCall { site: string | undefined; mode: string; forceHelper: boolean; observeOnly: boolean; helperDeadline: boolean; at: number }
interface Rig {
  runner: ReturnType<typeof runnerWith>;
  quiesceCalls: QuiesceCall[];
  /** `heldMs` is the section's own measure of how long B was held (the result's `heldMs`); `bodyMs` is the older body-start clock. */
  sections: Array<{ heldMs: number; bodyMs: number; totalMs: number }>;
  statusCalls: string[];
  /** Every result the REAL section returned (kind `ran` rows carry `aborted`/`timedOut`/`entryKept`). */
  results: Array<{ kind: string; aborted?: boolean; timedOut?: boolean; entryKept?: boolean; keptReason?: string; heldMs?: number }>;
}
let rig: Rig;
type BoundaryRunner = {
  withInventorySourceBoundary(
    context: { runId: string; generation: number; barePath: string; reportDiagnostic?: (d: { check: string }) => unknown },
    action: (prove: () => Promise<boolean>) => Promise<void>,
  ): Promise<"passed" | "retained">;
};

/** A real RunRunner (its constructor registers the real observe-only probe on `git`), a recording real quiescer and an
 *  instrumented `withInventorySourceLock` (the real method; optional smaller budget D). */
function makeRig(opts: { deadlineMs?: number; log?: ReturnType<typeof recordingLogger>["logger"]; claim?: boolean } = {}): Rig {
  const quiesceCalls: QuiesceCall[] = [];
  const quiesceRun = (req: QuiesceRunRequest): Promise<QuiesceRunOutcome> => {
    quiesceCalls.push({ site: req.site, mode: req.mode, forceHelper: req.forceHelper === true, observeOnly: req.observeOnly === true, helperDeadline: req.helperDeadline === true, at: Date.now() });
    return quiesceRunAttempt({ ...req, dockerHost: undefined }, { docker: { intervalMs: 5, deadlineMs: 1_000, requestTimeoutMs: 500 } });
  };
  const factory: ExecutorFactory = (runId) => ({ homeDir: path.join(homeDir, runId), executor: { run: async () => { throw new Error("no executor"); } } });
  const runner = runnerWith(factory, fakeGitlab().gitlab, undefined, opts.log ?? nullLogger(), {
    quiesceRun, liveAttempts: live, recoveryRetryMs: 5,
    ...(opts.claim ? { dockerHost: "unix:///nonexistent-4632.sock" } : {}),
  });
  const sections: Rig["sections"] = [];
  const results: Rig["results"] = [];
  const real =GitCache.prototype.withInventorySourceLock.bind(git);
  git.withInventorySourceLock = (async (barePath: string, o: InventorySourceLockOptions, body: (s: InventorySection) => Promise<unknown>) => {
    const startedAt = Date.now();
    let acquiredAt = startedAt;
    let held: number | undefined;
    try {
      const res = await real(barePath, { ...o, ...(opts.deadlineMs === undefined ? {} : { deadlineMs: opts.deadlineMs }) }, (s) => { acquiredAt = Date.now(); return body(s); });
      held = res.heldMs;
      results.push(res as Rig["results"][number]);
      return res;
    } finally {
      sections.push({ heldMs: held ?? Number.NaN, bodyMs: Date.now() - acquiredAt, totalMs: Date.now() - startedAt });
    }
  }) as typeof git.withInventorySourceLock;
  return { runner, quiesceCalls, sections, statusCalls: [], results };
}

/** Script the proof's git: the FIRST status call leaves a detached unmarked `cat <fifo>` in `where` and returns only once that
 *  process has exited (the way a real git's child outlives nothing: the section awaits it). `left` leaves a `sleep` instead and
 *  returns at once (git exited 0, a descendant still alive). */
function scriptStatus(mode: { hold: { fifo: string; started: () => void; pid: (n: number) => void } } | { left: { where: string; pid: (n: number) => void } } | "plain"): void {
  let first = true;
  git.worktreeStatus = (async (clone: string) => {
    rig.statusCalls.push(clone);
    if (mode !== "plain" && first) {
      first = false;
      if ("hold" in mode) {
        const pid = detach(clone, `cat '${mode.hold.fifo}'`);
        mode.hold.pid(pid);
        mode.hold.started();
        await until(() => gone(pid), 60_000, "the held git to exit");
      } else {
        mode.left.pid(detach(mode.left.where, "sleep 300"));
      }
    }
    return [];
  }) as typeof git.worktreeStatus;
}

interface ProofResult { outcome: "passed" | "retained"; proved: boolean | undefined; checks: string[] }
async function runProof(clone: { clonePath: string; runId: string; branch: string }): Promise<ProofResult> {
  const checks: string[] = [];
  let proved: boolean | undefined;
  git.readInventoryCloneHeads = (async () => ({
    kind: "verified", heads: ["a".repeat(40)], clones: [{ clonePath: clone.clonePath, branch: clone.branch, runId: clone.runId }], foreignOwners: [],
  })) as unknown as typeof git.readInventoryCloneHeads;
  const outcome = await (rig.runner as unknown as BoundaryRunner).withInventorySourceBoundary(
    { runId: clone.runId, generation: 1, barePath: bare(), reportDiagnostic: (d) => { checks.push(d.check); return false; } },
    async () => { /* the first prove is the one under test */ });
  // `prove` is invoked by the boundary itself before the action; its boolean is only visible through `outcome`.
  proved = outcome === "passed";
  return { outcome, proved, checks };
}

beforeEach(() => {
  if (SKIP) return;
  // The harness installs a passthrough for the section; these tests exercise the real one.
  delete (git as unknown as Record<string, unknown>).withInventorySourceLock;
  scratch = fs.mkdtempSync(path.join(fs.realpathSync(os.tmpdir()), "uzi-serialize-"));
  pidFile = path.join(scratch, "pids");
  fs.writeFileSync(pidFile, "");
  live = new LiveAttemptRegistry();
  setQuiescenceViewForTests(scopedRealView({ pidFiles: [pidFile] }));
});
afterEach(async () => {
  if (SKIP) return;
  for (const pid of spawned.splice(0)) { try { process.kill(pid, "SIGKILL"); } catch { /* gone */ } }
  restoreHermeticView();
  fs.rmSync(scratch, { recursive: true, force: true });
  // Let a late continuation of an abandoned section finish before the harness removes the repo.
  await tick(20);
});

const log = (label: string, rows: Rig["sections"]): void => {
  console.log(`latency ${label}: ${rows.map((r) => `heldMs=${r.heldMs}ms bodyClock=${r.bodyMs}ms total=${r.totalMs}ms`).join("; ")}`);
};
/** The latency bound, read from the section's own `heldMs` (B held, acquisition to return), never the body-start clock. */
const assertHeldBounded = (rows: Rig["sections"]): void => {
  assert.ok(rows.length > 0, "at least one section ran");
  for (const s of rows) {
    assert.ok(Number.isFinite(s.heldMs), "the section result carries heldMs");
    assert.ok(s.heldMs < LATENCY_CAP_MS, `B held ${s.heldMs} ms (section result heldMs)`);
  }
};

describe("issue #4632: the inventory proof and a same-key attempt seed serialize", { skip: SKIP }, () => {
  it("case 1 (order 1): a seed arriving while the proof's git is held waits for the section, then seeds", async () => {
    const iid = 4601;
    const r = await retainedClone(iid);
    rig = makeRig();
    const fifo = mkfifo("c1.fifo");
    const started = deferred();
    let heldPid = 0;
    scriptStatus({ hold: { fifo, started: started.resolve, pid: (n) => { heldPid = n; } } });
    const proofP = runProof(r);
    await started.promise;
    assert.equal(alive(heldPid), true);
    assert.equal(live.isLivePath(r.clonePath), false);
    assert.ok((await classify(canonicalOf(iid), [r.clonePath])).includes("unattributed_in_scope"),
      "a direct seed-mode sweep sees the held process as unattributed residue in scope");

    const probe = seedOptions();
    const seedP = seed(iid, probe);
    let seedSettled = false;
    seedP.then(() => { seedSettled = true; }, () => { seedSettled = true; });
    await tick(500);
    assert.equal(probe.beforeSeedAt, undefined, "the seed's sweep did not run while the proof's git was held");
    assert.equal(seedSettled, false);
    assert.deepEqual(attemptDirs(iid), [path.basename(r.clonePath)], "nothing was seeded meanwhile");

    const releasedAt = Date.now();
    release(fifo);
    const proof = await proofP;
    const seeded = await seedP;
    assert.equal(proof.outcome, "passed", `the proof passed (checks ${proof.checks.join(",")})`);
    assert.ok(probe.beforeSeedAt! >= releasedAt, "the seed's sweep ran only after the release");
    assert.ok(fs.existsSync(path.join(seeded.path, ".git")), "the seed succeeded");
    assert.equal(alive(heldPid), false);
    log("case 1", rig.sections);
    assertHeldBounded(rig.sections);
    executed++;
  });

  it("case 2 (order 2): a seed holding the bare lock delays the proof, which then passes", async () => {
    const iid = 4602;
    const r = await retainedClone(iid);
    rig = makeRig();
    scriptStatus("plain");
    const gate = deferred();
    const inSweep = deferred();
    const probe = seedOptions(async (nonLive, canonical) => { inSweep.resolve(); await gate.promise; await sweep(nonLive, canonical); });
    const seedP = seed(iid, probe);
    await inSweep.promise;
    const before = rig.quiesceCalls.length;
    const proofP = runProof(r);
    await tick(500);
    assert.equal(rig.quiesceCalls.length, before, "the proof's before-scan did not start");
    assert.equal(rig.statusCalls.length, 0, "the proof's git did not start");
    const releasedAt = Date.now();
    gate.resolve();
    await seedP;
    const proof = await proofP;
    assert.equal(proof.outcome, "passed", `checks ${proof.checks.join(",")}`);
    const proofCalls = rig.quiesceCalls.filter((c) => c.site === "inventory_source" || c.site === "inventory_source:after_runner_git");
    assert.ok(proofCalls.length >= 2 && proofCalls.every((c) => c.at >= releasedAt), "every proof stage ran after the seed released the lock");
    assert.deepEqual([...new Set(proofCalls.map((c) => c.site))].sort(), ["inventory_source", "inventory_source:after_runner_git"], "both the before and the after scan ran");
    assert.ok(proofCalls.every((c) => c.helperDeadline && c.forceHelper), "issue #4632: the inventory before/after quiesce requests opt in to the helper deadline");
    assert.equal(rig.statusCalls.length >= 1, true);
    log("case 2", rig.sections);
    assertHeldBounded(rig.sections);
    executed++;
  });

  it("case 3 (timeout past D): the proof is retained as unsettled, no later stage runs, same-key seeds refuse, the next seed clears it", { timeout: 60_000 }, async () => {
    const iid = 4603;
    const r = await retainedClone(iid);
    rig = makeRig({ deadlineMs: 2_500 });
    const fifo = mkfifo("c3.fifo");
    const started = deferred();
    let heldPid = 0;
    scriptStatus({ hold: { fifo, started: started.resolve, pid: (n) => { heldPid = n; } } });
    const proofP = runProof(r);
    await started.promise;
    const proof = await proofP;
    assert.equal(proof.outcome, "retained");
    assert.deepEqual(proof.checks, ["source_settlement_unproven"]);
    assert.equal(alive(heldPid), true, "the proof's git is still running past D");
    const after = rig.quiesceCalls.filter((c) => c.site === "inventory_source:after_runner_git");
    assert.equal(after.length, 0, "no after-scan ran past the deadline");

    const refused = seed(iid, seedOptions());
    await assert.rejects(refused, (e: unknown) => e instanceof InventorySourceExclusionRefusal && e.reason === "excluded");
    assert.deepEqual(attemptDirs(iid), [path.basename(r.clonePath)], "the refused seed seeded nothing");

    release(fifo);
    await until(() => gone(heldPid), 10_000, "the held git to exit");
    await git.inventoryExclusionSettled(bare(), keyOf(iid));
    await tick(50);
    assert.equal(rig.quiesceCalls.filter((c) => c.site === "inventory_source:after_runner_git").length, 0, "the abandoned section never ran a later stage");
    const seeded = await seed(iid, seedOptions());
    assert.ok(fs.existsSync(path.join(seeded.path, ".git")), "the lazy clear admitted the seed once the process exited");
    assert.ok(rig.quiesceCalls.some((c) => c.site === "inventory_source:clear_probe" && c.observeOnly && c.forceHelper), "the clear ran the observe-only helper probe");
    log("case 3", rig.sections);
    assertHeldBounded(rig.sections);
    executed++;
  });

  it("case 4 (sibling descendant): the proof passes but a detached process in a non-live sibling keeps the exclusion; seeds refuse nonterminally until it exits", async () => {
    const iid = 4604;
    const r = await retainedClone(iid);
    const sibling = await plantSibling(iid);
    rig = makeRig();
    let sleepPid = 0;
    scriptStatus({ left: { where: sibling, pid: (n) => { sleepPid = n; } } });
    const proof = await runProof(r);
    assert.equal(live.isLivePath(sibling), false, "the sibling is not a live attempt");
    assert.equal(alive(sleepPid), true);
    assert.ok((await classify(canonicalOf(iid), [sibling])).includes("unattributed_in_scope"));
    assert.equal(proof.outcome, "passed", "the proof's own scope (the source clone) is quiescent");

    const probe = seedOptions();
    await assert.rejects(seed(iid, probe), (e: unknown) =>
      e instanceof InventorySourceExclusionRefusal && !(e instanceof RunResidueBlockedError) && e.reason === "excluded");
    assert.equal(probe.beforeSeedAt, undefined, "the refusal precedes the sweep");
    assert.equal(alive(sleepPid), true, "nothing was killed");

    await killAndWait(sleepPid);
    const seeded = await seed(iid, seedOptions());
    assert.ok(fs.existsSync(path.join(seeded.path, ".git")), "admitted once the descendant is gone");
    log("case 4", rig.sections);
    assertHeldBounded(rig.sections);
    executed++;
  });

  it("case 5: a live same-key sibling attempt's marked process does not keep the exclusion once our unmarked descendant exits", async () => {
    const iid = 4605;
    const r = await retainedClone(iid);
    const sibling = await plantSibling(iid);
    const liveDir = `${canonicalOf(iid)}.attempt-${mintAttemptId(3)}`;
    fs.mkdirSync(liveDir, { recursive: true });
    const other = newRunAttempt(randomUUID(), 3, liveDir, () => []);
    live.add(other);
    const markedPid = detach(liveDir, "sleep 300", { [RUN_ATTEMPT_ENV]: other.marker, [RUN_CLONE_KEY_ENV]: other.cloneKey });
    rig = makeRig();
    // The proof's own capture scan would refuse before any git while the live sibling's marked process is in scope, so the
    // exclusion is left by a section whose git started and left a descendant in the non-live sibling.
    const sleepPid = detach(sibling, "sleep 300");
    const kept = await git.withInventorySourceLock(bare(), { clonePath: r.clonePath, branch: r.branch, isLive: (p) => live.isLivePath(p) },
      async (s) => { s.markGitStarted(); await s.stage(async () => 1); s.afterScanQuiescent(true); });
    assert.equal(kept.kind === "ran" && kept.entryKept, true, "the exclusion entry was kept");
    await assert.rejects(seed(iid, seedOptions(async () => {})), (e: unknown) => e instanceof InventorySourceExclusionRefusal);
    await killAndWait(sleepPid);
    const seeded = await seed(iid, seedOptions(async () => {}));
    assert.ok(fs.existsSync(path.join(seeded.path, ".git")), "cleared although the live sibling's marked process still runs");
    assert.equal(alive(markedPid), true, "the live attempt's process was never signalled");
    executed++;
  });

  it("case 6 (negative control): an unmarked process in a retained clone with no inventory running still blocks the seed", async () => {
    const iid = 4606;
    const r = await retainedClone(iid);
    rig = makeRig();
    const pid = detach(r.clonePath, "sleep 300");
    await assert.rejects(seed(iid, seedOptions()), (e: unknown) =>
      e instanceof RunResidueBlockedError && /worker_residue_blocked/.test((e as Error).message));
    assert.equal(alive(pid), true);
    assert.deepEqual(attemptDirs(iid), [path.basename(r.clonePath)]);
    executed++;
  });

  it("case 7: a claim whose key is excluded stops for retry with the typed warning and no failed report; a later claim runs", async () => {
    const iid = 4607;
    await git.ensureClone(fx.originPath);
    const sibling = await plantSibling(iid);
    const rec = recordingLogger();
    rig = makeRig({ log: rec.logger, claim: true });
    // A section whose git left a detached descendant in the key's sibling leaves the exclusion standing.
    const sleepPid = detach(sibling, "sleep 300");
    const kept = await git.withInventorySourceLock(bare(), { clonePath: canonicalOf(iid), branch: branchOf(iid), isLive: (p) => live.isLivePath(p) },
      async (s) => { s.markGitStarted(); await s.stage(async () => 1); s.afterScanQuiescent(true); });
    assert.equal(kept.kind === "ran" && kept.entryKept, true);

    let started = 0;
    const factory: ExecutorFactory = (runId) => ({
      homeDir: path.join(homeDir, runId),
      executor: { run: async (_ctx: RunContext): Promise<ExecutorResult> => { started++; throw new Error("stop after the seed"); } },
    });
    const claimRunner = runnerWith(factory, fakeGitlab().gitlab, undefined, rec.logger, {
      quiesceRun: (req) => quiesceRunAttempt({ ...req, dockerHost: undefined }), liveAttempts: live, recoveryRetryMs: 5,
      dockerHost: "unix:///nonexistent-4632.sock",
    });
    const claim = gitlabClaim(iid);
    await claimRunner.execute(claim);
    assert.equal(started, 0, "no model work started");
    assert.equal(api.states.some((s) => s.body.status === "failed"), false, "no failed report");
    assert.equal(api.states.some((s) => s.body.fail_origin === "worker_residue_blocked"), false);
    const warn = rec.lines.find((l) => (l as { msg?: string }).msg === "retained inventory proof still settling on this clone key; stopping claim for retry") as
      { level?: string; cause?: string; reason?: string } | undefined;
    assert.ok(warn, "the typed warning was logged");
    assert.equal(warn.level, "warn");
    assert.equal(warn.cause, "inventory_source_exclusion");
    assert.equal(warn.reason, "excluded");

    await killAndWait(sleepPid);
    await claimRunner.execute(gitlabClaim(iid));
    assert.equal(started, 1, "a retry after the descendant exited seeds and starts");
    executed++;
  });

  // ─── real git: the proof's own `git status` runs a planted fsmonitor hook that leaves a detached unmarked process ───

  /** A `filter.x.clean` driver in the retained clone (`core.fsmonitor` cannot be used: the runner's git env pins it to false, so
   *  the hook never runs; the clean filter is not pinnable and runs UNMARKED). The filter double-forks (setsid + background) an
   *  unmarked FIFO-blocked `cat` whose cwd is the clone (git runs filters at the work-tree root), records its pid in the
   *  quiescence pid file, then blocks itself on the same FIFO and finally passes the file through unchanged. A tracked file
   *  is made stat-dirty (mtime) so the runner's real `worktreeStatus` (runGitAsRunner/execScoped, `status --porcelain` with
   *  GIT_OPTIONAL_LOCKS=0) must re-hash it, i.e. run the filter. Nothing about git is scripted. */
  function plantCleanFilterHook(clonePath: string, fifo: string): void {
    const hook = path.join(scratch, "clean-filter.sh");
    fs.writeFileSync(hook, [
      "#!/bin/sh",
      `setsid sh -c "cat '${fifo}' </dev/null >/dev/null 2>&1 & echo \\$! >> '${pidFile}'" </dev/null >/dev/null 2>&1`,
      `cat '${fifo}' >/dev/null`,
      "cat", "",
    ].join("\n"), { mode: 0o755 });
    const g = (...args: string[]): string => execFileSync("git", ["-C", clonePath, ...args], { env: GIT_ENV, stdio: "pipe" }).toString();
    const tracked = g("ls-files").split("\n").filter(Boolean)[0]!;
    g("config", "filter.x.clean", hook);
    fs.appendFileSync(path.join(clonePath, ".git", "info", "attributes"), `${tracked} filter=x\n`);
    const later = new Date(Date.now() + 60_000);
    fs.utimesSync(path.join(clonePath, tracked), later, later);
  }
  const pidsWritten = (): number[] => fs.readFileSync(pidFile, "utf8").split("\n").filter(Boolean).map(Number);
  /** Unblocks every reader of `fifo` until all of `pids` are gone (a reader that had not reached its open yet is released by a later pass). */
  async function releaseAll(fifo: string, pids: number[]): Promise<void> {
    const end = Date.now() + 10_000;
    while (pids.some(alive)) {
      if (Date.now() > end) throw new Error("timed out releasing the fifo readers");
      release(fifo);
      await tick(50);
    }
  }
  const sites = (site: string): number => rig.quiesceCalls.filter((c) => c.site === site).length;

  it("case 10 (real git, ambient cancellation): an abort mid-git keeps the entry, starts no later stage, and the seed is admitted only after the real processes exit", { timeout: 90_000 }, async () => {
    const iid = 4611;
    const r = await retainedClone(iid);
    rig = makeRig();
    const fifo = mkfifo("c10.fifo");
    plantCleanFilterHook(r.clonePath, fifo);
    const ac = new AbortController();
    let proof: ProofResult | undefined;
    // The abort reaches the section only through the ambient scope (withLock honours the same scopes), not through opts.signal.
    const scoped = MaintenanceBudget.runProducer(async () => { proof = await runProof(r); }, { signal: ac.signal, durationMs: 60_000 }).catch(() => undefined);
    await until(() => pidsWritten().length >= 1, 20_000, "the clean filter to leave its detached child");
    const child = pidsWritten()[0]!;
    spawned.push(child);
    assert.equal(alive(child), true);
    assert.equal(fs.readlinkSync(`/proc/${child}/cwd`), fs.realpathSync(r.clonePath), "the child's cwd is inside the retained clone");
    assert.equal(sites("inventory_source"), 1, "only the before-scan ran so far");
    ac.abort();
    await scoped;
    assert.ok(proof, "the proof returned");
    assert.equal(proof.outcome, "retained");
    // An ambient abort is also seen by withLock itself, which rejects the locked call after fn (post-read interruption), so the
    // section's own `aborted` result is never returned here and the boundary reports a thrown abort: a retained proof either way.
    // (`aborted: true` itself is asserted through opts.signal in case 11.)
    assert.equal(proof.checks.length, 1, `one refusal reported (${proof.checks.join(",")})`);
    assert.ok(["boundary_exception", "source_settlement_unproven"].includes(proof.checks[0]!), `refusal ${proof.checks[0]}`);
    assert.equal(alive(child), true, "the detached process outlived the abort");
    // Let the real git status settle (killed by its own timeout or by the abort): still no later stage may have started.
    await git.inventoryExclusionSettled(bare(), keyOf(iid));
    await tick(100);
    assert.equal(sites("inventory_source:after_runner_git"), 0, "no after-scan started");
    assert.equal(sites("inventory_source:clear_probe"), 0, "no step-7 stage started");
    assert.equal(sites("inventory_source"), 1);

    const attempts = attemptDirs(iid);
    await assert.rejects(seed(iid, seedOptions()), (e: unknown) => e instanceof InventorySourceExclusionRefusal && e.reason === "excluded");
    assert.deepEqual(attemptDirs(iid), attempts, "the refused seed seeded nothing");
    assert.equal(alive(child), true, "nothing signalled the process");

    // (b) release the FIFO, let everything exit: the lazy clear admits the seed.
    await releaseAll(fifo, [child]);
    const seeded = await seed(iid, seedOptions());
    assert.ok(fs.existsSync(path.join(seeded.path, ".git")), "admitted once the real processes exited");
    assert.ok(sites("inventory_source:clear_probe") >= 1, "the seed's clear ran the observe-only probe");
    executed++;
  });

  it("case 11 (real git, caller signal): the same topology cancelled through opts.signal behaves identically", { timeout: 90_000 }, async () => {
    const iid = 4612;
    const r = await retainedClone(iid);
    const ac = new AbortController();
    rig = makeRig();
    const wrapped = git.withInventorySourceLock;
    git.withInventorySourceLock = ((b: string, o: InventorySourceLockOptions, body: Parameters<typeof wrapped>[2]) =>
      wrapped.call(git, b, { ...o, signal: ac.signal }, body)) as typeof git.withInventorySourceLock;
    const fifo = mkfifo("c11.fifo");
    plantCleanFilterHook(r.clonePath, fifo);
    const proofP = runProof(r);
    await until(() => pidsWritten().length >= 1, 20_000, "the clean filter to leave its detached child");
    const child = pidsWritten()[0]!;
    spawned.push(child);
    ac.abort();
    const proof = await proofP;
    assert.equal(proof.outcome, "retained");
    assert.deepEqual(proof.checks, ["source_settlement_unproven"]);
    assert.equal(rig.results.at(-1)?.kind, "ran");
    assert.equal(rig.results.at(-1)?.aborted, true, "the section reports the abort");
    assert.equal(rig.results.at(-1)?.timedOut, false);
    assert.equal(rig.results.at(-1)?.entryKept, true, "the entry is kept");
    assert.equal(alive(child), true, "the detached process outlived the abort");
    await git.inventoryExclusionSettled(bare(), keyOf(iid));
    await tick(100);
    assert.equal(sites("inventory_source:after_runner_git"), 0);
    assert.equal(sites("inventory_source:clear_probe"), 0);
    await assert.rejects(seed(iid, seedOptions()), (e: unknown) => e instanceof InventorySourceExclusionRefusal);
    await releaseAll(fifo, [child]);
    const seeded = await seed(iid, seedOptions());
    assert.ok(fs.existsSync(path.join(seeded.path, ".git")));
    log("case 11", rig.sections);
    assertHeldBounded(rig.sections);
    executed++;
  });

  it("case 12 (A-wait, k=1): a section held until D under a seed that holds A queued on B still lets a third 30 s A acquisition succeed", { timeout: 60_000 }, async () => {
    const iid = 4613;
    await git.ensureClone(fx.originPath);
    const authRoot = path.join(scratch, "authority");
    fs.mkdirSync(authRoot, { recursive: true });
    const lifetime = await acquireWorkerLifetime(authRoot, testLockOptions());
    const A_WAIT_MS = 30_000;
    const D_MS = 12_000; // INVENTORY_SECTION_BUDGET_MS, the real default
    const gate = deferred();
    // A body that never finishes on its own: the section is held for the whole of D and is then ended by it.
    const sectionP = git.withInventorySourceLock(bare(), { clonePath: canonicalOf(iid), branch: branchOf(iid), isLive: () => false },
      async () => { await gate.promise; });
    await tick(300);
    const t0 = Date.now();
    let seedHasA = 0;
    let seedDoneAt = 0;
    // The seed runs inside A (as the runner's inFlightAuthority transaction does) and queues on B behind the section.
    const seedP = withAuthorityTransaction(lifetime, { timeoutMs: A_WAIT_MS }, async () => {
      seedHasA = Date.now() - t0;
      const clone = await seed(iid, seedOptions(async () => {}));
      seedDoneAt = Date.now() - t0;
      return clone;
    });
    await until(() => seedHasA > 0 || Date.now() - t0 > 2_000, 5_000, "the seed to hold A");
    assert.ok(seedHasA > 0, "the seed took A while the section still held B");
    let thirdAt = 0;
    const thirdP = withAuthorityTransaction(lifetime, { timeoutMs: A_WAIT_MS }, async () => { thirdAt = Date.now() - t0; });
    await tick(D_MS - 1_500 - (Date.now() - t0));
    assert.equal(seedDoneAt, 0, "the seed is still queued on B near D");
    assert.equal(thirdAt, 0, "the third A acquisition is still waiting near D");
    const section = await sectionP;
    gate.resolve();
    assert.equal(section.kind === "ran" && section.timedOut, true, "D ended the section");
    const heldMs = section.kind === "ran" ? section.heldMs : Number.NaN;
    assert.ok(heldMs >= D_MS - 500 && heldMs < D_MS + 1_000, `B held ${heldMs} ms, bounded by D (${D_MS} ms)`);
    const seeded = await seedP;
    await thirdP;
    assert.ok(fs.existsSync(path.join(seeded.path, ".git")), "the seed ran after the section released B");
    assert.ok(thirdAt >= seedDoneAt && thirdAt < A_WAIT_MS, `third A acquisition at ${thirdAt} ms, inside its ${A_WAIT_MS} ms wait`);
    console.log(`latency case 12: section heldMs=${heldMs}ms seed holdsA@${seedHasA}ms seedDone@${seedDoneAt}ms thirdA@${thirdAt}ms (A wait ${A_WAIT_MS}ms)`);
    executed++;
  });

  // ─── an unverified probe result of ANY shape keeps the exclusion (owner requirement) ───────────────────────────────

  /** A ProcessQuiescence built exactly as the observe-only scan returns it for a persistent unreadable pid: the real reap loop
   *  over a fake procfs holding one unreadable, unattributed runner-uid process (state `unverified`, one process). No gate
   *  deadline is passed (the production probe serializes one; the EACCES case below pins that shape). */
  async function unverifiedUnreadableOutcome(req: QuiesceRunRequest): Promise<QuiesceRunOutcome> {
    const fakeRoot = makeFakeProcRoot();
    try {
      plantUnreadableUnattributed(fakeRoot, 73);
      setQuiescenceViewForTests({ procRoot: fakeRoot });
      let t = 0;
      const scan: ScanRequest = {
        mode: req.mode, targetUid: process.getuid?.() ?? 0, targetKey: req.cloneKey, targetPaths: req.targetPaths,
        liveMarkers: [], liveRoots: [], workerNonce: workerSpawnNonce(), observeOnly: true,
      };
      const process_ = await reapProcesses(scan, {
        table: procfsTableAt(fakeRoot), selfPid: 1, now: () => t, sleep: async (ms) => { t += ms; }, deadlineMs: 5_000,
        kill: () => { throw new Error("an observe-only scan never signals"); },
      });
      return { process: process_, docker: { state: "not_wired", removed: [], detail: "" } };
    } finally {
      setQuiescenceViewForTests(scopedRealView({ pidFiles: [pidFile] }));
      fs.rmSync(fakeRoot, { recursive: true, force: true });
    }
  }

  it("case 13: an unverified probe result with an unreadable_unattributed process keeps the key excluded; the claim stops for retry nonterminally and a later clear probe admits", { timeout: 60_000 }, async () => {
    const iid = 4614;
    await git.ensureClone(fx.originPath);
    const sibling = await plantSibling(iid);
    const dirsBefore = attemptDirs(iid);
    const rec = recordingLogger();
    // The exclusion is left by a section whose git left a detached descendant in the key's sibling; the descendant then exits,
    // so the only thing that can keep the key excluded is the probe's own verdict.
    const sleepPid = detach(sibling, "sleep 300");
    const lockOpts = { clonePath: canonicalOf(iid), branch: branchOf(iid), isLive: (p: string) => live.isLivePath(p) };
    const kept = await git.withInventorySourceLock(bare(), lockOpts,
      async (s) => { s.markGitStarted(); await s.stage(async () => 1); s.afterScanQuiescent(true); });
    assert.equal(kept.kind === "ran" && kept.entryKept, true, "the exclusion entry was kept");
    await killAndWait(sleepPid);

    let unverifiedProbe = true;
    const probeOutcomes: string[] = [];
    let started = 0;
    const factory: ExecutorFactory = (runId) => ({
      homeDir: path.join(homeDir, runId),
      executor: { run: async (_ctx: RunContext): Promise<ExecutorResult> => { started++; throw new Error("stop after the seed"); } },
    });
    const claimRunner = runnerWith(factory, fakeGitlab().gitlab, undefined, rec.logger, {
      quiesceRun: async (req) => {
        if (req.site === "inventory_source:clear_probe" && unverifiedProbe) {
          const out = await unverifiedUnreadableOutcome(req);
          probeOutcomes.push(`${out.process?.state}:${out.process?.processes.map((p) => p.reason).join(",")}`);
          assert.equal(req.observeOnly, true);
          assert.equal(req.forceHelper, true);
          return out;
        }
        return quiesceRunAttempt({ ...req, dockerHost: undefined });
      },
      liveAttempts: live, recoveryRetryMs: 5, dockerHost: "unix:///nonexistent-4632.sock",
    });
    // The real verdict over exactly that outcome (not an injected "not_clear").
    assert.equal(inventoryClearVerdict(await unverifiedUnreadableOutcome({ mode: "capture", cloneKey: keyOf(iid), targetPaths: [sibling] } as QuiesceRunRequest).then((o) => o.process)), "not_clear");

    await assert.rejects(seed(iid, seedOptions(async () => {})), (e: unknown) =>
      e instanceof InventorySourceExclusionRefusal && !(e instanceof RunResidueBlockedError) && e.reason === "excluded");
    assert.deepEqual(attemptDirs(iid), dirsBefore, "the refused seed seeded nothing");
    assert.ok(probeOutcomes.includes("unverified:unreadable_unattributed"), `the probe saw the unverified/unreadable result: ${JSON.stringify(probeOutcomes)}`);

    // Retry contract: exactly one settlement wait and exactly one seed retry (two admissions, each probed) before the stop.
    const probesBeforeClaim = probeOutcomes.length;
    let settleWaits = 0;
    const realSettled = git.inventoryExclusionSettled.bind(git);
    git.inventoryExclusionSettled = (b: string, k: string) => { settleWaits++; return realSettled(b, k); };
    try {
      await claimRunner.execute(gitlabClaim(iid));
    } finally {
      delete (git as unknown as Record<string, unknown>).inventoryExclusionSettled;
    }
    assert.equal(settleWaits, 1, "exactly one settlement wait before the stop");
    assert.equal(probeOutcomes.length - probesBeforeClaim, 2, "exactly two probed seed admissions (the first and exactly one retry) before the stop");
    assert.equal(started, 0, "no model work started");
    assert.equal(api.states.some((s) => s.body.status === "failed"), false, "no failed report");
    assert.equal(api.states.some((s) => s.body.fail_origin === "worker_residue_blocked"), false);
    const warn = rec.lines.find((l) => (l as { msg?: string }).msg === "retained inventory proof still settling on this clone key; stopping claim for retry") as
      { level?: string; cause?: string; reason?: string } | undefined;
    assert.ok(warn, "the typed warning was logged");
    assert.equal(warn.cause, "inventory_source_exclusion");
    assert.equal(warn.reason, "excluded");

    unverifiedProbe = false; // the same key with the real (clear) observe-only probe
    await claimRunner.execute(gitlabClaim(iid));
    assert.equal(started, 1, "a retry whose probe is clear seeds and starts");
    executed++;
  });

  it("case 14: a before-scan carrying helperDeadline whose helper hit EACCES on an unattributed pid is unverified (unreadable_unattributed), so the proof refuses before any git and keeps custody", async () => {
    const iid = 4615;
    const r = await retainedClone(iid);
    const calls: Array<{ site: string | undefined; deadlineAt: number | undefined; forceHelper: boolean; observeOnly: boolean; helperDeadline: boolean }> = [];
    const eaccesTable = (clone: string): ProcTable => ({
      listPids: () => [2000],
      readStatus: () => `Name:\tproc\nState:\tS (sleeping)\nUid:\t${process.getuid?.() ?? 0}\t${process.getuid?.() ?? 0}\t${process.getuid?.() ?? 0}\t${process.getuid?.() ?? 0}\n`,
      readEnviron: () => { throw Object.assign(new Error("EACCES"), { code: "EACCES" }); },
      readCwd: () => clone,
      readStat: () => `2000 (proc) S 1 2000 2000 ${"0 ".repeat(15)}1000 0 0`,
    });
    const factory: ExecutorFactory = (runId) => ({ homeDir: path.join(homeDir, runId), executor: { run: async () => { throw new Error("no executor"); } } });
    const runner = runnerWith(factory, fakeGitlab().gitlab, undefined, nullLogger(), {
      liveAttempts: live, recoveryRetryMs: 5,
      // The runner's own before-scan request (site inventory_source) run through the real reap loop with the table the helper
      // would have seen; the deadline the runner computed is passed as the helper's reap deadline, as the helper forwards it.
      quiesceRun: async (req) => {
        calls.push({ site: req.site, deadlineAt: req.deadlineAt, forceHelper: req.forceHelper === true, observeOnly: req.observeOnly === true, helperDeadline: req.helperDeadline === true });
        let t = 0;
        const scan: ScanRequest = {
          mode: req.mode, targetUid: process.getuid?.() ?? 0, targetKey: req.cloneKey, targetPaths: req.targetPaths,
          liveMarkers: [], liveRoots: [], workerNonce: workerSpawnNonce(),
          // As the helper receives it: the opted-in request's serialized deadline rides on the scan itself.
          ...(req.deadlineAt === undefined ? {} : { deadlineAt: req.deadlineAt }),
        };
        const killed: number[] = [];
        const proc = await reapProcesses(scan, {
          table: eaccesTable(req.targetPaths[0]!), selfPid: 1, excludeAllDescendants: true, now: () => t, sleep: async (ms) => { t += ms; },
          kill: (pid) => { killed.push(pid); }, ...(req.deadlineAt === undefined ? {} : { deadlineAt: req.deadlineAt }),
        });
        assert.deepEqual(killed, [], "nothing is signalled");
        assert.deepEqual(proc.processes.map((p) => p.reason), ["unreadable_unattributed"],
          "the EACCES pid reaches attribution and, unattributed, stays unverified (not a table failure)");
        assert.equal(inventoryClearVerdict(proc), "not_clear");
        return { process: proc as ProcessQuiescence, docker: { state: "not_wired", removed: [], detail: "" } };
      },
    });
    rig = { runner, quiesceCalls: [], sections: [], statusCalls: [], results: [] };
    scriptStatus("plain");
    const proof = await runProof(r);
    assert.equal(proof.outcome, "retained", "the proof refused and kept custody");
    assert.deepEqual(proof.checks, ["process_not_quiescent_before_git"]);
    assert.deepEqual(rig.statusCalls, [], "no git ran after the unverified before-scan");
    assert.deepEqual(calls.map((c) => c.site), ["inventory_source"], "no after-scan or later stage ran");
    assert.equal(calls[0]!.forceHelper, true);
    assert.equal(calls[0]!.observeOnly, false, "a signalling (non-observe) scan");
    assert.equal(calls[0]!.helperDeadline, true, "issue #4632: the runner's inventory before-scan opts in to the helper deadline");
    assert.equal(typeof calls[0]!.deadlineAt, "number", "the signalling inventory scan carries a deadline into the helper");
    executed++;
  });

  // ─── the REAL probe (forceHelper + observeOnly + deadlineAt, real helper) over a fake procfs holding a non-dumpable peer ───

  /** Leaves a same-key exclusion the way case 13 does (a section whose git left a detached sibling descendant that then exits),
   *  so only the probe's own verdict can clear it. */
  async function leaveKeptExclusion(iid: number): Promise<void> {
    await git.ensureClone(fx.originPath);
    const sibling = await plantSibling(iid);
    const sleepPid = detach(sibling, "sleep 300");
    const kept = await git.withInventorySourceLock(bare(), { clonePath: canonicalOf(iid), branch: branchOf(iid), isLive: (p: string) => live.isLivePath(p) },
      async (s) => { s.markGitStarted(); await s.stage(async () => 1); s.afterScanQuiescent(true); });
    assert.equal(kept.kind === "ran" && kept.entryKept, true, "the exclusion entry was kept");
    await killAndWait(sleepPid);
  }
  const excluded = (iid: number): boolean =>
    (git as unknown as { inventoryExclusion: { excluded(b: string, k: string): boolean } }).inventoryExclusion.excluded(bare(), keyOf(iid));

  /** A runner whose clear probe is the REAL one (forceHelper + observeOnly + deadlineAt, real helper) over a fake proc root holding
   *  a non-dumpable runner-uid peer (the Codex supervisor shape). The fake view is installed only around the probe scan, so the
   *  runner's own worker-wide and seed scans keep the real view (the peer is not visible to them). `recorded` makes the peer the
   *  recorded root (pid and start time) of another live attempt; without it the peer is attributed to nothing. */
  function runnerOverFakePeer(recorded: boolean, rec: ReturnType<typeof recordingLogger>, counters: { started: number }, cleanup: Array<() => void>) {
    const fakeRoot = makeFakeProcRoot();
    cleanup.push(() => fs.rmSync(fakeRoot, { recursive: true, force: true }));
    const PEER = { pid: 91, startTime: 4242 };
    plantFakeProc(fakeRoot, PEER.pid, { uid: process.getuid?.() ?? 0, comm: "codex-supervisor", env: "unreadable", cwd: "/", startTime: PEER.startTime });
    if (recorded) live.add(newRunAttempt(randomUUID(), 1, path.join(scratch, "other-attempt"), () => [PEER]));
    return runnerWith((runId): ReturnType<ExecutorFactory> => ({
      homeDir: path.join(homeDir, runId),
      executor: { run: async (_ctx: RunContext): Promise<ExecutorResult> => { counters.started++; throw new Error("stop after the seed"); } },
    }), fakeGitlab().gitlab, undefined, rec.logger, {
      quiesceRun: async (req) => {
        if (req.site !== "inventory_source:clear_probe") return quiesceRunAttempt({ ...req, dockerHost: undefined });
        assert.equal(req.forceHelper, true);
        assert.equal(req.observeOnly, true);
        assert.equal(typeof req.deadlineAt, "number");
        setQuiescenceViewForTests({ procRoot: fakeRoot });
        try { return await quiesceRunAttempt({ ...req, dockerHost: undefined }); }
        finally { setQuiescenceViewForTests(scopedRealView({ pidFiles: [pidFile] })); }
      },
      liveAttempts: live, recoveryRetryMs: 5, dockerHost: "unix:///nonexistent-4632.sock",
    });
  }

  it("case 15: the real probe over an attributed non-dumpable peer clears a kept exclusion, and the seed proceeds", { timeout: 90_000 }, async (t) => {
    const iid = 4616;
    await leaveKeptExclusion(iid);
    assert.equal(excluded(iid), true);
    const counters = { started: 0 };
    const cleanup: Array<() => void> = [];
    t.after(() => { for (const fn of cleanup) fn(); });
    const claimRunner = runnerOverFakePeer(true, recordingLogger(), counters, cleanup);
    await claimRunner.execute(gitlabClaim(iid));
    assert.equal(counters.started, 1, "the probe was clear (the peer is attributed to another live attempt), so the seed ran and the claim started");
    assert.equal(excluded(iid), false, "the exclusion was cleared");
    executed++;
  });

  it("case 16: the same peer with no recorded root keeps the key excluded; the seed refuses (not a residue block) and the claim stops for retry", { timeout: 90_000 }, async (t) => {
    const iid = 4617;
    await leaveKeptExclusion(iid);
    const dirsBefore = attemptDirs(iid);
    const counters = { started: 0 };
    const rec = recordingLogger();
    const cleanup: Array<() => void> = [];
    t.after(() => { for (const fn of cleanup) fn(); });
    const claimRunner = runnerOverFakePeer(false, rec, counters, cleanup); // registers its probe on the shared git
    const statesBefore = api.states.length;
    await assert.rejects(seed(iid, seedOptions(async () => {})), (e: unknown) =>
      e instanceof InventorySourceExclusionRefusal && !(e instanceof RunResidueBlockedError) && e.reason === "excluded");
    await claimRunner.execute(gitlabClaim(iid));
    assert.deepEqual(attemptDirs(iid), dirsBefore, "nothing was seeded");
    assert.equal(excluded(iid), true, "the exclusion stays");
    assert.equal(counters.started, 0, "no model work started");
    assert.equal(api.states.slice(statesBefore).some((s) => s.body.status === "failed"), false, "no failed report");
    const warn = rec.lines.find((l) => (l as { msg?: string }).msg === "retained inventory proof still settling on this clone key; stopping claim for retry") as
      { cause?: string; reason?: string } | undefined;
    assert.ok(warn, "the typed warning was logged");
    assert.equal(warn.cause, "inventory_source_exclusion");
    assert.equal(warn.reason, "excluded");
    executed++;
  });

  it("case 9a (5f): a restore entry defers the ordinary clear without a probe; a restore-class clear admits and keeps the entry", async () => {
    const iid = 4609;
    await git.ensureClone(fx.originPath);
    const probes: number[] = [];
    let verdict: "clear" | "not_clear" = "not_clear";
    git.setInventoryExclusionProbe(async () => { probes.push(Date.now()); return verdict; });
    const section = await git.withInventorySourceLock(bare(), { clonePath: canonicalOf(iid), branch: branchOf(iid), isLive: () => false },
      async (s) => { s.markGitStarted(); await s.stage(async () => 1); s.afterScanQuiescent(true); });
    assert.equal(section.kind === "ran" && section.entryKept, true);
    const afterSection = probes.length;
    // An opaque value under the restore key: the holder is undecodable but present, which is all the full clear looks at.
    execFileSync("git", ["-C", bare(), "config", "--local", restoreJournalKey(branchOf(iid)), "{}"], { env: GIT_ENV, stdio: "pipe" });
    const priv = git as unknown as {
      clearExclusion(b: string, br: string, c: string, isLive: (p: string) => boolean, cls: "full" | "restore", deadlineAt: number,
        opaque?: ReadonlySet<string>): Promise<"admit" | "deferred_restore" | "not_clear">;
      inventoryExclusion: { excluded(b: string, k: string): boolean };
    };
    assert.equal(await priv.clearExclusion(bare(), branchOf(iid), canonicalOf(iid), () => false, "full", Date.now() + 2_000), "deferred_restore");
    assert.equal(probes.length, afterSection, "the full clear never called the probe");
    verdict = "clear";
    assert.equal(await priv.clearExclusion(bare(), branchOf(iid), canonicalOf(iid), () => false, "restore", Date.now() + 2_000, new Set()), "admit");
    assert.equal(probes.length, afterSection + 1, "the restore-class clear ran one probe");
    assert.equal(priv.inventoryExclusion.excluded(bare(), keyOf(iid)), true, "the restore-class admit removed nothing");
    await assert.rejects(seed(iid, seedOptions(async () => {})), (e: unknown) =>
      e instanceof InventorySourceExclusionRefusal || (e as Error).constructor.name.startsWith("Restore"));
  });

  it("case 9b (5g): a reclaim admission refuses while a descendant keeps the key excluded, then admits and blocks sections meanwhile", async () => {
    const iid = 4610;
    await git.ensureClone(fx.originPath);
    const sibling = await plantSibling(iid);
    rig = makeRig();
    const sleepPid = detach(sibling, "sleep 300");
    const lockOpts = { clonePath: canonicalOf(iid), branch: branchOf(iid), isLive: (p: string) => live.isLivePath(p) };
    const body = async (s: InventorySection): Promise<void> => { s.markGitStarted(); await s.stage(async () => 1); s.afterScanQuiescent(true); };
    assert.equal(((await git.withInventorySourceLock(bare(), lockOpts, body)) as { entryKept?: boolean }).entryKept, true);
    const target = [{ path: sibling, branch: branchOf(iid) }];
    let ran = false;
    await assert.rejects(git.withReclaimAdmission(bare(), target, (p) => live.isLivePath(p), async () => { ran = true; }),
      (e: unknown) => e instanceof InventorySourceExclusionRefusal);
    assert.equal(ran, false);
    await killAndWait(sleepPid);
    let during: Awaited<ReturnType<typeof git.withInventorySourceLock>> | undefined;
    await git.withReclaimAdmission(bare(), target, (p) => live.isLivePath(p), async () => {
      during = await git.withInventorySourceLock(bare(), lockOpts, body);
    });
    assert.ok(during && during.kind === "refused" && during.check === "source_lock_unavailable", "a section refuses while the reclaim admission stands");
    const afterwards = await git.withInventorySourceLock(bare(), lockOpts, body);
    assert.equal(afterwards.kind, "ran", "and runs once the admission ended");
    executed++;
  });
});
