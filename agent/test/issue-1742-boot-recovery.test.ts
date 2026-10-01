import { afterEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import fsp from "node:fs/promises";
import os from "node:os";
import path from "node:path";

import { Worker } from "../src/worker.js";
import { Outbox } from "../src/outbox.js";
import { ActiveRunRegistry } from "../src/active-run-registry.js";
import type { Config } from "../src/config.js";
import type { WorkerClient } from "../src/client.js";
import type { RunRunner } from "../src/runner.js";
import type { ChatRunner } from "../src/chat-runner.js";
import type { JudgeRunner } from "../src/judge-runner.js";
import type { ReviewRunner } from "../src/review-runner.js";
import type { ChatClaimResponse, ClaimResponse } from "../src/protocol.js";
import type { RecoveryRecord } from "../src/recovery.js";
import { nullLogger } from "./helpers.js";
import { sleep } from "../src/util.js";

// issue #1742 D4(a) — the worker's boot ordering: the previous process's recovery records are
// snapshotted BEFORE register, and that exact snapshot is handed to the restart sweep.

const noJudge = { execute: async () => {} } as unknown as JudgeRunner;
const noReview = { execute: async () => {} } as unknown as ReviewRunner;
const idleChat = { execute: async () => {} } as unknown as ChatRunner;

const tmpRoots: string[] = [];
afterEach(async () => {
  for (const r of tmpRoots.splice(0)) await fsp.rm(r, { recursive: true, force: true }).catch(() => undefined);
});

describe("Worker boot recovery ordering (issue #1742 D4a)", () => {
  it("snapshots the recovery records before register and passes that same snapshot to the sweep", async () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), "worker-boot-recovery-"));
    tmpRoots.push(dir);
    const outbox = new Outbox({
      root: path.join(dir, "outbox"),
      log: nullLogger(),
      runMaxBytes: 64 * 1024 * 1024,
      maxBytes: 512 * 1024 * 1024,
      retentionMs: 7 * 86_400_000,
    });
    await outbox.init();

    const order: string[] = [];
    const snapshot = [{ runId: "r1", captureId: "c1" }] as unknown as RecoveryRecord[];
    let swept: RecoveryRecord[] | undefined;
    const runner = {
      snapshotBootRecoveries: async () => {
        order.push("snapshot");
        return snapshot;
      },
      resumePendingRecoveries: async (_signal: AbortSignal | undefined, snap?: RecoveryRecord[]) => {
        order.push("sweep");
        swept = snap;
      },
      execute: async () => {},
    } as unknown as RunRunner;
    const client = {
      register: async () => {
        order.push("register");
        return {};
      },
      heartbeat: async () => {},
      reportState: async () => ({ applied: true, status: "completed" }),
      claimRun: async (): Promise<ClaimResponse | null> => null,
      claimChat: async (): Promise<ChatClaimResponse | null> => null,
      hasFeature: () => false,
      getMessageGaps: async () => ({ gaps: [] }),
      postMessages: async () => {},
    } as unknown as WorkerClient;
    const registry = new ActiveRunRegistry(() => outbox.listPendingTerminals(), () => 32);
    const worker = new Worker(
      {
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
      } as unknown as Config,
      client,
      runner,
      idleChat,
      noJudge,
      noReview,
      nullLogger(),
      () => ({ ok: true, missing: [] }),
      outbox,
      new Map(),
      registry,
    );
    const controller = new AbortController();
    const done = worker.run(controller.signal);
    try {
      const deadline = Date.now() + 2000;
      while (!order.includes("sweep") && Date.now() < deadline) await sleep(5);
      assert.deepEqual(order.slice(0, 3), ["snapshot", "register", "sweep"]);
      assert.equal(swept, snapshot, "the SAME snapshot object reaches the sweep");
    } finally {
      controller.abort();
      await done.catch(() => undefined);
    }
  });
});
