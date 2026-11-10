import assert from "node:assert/strict";
import { execFileSync, type ChildProcess } from "node:child_process";
import { EventEmitter } from "node:events";
import fs from "node:fs";
import fsp from "node:fs/promises";
import path from "node:path";
import { test, type TestContext } from "node:test";
import { CloneResidueBlockedError, GitCache } from "../src/git.js";
import type { AuthoritySettlement, GitAuthorityHooks } from "../src/git-authority-hooks.js";
import { makeTmp } from "./authority-test-support.js";
import { makeFixture } from "./fixture-repo.js";
import { noProofReseed, nullLogger } from "./helpers.js";

// issue #4066 - GitCache.pendingWork() is the shutdown barrier's view of the work GitCache still owes the authority
// after its caller returned: runner-uid deletes recorded as unsettled (until the late close AND the authority settle
// it triggers have both finished) and cleanupArchivedRecoveryPin continuations its caller's deadline abandoned (until
// the whole attempt, including its awaited settle, has settled). The authority is a stub whose settle is gated by the
// test, so "settled" is observable without a real store.

interface Gate { promise: Promise<void>; open(): void }
function gate(): Gate {
  let open!: () => void;
  const promise = new Promise<void>((resolve) => { open = resolve; });
  return { promise, open };
}

function stubHooks(settleGate: Gate): { hooks: GitAuthorityHooks; outcomes: unknown[] } {
  const outcomes: unknown[] = [];
  const settlement = (): AuthoritySettlement => ({
    settle: async (outcome) => { outcomes.push(outcome); await settleGate.promise; },
  });
  return {
    outcomes,
    hooks: {
      admitSourceUse: async () => undefined,
      beginBareMutation: async () => settlement(),
      assertBareClear: async () => undefined,
      beginDestructive: async () => settlement(),
    },
  };
}

const pending = <T>(promise: Promise<T>, ms = 100): Promise<"pending" | "settled"> =>
  Promise.race([promise.then(() => "settled" as const), new Promise<"pending">((resolve) => { setTimeout(() => resolve("pending"), ms).unref(); })]);

async function fixtureBare(t: TestContext): Promise<{ dataDir: string; bare: string; plain: GitCache; originPath: string }> {
  const fx = makeFixture();
  t.after(() => fx.cleanup());
  const dataDir = fs.realpathSync(fx.dataDir);
  const plain = new GitCache(dataDir, nullLogger(), undefined);
  const bare = await plain.ensureClone(fx.originPath);
  fs.chmodSync(path.dirname(bare), 0o700);
  fs.chmodSync(bare, 0o700);
  return { dataDir, bare, plain, originPath: fx.originPath };
}

test("issue #4066 pendingWork: an unsettled runner-uid delete is pending until its late close AND its authority settle have finished", async (t) => {
  const { dataDir, bare, plain } = await fixtureBare(t);
  const BRANCH = "agent/issue-5301";
  await plain.runnerCloneForBranch(bare, BRANCH, "issue-5301", noProofReseed, "run-4066-a");

  const settleGate = gate();
  const { hooks, outcomes } = stubHooks(settleGate);
  const child = new EventEmitter() as unknown as ChildProcess;
  Object.assign(child, { pid: 910_066, stderr: null });
  const git = new GitCache(dataDir, nullLogger(), undefined, {
    authority: hooks, authorityRequired: true,
    retentionDelete: { split: true, spawn: () => child, kill: () => false, timeoutMs: 20, closeWaitMs: 50 },
  });

  assert.equal(await pending(git.pendingWork(), 20), "settled", "nothing owed: resolves at once");
  assert.equal(git.unsettledDeleteCount(), 0);

  await assert.rejects(git.runnerCloneForBranch(bare, BRANCH, "issue-5301", noProofReseed, "run-4066-a"), (error: unknown) => {
    assert.ok(error instanceof CloneResidueBlockedError, String(error));
    return true;
  });
  assert.equal(git.unsettledDeleteCount(), 1, "the delete is recorded as unsettled");
  const work = git.pendingWork();
  assert.equal(await pending(work), "pending", "the child has not closed");

  child.emit("close", null, "SIGKILL"); // the late close: the settle it starts is gated by the test
  assert.equal(await pending(work), "pending", "closed, but the settlement has not settled yet");
  assert.equal(git.unsettledDeleteCount(), 1, "the path stays counted until the settle settled");
  assert.deepEqual(outcomes.length, 1, "the late close settled the authority record exactly once");

  settleGate.open();
  await work;
  assert.equal(git.unsettledDeleteCount(), 0);
  assert.equal(await pending(git.pendingWork(), 20), "settled");
});

test("issue #4066 pendingWork: a pin continuation the caller's deadline abandoned is pending until its awaited settle has settled", async (t) => {
  const { dataDir, bare, plain } = await fixtureBare(t);
  const sourceSha = execFileSync("git", ["-C", bare, "rev-parse", "refs/remotes/origin/main"], { encoding: "utf8" }).trim();
  const runId = "40664066-9999-aaaa-bbbb-cccccccccccc";
  assert.equal(await plain.anchorRecoveryHead(bare, runId, 1, sourceSha), true);

  // The real `git update-ref --stdin` child of the cleanup sits behind a FIFO until the test lets it run.
  const dir = makeTmp(t, "git-pending-work-");
  const armedFifo = path.join(dir, "armed");
  const goFifo = path.join(dir, "go");
  for (const fifo of [armedFifo, goFifo]) execFileSync("mkfifo", [fifo]);
  const wrapDir = makeTmp(t, "git-pending-wrap-");
  fs.writeFileSync(path.join(wrapDir, "git"), [
    "#!/bin/sh",
    "hit=0; stdin=0",
    'for a in "$@"; do [ "$a" = update-ref ] && hit=1; [ "$a" = --stdin ] && stdin=1; done',
    'if [ "$hit$stdin" = 11 ]; then',
    `  echo armed > '${armedFifo}'; read _ < '${goFifo}'`,
    "fi",
    'exec /usr/bin/git "$@"', "",
  ].join("\n"), { mode: 0o755 });
  const previousPath = process.env.PATH;
  process.env.PATH = `${wrapDir}:${previousPath}`;
  t.after(() => { process.env.PATH = previousPath; });

  const settleGate = gate();
  const { hooks, outcomes } = stubHooks(settleGate);
  const git = new GitCache(dataDir, nullLogger(), undefined, { authority: hooks, authorityRequired: true });
  const cleanup = git.cleanupArchivedRecoveryPin({
    runId, generation: 1, sourceSha, recoveryPinBareDir: path.basename(bare), isEligible: () => true, deadline: Date.now() + 400,
  });
  await fsp.readFile(armedFifo); // the child exists and is held
  assert.equal(git.pendingArchivedPinContinuations(), 0, "the attempt is still within its caller's bound");
  await new Promise((resolve) => setTimeout(resolve, 600)); // the caller's deadline passes with the child in flight
  await fsp.writeFile(goFifo, "go\n"); // git sees prepare then abort and exits by itself
  assert.deepEqual(await cleanup, { state: "retained", reason: "deadline" }, "the caller got its bounded answer");

  assert.equal(git.pendingArchivedPinContinuations(), 1, "the abandoned attempt is still running: its settle is gated");
  const work = git.pendingWork();
  assert.equal(await pending(work), "pending");
  assert.equal(outcomes.length, 1, "the continuation reached its settle");

  settleGate.open();
  await work;
  assert.equal(git.pendingArchivedPinContinuations(), 0, "counted until the whole attempt settled");
  assert.equal(await pending(git.pendingWork(), 20), "settled");
});

test("issue #4066 pendingWork: an attempt that finishes within its bound is never counted", async (t) => {
  const { dataDir, bare, plain } = await fixtureBare(t);
  const sourceSha = execFileSync("git", ["-C", bare, "rev-parse", "refs/remotes/origin/main"], { encoding: "utf8" }).trim();
  const runId = "40664066-dddd-eeee-ffff-000000000000";
  assert.equal(await plain.anchorRecoveryHead(bare, runId, 1, sourceSha), true);
  const settleGate = gate();
  settleGate.open();
  const { hooks } = stubHooks(settleGate);
  const git = new GitCache(dataDir, nullLogger(), undefined, { authority: hooks, authorityRequired: true });
  const result = await git.cleanupArchivedRecoveryPin({
    runId, generation: 1, sourceSha, recoveryPinBareDir: path.basename(bare), isEligible: () => true,
  });
  assert.equal(result.state, "deleted");
  assert.equal(git.pendingArchivedPinContinuations(), 0);
  assert.equal(await pending(git.pendingWork(), 20), "settled");
});
