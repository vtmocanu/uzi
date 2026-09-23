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
      // PRD #1391 Run B M4: the boot terminal resolve reads these; these drainer tests carry no
      // pending terminal, so the store is enabled and lists nothing (resolveBootTerminals no-ops).
      isDisabled: () => false,
      listPendingTerminals: () => [],
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
      // PRD #1391 Run B M4: the boot terminal resolve reads these; these drainer tests carry no
      // pending terminal, so the store is enabled and lists nothing (resolveBootTerminals no-ops).
      isDisabled: () => false,
      listPendingTerminals: () => [],
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

  it("re-resolves a run's pending terminal once its segments retire (finish-during-outage lands with no restart)", async () => {
    // PRD #1391 Run B regression: a run that FINISHED during an outage journals its terminal and
    // loses its first send. On recovery the terminal-resolve can reach the api BEFORE this run's own
    // message drain completes, get messages_pending, and DEFER via the N3 guard (gapFillLoop leaves
    // the journal "for a later resolve after the drain"). Boot ran long before the run and there is
    // no re-claim, so ONLY the drainer's retire can re-fire that resolve. Without it the terminal
    // strands non-terminal until a restart. This asserts the drainer re-resolves at the retire point.
    //
    // Determinism: drainRun BLOCKS on a gate so no retire happens until the test releases it. While
    // blocked, `retired` is false, so listPendingTerminals is empty and the boot resolve
    // (resolveBootTerminals, awaited in run()) provably no-ops — it cannot be the source of the
    // resolve. The single-flight guard holds the heartbeat drains off while the one boot drain blocks.
    let release!: () => void;
    const gate = new Promise<void>((r) => (release = r));
    let retired = false;
    const reports: string[] = [];
    let r1Resolved = false;
    let retiredTerminal = 0;
    const outbox = {
      uncleanRuns: () => [],
      isDisabled: () => false,
      // Empty until the drain retires the segments — mirroring a terminal journaled AFTER boot and
      // only re-resolvable once the N3 undrained-messages guard would let it through.
      listPendingTerminals: () => (retired ? [{ run_id: "r1", claim_generation: 5 }] : []),
      runsWithPending: () => (retired ? [] : ["r1"]),
      depthFor: (id: string) => depth(id),
      drainRun: async () => {
        await gate; // hold the ONE (boot) drain open so boot-resolve sees an empty pending list
        retired = true;
        return { retired: true, staleRetired: 0 };
      },
      readTerminalJournal: async (_id: string, gen: number) => ({
        blocked: false,
        body: { status: "completed" },
        messagesThroughSeq: 0,
        claim_generation: gen,
      }),
      retireTerminal: async () => {
        retiredTerminal += 1;
      },
      // PRD #1539: not held here — the drain resolves normally.
      isTerminalResolveHeld: () => false,
      noteHeldSkip: () => {},
    } as unknown as Outbox;

    const client = {
      ...idleClient(),
      hasFeature: () => false, // no terminal_fence → the send carries no messages_through_seq
      reportState: async (runId: string) => {
        reports.push(runId);
        if (runId === "r1") r1Resolved = true;
        return { applied: true, status: "completed" };
      },
    } as unknown as WorkerClient;

    const controller = new AbortController();
    const worker = new Worker(
      fakeConfig({ gapFillMax: 1000, outboxTerminalMaxBytes: 1 << 20 } as unknown as Partial<Config>),
      client,
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
    try {
      // Boot resolve has run against an empty pending list: it is NOT the source of any resolve.
      await sleep(40);
      assert.deepStrictEqual(reports, [], "the boot resolve did not resolve the terminal (nothing pending yet)");
      // Let the drain retire the segments; the drainer must now re-resolve the pending terminal.
      release();
      await pollUntil(() => r1Resolved, 2000, "the drainer re-resolves the run's pending terminal after retire");
    } finally {
      release(); // idempotent: never leave the drain gate blocking on a failure path (fail cleanly, no hang)
      controller.abort();
      await done;
    }
    assert.ok(r1Resolved, "the post-drain resolve sent the terminal for r1");
    assert.ok(retiredTerminal >= 1, "the landed terminal (200) retired the journal — it did not strand");
  });

  it("PRD #1539 (8): a HELD terminal is NOT resolved by the drain, and the skip is recorded", async () => {
    // The live run's permanent-failure hook holds this generation's resolve while it aborts + reaps
    // between the durable install and its own send. The drainer must SKIP the held terminal (sending
    // here would race the hook's own resolve) and RECORD the skip, so the hook's release can re-drive
    // one resolve if its own send failed. Removing the drainer's hold check (mutation M-b) resolves
    // the held terminal (a reportState for r1) and never records the skip — reddening both asserts.
    // Gate the ONE (boot) drain so the boot resolve — which deliberately IGNORES the hold — runs
    // against an empty pending list first (retired=false). Only after release does the drain retire,
    // reach resolveRunTerminal, and hit the hold. This isolates the drainer's hold check.
    let release!: () => void;
    const gate = new Promise<void>((r) => (release = r));
    let retired = false;
    const reports: string[] = [];
    let skipRecorded = 0;
    const outbox = {
      uncleanRuns: () => [],
      isDisabled: () => false,
      listPendingTerminals: () => (retired ? [{ run_id: "r1", claim_generation: 5 }] : []),
      runsWithPending: () => (retired ? [] : ["r1"]),
      depthFor: (id: string) => depth(id),
      drainRun: async () => {
        await gate;
        retired = true;
        return { retired: true, staleRetired: 0 };
      },
      readTerminalJournal: async (_id: string, gen: number) => ({
        blocked: false,
        body: { status: "failed" },
        messagesThroughSeq: 0,
        claim_generation: gen,
      }),
      retireTerminal: async () => {},
      // The hook holds r1/gen5's resolve, so the drain must skip it and record the skip.
      isTerminalResolveHeld: (_id: string, gen: number) => gen === 5,
      noteHeldSkip: () => {
        skipRecorded += 1;
      },
    } as unknown as Outbox;

    const client = {
      ...idleClient(),
      hasFeature: () => false,
      reportState: async (runId: string) => {
        reports.push(runId);
        return { applied: true, status: "failed" };
      },
    } as unknown as WorkerClient;

    const controller = new AbortController();
    const worker = new Worker(
      fakeConfig({ gapFillMax: 1000, outboxTerminalMaxBytes: 1 << 20 } as unknown as Partial<Config>),
      client,
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
    try {
      // Boot resolve has run against an empty pending list (nothing pending yet).
      await sleep(40);
      assert.deepStrictEqual(reports, [], "the boot resolve did not resolve the terminal (nothing pending yet)");
      release(); // let the drain retire, reach resolveRunTerminal, and hit the hold
      await pollUntil(() => skipRecorded > 0, 2000, "the drain records a skip for the held terminal");
    } finally {
      release();
      controller.abort();
      await done;
    }
    assert.equal(skipRecorded, 1, "the held terminal's skip was recorded exactly once");
    assert.deepStrictEqual(reports, [], "the held terminal was NOT resolved by the drain");
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
      // PRD #1391 Run B M4: the boot terminal resolve reads these; these drainer tests carry no
      // pending terminal, so the store is enabled and lists nothing (resolveBootTerminals no-ops).
      isDisabled: () => false,
      listPendingTerminals: () => [],
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
      // PRD #1391 Run B M4: the boot terminal resolve reads these; these drainer tests carry no
      // pending terminal, so the store is enabled and lists nothing (resolveBootTerminals no-ops).
      isDisabled: () => false,
      listPendingTerminals: () => [],
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
