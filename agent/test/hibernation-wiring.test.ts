import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { Worker } from "../src/worker.js";
import type { WorkerClient } from "../src/client.js";
import type { Config } from "../src/config.js";
import type { RunRunner } from "../src/runner.js";
import type { ChatRunner } from "../src/chat-runner.js";
import type { JudgeRunner } from "../src/judge-runner.js";
import type { ReviewRunner } from "../src/review-runner.js";
import type { StatsCollector } from "../src/stats.js";
import { DindPruneController, DindPruneGate } from "../src/dind-prune.js";
import {
  hibernationWriters, HibernationInventoryController, HibernationWriterRegistry,
  type HibernationInventoryMember, type HibernationDeps, type HibernationExclusionReport,
} from "../src/hibernation-inventory.js";
import { hibernationFinalizeBudgetMs, hibernationParticipates } from "../src/main.js";
import { nullLogger } from "./helpers.js";

// PRD #4208 M1 wiring: who advertises the capability, which writers honour the latch, and the
// heartbeat-to-controller hand-off.

function worker(hibernation: HibernationInventoryController | undefined, client: Record<string, unknown> = {}) {
  const registrations: Parameters<WorkerClient["register"]>[] = [];
  const c = {
    register: async (...args: Parameters<WorkerClient["register"]>) => {
      registrations.push(args);
      return { worker_id: "worker", register_nonce: "n" };
    },
    hasFeature: () => false,
    heartbeat: async () => undefined,
    hibernationExclusionDelivered: () => false,
    ...client,
  };
  const runner = { execute: async () => {}, resumePendingRecoveries: async () => {}, settlePendingPredecessors: async () => {} };
  const collector = { collect: () => ({}), latestDindSample: undefined } as unknown as StatsCollector;
  const w = new Worker(
    { workerName: "w", workerTemplate: "base", maxConcurrentRuns: 1, chatSessions: 1,
      pollIntervalMs: 1, chatPollMs: 1, heartbeatIntervalMs: 1,
      dockerWiring: { dockerHost: "tcp://dind:2375" } } as Config,
    c as unknown as WorkerClient, runner as unknown as RunRunner, { execute: async () => {} } as unknown as ChatRunner,
    {} as JudgeRunner, {} as ReviewRunner, nullLogger(), () => ({ ok: true, missing: [] }),
    undefined, undefined, undefined, undefined, undefined, undefined, undefined,
    () => collector, undefined, undefined, undefined,
    undefined, undefined, undefined, undefined, undefined, undefined, undefined, hibernation,
  );
  const loops = w as unknown as {
    registerWithRetry(signal: AbortSignal): Promise<void>;
    heartbeatLoop(signal: AbortSignal): Promise<void>;
  };
  return { w, loops, registrations, c };
}

function controller(registry = new HibernationWriterRegistry(), overrides: Partial<HibernationDeps> = {}) {
  return new HibernationInventoryController({
    registry, gate: new DindPruneGate(),
    client: {
      hasFeature: () => true, currentRegisterNonce: () => "n",
      postHibernationInventory: async () => {}, postHibernationExclusionFinal: async () => {},
    },
    ownership: undefined, laneSupported: true, dockerStorage: true, podUid: () => undefined, isIdle: () => true,
    proof: async () => true,
    census: async () => ({ complete: false, entries: [], fingerprint: "", digest: "", docker: null, uncovered: [] }),
    recomputeFingerprint: async () => ({ digest: "", fingerprint: "" }),
    fatalExit: () => {}, log: nullLogger(),
    ...overrides,
  });
}

async function heldController(losePostResponse: boolean) {
  const registry = new HibernationWriterRegistry();
  const gate = new DindPruneGate();
  let mode: "shared" | "exclusive" = "shared";
  const member: HibernationInventoryMember = {
    op_id: "55555555-5555-5555-5555-555555555555", register_nonce: "n", phase: "awaiting",
    pod_uid: "66666666-6666-6666-6666-666666666666", deployment_resource_version: "r1",
    pvcs: [{ role: "data", name: "data", uid: "88888888-8888-8888-8888-888888888888" }],
  };
  const hib = controller(registry, {
    gate, podUid: () => member.pod_uid,
    ownership: { get mode() { return mode; }, async upgradeExclusive() { mode = "exclusive"; return true; },
      async reacquireShared() { mode = "shared"; } } as never,
    client: { hasFeature: () => true, currentRegisterNonce: () => "n",
      postHibernationInventory: async (body) => {
        if (losePostResponse && "complete" in body) throw new Error("post-EX response lost after commit");
      }, postHibernationExclusionFinal: async () => {} },
  });
  hib.observe(member);
  await hib.whenIdle();
  return { hib, gate, registry, member };
}

describe("hibernation wiring", () => {
  it("advertises hibernation_inventory_v1 only when the controller is constructed", async () => {
    const withController = worker(controller());
    await withController.loops.registerWithRetry(new AbortController().signal);
    assert.ok(withController.registrations[0]?.[4]?.includes("hibernation_inventory_v1"));
    const without = worker(undefined);
    await without.loops.registerWithRetry(new AbortController().signal);
    assert.equal(without.registrations[0]?.[4]?.includes("hibernation_inventory_v1"), false);
  });

  it("the lane supports exclusion only on the DinD lane with a supported epoch ownership", () => {
    const dind = { dockerWiring: { dockerHost: "tcp://dind:2375" } };
    assert.equal(hibernationParticipates(dind, "shared"), true);
    assert.equal(hibernationParticipates(dind, "exclusive"), true);
    assert.equal(hibernationParticipates(dind, "unsupported"), false);
    assert.equal(hibernationParticipates(dind, undefined), false);
    assert.equal(hibernationParticipates({ dockerWiring: {} }, "shared"), false);
    assert.equal(hibernationParticipates({ ...dind, fetcherUrl: "https://f", fetcherCaFile: "/ca" }, "shared"), false);
  });

  it("a wrapped writer (the dind prune loop) skips its tick while the registry is latched", async (t) => {
    t.after(() => hibernationWriters.unlatch());
    let reads = 0;
    let current = new AbortController();
    const ctl = new DindPruneController({
      dockerHost: "tcp://127.0.0.1:2375", heartbeatIntervalMs: 15_000, workerName: "w",
      gate: new DindPruneGate(), isIdle: () => true, log: nullLogger(),
      readSample: () => { reads++; return null; },
      exec: async () => "",
      sleep: async () => { current.abort(); }, // one loop iteration per run
    });
    hibernationWriters.latch();
    await ctl.loop(current.signal);
    assert.equal(reads, 0, "no tick while latched");
    hibernationWriters.unlatch();
    current = new AbortController();
    await ctl.loop(current.signal);
    assert.ok(reads > 0, "the tick runs once unlatched");
  });

  it("a successful heartbeat without the negotiated feature releases an active hold", async () => {
    const seen: (HibernationInventoryMember | null | undefined)[] = [];
    const hib = { observe: (m: HibernationInventoryMember | null | undefined) => { seen.push(m); },
      exclusionReport: () => undefined } as unknown as HibernationInventoryController;
    const h = worker(hib);
    await h.loops.registerWithRetry(new AbortController().signal);
    const stop = new AbortController();
    const original = h.c.heartbeat;
    h.c.heartbeat = async () => { try { return await original(); } finally { stop.abort(); } };
    await h.loops.heartbeatLoop(stop.signal);
    assert.deepEqual(seen, [null], "null (explicitly none), never undefined, so a hold releases");
  });

  for (const first of ["failed", "omitted"] as const) {
    it(`retries a loss after a ${first} heartbeat and acknowledges only the delivered report`, async () => {
      const r = await heldController(true);
      const stop = new AbortController();
      let delivered = false;
      let calls = 0;
      const reports: (HibernationExclusionReport | undefined)[] = [];
      const h = worker(r.hib, {
        hasFeature: (f: string) => f === "hibernation_inventory_v1",
        latestHibernationInventory: { ...r.member, phase: "accepted" },
        hibernationExclusionDelivered: () => delivered,
      });
      await h.loops.registerWithRetry(new AbortController().signal);
      h.c.heartbeat = async (...args: Parameters<WorkerClient["heartbeat"]>) => {
        reports.push(args[7]);
        calls++;
        delivered = calls === 2;
        if (calls === 2) stop.abort();
        if (calls === 1 && first === "failed") throw new Error("HTTP 500");
        return undefined;
      };
      await h.loops.heartbeatLoop(stop.signal);
      const lost = { op_id: r.member.op_id, held: false };
      assert.deepEqual(reports, [lost, lost]);
      assert.equal(r.hib.exclusionReport(), undefined, "only the second, delivered send clears it");
    });
  }

  it("a failed heartbeat preserves a real held exclusion", async () => {
    const r = await heldController(false);
    const h = worker(r.hib);
    await h.loops.registerWithRetry(new AbortController().signal);
    const stop = new AbortController();
    h.c.heartbeat = async () => { stop.abort(); throw new Error("HTTP 500"); };
    await h.loops.heartbeatLoop(stop.signal);
    await r.hib.whenIdle();
    assert.equal(r.hib.exclusionReport()?.held, true);
    assert.equal(r.gate.heldBy("hibernation_inventory"), true);
    assert.equal(r.registry.isLatched(), true);
    r.hib.observe(null);
    await r.hib.whenIdle();
  });

  it("a failed heartbeat leaves the observation unknown", async () => {
    const seen: unknown[] = [];
    const hib = { observe: (m: unknown) => { seen.push(m); }, exclusionReport: () => undefined } as unknown as HibernationInventoryController;
    const h = worker(hib);
    await h.loops.registerWithRetry(new AbortController().signal);
    const stop = new AbortController();
    h.c.heartbeat = async () => { stop.abort(); throw new Error("down"); };
    await h.loops.heartbeatLoop(stop.signal);
    assert.deepEqual(seen, [undefined]);
  });

  it("bounds the shutdown finalize by the barrier deadline, skipping when too little remains", () => {
    assert.equal(hibernationFinalizeBudgetMs(undefined, 0), 15_000);
    assert.equal(hibernationFinalizeBudgetMs(100_000, 0), 15_000);
    assert.equal(hibernationFinalizeBudgetMs(10_000, 0), 10_000, "never past the deadline");
    assert.equal(hibernationFinalizeBudgetMs(3_000, 0), 3_000);
    assert.equal(hibernationFinalizeBudgetMs(2_999, 0), undefined);
    assert.equal(hibernationFinalizeBudgetMs(1_000, 5_000), undefined, "deadline already passed");
  });
});
