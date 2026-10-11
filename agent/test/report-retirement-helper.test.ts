import { it } from "node:test";
import assert from "node:assert/strict";
import { EventEmitter } from "node:events";
import { PassThrough } from "node:stream";
import type { ChildProcess } from "node:child_process";
import fs from "node:fs/promises";
import path from "node:path";
import os from "node:os";
import { reapRunProcesses, runnerHelperTmp, type ScanRequest } from "../src/run-quiescence.js";
const request: ScanRequest = { mode: "capture", targetUid: process.getuid?.() ?? 0,
  targetKey: "fixture", targetPaths: ["/fixture"], liveMarkers: [], liveRoots: [], workerNonce: "fixture" };
function deferred() {
  let resolve!: () => void;
  const promise = new Promise<void>(r => { resolve = r; });
  return { promise, resolve };
}
const tick = () => new Promise<void>(r => setImmediate(r));
function fakeChild() {
  return Object.assign(new EventEmitter(), { stdin: new PassThrough(), stdout: new PassThrough(),
    stderr: new PassThrough(), pid: undefined }) as unknown as ChildProcess;
}
for (const failure of ["cancel", "expiry", "error", "overflow", "cleanup cancel"] as const) {
  it(`report helper ${failure} holds proof until CLOSE and cleanup settlement`, async () => {
    const child = fakeChild(), abort = new AbortController();
    const entered = deferred(), release = deferred();
    const budget = { signal: abort.signal, deadline: Date.now() + 5000 };
    let settled = false, spawned = 0;
    const pending = reapRunProcesses(request, {
      viaHelper: true, reportBudget: budget, helperTimeoutMs: failure === "expiry" ? 10 : 5000,
      spawnHelper: () => { spawned++; return child; },
      helperTmp: {
        make: received => { assert.equal(received, budget); return "/fixture/private"; },
        remove: async (_dir, received) => {
          assert.notEqual(received, budget); assert.equal(received?.signal.aborted, false);
          entered.resolve();
          if (received) received.deadline = Date.now() - 1;
          await release.promise;
        },
      },
    }).then(result => { settled = true; return result; });
    await tick();
    assert.equal(spawned, 1);
    if (failure === "cancel") abort.abort();
    if (failure === "error") child.emit("error", new Error("fixture helper error"));
    if (failure === "overflow") child.stdout!.emit("data", Buffer.alloc(1024 * 1024 + 1));
    if (failure === "expiry") await new Promise(r => setTimeout(r, 30));
    if (failure === "cleanup cancel") child.stdout!.emit("data", Buffer.from(JSON.stringify({
      state: "quiescent", processes: [], killed: [], detail: "",
    })));
    await tick();
    assert.equal(settled, false, "failure is not child settlement");
    child.emit("close", 0, null);
    await entered.promise;
    if (failure === "cleanup cancel") abort.abort();
    assert.equal(settled, false, "cleanup owns the proof boundary");
    release.resolve();
    assert.equal((await pending).state, "unverified");
  });
}
it("stalled setup settles and cleans up without late helper authorization", async () => {
  const entered = deferred(), release = deferred(), cleanup = deferred(), finish = deferred();
  const abort = new AbortController();
  const budget = { signal: abort.signal, deadline: Date.now() + 5000 };
  let spawned = 0, settled = false;
  const pending = reapRunProcesses(request, {
    viaHelper: true, reportBudget: budget,
    spawnHelper: () => { spawned++; throw new Error("late spawn"); },
    helperTmp: {
      make: async received => {
        assert.equal(received, budget); entered.resolve(); await release.promise; return "/fixture/private";
      },
      remove: async (_dir, received) => {
        assert.notEqual(received, budget); assert.equal(received?.signal.aborted, false);
        cleanup.resolve(); await finish.promise;
      },
    },
  }).then(result => { settled = true; return result; });
  await entered.promise; abort.abort(); await tick();
  assert.equal(settled, false);
  release.resolve(); await cleanup.promise;
  assert.equal(settled, false);
  finish.resolve();
  assert.equal((await pending).state, "unverified");
  assert.equal(spawned, 0);
});
it("expired budget starts neither setup nor scan", async () => {
  let calls = 0;
  const result = await reapRunProcesses(request, {
    viaHelper: true, reportBudget: { signal: new AbortController().signal, deadline: Date.now() - 1 },
    helperTmp: { make: () => { calls++; return "/fixture"; }, remove: () => { calls++; } },
    spawnHelper: () => { calls++; throw new Error("unexpected spawn"); },
  });
  assert.equal(result.state, "unverified");
  assert.equal(calls, 0);
});
for (const fault of ["cancel", "expiry"] as const) {
  it(`production runnerHelperTmp removes owned directory after report ${fault}`, async t => {
    const base = await fs.mkdtemp(path.join(os.tmpdir(), "report-helper-"));
    t.after(() => fs.rm(base, { recursive: true, force: true }));
    const child = fakeChild(), abort = new AbortController();
    const budget = { signal: abort.signal, deadline: Date.now() + 5000 };
    let owned = "";
    const ready = deferred();
    const wrap = (command: string, args: string[]) => ({ command, args });
    const pending = reapRunProcesses(request, {
      viaHelper: true, reportBudget: budget,
      helperTmp: {
        make: async received => {
          owned = await runnerHelperTmp(base, wrap, { budget: received }).make();
          return owned;
        },
        remove: (dir, received) => runnerHelperTmp(base, wrap, { budget: received }).remove(dir),
      },
      spawnHelper: () => { ready.resolve(); return child; },
    });
    await ready.promise;
    await fs.stat(owned);
    if (fault === "cancel") abort.abort();
    else budget.deadline = Date.now() - 1;
    child.stdout!.emit("data", Buffer.from(JSON.stringify({ state: "quiescent", processes: [], killed: [], detail: "" })));
    child.emit("close", 0, null);
    assert.equal((await pending).state, "unverified");
    await assert.rejects(fs.stat(owned), { code: "ENOENT" });
  });
}
it("synchronous report cleanup failure settles as unverified", async () => {
  const child = fakeChild();
  const pending = reapRunProcesses(request, {
    viaHelper: true, reportBudget: { signal: new AbortController().signal, deadline: Date.now() + 5000 },
    spawnHelper: () => child,
    helperTmp: { make: () => "/fixture/private", remove: () => { throw new Error("cleanup refused"); } },
  });
  await tick();
  child.stdout!.emit("data", Buffer.from(JSON.stringify({ state: "quiescent", processes: [], killed: [], detail: "" })));
  child.emit("close", 0, null);
  assert.equal((await pending).state, "unverified");
});
