import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { PassThrough, Readable } from "node:stream";
import { getEventListeners } from "node:events";
import { GitCache, gitEnv } from "../src/git.js";
import type { BoundaryProcessHandle, BoundaryProcessRequest } from "../src/harness.js";
import { TickSpawner } from "../src/tick-spawner.js";
import { runnerPath, runnerTmpdir } from "../src/runner-uid.js";
import { nullLogger } from "./helpers.js";

const cap = 512 * 1024;
const git = new GitCache(process.cwd(), nullLogger());
const tick = (): Promise<void> => new Promise((resolve) => setImmediate(resolve));
function deferred<T>(): { promise: Promise<T>; resolve: (value: T) => void } {
  let resolve!: (value: T) => void;
  return { promise: new Promise<T>((r) => { resolve = r; }), resolve: (v) => resolve(v) };
}
function pipe(data?: Buffer): PassThrough {
  const stream = new PassThrough();
  if (data) stream.end(data);
  return stream;
}
function fake(stdout: Readable, overrides: Partial<BoundaryProcessHandle> = {}): BoundaryProcessHandle {
  return {
    stdin: null, stdout, stderr: pipe(Buffer.alloc(0)),
    completed: Promise.resolve({ code: 0 }), cancel: async () => {},
    ...overrides,
  };
}
function run(handle: BoundaryProcessHandle, signal = new AbortController().signal): Promise<Buffer> {
  return git.withBoundaryProcessSpawner(async () => handle, signal,
    () => git.readBoundedPlanningOutput(process.cwd(), [process.execPath], 2000));
}

describe("Unit2 bounded runner stdout transport (not source capture)", () => {
  it("requires a trusted boundary owner and absolute argv/cwd", async () => {
    await assert.rejects(git.readBoundedPlanningOutput(process.cwd(), [process.execPath]), /trusted boundary/);
    let spawned = false;
    await assert.rejects(git.withBoundaryProcessSpawner(async () => {
      spawned = true;
      return fake(pipe(Buffer.alloc(0)));
    }, new AbortController().signal, () => git.readBoundedPlanningOutput(process.cwd(), ["node"])), /absolute/);
    assert.equal(spawned, false);
  });

  it("reads at most cap+1 bytes with read(n), and awaits cancellation", async () => {
    const stdout = pipe(Buffer.alloc(cap * 3, 97));
    const original = stdout.read.bind(stdout);
    let total = 0;
    stdout.read = (size?: number): Buffer | null => {
      assert.ok(size !== undefined && size <= 16 * 1024);
      const chunk = original(size) as Buffer | null;
      if (chunk) total += chunk.length;
      return chunk;
    };
    const cancelled = deferred<void>();
    let calls = 0;
    let settled = false;
    const pending = run(fake(stdout, { cancel: async () => { calls++; await cancelled.promise; } }));
    const verdict = assert.rejects(pending, /exceeded 512 KiB/).then(() => { settled = true; });
    await tick();
    assert.equal(total, cap + 1);
    assert.equal(calls, 1);
    assert.equal(settled, false);
    assert.equal(stdout.listenerCount("data"), 0);
    cancelled.resolve();
    await verdict;
  });

  it("accepts exactly the cap but waits for whole-root completion", async () => {
    const completed = deferred<{ code: number }>();
    let settled = false;
    const pending = run(fake(pipe(Buffer.alloc(cap, 98)), { completed: completed.promise }))
      .then((output) => { settled = true; return output; });
    await tick();
    assert.equal(settled, false);
    completed.resolve({ code: 0 });
    assert.deepEqual(await pending, Buffer.alloc(cap, 98));
  });

  it("accepts a clean empty early EOF and refuses previously read output", async () => {
    const empty = pipe(Buffer.alloc(0));
    empty.resume();
    await tick();
    assert.equal(empty.readableEnded, true);
    assert.equal(empty.readableDidRead, false);
    assert.equal((await run(fake(empty))).length, 0);
    const consumed = pipe(Buffer.from("lost"));
    consumed.resume();
    await tick();
    let cancelled = false;
    await assert.rejects(run(fake(consumed, { cancel: async () => { cancelled = true; } })), /consumed/);
    assert.equal(cancelled, true);
  });

  it("cancels on collector error and fails closed on refused cleanup", async () => {
    const stdout = pipe();
    let cancelled = false;
    const pending = run(fake(stdout, { cancel: async () => {
      cancelled = true;
      throw new Error("owner refused reap");
    } }));
    const verdict = assert.rejects(pending, /cleanup failed/);
    await tick();
    stdout.destroy(new Error("pipe failed"));
    await verdict;
    assert.equal(cancelled, true);
  });

  it("keeps abort live after EOF until cleanup, and awaits cancel", async () => {
    const ac = new AbortController();
    const completed = deferred<{ code: number }>();
    const cancelled = deferred<void>();
    let called = false;
    let settled = false;
    const pending = run(fake(pipe(Buffer.alloc(0)), {
      completed: completed.promise,
      cancel: async () => { called = true; await cancelled.promise; completed.resolve({ code: 128 }); },
    }), ac.signal);
    const verdict = assert.rejects(pending, /aborted/).then(() => { settled = true; });
    await tick();
    ac.abort();
    await tick();
    assert.equal(called, true);
    assert.equal(settled, false);
    cancelled.resolve();
    await verdict;
    assert.equal(getEventListeners(ac.signal, "abort").length, 0);
  });

  it("bounds stderr too, and cancels rather than deadlocking on a full pipe", async () => {
    let cancelled = false;
    await assert.rejects(run(fake(pipe(Buffer.from("small stdout")), {
      stderr: pipe(Buffer.alloc(cap + 1)),
      cancel: async () => { cancelled = true; },
    })), /exceeded/);
    assert.equal(cancelled, true);
  });

  it("refuses premature close and nonzero exit without returning partial bytes", async () => {
    const stdout = pipe();
    let cancelled = false;
    const pending = run(fake(stdout, { cancel: async () => { cancelled = true; } }));
    const verdict = assert.rejects(pending, /closed before end/);
    await tick();
    stdout.destroy();
    await verdict;
    assert.equal(cancelled, true);
    await assert.rejects(run(fake(pipe(Buffer.from("partial")), {
      completed: Promise.resolve({ code: 1 }),
    })), /process failed/);
  });

  it("refuses an already aborted boundary before spawn", async () => {
    const ac = new AbortController();
    ac.abort();
    let spawned = false;
    await assert.rejects(git.withBoundaryProcessSpawner(async () => {
      spawned = true;
      return fake(pipe(Buffer.alloc(0)));
    }, ac.signal, () => git.readBoundedPlanningOutput(process.cwd(), [process.execPath])), /aborted/);
    assert.equal(spawned, false);
  });

  it("refuses success when whole-root completion rejects", async () => {
    let cancelled = false;
    await assert.rejects(run(fake(pipe(Buffer.from("unapproved")), {
      completed: Promise.reject(new Error("root not reaped")),
      cancel: async () => { cancelled = true; },
    })), /cleanup failed/);
    assert.equal(cancelled, true);
  });

  it("runs a real fast child under TickSpawner with command identity and pinned env", async () => {
    const ac = new AbortController();
    const owner = new TickSpawner({ signal: ac.signal, killGraceMs: 50 });
    let request!: BoundaryProcessRequest;
    const output = await git.withBoundaryProcessSpawner((req) => {
      request = req;
      return owner.spawn(req);
    }, ac.signal, () => git.readBoundedPlanningOutput(process.cwd(),
      [process.execPath, "-e", "process.stdout.write('fast stdout'); process.stderr.write('diagnostic')"], 2000));
    assert.equal(output.toString(), "fast stdout");
    assert.equal(request.identity, "command");
    assert.equal(request.cwd, process.cwd());
    assert.equal(request.env.PATH, runnerPath());
    assert.equal(request.env.TMPDIR, runnerTmpdir() ?? gitEnv().TMPDIR);
    for (const [key, value] of Object.entries(gitEnv())) {
      if (key !== "PATH" && key !== "TMPDIR") assert.equal(request.env[key], value, key);
    }
    await owner.settled();
    assert.deepEqual(owner.survivors(), []);
    assert.throws(() => process.kill(owner.pids()[0]!, 0), { code: "ESRCH" });
  });

  it("cancels/reaps a real overflow child instead of buffering its output", async () => {
    const ac = new AbortController();
    const owner = new TickSpawner({ signal: ac.signal, killGraceMs: 50 });
    await assert.rejects(git.withBoundaryProcessSpawner(owner.spawn, ac.signal,
      () => git.readBoundedPlanningOutput(process.cwd(), [process.execPath, "-e",
        "process.stdout.write(Buffer.alloc(2 * 1024 * 1024)); setInterval(() => {}, 1000)"], 2000)), /exceeded/);
    await owner.settled();
    assert.equal(owner.cancelledAny(), true);
    assert.deepEqual(owner.survivors(), []);
    assert.throws(() => process.kill(owner.pids()[0]!, 0), { code: "ESRCH" });
  });

  it("bounds a real silent child with a live deadline", async () => {
    const ac = new AbortController();
    const owner = new TickSpawner({ signal: ac.signal, killGraceMs: 50 });
    await assert.rejects(git.withBoundaryProcessSpawner(owner.spawn, ac.signal,
      () => git.readBoundedPlanningOutput(process.cwd(),
        [process.execPath, "-e", "setInterval(() => {}, 1000)"], 100)), /timed out|cleanup failed/);
    await owner.settled();
    assert.deepEqual(owner.survivors(), []);
    assert.throws(() => process.kill(owner.pids()[0]!, 0), { code: "ESRCH" });
  });
});
