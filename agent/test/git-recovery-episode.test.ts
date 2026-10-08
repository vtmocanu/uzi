import { beforeEach, afterEach, test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { GitCache, RetainedRecoveryBlockedError } from "../src/git.js";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { nullLogger, noProofReseed, testGitCacheOptions } from "./helpers.js";
import { fixtureFetchTracking } from "./runner-tracking-fixture.js";

const branch = "agent/issue-2512";
const key = "issue-2512";
const runId = "run-2512";
const aid = (n: number) => `20261008T171401Z-g${n}-0123456789abcdef`;
let fx: Fixture;
let cache: GitCache;
let bare: string;
let source: { runId: string; clonePath: string };
function git(dir: string, args: string[]): string {
  return execFileSync("git", ["-C", dir, ...args], {
    encoding: "utf8", env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" },
    stdio: ["pipe", "pipe", "pipe"],
  }).trim();
}
function journal(): any {
  return JSON.parse(git(bare, ["config", `uzi-recovery.${branch}.clone`]));
}
function writeJournal(value: unknown): void {
  git(bare, ["config", `uzi-recovery.${branch}.clone`, JSON.stringify(value)]);
}
function recreate(): GitCache { return new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions()); }
beforeEach(async () => {
  fx = makeFixture();
  cache = recreate();
  bare = await cache.ensureClone(fx.originPath);
  fs.chmodSync(path.dirname(bare), 0o700);
  fs.chmodSync(bare, 0o700);
  const clone = await cache.createOrAttachRunnerClone(bare, 2512, noProofReseed, runId);
  source = { runId, clonePath: clone.path };
  await cache.markRecoveryCapture(bare, clone.path, branch, runId);
});
afterEach(() => fx.cleanup());

test("missing source discovery carries trusted coordinates and first blocker persists exhausted identity", async () => {
  fs.renameSync(source.clonePath, source.clonePath + ".saved");
  await assert.rejects(cache.discoverRetainedRecovery(fx.originPath, branch, key, runId), err => {
    assert.ok(err instanceof RetainedRecoveryBlockedError);
    assert.equal(err.barePath, bare);
    assert.equal(err.branch, branch);
    assert.equal(err.key, key);
    assert.equal(err.journal.clonePath, source.clonePath);
    return true;
  });
  await cache.blockRecoveryEpisode(bare, branch, key, source, "source_missing");
  assert.equal(journal().recovery.attempts, 3);
  assert.equal(journal().recovery.stage, "blocked");
  await cache.blockRecoveryEpisode(bare, branch, key, source, "capture_failed");
  assert.equal(journal().recovery.blocker, "source_missing");
  fs.renameSync(source.clonePath + ".saved", source.clonePath);
  await assert.rejects(cache.reserveRecoveryIteration(bare, branch, key, source), /blocked/);
});

test("backward and invalid clocks fail closed at reservation, capture, adoption and reset", async () => {
  const realNow = Date.now;
  const first = await cache.reserveRecoveryIteration(bare, branch, key, source, 30_000);
  const tip = await capture();
  const original = journal();
  try {
    for (const now of [first.startedAt - 1, NaN, Infinity]) {
      for (const operation of ["reserve", "capture", "adopt", "complete"]) {
        Date.now = realNow;
        writeJournal(original);
        let expected: { runId: string; clonePath: string; attemptId?: string } = source;
        if (operation === "adopt" || operation === "complete") {
          await cache.recordRecoveryCapture(bare, branch, key, source, 1, tip);
        }
        if (operation === "complete") {
          const clone = await cache.prepareRecoverySuccessor(bare, branch, key, source, 1, aid(8));
          expected = { runId, clonePath: clone.path, attemptId: aid(8) };
        }
        Date.now = () => now;
        const call = operation === "reserve" ? cache.reserveRecoveryIteration(bare, branch, key, source)
          : operation === "capture" ? cache.recordRecoveryCapture(bare, branch, key, source, 1, tip)
          : operation === "adopt" ? cache.prepareRecoverySuccessor(bare, branch, key, source, 1, aid(9))
          : cache.completeRecoveryEpisode(bare, branch, key, expected, { settled: true, processedEvents: 1, attemptId: aid(8) });
        await assert.rejects(call, /clock|current/);
        assert.equal(journal().recovery.stage, "blocked");
        assert.equal(journal().recovery.attempts, 1);
        assert.equal(journal().recovery.startedAt, first.startedAt);
        if (operation === "complete") {
          Date.now = realNow;
          fs.rmSync(expected.clonePath, { recursive: true, force: true });
          // Clear the fixture ledger to reuse this test-only successor identity.
          git(bare, ["config", "--unset-all", `uzi-attempts.${branch}.entry`]);
        }
      }
    }
  } finally { Date.now = realNow; }
});

test("backoff uses persisted attempt exponent and permits a 30-second base", async () => {
  assert.equal((await cache.reserveRecoveryIteration(bare, branch, key, source, 30_000)).backoffMs, 30_000);
  assert.equal((await recreate().reserveRecoveryIteration(bare, branch, key, source, 30_000)).backoffMs, 60_000);
  assert.equal((await recreate().reserveRecoveryIteration(bare, branch, key, source, 30_000)).backoffMs, 120_000);
});

test("untrusted bare config and caller coordinates cannot publish progress", async () => {
  const before = journal();
  await assert.rejects(cache.reserveRecoveryIteration(fx.originPath, branch, key, source));
  const config = path.join(bare, "config");
  fs.renameSync(config, config + ".saved");
  fs.symlinkSync(config + ".saved", config);
  await assert.rejects(cache.discoverRetainedRecovery(fx.originPath, branch, key, runId), /unsafe/);
  await assert.rejects(cache.blockRecoveryEpisode(bare, branch, key, source, "source_missing"), /unsafe/);
  fs.unlinkSync(config);
  fs.renameSync(config + ".saved", config);
  git(bare, ["config", "include.path", config + ".saved"]);
  await assert.rejects(cache.reserveRecoveryIteration(bare, branch, key, source), /unsafe/);
  git(bare, ["config", "--unset", "include.path"]);
  assert.deepEqual(journal(), before);
});
async function capture(wip = false): Promise<string> {
  fs.writeFileSync(path.join(source.clonePath, "work.txt"), "retained work\n");
  git(source.clonePath, ["add", "work.txt"]);
  if (wip) await cache.commitWipMarker(source.clonePath);
  else git(source.clonePath, ["commit", "-m", "milestone"]);
  await fixtureFetchTracking(cache, bare, source.clonePath, branch, runId);
  return git(source.clonePath, ["rev-parse", "HEAD"]);
}
async function ready(wip = false) {
  const reservation = await cache.reserveRecoveryIteration(bare, branch, key, source);
  const tip = await capture(wip);
  await cache.recordRecoveryCapture(bare, branch, key, source, reservation.attempts, tip);
  const successor = await cache.prepareRecoverySuccessor(bare, branch, key, source, reservation.attempts, aid(2));
  return { successor, tip, expected: { runId, clonePath: successor.path, attemptId: aid(2) } };
}

test("legacy discovery is credential-free and precharged attempts survive worker recreation and reclaim", async () => {
  assert.equal(journal().recovery, undefined);
  assert.equal((await cache.discoverRetainedRecovery(fx.originPath, branch, key, runId))?.journal.clonePath, source.clonePath);
  const first = await cache.reserveRecoveryIteration(bare, branch, key, source, 20_000);
  await recreate().releaseAttemptInPlace(bare, source.clonePath, branch, runId, "reclaimed");
  await recreate().markRecoveryCapture(bare, source.clonePath, branch, runId);
  const second = await recreate().reserveRecoveryIteration(bare, branch, key, source, 20_000);
  assert.equal(second.attempts, 2);
  assert.equal(second.startedAt, first.startedAt);
  assert.equal(second.deadline, first.deadline);
  assert.equal(second.backoffMs, 40_000);
  assert.equal((await recreate().reserveRecoveryIteration(bare, branch, key, source)).attempts, 3);
  await assert.rejects(recreate().reserveRecoveryIteration(bare, branch, key, source), /budget exhausted/);
  assert.equal(journal().recovery.stage, "blocked");
  await assert.rejects(cache.reserveRecoveryIteration(bare, branch, key, source), /blocked/);
});

test("identity mismatches and missing known source never overwrite attribution or seed", async () => {
  const before = git(bare, ["config", `uzi-recovery.${branch}.clone`]);
  await assert.rejects(cache.markRecoveryCapture(bare, source.clonePath, branch, "foreign"), /overwritten/);
  await assert.rejects(cache.discoverRetainedRecovery(fx.originPath, branch, key, "foreign"), /foreign/);
  await assert.rejects(cache.reserveRecoveryIteration(bare, branch, "other-key", source), /key/);
  fs.renameSync(source.clonePath, source.clonePath + ".saved");
  await assert.rejects(cache.createOrAttachRunnerClone(bare, 2512, noProofReseed, runId), /missing/);
  await assert.rejects(cache.createOrAttachRunnerClone(bare, 2512, noProofReseed, runId, true, undefined, {
    attemptId: aid(2), isLive: () => false, beforeSeed: async () => {}, quiescent: async () => true,
  }), /missing/);
  assert.equal(git(bare, ["config", `uzi-recovery.${branch}.clone`]), before);
});

test("invalid progress, unknown versions, attribution and unsafe paths fail closed", async () => {
  await cache.reserveRecoveryIteration(bare, branch, key, source);
  const original = journal();
  for (const mutation of [
    { version: 2 }, { attempts: 4 }, { attempts: 1.5 }, { deadline: 0 }, { backoffMs: 480_001 },
    { stage: "ready-for-model" }, { stage: "blocked", blocker: "unsafe\noutput" },
    { source: { ...source, runId: "foreign" } }, { surprise: true },
  ]) {
    writeJournal({ ...original, recovery: { ...original.recovery, ...mutation } });
    await assert.rejects(cache.discoverRetainedRecovery(fx.originPath, branch, key, runId), /invalid|identity/);
  }
  writeJournal({ ...source, recovery: original.recovery, retainedSources: [{ ...source, clonePath: "/unsafe/../path" }] });
  await assert.rejects(cache.discoverRetainedRecovery(fx.originPath, branch, key, runId), /invalid/);
});

test("successor handoff preserves predecessor bytes, canonical inventory, pins and episode identity", async () => {
  const reservation = await cache.reserveRecoveryIteration(bare, branch, key, source);
  const tip = await capture();
  const headBytes = fs.readFileSync(path.join(source.clonePath, ".git", "HEAD"));
  const content = fs.readFileSync(path.join(source.clonePath, "work.txt"));
  await cache.recordRecoveryCapture(bare, branch, key, source, 1, tip);
  await assert.rejects(cache.prepareRecoverySuccessor(bare, branch, key, source, 2, aid(2)), /current/);
  const successor = await cache.prepareRecoverySuccessor(bare, branch, key, source, 1, aid(2), { selfContained: true });
  assert.equal(successor.attemptId, aid(2));
  assert.equal(successor.attemptId, journal().attemptId);
  assert.notEqual(successor.path, source.clonePath);
  assert.equal(git(successor.path, ["rev-parse", "HEAD"]), tip);
  assert.deepEqual(fs.readFileSync(path.join(source.clonePath, ".git", "HEAD")), headBytes);
  assert.deepEqual(fs.readFileSync(path.join(source.clonePath, "work.txt")), content);
  assert.equal(journal().recovery.deadline, reservation.deadline);
  assert.equal(journal().recovery.source.clonePath, source.clonePath);
  assert.equal(journal().retainedSources[0].attemptId, undefined);
  assert.equal(git(bare, ["rev-parse", `refs/uzi-recovery-episode/${runId}/${tip}`]), tip);
  const inventory = await cache.readInventoryCloneHeads(bare, runId);
  assert.equal(inventory.kind, "verified");
  if (inventory.kind === "verified") {
    assert.ok(inventory.clones.some(c => c.clonePath === source.clonePath));
    assert.ok(inventory.clones.some(c => c.clonePath === successor.path));
  }
  await cache.releaseAttemptInPlace(bare, successor.path, branch, runId, "abandoned");
  assert.equal(journal().recovery.attempts, 1);
  await assert.rejects(cache.retireRunnerClone(bare, source.clonePath, branch, runId, { discard: true }), /different clone|disposition/);
  const expected = { runId, clonePath: successor.path, attemptId: aid(2) };
  const second = await recreate().reserveRecoveryIteration(bare, branch, key, expected);
  assert.equal(second.attempts, 2);
  assert.equal(second.deadline, reservation.deadline);
  await assert.rejects(cache.completeRecoveryEpisode(bare, branch, key, expected,
    { settled: true, processedEvents: 1, attemptId: aid(2) }), /cannot complete/);
});

test("multiple successor handoffs keep the original episode source, deadline and total budget", async () => {
  const { expected, tip } = await ready();
  const first = journal().recovery;
  await cache.reserveRecoveryIteration(bare, branch, key, expected);
  await cache.recordRecoveryCapture(bare, branch, key, expected, 2, tip);
  const second = await cache.prepareRecoverySuccessor(bare, branch, key, expected, 2, aid(3));
  assert.equal(journal().recovery.source.clonePath, source.clonePath);
  assert.equal(journal().recovery.startedAt, first.startedAt);
  assert.equal(journal().recovery.deadline, first.deadline);
  assert.equal(journal().recovery.attempts, 2);
  assert.ok(journal().retainedSources.some((s: any) => s.clonePath === expected.clonePath));
  const next = { runId, clonePath: second.path, attemptId: aid(3) };
  await cache.releaseAttemptInPlace(bare, second.path, branch, runId, "reclaimed");
  await cache.markRecoveryCapture(bare, second.path, branch, runId, aid(3));
  assert.equal((await recreate().reserveRecoveryIteration(bare, branch, key, next)).attempts, 3);
  await assert.rejects(cache.reserveRecoveryIteration(bare, branch, key, next), /budget/);
});

test("bridge recovery preserves the exact tree and both parent histories", async () => {
  await cache.reserveRecoveryIteration(bare, branch, key, source);
  const tip = await capture();
  const base = git(bare, ["rev-parse", "refs/remotes/origin/main"]);
  const tree = git(bare, ["rev-parse", `${tip}^{tree}`]);
  const floor = git(bare, ["-c", "user.name=fixture", "-c", "user.email=fixture@example.com",
    "commit-tree", tree, "-p", base, "-m", "parallel published history"]);
  const bridge = await cache.bridgeToFloors(bare, tip, [floor]);
  assert.equal(bridge.kind, "built");
  assert.ok(bridge.kind === "built");
  await cache.recordRecoveryCapture(bare, branch, key, source, 1, bridge.sha);
  const successor = await cache.prepareRecoverySuccessor(bare, branch, key, source, 1, aid(2));
  assert.equal(git(successor.path, ["rev-parse", "HEAD"]), bridge.sha);
  assert.equal(git(successor.path, ["write-tree"]), tree);
  git(successor.path, ["merge-base", "--is-ancestor", tip, "HEAD"]);
  git(successor.path, ["merge-base", "--is-ancestor", floor, "HEAD"]);
});

test("WIP unwrap positively preserves staged tree and committed parent history", async () => {
  const { successor, tip } = await ready(true);
  assert.equal(git(successor.path, ["rev-parse", "HEAD"]), git(bare, ["rev-parse", `${tip}^`]));
  assert.equal(git(successor.path, ["write-tree"]), git(bare, ["rev-parse", `${tip}^{tree}`]));
  assert.equal(fs.readFileSync(path.join(successor.path, "work.txt"), "utf8"), "retained work\n");
  assert.match(git(successor.path, ["diff", "--cached", "--name-only"]), /work.txt/);
});

test("only explicit completion with settled processing evidence resets budget; blocked never resets", async () => {
  const { expected, tip } = await ready();
  await assert.rejects(cache.completeRecoveryEpisode(bare, branch, key, expected,
    { settled: true, processedEvents: 0, attemptId: aid(2) }), /evidence/);
  fs.writeFileSync(path.join(bare, "config.lock"), "");
  await assert.rejects(cache.completeRecoveryEpisode(bare, branch, key, expected,
    { settled: true, processedEvents: 1, attemptId: aid(2) }));
  fs.unlinkSync(path.join(bare, "config.lock"));
  assert.equal(journal().recovery.attempts, 1);
  await cache.completeRecoveryEpisode(bare, branch, key, expected, { settled: true, processedEvents: 1, attemptId: aid(2) });
  assert.equal(journal().recovery, undefined);
  assert.ok(journal().retainedSources.some((s: any) => s.clonePath === source.clonePath));
  assert.equal(git(bare, ["rev-parse", `refs/uzi-recovery-episode/${runId}/${tip}`]), tip);
  await cache.reserveRecoveryIteration(bare, branch, key, expected);
  await cache.blockRecoveryEpisode(bare, branch, key, expected, "capture_failed");
  await assert.rejects(cache.completeRecoveryEpisode(bare, branch, key, expected,
    { settled: true, processedEvents: 1, attemptId: aid(2) }), /cannot complete/);
  await assert.rejects(cache.reserveRecoveryIteration(bare, branch, key, expected), /blocked/);
});

test("settled model completion beyond recovery deadline durably resets without releasing retention", async () => {
  const { expected, tip } = await ready();
  const before = journal();
  const realNow = Date.now;
  try {
    Date.now = () => before.recovery.deadline + 600_000;
    await cache.completeRecoveryEpisode(bare, branch, key, expected,
      { settled: true, processedEvents: 1, attemptId: aid(2) });
  } finally { Date.now = realNow; }
  const persisted = (await recreate().discoverRetainedRecovery(fx.originPath, branch, key, runId))!.journal;
  const { recovery: _completed, ...retained } = before;
  assert.deepEqual(persisted, retained, "custody and retained source descriptors survive completion");
  assert.equal(fs.readFileSync(path.join(source.clonePath, "work.txt"), "utf8"), "retained work\n");
  assert.equal(git(bare, ["rev-parse", `refs/uzi-recovery-episode/${runId}/${tip}`]), tip);
});

test("pin write failure retains charged capturing stage; deadline is never refreshed", async () => {
  const first = await cache.reserveRecoveryIteration(bare, branch, key, source);
  const tip = await capture();
  fs.mkdirSync(path.join(bare, "refs", "uzi-recovery-episode", runId), { recursive: true });
  fs.writeFileSync(path.join(bare, "refs", "uzi-recovery-episode", runId, tip + ".lock"), "");
  await assert.rejects(cache.recordRecoveryCapture(bare, branch, key, source, 1, tip));
  assert.equal(journal().recovery.stage, "capturing");
  assert.equal(journal().recovery.attempts, 1);
  const expired = { ...journal(), recovery: { ...journal().recovery, startedAt: first.startedAt - 300_001, deadline: first.deadline - 300_001 } };
  writeJournal(expired);
  await assert.rejects(recreate().reserveRecoveryIteration(bare, branch, key, source), /budget/);
  assert.equal(journal().recovery.deadline, expired.recovery.deadline);
});

test("failed fresh successor retains adopting evidence and cannot be mistaken for execution authority", async () => {
  await cache.reserveRecoveryIteration(bare, branch, key, source);
  const tip = await capture();
  await cache.recordRecoveryCapture(bare, branch, key, source, 1, tip);
  // A disjoint default prevents positive history verification after successor identity lands.
  const tree = git(bare, ["rev-parse", `${tip}^{tree}`]);
  const root = git(bare, ["-c", "user.name=fixture", "-c", "user.email=fixture@example.com", "commit-tree", tree, "-m", "unrelated"]);
  git(bare, ["update-ref", "refs/remotes/origin/main", root]);
  await assert.rejects(cache.prepareRecoverySuccessor(bare, branch, key, source, 1, aid(2)), /history/);
  assert.equal(journal().clonePath, source.clonePath);
  assert.equal(journal().recovery.stage, "adopting");
  assert.equal(journal().recovery.successor.attemptId, aid(2));
  assert.equal((await recreate().reserveRecoveryIteration(bare, branch, key, source)).attempts, 2);
});

test("effective attempt mode refuses unknown markers, unsafe coordinates and config read errors", async () => {
  const marker = `uzi-attemptmode.${key}.enabled`;
  await assert.rejects(cache.recoveryAttemptMode(fx.originPath, "../escape"), /key/);
  await assert.rejects(cache.recoveryAttemptMode(fx.originPath + "-missing", key));
  for (const value of ["false", "", "unknown", "TRUE"]) {
    git(bare, ["config", marker, value]);
    await assert.rejects(cache.recoveryAttemptMode(fx.originPath, key), /attempt mode/);
    await assert.rejects(cache.createOrAttachRunnerClone(bare, 2512, noProofReseed, runId));
  }
  git(bare, ["config", marker, "true"]);
  git(bare, ["config", "--add", marker, "true"]);
  await assert.rejects(cache.recoveryAttemptMode(fx.originPath, key), /attempt mode/);
  git(bare, ["config", "--unset-all", marker]);
  const config = path.join(bare, "config");
  fs.renameSync(config, config + ".saved");
  fs.symlinkSync(config + ".saved", config);
  await assert.rejects(cache.recoveryAttemptMode(fx.originPath, key), /unsafe/);
  fs.unlinkSync(config);
  fs.writeFileSync(config, "[broken config");
  await assert.rejects(cache.recoveryAttemptMode(fx.originPath, key));
  fs.unlinkSync(config);
  fs.renameSync(config + ".saved", config);
  git(bare, ["config", "include.path", config + ".saved"]);
  await assert.rejects(cache.recoveryAttemptMode(fx.originPath, key), /unsafe/);
  git(bare, ["config", "--unset", "include.path"]);
  assert.equal(await cache.recoveryAttemptMode(fx.originPath, key), false);
});

test("more than eight settled recovery episodes retain every source, pin and byte and adopt fresh paths", async () => {
  let expected: { runId: string; clonePath: string; attemptId?: string } = source;
  const retained: Array<{ clonePath: string; head: Buffer; bytes: Buffer; tip: string }> = [];
  // Ten episodes exceed both former descriptor limits; each performs real capture and handoff.
  for (let n = 1; n <= 10; n++) {
    const reservation = await recreate().reserveRecoveryIteration(bare, branch, key, expected);
    assert.equal(reservation.attempts, 1, "only the preceding recorded completion reset the budget");
    fs.writeFileSync(path.join(expected.clonePath, "work.txt"), "episode " + n + "\n");
    git(expected.clonePath, ["add", "work.txt"]);
    git(expected.clonePath, ["commit", "-m", "episode " + n]);
    await fixtureFetchTracking(cache, bare, expected.clonePath, branch, runId);
    const tip = git(expected.clonePath, ["rev-parse", "HEAD"]);
    retained.push({ clonePath: expected.clonePath, tip,
      head: fs.readFileSync(path.join(expected.clonePath, ".git", "HEAD")),
      bytes: fs.readFileSync(path.join(expected.clonePath, "work.txt")) });
    await cache.recordRecoveryCapture(bare, branch, key, expected, 1, tip);
    const successor = await cache.prepareRecoverySuccessor(bare, branch, key, expected, 1, aid(n + 1));
    assert.notEqual(successor.path, expected.clonePath);
    expected = { runId, clonePath: successor.path, attemptId: aid(n + 1) };
    assert.equal(journal().recovery.attempts, 1, "capture and handoff keep the charged budget");
    assert.equal(journal().recovery.deadline, reservation.deadline);
    await assert.rejects(cache.completeRecoveryEpisode(bare, branch, key, expected,
      { settled: true, processedEvents: 0, attemptId: aid(n + 1) }), /evidence/);
    assert.equal(journal().recovery.attempts, 1, "unrecorded processing cannot reset");
    await cache.completeRecoveryEpisode(bare, branch, key, expected,
      { settled: true, processedEvents: 1, attemptId: aid(n + 1) });
    assert.equal(journal().recovery, undefined);
  }
  const discovered = (await recreate().discoverRetainedRecovery(fx.originPath, branch, key, runId))!.journal;
  assert.equal(discovered.retainedSources!.length, 10);
  for (const saved of retained) {
    assert.ok(discovered.retainedSources!.some(s => s.clonePath === saved.clonePath));
    assert.deepEqual(fs.readFileSync(path.join(saved.clonePath, ".git", "HEAD")), saved.head);
    assert.deepEqual(fs.readFileSync(path.join(saved.clonePath, "work.txt")), saved.bytes);
    assert.equal(git(bare, ["rev-parse", "refs/uzi-recovery-episode/" + runId + "/" + saved.tip]), saved.tip);
  }
});

for (const stage of ["captured", "handoff", "completed"] as const) {
  test("explicit discard tracks distinct retry captures through " + stage + " and retries failed pin deletion", async () => {
    const first = await cache.reserveRecoveryIteration(bare, branch, key, source);
    const tipA = await capture();
    await cache.recordRecoveryCapture(bare, branch, key, source, 1, tipA);
    const retry = await recreate().reserveRecoveryIteration(bare, branch, key, source);
    assert.equal(retry.attempts, 2);
    assert.equal(retry.deadline, first.deadline);
    fs.writeFileSync(path.join(source.clonePath, "work.txt"), "second capture\n");
    git(source.clonePath, ["add", "work.txt"]);
    git(source.clonePath, ["commit", "-m", "distinct retry capture"]);
    await fixtureFetchTracking(cache, bare, source.clonePath, branch, runId);
    const tipB = git(source.clonePath, ["rev-parse", "HEAD"]);
    assert.notEqual(tipA, tipB);
    await cache.recordRecoveryCapture(bare, branch, key, source, 2, tipB);
    let expected: { runId: string; clonePath: string; attemptId?: string } = source;
    if (stage !== "captured") {
      const successor = await cache.prepareRecoverySuccessor(bare, branch, key, source, 2, aid(2));
      expected = { runId, clonePath: successor.path, attemptId: aid(2) };
      assert.equal(journal().recovery.attempts, 2);
      if (stage === "completed") await cache.completeRecoveryEpisode(bare, branch, key, expected,
        { settled: true, processedEvents: 1, attemptId: aid(2) });
    }
    const pins: [string, string] = [
      "refs/uzi-recovery-episode/" + runId + "/" + tipA,
      "refs/uzi-recovery-episode/" + runId + "/" + tipB,
    ];
    const sibling = "refs/uzi-recovery-episode/" + runId + "-sibling/" + tipA;
    const unrelated = "refs/keep/" + tipB;
    git(bare, ["update-ref", sibling, tipA]);
    git(bare, ["update-ref", unrelated, tipB]);
    for (const [i, pin] of pins.entries()) assert.equal(git(bare, ["rev-parse", pin]), [tipA, tipB][i]);
    const before = journal();
    const lock = path.join(bare, ...pins[1].split("/")) + ".lock";
    fs.writeFileSync(lock, "");
    await assert.rejects(recreate().discardRetainedRecovery(bare, branch, key, expected, true));
    assert.deepEqual(journal(), before, "failed cleanup retains trusted attribution");
    assert.equal(git(bare, ["rev-parse", pins[1]]), tipB);
    fs.unlinkSync(lock);
    await recreate().discardRetainedRecovery(bare, branch, key, expected, true);
    for (const pin of pins) assert.throws(() => git(bare, ["rev-parse", "--verify", pin]));
    assert.equal(git(bare, ["config", "uzi-recovery." + branch + ".clone"]), "");
    assert.equal(git(bare, ["rev-parse", sibling]), tipA);
    assert.equal(git(bare, ["rev-parse", unrelated]), tipB);
    assert.equal(fs.readFileSync(path.join(source.clonePath, "work.txt"), "utf8"), "second capture\n");
    assert.equal(fs.readFileSync(path.join(expected.clonePath, "work.txt"), "utf8"), "second capture\n");
    assert.equal(await cache.recoveryAttemptMode(fx.originPath, key), true);
  });
}

test("discard refuses malformed or changed run pin identity before deleting any pins or attribution", async () => {
  const { expected, tip } = await ready();
  const before = journal();
  const good = "refs/uzi-recovery-episode/" + runId + "/" + tip;
  const parent = git(bare, ["rev-parse", tip + "^"]);
  for (const bad of ["refs/uzi-recovery-episode/" + runId + "/unexpected",
    "refs/uzi-recovery-episode/" + runId + "/" + parent,
    "refs/uzi-recovery-episode/" + runId + "/nested/" + tip]) {
    git(bare, ["update-ref", bad, tip]);
    await assert.rejects(cache.discardRetainedRecovery(bare, branch, key, expected, true), /pin identity/);
    assert.deepEqual(journal(), before);
    assert.equal(git(bare, ["rev-parse", good]), tip);
    assert.equal(git(bare, ["rev-parse", bad]), tip);
    git(bare, ["update-ref", "-d", bad, tip]);
  }
});

test("explicit owner discard keeps paths intact and per-key attempt mode outlives cleared journal", async () => {
  assert.equal(await cache.recoveryAttemptMode(fx.originPath, key), false);
  const { expected, tip } = await ready();
  assert.equal(await cache.recoveryAttemptMode(fx.originPath, key), true);
  await cache.discardRetainedRecovery(bare, branch, key, expected, true);
  assert.equal(git(bare, ["config", `uzi-recovery.${branch}.clone`]), "");
  assert.equal(await recreate().discoverRetainedRecovery(fx.originPath, branch, key, runId), undefined);
  assert.equal(await recreate().recoveryAttemptMode(fx.originPath, key), true);
  assert.ok(fs.existsSync(source.clonePath));
  assert.ok(fs.existsSync(expected.clonePath));
  assert.throws(() => git(bare, ["rev-parse", "--verify", `refs/uzi-recovery-episode/${runId}/${tip}`]));
  await assert.rejects(cache.createOrAttachRunnerClone(bare, 2512, noProofReseed, runId), /fresh attempt/);
  const clone = await cache.createOrAttachRunnerClone(bare, 2512, noProofReseed, runId, true, undefined, {
    attemptId: aid(3), isLive: () => false, beforeSeed: async () => {}, quiescent: async () => false,
  });
  assert.ok(clone.path.endsWith(".attempt-" + aid(3)));
});
