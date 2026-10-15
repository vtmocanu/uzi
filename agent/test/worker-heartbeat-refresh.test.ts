import { it } from "node:test";
import assert from "node:assert/strict";
import type { Worker as WorkerType } from "../src/worker.js";

import type { Config } from "../src/config.js";
import type { RunRunner } from "../src/runner.js";
import type { ChatRunner } from "../src/chat-runner.js";
import type { JudgeRunner } from "../src/judge-runner.js";
import type { ReviewRunner } from "../src/review-runner.js";
import type { StatsCollector } from "../src/stats.js";
import type { DindPruneController } from "../src/dind-prune.js";


it("regular Worker ticks retain 15s cadence, original report and nonce, epochs and custody without boot calls", async t => {
  t.mock.timers.enable({ apis: ["setTimeout", "Date"], now: 100_000 });
  const { Worker } = await import("../src/worker.js");
  const { WorkerClient } = await import("../src/client.js");
  const { ActiveRunRegistry } = await import("../src/active-run-registry.js");
  const { nullLogger } = await import("./helpers.js");
  const nonce = Buffer.alloc(32, 11).toString("base64url");
  const requests: { path: string; body: Record<string, unknown> }[] = [];
  const custody: unknown[][] = [];
  const client = new WorkerClient("http://example.test", "join-fixture", "m3", nullLogger(), {
    fetch: async (input, init) => {
      const path = String(input);
      requests.push({ path, body: JSON.parse(String(init?.body)) });
      return new Response(JSON.stringify(path.endsWith("/register")
        ? { worker_id: "worker", register_nonce: nonce, protocol_features: ["active_run_snapshot"] }
        : { protocol_features: ["active_run_snapshot", "heartbeat_protocol_refresh_v1"], worker: { retaining_unpublished_work: false } }));
    },
  });
  const registry = new ActiveRunRegistry();
  registry.add("held-run", 7);
  registry.setPhase("held-run", "awaiting_approval");
  const worker: WorkerType = new Worker({
    workerName: "m3", workerTemplate: "base", maxConcurrentRuns: 4, crossCheckSlots: 2,
    heartbeatIntervalMs: 15_000, pollIntervalMs: 1000, dataDir: ".",
    codexHarness: { advertise: false },
  } as Config, client, {} as RunRunner, {} as ChatRunner, {} as JudgeRunner, {} as ReviewRunner,
  nullLogger(), () => ({ ok: true, missing: [] }), undefined, undefined, registry,
  undefined, { setWorkerId: () => {}, recordCustody: (...args: unknown[]) => custody.push(args) } as unknown as DindPruneController,
  undefined, undefined, () => ({ collect: () => ({ mem_bytes: 100, source: "process" }) }) as unknown as StatsCollector);
  const loops = worker as unknown as {
    registerWithRetry(signal: AbortSignal): Promise<void>;
    heartbeatLoop(signal: AbortSignal): Promise<void>;
  };
  await loops.registerWithRetry(new AbortController().signal);
  const registered = requests[0]!.body;
  assert.ok((registered.protocol_capabilities as string[]).includes("completion_interlock_v1"));
  assert.equal((registered.protocol_capabilities as string[]).includes("codex_runtime_v2"), false);
  const stop = new AbortController();
  const loop = loops.heartbeatLoop(stop.signal);
  const flush = async () => { for (let n = 0; n < 10; n++) await new Promise<void>(resolve => setImmediate(resolve)); };
  try {
    await flush();
    assert.equal(requests.length, 2);
    t.mock.timers.tick(14_999);
    await flush();
    assert.equal(requests.length, 2);
    t.mock.timers.tick(1);
    await flush();
    assert.equal(requests.length, 3);
    const ticks = requests.slice(1).map(r => r.body);
    assert.equal(ticks[0]!.protocol_refresh, undefined);
    assert.deepEqual(ticks[1]!.protocol_refresh, {
      protocol_capabilities: registered.protocol_capabilities, max_concurrent_runs: 4,
      max_cross_check_slots: 2, register_nonce: nonce,
    }, "regular tick 2 must send the original implementation report");
    for (const [index, tick] of ticks.entries()) assert.deepEqual(tick.active_snapshot, {
      snapshot_epoch: index + 1, active: [{ run_id: "held-run", claim_generation: 7, phase: "awaiting_approval", terminal_pending: false }],
      pending_overflow: false, pending_all: [], register_nonce: nonce,
    });
    assert.deepEqual(custody, [[false, 100_000], [false, 115_000]]);
    const { protocol_refresh: report, ...withoutReport } = ticks[1]!;
    console.log("M3_WORKER_WIRE " + JSON.stringify({
      implemented_count: (registered.protocol_capabilities as string[]).length,
      request_bytes: Buffer.byteLength(JSON.stringify(ticks[1])),
      request_bytes_without_refresh: Buffer.byteLength(JSON.stringify(withoutReport)),
      report_bytes: Buffer.byteLength(JSON.stringify(report)),
      successful_regular_heartbeats: ticks.length,
    }));
    assert.equal(requests.filter(r => r.path.endsWith("/register")).length, 1);
    assert.ok(requests.every(r => r.path.endsWith("/register") || r.path.endsWith("/heartbeat")));
  } finally {
    stop.abort();
    await loop;
  }
});
