import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { randomBytes, randomUUID } from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import { acquireWorkerLifetime, AuthorityRefusal, SourceUseHeldRefusal } from "../src/authority-lock.js";
import { AuthorityRuntime } from "../src/authority-runtime.js";
import { openAuthorityStore, type SlotSnapshot } from "../src/completion-authority.js";
import { StubExecutor, type Executor, type ExecutorResult, type RunContext } from "../src/executor.js";
import { ForeignCaptureBlockedError, GitCache } from "../src/git.js";
import { RunRunner } from "../src/runner.js";
import { formatAttemptId } from "../src/attempt-path.js";
import type { RemediationRecovery } from "../src/git.js";
import { portableTeardownTestDeps } from "./teardown-fixtures.js";
import { followSuccessfulPush } from "./publication-fixture.js";
import { testLockOptions } from "./authority-test-support.js";
import { defaultGitleaksShim } from "./gitleaks-shim.js";
import { noProofReseed, nullLogger, testGitCacheOptions } from "./helpers.js";
import {
  api, client, fakeGitlab as rawFakeGitlab, fx, git, gitlabClaim, homeDir, installHarness, runnerWith,
} from "./runner-harness.js";

installHarness();

// issue #3404 M2 U3 — a flight that ends its source use releases the slots it admitted (so a later worker
// incarnation can use the path again), and the executor never starts on a clone whose source-use epoch is
// not the flight's. Driven through the REAL RunRunner.execute() over the real fixture origin and clone
// seeding; the authority runtime is the production one over a real kernel flock.

const TOKEN = "unit-join-" + randomBytes(12).toString("hex");

interface Rig {
  dataDir: string;
  rt: AuthorityRuntime;
  /** A second worker process over the same data root: its own incarnation, its own GitCache with the hooks. */
  successor(): { rt: AuthorityRuntime; cache: GitCache };
}

async function rig(): Promise<Rig> {
  const dataDir = fs.realpathSync(fx.dataDir);
  assert.equal(dataDir, fx.dataDir, "the fixture data root is canonical");
  const lifetime = await acquireWorkerLifetime(dataDir, testLockOptions());
  const rt = new AuthorityRuntime({ lifetime, workerToken: TOKEN, timeoutMs: 5000 });
  return {
    dataDir, rt,
    successor: () => {
      const next = new AuthorityRuntime({ lifetime, workerToken: TOKEN, workerIncarnation: randomUUID(), timeoutMs: 5000 });
      const cache = new GitCache(dataDir, nullLogger(), undefined, testGitCacheOptions({
        authority: next.hooks, authorityRequired: true, gitleaksBin: defaultGitleaksShim(),
      }));
      return { rt: next, cache };
    },
  };
}

async function slotOf(rt: AuthorityRuntime, p: string): Promise<SlotSnapshot | null> {
  return rt.withTransaction(async (tx) => {
    const store = await openAuthorityStore(tx, { workerToken: TOKEN });
    return store.readSlot(tx, await store.slotKey(tx, p));
  });
}

const failed = (runId: string): boolean => api.states.some((s) => s.runId === runId && s.body.status === "failed");

function stopAfterSeed(onRun: () => void = () => {}): (runId: string) => { homeDir: string; executor: Executor } {
  return (runId) => ({
    homeDir: path.join(homeDir, runId),
    executor: {
      run: async (_ctx: RunContext): Promise<ExecutorResult> => { onRun(); throw new Error("stop after the seed"); },
    },
  });
}

/** A runner whose GitCache (carrying the authority hooks, required) is passed through the constructor. */
function runnerOnHookedGit(r: Rig, makeExecutor: (runId: string) => { homeDir: string; executor: Executor }): RunRunner {
  const hooked = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions({
    authority: r.rt.hooks, authorityRequired: true, gitleaksBin: defaultGitleaksShim(),
  }));
  return new RunRunner(client, hooked, makeExecutor, nullLogger(), 20, undefined, {
    pollMs: 5,
    planApprovalTimeoutMs: 0,
    questionTimeoutMs: 600,
    prDescriptionHeadLagMs: 0,
    gitlab: rawFakeGitlab({}).gitlab,
    teardownTestDeps: portableTeardownTestDeps,
    authority: r.rt,
    checkpointTestHooks: { retryBackoffMs: [0, 0] },
  });
}

describe("end of a flight's source use", () => {
  it("a flight that ends normally lets a different worker incarnation seed the same canonical path", async () => {
    const r = await rig();
    const forge = rawFakeGitlab({});
    followSuccessfulPush(git, forge.pr, true);
    const claim = gitlabClaim(340401);
    const runner = runnerWith(() => ({ executor: new StubExecutor(nullLogger()) }), forge.gitlab, undefined, undefined, { authority: r.rt });
    await runner.execute(claim);
    assert.equal(failed(claim.run_id), false, "the run completed");

    const bare = git.barePathFor(fx.originPath);
    const canonical = git.runnerClonePath(bare, "issue-340401");
    const slot = await slotOf(r.rt, canonical);
    assert.equal(slot?.state, "ineligible", "the live owner moved the slot out of active at the end of the flight");

    const next = r.successor();
    const clone = await next.rt.withTransaction(() => next.cache.createOrAttachRunnerClone(bare, 340401, noProofReseed, "run-next"));
    assert.equal(clone.path, canonical, "the successor incarnation seeded the same canonical path");
    assert.equal((await slotOf(next.rt, canonical))?.state, "active");
  });

  it("a flight that fails after the seed also ends its source use", async () => {
    const r = await rig();
    const claim = gitlabClaim(340402);
    const runner = runnerWith(stopAfterSeed(), rawFakeGitlab({}).gitlab, undefined, undefined, { authority: r.rt });
    await runner.execute(claim);
    assert.equal(failed(claim.run_id), true);
    const canonical = git.runnerClonePath(git.barePathFor(fx.originPath), "issue-340402");
    assert.equal((await slotOf(r.rt, canonical))?.state, "ineligible");
  });

  it("a flight whose run processes could not be proven quiescent keeps its slot active (fail closed)", async () => {
    const r = await rig();
    const claim = gitlabClaim(340408);
    const quiesceRun = async (): Promise<never> => ({
      process: { state: "survivors", processes: [], killed: [], detail: "scripted survivor" },
      docker: { state: "docker_unconfirmed", removed: [], detail: "" },
    }) as never;
    const runner = runnerWith(stopAfterSeed(), rawFakeGitlab({}).gitlab, undefined, undefined, { authority: r.rt, quiesceRun });
    await runner.execute(claim);
    assert.equal(failed(claim.run_id), true);
    const canonical = git.runnerClonePath(git.barePathFor(fx.originPath), "issue-340408");
    assert.equal((await slotOf(r.rt, canonical))?.state, "active", "a surviving process may still write the clone");
    const next = r.successor();
    await assert.rejects(
      next.rt.withTransaction(() => next.rt.hooks.admitSourceUse(canonical, "seed-canonical")),
      /active under another worker incarnation/);
  });

  it("a teardown through execute() whose process half was never positively joined keeps the slot active and warns", async () => {
    // evidenceProcessJoined unset with NO evidenceJoinFailed: the quiescence result is real and clean, but the
    // flight's join record is withheld, so "no failure was seen" must not be read as "joined".
    const r = await rig();
    const claim = gitlabClaim(340409);
    const warnings: string[] = [];
    const runner = runnerWith(stopAfterSeed(), rawFakeGitlab({}).gitlab, undefined, undefined, { authority: r.rt });
    const internals = runner as unknown as {
      quiesceRun(flight: { evidenceProcessJoined?: unknown; evidenceJoinFailed?: boolean; runLog: { warn: (m: string) => void } }, ...rest: unknown[]): Promise<unknown>;
    };
    const quiesce = internals.quiesceRun.bind(runner);
    let sawUnset = false;
    internals.quiesceRun = async (flight, ...rest) => {
      const result = await quiesce(flight, ...rest);
      flight.evidenceProcessJoined = undefined;
      sawUnset = flight.evidenceJoinFailed !== true;
      const warn = flight.runLog.warn.bind(flight.runLog);
      flight.runLog.warn = (message: string, ...more: unknown[]) => { warnings.push(message); (warn as (m: string, ...m2: unknown[]) => void)(message, ...more); };
      return result;
    };
    await runner.execute(claim);
    assert.equal(failed(claim.run_id), true);
    assert.equal(sawUnset, true, "the join record was withheld without any recorded failure");
    const canonical = git.runnerClonePath(git.barePathFor(fx.originPath), "issue-340409");
    assert.equal((await slotOf(r.rt, canonical))?.state, "active", "an unset join is not a join");
    assert.match(warnings.join("\n"), /not positively joined/);
  });

  it("a worker that dies mid-flight (no end) leaves the slot active, and a successor incarnation refuses the path", async () => {
    const r = await rig();
    const bare = await git.ensureClone(fx.originPath);
    const first = r.successor();
    await first.rt.withTransaction(() => first.cache.createOrAttachRunnerClone(bare, 340403, noProofReseed, "run-dead"));
    const canonical = git.runnerClonePath(bare, "issue-340403");
    assert.equal((await slotOf(first.rt, canonical))?.state, "active");
    // `first` is never ended: its process "crashed". A later incarnation must not advance the foreign epoch.
    const next = r.successor();
    fs.rmSync(path.join(canonical), { recursive: true, force: true });
    await assert.rejects(
      next.rt.withTransaction(() => next.cache.createOrAttachRunnerClone(bare, 340403, noProofReseed, "run-next")),
      (e: unknown) => e instanceof AuthorityRefusal && /active under another worker incarnation/.test(e.message));
  });

  it("tails that were not positively joined leave the slot active (fail closed) and log", async () => {
    const r = await rig();
    const bare = await git.ensureClone(fx.originPath);
    const canonical = git.runnerClonePath(bare, "issue-340404");
    fs.mkdirSync(path.dirname(canonical), { recursive: true });
    await r.rt.withTransaction(() => r.rt.bindSource(canonical));
    const warnings: string[] = [];
    const log = Object.assign(nullLogger(), { warn: (message: string) => { warnings.push(message); } });
    const runner = runnerWith(stopAfterSeed(), rawFakeGitlab({}).gitlab, undefined, undefined, { authority: r.rt });
    const end = (flight: object) => (runner as unknown as {
      endFlightSourceUse(flight: object, log: unknown): Promise<void>;
    }).endFlightSourceUse(flight, log);

    await end({ worktreePath: canonical, sourceTailsJoined: false });
    assert.equal((await slotOf(r.rt, canonical))?.state, "active", "not joined: nothing ends");
    assert.match(warnings.join("\n"), /not positively joined/);
    await end({ worktreePath: canonical });
    assert.equal((await slotOf(r.rt, canonical))?.state, "active", "an unknown join is not a join");
    await end({ worktreePath: canonical, sourceTailsJoined: true });
    assert.equal((await slotOf(r.rt, canonical))?.state, "ineligible");
  });
});

describe("the executor-launch gate", () => {
  it("never starts the executor on a clone whose source-use epoch is not the flight's", async () => {
    const r = await rig();
    let started = 0;
    const claim = gitlabClaim(340405);
    const runner = runnerWith(stopAfterSeed(() => { started++; }), rawFakeGitlab({}).gitlab, undefined, undefined, { authority: r.rt });
    // The flight binds its lease, then another admission of the same clone replaces the epoch before launch.
    const internals = runner as unknown as {
      bindFlightSource(flight: { sourceLease?: unknown; sourceUsePaths?: Set<string> }, clonePath: string): Promise<void>;
    };
    const bind = internals.bindFlightSource.bind(runner);
    internals.bindFlightSource = async (flight, clonePath) => {
      await bind(flight, clonePath);
      // The replacement is made inside the flight's own sink: another sink (or none) is refused as a foreign owner.
      await r.rt.withTransaction(() => r.rt.recordAdmissions(flight.sourceUsePaths!, () => r.rt.hooks.admitSourceUse(clonePath, "seed-canonical")));
    };
    await runner.execute(claim);
    assert.equal(started, 0, "the executor did not start");
    assert.equal(failed(claim.run_id), true);
    const reason = api.states.find((s) => s.runId === claim.run_id && s.body.status === "failed")?.body.failure_reason;
    assert.match(String(reason), /executor launch refused/);
  });

  it("a flight with no bound lease cannot start the executor either", async () => {
    const r = await rig();
    let started = 0;
    const claim = gitlabClaim(340406);
    const runner = runnerWith(stopAfterSeed(() => { started++; }), rawFakeGitlab({}).gitlab, undefined, undefined, { authority: r.rt });
    (runner as unknown as { bindFlightSource(): Promise<void> }).bindFlightSource = async () => undefined;
    await runner.execute(claim);
    assert.equal(started, 0);
    assert.equal(failed(claim.run_id), true);
  });

  it("without an authority runtime the executor starts as before", async () => {
    await rig();
    let started = 0;
    const claim = gitlabClaim(340407);
    await runnerWith(stopAfterSeed(() => { started++; }), rawFakeGitlab({}).gitlab).execute(claim);
    assert.equal(started, 1);
  });
});

// issue #3638 D2 — a flight's teardown retires its sink and latches, whichever way it returns, and only ever ends
// the keys its own sink admitted. Driven through the runner's own endFlightSourceUse / publishFlightSource with a
// scripted flight record, over the production AuthorityRuntime.
describe("sink retirement and ownership at a flight's teardown", () => {
  type Flight = {
    worktreePath?: string; sourceUsePaths?: Set<string>; sourceTailsJoined?: boolean; sourceAdmissionRetired?: boolean;
    sourceLease?: unknown; completionSourceBound?: unknown;
  };
  const internalsOf = (runner: unknown) => runner as {
    endFlightSourceUse(flight: Flight, log: unknown): Promise<void>;
    publishFlightSource(flight: Flight, bound: unknown, log: unknown): Promise<string>;
    inFlightAuthority<T>(flight: Flight, fn: () => Promise<T>): Promise<T>;
    bindFlightSource(flight: Flight, clonePath: string): Promise<void>;
    admitPredecessorSource(flight: Flight, clonePath: string, wait: object): Promise<void>;
  };
  const isHeld = (e: unknown): boolean => e instanceof SourceUseHeldRefusal;

  async function setup(iid: number) {
    const r = await rig();
    const bare = await git.ensureClone(fx.originPath);
    const canonical = git.runnerClonePath(bare, `issue-${iid}`);
    fs.mkdirSync(path.dirname(canonical), { recursive: true });
    const runner = runnerWith(stopAfterSeed(), rawFakeGitlab({}).gitlab, undefined, undefined, { authority: r.rt });
    return { r, canonical, internals: internalsOf(runner), log: nullLogger() };
  }

  for (const [name, flightOf] of [
    ["not positively joined", (canonical: string, sink: Set<string>): Flight => ({ worktreePath: canonical, sourceUsePaths: sink, sourceTailsJoined: false })],
    ["no path to end", (_canonical: string, sink: Set<string>): Flight => ({ sourceUsePaths: sink })],
    ["joined", (canonical: string, sink: Set<string>): Flight => ({ worktreePath: canonical, sourceUsePaths: sink, sourceTailsJoined: true })],
  ] as const) {
    it(`teardown (${name}) retires the flight's sink and latches: a leftover callback is refused before any effect`, async () => {
      const { r, canonical, internals, log } = await setup(364001);
      const sink = new Set<string>();
      const other = path.join(path.dirname(canonical), "issue-364002");
      fs.mkdirSync(other, { recursive: true });
      if (name !== "no path to end") await r.rt.recordAdmissions(sink, () => r.rt.bindSource(canonical));
      const flight = flightOf(canonical, sink);
      await internals.endFlightSourceUse(flight, log);
      assert.equal(flight.sourceAdmissionRetired, true, "the latch is set on this return path");

      const before = JSON.stringify(await slotOf(r.rt, canonical));
      // The runtime-level callback a late git hook would make inside the flight's own sink.
      await assert.rejects(r.rt.recordAdmissions(sink, () => r.rt.bindSource(other)), isHeld, "a late bind");
      await assert.rejects(r.rt.recordAdmissions(sink, () => r.rt.withTransaction(() => r.rt.hooks.admitSourceUse(other, "seed-canonical"))), isHeld, "a late seed hook");
      assert.equal(await slotOf(r.rt, other), null, "the other path was never admitted");
      assert.equal(JSON.stringify(await slotOf(r.rt, canonical)), before, "the flight's own slot is untouched by the refusals");
    });

    it(`teardown (${name}) latches the flight: the runner's own admission seams refuse before running anything`, async () => {
      const { r, canonical, internals, log } = await setup(364003);
      const sink = new Set<string>();
      const flight = flightOf(canonical, sink);
      await internals.endFlightSourceUse(flight, log);
      let ran = false;
      await assert.rejects(async () => internals.inFlightAuthority(flight, async () => { ran = true; }), isHeld);
      assert.equal(ran, false, "the wrapped operation never ran");
      await assert.rejects(internals.bindFlightSource(flight, canonical), isHeld);
      await assert.rejects(internals.admitPredecessorSource(flight, canonical, {}), isHeld);
      assert.equal(await slotOf(r.rt, canonical), null, "nothing was admitted");
    });
  }

  it("a flight that was refused a path another flight still owns cannot end or publish it", async () => {
    const { r, canonical, internals, log } = await setup(364004);
    const sinkA = new Set<string>();
    const sinkB = new Set<string>();
    const leaseA = await r.rt.recordAdmissions(sinkA, () => r.rt.bindSource(canonical));
    r.rt.retireSink(sinkA);
    const epoch = (await slotOf(r.rt, canonical))?.epoch;
    // B (a retained-recovery flight refused with SourceUseHeldRefusal) still names the path as its worktree and
    // its tails were joined, but its sink never admitted it.
    const warnings: string[] = [];
    const spy = Object.assign(nullLogger(), { warn: (message: string, fields?: Record<string, unknown>) => { warnings.push(message + " " + JSON.stringify(fields ?? {})); } });
    const flightB: Flight = { worktreePath: canonical, sourceUsePaths: sinkB, sourceTailsJoined: true };
    await internals.endFlightSourceUse(flightB, spy);
    const slot = await slotOf(r.rt, canonical);
    assert.equal(slot?.state, "active", "B's teardown did not end A's epoch");
    assert.equal(slot?.epoch, epoch);
    assert.equal(r.rt.heldByOtherSink(canonical, sinkB), true, "A is still the owner");
    assert.equal(r.rt.heldByOtherSink(canonical, sinkA), false);

    // Nor does B publish it, even with A's lease in hand.
    const reasons: string[] = [];
    const logger = Object.assign(nullLogger(), { warn: (_m: string, fields?: { reason?: string }) => { if (fields?.reason) reasons.push(fields.reason); } });
    const outcome = await internals.publishFlightSource({ ...flightB, sourceLease: leaseA }, { clonePath: canonical }, logger);
    assert.equal(outcome, "unpublished");
    assert.deepEqual(reasons, ["another flight owns this source"]);
    assert.equal((await slotOf(r.rt, canonical))?.state, "active");

    // A's own teardown still ends it.
    await internals.endFlightSourceUse({ worktreePath: canonical, sourceUsePaths: sinkA, sourceTailsJoined: true }, log);
    assert.equal((await slotOf(r.rt, canonical))?.state, "ineligible");
  });

  it("a later run of the same issue whose canonical path an earlier unjoined flight still owns stops quietly", async () => {
    const r = await rig();
    const iid = 364005;
    const bare = await git.ensureClone(fx.originPath);
    const canonical = git.runnerClonePath(bare, `issue-${iid}`);
    fs.mkdirSync(path.dirname(canonical), { recursive: true });
    // Flight A of this issue (another run) ended with its tails unjoined: its sink is retired, the key stays owned.
    const sinkA = new Set<string>();
    await r.rt.recordAdmissions(sinkA, () => r.rt.bindSource(canonical));
    r.rt.retireSink(sinkA);
    const epoch = (await slotOf(r.rt, canonical))?.epoch;
    let started = 0;
    const claim = gitlabClaim(iid, { run_id: randomUUID() });
    // The production seed admission: this runner's GitCache carries the authority hooks and requires them, so the
    // refusal is raised by the seed's own admission of the canonical path (not scripted around it).
    const runner = runnerOnHookedGit(r, stopAfterSeed(() => { started++; }));
    let tornDown: { preserveRecoveryClone?: boolean; preserveSession?: boolean; retainedLocalCustody?: boolean; keepGuardedInventoryOpen?: boolean } | undefined;
    const internals = runner as unknown as { endFlightSourceUse(flight: object, log: unknown): Promise<void> };
    const endFlight = internals.endFlightSourceUse.bind(runner);
    internals.endFlightSourceUse = async (flight, log) => { tornDown = flight; return endFlight(flight, log); };
    assert.equal(fs.existsSync(canonical), false, "the canonical clone does not exist before the run");
    await runner.execute(claim);
    assert.equal(fs.existsSync(canonical), false, "the refused seed created no canonical clone");
    assert.equal(tornDown?.preserveRecoveryClone, true, "custody: the recovery clone is preserved");
    assert.equal(tornDown?.preserveSession, true, "custody: the session is preserved");
    assert.equal(tornDown?.retainedLocalCustody, true, "custody: local custody is retained");
    assert.equal(tornDown?.keepGuardedInventoryOpen, true, "custody: the guarded inventory stays open");
    assert.equal(started, 0, "no model work");
    assert.equal(api.states.some((s) => s.runId === claim.run_id && s.body.status === "failed"), false, "no failed report");
    assert.equal(api.states.some((s) => s.runId === claim.run_id && (s.body.status === "completed")), false);
    const slot = await slotOf(r.rt, canonical);
    assert.equal(slot?.state, "active", "A's epoch is untouched");
    assert.equal(slot?.epoch, epoch);
    assert.equal(r.rt.heldByOtherSink(canonical, new Set()), true, "A still owns the key");
  });

  it("a canonical re-seed refused after a reclaimed terminal orphan stops quietly and leaves the earlier flight's slot owned", async (t) => {
    // Stubbed: runnerCloneForClaim throws ForeignCaptureBlockedError on its FIRST call only (the real one runs on
    // the second, over the hooked GitCache, and is refused by the seed admission); client.getRunOrphanClassification
    // reports a terminal same-repo issue owner; GitCache.classifyOwnerClonePath / terminalRetainedSnapshot /
    // retireRunnerClone are stubbed to a canonical-shaped, non-retained, successfully retired orphan. The real
    // reclaimTerminalOrphan and reseedAfterReclaim run.
    const r = await rig();
    const iid = 364006;
    const bare = await git.ensureClone(fx.originPath);
    const canonical = git.runnerClonePath(bare, `issue-${iid}`);
    fs.mkdirSync(path.dirname(canonical), { recursive: true });
    const sinkA = new Set<string>();
    await r.rt.recordAdmissions(sinkA, () => r.rt.bindSource(canonical));
    r.rt.retireSink(sinkA);
    const epoch = (await slotOf(r.rt, canonical))?.epoch;
    const claim = gitlabClaim(iid, { run_id: randomUUID() });
    const ownerRunId = randomUUID();
    const branch = `agent/issue-${iid}`;
    let started = 0;
    const runner = runnerOnHookedGit(r, stopAfterSeed(() => { started++; }));
    const hooked = (runner as unknown as { git: GitCache }).git;
    t.mock.method(client, "getRunOrphanClassification", async () => ({
      status: "completed", repo_id: claim.repo.id, kind: "issue", issue_iid: iid, branch,
    }));
    t.mock.method(hooked, "classifyOwnerClonePath", async () => "canonical");
    t.mock.method(hooked, "terminalRetainedSnapshot", async () => undefined);
    t.mock.method(hooked, "retireRunnerClone", async () => "quarantined");
    const internals = runner as unknown as { runnerCloneForClaim(...args: unknown[]): Promise<unknown> };
    const real = internals.runnerCloneForClaim.bind(runner);
    let calls = 0;
    internals.runnerCloneForClaim = async (...args) => {
      if (++calls === 1) throw new ForeignCaptureBlockedError(canonical, branch, ownerRunId);
      return real(...args);
    };
    let tornDown: { preserveRecoveryClone?: boolean; preserveSession?: boolean; retainedLocalCustody?: boolean; keepGuardedInventoryOpen?: boolean } | undefined;
    const sealed = runner as unknown as { endFlightSourceUse(flight: object, log: unknown): Promise<void> };
    const endFlight = sealed.endFlightSourceUse.bind(runner);
    sealed.endFlightSourceUse = async (flight, log) => { tornDown = flight; return endFlight(flight, log); };
    await runner.execute(claim);
    assert.equal(calls, 2, "the re-seed after the reclaim was attempted");
    assert.equal(started, 0, "no model work");
    assert.equal(failed(claim.run_id), false, "no failed report");
    assert.equal(api.states.some((s) => s.runId === claim.run_id && s.body.status === "completed"), false);
    assert.equal(tornDown?.preserveRecoveryClone, true);
    assert.equal(tornDown?.preserveSession, true);
    assert.equal(tornDown?.retainedLocalCustody, true);
    assert.equal(tornDown?.keepGuardedInventoryOpen, true);
    const slot = await slotOf(r.rt, canonical);
    assert.equal(slot?.state, "active", "the earlier flight's epoch is untouched");
    assert.equal(slot?.epoch, epoch);
    assert.equal(r.rt.heldByOtherSink(canonical, new Set()), true, "the earlier flight still owns the key");
  });
});


// Git durability is stubbed only at the confirmed GitCache seam here. AuthorityRuntime, A/flock,
// sink ownership, the persistent store and the successor incarnation are real; this is not full E2E.
describe("pending remediation source end foundation", () => {
  const q = "a".repeat(40);
  const newerQ = "b".repeat(40);
  const attemptId = formatAttemptId(new Date("2026-01-01T00:00:00Z"), 1, "0123456789abcdef");
  type Pending = {
    runId: string; clonePath?: string; attemptId?: string; barePath?: string; branch?: string;
    requiredOIDs: string[]; requireDenial: boolean; bindingUncertain: boolean;
  };
  type Flight = Record<string, unknown> & {
    sourceUsePaths: Set<string>; sourceAdmissionRetired?: boolean;
    remediationRestrictions?: { roots: string[]; pending?: Pending };
  };
  type Subject = {
    persistRemediationScan(flight: Flight, bare: string, branch: string, tip: string,
      findings: Array<{ commit: string }>, origin?: "pre-exit"): Promise<void>;
    endFlightSourceUse(flight: Flight, log: unknown): Promise<{ endedSlots: unknown[]; published: boolean }>;
    publishFlightSource(...args: unknown[]): Promise<string>;
  };
  async function setup() {
    const r = await rig();
    const bare = await git.ensureClone(fx.originPath);
    const canonical = git.runnerClonePath(bare, "issue-374701");
    fs.mkdirSync(path.dirname(canonical), { recursive: true });
    const sink = new Set<string>();
    const lease = await r.rt.recordAdmissions(sink, () => r.rt.bindSource(canonical));
    const runner = runnerWith(stopAfterSeed(), rawFakeGitlab({}).gitlab, undefined, undefined, { authority: r.rt });
    const flight: Flight = {
      runId: "run-pending", barePath: bare, branch: "agent/issue-374701",
      worktreePath: canonical, runnerClone: { path: canonical, attemptId },
      sourceUsePaths: sink, sourceLease: lease, sourceTailsJoined: true,
      completionSourceBound: { clonePath: canonical },
      cancel: new AbortController(), steering: { lifecycleSignal: () => new AbortController().signal },
    };
    const record: RemediationRecovery = {
      version: 1, blocked: false, runId: "run-pending", clonePath: canonical, attemptId,
      branch: "agent/issue-374701", sequence: 1, ownedTip: newerQ, flaggedOIDs: [q, newerQ], excludedRoots: [], edges: [],
    };
    return { r, bare, canonical, flight, record, subject: runner as unknown as Subject };
  }


  for (const phase of ["read", "fallback", "combined"] as const) {
    it("pending lifecycle expiry keeps actual source epoch active: " + phase, async t => {
      const { r, bare, canonical, flight, record, subject } = await setup();
      const before = await slotOf(r.rt, canonical);
      t.mock.method(git, "recordRemediationScan", async () => { throw new Error("producer sync failed"); });
      await assert.rejects(subject.persistRemediationScan(flight, bare, record.branch, newerQ, [{ commit: newerQ }], "pre-exit"));
      (flight.cancel as AbortController).abort(); // Authorized cleanup must ignore flight cancellation.
      let wall = Date.now(), deadline = 0, reads = 0, writes = 0;
      t.mock.method(Date, "now", () => wall);
      const scope = () => (git as unknown as {
        recoveryOperations: { getStore(): { deadline: number; signal: AbortSignal } };
      }).recoveryOperations.getStore();
      // These bodies are fixture proof responses, not actual RR config fsync grants.
      t.mock.method(git, "readRuntimeRemediationRecovery", async () => {
        assert.ok(r.rt.active());
        assert.equal(scope().signal.aborted, false, "cancelled flight has a healthy cleanup allowance");
        deadline = scope().deadline;
        reads++;
        if (phase === "read") { wall = deadline; return record; }
        wall = phase === "combined" ? deadline - 1 : wall;
        return { ...record, flaggedOIDs: [q] };
      });
      t.mock.method(git, "blockRemediationRecovery", async () => {
        assert.ok(r.rt.active());
        assert.equal(scope().deadline, deadline, "fallback consumes the original read allowance");
        writes++;
        wall = phase === "combined" ? wall + 1 : deadline;
        return "confirmed";
      });
      const publish = t.mock.method(subject, "publishFlightSource", async () => { throw new Error("must not publish"); });
      const end = t.mock.method(r.rt, "endSourceUse");
      const result = await subject.endFlightSourceUse(flight, nullLogger());
      assert.equal(reads, 1); assert.equal(writes, phase === "read" ? 0 : 1);
      assert.deepEqual(result.endedSlots, []); assert.equal(result.published, false);
      assert.equal(publish.mock.callCount(), 0); assert.equal(end.mock.callCount(), 0);
      assert.equal(flight.sourceAdmissionRetired, true);
      await r.rt.withTransaction(async () => undefined);
      const after = await slotOf(r.rt, canonical);
      assert.equal(after?.state, "active"); assert.equal(after?.epoch, before?.epoch);
    });
  }

  for (const proof of ["read failure", "old body missing Q", "absent body"] as const) {
    it(`pending foundation: ${proof} plus failed denial keeps the same owned epoch with no queued end`, async t => {
      const { r, bare, canonical, flight, record, subject } = await setup();
      const before = await slotOf(r.rt, canonical);
      t.mock.method(git, "recordRemediationScan", async () => { throw new Error("producer sync failed"); });
      await assert.rejects(subject.persistRemediationScan(flight, bare, record.branch, newerQ,
        [{ commit: q }, { commit: newerQ }], "pre-exit"), /producer sync failed/);
      let confirmedUnderA = false;
      t.mock.method(git, "readRuntimeRemediationRecovery", async (_bare: string, _branch: string, source: { runId: string; clonePath: string; attemptId?: string }) => {
        assert.ok(r.rt.active(), "owning A is held");
        assert.equal(flight.sourceAdmissionRetired, undefined);
        assert.deepEqual(source, { runId: record.runId, clonePath: canonical, attemptId });
        assert.equal(r.rt.heldByOtherSink(canonical, flight.sourceUsePaths), false);
        confirmedUnderA = true;
        if (proof === "read failure") throw new Error("consumer sync failed");
        return proof === "absent body" ? undefined : { ...record, flaggedOIDs: [q] };
      });
      const denial = t.mock.method(git, "blockRemediationRecovery", async () => { throw new Error("denial config failed"); });
      const publish = t.mock.method(subject, "publishFlightSource", async () => { throw new Error("must not publish"); });
      const end = t.mock.method(r.rt, "endSourceUse");
      const result = await subject.endFlightSourceUse(flight, nullLogger());
      assert.equal(confirmedUnderA, true);
      assert.equal(denial.mock.callCount(), 1);
      assert.deepEqual(result.endedSlots, []);
      assert.equal(result.published, false);
      assert.equal(publish.mock.callCount(), 0);
      assert.equal(end.mock.callCount(), 0, "no call that could queue a failed end");
      assert.equal(flight.sourceAdmissionRetired, true);
      const pending = structuredClone(flight.remediationRestrictions);
      await assert.rejects(subject.persistRemediationScan(flight, bare, record.branch, newerQ, [{ commit: q }], "pre-exit"),
        e => e instanceof SourceUseHeldRefusal);
      assert.deepEqual(flight.remediationRestrictions, pending, "late callbacks cannot alter pending knowledge");
      await assert.rejects(r.rt.recordAdmissions(flight.sourceUsePaths,
        () => r.rt.withTransaction(() => r.rt.hooks.admitSourceUse(canonical, "capture"))),
        e => e instanceof SourceUseHeldRefusal);
      // Several later real transactions cannot drain an end that was never queued.
      await r.rt.withTransaction(async () => undefined);
      const after = await slotOf(r.rt, canonical);
      assert.equal(after?.state, "active");
      assert.equal(after?.epoch, before?.epoch);
      assert.equal(r.rt.ownedSourceEpoch(canonical, flight.sourceUsePaths), before?.epoch);
      await assert.rejects(r.rt.recordAdmissions(new Set(),
        () => r.rt.withTransaction(() => r.rt.hooks.admitSourceUse(canonical, "capture"))),
        e => e instanceof SourceUseHeldRefusal);
      const next = r.successor();
      await assert.rejects(next.rt.withTransaction(() => next.rt.hooks.admitSourceUse(canonical, "capture")),
        /active under another worker incarnation/);
    });
  }

  it("pending foundation: confirmed permanent denial ends ineligible without publication", async t => {
    const { r, bare, canonical, flight, record, subject } = await setup();
    t.mock.method(git, "recordRemediationScan", async () => { throw new Error("producer failed"); });
    await assert.rejects(subject.persistRemediationScan(flight, bare, record.branch, newerQ, [{ commit: newerQ }], "pre-exit"));
    (flight.cancel as AbortController).abort();
    t.mock.method(git, "readRuntimeRemediationRecovery", async () => ({ ...record, flaggedOIDs: [q] }));
    t.mock.method(git, "blockRemediationRecovery", async () => {
      assert.ok(r.rt.active());
      assert.equal(flight.sourceAdmissionRetired, undefined);
      assert.equal(r.rt.heldByOtherSink(canonical, flight.sourceUsePaths), false);
      return "confirmed";
    });
    const publish = t.mock.method(subject, "publishFlightSource", async () => { throw new Error("must not publish"); });
    const result = await subject.endFlightSourceUse(flight, nullLogger());
    assert.equal(result.endedSlots.length, 1);
    assert.equal(result.published, false);
    assert.equal(publish.mock.callCount(), 0);
    assert.equal((await slotOf(r.rt, canonical))?.state, "ineligible");
  });

  it("pending foundation: independently complete NEW body after producer sync failure permits normal end", async t => {
    const { r, bare, canonical, flight, record, subject } = await setup();
    t.mock.method(git, "recordRemediationScan", async () => { throw new Error("producer sync failed"); });
    await assert.rejects(subject.persistRemediationScan(flight, bare, record.branch, newerQ, [{ commit: newerQ }], "pre-exit"));
    (flight.cancel as AbortController).abort();
    t.mock.method(git, "readRuntimeRemediationRecovery", async () => {
      assert.ok(r.rt.active());
      assert.equal(flight.sourceAdmissionRetired, undefined);
      return record;
    });
    const denial = t.mock.method(git, "blockRemediationRecovery", async () => { throw new Error("unneeded denial"); });
    const publish = t.mock.method(subject, "publishFlightSource", async () => "unpublished");
    const result = await subject.endFlightSourceUse(flight, nullLogger());
    assert.equal(denial.mock.callCount(), 0);
    assert.equal(publish.mock.callCount(), 1, "normal publication path remains reachable");
    assert.equal(result.endedSlots.length, 1);
    assert.equal((await slotOf(r.rt, canonical))?.state, "ineligible");
    assert.deepEqual(flight.remediationRestrictions?.pending?.requiredOIDs, [newerQ], "producer success never clears pending knowledge");
  });

  it("pending foundation: unavailable A retires the sink and retains the epoch without end", async t => {
    const { r, bare, canonical, flight, record, subject } = await setup();
    t.mock.method(git, "recordRemediationScan", async () => { throw new Error("producer failed"); });
    await assert.rejects(subject.persistRemediationScan(flight, bare, record.branch, newerQ, [{ commit: q }], "pre-exit"));
    const transaction = t.mock.method(r.rt, "withTransaction", async () => { throw new Error("A unavailable"); });
    const end = t.mock.method(r.rt, "endSourceUse");
    const result = await subject.endFlightSourceUse(flight, nullLogger());
    assert.equal(result.endedSlots.length, 0);
    assert.equal(end.mock.callCount(), 0);
    assert.equal(flight.sourceAdmissionRetired, true);
    transaction.mock.restore();
    assert.equal((await slotOf(r.rt, canonical))?.state, "active");
    await assert.rejects(r.rt.recordAdmissions(flight.sourceUsePaths,
      () => r.rt.withTransaction(() => r.rt.hooks.admitSourceUse(canonical, "capture"))),
      e => e instanceof SourceUseHeldRefusal);
  });

  for (const invalid of [false, true]) {
    it(`pending foundation: ${invalid ? "invalid OID" : "Q65"} requires denial and cannot borrow a complete positive body`, async t => {
      const { r, bare, canonical, flight, record, subject } = await setup();
      const findings = invalid ? [{ commit: "invalid" }] :
        Array.from({ length: 65 }, (_, i) => ({ commit: (i + 1).toString(16).padStart(40, "0") }));
      t.mock.method(git, "recordRemediationScan", async () => { throw new Error("producer failed"); });
      await assert.rejects(subject.persistRemediationScan(flight, bare, record.branch, newerQ, findings, "pre-exit"));
      const read = t.mock.method(git, "readRuntimeRemediationRecovery", async () => record);
      t.mock.method(git, "blockRemediationRecovery", async () => { throw new Error("denial failed"); });
      const end = t.mock.method(r.rt, "endSourceUse");
      await subject.endFlightSourceUse(flight, nullLogger());
      assert.equal(read.mock.callCount(), 0);
      assert.equal(end.mock.callCount(), 0);
      assert.equal((await slotOf(r.rt, canonical))?.state, "active");
    });
  }

  it("pending foundation: producer success still requires independent consumer confirmation", async t => {
    const { r, bare, canonical, flight, record, subject } = await setup();
    t.mock.method(git, "recordRemediationScan", async () => {});
    await subject.persistRemediationScan(flight, bare, record.branch, newerQ, [{ commit: newerQ }], "pre-exit");
    const read = t.mock.method(git, "readRuntimeRemediationRecovery", async () => { throw new Error("later read failed"); });
    t.mock.method(git, "blockRemediationRecovery", async () => { throw new Error("denial failed"); });
    await subject.endFlightSourceUse(flight, nullLogger());
    assert.equal(read.mock.callCount(), 1);
    assert.equal((await slotOf(r.rt, canonical))?.state, "active");
  });

  it("pending foundation: changed knowledge while confirming keeps custody before retirement", async t => {
    const { r, bare, canonical, flight, record, subject } = await setup();
    t.mock.method(git, "recordRemediationScan", async () => {});
    await subject.persistRemediationScan(flight, bare, record.branch, newerQ, [{ commit: q }], "pre-exit");
    t.mock.method(git, "readRuntimeRemediationRecovery", async () => {
      await subject.persistRemediationScan(flight, bare, record.branch, newerQ, [{ commit: newerQ }], "pre-exit");
      return record;
    });
    const end = t.mock.method(r.rt, "endSourceUse");
    await subject.endFlightSourceUse(flight, nullLogger());
    assert.equal(end.mock.callCount(), 0);
    assert.equal(flight.sourceAdmissionRetired, true);
    assert.equal((await slotOf(r.rt, canonical))?.state, "active");
  });

  // Partial Git seam only: real authority epochs and retirement, stubbed Git evidence.
  for (const producer of ["positive", "negative"] as const) {
    it(`source finding unit: predecessor ${producer} uses independent selected attempt through source end`, async t => {
      const { r, bare, canonical, flight, record, subject } = await setup();
      flight.predecessorCapture = true;
      flight.remediationPredecessorAttempt = attemptId;
      flight.runnerClone = { path: canonical + "-different", attemptId: "not-the-predecessor" };
      const writer = t.mock.method(git,
        producer === "positive" ? "recordRemediationScan" : "recordRemediationRestrictions",
        async (_bare: string, _branch: string, source: { runId: string; clonePath: string; attemptId: string }) => {
          assert.ok(r.rt.active());
          assert.deepEqual(source, { runId: record.runId, clonePath: canonical, attemptId });
        });
      const negative = subject as unknown as {
        persistRemediationRestrictions(flight: Flight, bare: string, branch: string, tip: string,
          findings: Array<{ commit: string }>): Promise<void>;
      };
      if (producer === "positive")
        await subject.persistRemediationScan(flight, bare, record.branch, newerQ, [{ commit: newerQ }], "pre-exit");
      else
        await negative.persistRemediationRestrictions(flight, bare, record.branch, newerQ, [{ commit: newerQ }]);
      assert.equal(writer.mock.callCount(), 1);
      assert.equal(flight.remediationRestrictions?.pending?.attemptId, attemptId);
      t.mock.method(git, "readRuntimeRemediationRecovery", async (_bare: string, _branch: string,
        source: { runId: string; clonePath: string; attemptId?: string }) => {
        assert.ok(r.rt.active());
        assert.deepEqual(source, { runId: record.runId, clonePath: canonical, attemptId });
        return record;
      });
      const denial = t.mock.method(git, "blockRemediationRecovery", async () => { throw new Error("unneeded denial"); });
      t.mock.method(subject, "publishFlightSource", async () => "unpublished");
      const result = await subject.endFlightSourceUse(flight, nullLogger());
      assert.equal(denial.mock.callCount(), 0);
      assert.equal(result.endedSlots.length, 1);
      assert.equal((await slotOf(r.rt, canonical))?.state, "ineligible");
    });
  }

  it("pending foundation: mismatched selected source cannot confirm or deny another source", async t => {
    const { r, bare, canonical, flight, record, subject } = await setup();
    t.mock.method(git, "recordRemediationScan", async () => {});
    await subject.persistRemediationScan(flight, bare, record.branch, newerQ, [{ commit: q }], "pre-exit");
    flight.runnerClone = { path: canonical, attemptId: "another-attempt" };
    const read = t.mock.method(git, "readRuntimeRemediationRecovery", async () => record);
    const denial = t.mock.method(git, "blockRemediationRecovery", async () => "confirmed");
    await subject.endFlightSourceUse(flight, nullLogger());
    assert.equal(read.mock.callCount(), 0);
    assert.equal(denial.mock.callCount(), 0);
    assert.equal((await slotOf(r.rt, canonical))?.state, "active");
  });
});
