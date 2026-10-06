import { after, describe, it, type TestContext } from "node:test";
import assert from "node:assert/strict";
import { rmSync } from "node:fs";
import { Worker } from "../src/worker.js";
import type { WorkerClient } from "../src/client.js";
import type { Config } from "../src/config.js";
import type { RunRunner } from "../src/runner.js";
import type { ChatRunner } from "../src/chat-runner.js";
import type { JudgeRunner } from "../src/judge-runner.js";
import type { ReviewRunner } from "../src/review-runner.js";
import type { StatsCollector } from "../src/stats.js";
import type { DindMaintenance, ClaimResponse, ChatClaimResponse } from "../src/protocol.js";
import { DindMaintenanceController } from "../src/dind-maintenance.js";
import { DindPruneGate, type DockerExec } from "../src/dind-prune.js";
import { nullLogger } from "./helpers.js";

const operation: DindMaintenance = {
  id: "op", nonce: "nonce", deployment_uid: "deployment", pvc_uid: "pvc",
  register_nonce: "registration", phase: "ready", fenced: true, ready_ack: false,
};
const dirs = new Set<string>();
after(() => { for (const dir of dirs) rmSync(dir, { recursive: true, force: true }); });

function deferred<T = void>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((r) => { resolve = r; });
  return { promise, resolve };
}

// Bounded polling of real Worker loop seams; siblings are joined in every test's finally.
async function until(condition: () => boolean) {
  for (let i = 0; i < 500 && !condition(); i++) await new Promise((r) => setTimeout(r, 2));
  assert.ok(condition(), "worker loop did not reach expected state");
}

type Loops = {
  registerWithRetry(signal: AbortSignal): Promise<void>;
  heartbeatLoop(signal: AbortSignal): Promise<void>;
  claimLoop(signal: AbortSignal): Promise<void>;
  chatClaimLoop(signal: AbortSignal): Promise<void>;
};

function build(t: TestContext, wired = true) {
  let now = 1_700_000_000_000;
  t.mock.method(Date, "now", () => now);
  const gate = new DindPruneGate();
  const sample = { epochS: now / 1000, bytesUsed: 90, bytesTotal: 100, inodesUsed: 90, inodesTotal: 100 };
  const calls: string[][] = [];
  const state = {
    nonce: "registration", enabled: true, retaining: false as boolean | undefined,
    operation: operation as DindMaintenance | null | undefined,
    fail: false, collectAdvance: 0, omitSample: false,
    exec: undefined as DockerExec | undefined,
  };
  let worker!: Worker;
  const maintenance = new DindMaintenanceController({
    gate, dockerHost: "tcp://dind:2375", heartbeatIntervalMs: 15_000,
    isIdle: () => worker.isIdle(), log: nullLogger(), threshold: () => 0.85,
    now: () => now, readSample: () => sample,
    exec: async (argv, opts) => {
      if (opts.env.DOCKER_CONFIG) dirs.add(opts.env.DOCKER_CONFIG);
      calls.push([...argv]);
      return state.exec ? state.exec(argv, opts) : "1.41"; // optional prune unsupported
    },
  });
  const reports: Parameters<WorkerClient["heartbeat"]>[] = [];
  const registrations: Parameters<WorkerClient["register"]>[] = [];
  const client = {
    register: async (...args: Parameters<WorkerClient["register"]>) => {
      registrations.push(args);
      return { worker_id: "worker", register_nonce: state.nonce };
    },
    hasFeature: (feature: string) => feature === "dind_maintenance_v1" && state.enabled,
    get latestDindMaintenance() { return state.operation; },
    heartbeat: async (...args: Parameters<WorkerClient["heartbeat"]>) => {
      reports.push(args);
      if (state.fail) throw new Error("heartbeat unavailable");
      return state.retaining;
    },
    claimRun: async (): Promise<ClaimResponse | null> => null,
    claimChat: async (): Promise<ChatClaimResponse | null> => null,
  };
  const runner = {
    execute: async () => {}, resumePendingRecoveries: async () => {},
    settlePendingPredecessors: async () => {},
  } as unknown as RunRunner;
  const chat = { execute: async () => {} } as unknown as ChatRunner;
  const collector = {
    collect: () => { now += state.collectAdvance; return { dind_disk_used_bytes: sample.bytesUsed }; },
    get latestDindSample() { return state.omitSample ? undefined : sample; },
  } as unknown as StatsCollector;
  worker = new Worker(
    { workerName: "w", workerTemplate: "base", maxConcurrentRuns: 1, chatSessions: 1,
      pollIntervalMs: 1, chatPollMs: 1, heartbeatIntervalMs: 1,
      dockerWiring: { dockerHost: "tcp://dind:2375" } } as Config,
    client as unknown as WorkerClient, runner, chat, {} as JudgeRunner, {} as ReviewRunner,
    nullLogger(), () => ({ ok: true, missing: [] }),
    undefined, undefined, undefined, undefined, undefined, undefined, undefined,
    () => collector, undefined, undefined, wired ? maintenance : undefined,
  );
  const loops = worker as unknown as Loops;
  const heartbeat = async () => {
    const stop = new AbortController();
    const original = client.heartbeat;
    client.heartbeat = async (...args) => {
      try { return await original(...args); } finally { stop.abort(); }
    };
    try { await loops.heartbeatLoop(stop.signal); } finally { client.heartbeat = original; }
  };
  const advance = () => { now += 10; };
  const fresh = async () => { advance(); await heartbeat(); };
  return { worker, loops, client, runner, chat, maintenance, gate, state, reports, registrations,
    calls, sample, heartbeat, fresh, advance, now: () => now };
}

describe("Worker server-fenced DinD maintenance", () => {
  it("advertises only when wired and binds each registration to nonce and negotiation", async (t) => {
    const h = build(t);
    await h.loops.registerWithRetry(new AbortController().signal);
    const registration = h.registrations[0];
    assert.ok(registration);
    assert.ok(registration[4]?.includes("dind_maintenance_v1"));
    await h.heartbeat();
    await h.fresh();
    await h.maintenance.tick();
    assert.ok(h.maintenance.acknowledgement());
    h.state.nonce = "new-registration";
    await h.loops.registerWithRetry(new AbortController().signal);
    await h.fresh();
    await h.maintenance.tick();
    assert.equal(h.maintenance.acknowledgement(), undefined);
    assert.ok(h.gate.claimsClosed(), "registration cannot release an existing fence");
    h.state.operation = { ...operation, register_nonce: "new-registration" };
    await h.fresh();
    await h.fresh();
    await h.maintenance.tick();
    assert.ok(h.maintenance.acknowledgement());
    h.state.enabled = false;
    await h.loops.registerWithRetry(new AbortController().signal);
    await h.fresh();
    await h.maintenance.tick();
    assert.equal(h.maintenance.acknowledgement(), undefined);
    const plain = build(t, false);
    await plain.loops.registerWithRetry(new AbortController().signal);
    const plainRegistration = plain.registrations[0];
    assert.ok(plainRegistration);
    assert.equal(plainRegistration[4]?.includes("dind_maintenance_v1"), false);
  });

  it("passes exact meter and ACK in argument four and stamps custody before collecting", async (t) => {
    const h = build(t);
    await h.loops.registerWithRetry(new AbortController().signal);
    await h.heartbeat();
    h.advance();
    const sent = h.now();
    h.state.collectAdvance = 5;
    await h.heartbeat();
    await h.maintenance.tick();
    await h.heartbeat();
    assert.equal(h.reports.at(-1)?.[3]?.sample, h.sample);
    assert.equal(h.reports.at(-1)?.[3]?.ack?.custody_checked_at, new Date(sent).toISOString());
    h.state.omitSample = true;
    await h.heartbeat();
    assert.equal(h.reports.at(-1)?.[3]?.sample, undefined);
  });

  it("requested drain leaves resumes open; ready blocks run and chat without cache prune", async (t) => {
    const h = build(t);
    await h.loops.registerWithRetry(new AbortController().signal);
    h.state.operation = { ...operation, phase: "requested", fenced: false };
    await h.heartbeat();
    const counts = { run: 0, chat: 0 };
    h.client.claimRun = async () => { counts.run++; return null; };
    h.client.claimChat = async () => { counts.chat++; return null; };
    const stop = new AbortController();
    const done = Promise.all([h.loops.claimLoop(stop.signal), h.loops.chatClaimLoop(stop.signal)]);
    try {
      await until(() => counts.run > 0 && counts.chat > 0);
      h.state.operation = operation;
      await h.fresh();
      const fenced = { ...counts };
      await new Promise((r) => setTimeout(r, 20));
      assert.deepEqual(counts, fenced);
      h.state.operation = { ...operation, phase: "cancelled" };
      await h.fresh();
      await until(() => counts.run > fenced.run && counts.chat > fenced.chat);
    } finally { stop.abort(); await done; }
    assert.equal(h.gate.inFlightClaims(), 0);
  });

  for (const lane of ["run", "chat"] as const) {
    it(`in-flight ${lane} claim and execution require a fresh heartbeat after last activity`, async (t) => {
      const h = build(t);
      await h.loops.registerWithRetry(new AbortController().signal);
      const claim = deferred<ClaimResponse & ChatClaimResponse>();
      const execution = deferred();
      if (lane === "run") {
        h.client.claimRun = () => claim.promise;
        h.runner.execute = () => execution.promise;
      } else {
        h.client.claimChat = () => claim.promise;
        h.chat.execute = () => execution.promise;
      }
      const stop = new AbortController();
      const done = lane === "run" ? h.loops.claimLoop(stop.signal) : h.loops.chatClaimLoop(stop.signal);
      try {
        await until(() => h.gate.inFlightClaims() === 1);
        await h.heartbeat();
        await h.fresh();
        await h.maintenance.tick();
        assert.equal(h.calls.length, 0);
        assert.equal(h.maintenance.acknowledgement(), undefined);
        claim.resolve({ run_id: "run" } as ClaimResponse & ChatClaimResponse);
        await until(() => !h.worker.isIdle());
        await h.fresh();
        await h.maintenance.tick();
        assert.equal(h.calls.length, 0);
        assert.equal(h.maintenance.acknowledgement(), undefined);
        h.advance();
        execution.resolve();
        await until(() => h.worker.isIdle());
        await h.maintenance.tick();
        assert.equal(h.calls.length, 0, "pre-completion custody cannot authorize Docker");
        await h.fresh();
        await h.maintenance.tick();
        assert.ok(h.maintenance.acknowledgement());
        await h.heartbeat();
        assert.ok(h.reports.at(-1)?.[3]?.ack);
      } finally { claim.resolve({ run_id: "run" } as ClaimResponse & ChatClaimResponse); execution.resolve(); stop.abort(); await done; }
    });
  }

  it("run starts and joins the independent maintenance loop", async (t) => {
    const h = build(t);
    let started = false;
    let ended = false;
    t.mock.method(h.maintenance, "loop", async (signal: AbortSignal) => {
      started = true;
      await new Promise<void>((resolve) => signal.addEventListener("abort", () => resolve(), { once: true }));
      ended = true;
    });
    const stop = new AbortController();
    const done = h.worker.run(stop.signal);
    try { await until(() => started && h.reports.length > 0); }
    finally { stop.abort(); await done; }
    assert.ok(ended);
  });

  it("a response held across execution completion cannot supply post-activity custody", async (t) => {
    const h = build(t);
    await h.loops.registerWithRetry(new AbortController().signal);
    await h.heartbeat();
    const response = deferred<boolean>();
    const original = h.client.heartbeat;
    h.client.heartbeat = async () => response.promise;
    h.advance();
    const heartbeat = h.heartbeat();
    const execution = deferred();
    h.runner.execute = () => execution.promise;
    // A claim entered before the server fence is allowed to finish and become active.
    h.state.operation = { ...operation, phase: "cancelled" };
    h.maintenance.observe(h.state.operation, false, h.now());
    h.client.claimRun = async () => ({ run_id: "run" } as ClaimResponse);
    const stop = new AbortController();
    const done = h.loops.claimLoop(stop.signal);
    try {
      await until(() => !h.worker.isIdle());
      h.advance();
      execution.resolve();
      stop.abort();
      await done;
      h.state.operation = operation;
      // Restore the ready fence before receiving the old heartbeat.
      h.maintenance.observe(operation, false, h.now());
      h.advance(); // Receipt is strictly newer than both completion and the ready fence.
      response.resolve(false);
      await heartbeat;
      await h.maintenance.tick();
      assert.equal(h.calls.length, 0);
      assert.equal(h.maintenance.acknowledgement(), undefined);
      h.client.heartbeat = original;
      await h.fresh();
      await h.maintenance.tick();
      assert.ok(h.maintenance.acknowledgement());
    } finally {
      execution.resolve(); response.resolve(false); stop.abort();
      await Promise.all([done, heartbeat]);
      h.client.heartbeat = original;
    }
  });

  it("failed and malformed heartbeats invalidate ACK and retain the fence", async (t) => {
    const h = build(t);
    await h.loops.registerWithRetry(new AbortController().signal);
    await h.heartbeat();
    await h.fresh();
    await h.maintenance.tick();
    assert.ok(h.maintenance.acknowledgement());
    h.state.fail = true;
    await h.fresh();
    assert.equal(h.maintenance.acknowledgement(), undefined);
    assert.ok(h.gate.claimsClosed());
    h.state.fail = false;
    await h.fresh();
    await h.maintenance.tick();
    assert.ok(h.maintenance.acknowledgement());
    h.state.operation = { ...operation, nonce: "" };
    await h.fresh();
    assert.equal(h.maintenance.acknowledgement(), undefined);
    assert.ok(h.gate.claimsClosed());
  });

  it("heartbeats continue during a held prune; cancellation releases only after command settles", async (t) => {
    const h = build(t);
    await h.loops.registerWithRetry(new AbortController().signal);
    await h.heartbeat();
    await h.fresh();
    const held = deferred<string>();
    h.state.exec = async (argv) => argv[0] === "version" ? "1.42" : held.promise;
    const pruning = h.maintenance.tick();
    try {
      await until(() => h.calls.some((args) => args[0] === "volume"));
      const before = h.reports.length;
      await h.fresh();
      await h.fresh();
      assert.equal(h.reports.length, before + 2);
      assert.equal(h.reports.at(-1)?.[3]?.ack, undefined);
      h.state.operation = { ...operation, phase: "cancelled" };
      await h.fresh();
      assert.ok(h.gate.claimsClosed());
      held.resolve("");
      await pruning;
      assert.equal(h.gate.claimsClosed(), false);
    } finally { held.resolve(""); await pruning; }
  });
});
