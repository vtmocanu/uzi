import test from "node:test";
import assert from "node:assert/strict";
import { EventEmitter } from "node:events";
import { PassThrough } from "node:stream";
import childProcess, { type ChildProcess } from "node:child_process";
import { syncBuiltinESMExports } from "node:module";
import { runAgentHelper } from "../src/rmtree.js";
import { MaintenanceBudget } from "../src/maintenance-budget.js";
import { SinkGate } from "../src/sink-gate.js";
import { reapRunProcesses, runnerHelperTmp, quiesceRunAttempt, LiveAttemptRegistry, type ScanRequest, teardownDocker } from "../src/run-quiescence.js";
import http from "node:http";

const turn = () => new Promise<void>(resolve => setImmediate(resolve));
const request: ScanRequest = {
  mode: "own", targetUid: 12345, targetKey: "test", targetPaths: ["/test"],
  liveMarkers: [], liveRoots: [], workerNonce: "test",
};

test("expired sink waiter never acts; surviving FIFO and reentrancy retain custody", async t => {
  let now = 0;
  const budget = new MaintenanceBudget(100, () => now);
  t.after(() => budget.dispose());
  const gate = new SinkGate();
  const release = gate.tryAcquire(() => {})!;
  const order: number[] = [];
  const waiting = MaintenanceBudget.run(budget, () => gate.run(async () => { order.push(0); }));
  const rejected = assert.rejects(waiting, /maintenance deadline/);
  const second = gate.run(async () => { order.push(1); await gate.run(async () => { order.push(2); }); });
  const third = gate.run(async () => { order.push(3); });
  now = 100;
  assert.throws(() => budget.check());
  await rejected;
  assert.equal(gate.isHeld(), true);
  release();
  await Promise.all([second, third]);
  assert.deepEqual(order, [1, 2, 3]);
  assert.equal(gate.isHeld(), false);
});

test("expired newly acquired holder releases and late action cannot return authority", async t => {
  let now = 0;
  const budget = new MaintenanceBudget(100, () => now);
  t.after(() => budget.dispose());
  const gate = new SinkGate();
  const action = MaintenanceBudget.run(budget, () => gate.run(async () => "proof"));
  now = 100;
  await assert.rejects(action, /maintenance deadline/);
  assert.equal(gate.isHeld(), false);
});

test("late setup starts neither helper nor renewed private cleanup", async t => {
  let now = 0, spawns = 0, removals = 0;
  const budget = new MaintenanceBudget(100, () => now);
  t.after(() => budget.dispose());
  const result = await MaintenanceBudget.run(budget, () => reapRunProcesses(request, {
    viaHelper: true,
    helperTmp: { make: async () => { now = 100; return "/private-test"; }, remove: () => { removals++; } },
    spawnHelper: () => { spawns++; throw new Error("unexpected"); },
  }));
  assert.equal(result.state, "unverified");
  assert.equal(spawns, 0);
  assert.equal(removals, 0);
});

test("helper abort and error retain custody until actual close and reject late proof", async t => {
  let now = 0, settled = false;
  const budget = new MaintenanceBudget(100, () => now);
  t.after(() => budget.dispose());
  const child = Object.assign(new EventEmitter(), {
    pid: undefined, stdout: new PassThrough(), stderr: new PassThrough(), stdin: new PassThrough(),
  });
  const pending = MaintenanceBudget.run(budget, () => reapRunProcesses(request, {
    viaHelper: true,
    helperTmp: { make: () => "/private-test", remove: () => { throw new Error("expired cleanup"); } },
    spawnHelper: () => child as unknown as ChildProcess,
  })).then(result => { settled = true; return result; });
  await turn();
  now = 100;
  assert.throws(() => budget.check());
  child.emit("error", new Error("controlled error"));
  await turn();
  assert.equal(settled, false);
  child.stdout.write(JSON.stringify({ state: "quiescent", processes: [], killed: [], detail: "late" }) + "\n");
  child.emit("close", 0, null);
  assert.equal((await pending).state, "unverified");
});


for (const native of ["agent helper", "private tmp helper"] as const) {
  test(`budgeted native ${native} abort and error join actual close`, async t => {
    let now = 0, settled = false;
    const budget = new MaintenanceBudget(600_000, () => now);
    t.after(() => budget.dispose());
    const child = Object.assign(new EventEmitter(), {
      pid: undefined, stdout: new PassThrough(), stderr: new PassThrough(), stdin: new PassThrough(),
      kill: () => true,
    });
    t.mock.method(childProcess, "spawn", () => child as unknown as ReturnType<typeof childProcess.spawn>);
    syncBuiltinESMExports();
    t.after(() => { t.mock.restoreAll(); syncBuiltinESMExports(); });
    const wrap = () => ({ command: "controlled-helper", args: [] as string[] });
    const operation = MaintenanceBudget.run(budget, () => native === "agent helper"
      ? runAgentHelper(wrap, "void 0", [], 1000, "inert input")
      : runnerHelperTmp("/private-base", wrap, { makeMs: 1000 }).make());
    const outcome = operation.then(() => { settled = true; return "resolved"; },
      () => { settled = true; return "rejected"; });
    await turn();
    now = 600_000;
    assert.throws(() => budget.check());
    child.emit("error", new Error("controlled native helper error"));
    await turn();
    const beforeClose = settled;
    child.stdout.write(Buffer.from("/private-base/uzi-quiesce-controlled\n"));
    child.emit("close", 0, null);
    assert.equal(await outcome, "rejected");
    assert.equal(beforeClose, false, "an error or abort request is not proof the native helper closed");
  });
}


for (const native of ["agent helper", "private tmp helper"] as const) {
  test(`budgeted ${native} refuses spawn after wrapper consumes its operation allowance`, async t => {
    let now = 0, spawns = 0;
    const budget = new MaintenanceBudget(600_000, () => now);
    t.after(() => budget.dispose());
    t.mock.method(childProcess, "spawn", () => { spawns++; throw new Error("unexpected expired spawn"); });
    syncBuiltinESMExports();
    t.after(() => { t.mock.restoreAll(); syncBuiltinESMExports(); });
    const wrap = () => { now = 1000; return { command: "controlled-helper", args: [] as string[] }; };
    await assert.rejects(MaintenanceBudget.run(budget, () => native === "agent helper"
      ? runAgentHelper(wrap, "void 0", [], 1000)
      : runnerHelperTmp("/private-base", wrap, { makeMs: 1000 }).make()));
    assert.equal(spawns, 0, "a consumed per-operation cap cannot admit a helper");
  });
}

test("expired quiescence cannot return an absent process proof when scanning is skipped", async t => {
  let now = 0;
  const budget = new MaintenanceBudget(100, () => now);
  t.after(() => budget.dispose());
  const outcome = await MaintenanceBudget.run(budget, () => {
    now = 100;
    return quiesceRunAttempt({ mode: "own", attempt: undefined, cloneKey: "test", targetPaths: ["/test"],
      processes: false, dockerHost: undefined, registry: new LiveAttemptRegistry() });
  });
  assert.equal(outcome.process?.state, "unverified");
});


test("expiry during process rescan returns an unverified orchestration outcome", async t => {
  let now = 0, sleeps = 0;
  const budget = new MaintenanceBudget(100, () => now);
  t.after(() => budget.dispose());
  const outcome = await MaintenanceBudget.run(budget, () => quiesceRunAttempt({
    mode: "own", attempt: undefined, cloneKey: "test", targetPaths: ["/test"],
    processes: true, dockerHost: undefined, registry: new LiveAttemptRegistry(),
  }, { targetUid: 12345, reap: {
    viaHelper: false, selfPid: 900,
    table: {
      listPids: () => [17777],
      readStatus: () => "Name:\towned\nState:\tS\nUid:\t12345\t12345\t12345\t12345\n",
      readStat: pid => `${pid} (owned) S 1 ${pid} ${pid} ${"0 ".repeat(15)}1000 0 0`,
      readEnviron: () => "", readCwd: () => "/test",
    },
    kill: () => {},
    sleep: async () => { sleeps++; now = 100; budget.check(); },
  } }));
  assert.equal(sleeps, 1, JSON.stringify(outcome));
  assert.equal(outcome.process?.state, "unverified");
  assert.equal(outcome.docker.state, "not_wired");
});

test("Docker consumes prior stage time despite wall jumps and admits no next delete or sleep", async t => {
  let now = 0, deletes = 0, sleeps = 0;
  const budget = new MaintenanceBudget(10_000, () => now);
  t.after(() => budget.dispose());
  const server = http.createServer((req, res) => {
    if (req.method === "GET") {
      res.end(JSON.stringify([{ Id: "a", Mounts: [{ Source: "/test" }] }, { Id: "b", Mounts: [{ Source: "/test" }] }]));
    } else {
      deletes++;
      now = 10_000;
      res.writeHead(204); res.end();
    }
  });
  await new Promise<void>(resolve => server.listen(0, "127.0.0.1", resolve));
  t.after(() => new Promise<void>(resolve => server.close(() => resolve())));
  t.mock.method(Date, "now", () => -1e12);
  const result = await MaintenanceBudget.run(budget, async () => {
    const proof = await reapRunProcesses(request, { viaHelper: false, table: {
      listPids: () => { now = 9_000; return []; },
      readStatus: () => "", readStat: () => "", readEnviron: () => "", readCwd: () => "",
    } });
    assert.equal(proof.state, "quiescent");
    t.mock.method(Date, "now", () => 1e12);
    return teardownDocker({
      dockerHost: `tcp://127.0.0.1:${(server.address() as { port: number }).port}`,
      targetPaths: ["/test"], sleep: async () => { sleeps++; },
    });
  });
  assert.equal(result.state, "docker_error");
  assert.equal(deletes, 1);
  assert.equal(sleeps, 0);
});
