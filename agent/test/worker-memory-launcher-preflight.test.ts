import assert from "node:assert/strict";
import { test } from "node:test";
import { Worker } from "../src/worker.js";
import type { WorkerClient } from "../src/client.js";
import type { Config } from "../src/config.js";
import type { RunRunner } from "../src/runner.js";
import type { ChatRunner } from "../src/chat-runner.js";
import type { JudgeRunner } from "../src/judge-runner.js";
import type { ReviewRunner } from "../src/review-runner.js";
import type { WorkerMemoryRuntime } from "../src/worker-memory-runtime.js";
import type { MemoryLauncherPreflightResult } from "../src/claude-command/preflight.js";
import { recordingLogger } from "./helpers.js";

const guard = { enabled: true as const, reserveBytes: 100, hysteresisBytes: 100,
  sampleMs: 2, responseBudgetMs: 100, rearmMs: 10, maxInterventions: 2 };

function config(memoryGuard: Config["memoryGuard"]): Config {
  return { memoryGuard, workerName: "fixture", workerTemplate: "base", pollIntervalMs: 1,
    heartbeatIntervalMs: 2, chatPollMs: 2, maxConcurrentRuns: 1, chatSessions: 1,
    dockerWiring: {}, dataDir: ".", gapFillMax: 100, outboxTerminalMaxBytes: 1 << 20 } as Config;
}

function rig(memoryGuard: Config["memoryGuard"], launcherPreflight: () => Promise<MemoryLauncherPreflightResult>) {
  const calls = { register: 0, sampler: 0 };
  const controller = new AbortController();
  const client = {
    hasFeature: () => true,
    subscribeMemoryInvalidation: () => () => undefined,
    register: async () => { calls.register++; controller.abort(); return { worker_id: "22222222-2222-4222-8222-222222222222" }; },
    heartbeat: async () => undefined,
    claimRun: async () => null,
    claimChat: async () => null,
  } as unknown as WorkerClient;
  const runner = { shutdown: () => undefined, resumePendingRecoveries: async () => undefined,
    settlePendingPredecessors: async () => undefined, observeSettlementTerminalAck: async () => undefined,
    snapshotBootRecoveries: async () => undefined } as unknown as RunRunner;
  const memory = { run: async () => { calls.sampler++; }, stop: async () => undefined } as unknown as WorkerMemoryRuntime;
  const { logger, lines } = recordingLogger();
  const worker = new Worker(config(memoryGuard), client, runner, {} as ChatRunner, {} as JudgeRunner, {} as ReviewRunner,
    logger, () => ({ ok: true, missing: [] }), undefined, undefined, undefined, undefined,
    undefined, undefined, undefined, undefined, undefined, undefined, undefined, undefined, undefined, undefined,
    undefined, memoryGuard?.enabled ? memory : undefined, launcherPreflight);
  return { worker, calls, controller, lines };
}

test("a guard-enabled worker refuses to register when the launcher preflight fails", async () => {
  let preflights = 0;
  const f = rig(guard, async () => { preflights++; return { ok: false, reason: "profile:capDrop" }; });
  await assert.rejects(f.worker.run(f.controller.signal), /memory guard launcher preflight failed: profile:capDrop/);
  assert.equal(preflights, 1);
  assert.equal(f.calls.register, 0, "no register after a failed launcher preflight");
  assert.ok(f.lines.some((line) => (line as { level: string; msg: string; reason?: string }).level === "error"
    && (line as { msg: string }).msg === "memory guard launcher preflight FAILED — refusing to register"
    && (line as { reason?: string }).reason === "profile:capDrop"));
});

test("a guard-enabled worker registers when the launcher preflight passes", async () => {
  const f = rig(guard, async () => ({ ok: true }));
  await f.worker.run(f.controller.signal);
  assert.equal(f.calls.register, 1);
});

for (const memoryGuard of [undefined, { enabled: false as const }]) {
  test("the launcher preflight does not run with the guard disabled: " + JSON.stringify(memoryGuard ?? null), async () => {
    let preflights = 0;
    const f = rig(memoryGuard, async () => { preflights++; return { ok: false, reason: "must not run" }; });
    await f.worker.run(f.controller.signal);
    assert.equal(preflights, 0);
    assert.equal(f.calls.register, 1);
  });
}
