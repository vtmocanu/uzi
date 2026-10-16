import test from "node:test";
import assert from "node:assert/strict";
import childProcess from "node:child_process";
import { syncBuiltinESMExports } from "node:module";
import { EventEmitter } from "node:events";
import { PassThrough, Readable, Writable } from "node:stream";
import type { BoundaryProcessHandle } from "../src/harness.js";
import { GitCache } from "../src/git.js";
import { MaintenanceBudget } from "../src/maintenance-budget.js";
import { nullLogger } from "./helpers.js";

const cache = () => new GitCache("/unused-maintenance-test", nullLogger());
const tick = () => new Promise<void>(resolve => setImmediate(resolve));

test("production monotonic clock runs native buffered Git with an integer timeout", async t => {
  const budget = new MaintenanceBudget();
  t.after(() => budget.dispose());
  const output = await MaintenanceBudget.run(budget, () => cache()["runGit"](undefined, ["--version"]));
  assert.match(output, /^git version /);
});

for (const path of ["buffered", "streaming", "runner stdin"] as const) {
  test(`late supervised ${path} spawn stops and joins without delivering stdin`, async t => {
    const git = cache();
    let now = 0, inputs = 0, cancelled = 0, settled = false;
    let close!: (result: { code: number }) => void;
    const completed = new Promise<{ code: number }>(resolve => { close = resolve; });
    const stdin = new Writable({ write(_chunk, _encoding, done) { inputs++; done(); } });
    const budget = new MaintenanceBudget(600_000, () => now);
    t.after(() => budget.dispose());
    const spawner = async (): Promise<BoundaryProcessHandle> => {
      now = 600_000; // Clock expires during the await, before its timer callback can run.
      return { stdin, stdout: Readable.from([]), stderr: Readable.from([]), completed,
        cancel: async () => { cancelled++; await completed; } };
    };
    const operation = MaintenanceBudget.run(budget, () => git.withBoundaryProcessSpawner(
      spawner, new AbortController().signal, async () => {
        if (path === "buffered") return git["execScoped"]("git", ["--version"],
          { env: process.env, input: "review-input" });
        if (path === "streaming") return git["spawnGit"]("/unused", ["version"], "review-input");
        const producer = await git["spawnGitAsRunnerWithStdin"]("/unused", ["version"]);
        producer.stdin.end("review-input");
        return producer;
      }));
    const result = operation.then(() => { settled = true; return undefined; },
      error => { settled = true; return error as unknown; });
    await tick();
    const beforeClose = { inputs, cancelled, settled };
    close({ code: 0 });
    const error = await result;
    assert.equal(beforeClose.inputs, 0, "no stdin may be delivered after the monotonic deadline");
    assert.equal(beforeClose.cancelled, 1, "the late handle is cancelled");
    assert.equal(beforeClose.settled, false, "the late handle is joined before returning");
    assert.match(String(error), /maintenance deadline/);
  });
}

for (const path of ["buffered", "streaming"] as const) {
  test(`supervised ${path} input admission does not yield after checking its budget`, async t => {
    let now = 0, armed = false, scheduled = false;
    const inputs: number[] = [];
    const budget = new MaintenanceBudget(600_000, () => {
      if (armed && !scheduled) {
        scheduled = true;
        queueMicrotask(() => { now = 600_000; });
      }
      return now;
    });
    t.after(() => budget.dispose());
    const stdin = new Writable({ write(_chunk, _encoding, done) { inputs.push(now); done(); } });
    const git = cache();
    await MaintenanceBudget.run(budget, () => git.withBoundaryProcessSpawner(async () => {
      armed = true;
      return { stdin, stdout: Readable.from([]), stderr: Readable.from([]),
        completed: Promise.resolve({ code: 0 }), cancel: async () => {} };
    }, new AbortController().signal, async () => {
      try {
        if (path === "buffered") await git["execScoped"]("git", ["--version"], { env: process.env, input: "input" });
        else await git["spawnGit"]("/unused", ["version"], "input");
      } catch { /* expiry may refuse the eventual result; it never authorizes late input */ }
    }));
    assert.equal(scheduled, true);
    assert.equal(inputs.some(at => at >= 600_000), false, "stdin admission must precede expiry");
  });
}

test("expiry during pre-release join cannot return lock result authority", async t => {
  let now = 0;
  const budget = new MaintenanceBudget(600_000, () => now);
  t.after(() => budget.dispose());
  const git = cache();
  const action = MaintenanceBudget.run(budget, () => git.withBoundaryProcessSpawner(
    async () => { throw new Error("no spawn expected"); }, new AbortController().signal,
    () => git.withBareLock("repo", async () => "authority"),
    { beforeLockRelease: async () => { now = 600_000; } }));
  await assert.rejects(action, /maintenance deadline/);
});


test("queued ambient Git lock expires and never starts after holder releases", async t => {
  const git = cache();
  let release!: () => void, held!: () => void;
  const ready = new Promise<void>(resolve => { held = resolve; });
  const holder = git.withBareLock("repo", async () => { held(); await new Promise<void>(resolve => { release = resolve; }); });
  await ready;
  let now = 0, started = false;
  const budget = new MaintenanceBudget(100, () => now);
  t.after(() => budget.dispose());
  const waiter = MaintenanceBudget.run(budget, () => git.withBareLock("repo", async () => { started = true; }));
  const rejected = assert.rejects(waiter);
  now = 100;
  assert.throws(() => budget.check());
  await rejected;
  release();
  await holder;
  await git.withBareLock("repo", async () => {});
  assert.equal(started, false);
});

test("explicit recovery budget is ambient, nested and checked after action", async t => {
  const git = cache();
  let now = 0;
  const budget = new MaintenanceBudget(600_000, () => now);
  t.after(() => budget.dispose());
  await assert.rejects(git.withRecoveryOperation(new AbortController().signal, Infinity, async () => {
    assert.equal(MaintenanceBudget.current(), budget);
    now = 300_000;
    await git.withRecoveryOperation(new AbortController().signal, Infinity, async () => {
      assert.equal(MaintenanceBudget.current(), budget);
      now = 600_000;
    });
  }, budget), /maintenance deadline/);
});

test("supervised Git caps cumulative monotonic time independently of wall jumps", async t => {
  const git = cache();
  let now = 0, wall = 100;
  const caps: number[] = [];
  const budget = new MaintenanceBudget(600_000, () => now);
  t.after(() => budget.dispose());
  t.mock.method(Date, "now", () => wall);
  await git.withRecoveryOperation(new AbortController().signal, Infinity, () =>
    git.withBoundaryProcessSpawner(async request => {
      caps.push(request.timeoutMs!);
      return { stdin: null, stdout: Readable.from(["ok"]), stderr: null,
        completed: Promise.resolve({ code: 0 }), cancel: async () => {} };
    }, new AbortController().signal, async () => {
      await git.readBare("/unused", ["version"], { maxBytes: 100, timeoutMs: 1_000_000 });
      now = 300_000;
      wall = -9_000_000;
      await git.readBare("/unused", ["version"], { maxBytes: 100, timeoutMs: 1_000_000 });
      now = 600_000;
      wall = 9_000_000_000;
      await assert.rejects(git.readBare("/unused", ["version"], { maxBytes: 100 }), /maintenance deadline/);
    }), budget).catch(error => { assert.match(error.message, /maintenance deadline/); });
  assert.deepEqual(caps, [600_000, 300_000]);
});

test("no-budget recovery keeps wall deadline and post-action check", async t => {
  let wall = 100;
  t.mock.method(Date, "now", () => wall);
  await assert.rejects(cache().withRecoveryOperation(new AbortController().signal, 200, async () => {
    wall = 200;
  }), /recovery deadline/);
});

test("budgeted native reader error and abort wait for actual close", async t => {
  const git = cache();
  const kills: string[] = [];
  let escalate!: () => void;
  const realSetTimeout = globalThis.setTimeout;
  t.mock.method(globalThis, "setTimeout", (callback: () => void, delay?: number) => {
    if (delay === 1_000) escalate = callback;
    return realSetTimeout(callback, delay);
  });
  const child = Object.assign(new EventEmitter(), {
    stdin: new PassThrough(), stdout: new PassThrough(), stderr: new PassThrough(),
    exitCode: null, signalCode: null,
    kill: (signal?: string) => { kills.push(signal ?? "SIGTERM"); return true; },
  });
  t.mock.method(childProcess, "spawn", () => child);
  syncBuiltinESMExports();
  t.after(() => { t.mock.restoreAll(); syncBuiltinESMExports(); });
  let now = 0;
  const budget = new MaintenanceBudget(100, () => now);
  t.after(() => budget.dispose());
  let settled = false;
  const reading = MaintenanceBudget.run(budget, () => git.readBare("/unused", ["version"], { maxBytes: 100 }));
  const outcome = reading.then(() => { settled = true; }, () => { settled = true; });
  await tick();
  child.emit("error", new Error("native early error"));
  now = 100;
  assert.throws(() => budget.check());
  await tick();
  assert.equal(settled, false, "error and SIGTERM are not a join");
  assert.deepEqual(kills, ["SIGTERM"]);
  escalate();
  await tick();
  assert.deepEqual(kills, ["SIGTERM", "SIGKILL"]);
  assert.equal(settled, false, "SIGKILL request is not close either");
  child.stdout.destroy();
  child.stderr.destroy();
  child.emit("close", null);
  await outcome;
  assert.equal(settled, true);
});
