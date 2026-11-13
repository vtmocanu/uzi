import assert from "node:assert/strict";
import { execFileSync, type ChildProcess } from "node:child_process";
import { EventEmitter } from "node:events";
import fs from "node:fs";
import fsp from "node:fs/promises";
import path from "node:path";
import { test, type TestContext } from "node:test";
import { CloneResidueBlockedError, GitCache } from "../src/git.js";
import type { AuthoritySettlement, GitAuthorityHooks } from "../src/git-authority-hooks.js";
import { fifoDir, makeTmp, manualClock, readFifoBounded, withinBound, writeFifoBounded, type ManualClock } from "./authority-test-support.js";
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

function stubHooks(settleGate: Gate): { hooks: GitAuthorityHooks; outcomes: unknown[]; settleEntered: Promise<void> } {
  const outcomes: unknown[] = [];
  const entered = gate();
  const settlement = (): AuthoritySettlement => ({
    settle: async (outcome) => { outcomes.push(outcome); entered.open(); await settleGate.promise; },
  });
  return {
    outcomes,
    settleEntered: entered.promise,
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

/** The wall-clock bound on every wait below. It only turns a hang into a named failure; nothing waits on it. */
const BOUND_MS = 60_000;

interface HeldPin {
  git: GitCache;
  clock: ManualClock;
  cleanup: ReturnType<GitCache["cleanupArchivedRecoveryPin"]>;
  outcomes: unknown[];
  settleGate: Gate;
  settleEntered: Promise<void>;
  go: string;
  /** Resolves once the cleanup's real `git update-ref --stdin` child is held; fails by name if the cleanup settles first. */
  armed: Promise<void>;
}

// issue #4091: the cleanup runs on a manual clock, so how long its pre-spawn git probes take on a loaded host cannot
// spend the deadline before the child exists. The real `git update-ref --stdin` child sits behind a FIFO until released.
async function startHeldPin(t: TestContext, runId: string, deadlineMs: number, stallFirstEligibleMs = 0): Promise<HeldPin> {
  const { dataDir, bare, plain } = await fixtureBare(t);
  const sourceSha = execFileSync("git", ["-C", bare, "rev-parse", "refs/remotes/origin/main"], { encoding: "utf8" }).trim();
  assert.equal(await plain.anchorRecoveryHead(bare, runId, 1, sourceSha), true);

  const fifos = fifoDir(t, "git-pending-work-", ["armed", "go"], { pidFile: true });
  const wrapDir = makeTmp(t, "git-pending-wrap-");
  fs.writeFileSync(path.join(wrapDir, "git"), [
    "#!/bin/sh",
    "hit=0; stdin=0",
    'for a in "$@"; do [ "$a" = update-ref ] && hit=1; [ "$a" = --stdin ] && stdin=1; done',
    'if [ "$hit$stdin" = 11 ]; then',
    `  echo $$ > '${fifos.pid}'; echo armed > '${fifos.path("armed")}'; read _ < '${fifos.path("go")}'`,
    "fi",
    'exec /usr/bin/git "$@"', "",
  ].join("\n"), { mode: 0o755 });
  const previousPath = process.env.PATH;
  process.env.PATH = `${wrapDir}:${previousPath}`;
  t.after(() => { process.env.PATH = previousPath; });

  const clock = manualClock();
  const settleGate = gate();
  const { hooks, outcomes, settleEntered } = stubHooks(settleGate);
  const git = new GitCache(dataDir, nullLogger(), undefined, { authority: hooks, authorityRequired: true, archivedPinClockForTest: clock });
  let eligibleCalls = 0;
  const cleanup = git.cleanupArchivedRecoveryPin({
    runId, generation: 1, sourceSha, recoveryPinBareDir: path.basename(bare), deadline: clock.now() + deadlineMs,
    isEligible: () => {
      // A pre-spawn phase that outlasts a real-time budget: only the first of the product's eligibility checks stalls.
      if (eligibleCalls++ === 0) for (const until = Date.now() + stallFirstEligibleMs; Date.now() < until;) { /* synchronous stall */ }
      return true;
    },
  });
  const settledFirst = cleanup.then((result): never => {
    throw new assert.AssertionError({ message: `cleanup settled before its update-ref child was armed: ${JSON.stringify(result)}` });
  });
  settledFirst.catch(() => undefined); // the cleanup settles at the end of every run; only the race below may observe it
  const armed = withinBound(readFifoBounded(fifos.path("armed"), BOUND_MS, "the armed FIFO").then(() => undefined),
    BOUND_MS, `the update-ref child was not armed within ${BOUND_MS}ms`, settledFirst);
  return { git, clock, cleanup, outcomes, settleGate, settleEntered, go: fifos.path("go"), armed };
}

async function abandonedPinIsPendingUntilSettled(t: TestContext, runId: string, deadlineMs: number, stallFirstEligibleMs = 0): Promise<void> {
  const held = await startHeldPin(t, runId, deadlineMs, stallFirstEligibleMs);
  const { git, clock, outcomes, settleGate } = held;
  await held.armed; // the child exists and is held
  assert.equal(git.pendingArchivedPinContinuations(), 0, "the attempt is still within its caller's bound");
  clock.advance(deadlineMs + 100); // the caller's deadline passes with the child in flight
  await writeFifoBounded(held.go, "go\n", BOUND_MS, "the go FIFO"); // git sees prepare then abort and exits by itself
  assert.deepEqual(await withinBound(held.cleanup, BOUND_MS, "the cleanup did not answer its caller"),
    { state: "retained", reason: "deadline" }, "the caller got its bounded answer");

  assert.equal(git.pendingArchivedPinContinuations(), 1, "the abandoned attempt is still running: its settle is gated");
  const work = git.pendingWork();
  assert.equal(await pending(work), "pending");
  await withinBound(held.settleEntered, BOUND_MS, "the continuation never reached its settle");
  assert.equal(outcomes.length, 1, "the continuation reached its settle");

  settleGate.open();
  await work;
  assert.equal(git.pendingArchivedPinContinuations(), 0, "counted until the whole attempt settled");
  assert.equal(await pending(git.pendingWork(), 20), "settled");
}

test("issue #4066 pendingWork: a pin continuation the caller's deadline abandoned is pending until its awaited settle has settled", async (t) => {
  await abandonedPinIsPendingUntilSettled(t, "40664066-9999-aaaa-bbbb-cccccccccccc", 20_000);
});

test("issue #4066 pendingWork controlled variant: a cleanup whose pre-spawn phase outlasts the wall-clock budget still reaches its held child", async (t) => {
  // The first isEligible call stalls 2.5 s of real time, past the 2 s budget the cleanup would have had on the real clock.
  await abandonedPinIsPendingUntilSettled(t, "40664066-9999-aaaa-bbbb-dddddddddddd", 2_000, 2_500);
});

test("issue #4066 pendingWork: the prepared-abort grace runs on the cleanup's clock", async (t) => {
  const held = await startHeldPin(t, "40664066-9999-aaaa-bbbb-eeeeeeeeeeee", 20_000);
  const { git, clock, outcomes } = held;
  await held.armed;
  clock.advance(20_100); // the deadline passes: the cleanup sends abort and starts its grace for the held child
  await clock.scheduled((ms) => ms === 5_000, 3_000); // PREPARED_ABORT_GRACE_MS, on the injected clock and not the real one
  assert.equal(git.pendingArchivedPinContinuations(), 1, "the held child keeps the abandoned attempt pending");
  clock.advance(5_001); // the grace expires with the child never released: it is cancelled
  assert.deepEqual(await withinBound(held.cleanup, 3_000, "the cleanup did not resolve within 3000ms of its grace expiring"),
    { state: "retained", reason: "deadline" });
  await withinBound(git.pendingWork(), BOUND_MS, "pendingWork did not settle after the cancelled child was joined");
  assert.equal(git.pendingArchivedPinContinuations(), 0);
  assert.equal(outcomes.length, 0, "a child ended by a signal is never joined, so nothing settled");
});

test("fifoDir teardown releases pending FIFO opens and removes its directory", async (t) => {
  let pendingRead!: Promise<string>;
  let pendingWrite!: Promise<string>;
  let dir!: string;
  await t.test("inner", (st) => {
    const fifos = fifoDir(st, "fifo-dir-selfcheck-", ["r", "w"]);
    dir = fifos.dir;
    // Neither open has a partner: each blocks until the inner test's teardown releases it.
    pendingRead = fsp.readFile(fifos.path("r")).then(() => "settled", () => "settled");
    pendingWrite = fsp.writeFile(fifos.path("w"), "x").then(() => "settled", () => "settled");
  });
  assert.deepEqual(await withinBound(Promise.all([pendingRead, pendingWrite]), 5_000, "a blocked FIFO open outlived its teardown"), ["settled", "settled"]);
  assert.equal(fs.existsSync(dir), false, "the directory is removed");
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
