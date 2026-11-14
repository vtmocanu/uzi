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
import type { ClaimResponse, HibernationCachePrune, HibernationCachePruneReport } from "../src/protocol.js";
import { HibernationCachePruneController } from "../src/hibernation-cache-prune.js";
import type { DindMaintenanceController } from "../src/dind-maintenance.js";
import { DindPruneGate, dockerExecEnvironment } from "../src/dind-prune.js";
import { nullLogger } from "./helpers.js";

after(() => { rmSync(dockerExecEnvironment("tcp://dind:2375").DOCKER_CONFIG ?? "", { recursive: true, force: true }); });

const NONCE = "registration";
const OP = "22222222-2222-2222-2222-222222222222";
const requested: HibernationCachePrune = {
  op_id: OP, register_nonce: NONCE, phase: "requested", cancel_requested: false, reconcile_seq: 1,
};

type Loops = {
  registerWithRetry(signal: AbortSignal): Promise<void>;
  heartbeatLoop(signal: AbortSignal): Promise<void>;
  claimLoop(signal: AbortSignal): Promise<void>;
};

function build(t: TestContext, opts: { wired?: boolean; maintenance?: unknown; hibernation?: unknown } = {}) {
  let now = 1_700_000_000_000;
  t.mock.method(Date, "now", () => now);
  const gate = new DindPruneGate();
  const calls: string[] = [];
  let worker!: Worker;
  const state = {
    projection: requested as HibernationCachePrune | null | undefined,
    outcome: "sent" as "none" | "sent" | "dropped",
    fail: false,
  };
  const real = new HibernationCachePruneController({
    gate, dockerHost: "tcp://dind:2375", heartbeatIntervalMs: 15_000, isIdle: () => worker.isIdle(),
    log: nullLogger(), exec: async (argv) => { calls.push(argv.join(" ")); return "1.45"; },
    runStep: async (argv) => { calls.push(argv.join(" ")); return { code: 0, signal: null }; },
    sleep: async () => {},
  });
  const hibernation = opts.hibernation ?? (opts.wired === false ? undefined : real);
  const registrations: Parameters<WorkerClient["register"]>[] = [];
  const heartbeats: Parameters<WorkerClient["heartbeat"]>[] = [];
  const client = {
    register: async (...args: Parameters<WorkerClient["register"]>) => {
      registrations.push(args);
      return { worker_id: "worker", register_nonce: NONCE };
    },
    hasFeature: () => false,
    get latestDindMaintenance() { return null; },
    get latestHibernationCachePrune() { return state.projection; },
    get hibernationReportOutcome() { return state.outcome; },
    heartbeat: async (...args: Parameters<WorkerClient["heartbeat"]>) => {
      heartbeats.push(args);
      if (state.fail) throw new Error("heartbeat unavailable");
      return false;
    },
    claimRun: async (): Promise<ClaimResponse | null> => null,
    claimChat: async () => null,
  };
  const runner = {
    execute: async () => {}, resumePendingRecoveries: async () => {}, settlePendingPredecessors: async () => {},
  } as unknown as RunRunner;
  const collector = { collect: () => ({}), latestDindSample: undefined } as unknown as StatsCollector;
  worker = new Worker(
    { workerName: "w", workerTemplate: "base", maxConcurrentRuns: 1, chatSessions: 1,
      pollIntervalMs: 1, chatPollMs: 1, heartbeatIntervalMs: 1,
      dockerWiring: { dockerHost: "tcp://dind:2375" } } as Config,
    client as unknown as WorkerClient, runner, { execute: async () => {} } as unknown as ChatRunner,
    {} as JudgeRunner, {} as ReviewRunner, nullLogger(), () => ({ ok: true, missing: [] }),
    undefined, undefined, undefined, undefined, undefined, undefined, undefined,
    () => collector, undefined, undefined, opts.maintenance as DindMaintenanceController | undefined,
    undefined, undefined, gate, undefined, undefined, undefined,
    hibernation as HibernationCachePruneController | undefined,
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
  return { worker, loops, client, real, gate, state, registrations, heartbeats, calls, heartbeat,
    advance: () => { now += 10; } };
}

describe("Worker hibernation cache prune", () => {
  it("advertises the capability only when the controller is wired", async (t) => {
    const wired = build(t);
    await wired.loops.registerWithRetry(new AbortController().signal);
    assert.ok(wired.registrations[0]?.[4]?.includes("hibernation_cache_prune_v1"));
    const plain = build(t, { wired: false });
    await plain.loops.registerWithRetry(new AbortController().signal);
    assert.equal(plain.registrations[0]?.[4]?.includes("hibernation_cache_prune_v1"), false);
  });

  it("drives requested to ready through heartbeats and hands the report to the client", async (t) => {
    const h = build(t);
    await h.loops.registerWithRetry(new AbortController().signal);
    await h.heartbeat(); // observes requested
    await h.real.tick();
    h.advance();
    await h.heartbeat(); // fresh custody
    await h.real.tick();
    assert.ok(h.gate.onlyHeldBy("hibernation"));
    h.advance();
    await h.heartbeat();
    const sent = h.heartbeats.at(-1)?.[6] as HibernationCachePruneReport | undefined;
    assert.equal(sent?.kind, "ready");
    assert.equal(sent?.register_nonce, NONCE);
    // The api's started projection runs both steps exactly once.
    h.state.projection = { ...requested, phase: "started", reconcile_seq: 2 };
    h.advance();
    await h.heartbeat();
    await new Promise((r) => setImmediate(r));
    h.advance();
    await h.heartbeat();
    assert.equal((h.heartbeats.at(-1)?.[6] as HibernationCachePruneReport | undefined)?.kind, "result");
    assert.deepEqual(h.calls, ["version --format {{.Server.APIVersion}}", "image prune -f", "builder prune -f"]);
  });

  it("never sends a cache prune report beside a maintenance ACK, and defers it to the next heartbeat", async (t) => {
    const ack = { id: "op" };
    let acks = 1;
    const reports: HibernationCachePruneReport[] = [{ op_id: OP, register_nonce: NONCE, kind: "ready" }];
    let reportCalls = 0;
    const maintenance = {
      gate: new DindPruneGate(), register: () => {}, updateNegotiation: () => {}, observe: () => {},
      noteActivityEnded: () => {}, loop: async () => {}, acknowledgement: () => (acks-- > 0 ? ack : undefined),
    };
    const hibernation = {
      register: () => {}, observe: () => {}, noteActivityEnded: () => {}, loop: async () => {},
      report: () => { reportCalls++; return reports[0]; }, reported: () => {},
    };
    const h = build(t, { maintenance, hibernation });
    await h.loops.registerWithRetry(new AbortController().signal);
    await h.heartbeat();
    assert.equal(h.heartbeats.at(-1)?.[3]?.ack, ack);
    assert.equal(h.heartbeats.at(-1)?.[6], undefined);
    assert.equal(reportCalls, 0, "the report is not even built while an ACK is pending");
    await h.heartbeat();
    assert.equal(h.heartbeats.at(-1)?.[3]?.ack, undefined);
    assert.equal(h.heartbeats.at(-1)?.[6], reports[0]);
  });

  it("a failed heartbeat keeps the report pending and makes the projection unknown", async (t) => {
    const seen: unknown[] = [];
    const outcomes: string[] = [];
    const hibernation = {
      register: () => {}, noteActivityEnded: () => {}, loop: async () => {},
      observe: (p: unknown, retaining: unknown) => { seen.push([p, retaining]); },
      report: () => ({ op_id: OP, register_nonce: NONCE, kind: "ready" }),
      reported: (_r: unknown, outcome: string) => { outcomes.push(outcome); },
    };
    const h = build(t, { hibernation });
    await h.loops.registerWithRetry(new AbortController().signal);
    h.state.fail = true;
    await h.heartbeat();
    assert.deepEqual(seen, [[undefined, undefined]]);
    assert.deepEqual(outcomes, []);
    h.state.fail = false;
    h.state.outcome = "dropped";
    await h.heartbeat();
    assert.deepEqual(outcomes, ["dropped"]);
    h.state.outcome = "none";
    await h.heartbeat();
    assert.deepEqual(outcomes, ["dropped", "failed"]);
  });

  it("run starts and joins the hibernation loop", async (t) => {
    const h = build(t);
    let started = false;
    let ended = false;
    t.mock.method(h.real, "loop", async (signal: AbortSignal) => {
      started = true;
      await new Promise<void>((resolve) => signal.addEventListener("abort", () => resolve(), { once: true }));
      ended = true;
    });
    const stop = new AbortController();
    const done = h.worker.run(stop.signal);
    try {
      for (let i = 0; i < 500 && !(started && h.heartbeats.length > 0); i++) await new Promise((r) => setTimeout(r, 2));
      assert.ok(started && h.heartbeats.length > 0);
    } finally { stop.abort(); await done; }
    assert.ok(ended);
  });
});
