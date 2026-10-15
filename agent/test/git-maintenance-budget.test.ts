import test from "node:test";
import assert from "node:assert/strict";
import childProcess from "node:child_process";
import { syncBuiltinESMExports } from "node:module";
import { EventEmitter } from "node:events";
import { PassThrough, Readable } from "node:stream";
import { GitCache } from "../src/git.js";
import { MaintenanceBudget } from "../src/maintenance-budget.js";
import { nullLogger } from "./helpers.js";

const cache = () => new GitCache("/unused-maintenance-test", nullLogger());
const tick = () => new Promise<void>(resolve => setImmediate(resolve));

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
