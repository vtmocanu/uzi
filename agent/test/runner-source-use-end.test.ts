import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { randomBytes, randomUUID } from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import { acquireWorkerLifetime, AuthorityRefusal } from "../src/authority-lock.js";
import { AuthorityRuntime } from "../src/authority-runtime.js";
import { openAuthorityStore, type SlotSnapshot } from "../src/completion-authority.js";
import { StubExecutor, type Executor, type ExecutorResult, type RunContext } from "../src/executor.js";
import { GitCache } from "../src/git.js";
import { followSuccessfulPush } from "./publication-fixture.js";
import { testLockOptions } from "./authority-test-support.js";
import { defaultGitleaksShim } from "./gitleaks-shim.js";
import { noProofReseed, nullLogger, testGitCacheOptions } from "./helpers.js";
import {
  api, fakeGitlab as rawFakeGitlab, fx, git, gitlabClaim, homeDir, installHarness, runnerWith,
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
    const internals = runner as unknown as { bindFlightSource(flight: { sourceLease?: unknown }, clonePath: string): Promise<void> };
    const bind = internals.bindFlightSource.bind(runner);
    internals.bindFlightSource = async (flight, clonePath) => {
      await bind(flight, clonePath);
      await r.rt.withTransaction(() => r.rt.hooks.admitSourceUse(clonePath, "seed-canonical"));
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
