import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { spawn } from "node:child_process";
import {
  LiveAttemptRegistry,
  inventoryClearVerdict,
  newRunAttempt,
  procfsTableAt,
  quiesceRunAttempt,
  reapProcesses,
  reapRunProcesses,
  setQuiescenceViewForTests,
  type HelperSpawn,
  type ProcessQuiescence,
  type ScanRequest,
} from "../src/run-quiescence.js";
import { workerSpawnNonce } from "../src/worker-spawn-mark.js";
import { makeFakeProcRoot, plantFakeProc, plantInScope, plantUnreadableUnattributed } from "./fake-proc.js";
import { restoreHermeticView } from "./setup/hermetic-proc.js";

// Observe-only scanner primitives: the reap loop never signals, ends once nothing is unverified, the
// helper request carries observeOnly / deadlineAt and rejects malformed values, and
// inventoryClearVerdict fails closed.

const HAS_LINUX = process.platform === "linux";
const ME = process.getuid?.() ?? 0;
const CLONE = "/data/runner/github.com+o+r/issue-17";
const own = newRunAttempt("run-17", 1, CLONE, () => []);
const TERMINAL_MARKER = "run-99:1";

let root: string;
beforeEach(() => {
  root = makeFakeProcRoot();
  setQuiescenceViewForTests({ procRoot: root });
});
afterEach(() => {
  restoreHermeticView();
  fs.rmSync(root, { recursive: true, force: true });
});

function req(extra: Partial<ScanRequest> = {}): ScanRequest {
  return {
    mode: "own",
    targetUid: ME,
    targetKey: own.cloneKey,
    targetPaths: [CLONE],
    ownMarker: own.marker,
    liveMarkers: [],
    liveRoots: [],
    workerNonce: workerSpawnNonce(),
    ...extra,
  };
}

/** A reap over the fake root with a counting clock/table and a kill spy that removes the entry. */
async function observedReap(r: ScanRequest) {
  const killed: number[] = [];
  let t = 0;
  let scans = 0;
  const base = procfsTableAt(root);
  const table = { ...base, listPids: () => { scans += 1; return base.listPids(); } };
  const result = await reapProcesses(r, {
    table,
    selfPid: 1,
    now: () => t,
    sleep: async (ms) => { t += ms; },
    deadlineMs: 5_000,
    kill: (pid) => { killed.push(pid); fs.rmSync(path.join(root, String(pid)), { recursive: true, force: true }); },
  });
  return { result, killed, scans: () => scans, elapsedFake: () => t };
}

describe("observeOnly reap loop", { skip: !HAS_LINUX }, () => {
  it("(a) a persistent terminal-attempt candidate is never killed and the loop ends after one scan", async () => {
    plantInScope(root, 70, CLONE, { marker: TERMINAL_MARKER, cloneKey: own.cloneKey });
    const o = await observedReap(req({ observeOnly: true }));
    assert.deepEqual(o.killed, []);
    assert.equal(o.scans(), 1);
    assert.equal(o.elapsedFake(), 0, "no rescan sleep: well under the deadline");
    assert.equal(fs.existsSync(path.join(root, "70")), true);
    assert.deepEqual(o.result.processes.map((p) => [p.pid, p.reason]), [[70, "terminal_attempt:kill_unconfirmed"]]);
    assert.equal(inventoryClearVerdict(o.result), "clear");
  });

  it("(a) control: without observeOnly the same request kills the candidate", async () => {
    plantInScope(root, 70, CLONE, { marker: TERMINAL_MARKER, cloneKey: own.cloneKey });
    const o = await observedReap(req());
    assert.deepEqual(o.killed, [70]);
    assert.equal(o.result.state, "quiescent");
  });

  it("(b) an unattributed_in_scope process is not_clear and nothing is killed", async () => {
    plantInScope(root, 71, CLONE);
    plantInScope(root, 72, CLONE, { marker: TERMINAL_MARKER, cloneKey: own.cloneKey });
    const o = await observedReap(req({ mode: "seed", observeOnly: true }));
    assert.deepEqual(o.killed, []);
    assert.equal(o.result.processes.some((p) => p.reason === "unattributed_in_scope"), true);
    assert.equal(inventoryClearVerdict(o.result), "not_clear");
  });

  it("a persistent unreadable_unattributed process settles after at most 3 scans, keeps its reasons, and is not_clear (unverified keeps the exclusion)", async () => {
    plantUnreadableUnattributed(root, 73);
    plantInScope(root, 74, CLONE, { marker: TERMINAL_MARKER, cloneKey: own.cloneKey });
    const o = await observedReap(req({ observeOnly: true }));
    assert.equal(o.scans(), 3, "first scan plus the two settle rescans");
    assert.ok(o.elapsedFake() < 5_000, "well before the 5 s deadline");
    assert.deepEqual(o.killed, []);
    assert.equal(o.result.state, "unverified");
    assert.notEqual(o.result.detail, "process proof deadline");
    assert.deepEqual(o.result.processes.map((p) => p.reason).sort(), ["terminal_attempt:kill_unconfirmed", "unreadable_unattributed"]);
    assert.equal(inventoryClearVerdict(o.result), "not_clear");
  });

  it("unreadable_unattributed plus unattributed_in_scope is not_clear", async () => {
    plantUnreadableUnattributed(root, 75);
    plantInScope(root, 76, CLONE);
    const o = await observedReap(req({ mode: "seed", observeOnly: true }));
    assert.equal(o.scans(), 3);
    assert.equal(o.result.state, "unverified");
    assert.equal(o.result.processes.some((p) => p.reason === "unattributed_in_scope"), true);
    assert.equal(inventoryClearVerdict(o.result), "not_clear");
  });
});

// A real helper child reached through a wrapper that can rewrite the stdin request before the helper sees it.
const REWRAP = `
const { spawnSync } = require("node:child_process");
const [cmd, patch, record] = JSON.parse(process.argv[1]);
let buf = "";
process.stdin.on("data", (c) => { buf += c; });
process.stdin.on("end", () => {
  const req = Object.assign(JSON.parse(buf), JSON.parse(patch));
  if (record) require("node:fs").writeFileSync(record, buf);
  const r = spawnSync(cmd[0], cmd.slice(1), { input: JSON.stringify(req), stdio: ["pipe", "pipe", "inherit"], encoding: "utf8" });
  process.stdout.write(r.stdout || "");
  process.exit(r.status === null ? 1 : r.status);
});`;

function rewrapSpawn(patch: Record<string, unknown>, record?: string): HelperSpawn {
  return (command, args, opts) => spawn(process.execPath, ["-e", REWRAP, JSON.stringify([[command, ...args], JSON.stringify(patch), record ?? ""])], {
    cwd: opts.cwd, env: opts.env, stdio: ["pipe", "pipe", "pipe"],
  });
}

describe("helper request validation (isScanRequest, through the real helper)", { skip: !HAS_LINUX }, () => {
  it("control: a well-formed observeOnly + deadlineAt request is honoured", async () => {
    const r = await reapRunProcesses(req(), { viaHelper: true, spawnHelper: rewrapSpawn({ observeOnly: true, deadlineAt: Date.now() + 60_000 }) });
    assert.equal(r.state, "quiescent", r.detail);
  });

  for (const [name, patch] of [
    ["non-boolean observeOnly", { observeOnly: "yes" }],
    ["numeric observeOnly", { observeOnly: 1 }],
    ["string deadlineAt", { deadlineAt: "soon" }],
    ["zero deadlineAt", { deadlineAt: 0 }],
    ["negative deadlineAt", { deadlineAt: -5 }],
    ["null deadlineAt", { deadlineAt: null }],
  ] as const) {
    it(`rejects ${name}`, async () => {
      const r = await reapRunProcesses(req(), { viaHelper: true, spawnHelper: rewrapSpawn(patch) });
      assert.equal(r.state, "unverified");
      assert.equal(r.detail, "helper request invalid");
    });
  }
});

describe("observeOnly and deadlineAt reach the helper", { skip: !HAS_LINUX }, () => {
  it("(d) quiesceRunAttempt with forceHelper serializes observeOnly and deadlineAt, and never kills", async () => {
    plantInScope(root, 80, CLONE, { marker: TERMINAL_MARKER, cloneKey: own.cloneKey });
    const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-obs-"));
    try {
      const record = path.join(tmp, "request.json");
      const registry = new LiveAttemptRegistry();
      registry.add(own);
      const deadlineAt = Date.now() + 30_000;
      const out = await quiesceRunAttempt({
        mode: "own", attempt: own, cloneKey: own.cloneKey, targetPaths: [CLONE], processes: true,
        dockerHost: undefined, registry, forceHelper: true, observeOnly: true, deadlineAt,
      }, { reap: { spawnHelper: rewrapSpawn({}, record) } });
      const sent = JSON.parse(fs.readFileSync(record, "utf8")) as ScanRequest;
      assert.equal(sent.observeOnly, true);
      assert.equal(sent.deadlineAt, deadlineAt);
      assert.equal(fs.existsSync(path.join(root, "80")), true, "observe-only helper never signals");
      assert.equal(inventoryClearVerdict(out.process), "clear", out.process?.detail ?? "no process verdict");
    } finally {
      fs.rmSync(tmp, { recursive: true, force: true });
    }
  });

  for (const [name, spawnHelper] of [
    ["the spawn throws", ((): never => { throw new Error("spawn failed (test)"); }) as HelperSpawn],
    ["the helper binary does not exist", ((_c, _a, opts) => spawn("/nonexistent-uzi-helper-4632", [], { cwd: opts.cwd, env: opts.env, stdio: ["pipe", "pipe", "pipe"] })) as HelperSpawn],
  ] as const) {
    it(`a forced-helper observe-only scan whose helper fails (${name}) is unverified, never clear, and nothing is signalled`, async () => {
      // A process the scan could otherwise only observe: it must stay alive and the verdict must not be clear.
      plantInScope(root, 83, CLONE, { marker: TERMINAL_MARKER, cloneKey: own.cloneKey });
      const registry = new LiveAttemptRegistry();
      registry.add(own);
      const out = await quiesceRunAttempt({
        mode: "own", attempt: own, cloneKey: own.cloneKey, targetPaths: [CLONE], processes: true,
        dockerHost: undefined, registry, forceHelper: true, observeOnly: true, deadlineAt: Date.now() + 30_000,
      }, { reap: { spawnHelper } });
      assert.equal(out.process?.state, "unverified", JSON.stringify(out.process));
      assert.deepEqual(out.process?.processes, [], "a helper failure names no process");
      assert.deepEqual(out.process?.killed ?? [], []);
      assert.equal(inventoryClearVerdict(out.process), "not_clear", out.process?.detail ?? "no process verdict");
      assert.equal(fs.existsSync(path.join(root, "83")), true, "nothing is signalled");
    });
  }

  it("an in-process observe-only probe (no helper, no deadline) under uid split never files an unreadable process for a kill, so it is unsignalled and (unverified) not_clear", async () => {
    const previous = process.env.UZI_UID_SPLIT;
    process.env.UZI_UID_SPLIT = "1";
    plantUnreadableUnattributed(root, 82);
    try {
      // In-process (the fake proc root is not visible to a real helper child); no other claim, empty registry.
      const out = await quiesceRunAttempt({
        mode: "own", attempt: undefined, cloneKey: own.cloneKey, targetPaths: [CLONE], processes: true,
        dockerHost: undefined, registry: new LiveAttemptRegistry(), observeOnly: true,
        otherClaimInFlight: false,
      }, { targetUid: ME, reap: { viaHelper: false } }); // no deadlineAt: a gate deadline turns the fake EISDIR into a table failure
      assert.deepEqual(out.process?.killed ?? [], []);
      assert.equal(fs.existsSync(path.join(root, "82")), true, "nothing is killed");
      assert.equal(out.process?.processes.some((p) => p.reason.startsWith("unreadable_no_other_claim")), false,
        JSON.stringify(out.process?.processes));
      assert.equal(out.process?.state, "unverified", JSON.stringify(out.process));
      assert.equal(inventoryClearVerdict(out.process), "not_clear", out.process?.detail ?? "no process verdict");
    } finally {
      if (previous === undefined) delete process.env.UZI_UID_SPLIT;
      else process.env.UZI_UID_SPLIT = previous;
    }
  });

  it("an opted-in (helperDeadline, the inventory shape) non-observe scan serializes the deadline but not observeOnly", async () => {
    const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-obs-"));
    try {
      const record = path.join(tmp, "request.json");
      const registry = new LiveAttemptRegistry();
      registry.add(own);
      const deadlineAt = Date.now() + 30_000;
      await quiesceRunAttempt({
        mode: "own", attempt: own, cloneKey: own.cloneKey, targetPaths: [CLONE], processes: true,
        dockerHost: undefined, registry, forceHelper: true, helperDeadline: true, deadlineAt,
      }, { reap: { spawnHelper: rewrapSpawn({}, record) } });
      const sent = JSON.parse(fs.readFileSync(record, "utf8")) as Record<string, unknown>;
      assert.equal("observeOnly" in sent, false);
      assert.equal(typeof sent.deadlineAt, "number");
      assert.ok((sent.deadlineAt as number) <= deadlineAt, "the serialized deadline never exceeds the gate deadline");
    } finally {
      fs.rmSync(tmp, { recursive: true, force: true });
    }
  });

  it("negative control: a non-observe, non-opted-in deadlined helper scan (the #3980 checkpoint shape) serializes NO deadlineAt", async () => {
    const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-obs-"));
    try {
      const record = path.join(tmp, "request.json");
      const registry = new LiveAttemptRegistry();
      registry.add(own);
      await quiesceRunAttempt({
        mode: "own", attempt: own, cloneKey: own.cloneKey, targetPaths: [CLONE], processes: true,
        dockerHost: undefined, registry, forceHelper: true, deadlineAt: Date.now() + 30_000,
      }, { reap: { spawnHelper: rewrapSpawn({}, record) } });
      const sent = JSON.parse(fs.readFileSync(record, "utf8")) as Record<string, unknown>;
      assert.equal("observeOnly" in sent, false);
      assert.equal("deadlineAt" in sent, false, "a gate checkpoint's helper scan stays byte-for-byte what it was before #4632");
    } finally {
      fs.rmSync(tmp, { recursive: true, force: true });
    }
  });

  it("the helper honours a serialized deadline on a NON-observe signalling request: past it nothing is signalled, and without it the same candidate is killed", async () => {
    // Pins helperMain's req.deadlineAt -> reapProcesses deps.deadlineAt mapping for a request without observeOnly.
    // deadlineAt: 1 is already past when the helper starts, so no timing is involved.
    const run = async (patch: Record<string, unknown>) => {
      plantInScope(root, 90, CLONE, { marker: TERMINAL_MARKER, cloneKey: own.cloneKey });
      return reapRunProcesses(req(), { viaHelper: true, spawnHelper: rewrapSpawn(patch) });
    };
    const control = await run({});
    assert.equal(control.state, "quiescent", control.detail);
    assert.equal(fs.existsSync(path.join(root, "90")), false, "control: without a deadline the helper kills the candidate");
    const r = await run({ deadlineAt: 1 });
    assert.equal(r.state, "unverified");
    assert.equal(r.detail, "process proof deadline");
    assert.deepEqual(r.killed, []);
    assert.equal(fs.existsSync(path.join(root, "90")), true, "nothing is signalled at or after the deadline");
  });

  describe("an attributed non-dumpable peer (the Codex supervisor shape) with unreadable environ", () => {
    const PEER = 91;
    const peerRoot = { pid: PEER, startTime: 4242 };
    const plantPeer = (): void => plantFakeProc(root, PEER, { uid: ME, comm: "codex-supervisor", env: "unreadable", cwd: "/", startTime: peerRoot.startTime });
    const scan = (): ScanRequest => req({ liveRoots: [peerRoot] });
    const deadlineAt = (): number => Date.now() + 60_000;

    it("a deadlined helper scan that is NOT opted in (a #3980 checkpoint) stays quiescent: the peer is attributed", async () => {
      plantPeer();
      const r = await reapRunProcesses(scan(), { viaHelper: true, deadlineAt: deadlineAt(), spawnHelper: rewrapSpawn({}) });
      assert.equal(r.state, "quiescent", JSON.stringify(r));
    });

    it("control: without any deadline the same peer is attributed and the scan is quiescent", async () => {
      plantPeer();
      const r = await reapRunProcesses(scan(), { viaHelper: true, spawnHelper: rewrapSpawn({}) });
      assert.equal(r.state, "quiescent", JSON.stringify(r));
    });

    it("an opted-in (inventory) deadlined helper scan is unverified with no processes: the EACCES-class read is a table failure", async () => {
      plantPeer();
      const r = await reapRunProcesses(scan(), { viaHelper: true, deadlineAt: deadlineAt(), forwardDeadlineToHelper: true, spawnHelper: rewrapSpawn({}) });
      assert.equal(r.state, "unverified");
      assert.deepEqual(r.processes, []);
      assert.match(r.detail, /EISDIR/);
      assert.equal(inventoryClearVerdict(r), "not_clear");
    });
  });

  it("the helper honours the serialized deadline while an unverified process persists", async () => {
    plantUnreadableUnattributed(root, 81);
    const started = Date.now();
    const r = await reapRunProcesses(req({ observeOnly: true }), {
      viaHelper: true, deadlineAt: Date.now() + 1_500, helperTimeoutMs: 20_000,
    });
    assert.equal(r.state, "unverified");
    assert.ok(Date.now() - started < 12_000, "ended near the deadline, not the helper timeout");
  });
});

describe("(e) inventoryClearVerdict", () => {
  const q = (state: ProcessQuiescence["state"], reasons: string[]): ProcessQuiescence => ({
    state, killed: [], detail: "",
    processes: reasons.map((reason, i) => ({ pid: 100 + i, uid: 1, comm: "c", cwd: "/", reason })),
  });
  const table: Array<[string, ProcessQuiescence | undefined, "clear" | "not_clear"]> = [
    ["undefined", undefined, "not_clear"],
    ["quiescent", q("quiescent", []), "clear"],
    ["unverified, no processes (table/helper/deadline error)", q("unverified", []), "not_clear"],
    ["unverified with only ignored reasons", q("unverified", ["terminal_attempt"]), "not_clear"],
    ["unverified unreadable_unattributed alone", q("unverified", ["unreadable_unattributed"]), "not_clear"],
    ["unverified unreadable_unattributed plus ignored", q("unverified", ["unreadable_unattributed", "protected_attempt"]), "not_clear"],
    ["unverified unreadable_unattributed plus unattributed_in_scope", q("unverified", ["unreadable_unattributed", "unattributed_in_scope"]), "not_clear"],
    ["unverified unreadable_unattributed plus unknown", q("unverified", ["unreadable_unattributed", "status_unreadable"]), "not_clear"],
    ["unverified status_unreadable", q("unverified", ["status_unreadable"]), "not_clear"],
    ["live_attempt_conflict", q("survivors", ["live_attempt_conflict"]), "clear"],
    ["protected_attempt", q("survivors", ["protected_attempt"]), "clear"],
    ["terminal_attempt", q("survivors", ["terminal_attempt"]), "clear"],
    ["terminal_attempt:kill_unconfirmed", q("survivors", ["terminal_attempt:kill_unconfirmed"]), "clear"],
    ["all ignored together", q("survivors", ["live_attempt_conflict", "protected_attempt", "terminal_attempt:kill_unconfirmed"]), "clear"],
    ["unattributed_in_scope", q("survivors", ["unattributed_in_scope"]), "not_clear"],
    ["unattributed_in_scope among ignored", q("survivors", ["terminal_attempt", "unattributed_in_scope"]), "not_clear"],
    ["unmarked_in_scope:kill_unconfirmed", q("survivors", ["unmarked_in_scope:kill_unconfirmed"]), "not_clear"],
    ["own_attempt:kill_unconfirmed", q("survivors", ["own_attempt:kill_unconfirmed"]), "not_clear"],
    ["unreadable_unattributed", q("survivors", ["unreadable_unattributed"]), "not_clear"],
    ["unknown reason", q("survivors", ["something_new"]), "not_clear"],
    ["empty reason", q("survivors", [""]), "not_clear"],
    ["ignored name as a suffix only", q("survivors", ["x:terminal_attempt"]), "not_clear"],
  ];
  for (const [name, input, want] of table) {
    it(`${name} -> ${want}`, () => assert.equal(inventoryClearVerdict(input), want));
  }
});
