import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { test, type TestContext } from "node:test";
import { fixtureFetchTracking } from "./runner-tracking-fixture.js";
import { GitCache, type RecoveryJournalEntry } from "../src/git.js";
import { makeFixture } from "./fixture-repo.js";
import { noProofReseed, nullLogger, testGitCacheOptions } from "./helpers.js";

function command(dir: string, ...args: string[]) {
  return execFileSync("git", ["-C", dir, ...args], { encoding: "utf8", stdio: "pipe" }).trim();
}
const attemptId = "20261027T171401Z-g2-0123456789abcdef";
async function setup(t: TestContext) {
  const fx = makeFixture();
  t.after(() => fx.cleanup());
  const cache = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
  const bare = await cache.ensureClone(fx.originPath);
  fs.chmodSync(path.dirname(bare), 0o700); fs.chmodSync(bare, 0o700);
  const runId = "owner-real", key = "issue-3527";
  const clone = await cache.createOrAttachRunnerClone(bare, 3527, noProofReseed, runId);
  const branch = clone.branch;
  const source = { runId, clonePath: clone.path };
  await cache.markRecoveryCapture(bare, source.clonePath, branch, runId);
  const read = (): RecoveryJournalEntry => JSON.parse(command(bare, "config", `uzi-recovery.${branch}.clone`));
  const reservation = await cache.reserveRecoveryIteration(bare, branch, key, source, 1, 1, 7, 0);
  await cache.blockRecoveryEpisode(bare, branch, key, source, "adoption_failed", reservation);
  const charged = await cache.reserveRecoveryIteration(bare, branch, key, source, 1, 2, 8, 1);
  await fixtureFetchTracking(cache, bare, source.clonePath, branch, runId);
  const tip = command(source.clonePath, "rev-parse", "HEAD");
  await cache.recordRecoveryCapture(bare, branch, key, source, charged.attempts, tip);
  return { fx, cache, bare, branch, source, key, read, charged };
}

for (const phase of ["before-successor", "ledger-landed", "partial-clone", "handoff-write-landed", "ready-for-model",
  "owner-successor-before", "owner-successor-after", "owner-successor-read", "owner-successor-read-after",
  "handoff-write-before", "handoff-read", "handoff-read-after"] as const) {
  test(`real Git exact reservation survives adoption failure at ${phase}`, async t => {
    const f = await setup(t);
    const { cache, bare, branch, source, key, charged } = f;
    const seam = cache as any;
    if (phase === "before-successor") t.mock.method(seam, "resolveMarkerWrapperAdoption", async () => { throw new Error("injected"); });
    if (phase === "ledger-landed") {
      const append = seam.appendAttemptLedger.bind(cache);
      t.mock.method(seam, "appendAttemptLedger", async (...args: any[]) => { await append(...args); throw new Error("injected"); });
    }
    if (phase === "partial-clone") {
      const seed = seam.seedRunnerClone.bind(cache);
      t.mock.method(seam, "seedRunnerClone", async (...args: any[]) => { await seed(...args); throw new Error("injected"); });
    }
    let injected = false;
    if (phase === "handoff-write-landed" || phase.startsWith("owner-successor-") || phase.startsWith("handoff-")) {
      const run = seam.runGit.bind(cache);
      let armed = false;
      t.mock.method(seam, "runGit", async (cwd: string, args: string[], ...rest: any[]) => {
        const ownerWrite = args.at(-2) === `uzi-recoveryowner.${source.runId}.record` &&
          !!JSON.parse(args.at(-1)!).successor;
        const readyWrite = args.at(-2) === `uzi-recovery.${branch}.clone` &&
          JSON.parse(args.at(-1)!).recovery?.stage === "ready-for-model";
        if ((ownerWrite && phase === "owner-successor-before") || (readyWrite && phase === "handoff-write-before") ||
            (armed && args.includes("--list") && !phase.endsWith("-read-after"))) {
          injected = true; throw new Error("injected " + phase);
        }
        const result = await run(cwd, args, ...rest);
        if ((readyWrite && phase === "handoff-write-landed") || (ownerWrite && phase === "owner-successor-after") ||
            (armed && args.includes("--list") && phase.endsWith("-read-after"))) {
          injected = true; throw new Error("injected " + phase);
        }
        if ((ownerWrite && phase.startsWith("owner-successor-read")) || (readyWrite && phase.startsWith("handoff-read"))) armed = true;
        return result;
      });
    }
    if (phase === "ready-for-model") await cache.prepareRecoverySuccessor(bare, branch, key, source, charged.attempts, attemptId);
    else await assert.rejects(cache.prepareRecoverySuccessor(bare, branch, key, source, charged.attempts, attemptId), /injected/);
    if (phase.startsWith("owner-successor-") || phase.startsWith("handoff-")) assert.equal(injected, true);
    t.mock.restoreAll();
    const current = await cache.recoveryReservationSnapshot(bare, branch, key, source, charged);
    await cache.blockRecoveryEpisode(bare, branch, key, current, "adoption_failed", charged);
    assert.equal(f.read().recovery?.blocker, "adoption_failed");
    assert.ok(fs.existsSync(source.clonePath));
    if (phase === "handoff-write-landed" || phase === "ready-for-model" || phase.startsWith("handoff-read"))
      assert.notEqual(current.clonePath, source.clonePath);
    assert.equal(await cache.canRenewRecoveryAdoption(bare, branch, key, current, 3, 1), false);
    assert.equal(await cache.canRenewRecoveryAdoption(bare, branch, key, current, 3, 2), true);
  });
}

test("real Git completion preserves consumed grant and current inventory ignores the external namespace", async t => {
  const f = await setup(t);
  const { cache, bare, branch, key, source, charged } = f;
  const successor = await cache.prepareRecoverySuccessor(bare, branch, key, source, charged.attempts, attemptId);
  const expected = { runId: source.runId, clonePath: successor.path, attemptId };
  const raw = command(bare, "config", `uzi-recoveryowner.${source.runId}.record`);
  await cache.completeRecoveryEpisode(bare, branch, key, expected, { settled: true, processedEvents: 1, attemptId });
  assert.equal(command(bare, "config", `uzi-recoveryowner.${source.runId}.record`), raw);
  assert.equal((await cache.readInventoryCloneHeads(bare, source.runId)).kind, "verified");
  const next = await cache.reserveRecoveryIteration(bare, branch, key, expected, 1, 3, 8, 1);
  await cache.blockRecoveryEpisode(bare, branch, key, expected, "adoption_failed", next);
  assert.equal(await cache.canRenewRecoveryAdoption(bare, branch, key, expected, 4, 1), false);
  assert.equal(await cache.canRenewRecoveryAdoption(bare, branch, key, expected, 4, 2), true);
  await cache.discardRetainedRecovery(bare, branch, key, expected, true);
  assert.equal(JSON.parse(command(bare, "config", `uzi-recoveryowner.${source.runId}.record`)).consumed, 1);
});

for (const side of ["before", "after"] as const) {
  test(`real Git pending ${side.toUpperCase()} interrupts accounting without losing retained clones`, async t => {
    const f = await setup(t);
    const { cache, bare, branch, key, source, charged } = f;
    await cache.blockRecoveryEpisode(bare, branch, key, source, "adoption_failed", charged);
    fs.writeFileSync(path.join(source.clonePath, "pending-work.txt"), "retained pending bytes\n");
    const seam = cache as any;
    const run = seam.runGit.bind(cache);
    t.mock.method(seam, "runGit", async (cwd: string, args: string[], ...rest: any[]) => {
      const journalWrite = args.at(-2) === `uzi-recovery.${branch}.clone`;
      if (journalWrite && side === "before") throw new Error("interrupted reservation");
      const result = await run(cwd, args, ...rest);
      if (journalWrite && side === "after") throw new Error("interrupted reservation");
      return result;
    });
    await assert.rejects(cache.reserveRecoveryIteration(bare, branch, key, source, 1, 3, 9, 2), /interrupted/);
    t.mock.restoreAll();
    const raw = command(bare, "config", `uzi-recovery.${branch}.clone`);
    await assert.rejects(cache.reserveRecoveryIteration(bare, branch, key, source, 1, 3, 9, 2), /pending/);
    assert.equal(command(bare, "config", `uzi-recovery.${branch}.clone`), raw);
    let ownership = 0;
    const next = await cache.reserveRecoveryIteration(bare, branch, key, source, 1, 4, 9, side === "before" ? 3 : 2,
      async () => { ownership++; });
    assert.equal(ownership, side === "before" ? 1 : 0);
    assert.equal(next.claimGeneration, 4);
    assert.equal(next.attempts, side === "before" ? 1 : 2);
    assert.equal(fs.readFileSync(path.join(source.clonePath, "pending-work.txt"), "utf8"), "retained pending bytes\n");
  });
}
