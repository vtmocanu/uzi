// issue #3915: the attribution trace on an unattributed runner-uid process and the root context
// of a quiescence scan are diagnostics only. They never change a kill or unverified decision.
import { describe, it } from "node:test";
import assert from "node:assert/strict";
import {
  collectLiveRoots,
  describeProcesses,
  describeRootContext,
  sanitizeAttributionTrace,
  scanOnce,
  type ProcTable,
  type ScanRequest,
} from "../src/run-quiescence.js";

const RUNNER = 1001;
const SELF = 900;
const START = 1000;

interface P { ppid?: number; pgid?: number; sid?: number; start?: number; readable?: boolean; statError?: boolean }

function errno(code: string): Error {
  return Object.assign(new Error(code), { code });
}

function table(procs: Record<number, P>): ProcTable {
  const get = (pid: number): P => {
    const p = procs[pid];
    if (!p) throw errno("ENOENT");
    return p;
  };
  return {
    listPids: () => Object.keys(procs).map(Number),
    readStatus: (pid) => { get(pid); return `Name:\tproc${pid}\nState:\tS (sleeping)\nUid:\t${RUNNER}\t${RUNNER}\t${RUNNER}\t${RUNNER}\n`; },
    readEnviron: (pid) => { if (get(pid).readable) return ""; throw errno("EACCES"); },
    readCwd: (pid) => { if (get(pid).readable) return "/elsewhere"; throw errno("EACCES"); },
    readStat: (pid) => {
      const p = get(pid);
      if (p.statError) throw errno("EACCES");
      return `${pid} (proc ${pid}) S ${p.ppid ?? 1} ${p.pgid ?? pid} ${p.sid ?? pid} ${"0 ".repeat(15)}${p.start ?? START} 0 0`;
    },
  };
}

function req(extra: Partial<ScanRequest> = {}): ScanRequest {
  return {
    mode: "own", targetUid: RUNNER, targetKey: "k", targetPaths: ["/data/runner/k"], ownMarker: "run-1:own",
    liveMarkers: ["run-2:peer"], liveRoots: [], workerNonce: "nonce", ...extra,
  };
}

describe("issue #3915: attribution trace on an unattributed unreadable process", () => {
  it("records a start-time mismatch on the recorded peer root, and stays unverified", () => {
    const procs = { 60: { readable: true, start: 1234 }, 62: { ppid: 60, pgid: 62, sid: 62 } };
    const res = scanOnce(req({ liveRoots: [{ pid: 60, startTime: START }] }), table(procs), SELF);
    assert.deepEqual(res.kill, []);
    const entry = res.unverified.find((p) => p.pid === 62);
    assert.equal(entry?.reason, "unreadable_unattributed");
    const t = entry?.attribution;
    assert.ok(t);
    assert.equal(t.startTime, START);
    assert.equal(t.ppid, 60);
    assert.equal(t.rootsCompared, 1);
    // pid 1 is not in the fake table, so its stat (and start time) is unreadable.
    assert.deepEqual(t.ancestors.map((a) => [a.pid, a.startTime]), [[60, 1234], [1, undefined]]);
    assert.deepEqual(t.candidates.find((c) => c.pid === 60), { pid: 60, role: "ancestor", match: "start_mismatch" });
    assert.deepEqual(t.candidates.find((c) => c.pid === 62), { pid: 62, role: "self", match: "not_recorded" });
  });

  it("records not_recorded everywhere when no peer root was supplied", () => {
    const res = scanOnce(req(), table({ 62: { pgid: 62, sid: 62 } }), SELF);
    const t = res.unverified[0]?.attribution;
    assert.ok(t);
    assert.equal(t.rootsCompared, 0);
    assert.ok(t.candidates.length > 0 && t.candidates.every((c) => c.match === "not_recorded"));
  });

  it("records start_unreadable when the recorded root's stat cannot be read", () => {
    const procs = { 60: { statError: true }, 62: { pgid: 60, sid: 62 } };
    const res = scanOnce(req({ liveRoots: [{ pid: 60, startTime: START }] }), table(procs), SELF);
    const t = res.unverified.find((p) => p.pid === 62)?.attribution;
    assert.deepEqual(t?.candidates.find((c) => c.role === "pgid"), { pid: 60, role: "pgid", match: "start_unreadable" });
  });

  it("does not trace a process a matching root attributes (it is not listed at all)", () => {
    const procs = { 60: { readable: true }, 62: { ppid: 60 } };
    const res = scanOnce(req({ liveRoots: [{ pid: 60, startTime: START }] }), table(procs), SELF);
    assert.equal(res.unverified.some((p) => p.pid === 62), false);
  });

  it("bounds the ancestor chain and the candidate list", () => {
    const procs: Record<number, P> = {};
    for (let pid = 100; pid < 140; pid++) procs[pid] = { ppid: pid === 100 ? 1 : pid - 1, readable: pid !== 139 };
    const t = scanOnce(req(), table(procs), SELF).unverified.find((p) => p.pid === 139)?.attribution;
    assert.ok(t);
    assert.equal(t.ancestors.length, 16);
    assert.ok(t.candidates.length <= 20);
  });

  it("traces at most four entries per scan; the rest stay listed and unverified", () => {
    const procs: Record<number, P> = {};
    for (let pid = 200; pid < 210; pid++) procs[pid] = {};
    const res = scanOnce(req(), table(procs), SELF);
    assert.equal(res.unverified.length, 10);
    assert.deepEqual(res.kill, []);
    assert.equal(res.unverified.filter((p) => p.attribution).length, 4);
    assert.ok(res.unverified.every((p) => p.reason === "unreadable_unattributed"));
  });

  it("builds traces from the scan's own stat pass, with no extra procfs reads", () => {
    const procs: Record<number, P> = {};
    // Four unreadable leaves, each under a 20-deep readable chain.
    for (let leaf = 0; leaf < 4; leaf++) {
      const base = 1000 + leaf * 100;
      for (let i = 0; i < 20; i++) procs[base + i] = { ppid: i === 0 ? 1 : base + i - 1, readable: true };
      procs[base + 20] = { ppid: base + 19 };
    }
    const t = table(procs);
    let statReads = 0;
    const counting: ProcTable = { ...t, readStat: (pid) => { statReads += 1; return t.readStat(pid); } };
    const res = scanOnce(req(), counting, SELF);
    assert.equal(res.unverified.filter((p) => p.attribution).length, 4);
    assert.equal(res.unverified[0]?.attribution?.ancestors.length, 16);
    const pids = Object.keys(procs).length;
    // One stat per pid in the first pass, plus at most the pre-existing per-entry reads
    // (ancestry, group and session checks happen only when roots exist; here there are none).
    assert.ok(statReads <= pids + 4 * 2, `stat reads ${statReads} for ${pids} pids`);
  });

  it("reaches the log fields, and a helper-supplied trace is rebuilt from validated fields only", () => {
    const t = scanOnce(req(), table({ 62: {} }), SELF).unverified[0]!.attribution!;
    const fields = describeProcesses({ state: "unverified", processes: [{ pid: 62, uid: RUNNER, comm: "x", cwd: "unreadable", reason: "unreadable_unattributed", attribution: t }], killed: [], detail: "d" });
    assert.deepEqual(fields[0]?.attribution_trace, t);
    assert.deepEqual(sanitizeAttributionTrace(JSON.parse(JSON.stringify(t))), t);
    const hostile = { ...t, ppid: -1, extra: "x", ancestors: [{ pid: 7, comm: "a\u0007b", argv: "secret" }, { pid: "x" }], candidates: [{ pid: 7, role: "root", match: "matched" }] };
    const clean = sanitizeAttributionTrace(hostile);
    assert.ok(clean);
    assert.equal("ppid" in clean, false);
    assert.equal("extra" in clean, false);
    assert.deepEqual(clean.ancestors, [{ pid: 7, comm: "a?b" }]);
    assert.deepEqual(clean.candidates, []);
    assert.equal(sanitizeAttributionTrace({ ancestors: [] }), undefined);
  });
});

describe("issue #3915: root context of a quiescence scan", () => {
  it("counts peer roots per attempt and marks a root read that threw, without its text", () => {
    const { roots, context } = collectLiveRoots(
      [
        { runId: "run-a", recordedRootPids: () => [{ pid: 60, startTime: START }] },
        { runId: "run-b", recordedRootPids: () => { throw new Error("token=secret-value"); } },
      ],
      [{ pid: 70, startTime: START }],
    );
    assert.deepEqual(roots, [{ pid: 60, startTime: START }, { pid: 70, startTime: START }]);
    assert.deepEqual(context, {
      liveAttempts: 2, attemptRoots: 1, workerRoots: 1, rootReadErrors: 1,
      attempts: [{ runId: "run-a", roots: 1 }, { runId: "run-b", roots: "error" }],
    });
    assert.deepEqual(describeRootContext({ state: "unverified", processes: [], killed: [], detail: "", rootContext: context }), {
      live_attempts: 2, attempt_roots: 1, worker_roots: 1, root_read_errors: 1,
      attempts: [{ run_id: "run-a", roots: 1 }, { run_id: "run-b", roots: "error" }],
    });
    assert.equal(JSON.stringify(context).includes("secret"), false, "exception text never reaches the context");
    assert.equal(describeRootContext(undefined), undefined);
  });
});
