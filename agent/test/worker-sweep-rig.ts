import fs from "node:fs";
import os from "node:os";
import path from "node:path";

import { Worker } from "../src/worker.js";
import { Outbox } from "../src/outbox.js";
import type { ActiveRunRegistry } from "../src/active-run-registry.js";
import type { Config } from "../src/config.js";
import type { WorkerClient } from "../src/client.js";
import type { RunRunner } from "../src/runner.js";
import type { ChatRunner } from "../src/chat-runner.js";
import type { JudgeRunner } from "../src/judge-runner.js";
import type { ReviewRunner } from "../src/review-runner.js";
import type { ChatClaimResponse, ClaimResponse, OutboxHeartbeatEntry, StateAck, StateRequest } from "../src/protocol.js";
import { nullLogger } from "./helpers.js";
import { sleep } from "../src/util.js";

// Issue #1512: shared rig for the heartbeat terminal-sweep tests. A real Outbox on a tmp dir, a
// programmable client, and a real Worker over them. Not a test file (no .test.ts suffix).

export interface SweepRig {
  outbox: Outbox;
  root: string;
}

export async function mkSweepOutbox(): Promise<SweepRig> {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "worker-sweep-"));
  const root = path.join(dir, "outbox");
  const outbox = new Outbox({
    root,
    log: nullLogger(),
    runMaxBytes: 64 * 1024 * 1024,
    maxBytes: 512 * 1024 * 1024,
    retentionMs: 7 * 86_400_000,
  });
  await outbox.init();
  return { outbox, root: dir };
}

export interface SweepClientHooks {
  heartbeat?: (entries: OutboxHeartbeatEntry[] | undefined) => Promise<void>;
  reportState?: (runId: string, body: StateRequest) => Promise<StateAck>;
  postMessages?: (runId: string) => Promise<void>;
}

export function sweepClient(hooks: SweepClientHooks = {}): WorkerClient {
  return {
    register: async () => ({}),
    heartbeat: async (_sample: unknown, entries?: OutboxHeartbeatEntry[]) => hooks.heartbeat?.(entries),
    reportState:
      hooks.reportState ?? (async () => ({ applied: true, status: "completed" }) as StateAck),
    claimRun: async (): Promise<ClaimResponse | null> => null,
    claimChat: async (): Promise<ChatClaimResponse | null> => null,
    hasFeature: () => false,
    getMessageGaps: async () => ({ gaps: [] }),
    postMessages: async (runId: string) => hooks.postMessages?.(runId),
  } as unknown as WorkerClient;
}

function sweepConfig(): Config {
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
    gapFillMax: 100,
    outboxTerminalMaxBytes: 1 << 20,
  } as unknown as Config;
}

/** Start a real Worker over `outbox`; `stop()` aborts and awaits it. */
export function startSweepWorker(opts: {
  outbox: Outbox;
  client: WorkerClient;
  activeRuns?: ActiveRunRegistry;
  runner?: Partial<RunRunner>;
}): { stop: () => Promise<void> } {
  const controller = new AbortController();
  const runner = {
    resumePendingRecoveries: async () => {},
    execute: async () => {},
    observeSettlementTerminalAck: async () => {},
    ...opts.runner,
  } as unknown as RunRunner;
  const worker = new Worker(
    sweepConfig(),
    opts.client,
    runner,
    { execute: async () => {} } as unknown as ChatRunner,
    { execute: async () => {} } as unknown as JudgeRunner,
    { execute: async () => {} } as unknown as ReviewRunner,
    nullLogger(),
    () => ({ ok: true, missing: [] }),
    opts.outbox,
    new Map(),
    opts.activeRuns,
  );
  const done = worker.run(controller.signal);
  return {
    stop: async () => {
      controller.abort();
      await done;
    },
  };
}

export async function pollUntil(pred: () => boolean, ms: number, label: string): Promise<void> {
  const deadline = Date.now() + ms;
  while (Date.now() < deadline) {
    if (pred()) return;
    await sleep(5);
  }
  throw new Error(`timed out waiting for: ${label}`);
}
