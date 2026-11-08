import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { EventEmitter } from "node:events";
import { PassThrough } from "node:stream";
import http from "node:http";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { ChildProcess } from "node:child_process";
import {
  LiveAttemptRegistry,
  quiesceRunAttempt,
  reapProcesses,
  reapRunProcesses,
  runnerHelperTmp,
  type ProcTable,
  type ProcessQuiescence,
  type QuiesceRunRequest,
  type ScanRequest,
} from "../src/run-quiescence.js";

// issue #3980: the absolute deadline a bounded Claude gate checkpoint threads into its quiescence
// proofs (QuiesceRunRequest.deadlineAt / ReapDeps.deadlineAt). Without it nothing here changes; with
// it a read, scan, helper, private-temp command or Docker request is admitted only while time is
// left, is cut off at the deadline, and an expiry reads as unverified.

const RUNNER = 10002;
const CLONE = "/data/runner/github.com+o+r/issue-17";
const KEY = "github.com+o+r/issue-17";
const SELF = 900;

interface FakeProc { uid: number; cwd: string }
function errno(code: string): Error { return Object.assign(new Error(code), { code }); }
function fakeTable(procs: Record<number, FakeProc>, wrap: (read: () => unknown) => void = () => undefined): ProcTable {
  const get = (pid: number): FakeProc => {
    const p = procs[pid];
    if (!p) throw errno("ENOENT");
    return p;
  };
  return {
    listPids: () => { wrap(() => undefined); return Object.keys(procs).map(Number); },
    readStatus: (pid) => { const p = get(pid); wrap(() => undefined); return `Name:\tproc${pid}\nState:\tS (sleeping)\nUid:\t${p.uid}\t${p.uid}\t${p.uid}\t${p.uid}\n`; },
    readEnviron: (pid) => { get(pid); wrap(() => undefined); return ""; },
    readCwd: (pid) => { const p = get(pid); wrap(() => undefined); return p.cwd; },
    readStat: (pid) => { get(pid); wrap(() => undefined); return `${pid} (proc ${pid}) S 1 ${pid} ${pid} ${"0 ".repeat(15)}1000 0 0`; },
  };
}
function fakeReq(extra: Partial<ScanRequest> = {}): ScanRequest {
  return { mode: "own", targetUid: RUNNER, targetKey: KEY, targetPaths: [CLONE], ownMarker: "run-17:own",
    liveMarkers: [], liveRoots: [], workerNonce: "nonce", ...extra };
}

describe("issue #3980: reapProcesses with a gate deadline", () => {
  it("an expiry in the middle of a scan is unverified and no later read or pass is admitted", async () => {
    let clock = 0;
    let lists = 0;
    let readsAfterExpiry = 0;
    const deadlineAt = 500;
    const procs: Record<number, FakeProc> = { 2000: { uid: RUNNER, cwd: CLONE } };
    const base = fakeTable(procs, () => { if (clock >= deadlineAt) readsAfterExpiry++; });
    const table: ProcTable = { ...base,
      listPids: () => { lists++; return base.listPids(); },
      // The first status read takes the whole budget.
      readStatus: (pid) => { const out = base.readStatus(pid); clock += 1000; return out; } };
    const killed: number[] = [];
    const r = await reapProcesses(fakeReq(), { table, selfPid: SELF, now: () => clock, sleep: async (ms) => { clock += ms; },
      kill: (pid) => { killed.push(pid); }, deadlineAt });
    assert.equal(r.state, "unverified");
    assert.match(r.detail, /process proof deadline/);
    assert.equal(lists, 1, "no second pass was admitted");
    assert.equal(readsAfterExpiry, 0, "no read reached the table after expiry");
    assert.deepEqual(killed, [], "and nothing was signalled");
  });

  it("a deadline that has already passed admits no read at all", async () => {
    let reads = 0;
    const table = fakeTable({ 2000: { uid: RUNNER, cwd: CLONE } }, () => { reads++; });
    const r = await reapProcesses(fakeReq(), { table, selfPid: SELF, now: () => 10, sleep: async () => undefined, kill: () => undefined, deadlineAt: 10 });
    assert.equal(r.state, "unverified");
    assert.equal(reads, 0);
  });

  it("the normal multi-pass loop is kept: a process killed in pass one is gone in pass two, as without a deadline", async () => {
    const run = async (deadlineAt?: number): Promise<ProcessQuiescence> => {
      const procs: Record<number, FakeProc> = { 2000: { uid: RUNNER, cwd: CLONE } };
      let clock = 0;
      return reapProcesses(fakeReq(), { table: fakeTable(procs), selfPid: SELF, now: () => clock, sleep: async (ms) => { clock += ms; },
        kill: (pid) => { delete procs[pid]; }, ...(deadlineAt === undefined ? {} : { deadlineAt }) });
    };
    const plain = await run();
    const bounded = await run(1_000_000);
    assert.equal(plain.state, "quiescent");
    assert.deepEqual(bounded, plain);
    assert.deepEqual(bounded.killed, [2000]);
  });

  it("the reap loop is capped at the deadline when the default 5 s would outlast it", async () => {
    const procs: Record<number, FakeProc> = { 2000: { uid: RUNNER, cwd: CLONE } };
    let clock = 0;
    let scans = 0;
    const table = fakeTable(procs, () => { scans++; });
    const r = await reapProcesses(fakeReq(), { table, selfPid: SELF, now: () => clock, sleep: async (ms) => { clock += ms; },
      kill: () => undefined /* the process never dies */, deadlineAt: 350 });
    assert.notEqual(r.state, "quiescent");
    assert.ok(clock <= 450, `the loop stopped near the deadline, not after 5 s (clock ${clock})`);
    assert.ok(scans > 0);
  });

  it("without a deadline the loop runs to its own 5 s deadline", async () => {
    const procs: Record<number, FakeProc> = { 2000: { uid: RUNNER, cwd: CLONE } };
    let clock = 0;
    const r = await reapProcesses(fakeReq(), { table: fakeTable(procs), selfPid: SELF, now: () => clock, sleep: async (ms) => { clock += ms; },
      kill: () => undefined });
    assert.equal(r.state, "survivors");
    assert.ok(clock >= 5000, `ran to the default deadline (clock ${clock})`);
  });
});

const quiescent = JSON.stringify({ state: "quiescent", processes: [], killed: [], detail: "reaped 0 process(es)" });
function fakeChild(): ChildProcess & EventEmitter {
  return Object.assign(new EventEmitter(), { pid: undefined, stdout: new PassThrough(), stderr: new PassThrough(), stdin: new PassThrough() }) as unknown as ChildProcess & EventEmitter;
}
const sleep = (ms: number): Promise<void> => new Promise((resolve) => setTimeout(resolve, ms));

describe("issue #3980: the uid-split helper path with a gate deadline", () => {
  it("setup is admitted only before the deadline", async () => {
    let makes = 0, spawns = 0;
    const r = await reapRunProcesses(fakeReq(), { viaHelper: true, deadlineAt: Date.now() - 1,
      helperTmp: { make: () => { makes++; return "/private"; }, remove: () => undefined },
      spawnHelper: () => { spawns++; throw new Error("unexpected"); } });
    assert.equal(r.state, "unverified");
    assert.equal(makes, 0);
    assert.equal(spawns, 0);
  });

  it("setup that crosses the deadline launches no helper and awaits the temp removal", async () => {
    const events: string[] = [];
    const deadlineAt = Date.now() + 150;
    const r = await reapRunProcesses(fakeReq(), { viaHelper: true, deadlineAt,
      helperTmp: {
        make: async () => { await sleep(250); events.push("made"); return "/private"; },
        remove: async () => { await sleep(40); events.push("removed"); },
      },
      spawnHelper: () => { events.push("spawn"); throw new Error("unexpected"); } });
    events.push("resolved");
    assert.equal(r.state, "unverified");
    assert.deepEqual(events, ["made", "removed", "resolved"], "removal finished before the verdict, and no helper was launched");
  });

  it("a hung setup command is killed at the deadline and the verdict waits for its close", async () => {
    const dir = fs.mkdtempSync(path.join(fs.realpathSync(os.tmpdir()), "gate-deadline-"));
    const pidFile = path.join(dir, "pid");
    try {
      const wrap = (): { command: string; args: string[] } => ({ command: "/bin/sh", args: ["-c", `echo $$ > '${pidFile}'; exec sleep 30`] });
      const started = Date.now();
      const r = await reapRunProcesses(fakeReq(), { viaHelper: true, deadlineAt: started + 400,
        helperTmp: { make: (budget) => runnerHelperTmp("/private-base", wrap, { budget }).make(), remove: () => undefined },
        spawnHelper: () => { throw new Error("unexpected"); } });
      const took = Date.now() - started;
      assert.equal(r.state, "unverified");
      assert.ok(took >= 350 && took < 3000, `settled at the deadline, not the command's 30 s: ${took} ms`);
      const pid = Number(fs.readFileSync(pidFile, "utf8"));
      assert.throws(() => process.kill(pid, 0), /ESRCH/, "the setup command is dead (joined) when the verdict returns");
    } finally { fs.rmSync(dir, { recursive: true, force: true }); }
  });

  it("the helper's timeout is capped at the deadline, and the verdict waits for close, then cleanup", async () => {
    const child = fakeChild();
    const events: string[] = [];
    const started = Date.now();
    let settled = false;
    const pending = reapRunProcesses(fakeReq(), { viaHelper: true, deadlineAt: started + 300, helperTimeoutMs: 10_000,
      helperTmp: { make: () => "/private", remove: async () => { await sleep(30); events.push("removed"); } },
      spawnHelper: () => child }).then((r) => { settled = true; events.push("resolved"); return r; });
    await sleep(500); // past the deadline, well under the 10 s helper timeout
    assert.equal(settled, false, "a timed-out helper is joined: the verdict waits for its close");
    events.push("close");
    child.emit("close", null, "SIGKILL");
    const r = await pending;
    assert.equal(r.state, "unverified");
    assert.match(r.detail, /timed out/);
    assert.deepEqual(events, ["close", "removed", "resolved"]);
  });

  it("a verdict that cleanup delays past the deadline is unverified; a timely one is returned as is", async () => {
    const run = async (cleanupMs: number, deadlineMs: number): Promise<ProcessQuiescence> => {
      const child = fakeChild();
      const pending = reapRunProcesses(fakeReq(), { viaHelper: true, deadlineAt: Date.now() + deadlineMs,
        helperTmp: { make: () => "/private", remove: async () => { await sleep(cleanupMs); } },
        spawnHelper: () => child });
      await sleep(10);
      (child.stdout as unknown as PassThrough).write(`${quiescent}\n`);
      child.emit("close", 0, null);
      return pending;
    };
    assert.equal((await run(0, 5000)).state, "quiescent");
    const late = await run(200, 100);
    assert.equal(late.state, "unverified");
    assert.match(late.detail, /deadline/);
  });
});

function dockerServer(): Promise<{ port: number; hits: number[]; close: () => Promise<void> }> {
  const hits: number[] = [];
  const server = http.createServer((_req, res) => { hits.push(Date.now()); res.writeHead(200, { "content-type": "application/json" }); res.end("[]"); });
  return new Promise((resolve) => server.listen(0, "127.0.0.1", () => resolve({
    port: (server.address() as { port: number }).port, hits,
    close: () => new Promise<void>((done) => { server.closeAllConnections(); server.close(() => done()); }),
  })));
}
const quiesceReq = (extra: Partial<QuiesceRunRequest>): QuiesceRunRequest => ({
  mode: "own", attempt: undefined, cloneKey: KEY, targetPaths: [CLONE], processes: false, dockerHost: undefined,
  registry: new LiveAttemptRegistry(), ...extra });

describe("issue #3980: quiesceRunAttempt Docker admission with a gate deadline", () => {
  it("past the deadline no Docker request is sent and the teardown reads docker_error", async () => {
    const daemon = await dockerServer();
    try {
      const r = await quiesceRunAttempt(quiesceReq({ dockerHost: `tcp://127.0.0.1:${daemon.port}`, deadlineAt: Date.now() - 1 }));
      assert.equal(r.docker.state, "docker_error");
      assert.equal(r.docker.detail, "deadline exhausted");
      assert.equal(daemon.hits.length, 0);
    } finally { await daemon.close(); }
  });

  it("a process proof that was quiescent is withdrawn when the deadline passed before Docker", async () => {
    const daemon = await dockerServer();
    try {
      // The proof reads on an injected clock (so it passes), but the table read itself takes real time.
      const table = fakeTable({}, () => { Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, 120); });
      const r = await quiesceRunAttempt(quiesceReq({ processes: true, dockerHost: `tcp://127.0.0.1:${daemon.port}`, deadlineAt: Date.now() + 60 }),
        { reap: { table, selfPid: SELF, now: () => 0, sleep: async () => undefined, kill: () => undefined }, targetUid: RUNNER });
      assert.equal(r.process?.state, "unverified");
      assert.match(r.process?.detail ?? "", /deadline/);
      assert.equal(daemon.hits.length, 0);
    } finally { await daemon.close(); }
  });

  it("with time left the request is admitted and bounded by the time left, not Docker's own 15 s", async () => {
    const hang = http.createServer(() => undefined); // never answers
    await new Promise<void>((resolve) => hang.listen(0, "127.0.0.1", resolve));
    try {
      const port = (hang.address() as { port: number }).port;
      const started = Date.now();
      const r = await quiesceRunAttempt(quiesceReq({ dockerHost: `tcp://127.0.0.1:${port}`, deadlineAt: started + 400 }));
      const took = Date.now() - started;
      assert.equal(r.docker.state, "docker_error");
      assert.ok(took < 3000, `bounded by the gate deadline: ${took} ms`);
    } finally { hang.closeAllConnections(); await new Promise<void>((resolve) => hang.close(() => resolve())); }
  });

  it("without a deadline nothing changes: the teardown reads its own result", async () => {
    const daemon = await dockerServer();
    try {
      const r = await quiesceRunAttempt(quiesceReq({ dockerHost: `tcp://127.0.0.1:${daemon.port}` }));
      assert.equal(r.docker.state, "docker_unconfirmed");
      assert.ok(daemon.hits.length >= 2);
    } finally { await daemon.close(); }
  });
});
