import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { reapProcesses, type ProcTable, type ScanRequest } from "../src/run-quiescence.js";
import { RUN_ATTEMPT_ENV, RUN_CLONE_KEY_ENV } from "../src/worker-spawn-mark.js";

// issue #3644 Gate B (plan 3.4 step 6): `protectedMarkers` makes the retained predecessor's processes block a seed instead of
// being reaped as a terminal attempt of the same clone key. An injected process table; nothing here touches a real process.

const RUNNER = 10002;
const SELF = 900;
const KEY = "github.com+o+r/issue-3644";
const CANONICAL = "/data/runner/github.com+o+r/issue-3644";
const P_MARKER = "run-3644:20261105T101010Z-g1-0123456789abcdef";
const S_MARKER = "run-3644:20261105T111111Z-g2-fedcba9876543210";

interface FakeProc { uid: number; ppid?: number; cwd?: string | "EACCES"; env?: Record<string, string> | "EACCES" }

function errno(code: string): Error { return Object.assign(new Error(code), { code }); }

function fakeTable(procs: Record<number, FakeProc>): ProcTable {
  const get = (pid: number): FakeProc => { const p = procs[pid]; if (!p) throw errno("ENOENT"); return p; };
  return {
    listPids: () => Object.keys(procs).map(Number),
    readStatus: pid => `Name:\tproc${pid}\nState:\tS (sleeping)\nUid:\t${get(pid).uid}\t${get(pid).uid}\t${get(pid).uid}\t${get(pid).uid}\n`,
    readEnviron: pid => {
      const e = get(pid).env ?? {};
      if (typeof e === "string") throw errno(e);
      return Object.entries(e).map(([k, v]) => `${k}=${v}\u0000`).join("");
    },
    readCwd: pid => { const c = get(pid).cwd ?? "/"; if (c === "EACCES") throw errno(c); return c; },
    readStat: pid => `${pid} (proc ${pid}) S ${get(pid).ppid ?? 1} ${pid} ${pid} ${"0 ".repeat(15)}1000 0 0`,
  };
}

function request(extra: Partial<ScanRequest> = {}): ScanRequest {
  return { mode: "seed", targetUid: RUNNER, targetKey: KEY, targetPaths: [CANONICAL], liveMarkers: [], liveRoots: [], workerNonce: "nonce", ...extra };
}

async function reap(procs: Record<number, FakeProc>, req: ScanRequest) {
  const signalled: number[] = [];
  let t = 0;
  const table: Record<number, FakeProc> = { [SELF]: { uid: RUNNER, ppid: 1 }, ...procs };
  const result = await reapProcesses(req, {
    table: fakeTable(table), selfPid: SELF, now: () => t,
    sleep: async ms => { t += ms; }, kill: pid => { signalled.push(pid); delete table[pid]; },
  });
  return { result, signalled };
}

const marked = (marker: string, cwd = "/elsewhere"): FakeProc => ({ uid: RUNNER, cwd, env: { [RUN_ATTEMPT_ENV]: marker, [RUN_CLONE_KEY_ENV]: KEY } });

describe("protectedMarkers", () => {
  it("control: without it a terminal attempt of the same key is reaped, in the key's scope by its marker alone", async () => {
    const { result, signalled } = await reap({ 20: marked(P_MARKER) }, request());
    assert.equal(result.state, "quiescent");
    assert.deepEqual(signalled, [20]);
  });

  it("a process carrying a protected marker is a survivor and is never signalled; the seed blocks", async () => {
    const { result, signalled } = await reap({ 20: marked(P_MARKER) }, request({ protectedMarkers: [P_MARKER] }));
    assert.equal(result.state, "survivors");
    assert.deepEqual(signalled, []);
    assert.deepEqual(result.processes.map(p => [p.pid, p.reason]), [[20, "protected_attempt"]]);
  });

  it("protection wins even for the request's own marker, and covers a descendant-looking cwd under the target path", async () => {
    const { result, signalled } = await reap(
      { 20: marked(P_MARKER, CANONICAL + "/sub"), 21: marked(P_MARKER) },
      request({ ownMarker: P_MARKER, protectedMarkers: [P_MARKER] }));
    assert.equal(result.state, "survivors");
    assert.deepEqual(signalled, []);
    assert.deepEqual(result.processes.map(p => p.reason), ["protected_attempt", "protected_attempt"]);
  });

  it("other terminal attempts are still reaped around a protected one, and a live attempt still conflicts", async () => {
    const { result, signalled } = await reap(
      { 20: marked(P_MARKER), 21: marked("run-3644:20261104T101010Z-g1-aaaaaaaaaaaaaaaa"), 22: marked(S_MARKER) },
      request({ protectedMarkers: [P_MARKER], liveMarkers: [S_MARKER] }));
    assert.equal(result.state, "survivors");
    assert.deepEqual(signalled, [21]);
    assert.deepEqual(result.processes.map(p => [p.pid, p.reason]).sort(), [[20, "protected_attempt"], [22, "live_attempt_conflict"]]);
  });

  it("an unreadable (non-dumpable) runner-uid process is not reaped as residue while a marker is protected", async () => {
    const unreadable: FakeProc = { uid: RUNNER, cwd: "EACCES", env: "EACCES" };
    const base = request({ mayKillUnreadableUnattributed: true });
    const control = await reap({ 30: { ...unreadable } }, base);
    assert.deepEqual(control.signalled, [30], "control: a solitary unreadable process is reapable");
    const guarded = await reap({ 30: { ...unreadable } }, { ...base, protectedMarkers: [P_MARKER] });
    assert.deepEqual(guarded.signalled, []);
    assert.equal(guarded.result.state, "unverified", "it blocks the seed as unattributed residue");
  });

  it("a process of another run or another key is out of scope whatever the protection", async () => {
    const { result, signalled } = await reap(
      { 40: { uid: RUNNER, cwd: "/elsewhere", env: { [RUN_ATTEMPT_ENV]: P_MARKER, [RUN_CLONE_KEY_ENV]: "github.com+o+r/issue-1" } } },
      request({ protectedMarkers: [P_MARKER] }));
    assert.equal(result.state, "quiescent");
    assert.deepEqual(signalled, []);
  });
});
