import { afterEach, beforeEach, test } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import {
  GitCache, ScratchPublicationError, type FetchAgentBranchOptions,
  type CheckpointPublicationContext, type PushPublicationContext, type RemediationRestrictionIdentity,
} from "../src/git.js";
import { formatAttemptId } from "../src/attempt-path.js";
import { AuthorityRuntime } from "../src/authority-runtime.js";
import { acquireWorkerLifetime } from "../src/authority-lock.js";
import { testLockOptions } from "./authority-test-support.js";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { nullLogger, noProofReseed, testGitCacheOptions } from "./helpers.js";

const BRANCH = "agent/issue-3747", RUN = "run-3747", KEY = "issue-3747";
const ATTEMPT = formatAttemptId(new Date("2026-01-01T00:00:00Z"), 1, "0123456789abcdef");
const OTHER = formatAttemptId(new Date("2026-01-01T00:00:00Z"), 1, "fedcba9876543210");
const ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" };
const IDENT = ["-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"];
const ACTIVE = "uzi-recovery." + BRANCH + ".clone";
let fx: Fixture, cache: GitCache, bare: string, clone: string, base: string, opts: FetchAgentBranchOptions;
let q: string, d: string, s: string;
const source = () => ({ runId: RUN, clonePath: clone, attemptId: ATTEMPT });
const identity = () => ({ exactRunId: RUN, workerSelectedSource: source() });
function git(dir: string, ...args: string[]): string {
  return execFileSync("git", ["-C", dir, ...args], { env: ENV, encoding: "utf8", stdio: ["pipe", "pipe", "pipe"] }).trim();
}
const journal = () => JSON.parse(git(bare, "config", "--get", ACTIVE));
const write = (value: unknown) => git(bare, "config", ACTIVE, JSON.stringify(value));
const query = () => cache.readRemediationRestrictionFacts(bare, BRANCH, RUN, source());
function block() {
  const j = journal(); j.remediationRecovery.blocked = true; write(j);
}
function commit(label: string, parent = base): string {
  const sha = git(clone, ...IDENT, "commit-tree", base + "^{tree}", "-p", parent, "-m", label);
  git(clone, "update-ref", "refs/heads/" + BRANCH, sha);
  return sha;
}
async function fetch() {
  assert.equal((await cache.fetchAgentBranch(bare, clone, BRANCH, RUN, opts)).kind, "updated");
}
function snapshot() {
  return { config: fs.readFileSync(path.join(bare, "config"), "utf8"),
    refs: git(bare, "for-each-ref", "--format=%(refname) %(objectname)"),
    head: git(clone, "rev-parse", "HEAD") };
}
function pack(guard?: (sha: string, context?: CheckpointPublicationContext) => Promise<void>,
  selected: RemediationRestrictionIdentity | undefined = identity(), onStep?: (step: string) => void) {
  return cache.checkpointPack(bare, BRANCH, undefined,
    { tipSha: s, excludeSha: base, defaultBlobSha: base }, onStep, s, guard, undefined,
    { restrictionIdentity: selected });
}
function push(guard?: (sha: string, context: PushPublicationContext) => Promise<void>,
  selected: RemediationRestrictionIdentity | undefined = identity()) {
  return cache.pushBranch(bare, BRANCH, "", fx.originPath, undefined,
    { floor: { kind: "absent" }, originalHead: s, candidate: s }, guard, selected);
}
async function drain(producer: { pack: NodeJS.ReadableStream; exited: Promise<number> }) {
  for await (const chunk of producer.pack) assert.ok(Buffer.isBuffer(chunk));
  assert.equal(await producer.exited, 0);
}
// Object preparation has four owned children; only pack-objects publishes bytes.
function producerCount(spy: { mock: { calls: Array<{ arguments: unknown[] }> } }): number {
  return spy.mock.calls.filter(call =>
    Array.isArray(call.arguments[1]) && call.arguments[1][0] === "pack-objects").length;
}
function refused(error: unknown): boolean {
  assert.ok(error instanceof ScratchPublicationError);
  assert.equal(error.message, "scratch_publication_refused: remediation restriction admission unavailable");
  assert.equal(error.cause, undefined);
  assert.equal(error.detail, undefined);
  assert.equal(error.rawDetail, undefined);
  return true;
}
function checkFacts(context: CheckpointPublicationContext | PushPublicationContext | undefined) {
  assert.ok(context && Object.isFrozen(context));
  const facts = context.restrictionFacts;
  assert.ok(facts && Object.isFrozen(facts));
  assert.ok(Object.isFrozen(facts.roots) && Object.isFrozen(facts.flaggedOIDs));
  assert.deepEqual(Object.keys(facts).sort(), ["blocked", "enrolled", "fingerprint", "flaggedOIDs", "roots"]);
  assert.equal(facts.enrolled, true); assert.equal(facts.blocked, false);
  assert.deepEqual(facts.flaggedOIDs, [q]);
  assert.deepEqual(facts.roots, [q, d].sort());
  return facts;
}
beforeEach(async () => {
  fx = makeFixture();
  cache = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
  bare = await cache.ensureClone(fx.originPath);
  base = git(bare, "rev-parse", "refs/remotes/origin/main");
  clone = (await cache.runnerCloneForBranch(bare, BRANCH, KEY, noProofReseed, RUN, false, undefined,
    { attemptId: ATTEMPT, isLive: () => false, beforeSeed: async () => {} })).path;
  await cache.markRecoveryCapture(bare, clone, BRANCH, RUN, ATTEMPT);
  opts = { context: { runId: RUN, generation: 1, kind: "issue", branch: BRANCH, barePath: bare,
    defaultIdentity: { ref: "refs/remotes/origin/main", sha: base } }, producingSource: source() };
  await fetch();
  q = commit("Q"); await fetch();
  await cache.recordRemediationScan(bare, BRANCH, source(),
    { trusted: true, tip: q, flaggedOIDs: [q], origin: "pre-exit" });
  d = commit("D", q); await fetch();
  s = commit("clean S"); await fetch();
  await cache.recordRemediationScan(bare, BRANCH, source(), { trusted: true, tip: s, flaggedOIDs: [] });
  assert.equal(journal().remediationRecovery.active, undefined);
});
afterEach(() => fx.cleanup());

test("pack admission sees only frozen durable negatives and literal producer inputs; B releases before drain", async t => {
  const before = snapshot(), expected = await query();
  let calls = 0, lockDepth = 0;
  const lock = (cache as any).withLock.bind(cache);
  t.mock.method(cache as any, "withLock", (key: string, action: () => Promise<unknown>, signal?: AbortSignal) =>
    lock(key, async () => {
      lockDepth++;
      try { return await action(); } finally { lockDepth--; }
    }, signal));
  const spawn = (cache as any).spawnGit.bind(cache);
  t.mock.method(cache as any, "spawnGit", async (...args: unknown[]) => {
    if (Array.isArray(args[1]) && args[1][0] === "pack-objects") {
      assert.equal(lockDepth, 1, "B remains held at actual producer start");
    }
    return spawn(...args);
  });
  const packed = await pack(async (sha, context) => {
    calls++;
    assert.equal(lockDepth, 1, "guard receives confirmed private facts under B");
    assert.equal(sha, s);
    assert.deepEqual(checkFacts(context), expected);
    assert.equal(context!.sourceSha, s); assert.equal(context!.candidateSha, s);
    assert.equal(context!.excludeSha, base); assert.equal(context!.defaultBlobSha, base);
    assert.deepEqual(context!.packExcludeShas, [base]);
  });
  assert.ok(packed);
  // This public B-taking read must finish while the producer's stream is still owned.
  assert.equal(lockDepth, 0);
  assert.deepEqual(await query(), expected);
  await drain(packed);
  await drain(await packed.replay());
  assert.equal(lockDepth, 0);
  assert.equal(calls, 2);
  assert.deepEqual(snapshot(), before);
});

test("push refreshes R and cleans scratch before passing fresh frozen negative facts", async () => {
  git(fx.originPath, "update-ref", "refs/heads/" + BRANCH, base);
  const expected = await query();
  let calls = 0;
  await push(async (sha, context) => {
    calls++; assert.equal(sha, s); assert.deepEqual(checkFacts(context), expected);
    assert.deepEqual(context.originalFloor, { kind: "absent" });
    assert.deepEqual(context.remoteFloor, { kind: "pinned", oid: base });
    assert.equal(git(bare, "for-each-ref", "--format=%(refname)", "refs/uzi-publication-floor/"), "");
  });
  assert.equal(calls, 1);
  assert.equal(git(fx.originPath, "rev-parse", "refs/heads/" + BRANCH), s);
});

for (const mode of ["pack", "push"] as const) {
  test(mode + ": wrong same-run attempt never falls back to custody", async t => {
    const spawn = t.mock.method(cache as any, "spawnGit");
    let calls = 0;
    const wrong = { exactRunId: RUN, workerSelectedSource: { ...source(), attemptId: OTHER } };
    const guard = async () => { calls++; };
    await assert.rejects(mode === "pack" ? pack(guard, wrong) : push(guard, wrong), refused);
    assert.equal(calls, 0); assert.equal(producerCount(spawn), 0);
    assert.equal(git(fx.originPath, "for-each-ref", "--format=%(refname)", "refs/heads/" + BRANCH), "");
  });

  test(mode + ": identity requires a guard even with readable nonblocked evidence", async t => {
    const spawn = t.mock.method(cache as any, "spawnGit");
    await assert.rejects(mode === "pack" ? pack() : push(), refused);
    // A malformed runtime caller cannot use null to bypass the required guard.
    const missing = null as unknown as undefined;
    await assert.rejects(mode === "pack" ? pack(missing) : push(missing), refused);
    assert.equal(producerCount(spawn), 0);
    assert.equal(git(fx.originPath, "for-each-ref", "--format=%(refname)", "refs/heads/" + BRANCH), "");
  });

  test(mode + ": source-less custody evidence is explicitly negative and guard refusal grants nothing", async t => {
    const expected = await cache.readRemediationRestrictionFacts(bare, BRANCH, RUN);
    const spawn = t.mock.method(cache as any, "spawnGit");
    const denial = new Error("trusted policy still refuses");
    let calls = 0;
    const guard = async (_sha: string, context?: CheckpointPublicationContext | PushPublicationContext) => {
      calls++; assert.deepEqual(checkFacts(context), expected); throw denial;
    };
    await assert.rejects(mode === "pack" ? pack(guard, { exactRunId: RUN }) : push(guard, { exactRunId: RUN }),
      error => error === denial);
    assert.equal(calls, 1); assert.equal(producerCount(spawn), 0);
    assert.equal(git(fx.originPath, "for-each-ref", "--format=%(refname)", "refs/heads/" + BRANCH), "");
  });
}

for (const fault of ["blocked", "malformed", "legacy", "prepared"] as const) {
  test("push freshly refuses " + fault + " written after preliminary query during remote refresh", async t => {
    assert.equal((await query()).blocked, false);
    const original = (cache as any).refreshScratchPublicationFloor.bind(cache);
    t.mock.method(cache as any, "refreshScratchPublicationFloor", async (...args: unknown[]) => {
      const floor = await original(...args);
      if (fault === "malformed") git(bare, "config", ACTIVE, "{private malformed journal");
      else {
        const j = journal(), r = j.remediationRecovery;
        if (fault === "blocked") r.blocked = true;
        if (fault === "legacy") delete r.excludedRoots;
        if (fault === "prepared") r.edges[0].phase = "prepared";
        write(j);
      }
      return floor;
    });
    let calls = 0;
    await assert.rejects(push(async () => { calls++; }), refused);
    assert.equal(calls, 0);
    assert.equal(git(fx.originPath, "for-each-ref", "--format=%(refname)", "refs/heads/" + BRANCH), "");
  });
}

test("pack freshly refuses RR blocked during awaited actual object preparation", async t => {
  const original = (cache as any).prepareCheckpointObjects.bind(cache);
  let prepared = 0, calls = 0;
  t.mock.method(cache as any, "prepareCheckpointObjects", async (...args: unknown[]) => {
    const result = await original(...args);
    await Promise.resolve(); block(); prepared++; return result;
  });
  const spawn = t.mock.method(cache as any, "spawnGit");
  await assert.rejects(pack(async () => { calls++; }), refused);
  assert.equal(prepared, 1); assert.equal(calls, 0); assert.equal(producerCount(spawn), 0);
});

test("pack observes checkpoint_pack phase mutations before callback or producer", async t => {
  const spawn = t.mock.method(cache as any, "spawnGit");
  let calls = 0;
  await assert.rejects(pack(async () => { calls++; }, identity(),
    step => { if (step === "checkpoint_pack") block(); }), refused);
  assert.equal(calls, 0); assert.equal(producerCount(spawn), 0);
});

test("drained safe producer cannot replay using cached negatives after RR blocks", async t => {
  const spawn = t.mock.method(cache as any, "spawnGit");
  let calls = 0;
  const packed = await pack(async (_sha, context) => { calls++; checkFacts(context); });
  assert.ok(packed); await drain(packed);
  assert.equal(producerCount(spawn), 1);
  block();
  await assert.rejects(packed.replay(), refused);
  assert.equal(calls, 1); assert.equal(producerCount(spawn), 1);
});

test("NFF refreshes only its remote proof without repeating admission or push", async t => {
  const original = (cache as any).runGit.bind(cache);
  let pushes = 0, observations = 0, calls = 0;
  const rejection = new Error("non-fast-forward");
  t.mock.method(cache as any, "runGit", async (dir: string, args: string[], ...rest: unknown[]) => {
    if (args[0] === "push") { pushes++; throw rejection; }
    if (args[0] === "ls-remote") observations++;
    return original(dir, args, ...rest);
  });
  const querySpy = t.mock.method(cache as any, "remediationRestrictionFactsUnderLock");
  await assert.rejects(push(async (_sha, context) => { calls++; checkFacts(context); }), error => error === rejection);
  assert.equal(calls, 1); assert.equal(pushes, 1); assert.equal(observations, 4);
  assert.equal(querySpy.mock.callCount(), 1);
});

test("push guard exception stays outside NFF refresh handling", async t => {
  let observations = 0;
  const original = (cache as any).runGit.bind(cache);
  t.mock.method(cache as any, "runGit", async (dir: string, args: string[], ...rest: unknown[]) => {
    if (args[0] === "push") assert.fail("guard refusal must prevent push");
    if (args[0] === "ls-remote") observations++;
    return original(dir, args, ...rest);
  });
  const rejection = new Error("non-fast-forward guard rejection");
  await assert.rejects(push(async (_sha, context) => { checkFacts(context); throw rejection; }),
    error => error === rejection);
  assert.equal(observations, 2);
});

for (const mode of ["pack", "push"] as const) {
  test(mode + ": entry snapshots exact identity before first await and pack replay", async () => {
    const selected = identity(), before = await query();
    const guard = async (_sha: string, context?: CheckpointPublicationContext | PushPublicationContext) => {
      assert.deepEqual(checkFacts(context), before);
    };
    const pending = mode === "pack" ? pack(guard, selected) : push(guard, selected);
    selected.exactRunId = "other-run";
    selected.workerSelectedSource.attemptId = OTHER;
    selected.workerSelectedSource.clonePath = "/arbitrary/rebind";
    selected.workerSelectedSource = { runId: "other-run", clonePath: clone, attemptId: OTHER };
    const result = await pending;
    if (result) { await drain(result); await drain(await result.replay()); }
  });
}

for (const fault of ["config", "directory", "readback"] as const) {
  for (const mode of ["pack", "push"] as const) {
    test(mode + ": durable " + fault + " failure refuses admission without callback or producer", async t => {
      const original = (cache as any).syncJournalAndReadBack.bind(cache);
      t.mock.method(cache as any, "syncJournalAndReadBack", (dir: string, branch: string, raw: string) =>
        original(dir, branch, raw, fault === "readback"
          ? { readback: async () => { throw new Error("private readback diagnostic"); } }
          : { sync: async (handle: { sync(): Promise<void> }, kind: string) => {
            if (kind === fault) throw new Error("private sync diagnostic");
            await handle.sync();
          } }));
      const spawn = t.mock.method(cache as any, "spawnGit");
      const before = snapshot();
      let calls = 0;
      await assert.rejects(mode === "pack" ? pack(async () => { calls++; }) : push(async () => { calls++; }), refused);
      assert.equal(calls, 0); assert.equal(producerCount(spawn), 0);
      assert.deepEqual(snapshot(), before);
    });
  }
}

for (const mode of ["pack", "push"] as const) {
  test(mode + ": real A and current live source-use lease are independently required", async t => {
    const lifetime = await acquireWorkerLifetime(fx.dataDir, testLockOptions());
    const runtime = new AuthorityRuntime({ lifetime, workerToken: "publication-restrictions", timeoutMs: 5000 });
    cache = new GitCache(fx.dataDir, nullLogger(), undefined,
      testGitCacheOptions({ authority: runtime.hooks, authorityRequired: true }));
    const spawn = t.mock.method(cache as any, "spawnGit");
    const run = (cache as any).runGit.bind(cache);
    let pushes = 0, calls = 0;
    t.mock.method(cache as any, "runGit", (dir: string, args: string[], ...rest: unknown[]) => {
      if (args[0] === "push") pushes++;
      return run(dir, args, ...rest);
    });
    const guard = async (_sha: string, context?: CheckpointPublicationContext | PushPublicationContext) => {
      calls++; checkFacts(context);
    };
    const attempt = () => runtime.withTransaction(async () => mode === "pack" ? await pack(guard) : await push(guard));
    await assert.rejects(attempt(), refused);
    assert.equal(calls, 0); assert.equal(producerCount(spawn), 0); assert.equal(pushes, 0);
    const lease = await runtime.bindSource(clone);
    const result = await attempt();
    if (result) await drain(result);
    assert.equal(runtime.leaseFor(clone), lease);
    assert.equal(calls, 1);
    assert.equal(producerCount(spawn), mode === "pack" ? 1 : 0);
    assert.equal(pushes, mode === "push" ? 1 : 0);
    await runtime.endSourceUse([clone]);
    await assert.rejects(attempt(), refused);
    assert.equal(calls, 1);
    assert.equal(producerCount(spawn), mode === "pack" ? 1 : 0);
    assert.equal(pushes, mode === "push" ? 1 : 0);
    // Custody can still report negatives; it cannot resurrect the ended live lease.
    assert.equal((await cache.readRemediationRestrictionFacts(bare, BRANCH, RUN)).enrolled, true);
    assert.equal(runtime.leaseFor(clone), undefined);
  });
}

test("legacy pack guard remains unbound and can take B without restriction facts", async () => {
  let calls = 0;
  const packed = await cache.checkpointPack(bare, BRANCH, undefined,
    { tipSha: s, excludeSha: base, defaultBlobSha: base }, undefined, s, async (_sha, context) => {
      calls++; assert.equal(context!.restrictionFacts, undefined); await query();
    });
  assert.ok(packed); await drain(packed); assert.equal(calls, 1);
});
