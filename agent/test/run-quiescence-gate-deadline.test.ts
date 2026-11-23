import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { EventEmitter } from "node:events";
import { PassThrough } from "node:stream";
import http from "node:http";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { ChildProcess } from "node:child_process";
import { Readable } from "node:stream";
import { createCodexExecutionSafety } from "../src/codex/safety.js";
import { ExecutionRegistry, newLocalExecutionEpoch } from "../src/codex/registry.js";
import { assertClosedReaperRequest } from "../src/closed-reaper.js";
import { runnerTmpdir } from "../src/runner-uid.js";
import { RUN_ATTEMPT_ENV, RUN_CLONE_KEY_ENV } from "../src/worker-spawn-mark.js";
import { restoreHermeticView } from "./setup/hermetic-proc.js";
import {
  LiveAttemptRegistry,
  helperReapDeps,
  inventoryClearVerdict,
  newRunAttempt,
  setQuiescenceViewForTests,
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

  describe("a survivors verdict that cleanup delays past the deadline (inventoryClearVerdict reads ignored reasons as clear)", () => {
    const ignoredSurvivor = JSON.stringify({ state: "survivors", killed: [], detail: "1 in-scope process(es) survive the reap",
      processes: [{ pid: 70, uid: 1, comm: "c", cwd: CLONE, reason: "terminal_attempt:kill_unconfirmed" }] });
    const run = async (req: ScanRequest, forward: boolean, cleanupMs: number, deadlineMs: number): Promise<ProcessQuiescence> => {
      const child = fakeChild();
      const pending = reapRunProcesses(req, { viaHelper: true, deadlineAt: Date.now() + deadlineMs,
        ...(forward ? { forwardDeadlineToHelper: true as const } : {}),
        helperTmp: { make: () => "/private", remove: async () => { await sleep(cleanupMs); } },
        spawnHelper: () => child });
      await sleep(10);
      (child.stdout as unknown as PassThrough).write(`${ignoredSurvivor}\n`);
      child.emit("close", 0, null);
      return pending;
    };

    for (const [name, req, forward] of [
      ["an observe-only probe", fakeReq({ observeOnly: true }), false],
      ["an opted-in (forwardDeadlineToHelper) inventory scan", fakeReq(), true],
    ] as const) {
      it(`${name}: timely is survivors and clear; expired is the unverified withdrawal and not_clear`, async () => {
        const timely = await run(req, forward, 0, 5000);
        assert.equal(timely.state, "survivors");
        assert.equal(inventoryClearVerdict(timely), "clear");
        const late = await run(req, forward, 250, 120);
        assert.equal(late.state, "unverified");
        assert.equal(late.detail, "gate checkpoint deadline exhausted");
        assert.deepEqual(late.processes, []);
        assert.equal(inventoryClearVerdict(late), "not_clear");
      });
    }

    it("control: a non-opted-in (#3980 checkpoint) scan keeps its survivors result past the deadline", async () => {
      const late = await run(fakeReq(), false, 250, 120);
      assert.equal(late.state, "survivors");
    });
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

// ─── an opted-in (inventory, issue #4632) helper scan carries the gate deadline; no other signalling scan does ───

/** Runs reapRunProcesses through a fake helper child and returns the request it was sent on stdin. */
async function sentToHelper(req: ScanRequest, deadlineAt: number | undefined, optIn = true): Promise<Record<string, unknown>> {
  const child = fakeChild();
  let sent = "";
  const stdin = child.stdin as unknown as PassThrough;
  stdin.on("data", (c: Buffer | string) => { sent += c; });
  stdin.on("end", () => {
    (child.stdout as unknown as PassThrough).write(`${quiescent}\n`);
    child.emit("close", 0, null);
  });
  await reapRunProcesses(req, { viaHelper: true, ...(deadlineAt === undefined ? {} : { deadlineAt }),
    ...(optIn ? { forwardDeadlineToHelper: true as const } : {}),
    helperTmp: { make: () => "/private", remove: () => undefined }, spawnHelper: () => child });
  return JSON.parse(sent) as Record<string, unknown>;
}

describe("an opted-in (inventory) helper scan carries the gate deadline", () => {
  it("an opted-in non-observe request without its own deadlineAt is serialized with deadlineAt <= D", async () => {
    const D = Date.now() + 30_000;
    const sent = await sentToHelper(fakeReq(), D);
    assert.equal("observeOnly" in sent, false);
    assert.equal(sent.deadlineAt, D);
  });

  it("the serialized deadline is the earlier of the request's own and D, never later than D", async () => {
    const D = Date.now() + 30_000;
    assert.equal((await sentToHelper(fakeReq({ deadlineAt: D + 10_000 }), D)).deadlineAt, D);
    assert.equal((await sentToHelper(fakeReq({ deadlineAt: D - 5_000 }), D)).deadlineAt, D - 5_000);
  });

  it("control: without a gate deadline the serialized request carries none", async () => {
    assert.equal("deadlineAt" in (await sentToHelper(fakeReq(), undefined)), false);
  });

  it("negative control: a non-observe, non-opted-in reapRunProcesses with a deadline serializes NO deadlineAt (#3980 checkpoints unchanged)", async () => {
    const D = Date.now() + 30_000;
    const sent = await sentToHelper(fakeReq(), D, false);
    assert.equal("deadlineAt" in sent, false);
    // Even a request that carries its own deadlineAt is untouched by the gate deadline: it is not min-ed with D.
    assert.equal((await sentToHelper(fakeReq({ deadlineAt: D + 10_000 }), D, false)).deadlineAt, D + 10_000);
  });

  it("an observe-only request carries the deadline without any opt-in", async () => {
    const D = Date.now() + 30_000;
    assert.equal((await sentToHelper(fakeReq({ observeOnly: true }), D, false)).deadlineAt, D);
  });

  it("negative control: a non-opted-in quiesceRunAttempt (the #3980 checkpoint shape) serializes no deadlineAt; helperDeadline opts in", async () => {
    const sentBy = async (extra: Partial<QuiesceRunRequest>): Promise<Record<string, unknown>> => {
      const child = fakeChild();
      let sent = "";
      const stdin = child.stdin as unknown as PassThrough;
      stdin.on("data", (c: Buffer | string) => { sent += c; });
      stdin.on("end", () => { (child.stdout as unknown as PassThrough).write(`${quiescent}\n`); child.emit("close", 0, null); });
      await quiesceRunAttempt(quiesceReq({ processes: true, forceHelper: true, deadlineAt: Date.now() + 30_000, ...extra }),
        { reap: { spawnHelper: () => child, helperTmp: { make: () => "/private", remove: () => undefined } }, targetUid: RUNNER });
      return JSON.parse(sent) as Record<string, unknown>;
    };
    assert.equal("deadlineAt" in await sentBy({}), false);
    assert.equal(typeof (await sentBy({ helperDeadline: true })).deadlineAt, "number");
    assert.equal(typeof (await sentBy({ observeOnly: true })).deadlineAt, "number");
  });

  it("the scoped (closed reaper) path serializes the scan's own deadlineAt for an opted-in non-observe request", async () => {
    // This path builds its stdin from the ScanRequest itself, not through the plain helper's serialization.
    const oldSplit = process.env.UZI_UID_SPLIT;
    process.env.UZI_UID_SPLIT = "1";
    setQuiescenceViewForTests(undefined);
    const attempt = newRunAttempt("gate-scoped", 1, path.join(os.tmpdir(), "capture-gate-scoped"), () => []);
    const live = new LiveAttemptRegistry();
    live.add(attempt);
    const deadlineAt = Date.now() + 30_000;
    const request: QuiesceRunRequest = { mode: "capture", attempt, cloneKey: attempt.cloneKey,
      targetPaths: [attempt.clonePath], processes: true, dockerHost: undefined, registry: live,
      otherClaimInFlight: false, deadlineAt, helperDeadline: true };
    const temp = path.join(runnerTmpdir() ?? os.tmpdir(), "uzi-quiesce-gatescoped");
    let sent: Record<string, unknown> | undefined;
    let seamFailure: unknown;
    const safety = createCodexExecutionSafety(new ExecutionRegistry(newLocalExecutionEpoch(1)),
      async () => { throw new Error("legacy spawn forbidden"); }, undefined, undefined,
      async r => {
        try {
          const input = assertClosedReaperRequest(r);
          const scan = r.argv[0] === process.execPath;
          if (scan) sent = JSON.parse(input) as Record<string, unknown>;
          return { root: { kind: "boundary_action", dispose: async () => {}, reap: async () => ({ ok: true }) },
            stdin: null, stdout: Readable.from([scan ? quiescent : r.argv[0] === "/bin/mktemp" ? temp + "\n" : ""]),
            stderr: Readable.from([]), waitChild: async () => { await new Promise<void>(resolve => setImmediate(resolve)); return { code: 0 }; } };
        } catch (error) { seamFailure = error; throw error; }
      }, undefined, undefined, { reaperAttempt: () => attempt });
    try {
      const result = await safety.withCredentialFreeCapture(9000, async () => quiesceRunAttempt(request))
        .catch(error => { throw seamFailure ?? error; });
      if (seamFailure) throw seamFailure;
      assert.equal(result.kind, "observed_empty");
      assert.ok(sent, "the scan request reached the closed reaper seam");
      assert.equal("observeOnly" in sent, false);
      assert.equal(sent.deadlineAt, deadlineAt);
    } finally {
      restoreHermeticView();
      if (oldSplit === undefined) delete process.env.UZI_UID_SPLIT;
      else process.env.UZI_UID_SPLIT = oldSplit;
    }
  });
});

describe("a helper-style reap (excludeAllDescendants + deadlineAt) never signals at or after D", () => {
  const helperDeps = (clock: { now: () => number; sleep: (ms: number) => Promise<void> }, table: ProcTable, kill: (pid: number) => void, deadlineAt: number) =>
    ({ table, selfPid: SELF, excludeAllDescendants: true, now: clock.now, sleep: clock.sleep, kill, deadlineAt });

  it("a persistent kill candidate: every kill is admitted before D and the verdict is unverified 'process proof deadline'", async () => {
    const D = 250;
    let t = 0;
    const killTimes: number[] = [];
    const r = await reapProcesses(fakeReq(), helperDeps({ now: () => t, sleep: async ms => { t += ms; } },
      fakeTable({ 2000: { uid: RUNNER, cwd: CLONE } }), () => { killTimes.push(t); }, D));
    assert.equal(r.state, "unverified");
    assert.equal(r.detail, "process proof deadline");
    assert.ok(killTimes.length > 0, "control: the candidate was signalled while time was left");
    assert.ok(killTimes.every(k => k < D), `a kill ran at or after D: ${JSON.stringify(killTimes)}`);
  });

  it("a clock that crosses D between the scan and the kill signals nothing", async () => {
    const D = 1_000;
    const run = async (jumpAtCall: number): Promise<{ r: ProcessQuiescence; killed: number[]; nowCallsAtFirstKill: number | undefined }> => {
      let calls = 0;
      let t = 0;
      let atFirstKill: number | undefined;
      const killed: number[] = [];
      const r = await reapProcesses(fakeReq(), helperDeps({ now: () => { if (++calls >= jumpAtCall) t = Math.max(t, D); return t; }, sleep: async ms => { t += ms; } },
        fakeTable({ 2000: { uid: RUNNER, cwd: CLONE } }), pid => { atFirstKill ??= calls; killed.push(pid); }, D));
      return { r, killed, nowCallsAtFirstKill: atFirstKill };
    };
    // Calibration (no crossing): the clock call that immediately precedes the first signal is the kill loop's last admission check.
    const calibration = await run(Number.POSITIVE_INFINITY);
    assert.ok(calibration.killed.length > 0 && calibration.nowCallsAtFirstKill !== undefined, "control: without a crossing the candidate is signalled");
    const crossed = await run(calibration.nowCallsAtFirstKill!);
    assert.deepEqual(crossed.killed, []);
    assert.equal(crossed.r.state, "unverified");
    assert.equal(crossed.r.detail, "process proof deadline");
  });

  it("the final kill gate: a candidate whose identity read crosses D after admission is not signalled, and its successor is not even read", async () => {
    // The kill loop's identity read goes through the bounded table, which checks the clock BEFORE the read. The
    // second candidate's read advances the clock to D from inside the read (after admission), so only the kill
    // loop's own gate stands between that candidate and a signal.
    const D = 5_000;
    let t = 0;
    const killed: number[] = [];
    const statReads: Record<number, number> = {};
    const env = `${RUN_ATTEMPT_ENV}=run-17:own\u0000${RUN_CLONE_KEY_ENV}=${KEY}\u0000`;
    const pids = [2001, 2002, 2003];
    const table: ProcTable = {
      listPids: () => pids,
      readStatus: (pid) => `Name:\tproc${pid}\nState:\tS (sleeping)\nUid:\t${RUNNER}\t${RUNNER}\t${RUNNER}\t${RUNNER}\n`,
      readEnviron: () => env,
      readCwd: () => CLONE,
      readStat: (pid) => {
        if (killed.length > 0) {
          statReads[pid] = (statReads[pid] ?? 0) + 1;
          if (pid === 2002) t = D;
        }
        return `${pid} (proc ${pid}) S 1 ${pid} ${pid} ${"0 ".repeat(15)}1000 0 0`;
      },
    };
    const req = fakeReq({ deadlineAt: D });
    const r = await reapProcesses(req, { ...helperReapDeps(req), table, selfPid: SELF, now: () => t, sleep: async (ms) => { t += ms; },
      kill: (pid) => { killed.push(pid); } });
    assert.deepEqual(killed, [2001], "only the candidate admitted before D was signalled");
    assert.equal(statReads[2002], 1, "the second candidate's identity read ran (and crossed D)");
    assert.equal(statReads[2003], undefined, "the loop stopped at the gate, before reading the third");
    assert.equal(r.state, "unverified");
    assert.equal(r.detail, "process proof deadline");
  });
});

describe("the helper's reap deps are built from the serialized request", () => {
  const sentBy = async (extra: Partial<QuiesceRunRequest>): Promise<ScanRequest> => {
    const child = fakeChild();
    let sent = "";
    const stdin = child.stdin as unknown as PassThrough;
    stdin.on("data", (c: Buffer | string) => { sent += c; });
    stdin.on("end", () => { (child.stdout as unknown as PassThrough).write(`${quiescent}\n`); child.emit("close", 0, null); });
    await quiesceRunAttempt(quiesceReq({ processes: true, forceHelper: true, ...extra }),
      { reap: { spawnHelper: () => child, helperTmp: { make: () => "/private", remove: () => undefined } }, targetUid: RUNNER });
    return JSON.parse(sent) as ScanRequest;
  };

  for (const [name, extra] of [
    ["observeOnly", { observeOnly: true as const }],
    ["helperDeadline", { helperDeadline: true as const }],
  ] as const) {
    it(`${name}: the deadline sent to the helper becomes the reaper's deadline, never later than D`, async () => {
      const D = Date.now() + 30_000;
      const parsed = await sentBy({ ...extra, deadlineAt: D });
      assert.equal(typeof parsed.deadlineAt, "number");
      assert.equal(helperReapDeps(parsed).deadlineAt, parsed.deadlineAt);
      assert.ok(helperReapDeps(parsed).deadlineAt! <= D);
      assert.equal(helperReapDeps(parsed).excludeAllDescendants, true);
    });
  }

  it("negative control: a plain deadlined helper scan (#3980) gives the reaper no deadline", async () => {
    const parsed = await sentBy({ deadlineAt: Date.now() + 30_000 });
    assert.equal(helperReapDeps(parsed).deadlineAt, undefined);
  });
});

describe("fail closed: a gate checkpoint (deps.deadlineAt, no serialized request deadline) records procfs EACCES as a table failure", () => {
  const eaccesTable = (): ProcTable => ({
    ...fakeTable({ 2000: { uid: RUNNER, cwd: CLONE } }),
    readEnviron: () => { throw errno("EACCES"); },
  });

  it("a serialized request deadline reaches per-PID attribution instead: unreadable_unattributed, unsignalled, not_clear", async () => {
    let t = 0;
    const killed: number[] = [];
    const r = await reapProcesses(fakeReq({ deadlineAt: 1_000_000 }), { table: eaccesTable(), selfPid: SELF, excludeAllDescendants: true,
      now: () => t, sleep: async ms => { t += ms; }, kill: pid => { killed.push(pid); }, deadlineAt: 1_000_000, deadlineMs: 100 });
    assert.equal(r.state, "unverified");
    assert.deepEqual(r.processes.map(p => p.reason), ["unreadable_unattributed"]);
    assert.deepEqual(killed, []);
    assert.equal(inventoryClearVerdict(r), "not_clear");
  });

  it("with only deps.deadlineAt the scan is unverified with no processes, nothing is killed, and the inventory verdict is not_clear", async () => {
    let t = 0;
    const killed: number[] = [];
    const r = await reapProcesses(fakeReq(), { table: eaccesTable(), selfPid: SELF, excludeAllDescendants: true,
      now: () => t, sleep: async ms => { t += ms; }, kill: pid => { killed.push(pid); }, deadlineAt: 1_000_000 });
    assert.equal(r.state, "unverified");
    assert.deepEqual(r.processes, []);
    assert.deepEqual(r.killed, []);
    assert.deepEqual(killed, []);
    assert.match(r.detail, /EACCES/);
    assert.equal(inventoryClearVerdict(r), "not_clear");
  });

  it("control: without deadlineAt the same EACCES is an unreadable_unattributed process, unsignalled, and still not_clear", async () => {
    let t = 0;
    const killed: number[] = [];
    const r = await reapProcesses(fakeReq(), { table: eaccesTable(), selfPid: SELF, excludeAllDescendants: true,
      now: () => t, sleep: async ms => { t += ms; }, kill: pid => { killed.push(pid); } });
    assert.equal(r.state, "unverified");
    assert.deepEqual(r.processes.map(p => p.reason), ["unreadable_unattributed"]);
    assert.deepEqual(killed, []);
    assert.equal(inventoryClearVerdict(r), "not_clear");
  });
});
