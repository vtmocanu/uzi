import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { Worker } from "../src/worker.js";
import type { Config } from "../src/config.js";
import type { WorkerClient } from "../src/client.js";
import type { RunRunner } from "../src/runner.js";
import type { ChatRunner } from "../src/chat-runner.js";
import type { ClaimResponse, ChatClaimResponse } from "../src/protocol.js";
import { recordingLogger } from "./helpers.js";

// The worker runs the RUN lane and the CHAT lane as two independent, concurrent
// claim loops (PRD #39 Decision 4). This proves they actually run at the same time
// (not one-after-the-other) while sharing the Logger and WorkerClient — the
// concurrency the audit signs off on. Scrubbing correctness itself is covered by
// the log/redact suites; here the point is safe concurrent use of the collaborators.

const tick = (ms = 2): Promise<void> => new Promise((r) => setTimeout(r, ms));

function fakeConfig(over: Partial<Config> = {}): Config {
  return {
    workerName: "w1",
    workerTemplate: "base",
    pollIntervalMs: 1,
    heartbeatIntervalMs: 5,
    chatPollMs: 1,
    chatSessions: 1,
    ...over,
  } as unknown as Config;
}

describe("Worker — concurrent run + chat lanes (Decision 4)", () => {
  it("executes a run and a chat session concurrently, sharing logger + client", async () => {
    const controller = new AbortController();
    const events: string[] = [];
    let ranRun = false;
    let ranChat = false;

    // The run's execute parks on a gate the chat's execute releases: if the two lanes
    // were sequential (the run fully before the chat), the run would deadlock here.
    let releaseRun!: () => void;
    const runGate = new Promise<void>((r) => (releaseRun = r));

    let gaveRun = false;
    let gaveChat = false;
    const client = {
      register: async () => ({}),
      heartbeat: async () => {},
      claimRun: async (): Promise<ClaimResponse | null> => {
        if (gaveRun) return null;
        gaveRun = true;
        return { run_id: "run-1" } as unknown as ClaimResponse;
      },
      claimChat: async (): Promise<ChatClaimResponse | null> => {
        if (gaveChat) return null;
        gaveChat = true;
        return { run_id: "chat-1" } as unknown as ChatClaimResponse;
      },
    } as unknown as WorkerClient;

    const { logger } = recordingLogger(); // shared across both lanes
    const runRunner = {
      execute: async () => {
        events.push("run:start");
        logger.addSecret("run-secret-000000"); // concurrent shared-registry mutation
        await runGate; // wait for the chat lane to prove it is running
        ranRun = true;
        events.push("run:end");
      },
    } as unknown as RunRunner;
    const chatRunner = {
      execute: async () => {
        events.push("chat:start");
        logger.addSecret("chat-secret-000000");
        ranChat = true;
        releaseRun();
        events.push("chat:end");
      },
    } as unknown as ChatRunner;

    const worker = new Worker(fakeConfig(), client, runRunner, chatRunner, logger);
    const done = worker.run(controller.signal);
    for (let i = 0; i < 500 && !(ranRun && ranChat); i++) await tick();
    controller.abort();
    await done;

    assert.ok(ranRun && ranChat, "both lanes executed");
    assert.ok(
      events.indexOf("chat:start") < events.indexOf("run:end"),
      "the chat session started before the run finished — the lanes ran concurrently",
    );
  });

  it("honors WORKER_CHAT_SESSIONS as the concurrent chat ceiling", async () => {
    const controller = new AbortController();
    let peak = 0;
    let active = 0;
    let claimed = 0;

    const client = {
      register: async () => ({}),
      heartbeat: async () => {},
      claimRun: async () => null, // run lane idle
      claimChat: async (): Promise<ChatClaimResponse | null> => {
        claimed++;
        return { run_id: `chat-${claimed}` } as unknown as ChatClaimResponse; // always more available
      },
    } as unknown as WorkerClient;

    // Each chat blocks until the worker shuts down (as a real parked chat would),
    // so the slots stay full and the ceiling is observable.
    const chatRunner = {
      execute: async (_claim: ChatClaimResponse, signal?: AbortSignal) => {
        active++;
        peak = Math.max(peak, active);
        await new Promise<void>((r) => {
          if (signal?.aborted) return r();
          signal?.addEventListener("abort", () => r(), { once: true });
        });
        active--;
      },
    } as unknown as ChatRunner;
    const runRunner = { execute: async () => {} } as unknown as RunRunner;

    const worker = new Worker(fakeConfig({ chatSessions: 2 }), client, runRunner, chatRunner, recordingLogger().logger);
    const done = worker.run(controller.signal);
    for (let i = 0; i < 300 && active < 2; i++) await tick();
    // Give the loop extra ticks to (wrongly) over-fill if the ceiling were not honored.
    for (let i = 0; i < 20; i++) await tick();
    assert.strictEqual(peak, 2, "never more than WORKER_CHAT_SESSIONS chats run at once");
    controller.abort(); // shutdown aborts the parked chats → clean drain
    await done;
  });
});
