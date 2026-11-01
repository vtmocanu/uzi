import assert from "node:assert/strict";
import { randomUUID } from "node:crypto";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import net from "node:net";
import path from "node:path";
import { it, type TestContext } from "node:test";
import { MaintenanceBudget } from "../src/maintenance-budget.js";
import type { HandoffRetirementInput } from "../src/recovery.js";
import type { RunRunner } from "../src/runner.js";
import { noProofReseed } from "./helpers.js";
import { client, fakeGitlab, fx, git, gitlabClaim, installHarness, runnerWith } from "./runner-harness.js";

installHarness();

type BareLocks = { withLock<T>(key: string, fn: () => Promise<T>, signal?: AbortSignal): Promise<T> };
type HandoffRunner = {
  authorityWait(...args: unknown[]): { signal?: AbortSignal; timeoutMs: number };
  recovery: RunRunner["recovery"];
  reclaimTerminalOrphan(...args: unknown[]): Promise<void>;
};

function deferred() {
  let resolve!: () => void;
  const promise = new Promise<void>(r => { resolve = r; });
  return { promise, resolve };
}

/** A watchdog for the regression only, never a substitute for cancelling a production operation. */
async function rejectedWhileHeld(attempt: Promise<void>): Promise<Error> {
  let timer: ReturnType<typeof setTimeout> | undefined;
  try {
    return await Promise.race([
      attempt.then(() => { throw new Error("reclaim unexpectedly succeeded"); }, error => error as Error),
      new Promise<never>((_resolve, reject) => {
        timer = setTimeout(() => reject(new Error("claimant remained queued behind the held bare lock")), 1000);
      }),
    ]);
  } finally { clearTimeout(timer); }
}

async function fixture(t: TestContext, fenced = true) {
  const branch = "agent/issue-3689", owner = randomUUID(), claimant = randomUUID();
  const bare = await git.ensureClone(fx.originPath);
  const clone = await git.runnerCloneForBranch(bare, branch, "agent-issue-3689", noProofReseed, owner);
  await git.markRecoveryCapture(bare, clone.path, branch, owner);
  const env = { ...Object.fromEntries(Object.entries(process.env).filter(([key]) => !key.startsWith("GIT_"))),
    GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" };
  const config = (...args: string[]) => execFileSync("git", ["-C", bare, "config", ...args], { env, encoding: "utf8" }).trim();
  const key = `uzi-recovery.${branch}.clone`;
  const fence = { version: 1, sourceUseEpoch: randomUUID(), sourceCoreDigest: "a".repeat(64) };
  if (fenced) config("--local", key, JSON.stringify({ ...JSON.parse(config("--get", key)), completionSourceFence: fence }));
  const previous = (git as unknown as { authorityRequired: boolean }).authorityRequired;
  Object.assign(git, { authorityRequired: true });
  t.after(() => { Object.assign(git, { authorityRequired: previous }); });
  fs.writeFileSync(path.join(clone.path, "owner.txt"), "retained owner bytes\n");
  t.mock.method(client, "getRunOrphanClassification", async () => ({
    status: "completed", repo_id: "r1", kind: "mr_rework", issue_iid: null,
    branch: null, pipeline_ref: branch, pipeline_id: null,
  }));
  const runner = runnerWith(() => { throw new Error("no executor may start"); }, fakeGitlab().gitlab) as unknown as HandoffRunner;
  const cancel = new AbortController(), lifecycle = new AbortController();
  let cancelled = false;
  const flight = {
    runId: claimant, claimGeneration: 1, cancel,
    steering: {
      claimFence: () => undefined, isCancelled: () => cancelled,
      lifecycleSignal: () => lifecycle.signal, terminalLifecycleSignal: () => lifecycle.signal,
    },
  };
  const realWait = runner.authorityWait.bind(runner);
  t.mock.method(runner, "authorityWait", (...args: unknown[]) => ({ ...realWait(...args), timeoutMs: 50 }));
  const locks = git as unknown as BareLocks;
  const opened = deferred(), release = deferred();
  let holder: Promise<void> | undefined;
  const hold = async () => {
    holder = locks.withLock(bare, async () => { opened.resolve(); await release.promise; });
    await opened.promise;
  };
  const journal = config("--get", key);
  const attempt = () => MaintenanceBudget.runAttempt(`${claimant}:1`, () => {
    throw new Error("no cancellation budget has been observed in this attempt");
  }, () => runner.reclaimTerminalOrphan(bare, gitlabClaim(3689, { run_id: claimant, kind: "mr_rework", branch }),
    flight, clone.path, branch, owner, new Error("original orphan refusal")));
  return {
    runner, bare, hold, attempt,
    cancel: () => { cancelled = true; cancel.abort(); },
    release: async () => { release.resolve(); await holder; },
    assertUnchanged: () => {
      assert.equal(config("--get", key), journal, "the journal and fence are byte-identical");
      assert.equal(fs.readFileSync(path.join(clone.path, "owner.txt"), "utf8"), "retained owner bytes\n");
    },
  };
}

it("orphan journal reread: a spent authority wait stops while the bare lock remains held", async t => {
  const f = await fixture(t);
  let now = Date.now();
  t.mock.method(Date, "now", () => now);
  t.mock.method(f.runner.recovery, "retireCompletedSourceForHandoff", async (input: HandoffRetirementInput) => {
    assert.equal(MaintenanceBudget.current(), undefined, "attempt context alone supplies no finite budget");
    await f.hold();
    now += input.wait.timeoutMs! + 1;
    return { kind: "pending", why: "authority_busy" };
  });
  const read = t.mock.method(git, "readOrphanJournalFence");
  const attempt = f.attempt();
  attempt.catch(() => undefined);
  try {
    assert.equal((await rejectedWhileHeld(attempt)).constructor.name, "RetainedRecoveryStop");
    assert.equal(read.mock.callCount(), 1, "an expired optional reread never joins the bare-lock queue");
    f.assertUnchanged();
  } finally { await f.release(); await attempt.catch(() => undefined); }
  assert.equal(read.mock.callCount(), 1, "no read executes later after the holder releases");
});

async function observation(pending: Promise<void>, label: string): Promise<void> {
  let timer: ReturnType<typeof setTimeout> | undefined;
  try {
    await Promise.race([pending, new Promise<never>((_resolve, reject) => {
      timer = setTimeout(() => reject(new Error(label)), 5000);
    })]);
  } finally { clearTimeout(timer); }
}

for (const point of ["initial", "unfenced reread"] as const) {
  it(`orphan journal child: ${point} cancellation reaches and joins the subprocess before B releases`, async t => {
    const f = await fixture(t, point === "initial");
    if (point === "unfenced reread") {
      t.mock.method(git, "terminalRetainedSnapshot", async () => { throw new Error("injected baseline snapshot refusal"); });
    }
    const ready = deferred(), aborted = deferred();
    let peer: net.Socket | undefined;
    let pid = 0;
    const server = net.createServer(socket => {
      peer = socket;
      socket.on("error", () => undefined); // A killed child may reset its test-only control socket.
      let buffered = "";
      socket.on("data", chunk => {
        buffered += chunk.toString();
        const lines = buffered.split("\n");
        buffered = lines.pop()!;
        for (const line of lines) {
          if (/^READY [1-9][0-9]*$/.test(line)) { pid = Number(line.slice(6)); ready.resolve(); }
          if (line === "ABORT") aborted.resolve();
        }
      });
    });
    await new Promise<void>((resolve, reject) => {
      server.once("error", reject);
      server.listen(0, "127.0.0.1", resolve);
    });
    const port = (server.address() as net.AddressInfo).port;
    // The controlled stand-in runs through the REAL execScoped child launcher. It holds its lifetime after SIGTERM
    // until the parent releases it (or execScoped's existing kill grace expires). No shell or real Git is stalled.
    const script = `const net = require('node:net');
const peer = net.connect(Number(process.argv[1]), '127.0.0.1');
process.on('SIGTERM', () => peer.write('ABORT\\n'));
peer.on('connect', () => peer.write('READY ' + process.pid + '\\n'));
peer.on('data', data => { if (data.toString().includes('EXIT')) process.exit(0); });`;
    const realRead = git.readOrphanJournalFence.bind(git);
    let reads = 0, selected = false;
    t.mock.method(git, "readOrphanJournalFence", async (...args: Parameters<typeof realRead>) => {
      selected = ++reads === (point === "initial" ? 1 : 2);
      try { return await realRead(...args); } finally { selected = false; }
    });
    type Exec = (command: string, args: string[], options: { env: NodeJS.ProcessEnv; timeout?: number }) =>
      Promise<{ stdout: string; stderr: string }>;
    const internals = git as unknown as { execScoped: Exec };
    const realExec = internals.execScoped.bind(git);
    let injected = false;
    t.mock.method(internals, "execScoped", async (command: string, args: string[], options: Parameters<Exec>[2]) => {
      if (selected && !injected && command === "git" && args.includes("config") && args.includes("--list")) {
        injected = true;
        return realExec(process.execPath, ["-e", script, String(port)], options);
      }
      return realExec(command, args, options);
    });
    const alive = () => { try { process.kill(pid, 0); return true; } catch { return false; } };
    const attempt = f.attempt();
    let finished = false;
    const settled = attempt.catch(error => error as Error).finally(() => { finished = true; });
    let competitor: Promise<void> | undefined;
    let childExitedAtAcquisition: boolean | undefined;
    try {
      await observation(ready.promise, "the journal read never launched the controlled child");
      assert.equal(alive(), true);
      competitor = (git as unknown as BareLocks).withLock(f.bare, async () => { childExitedAtAcquisition = !alive(); });
      f.cancel();
      await observation(aborted.promise, "cancellation never reached the running journal child");
      if (alive()) {
        assert.equal(finished, false, "the claimant joins the still-running child");
        assert.equal(childExitedAtAcquisition, undefined, "B stays held while the child is running");
      }
      if (peer?.writable) peer.end("EXIT\n");
      const error = await settled;
      assert.ok(error instanceof Error);
      assert.equal(error.constructor.name, "RetainedRecoveryCancelled");
      await competitor;
      assert.equal(childExitedAtAcquisition, true, "B never releases before the child exits");
      f.assertUnchanged();
    } finally {
      if (peer?.writable) peer.end("EXIT\n");
      await settled;
      await competitor;
      peer?.destroy();
      await new Promise<void>((resolve, reject) => server.close(error => error ? reject(error) : resolve()));
    }
  });
}

it("orphan journal reread: cancellation leaves the queued read inert and owner bytes unchanged", async t => {
  const f = await fixture(t);
  t.mock.method(f.runner.recovery, "retireCompletedSourceForHandoff", async () => {
    await f.hold();
    return { kind: "pending", why: "no_receipt" };
  });
  const queued = deferred();
  const real = git.readOrphanJournalFence.bind(git);
  let reads = 0;
  t.mock.method(git, "readOrphanJournalFence", async (...args: Parameters<typeof real>) => {
    if (++reads === 2) queued.resolve();
    return real(...args);
  });
  const internals = git as unknown as { readRecoveryCapture(...args: unknown[]): Promise<unknown> };
  const materialize = t.mock.method(internals, "readRecoveryCapture");
  const attempt = f.attempt();
  attempt.catch(() => undefined);
  try {
    await queued.promise;
    const before = materialize.mock.callCount();
    assert.ok(before > 0, "the spy observed the initial journal read, so an unchanged count is meaningful");
    f.cancel();
    assert.equal((await rejectedWhileHeld(attempt)).constructor.name, "RetainedRecoveryCancelled");
    assert.equal(materialize.mock.callCount(), before, "cancelled waiter never reads the journal");
    f.assertUnchanged();
    await f.release();
    await new Promise<void>(resolve => setImmediate(resolve));
    assert.equal(materialize.mock.callCount(), before, "the cancelled slot never runs after B releases");
  } finally { await f.release(); await attempt.catch(() => undefined); }
});

it("orphan journal reread: the remaining handoff allowance bounds an already queued read", async t => {
  const f = await fixture(t);
  t.mock.timers.enable({ apis: ["setTimeout", "Date"] });
  t.mock.method(f.runner.recovery, "retireCompletedSourceForHandoff", async () => {
    await f.hold();
    return { kind: "pending", why: "no_receipt" };
  });
  const queued = deferred();
  const real = git.readOrphanJournalFence.bind(git);
  let reads = 0;
  t.mock.method(git, "readOrphanJournalFence", async (...args: Parameters<typeof real>) => {
    if (++reads === 2) queued.resolve();
    return real(...args);
  });
  const internals = git as unknown as { readRecoveryCapture(...args: unknown[]): Promise<unknown> };
  const materialize = t.mock.method(internals, "readRecoveryCapture");
  const attempt = f.attempt();
  let rejected: unknown;
  const settled = attempt.catch(error => { rejected = error; });
  try {
    await queued.promise;
    t.mock.timers.tick(51);
    await new Promise<void>(resolve => setImmediate(resolve));
    assert.ok(rejected instanceof Error, "the remaining allowance expires before B releases");
    assert.equal(rejected.constructor.name, "RetainedRecoveryStop");
    assert.equal(reads, 2, "the optional reread started while allowance remained");
    const before = materialize.mock.callCount();
    assert.ok(before > 0, "the spy observed the initial journal read, so an unchanged count is meaningful");
    f.assertUnchanged();
    await f.release();
    await new Promise<void>(resolve => setImmediate(resolve));
    assert.equal(materialize.mock.callCount(), before, "the expired queue slot never materializes a journal later");
  } finally { t.mock.timers.reset(); await f.release(); await settled; }
});
