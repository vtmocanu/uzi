import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import net from "node:net";
import os from "node:os";
import path from "node:path";
import { execFileSync, spawnSync } from "node:child_process";
import { formatAttemptId } from "../src/attempt-path.js";
import { DindPruneGate } from "../src/dind-prune.js";
import { GitCache } from "../src/git.js";
import { RetainedSourceServer, type RetainedSourceServerDeps } from "../src/ops/retained-source-server.js";
import type { RecoveryHoldsResponse, ClaimResponse, RunOwnershipResponse } from "../src/protocol.js";
import type { QuiesceRunOutcome, QuiesceRunRequest } from "../src/run-quiescence.js";
import { RunRunner } from "../src/runner.js";
import { Worker } from "../src/worker.js";
import { loadConfig } from "../src/config.js";
import { RequestError, type WorkerClient } from "../src/client.js";
import type { ChatRunner } from "../src/chat-runner.js";
import type { JudgeRunner } from "../src/judge-runner.js";
import type { ReviewRunner } from "../src/review-runner.js";
import type { StatsCollector } from "../src/stats.js";
import type { Logger } from "../src/log.js";
import { resetResidueQuarantineAfterEach } from "./setup/hermetic-proc.js";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { noProofReseed, nullLogger, recordingLogger, testGitCacheOptions } from "./helpers.js";
import { fixtureFetchTracking } from "./runner-tracking-fixture.js";
import { mkdtempForSocket } from "./short-sock-dir.js";

// issue #1786 M0b — the operator retained-source channel, against a REAL GitCache, a REAL RunRunner
// reservation, the REAL Worker claim loops and a REAL Unix socket. The destructive sequence is
//   gate closed -> in-flight claims drained -> idle proven -> run reservation installed ->
//   server holds read -> GitCache bare lock -> deletion -> reservation released -> gate reopened.
// The only production hold insertion is the claim CTE, so a closed gate + drained claims + proven
// idleness + a held run reservation keep the pre-lock holds snapshot current through the deletion.

resetResidueQuarantineAfterEach();

const GIT_ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0" };
const tick = (ms = 2): Promise<void> => new Promise((r) => setTimeout(r, ms));
async function until(cond: () => boolean, what: string, max = 3000): Promise<void> {
  for (let i = 0; i < max && !cond(); i++) await tick();
  assert.ok(cond(), `timed out waiting for: ${what}`);
}
function deferred<T = void>(): { promise: Promise<T>; resolve: (v: T) => void } {
  let resolve!: (v: T) => void;
  const promise = new Promise<T>((r) => (resolve = r));
  return { promise, resolve };
}
/** Bound a whole scenario so a deadlock fails the test instead of hanging it. */
async function within<T>(ms: number, what: string, p: Promise<T>): Promise<T> {
  let timer: NodeJS.Timeout | undefined;
  try {
    return await Promise.race([p, new Promise<never>((_, rej) => { timer = setTimeout(() => rej(new Error(`deadline: ${what}`)), ms); })]);
  } finally {
    if (timer) clearTimeout(timer);
  }
}

let fx: Fixture;
let git: GitCache;
let bare: string;
let scratch: string[] = [];

function gitIn(dir: string, args: string[]): string {
  return execFileSync("git", ["-C", dir, ...args], { env: GIT_ENV, encoding: "utf8", stdio: ["pipe", "pipe", "pipe"] }).trim();
}
function cfgAll(key: string): string[] {
  try {
    return gitIn(bare, ["config", "--local", "--get-all", key]).split("\n").filter((l) => l !== "");
  } catch {
    return [];
  }
}
function journalRaw(branch: string): string {
  try {
    return gitIn(bare, ["config", "--local", `uzi-recovery.${branch}.clone`]);
  } catch {
    return "";
  }
}
/** A child node process must not inherit the parent test runner's context. */
function childEnv(): NodeJS.ProcessEnv {
  const env = { ...process.env };
  delete env.NODE_TEST_CONTEXT;
  return env;
}
const hex16 = (n: number): string => n.toString(16).padStart(16, "0");
const attemptIdAt = (n: number, gen = 1): string => formatAttemptId(new Date(Date.UTC(2026, 0, 1, 0, 0, n)), gen, hex16(n + 1));

interface Attempt { branch: string; key: string; runId: string; attemptId: string; path: string; clonePath: string }

/** A REAL stopped attempt retained by the production helper (ledger `reclaimed`, bytes left). */
async function retainedAttempt(runId: string, n: number, iid = 2512): Promise<Attempt> {
  const branch = `agent/issue-${iid}`;
  const key = `issue-${iid}`;
  const attemptId = attemptIdAt(n);
  const clone = await git.runnerCloneForBranch(bare, branch, key, noProofReseed, runId, false, undefined,
    { attemptId, isLive: () => false, beforeSeed: async () => {} });
  await git.markRecoveryCapture(bare, clone.path, branch, runId, attemptId);
  fs.writeFileSync(path.join(clone.path, "unpreserved.txt"), "retained bytes\n");
  const a: Attempt = { branch, key, runId, attemptId, path: clone.path, clonePath: clone.path };
  assert.equal(await git.retainStoppedAttempt(bare, branch, a), "retained");
  return a;
}

const EP_RUN = "run-2700";
const aid = (n: number): string => `20261008T171401Z-g${n}-0123456789abcdef`;
/** A stopped predecessor-capture episode (canonical predecessor + ready attempt successor). */
async function buildEpisode() {
  const iid = 2700;
  const branch = `agent/issue-${iid}`;
  const key = `issue-${iid}`;
  const source = await git.createOrAttachRunnerClone(bare, iid, noProofReseed, EP_RUN);
  await git.markRecoveryCapture(bare, source.path, branch, EP_RUN);
  const src = { runId: EP_RUN, clonePath: source.path };
  const reservation = await git.reserveRecoveryIteration(bare, branch, key, src);
  fs.writeFileSync(path.join(source.path, "work.txt"), "retained work\n");
  gitIn(source.path, ["add", "work.txt"]);
  gitIn(source.path, ["commit", "-m", "milestone"]);
  await fixtureFetchTracking(git, bare, source.path, branch, EP_RUN);
  const tip = gitIn(source.path, ["rev-parse", "HEAD"]);
  await git.recordRecoveryCapture(bare, branch, key, src, reservation.attempts, tip);
  const successor = await git.prepareRecoverySuccessor(bare, branch, key, src, reservation.attempts, aid(2));
  return { branch, key, tip, sourcePath: source.path, successorPath: successor.path };
}

// ─── the rig: server + gate + a REAL RunRunner reservation + a recording event log ───────────

const QUIESCENT = { process: { state: "quiescent" }, docker: { state: "not_wired" } } as unknown as QuiesceRunOutcome;

interface RigOptions {
  holds?: (runId: string) => RecoveryHoldsResponse | Promise<RecoveryHoldsResponse>;
  /** The ownership probe stub (default: the run is `completed`). Throw a RequestError(404) for a run this worker does not hold. */
  ownership?: (runId: string) => RunOwnershipResponse | Promise<RunOwnershipResponse>;
  idle?: () => boolean;
  quiesce?: (req: QuiesceRunRequest) => Promise<QuiesceRunOutcome>;
  server?: Partial<RetainedSourceServerDeps>;
  log?: Logger;
  /** A directory (default: a fresh short one) used as the worker data dir of the ops socket. */
  dataDir?: string;
  noGate?: boolean;
  /** Extra RunRunner options (e.g. a Docker endpoint). */
  runnerOpts?: Record<string, unknown>;
}

interface Rig {
  server: RetainedSourceServer;
  gate: DindPruneGate;
  runner: RunRunner;
  events: string[];
  sock: string;
  dataDir: string;
  idle: { value: boolean };
  abort: () => Promise<void>;
  served: Promise<void>;
  quiesceRequests: QuiesceRunRequest[];
  holdsCalls: string[];
  ownershipCalls: string[];
}

function shortDir(prefix = "rso-"): string {
  const dir = mkdtempForSocket(prefix);
  scratch.push(dir);
  return dir;
}

function instrumentGit(events: string[]): void {
  const seam = git as unknown as {
    withLock: <T>(key: string, fn: () => Promise<T>) => Promise<T>;
    deleteRetainedArtifact: (...a: unknown[]) => Promise<boolean>;
  };
  const realLock = seam.withLock.bind(git);
  seam.withLock = (key, fn) => {
    if (key === bare) events.push("lock:bare");
    return realLock(key, fn);
  };
  const realDelete = seam.deleteRetainedArtifact.bind(git);
  seam.deleteRetainedArtifact = async (...args) => {
    events.push("delete");
    return realDelete(...args);
  };
}

async function makeRig(o: RigOptions = {}): Promise<Rig> {
  const events: string[] = [];
  instrumentGit(events);
  const quiesceRequests: QuiesceRunRequest[] = [];
  const holdsCalls: string[] = [];
  const ownershipCalls: string[] = [];
  const runner = new RunRunner({} as unknown as WorkerClient, git, () => ({ executor: {} as never }), nullLogger(), 20, undefined, {
    pollMs: 5,
    quiesceRun: async (req) => {
      quiesceRequests.push(req);
      events.push("quiesce");
      return o.quiesce ? o.quiesce(req) : QUIESCENT;
    },
    ...o.runnerOpts,
  });
  const gate = new DindPruneGate();
  const realClose = gate.close.bind(gate);
  const realOpen = gate.open.bind(gate);
  gate.close = (owner) => { events.push(`gate_closed:${owner ?? "cache"}`); realClose(owner); };
  gate.open = (owner) => { events.push(`gate_opened:${owner ?? "cache"}`); realOpen(owner); };
  const idle = { value: true };
  const port = {
    isLiveAttemptPath: (p: string) => runner.isLiveAttemptPath(p),
    withRetainedSourceBoundary: async <T,>(
      runId: string, paths: readonly string[],
      action: (ctx: { open: () => boolean; quiesce: (p: string[]) => Promise<boolean>; isLivePath: (p: string) => boolean }) => Promise<T>,
    ) => {
      const r = await runner.withRetainedSourceBoundary(runId, paths, async (ctx) => {
        events.push("reservation_installed");
        try { return await action(ctx); } finally { events.push("deletion_finished"); }
      });
      events.push(r.entered ? "reservation_released" : "reservation_refused");
      return r;
    },
  };
  const dataDir = o.dataDir ?? shortDir();
  const server = new RetainedSourceServer({
    dataDir, git, gate: o.noGate ? undefined : gate, runner: port, log: o.log ?? nullLogger(),
    client: {
      listRecoveryHolds: async (runId: string) => {
        events.push("holds_read");
        holdsCalls.push(runId);
        return o.holds ? o.holds(runId) : { run_id: runId, holds: [] };
      },
      getRunOwnership: async (runId: string) => {
        events.push("ownership_read");
        ownershipCalls.push(runId);
        return o.ownership ? o.ownership(runId) : { status: "completed", claim_generation: 1 };
      },
    },
    isIdle: () => { const v = o.idle ? o.idle() : idle.value; events.push(`idle:${v}`); return v; },
    ...o.server,
  });
  const ac = new AbortController();
  const served = server.serve(ac.signal);
  await server.ready;
  return {
    server, gate, runner, events, sock: server.path, dataDir, idle, served, quiesceRequests, holdsCalls, ownershipCalls,
    abort: async () => { ac.abort(); await served; },
  };
}

type Json = Record<string, any>;

/** One request on a fresh connection; resolves with the parsed response (the connection ends). */
function send(sock: string, req: unknown | string): Promise<Json> {
  return new Promise((resolve, reject) => {
    const conn = net.createConnection(sock);
    const chunks: Buffer[] = [];
    conn.on("error", reject);
    conn.on("connect", () => conn.write(typeof req === "string" ? req : `${JSON.stringify(req)}\n`));
    conn.on("data", (c) => chunks.push(c));
    conn.on("end", () => {
      try {
        resolve(JSON.parse(Buffer.concat(chunks).toString("utf8")) as Json);
      } catch (err) {
        reject(err);
      }
    });
  });
}
/** Send a request and hand back the live socket (to disconnect mid-operation). */
function sendDetached(sock: string, req: unknown): Promise<net.Socket> {
  return new Promise((resolve, reject) => {
    const conn = net.createConnection(sock);
    conn.on("error", () => undefined);
    conn.on("connect", () => { conn.write(`${JSON.stringify(req)}\n`, () => resolve(conn)); });
    conn.on("error", reject);
  });
}

const inspectReq = (a: { runId: string; attemptId?: string }, generation: number | "none" = 1) =>
  ({ op: "inspect", run: a.runId, generation, attempt: a.attemptId ?? "canonical" });

/** Inspect over the socket and return the single source's evidence. */
async function inspectOne(rig: Rig, a: { runId: string; attemptId?: string }, generation: number | "none" = 1): Promise<Json> {
  const r = await send(rig.sock, inspectReq(a, generation));
  assert.equal(r.ok, true, JSON.stringify(r));
  assert.equal(r.sources.length, 1, JSON.stringify(r));
  return r.sources[0] as Json;
}

beforeEach(async () => {
  fx = makeFixture();
  git = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
  bare = await git.ensureClone(fx.originPath);
  fs.chmodSync(path.dirname(bare), 0o700);
  fs.chmodSync(bare, 0o700);
});

const rigs: Rig[] = [];
afterEach(async () => {
  for (const r of rigs.splice(0)) await r.abort().catch(() => undefined);
  for (const d of scratch.splice(0)) {
    try { fs.chmodSync(d, 0o700); } catch { /* gone */ }
    fs.rmSync(d, { recursive: true, force: true });
  }
  fx.cleanup();
});
async function rigOf(o: RigOptions = {}): Promise<Rig> {
  const r = await makeRig(o);
  rigs.push(r);
  return r;
}

// ─── the claim gate: all three lanes, with no DinD controller ─────────────────────────────────

describe("the unconditional claim gate", () => {
  it("open('retained_ops') removes only that owner; a held cache or maintenance blocker stays closed", () => {
    const gate = new DindPruneGate();
    gate.close("cache");
    gate.close("maintenance");
    gate.close("retained_ops");
    assert.ok(gate.heldBy("retained_ops"));
    gate.open("retained_ops");
    assert.equal(gate.heldBy("retained_ops"), false);
    assert.ok(gate.heldBy("cache") && gate.heldBy("maintenance"));
    assert.equal(gate.claimsClosed(), true, "the other owners still close the gate");
    assert.equal(gate.tryEnterClaim(), false);
    gate.open("cache");
    gate.open("maintenance");
    assert.equal(gate.claimsClosed(), false);
  });

  function workerRig(claimGate: DindPruneGate | undefined) {
    const counts = { run: 0, chat: 0, cross: 0 };
    const client = {
      register: async () => ({ worker_id: "w" }),
      hasFeature: () => false,
      heartbeat: async () => false,
      claimRun: async (): Promise<ClaimResponse | null> => { counts.run++; return null; },
      claimChat: async () => { counts.chat++; return null; },
      claimCrossCheck: async (): Promise<ClaimResponse | null> => { counts.cross++; return null; },
      reportState: async () => ({ applied: true, status: "failed" }),
    };
    const config = loadConfig({ UZI_API_URL: "http://example.com", UZI_WORKER_TOKEN: "worker-token", WORKER_CROSS_CHECK_SLOTS: "1" });
    config.pollIntervalMs = config.chatPollMs = config.heartbeatIntervalMs = 2;
    const runner = { resumePendingRecoveries: async () => {}, settlePendingPredecessors: async () => {}, execute: async () => {} };
    const worker = new Worker(config, client as unknown as WorkerClient, runner as unknown as RunRunner,
      {} as ChatRunner, {} as JudgeRunner, {} as ReviewRunner, nullLogger(), () => ({ ok: true, missing: [] }),
      undefined, undefined, undefined, undefined, undefined, undefined, undefined,
      () => ({ collect: () => ({}) }) as unknown as StatsCollector,
      undefined, undefined, undefined, undefined, { execute: async () => {} }, claimGate);
    return { worker, counts };
  }

  it("a worker with NO DinD controller still gates the run, cross-check and chat lanes while retained_ops is closed", async () => {
    const gate = new DindPruneGate();
    const { worker, counts } = workerRig(gate);
    gate.close("retained_ops");
    const ac = new AbortController();
    const done = worker.run(ac.signal);
    try {
      await tick(150);
      assert.deepEqual(counts, { run: 0, chat: 0, cross: 0 }, "no lane claims while the channel holds the gate");
      gate.open("retained_ops");
      await until(() => counts.run > 0 && counts.chat > 0 && counts.cross > 0, "all three lanes claim after the gate reopens");
    } finally {
      ac.abort();
      await done;
    }
    assert.equal(gate.inFlightClaims(), 0, "every entered claim exited");
  });
});

// ─── the discard sequence and the claim race ──────────────────────────────────────────────────

function claimWorker(rig: Rig, opts: { claimRun: () => Promise<ClaimResponse | null>; execute?: () => Promise<void>; events?: string[] }) {
  const events = opts.events ?? rig.events;
  const client = {
    register: async () => ({ worker_id: "w" }),
    hasFeature: () => false,
    heartbeat: async () => false,
    claimRun: async (): Promise<ClaimResponse | null> => { events.push("claim_started"); return opts.claimRun(); },
    claimChat: async () => null,
    claimCrossCheck: async () => null,
    reportState: async () => ({ applied: true, status: "failed" }),
  };
  const config = loadConfig({ UZI_API_URL: "http://example.com", UZI_WORKER_TOKEN: "worker-token" });
  config.pollIntervalMs = config.chatPollMs = config.heartbeatIntervalMs = 2;
  const runner = { resumePendingRecoveries: async () => {}, settlePendingPredecessors: async () => {}, execute: opts.execute ?? (async () => {}) };
  // Late-bound: the server's idle probe is the worker's own.
  let worker!: Worker;
  rig.idle.value = true;
  const retained = new RetainedSourceServer({
    ...(rig.server as unknown as { deps: RetainedSourceServerDeps }).deps,
    isIdle: () => { const v = worker.isIdle(); events.push(`in_flight_claims:${rig.gate.inFlightClaims()}`, `idle:${v}`); return v; },
  });
  worker = new Worker(config, client as unknown as WorkerClient, runner as unknown as RunRunner,
    {} as ChatRunner, {} as JudgeRunner, {} as ReviewRunner, nullLogger(), () => ({ ok: true, missing: [] }),
    undefined, undefined, undefined, undefined, undefined, undefined, undefined,
    () => ({ collect: () => ({}) }) as unknown as StatsCollector,
    undefined, undefined, undefined, undefined, { execute: async () => {} }, rig.gate, retained);
  return { worker, retained };
}

describe("discard through the real socket", () => {
  it("runs gate closed -> drained -> idle -> reservation -> holds read -> bare lock -> deletion -> release -> gate reopened", async () => {
    const a = await retainedAttempt("run-seq", 1);
    const rig = await rigOf();
    const ev = await inspectOne(rig, a);
    assert.equal(ev.disposition.kind, "local_discard");
    rig.events.length = 0;
    const r = await send(rig.sock, { op: "discard", run: a.runId, generation: 1, attempt: a.attemptId, confirm: ev.descriptor });
    assert.equal(r.ok, true, JSON.stringify(r));
    assert.equal(r.result.kind, "done");
    assert.equal(fs.existsSync(a.path), false);
    const ix = (name: string): number => rig.events.indexOf(name);
    const closed = ix("gate_closed:retained_ops");
    const idle = ix("idle:true");
    const reserved = ix("reservation_installed");
    const holds = ix("holds_read");
    const del = ix("delete");
    const finished = ix("deletion_finished");
    const released = ix("reservation_released");
    const opened = ix("gate_opened:retained_ops");
    assert.ok(closed >= 0 && closed < idle, `gate closed before idle proof: ${rig.events.join(",")}`);
    assert.ok(idle < reserved && reserved < holds, `reservation installed before the holds read: ${rig.events.join(",")}`);
    // The holds read happens INSIDE the reservation and BEFORE GitCache enters the bare lock.
    const lockAfterReservation = rig.events.findIndex((e, i) => i > reserved && e === "lock:bare");
    assert.ok(lockAfterReservation > holds, `the bare lock is entered only after the holds read: ${rig.events.join(",")}`);
    assert.ok(rig.events.slice(reserved, holds).every((e) => e !== "lock:bare"), "no bare lock between the reservation and the holds read");
    assert.ok(holds < del && del < finished && finished < released && released < opened, rig.events.join(","));
    assert.ok(lockAfterReservation < released, "the bare lock section ends before the reservation is released");
    assert.equal(rig.gate.claimsClosed(), false, "the gate is reopened");
    assert.deepEqual(rig.holdsCalls.filter((x) => x === a.runId).length >= 1, true);
  });

  it("the quiescence the boundary hands GitCache is capture-mode and scoped to the clone and its skills sibling", async () => {
    const a = await retainedAttempt("run-q", 1);
    const skills = path.join(path.dirname(a.path), `.uzi-skills-${path.basename(a.path)}`);
    fs.mkdirSync(skills);
    fs.writeFileSync(path.join(skills, "plugin.json"), "{}");
    const rig = await rigOf();
    const ev = await inspectOne(rig, a);
    const r = await send(rig.sock, { op: "discard", run: a.runId, generation: 1, attempt: a.attemptId, confirm: ev.descriptor });
    assert.equal(r.result.kind, "done", JSON.stringify(r));
    assert.equal(fs.existsSync(skills), false);
    assert.ok(rig.quiesceRequests.length >= 2);
    for (const q of rig.quiesceRequests) {
      assert.equal(q.mode, "capture");
      assert.equal(q.site, "retained_ops");
      assert.equal(q.processes, true);
    }
    assert.deepEqual(rig.quiesceRequests.flatMap((q) => q.targetPaths).sort(), [a.path, skills].sort());
  });

  it("a quiescence that is not proven refuses and deletes nothing", async () => {
    const a = await retainedAttempt("run-nq", 1);
    const rig = await rigOf({ quiesce: async () => ({ process: { state: "unverified" }, docker: { state: "not_wired" } }) as unknown as QuiesceRunOutcome });
    const ev = await inspectOne(rig, a);
    const r = await send(rig.sock, { op: "discard", run: a.runId, generation: 1, attempt: a.attemptId, confirm: ev.descriptor });
    assert.equal(r.ok, false);
    assert.equal(r.result.kind, "refused");
    assert.equal(r.result.reason, "not_quiescent");
    assert.equal(fs.existsSync(a.path), true);
    assert.equal(rig.gate.claimsClosed(), false);
  });

  it("a Docker-wired discard is refused when the Docker teardown errored, and proceeds when it is merely unconfirmed", async () => {
    const dockerOf = (state: string) => async () => ({ process: { state: "quiescent" }, docker: { state, removed: [], detail: "" } }) as unknown as QuiesceRunOutcome;
    const a = await retainedAttempt("run-dk", 1);
    const bad = await rigOf({ quiesce: dockerOf("docker_error"), runnerOpts: { dockerHost: "unix:///nonexistent/uzi-test-docker.sock" } });
    const ev = await inspectOne(bad, a);
    const refused = await send(bad.sock, { op: "discard", run: a.runId, generation: 1, attempt: a.attemptId, confirm: ev.descriptor });
    assert.equal(refused.ok, false, JSON.stringify(refused));
    assert.equal(refused.result.reason, "not_quiescent");
    assert.equal(fs.existsSync(a.path), true, "nothing was deleted");
    assert.ok(bad.quiesceRequests.length > 0, "a quiescence proof was requested");
    assert.ok(bad.quiesceRequests.every((q) => q.dockerHost !== undefined), "the Docker teardown was requested");
    const ok = await rigOf({ quiesce: dockerOf("docker_unconfirmed"), runnerOpts: { dockerHost: "unix:///nonexistent/uzi-test-docker.sock" } });
    const ev2 = await inspectOne(ok, a);
    const done = await send(ok.sock, { op: "discard", run: a.runId, generation: 1, attempt: a.attemptId, confirm: ev2.descriptor });
    assert.equal(done.result.kind, "done", JSON.stringify(done));
    assert.equal(fs.existsSync(a.path), false);
  });

  it("a held cache or maintenance blocker stays closed after retained_ops reopens", async () => {
    const a = await retainedAttempt("run-block", 1);
    const rig = await rigOf();
    rig.gate.close("cache");
    rig.gate.close("maintenance");
    const ev = await inspectOne(rig, a);
    const r = await send(rig.sock, { op: "discard", run: a.runId, generation: 1, attempt: a.attemptId, confirm: ev.descriptor });
    assert.equal(r.result.kind, "done", JSON.stringify(r));
    assert.equal(rig.gate.heldBy("retained_ops"), false);
    assert.ok(rig.gate.heldBy("cache") && rig.gate.heldBy("maintenance"), "the other owners are untouched");
    assert.equal(rig.gate.claimsClosed(), true);
  });

  it("a wrong digest, an unknown run and a plain/protected mismatch refuse and delete nothing", async () => {
    const a = await retainedAttempt("run-bad", 1);
    const rig = await rigOf();
    const ev = await inspectOne(rig, a);
    const wrong = await send(rig.sock, { op: "discard", run: a.runId, generation: 1, attempt: a.attemptId, confirm: "0".repeat(64) });
    assert.equal(wrong.result.reason, "descriptor_changed");
    const missing = await send(rig.sock, { op: "discard", run: "run-nope", generation: 1, attempt: a.attemptId, confirm: ev.descriptor });
    assert.equal(missing.result.reason, "not_found");
    const kind = await send(rig.sock, { op: "discard", run: a.runId, generation: 1, attempt: a.attemptId, confirm: ev.descriptor, protected: true });
    assert.equal(kind.result.reason, "wrong_discard_kind");
    assert.equal(fs.existsSync(a.path), true);
    assert.equal(rig.gate.claimsClosed(), false);
  });

  it("an unreadable server holds list is unknown and refuses; a discard with no claim gate refuses", async () => {
    const a = await retainedAttempt("run-hu", 1);
    const failing = await rigOf({ holds: async () => { throw new Error("api down"); } });
    const ev = await inspectOne(failing, a);
    const r = await send(failing.sock, { op: "discard", run: a.runId, generation: 1, attempt: a.attemptId, confirm: ev.descriptor });
    assert.equal(r.result.reason, "holds_unknown");
    assert.equal(fs.existsSync(a.path), true);
    assert.ok(failing.events.indexOf("reservation_installed") < failing.events.lastIndexOf("holds_read"), "the read ran inside the reservation");
    assert.equal(failing.gate.claimsClosed(), false);
    await failing.abort();
    const ungated = await rigOf({ noGate: true });
    const r2 = await send(ungated.sock, { op: "discard", run: a.runId, generation: 1, attempt: a.attemptId, confirm: ev.descriptor });
    assert.equal(r2.result.reason, "no_claim_gate");
  });

  it("an execution active after its claim completed is not idle: the discard refuses and the gate reopens", async () => {
    const a = await retainedAttempt("run-act", 1);
    const rig = await rigOf({ idle: () => false });
    const ev = await inspectOne(rig, a);
    const r = await send(rig.sock, { op: "discard", run: a.runId, generation: 1, attempt: a.attemptId, confirm: ev.descriptor });
    assert.equal(r.result.reason, "worker_not_idle");
    assert.equal(fs.existsSync(a.path), true);
    assert.equal(rig.events.includes("reservation_installed"), false, "no reservation before idleness is proven");
    assert.equal(rig.events.slice(rig.events.indexOf("gate_closed:retained_ops")).includes("holds_read"), false, "no holds read before idleness is proven");
    assert.equal(rig.gate.claimsClosed(), false);
  });

  it("background recovery holding the run reservation refuses the discard; it succeeds once released", async () => {
    const a = await retainedAttempt("run-rec", 1);
    const rig = await rigOf();
    const ev = await inspectOne(rig, a);
    const release = deferred();
    const entered = deferred();
    // Background recovery's own boundary on the same run: the real execution tail + reservation.
    const held = rig.runner.withRetainedSourceBoundary(a.runId, [a.path], async () => { entered.resolve(); await release.promise; });
    await entered.promise;
    const r = await send(rig.sock, { op: "discard", run: a.runId, generation: 1, attempt: a.attemptId, confirm: ev.descriptor });
    assert.equal(r.result.reason, "run_reserved");
    assert.equal(fs.existsSync(a.path), true);
    assert.equal(rig.gate.claimsClosed(), false, "the gate reopened");
    assert.equal(rig.events.slice(rig.events.indexOf("gate_closed:retained_ops")).includes("holds_read"), false, "no holds read without the reservation");
    release.resolve();
    await held;
    const again = await send(rig.sock, { op: "discard", run: a.runId, generation: 1, attempt: a.attemptId, confirm: ev.descriptor });
    assert.equal(again.result.kind, "done", JSON.stringify(again));
  });

  it("a second concurrent discard is busy (no queue); the first still completes", async () => {
    const a = await retainedAttempt("run-busy", 1);
    const rig = await rigOf();
    const ev = await inspectOne(rig, a);
    assert.ok(rig.gate.tryEnterClaim(), "a manual in-flight claim keeps the first discard waiting on the drain");
    const req = { op: "discard", run: a.runId, generation: 1, attempt: a.attemptId, confirm: ev.descriptor };
    const first = send(rig.sock, req);
    await until(() => rig.gate.heldBy("retained_ops"), "the first discard closed the gate");
    const second = await send(rig.sock, req);
    assert.equal(second.ok, false);
    assert.equal(second.result.reason, "busy");
    assert.equal(rig.gate.heldBy("retained_ops"), true, "the busy refusal did not touch the gate");
    rig.gate.exitClaim();
    const r = await within(20_000, "the first discard", first);
    assert.equal(r.result.kind, "done", JSON.stringify(r));
    assert.equal(rig.gate.claimsClosed(), false);
  });

  it("in-flight claims that never drain within 5 s (injected clock) refuse and reopen the gate", async () => {
    const a = await retainedAttempt("run-to", 1);
    let t = 0;
    const rig = await rigOf({ server: { now: () => t, sleep: async (ms) => { t += ms; } } });
    const ev = await inspectOne(rig, a);
    assert.ok(rig.gate.tryEnterClaim());
    const r = await send(rig.sock, { op: "discard", run: a.runId, generation: 1, attempt: a.attemptId, confirm: ev.descriptor });
    assert.equal(r.ok, false);
    assert.equal(r.result.reason, "claims_not_drained");
    assert.ok(t >= 5_000 && t < 6_000, `the injected clock advanced ${t} ms`);
    assert.equal(rig.gate.claimsClosed(), false, "reopened");
    assert.equal(fs.existsSync(a.path), true);
    assert.equal(rig.events.includes("reservation_installed"), false);
    rig.gate.exitClaim();
  });

  it("shutdown during the drain refuses with shutting_down and reopens the gate", async () => {
    const a = await retainedAttempt("run-sd", 1);
    const rig = await rigOf();
    const ev = await inspectOne(rig, a);
    assert.ok(rig.gate.tryEnterClaim());
    const pending = send(rig.sock, { op: "discard", run: a.runId, generation: 1, attempt: a.attemptId, confirm: ev.descriptor }).catch(() => ({ closed: true }) as Json);
    await until(() => rig.gate.heldBy("retained_ops"), "gate closed");
    await rig.abort();
    await pending;
    assert.equal(rig.gate.heldBy("retained_ops"), false);
    assert.equal(fs.existsSync(a.path), true);
    rig.gate.exitClaim();
  });
});

/** A GitCache stand-in whose evidence is fixed; `discard` is whatever the test needs. */
function fakeGitOf(o: {
  inspect?: () => Promise<Json>;
  discard?: () => Promise<unknown>;
}): GitCache {
  const evidence = {
    kind: "attempt", barePath: bare, descriptor: "d".repeat(64), sources: [{ clonePath: path.join(os.tmpdir(), "rso-fake-clone") }],
  };
  return {
    inspectRetainedSources: o.inspect ?? (async () => ({ sources: [evidence], partial: false, unreadable: 0, sampledAt: new Date().toISOString() })),
    discardStoppedSource: o.discard ?? (async () => ({ kind: "done", sources: [], metadata: "retired" })),
    discardProtectedSource: async () => { throw new Error("unused"); },
  } as unknown as GitCache;
}
const fakeDiscard = { op: "discard", run: "run-fake", generation: 1, attempt: "canonical", confirm: "d".repeat(64) };

describe("discard hardening", () => {
  it("a discard refused busy does not displace the in-flight one: close() still waits for the first", async () => {
    const release = deferred();
    const entered = deferred();
    const git2 = fakeGitOf({ discard: async () => { entered.resolve(); await release.promise; return { kind: "done", sources: [], metadata: "retired" }; } });
    const rig = await rigOf({ server: { git: git2 } });
    const first = send(rig.sock, fakeDiscard).catch(() => ({ closed: true }) as Json);
    await within(5000, "the first discard reached deletion", entered.promise);
    const second = await send(rig.sock, fakeDiscard);
    assert.equal(second.result.reason, "busy");
    let closed = false;
    const closing = rig.abort().then(() => { closed = true; });
    await tick(150);
    assert.equal(closed, false, "close() is still waiting for the first discard");
    release.resolve();
    await within(7000, "close() after the first discard ends", closing);
    await first;
    assert.equal(closed, true);
  });

  it("a wrong confirm digest is refused before the claim gate is ever closed", async () => {
    const a = await retainedAttempt("run-dig", 1);
    const rig = await rigOf();
    rig.events.length = 0;
    const r = await send(rig.sock, { op: "discard", run: a.runId, generation: 1, attempt: a.attemptId, confirm: "0".repeat(64) });
    assert.equal(r.ok, false);
    assert.equal(r.result.reason, "descriptor_changed");
    assert.equal(rig.events.some((e) => e.startsWith("gate_closed")), false, rig.events.join(","));
    assert.equal(rig.events.includes("reservation_installed"), false);
    assert.equal(fs.existsSync(a.path), true);
  });

  it("a partial or unreadable locate that finds no source is inspect_partial; a complete empty one is not_found", async () => {
    let state: { partial: boolean; unreadable: number } = { partial: true, unreadable: 0 };
    const rig = await rigOf({ server: { git: fakeGitOf({
      inspect: async () => ({ sources: [], ...state, sampledAt: new Date().toISOString() }),
    }) } });
    assert.equal((await send(rig.sock, fakeDiscard)).result.reason, "inspect_partial");
    state = { partial: false, unreadable: 2 };
    assert.equal((await send(rig.sock, fakeDiscard)).result.reason, "inspect_partial");
    state = { partial: false, unreadable: 0 };
    assert.equal((await send(rig.sock, fakeDiscard)).result.reason, "not_found");
    assert.equal(rig.events.some((e) => e.startsWith("gate_closed")), false);
  });
});

describe("the claim race, through the real Worker claim loops", () => {
  it("a claim in flight when the discard arrives completes: the worker is not idle, the discard refuses and no claim starts under the closed gate", async () => {
    const a = await retainedAttempt("run-race", 1);
    const rig = await rigOf();
    const claimRelease = deferred();
    const execRelease = deferred();
    let claims = 0;
    const { worker } = claimWorker(rig, {
      claimRun: async () => {
        claims++;
        if (claims > 1) return null;
        await claimRelease.promise;
        return { run_id: "run-other", kind: "issue", claim_generation: 1 } as unknown as ClaimResponse;
      },
      execute: async () => { await execRelease.promise; },
    });
    const ac = new AbortController();
    // The worker owns its own server; open that socket (the rig's own server is unused here).
    await rig.abort();
    const sock = rig.sock;
    const done = worker.run(ac.signal);
    try {
      await until(() => fs.existsSync(sock), "the worker's server is listening");
      await until(() => claims === 1, "the first claim is in flight");
      assert.equal(rig.gate.inFlightClaims(), 1);
      const ev = await inspectOne(rig, a);
      rig.events.length = 0;
      const pending = send(sock, { op: "discard", run: a.runId, generation: 1, attempt: a.attemptId, confirm: ev.descriptor });
      await until(() => rig.gate.heldBy("retained_ops"), "the discard closed the gate");
      assert.equal(fs.existsSync(a.path), true, "nothing is deleted while a claim is in flight");
      claimRelease.resolve(); // the in-flight claim returns a run: it joins the active set, then exits the gate
      const r = await within(10_000, "the discard", pending);
      assert.equal(r.ok, false);
      assert.equal(r.result.reason, "worker_not_idle", JSON.stringify(r));
      assert.equal(fs.existsSync(a.path), true);
      assert.equal(rig.events.includes("reservation_installed"), false, "no reservation: idleness failed first");
      assert.equal(rig.events.includes("delete"), false);
      const closed = rig.events.indexOf("gate_closed:retained_ops");
      const opened = rig.events.indexOf("gate_opened:retained_ops");
      assert.ok(closed >= 0 && opened > closed);
      assert.equal(rig.events.slice(closed, opened).includes("claim_started"), false, "no claim started under the closed gate");
      assert.equal(rig.gate.heldBy("retained_ops"), false);
      execRelease.resolve();
    } finally {
      execRelease.resolve();
      claimRelease.resolve();
      ac.abort();
      await done;
    }
  });

  it("with no claim in flight the discard completes first under the closed gate: no claim starts between the idle proof and the deletion", async () => {
    // Invariant under test: gate closed, claims drained, idle proven, run reservation held through
    // disposal; the claim CTE is the only production hold insertion, so the pre-lock holds read is current.
    const a = await retainedAttempt("run-race2", 1);
    const rig = await rigOf();
    let claimCount = 0;
    const { worker } = claimWorker(rig, { claimRun: async () => { claimCount++; return null; } });
    await rig.abort();
    const ac = new AbortController();
    const done = worker.run(ac.signal);
    try {
      await until(() => fs.existsSync(rig.sock) && claimCount > 3, "the worker's server is listening and the run lane is polling");
      const ev = await inspectOne(rig, a);
      rig.events.length = 0;
      const r = await within(20_000, "the discard", send(rig.sock, { op: "discard", run: a.runId, generation: 1, attempt: a.attemptId, confirm: ev.descriptor }));
      assert.equal(r.result.kind, "done", JSON.stringify(r));
      const names = rig.events;
      const closed = names.indexOf("gate_closed:retained_ops");
      const opened = names.indexOf("gate_opened:retained_ops");
      assert.ok(closed >= 0 && opened > closed, names.join(","));
      // Nothing claims between the gate closing and it reopening (the idle proof, the holds read
      // and the deletion all sit inside that window).
      assert.equal(names.slice(closed, opened).includes("claim_started"), false, names.join(","));
      assert.ok(names.indexOf("idle:true") > closed && names.indexOf("idle:true") < names.indexOf("reservation_installed"));
      assert.ok(names.indexOf("delete") > names.indexOf("holds_read"));
      assert.equal(names.indexOf("in_flight_claims:0") + 1, names.indexOf("idle:true"), `no claim was in flight at the idle proof: ${names.join(",")}`);
      // The pre-gate locate inspect and the lane's own work may lock the bare earlier; the invariant
      // is about the first bare lock inside the reservation.
      const firstLock = names.findIndex((e, i) => i > names.indexOf("reservation_installed") && e === "lock:bare");
      assert.ok(names.indexOf("holds_read") >= 0 && names.indexOf("holds_read") < firstLock, `holds read before the first bare lock: ${names.join(",")}`);
      assert.ok(names.indexOf("deletion_finished") >= 0 && names.indexOf("deletion_finished") < names.indexOf("reservation_released"),
        `the reservation is held until the deletion finished: ${names.join(",")}`);
      // The lane resumes once the gate is back.
      const before = claimCount;
      await until(() => claimCount > before, "the lane claims again after the gate reopens");
    } finally {
      ac.abort();
      await done;
    }
  });

  it("the worker with an overlong data dir runs without the channel: the claim loop keeps polling", async () => {
    const base = shortDir();
    const deep = path.join(base, "d".repeat(110));
    fs.mkdirSync(deep);
    const { logger, lines } = recordingLogger();
    const rig = await rigOf({ dataDir: deep, log: logger });
    assert.equal(rig.server.listening, false);
    assert.ok((lines as Array<{ level?: string; msg?: string }>).some((l) => l.level === "warn" && /unavailable/.test(l.msg ?? "")));
    let claims = 0;
    const { worker } = claimWorker(rig, { claimRun: async () => { claims++; return null; } });
    await rig.abort();
    const ac = new AbortController();
    const done = worker.run(ac.signal);
    try {
      await until(() => claims > 3, "the run lane keeps polling with the channel unavailable");
    } finally {
      ac.abort();
      await done;
    }
  });
});

describe("disconnects", () => {
  it("a client that leaves before the destructive stage gets no deletion; the outcome is visible to a later inspect", async () => {
    const a = await retainedAttempt("run-dc1", 1);
    const rig = await rigOf();
    const ev = await inspectOne(rig, a);
    assert.ok(rig.gate.tryEnterClaim());
    const conn = await sendDetached(rig.sock, { op: "discard", run: a.runId, generation: 1, attempt: a.attemptId, confirm: ev.descriptor });
    await until(() => rig.gate.heldBy("retained_ops"), "the discard is waiting on the drain");
    conn.destroy();
    await until(() => rig.server.activeConnections === 0, "the server saw the disconnect");
    rig.gate.exitClaim();
    await until(() => !rig.gate.heldBy("retained_ops"), "the abandoned operation ended and reopened the gate");
    assert.equal(fs.existsSync(a.path), true, "nothing was deleted for a client that is gone");
    const after = await send(rig.sock, inspectReq(a));
    assert.equal(after.lastDiscards[0].outcome, "refused");
    assert.equal(after.lastDiscards[0].reason, "client_disconnected");
    assert.equal(after.sources.length, 1);
  });

  it("a client that leaves once the destructive stage began does not stop it: the exact disposition is recorded for inspect", async () => {
    const a = await retainedAttempt("run-dc2", 1);
    const gateOpen = deferred();
    const rig = await rigOf({
      // Block inside GitCache's quiescence (the bare lock is held) until the client is gone.
      quiesce: async () => { await gateOpen.promise; return QUIESCENT; },
    });
    const ev = await inspectOne(rig, a);
    const conn = await sendDetached(rig.sock, { op: "discard", run: a.runId, generation: 1, attempt: a.attemptId, confirm: ev.descriptor });
    await until(() => rig.events.includes("quiesce"), "the destructive stage began");
    conn.destroy();
    await until(() => rig.server.activeConnections === 0, "the server saw the disconnect");
    gateOpen.resolve();
    await until(() => !rig.gate.heldBy("retained_ops") && rig.events.includes("reservation_released"), "the operation finished by itself");
    assert.equal(fs.existsSync(a.path), false, "the operation ran to completion");
    const after = await send(rig.sock, { op: "inspect" });
    assert.equal(after.lastDiscards[0].outcome, "done");
    assert.equal(after.lastDiscards[0].attempt, a.attemptId);
  });
});

// ─── inspect ─────────────────────────────────────────────────────────────────────────────────

describe("inspect", () => {
  it("is read-only: it never closes the gate and reports the holds it read for a selected run", async () => {
    const a = await retainedAttempt("run-ins", 1);
    const rig = await rigOf({ holds: (runId) => ({ run_id: runId, holds: [{ hold_id: "hold-9", generation: 1 } as never] }) });
    const ev = await inspectOne(rig, a);
    assert.deepEqual(ev.holds, { state: "read", ids: ["hold-9"] });
    assert.deepEqual(ev.disposition, { kind: "owner_hold_discard", holdIds: ["hold-9"] });
    assert.equal(rig.events.some((e) => e.startsWith("gate_")), false, "inspect never touches the gate");
    const aggregate = await send(rig.sock, { op: "inspect" });
    assert.equal(aggregate.ok, true);
    assert.equal(aggregate.sources.length, 1);
    assert.equal(aggregate.sources[0].disposition, undefined, "an aggregate view has no per-run disposition");
    assert.equal(rig.holdsCalls.length, 1, "holds are read only for a selected run");
    assert.deepEqual(rig.ownershipCalls, [a.runId], "inspect reads ownership for the selected run only");
  });

  it("a failed holds read is unknown with no disposition", async () => {
    const a = await retainedAttempt("run-ins2", 1);
    const rig = await rigOf({ holds: async () => { throw new Error("api down"); } });
    const ev = await inspectOne(rig, a);
    assert.equal(ev.holds.state, "unknown");
    assert.notEqual(ev.disposition?.kind, "local_discard");
    assert.notEqual(ev.disposition?.kind, "owner_hold_discard");
    const whole = await send(rig.sock, inspectReq(a));
    assert.equal(whole.holdsRead, "unknown");
    for (const bad of [{ run_id: "other", holds: [] }, { run_id: a.runId, holds: "x" }, { run_id: a.runId, holds: [{ hold_id: "", generation: 1 }] }]) {
      const r2 = await rigOf({ holds: async () => bad as never, dataDir: shortDir() });
      assert.equal((await inspectOne(r2, a)).holds.state, "unknown", JSON.stringify(bad));
    }
  });

  it("logs a bounded retained count and bytes after an operation, without touching the heartbeat", async () => {
    const a = await retainedAttempt("run-diag", 1);
    const { logger, lines } = recordingLogger();
    const rig = await rigOf({ log: logger });
    const ev = await inspectOne(rig, a);
    const r = await send(rig.sock, { op: "discard", run: a.runId, generation: 1, attempt: a.attemptId, confirm: ev.descriptor });
    assert.equal(r.result.kind, "done");
    const diag = (): Array<Record<string, unknown>> => (lines as Array<Record<string, unknown>>).filter((l) => l.msg === "retained sources");
    await until(() => diag().length > 0, "the diagnostic line");
    assert.equal(diag()[0]!.retained_count, 0);
    assert.equal(diag()[0]!.retained_bytes, 0);
    await rig.server.reportRetained();
    assert.ok(diag().length >= 2, "the runner can call reportRetained after retaining a source");
  });
});

// ─── protected sources through the real channel ──────────────────────────────────────────────

describe("protected-source discard through the real socket channel", () => {
  const epSel = () => ({ runId: EP_RUN, attemptId: aid(2) });

  it("completes against a real episode fixture within a bounded deadline, with no deadlock on the bare lock", async () => {
    const e = await buildEpisode();
    const rig = await rigOf();
    const ev = await inspectOne(rig, epSel(), 2);
    assert.equal(ev.kind, "protected");
    assert.deepEqual(ev.disposition, { kind: "local_protected_discard" });
    rig.events.length = 0;
    const r = await within(20_000, "the protected discard", send(rig.sock, {
      op: "discard", run: EP_RUN, generation: 2, attempt: aid(2), confirm: ev.descriptor, protected: true,
    }));
    assert.equal(r.ok, true, JSON.stringify(r));
    assert.equal(r.result.kind, "done");
    assert.equal(fs.existsSync(e.sourcePath) || fs.existsSync(e.successorPath), false);
    assert.equal(cfgAll(`uzi-retained.${EP_RUN}.journal`).length, 0);
    assert.equal(journalRaw(e.branch), "");
    assert.throws(() => gitIn(bare, ["rev-parse", "--verify", `refs/uzi-recovery-episode/${EP_RUN}/${e.tip}`]));
    const reserved = rig.events.indexOf("reservation_installed");
    const holds = rig.events.indexOf("holds_read");
    const lock = rig.events.findIndex((x, i) => i > reserved && x === "lock:bare");
    assert.ok(reserved >= 0 && holds > reserved && lock > holds, rig.events.join(","));
  });

  it("an open server hold is owner_hold_discard with the server's id, and the protected discard refuses", async () => {
    const e = await buildEpisode();
    const rig = await rigOf({ holds: (runId) => ({ run_id: runId, holds: [{ hold_id: "hold-from-server", generation: 1 } as never] }) });
    const ev = await inspectOne(rig, epSel(), 2);
    assert.deepEqual(ev.disposition, { kind: "owner_hold_discard", holdIds: ["hold-from-server"] });
    const r = await send(rig.sock, { op: "discard", run: EP_RUN, generation: 2, attempt: aid(2), confirm: ev.descriptor, protected: true });
    assert.equal(r.ok, false);
    assert.equal(r.result.kind, "refused");
    assert.equal(r.result.disposition.kind, "owner_hold_discard");
    assert.ok(fs.existsSync(e.sourcePath) && fs.existsSync(e.successorPath));
  });

  it("empty holds with a surviving local record is resolve_local_record and deletes nothing", async () => {
    const e = await buildEpisode();
    fs.mkdirSync(path.join(fx.dataDir, "recovery", EP_RUN), { recursive: true });
    fs.writeFileSync(path.join(fx.dataDir, "recovery", EP_RUN, "capture-1.json"), JSON.stringify({ generation: 1 }));
    const rig = await rigOf();
    const ev = await inspectOne(rig, epSel(), 2);
    assert.equal(ev.disposition.kind, "resolve_local_record");
    assert.deepEqual(ev.localRecords.map((x: Json) => [x.kind, x.name]), [["recovery", "capture-1.json"]]);
    const r = await send(rig.sock, { op: "discard", run: EP_RUN, generation: 2, attempt: aid(2), confirm: ev.descriptor, protected: true });
    assert.equal(r.result.kind, "refused");
    assert.ok(fs.existsSync(e.sourcePath) && fs.existsSync(e.successorPath));
    assert.equal(rig.gate.claimsClosed(), false);
  });
});

describe("a canonical source left under its plain journal, through the real socket channel", () => {
  const plainCanonical = async (runId: string, iid: number) => {
    const branch = `agent/issue-${iid}`;
    const clone = await git.createOrAttachRunnerClone(bare, iid, noProofReseed, runId);
    await git.markRecoveryCapture(bare, clone.path, branch, runId);
    fs.writeFileSync(path.join(clone.path, "unpreserved.txt"), "kept bytes\n");
    return { branch, runId, path: clone.path };
  };

  it("with no server hold it is listed and discarded; the plain journal goes with it", async () => {
    const c = await plainCanonical("run-plain", 2810);
    const rig = await rigOf();
    const ev = await inspectOne(rig, { runId: c.runId }, "none");
    assert.deepEqual([ev.kind, ev.markerState, ev.journalState], ["canonical", "none", "plain"]);
    assert.equal(ev.disposition.kind, "local_discard");
    assert.ok(ev.consumers.includes("run_state:terminal:completed"), ev.consumers.join());
    const r = await send(rig.sock, { op: "discard", run: c.runId, generation: "none", attempt: "canonical", confirm: ev.descriptor });
    assert.equal(r.ok, true, JSON.stringify(r));
    assert.equal(r.result.kind, "done");
    assert.equal(fs.existsSync(c.path), false);
    assert.equal(journalRaw(c.branch), "");
    assert.equal(rig.gate.claimsClosed(), false);
    assert.ok(rig.ownershipCalls.length >= 2, "ownership was read on inspect and on discard");
    assert.deepEqual([...new Set(rig.ownershipCalls)], [c.runId], "ownership is read only for the selected run");
  });

  it("a 404 from the ownership probe (a run this worker does not hold) keeps the leftover: inspect and discard refuse and delete nothing", async () => {
    const c = await plainCanonical("run-404", 2812);
    const rig = await rigOf({ ownership: async () => { throw new RequestError("GET", "/api/worker/runs/run-404/ownership", 404, "not found"); } });
    const ev = await inspectOne(rig, { runId: c.runId }, "none");
    assert.deepEqual(ev.disposition, { kind: "refused", consumers: ["run_state_unknown"] });
    assert.ok(ev.consumers.includes("blocker:run_state_unknown"), ev.consumers.join());
    const journal = journalRaw(c.branch);
    const r = await send(rig.sock, { op: "discard", run: c.runId, generation: "none", attempt: "canonical", confirm: ev.descriptor });
    assert.equal(r.ok, false);
    assert.equal(r.result.kind, "refused");
    assert.deepEqual(r.result.consumers, ["run_state_unknown"]);
    assert.ok(fs.existsSync(c.path));
    assert.equal(journalRaw(c.branch), journal);
    assert.equal(rig.gate.claimsClosed(), false);
  });

  it("a non-terminal ownership status or a body without a status keeps the leftover", async () => {
    const c = await plainCanonical("run-paused", 2813);
    for (const [label, ownership, reason] of [
      ["paused", () => ({ status: "paused", claim_generation: 2 }), "run_state_not_terminal"],
      ["malformed", () => ({}) as never, "run_state_unknown"],
    ] as const) {
      const rig = await rigOf({ ownership, dataDir: shortDir() });
      const ev = await inspectOne(rig, { runId: c.runId }, "none");
      assert.equal((await send(rig.sock, inspectReq({ runId: c.runId }, "none"))).lifecycleRead, label === "paused" ? "not_terminal" : "unknown");
      const r = await send(rig.sock, { op: "discard", run: c.runId, generation: "none", attempt: "canonical", confirm: ev.descriptor });
      assert.deepEqual(r.result.consumers, [reason], label);
      assert.ok(fs.existsSync(c.path), label);
    }
  });

  it("an open server hold means the run is still parked: the discard refuses and deletes nothing", async () => {
    const c = await plainCanonical("run-parked", 2811);
    const rig = await rigOf({ holds: (runId) => ({ run_id: runId, holds: [{ hold_id: "hold-parked", generation: 1 } as never] }) });
    const ev = await inspectOne(rig, { runId: c.runId }, "none");
    assert.deepEqual(ev.disposition, { kind: "owner_hold_discard", holdIds: ["hold-parked"] });
    const journal = journalRaw(c.branch);
    const r = await send(rig.sock, { op: "discard", run: c.runId, generation: "none", attempt: "canonical", confirm: ev.descriptor });
    assert.equal(r.ok, false);
    assert.equal(r.result.kind, "refused");
    assert.ok(fs.existsSync(c.path));
    assert.equal(journalRaw(c.branch), journal);
  });
});

// ─── socket setup, limits and the wire ───────────────────────────────────────────────────────

describe("the socket and its directory", () => {
  it("creates a 0700 ops dir under a setgid parent and a 0600 worker-owned socket", async () => {
    const parent = shortDir();
    fs.chmodSync(parent, 0o2775);
    const dataDir = path.join(parent, "data");
    fs.mkdirSync(dataDir);
    const rig = await rigOf({ dataDir });
    assert.equal(rig.server.listening, true);
    const ops = fs.lstatSync(path.join(dataDir, "ops"));
    assert.equal(ops.mode & 0o777, 0o700);
    assert.equal(ops.uid, process.geteuid!());
    const sock = fs.lstatSync(rig.sock);
    assert.ok(sock.isSocket());
    assert.equal(sock.mode & 0o777, 0o600);
    assert.equal(sock.uid, process.geteuid!());
    assert.equal((await send(rig.sock, { op: "inspect" })).ok, true);
  });

  it("refuses a symlinked ops dir, a wrong-mode ops dir and a foreign entry at the socket path; the worker continues", async () => {
    const target = shortDir();
    const linked = shortDir();
    fs.symlinkSync(target, path.join(linked, "ops"));
    const r1 = await rigOf({ dataDir: linked });
    assert.equal(r1.server.listening, false, "symlinked ops dir");
    assert.equal(fs.readdirSync(target).length, 0, "nothing was created through the link");

    const loose = shortDir();
    fs.mkdirSync(path.join(loose, "ops"), { mode: 0o755 });
    fs.chmodSync(path.join(loose, "ops"), 0o755);
    const r2 = await rigOf({ dataDir: loose });
    assert.equal(r2.server.listening, false, "wrong mode");
    assert.equal(fs.lstatSync(path.join(loose, "ops")).mode & 0o777, 0o755, "a pre-existing directory is refused, not fixed up");

    const foreign = shortDir();
    fs.mkdirSync(path.join(foreign, "ops"), { mode: 0o700 });
    fs.writeFileSync(path.join(foreign, "ops", "retained-source.sock"), "not a socket");
    const r3 = await rigOf({ dataDir: foreign });
    assert.equal(r3.server.listening, false, "a regular file at the socket path");
    assert.equal(fs.readFileSync(path.join(foreign, "ops", "retained-source.sock"), "utf8"), "not a socket");
  });

  it("replaces a stale socket of our own and removes its socket on shutdown", async () => {
    const dataDir = shortDir();
    fs.mkdirSync(path.join(dataDir, "ops"), { mode: 0o700 });
    const stale = net.createServer();
    await new Promise<void>((res) => stale.listen(path.join(dataDir, "ops", "retained-source.sock"), res));
    const rig = await rigOf({ dataDir });
    assert.equal(rig.server.listening, true);
    assert.equal((await send(rig.sock, { op: "inspect" })).ok, true);
    await new Promise<void>((res) => stale.close(() => res()));
    await rig.abort();
    assert.equal(fs.existsSync(rig.sock), false, "the socket is removed on shutdown");
  });

  it("an overlong socket path is channel-unavailable (logged), not a crash", async () => {
    const base = shortDir();
    const deep = path.join(base, "e".repeat(120));
    fs.mkdirSync(deep);
    const { logger, lines } = recordingLogger();
    const rig = await rigOf({ dataDir: deep, log: logger });
    assert.equal(rig.server.listening, false);
    assert.ok((lines as Array<{ msg?: string }>).some((l) => /sun_path/.test(JSON.stringify(l))));
  });

  it("denies the runner uids: a connect as RUNNER_UID or COMMAND_UID fails with EACCES (root only)", { skip: process.getuid?.() !== 0 ? "needs root to switch uid (the split-UID fixture is a root-started image)" : false }, async () => {
    const dataDir = shortDir();
    fs.chmodSync(dataDir, 0o755);
    const rig = await rigOf({ dataDir });
    for (const uid of [10002, 10003]) {
      const out = spawnSync("setpriv", [`--reuid=${uid}`, `--regid=${uid}`, "--clear-groups", process.execPath, "-e",
        `require("net").createConnection(${JSON.stringify(rig.sock)}).on("error", e => { console.log(e.code); process.exit(0); }).on("connect", () => { console.log("CONNECTED"); process.exit(0); })`,
      ], { encoding: "utf8", env: childEnv() });
      assert.equal(out.stdout.trim(), "EACCES", `uid ${uid}: ${out.stdout}${out.stderr}`);
    }
  });
});

describe("the wire", () => {
  it("rejects a request over 8 KiB before parsing it, and answers every malformed request with an error", async () => {
    const rig = await rigOf();
    const big = await send(rig.sock, `{"op":"inspect","pad":"${"x".repeat(9000)}"}\n`);
    assert.deepEqual(big, { ok: false, error: "request_too_large" });
    const unterminated = await send(rig.sock, "x".repeat(9000));
    assert.equal(unterminated.error, "request_too_large");
    for (const bad of [
      "not json\n", "[]\n", '{"op":"nope"}\n', '{"op":"inspect","extra":1}\n', '{"op":"inspect","run":"r"}\n',
      '{"op":"inspect","run":"a b","generation":1,"attempt":"canonical"}\n',
      '{"op":"inspect","run":"r","generation":-1,"attempt":"canonical"}\n',
      '{"op":"inspect","run":"r","generation":1,"attempt":"../x"}\n',
      '{"op":"discard","run":"r","generation":1,"attempt":"canonical"}\n',
      '{"op":"discard","run":"r","generation":1,"attempt":"canonical","confirm":"short"}\n',
      `{"op":"discard","run":"r","generation":1,"attempt":"canonical","confirm":"${"a".repeat(64)}","protected":"yes"}\n`,
      `{"op":"inspect","confirm":"${"a".repeat(64)}"}\n`,
    ]) {
      const r = await send(rig.sock, bad);
      assert.equal(r.ok, false, bad);
      assert.match(String(r.error), /^invalid_/, bad);
    }
  });

  it("a client that trickles bytes without a newline is disconnected by the absolute deadline despite the idle timer", async () => {
    const rig = await rigOf({ server: { requestDeadlineMs: 200, requestReadTimeoutMs: 5_000 } });
    const started = Date.now();
    const elapsed = await within(4000, "the trickling client is cut off", new Promise<number>((resolve) => {
      const c = net.createConnection(rig.sock);
      c.on("error", () => undefined);
      const t = setInterval(() => { if (!c.destroyed) c.write("x"); }, 20);
      c.on("close", () => { clearInterval(t); resolve(Date.now() - started); });
    }));
    assert.ok(elapsed < 2000, `disconnected after ${elapsed} ms, well before the 5 s idle timeout`);
  });

  it("serves one request per connection: a second line on the same connection is ignored", async () => {
    const rig = await rigOf();
    const r = await send(rig.sock, `${JSON.stringify({ op: "inspect" })}\n${JSON.stringify({ op: "bogus" })}\n`);
    assert.equal(r.ok, true);
  });

  it("truncates an oversized source list with a flag instead of exceeding 64 KiB", async () => {
    const huge = Array.from({ length: 64 }, (_, i) => ({ runId: `r${i}`, consumers: ["c".repeat(2000)] }));
    const fakeGit = {
      inspectRetainedSources: async () => ({ sources: huge, partial: false, unreadable: 0, sampledAt: new Date().toISOString() }),
      discardStoppedSource: async () => { throw new Error("unused"); },
      discardProtectedSource: async () => { throw new Error("unused"); },
    } as unknown as GitCache;
    const rig = await rigOf({ server: { git: fakeGit } });
    const chunks: Buffer[] = [];
    await new Promise<void>((resolve, reject) => {
      const c = net.createConnection(rig.sock);
      c.on("connect", () => c.write('{"op":"inspect"}\n'));
      c.on("data", (d) => chunks.push(d));
      c.on("end", () => resolve());
      c.on("error", reject);
    });
    const raw = Buffer.concat(chunks);
    assert.ok(raw.length <= 65536, `response is ${raw.length} bytes`);
    const r = JSON.parse(raw.toString("utf8")) as Json;
    assert.equal(r.ok, true);
    assert.equal(r.truncated, true);
    assert.ok(r.omitted > 0 && r.sources.length + r.omitted === 64);
  });
});
