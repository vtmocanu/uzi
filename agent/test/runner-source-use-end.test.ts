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
