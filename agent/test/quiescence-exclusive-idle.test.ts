import { test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { procMountShowsAllProcesses, reapRunProcesses, scanOnce, snapshotMaintenanceBootstrap, type ProcTable, type ScanRequest } from "../src/run-quiescence.js";
import { RecoveryMaintenance, MaintenanceResources } from "../src/recovery-maintenance.js";
import type { RecordedRoot } from "../src/worker-spawn-mark.js";

type ExclusiveRequest = ScanRequest & { exclusiveIdle: true; exclusiveIdleRoots: RecordedRoot[] };
type Process = { uid?: number; ppid?: number; start?: number; state?: string; unreadable?: boolean;
  env?: string; pgid?: number; sid?: number; executable?: string };
const self = 10;
const procMount = "22 1 0:5 / /proc rw,nosuid,nodev,noexec - proc proc rw\n";
function table(rows: Record<number, Process>): ProcTable {
  return {
    listPids: () => Object.keys(rows).map(Number),
    readStatus: pid => {
      const row = rows[pid];
      if (!row || row.unreadable) throw Object.assign(new Error("unreadable"), { code: row ? "EACCES" : "ENOENT" });
      return `Name:\tprocess\nState:\t${row.state ?? "S"} (state)\nUid:\t${row.uid ?? 10001}\t10001\t10001\t10001\n`;
    },
    readStat: pid => {
      const row = rows[pid];
      if (!row) throw Object.assign(new Error("gone"), { code: "ENOENT" });
      return `${pid} (process) ${row.state ?? "S"} ${row.ppid ?? 1} ${row.pgid ?? pid} ${row.sid ?? pid} ${"0 ".repeat(15)}${row.start ?? 100} 0 0`;
    },
    readEnviron: pid => rows[pid]?.env ?? "",
    readCwd: () => "/unrelated",
    readExecutable: pid => rows[pid]?.executable ?? "/image/esbuild",
    readProcMountInfo: () => procMount,
  };
}
function request(roots: RecordedRoot[] = []): ExclusiveRequest {
  return { mode: "capture", targetUid: 10001, targetKey: "", targetPaths: [], liveMarkers: [],
    liveRoots: [], workerNonce: "forged-nonce", exclusiveIdle: true, exclusiveIdleRoots: roots };
}
const base = () => ({ 1: { uid: 0, ppid: 0 }, [self]: { ppid: 1 } });

for (const [label, process] of [
  ["readable same-UID orphan", { uid: 10001 }],
  ["forged nonce without attempt marker", { env: "UZI_WORKER_SPAWN=forged-nonce\0" }],
  ["marker-stripped worker descendant", { ppid: self }],
  ["different runner UID", { uid: 10002 }],
  ["different command UID", { uid: 10003 }],
  ["root UID", { uid: 0 }],
  ["unreadable status", { unreadable: true }],
] as const) {
  test(`#3031 exclusive-idle refuses ${label} without signalling`, () => {
    const result = scanOnce(request(), table({ ...base(), 20: process }), self);
    assert.deepEqual(result.kill, []);
    assert.equal(result.survivors.length + result.unverified.length, 1);
    assert.equal((result.survivors[0] ?? result.unverified[0])?.pid, 20);
  });
}

test("#3031 exclusive-idle accepts only pinned root subtrees, not PGID/SID peers", () => {
  const roots = [{ pid: 30, startTime: 200 }];
  const rows = { ...base(), 30: { ppid: self, start: 200 }, 31: { ppid: 30 },
    40: { pgid: 30, sid: 30 } };
  const result = scanOnce(request(roots), table(rows), self);
  assert.deepEqual(result.kill, []);
  assert.deepEqual(result.survivors.map(p => p.pid), [40]);
  assert.deepEqual(result.unverified, []);
  const reused = scanOnce(request([{ pid: 30, startTime: 199 }]), table(rows), self);
  assert.equal(reused.survivors.length + reused.unverified.length, 3, "reused PID grants no subtree exemption");
});

test("#3031 exclusive-idle ignores zombies and trusts only the helper's own descendants when explicitly requested", () => {
  const rows = { ...base(), 20: { state: "Z" }, 30: { ppid: self }, 31: { ppid: 30 } };
  assert.equal(scanOnce(request(), table(rows), self).survivors.length, 2);
  const helper = scanOnce(request(), table(rows), self, true);
  assert.deepEqual(helper, { kill: [], survivors: [], unverified: [] });
});

test("#3031 legacy workerWide keeps its narrower cooperative classification", () => {
  const req = request();
  delete (req as Partial<ExclusiveRequest>).exclusiveIdle;
  delete (req as Partial<ExclusiveRequest>).exclusiveIdleRoots;
  const result = scanOnce({ ...req, workerWide: true }, table({ ...base(), 20: {} }), self);
  assert.deepEqual(result, { kill: [], survivors: [], unverified: [] });
});

for (const pid of [1, self]) {
  test(`#3031 exclusive-idle cannot exempt the worker's whole tree through root ${pid}`, () => {
    const result = scanOnce(request([{ pid, startTime: 100 }]), table({ ...base(), 20: { ppid: self } }), self);
    assert.ok(result.tableError);
    assert.deepEqual(result.kill, []);
  });
}

test("#3031 exclusive-idle refuses a new orphan appearing during the inventory", () => {
  const rows: Record<number, Process> = base();
  const view = table(rows), list = view.listPids;
  let calls = 0;
  view.listPids = () => {
    if (++calls === 2) rows[20] = { uid: 10001 };
    return list();
  };
  const result = scanOnce(request(), view, self);
  assert.ok(result.tableError);
  assert.deepEqual(result.kill, []);
});

test("#3031 exclusive-idle never ignores a live PID reused after a zombie status read", () => {
  const rows: Record<number, Process> = { ...base(), 20: { state: "Z", start: 100 } };
  const view = table(rows), status = view.readStatus;
  view.readStatus = pid => {
    const observed = status(pid);
    if (pid === 20) rows[pid] = { state: "S", start: 200 };
    return observed;
  };
  const result = scanOnce(request(), view, self);
  assert.ok(result.tableError || result.unverified.length || result.survivors.length,
    "zombie observation must not exempt a replacement live identity");
  assert.deepEqual(result.kill, []);
});

test("#3031 bootstrap enrollment is exact-only and never exempts later descendants", () => {
  const req = request();
  req.exclusiveIdleBootstrap = [{ pid: 30, parentPid: self, startTime: 200, executable: "/image/esbuild" }];
  const rows: Record<number, Process> = { ...base(), 30: { ppid: self, start: 200 }, 31: { ppid: 30 } };
  const result = scanOnce(req, table(rows), self);
  assert.deepEqual(result.survivors.map(p => p.pid), [31]);
  assert.deepEqual(result.kill, []);
  delete rows[31];
  assert.deepEqual(scanOnce(req, table(rows), self), { kill: [], survivors: [], unverified: [] });
  for (const change of [{ start: 201 }, { ppid: 1 }, { executable: "/other/program" }, { unreadable: true }]) {
    const bad = scanOnce(req, table({ ...rows, 30: { ...rows[30], ...change } }), self);
    assert.ok(bad.tableError || bad.unverified.length || bad.survivors.length);
  }
});

test("#3031 bootstrap snapshot permits only one exact initial native compiler child", { skip: process.platform !== "linux" }, () => {
  const expected = () => "/image/esbuild";
  assert.deepEqual(snapshotMaintenanceBootstrap(table(base()), expected, self), []);
  assert.deepEqual(snapshotMaintenanceBootstrap(table({ ...base(), 30: { ppid: self, start: 200 } }), expected, self), [
    { pid: 30, parentPid: self, startTime: 200, executable: "/image/esbuild" },
  ]);
  for (const rows of [
    { ...base(), 30: { ppid: self, executable: "/other/program" } },
    { ...base(), 30: { ppid: self }, 31: { ppid: self } },
  ]) assert.equal(snapshotMaintenanceBootstrap(table(rows), expected, self), undefined);
});

test("#3031 exclusive-idle refuses a hidepid-restricted or unproven proc mount", () => {
  const line = (options: string, point = "/proc") => `22 1 0:5 / ${point} rw - proc proc ${options}\n`;
  assert.equal(procMountShowsAllProcesses(line("rw")), true);
  assert.equal(procMountShowsAllProcesses(line("rw,hidepid=0")), true);
  assert.equal(procMountShowsAllProcesses(line("rw,hidepid=off,subset=pid")), true);
  for (const info of [line("rw,hidepid=2"), line("rw,hidepid=invisible"), line("rw,hidepid=1"), line("rw,hidepid=noaccess"),
    line("rw") + line("rw,hidepid=2"), line("rw", "/host/proc"), "", "garbage"])
    assert.equal(procMountShowsAllProcesses(info), false, info);
  const hidden = table(base());
  hidden.readProcMountInfo = () => line("rw,hidepid=2");
  assert.ok(scanOnce(request(), hidden, self).tableError);
  const unknown = table(base());
  delete unknown.readProcMountInfo;
  assert.ok(scanOnce(request(), unknown, self).tableError);
  assert.deepEqual(scanOnce(request(), table(base()), self), { kill: [], survivors: [], unverified: [] });
});

test("#3031 an enrolled bootstrap pid that became a zombie keeps its exact start and parent", () => {
  const req = request();
  req.exclusiveIdleBootstrap = [{ pid: 30, parentPid: self, startTime: 200, executable: "/image/esbuild" }];
  const exe = (rows: Record<number, Process>) => {
    const view = table(rows);
    view.readExecutable = () => { throw Object.assign(new Error("zombie exe"), { code: "ENOENT" }); };
    return view;
  };
  assert.deepEqual(scanOnce(req, exe({ ...base(), 30: { ppid: self, start: 200, state: "Z" } }), self),
    { kill: [], survivors: [], unverified: [] });
  for (const change of [{ ppid: 1 }, { start: 201 }])
    assert.ok(scanOnce(req, exe({ ...base(), 30: { ppid: self, start: 200, state: "Z", ...change } }), self).tableError);
});

const exclusive = (scans: number) => ({ ...request(), exclusiveIdleScans: scans });

test("#3031 exclusive-idle reap uses a fixed scan count, never the reap deadline loop, and never signals", async () => {
  let reads = 0, kills = 0, sleeps = 0;
  const rows: Record<number, Process> = { ...base(), 20: { uid: 10001 } };
  const view = table(rows), status = view.readStatus;
  view.readStatus = pid => { if (pid === 20) reads++; return status(pid); };
  const deps = { viaHelper: false, table: view, selfPid: self, kill: () => { kills++; }, sleep: async () => { sleeps++; } };
  for (const scans of [1, 3]) {
    reads = 0; sleeps = 0;
    const result = await reapRunProcesses(exclusive(scans), deps);
    assert.equal(result.state, "survivors", result.detail);
    assert.equal(reads, scans);
    assert.equal(sleeps, scans - 1);
  }
  assert.equal(kills, 0);
  // A transient helper that exits between scans clears within the fixed count.
  reads = 0;
  view.readStatus = pid => { if (pid === 20) { reads++; delete rows[20]; } return status(pid); };
  rows[20] = { uid: 10001 };
  assert.equal((await reapRunProcesses(exclusive(3), deps)).state, "quiescent");
  for (const scans of [0, 4, 1.5, Number.NaN])
    assert.equal((await reapRunProcesses(exclusive(scans), deps)).state, "unverified");
  const missing = request();
  assert.equal((await reapRunProcesses(missing, deps)).state, "unverified");
  assert.equal((await reapRunProcesses(exclusive(1), { ...deps, viaHelper: true })).detail,
    "exclusive idle proof is worker-local");
  assert.equal(kills, 0);
});

async function chargedSlot(t: { after(fn: () => Promise<void>): void }) {
  const dir = await fs.promises.mkdtemp(path.resolve("../.uzi/scratch/exclusive-idle-"));
  const ledger = new RecoveryMaintenance({ root: path.join(dir, "recovery-maintenance"), workerToken: "exclusive-fixture" });
  const admitted = await ledger.admit({
    identity: { run: "run", generation: 1, hold: "hold", repo: "repo", custodian: "worker",
      bareBasename: "repo.git", origin: "a".repeat(64), originalSource: "b".repeat(64), sourceConfig: "c".repeat(64) },
    config: RecoveryMaintenance.configuration(), exactCustody: true, terminalNonStopped: true, source: "b".repeat(64),
  });
  assert.ok(admitted.ok);
  const slot = admitted.value;
  t.after(async () => { await ledger.finish(slot, "transient"); await fs.promises.rm(dir, { recursive: true, force: true }); });
  return { run: <T>(action: () => T) => MaintenanceResources.run(slot, action) };
}

test("#3031 in-process exclusive-idle charges per-read admin work to the local envelope", async t => {
  const f = await chargedSlot(t);
  // Three PIDs, listed twice (inventory plus final reread): 6 admin entries before any per-read work.
  const rows: Record<number, Process> = { ...base(), 20: { state: "Z" } };
  const run = (adminEntries: number) => f.run(() => reapRunProcesses(exclusive(1), {
    viaHelper: false, table: table(rows), selfPid: self, sleep: async () => {},
    producerWorkLimits: { adminEntries, metadataBytes: 16 * 1024 ** 2 } }));
  assert.equal((await run(6)).state, "unverified", "stat/status reads must also be charged");
  assert.equal((await run(128)).state, "quiescent");
});

test("#3031 only the exclusive-idle proof may use a producer envelope inside uncharged discovery", async () => {
  const rows: Record<number, Process> = base();
  const limits = { adminEntries: 128, metadataBytes: 16 * 1024 ** 2 };
  const deps = { viaHelper: false, table: table(rows), selfPid: self, sleep: async () => {}, producerWorkLimits: limits };
  assert.equal((await MaintenanceResources.runDiscovery(() => reapRunProcesses(exclusive(1), deps))).state, "quiescent");
  const ordinary = { ...request(), targetKey: "test", targetPaths: ["/test"] };
  delete (ordinary as Partial<ExclusiveRequest>).exclusiveIdle;
  assert.equal((await MaintenanceResources.runDiscovery(() => reapRunProcesses(ordinary, deps))).state, "unverified");
});

test("#3031 proc mount parser refuses incomplete rows and a non-proc mount over /proc", () => {
  const proc = "22 1 0:5 / /proc rw - proc proc rw\n";
  assert.equal(procMountShowsAllProcesses(proc), true);
  for (const info of ["22 1 0:5 / /proc rw - proc proc\n", proc + "40 22 0:9 / /proc rw - tmpfs tmpfs rw\n",
    "22 1 0:5 / /proc rw - proc proc rw extra\n", "22 1 0:5 /proc rw - proc proc rw\n", proc + "broken line without separator\n"])
    assert.equal(procMountShowsAllProcesses(info), false, info);
  const tmpfs = "40 1 0:9 / /proc rw - tmpfs tmpfs rw\n", second = "41 22 0:5 / /proc rw - proc proc rw\n";
  for (const info of [tmpfs + proc, proc + tmpfs, proc + second, second + proc])
    assert.equal(procMountShowsAllProcesses(info), false, "stacked /proc mounts are ambiguous in any order");
});

test("#3031 uncharged discovery gets exactly one exclusive-idle pass", async t => {
  let reads = 0;
  const view = table(base()), list = view.listPids;
  view.listPids = () => { reads++; return list(); };
  const deps = { viaHelper: false, table: view, selfPid: self, sleep: async () => {} };
  for (const scans of [2, 3]) {
    reads = 0;
    assert.equal((await MaintenanceResources.runDiscovery(() => reapRunProcesses(exclusive(scans), deps))).state, "unverified");
    assert.equal(reads, 0, "a refused discovery request reads nothing");
  }
  assert.equal((await MaintenanceResources.runDiscovery(() => reapRunProcesses(exclusive(1), deps))).state, "quiescent");
  const f = await chargedSlot(t);
  assert.equal((await f.run(() => reapRunProcesses(exclusive(3), deps))).state, "quiescent", "a charged slot may rescan");
});

test("#3031 the proc mount read is charged as one admin entry", async t => {
  const f = await chargedSlot(t);
  // pids [1, 10, 20(zombie)]: 3 listed + 1 mount + 2 own ancestry stats + 3 for the zombie
  // (stat, status, stat) + 3 relisted + 3 final stats = 15 admin entries.
  const rows: Record<number, Process> = { ...base(), 20: { state: "Z" } };
  const run = (adminEntries: number) => f.run(() => reapRunProcesses(exclusive(1), {
    viaHelper: false, table: table(rows), selfPid: self, sleep: async () => {},
    producerWorkLimits: { adminEntries, metadataBytes: 16 * 1024 ** 2 } }));
  assert.equal((await run(14)).state, "unverified");
  assert.equal((await run(15)).state, "quiescent");
});
