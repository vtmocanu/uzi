import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { randomBytes, randomUUID } from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import { acquireWorkerLifetime, type WorkerLifetime } from "../src/authority-lock.js";
import { AuthorityRuntime } from "../src/authority-runtime.js";
import { openAuthorityStore, type SlotSnapshot } from "../src/completion-authority.js";
import { formatAttemptId } from "../src/attempt-path.js";
import { GitCache, type RetainedHoldsSnapshot, type RetainedSourceSelector } from "../src/git.js";
import { resetResidueQuarantineForTests } from "../src/residue-quarantine.js";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { nullLogger, testGitCacheOptions } from "./helpers.js";
import { testLockOptions } from "./authority-test-support.js";

// issue #3404 M2 U5 - the guarded operator discard takes over the exact footprints it already authorizes with
// its own pending transaction (AuthorityStore.admitOperatorDiscard), instead of being blocked by authority
// state: a foreign active slot a crashed worker left, an ineligible slot with settled_failed residue. A
// running pending record still refuses, and the discard never widens which sources it may delete.

const TOKEN = "unit-join-" + randomBytes(12).toString("hex");
const GIT_ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0" };
const NO_HOLDS: RetainedHoldsSnapshot = { state: "read", holds: [] };
const quiet = async (): Promise<boolean> => true;
const notLive = (): boolean => false;

let fx: Fixture;
let bare: string;
let lifetime: WorkerLifetime;
let live: AuthorityRuntime;
let git: GitCache;

const gitIn = (dir: string, args: string[]): string => execFileSync("git", ["-C", dir, ...args], { env: GIT_ENV, encoding: "utf8", stdio: ["pipe", "pipe", "pipe"] }).trim();
const runtimeOf = (): AuthorityRuntime => new AuthorityRuntime({ lifetime, workerToken: TOKEN, workerIncarnation: randomUUID(), timeoutMs: 5000 });

async function slotOf(rt: AuthorityRuntime, p: string): Promise<SlotSnapshot | null> {
  return rt.withTransaction(async tx => {
    const store = await openAuthorityStore(tx, { workerToken: TOKEN });
    return store.readSlot(tx, await store.slotKey(tx, p));
  });
}

/** A planted abandoned attempt with its skills sibling and ledger entry: discardable by the plain discard. */
function plant(n: number): { dir: string; skills: string; attemptId: string; selector: RetainedSourceSelector } {
  const attemptId = formatAttemptId(new Date(Date.UTC(2026, 0, 1, 0, 0, n)), 1, (n + 1).toString(16).padStart(16, "0"));
  const dir = `${git.runnerClonePath(bare, "issue-3001")}.attempt-${attemptId}`;
  const skills = path.join(path.dirname(dir), `.uzi-skills-${path.basename(dir)}`);
  fs.mkdirSync(dir, { recursive: true });
  fs.writeFileSync(path.join(dir, "WORK.txt"), attemptId);
  fs.mkdirSync(skills, { recursive: true });
  fs.writeFileSync(path.join(skills, "plugin.json"), "{}");
  gitIn(bare, ["config", "--local", "--add", "uzi-attempts.agent/issue-3001.entry", JSON.stringify({ attemptId, runId: "run-ab", clonePath: dir, state: "abandoned" })]);
  return { dir, skills, attemptId, selector: { runId: "run-ab", generation: 1, attempt: attemptId } };
}

/** What retained-source-server does: A around the discard, then the end of the source use it opened. */
async function discard(rt: AuthorityRuntime, source: ReturnType<typeof plant>, over: { quiesce?: () => Promise<boolean> } = {}) {
  return rt.withTransaction(async () => {
    const inspected = await git.inspectRetainedSources({ selector: source.selector, holds: NO_HOLDS, isLivePath: notLive, barePath: bare });
    assert.equal(inspected.sources.length, 1);
    const result = await git.discardStoppedSource({
      selector: source.selector, confirm: inspected.sources[0]!.descriptor, holds: NO_HOLDS, isLivePath: notLive,
      quiesce: over.quiesce ?? quiet, barePath: bare,
    });
    await rt.endSourceUse([source.dir]);
    return result;
  });
}

beforeEach(async () => {
  fx = makeFixture();
  const root = fs.realpathSync(fx.dataDir);
  lifetime = await acquireWorkerLifetime(root, testLockOptions());
  live = runtimeOf();
  git = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions({ authority: live.hooks, authorityRequired: true }));
  bare = await git.ensureClone(fx.originPath);
  fs.chmodSync(path.dirname(bare), 0o700);
  fs.chmodSync(bare, 0o700);
});

afterEach(() => {
  resetResidueQuarantineForTests();
  fx.cleanup();
});

describe("operator discard takes over exactly its footprint", () => {
  it("a fresh source: the discard's own transaction deletes it and tombstones the slot", async () => {
    const source = plant(3);
    const result = await discard(live, source);
    assert.equal(result.kind, "done");
    assert.equal(fs.existsSync(source.dir), false);
    assert.equal(fs.existsSync(source.skills), false);
    const slot = await slotOf(live, source.dir);
    assert.equal(slot?.state, "retired");
    assert.equal(slot?.pending ?? null, null);
    assert.equal(slot?.retiring ?? null, null);
  });

  it("an active slot a crashed worker left behind no longer blocks the discard", async () => {
    const source = plant(4);
    const crashed = runtimeOf();
    await crashed.bindSource(source.dir);
    assert.equal((await slotOf(live, source.dir))?.state, "active");
    // Without the takeover this is the "source is active under another worker incarnation" refusal.
    const result = await discard(live, source);
    assert.equal(result.kind, "done");
    assert.equal(fs.existsSync(source.dir), false);
    assert.equal((await slotOf(live, source.dir))?.state, "retired");
  });

  it("an ineligible slot with settled_failed residue is resolved by the discard that deletes that residue", async () => {
    const source = plant(5);
    const failed = runtimeOf();
    await failed.withTransaction(async () => {
      await failed.hooks.admitSourceUse(source.dir, "seed-attempt");
      const settlement = await failed.hooks.beginDestructive([source.skills], "runner-uid-delete");
      await settlement.settle({ ok: false, residue: [source.skills] });
    });
    const before = await slotOf(live, source.dir);
    assert.equal(before?.state, "ineligible");
    assert.equal(before?.pending?.status, "settled_failed");
    const result = await discard(live, source);
    assert.equal(result.kind, "done");
    const slot = await slotOf(live, source.dir);
    assert.equal(slot?.state, "retired");
    assert.equal(slot?.pending ?? null, null, "the residue it recorded went with the deletion");
  });

  it("a running pending record (an unresolved helper) still refuses before anything is deleted", async () => {
    const source = plant(6);
    const dying = runtimeOf();
    await dying.withTransaction(async () => {
      await dying.hooks.admitSourceUse(source.dir, "seed-attempt");
      await dying.hooks.beginDestructive([source.skills], "runner-uid-delete");
    });
    const result = await discard(live, source);
    assert.equal(result.kind, "refused");
    assert.equal(result.kind === "refused" ? result.reason : "", "source_use_refused");
    assert.ok(fs.existsSync(source.dir) && fs.existsSync(source.skills), "nothing was deleted");
    assert.equal((await slotOf(live, source.dir))?.pending?.status, "running");
  });

  it("settled_failed residue outside the discard's footprint refuses (the discard never clears evidence it does not delete)", async () => {
    const source = plant(7);
    const failed = runtimeOf();
    await failed.withTransaction(async () => {
      await failed.hooks.admitSourceUse(source.dir, "seed-attempt");
      const settlement = await failed.hooks.beginDestructive([source.skills], "runner-uid-delete");
      await settlement.settle({ ok: false, residue: [source.skills] });
    });
    // Rewrite the evidence to name a path the discard does not own.
    await live.withTransaction(async tx => {
      const store = await openAuthorityStore(tx, { workerToken: TOKEN });
      await assert.rejects(store.admitOperatorDiscard(tx, source.dir, [path.join(path.dirname(source.dir), "somewhere-else")]), /not covered by the discard footprint/);
    });
    assert.ok(fs.existsSync(source.dir));
  });

  it("a refused discard (not quiescent) rewrites no authority state at all", async () => {
    const source = plant(8);
    const result = await discard(live, source, { quiesce: async () => false });
    assert.equal(result.kind, "refused");
    assert.equal(result.kind === "refused" ? result.reason : "", "not_quiescent");
    assert.ok(fs.existsSync(source.dir) && fs.existsSync(source.skills));
    assert.equal(await slotOf(live, source.dir), null, "no slot was written: the takeover comes after every refusal");
  });

  it("a crashed worker's active slot survives a not_quiescent discard: still active under the dead incarnation, a seed is still refused", async () => {
    const source = plant(11);
    const crashed = runtimeOf();
    await crashed.bindSource(source.dir);
    const before = await slotOf(live, source.dir);
    assert.equal(before?.state, "active");
    const result = await discard(live, source, { quiesce: async () => false });
    assert.equal(result.kind, "refused");
    assert.deepEqual(await slotOf(live, source.dir), before, "the slot is byte-for-byte what the crashed worker left");
    await assert.rejects(live.withTransaction(() => live.hooks.admitSourceUse(source.dir, "seed-attempt")), /active under another worker incarnation/);
  });

  it("a discard that took the slot over but deleted nothing restores the prior state exactly (a foreign active epoch stays guarded)", async t => {
    const source = plant(12);
    const crashed = runtimeOf();
    await crashed.bindSource(source.dir);
    const before = await slotOf(live, source.dir);
    assert.equal(before?.state, "active");
    // Every delete is refused by a guard after the takeover: nothing is removed.
    t.mock.method(git as unknown as { deleteRetainedArtifact(): Promise<boolean> }, "deleteRetainedArtifact", async () => false);
    const result = await discard(live, source);
    assert.equal(result.kind, "incomplete");
    assert.ok(fs.existsSync(source.dir) && fs.existsSync(source.skills));
    const after = await slotOf(live, source.dir);
    assert.equal(after?.state, "active");
    assert.equal(after?.epoch, before?.epoch, "the foreign epoch is back");
    assert.deepEqual(after?.owner, before?.owner, "under the dead incarnation");
    assert.equal(after?.retiring ?? null, null);
    await assert.rejects(live.withTransaction(() => live.hooks.admitSourceUse(source.dir, "seed-attempt")), /active under another worker incarnation/);
  });

  it("a discard that deleted nothing keeps a settled_failed record as evidence (never ineligible without it)", async t => {
    const source = plant(13);
    const failed = runtimeOf();
    await failed.withTransaction(async () => {
      await failed.hooks.admitSourceUse(source.dir, "seed-attempt");
      const settlement = await failed.hooks.beginDestructive([source.skills], "runner-uid-delete");
      await settlement.settle({ ok: false, residue: [source.skills] });
    });
    const before = await slotOf(live, source.dir);
    t.mock.method(git as unknown as { deleteRetainedArtifact(): Promise<boolean> }, "deleteRetainedArtifact", async () => false);
    const result = await discard(live, source);
    assert.equal(result.kind, "incomplete");
    const after = await slotOf(live, source.dir);
    assert.deepEqual(after?.pending, before?.pending, "the residue evidence stays on record");
    assert.equal(after?.state, before?.state);
    assert.equal(after?.epoch, before?.epoch);
  });

  it("a sibling attempt of the same key is untouched", async () => {
    const mine = plant(9);
    const sibling = plant(10);
    assert.equal((await discard(live, mine)).kind, "done");
    assert.ok(fs.existsSync(sibling.dir) && fs.existsSync(sibling.skills));
    assert.equal(await slotOf(live, sibling.dir), null, "no authority state was written for the sibling");
  });
});
