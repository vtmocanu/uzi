import { afterEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";

import { Worker } from "../src/worker.js";
import { Outbox } from "../src/outbox.js";
import type { Config } from "../src/config.js";
import type { WorkerClient } from "../src/client.js";
import type { RunRunner } from "../src/runner.js";
import type { ChatRunner } from "../src/chat-runner.js";
import type { JudgeRunner } from "../src/judge-runner.js";
import type { ReviewRunner } from "../src/review-runner.js";
import type { ClaimResponse, ChatClaimResponse } from "../src/protocol.js";
import type { OutboxDepth } from "../src/outbox.js";
import { nullLogger, recordingLogger } from "./helpers.js";
import { sleep } from "../src/util.js";

// PRD #1391 M2 — the per-worker outbox drainer: single-flight across concurrent
// heartbeat/boot triggers, re-arm after a run's segments retire, a boot drain that
// runs without waiting for the first heartbeat, and the crash-before-range-flush
// admitted-loss log a fresh worker emits over a spilled_unclean run.

const noJudge = { execute: async () => {} } as unknown as JudgeRunner;
const noReview = { execute: async () => {} } as unknown as ReviewRunner;
const noResumeRecoveries = { resumePendingRecoveries: async () => {} };
const okPreflight = (): { ok: boolean; missing: string[] } => ({ ok: true, missing: [] });
const idleRunner = { ...noResumeRecoveries, execute: async () => {} } as unknown as RunRunner;
const idleChat = { execute: async () => {} } as unknown as ChatRunner;

function fakeConfig(over: Partial<Config> = {}): Config {
  return {
    workerName: "w1",
    workerTemplate: "base",
    pollIntervalMs: 1,
    heartbeatIntervalMs: 5,
    chatPollMs: 1,
    chatSessions: 1,
    maxConcurrentRuns: 1,
    dockerWiring: {},
    dataDir: "/tmp/does-not-matter",
    ...over,
  } as unknown as Config;
}

/** A client whose lanes are idle; heartbeat is programmable so a test can make it
 *  succeed (heartbeat-driven drains fire) or always throw (only the boot drain runs). */
function idleClient(heartbeat: () => Promise<void> = async () => {}): WorkerClient {
  return {
    register: async () => ({}),
    heartbeat,
    claimRun: async (): Promise<ClaimResponse | null> => null,
    claimChat: async (): Promise<ChatClaimResponse | null> => null,
  } as unknown as WorkerClient;
}

const depth = (runId: string): OutboxDepth => ({
  runId,
  pendingMessages: 1,
  pendingTerminal: 0,
  staleRetired: 0,
  since: 0,
});

const tmpRoots: string[] = [];
afterEach(async () => {
  for (const r of tmpRoots.splice(0)) await fs.rm(r, { recursive: true, force: true }).catch(() => undefined);
});

describe("Worker outbox drainer (PRD #1391 M2)", () => {
  it("drain is SINGLE-FLIGHT: overlapping boot + heartbeat triggers never run the drain body concurrently", async () => {
    let release!: () => void;
    const gate = new Promise<void>((r) => (release = r));
    let concurrent = 0;
    let maxConcurrent = 0;
    let drainCalls = 0;
    const outbox = {
      uncleanRuns: () => [],
      runsWithPending: () => ["r1"],
      depthFor: (id: string) => depth(id),
      drainRun: async () => {
        drainCalls += 1;
        concurrent += 1;
        maxConcurrent = Math.max(maxConcurrent, concurrent);
        await gate; // hold the drain body open across several heartbeat ticks
        concurrent -= 1;
        return { retired: false, staleRetired: 0 };
      },
    } as unknown as Outbox;

    const controller = new AbortController();
    const worker = new Worker(
      fakeConfig(),
      idleClient(), // heartbeats SUCCEED, so each tick tries to drain
      idleRunner,
      idleChat,
      noJudge,
      noReview,
      nullLogger(),
      okPreflight,
      outbox,
      new Map(),
    );
    const done = worker.run(controller.signal);

    // Let many heartbeat ticks fire while the one in-flight drain is held open.
    await sleep(80);
    assert.strictEqual(maxConcurrent, 1, "the single-flight guard must keep exactly one drain body running at a time");
    assert.strictEqual(concurrent, 1, "the held boot drain is still the only one running");
    assert.ok(drainCalls <= 1, "the concurrent heartbeat ticks were all skipped, not queued");

    release();
    controller.abort();
    await done;
  });

  it("re-arms a run's live batcher (fires its rearm hook) once its segments retire", async () => {
    let rearmed = 0;
    const rearm = new Map<string, () => void>([["r1", () => (rearmed += 1)]]);
    let retired = false;
    const outbox = {
      uncleanRuns: () => [],
      runsWithPending: () => (retired ? [] : ["r1"]),
      depthFor: (id: string) => depth(id),
      drainRun: async () => {
        retired = true;
        return { retired: true, staleRetired: 0 };
      },
    } as unknown as Outbox;

    const controller = new AbortController();
    const worker = new Worker(
      fakeConfig(),
      idleClient(),
      idleRunner,
      idleChat,
      noJudge,
      noReview,
      nullLogger(),
      okPreflight,
      outbox,
      rearm,
    );
    const done = worker.run(controller.signal);

    await pollUntil(() => rearmed > 0, 2000, "the batcher's rearm hook fires after retire");
    controller.abort();
    await done;
    assert.ok(rearmed >= 1, "a retired run re-arms its batcher exactly through the shared registry hook");
  });

  it("the heartbeat-triggered drain is FIRE-AND-FORGET: a slow drain never delays the next heartbeat", async () => {
    // drainOutbox replays the ENTIRE per-run backlog with no time budget. If the
    // heartbeat loop AWAITED it, a slow drain would push the next heartbeat past the
    // api's 45s stale cutoff → the sweeper re-queues still-running runs (duplicate
    // execution). This proves the heartbeat keeps ticking while a drain is in flight.
    let release!: () => void;
    const gate = new Promise<void>((r) => (release = r));
    let drainStarted = 0;
    const outbox = {
      uncleanRuns: () => [],
      runsWithPending: () => ["r1"],
      depthFor: (id: string) => depth(id),
      drainRun: async () => {
        drainStarted += 1;
        // The boot drain (call #1) completes fast so the single-flight guard clears;
        // the first heartbeat-triggered drain (call #2) blocks on the gate. If the loop
        // awaited it, heartbeats would stall here.
        if (drainStarted === 1) return { retired: false, staleRetired: 0 };
        await gate;
        return { retired: false, staleRetired: 0 };
      },
    } as unknown as Outbox;

    let heartbeats = 0;
    const controller = new AbortController();
    const worker = new Worker(
      fakeConfig(),
      idleClient(async () => {
        heartbeats += 1; // every heartbeat SUCCEEDS, so each tick triggers a drain
      }),
      idleRunner,
      idleChat,
      noJudge,
      noReview,
      nullLogger(),
      okPreflight,
      outbox,
      new Map(),
    );
    const done = worker.run(controller.signal);

    // Heartbeats must keep advancing while a heartbeat-triggered drain is blocked on the
    // gate. An awaited drain would freeze the loop at ~1 tick.
    await pollUntil(() => heartbeats >= 3, 2000, "heartbeats keep firing while the drain is blocked");
    assert.ok(drainStarted >= 2, "a heartbeat-triggered drain started (call #2) and is still blocked on the gate");

    release();
    controller.abort();
    await done;
  });

  it("the BOOT drain runs even when heartbeats never succeed", async () => {
    let drainCalls = 0;
    const outbox = {
      uncleanRuns: () => [],
      runsWithPending: () => ["r1"],
      depthFor: (id: string) => depth(id),
      drainRun: async () => {
        drainCalls += 1;
        return { retired: true, staleRetired: 0 };
      },
    } as unknown as Outbox;

    const controller = new AbortController();
    const worker = new Worker(
      fakeConfig(),
      idleClient(async () => {
        throw new Error("api down"); // every heartbeat fails ⇒ no heartbeat-driven drain
      }),
      idleRunner,
      idleChat,
      noJudge,
      noReview,
      nullLogger(),
      okPreflight,
      outbox,
      new Map(),
    );
    const done = worker.run(controller.signal);

    await pollUntil(() => drainCalls >= 1, 2000, "the boot drain fires without a successful heartbeat");
    controller.abort();
    await done;
    // The only possible source is the boot drain: heartbeats always threw, so no
    // heartbeat-driven drain ever ran.
    assert.strictEqual(drainCalls, 1, "exactly the one boot drain ran; heartbeat drains never fired");
  });

  it("a fresh Worker over a spilled_unclean run logs an admitted-loss line that NAMES NO span and NO count", async () => {
    const dir = await fs.mkdtemp(path.join(os.tmpdir(), "worker-outbox-"));
    tmpRoots.push(dir);
    const root = path.join(dir, "outbox");

    // Simulate a crash mid-spill: mark the run unclean on disk (the flush that would
    // have sized the loss never ran), then load a FRESH store over the same root.
    const first = new Outbox({ root, log: nullLogger(), runMaxBytes: 1 << 26, maxBytes: 1 << 29, retentionMs: 1 });
    await first.init();
    await first.markSpillUnclean("r1");

    const reloaded = new Outbox({ root, log: nullLogger(), runMaxBytes: 1 << 26, maxBytes: 1 << 29, retentionMs: 1 });
    await reloaded.init();
    assert.ok(reloaded.uncleanRuns().includes("r1"), "the spill-unclean flag survives restart");

    const { logger, lines } = recordingLogger();
    const controller = new AbortController();
    const worker = new Worker(
      fakeConfig(),
      idleClient(),
      idleRunner,
      idleChat,
      noJudge,
      noReview,
      logger,
      okPreflight,
      reloaded,
      new Map(),
    );
    const done = worker.run(controller.signal);
    await pollUntil(
      () => lines.some((l) => isUncleanLog(l)),
      2000,
      "the admitted-loss line is logged at boot",
    );
    controller.abort();
    await done;

    const record = lines.find(isUncleanLog) as Record<string, unknown> | undefined;
    assert.ok(record, "a fresh worker must warn that an unflushed tail MAY have been lost");
    assert.strictEqual(record["run_id"], "r1", "the log names the run");
    // It must invent NO record: no span (first/last seq), no count.
    for (const forbidden of ["first_seq", "last_seq", "seq", "count", "span", "dropped", "pending_messages"]) {
      assert.strictEqual(record[forbidden], undefined, `the admitted-loss log must not fabricate a ${forbidden}`);
    }
  });
});

function isUncleanLog(l: unknown): l is Record<string, unknown> {
  return (
    !!l &&
    typeof l === "object" &&
    typeof (l as { msg?: unknown }).msg === "string" &&
    (l as { msg: string }).msg.includes("unflushed tail may have been lost")
  );
}

async function pollUntil(pred: () => boolean, ms: number, label: string): Promise<void> {
  const deadline = Date.now() + ms;
  while (Date.now() < deadline) {
    if (pred()) return;
    await sleep(10);
  }
  assert.fail(`timed out waiting for: ${label}`);
}
