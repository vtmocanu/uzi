import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";

import { Outbox } from "../src/outbox.js";
import { ActiveRunRegistry } from "../src/active-run-registry.js";
import { Worker } from "../src/worker.js";
import type { Config } from "../src/config.js";
import type { WorkerClient } from "../src/client.js";
import type { RunRunner } from "../src/runner.js";
import type { ChatRunner } from "../src/chat-runner.js";
import type { JudgeRunner } from "../src/judge-runner.js";
import type { ReviewRunner } from "../src/review-runner.js";
import type { ActiveSnapshot } from "../src/protocol.js";
import { recordingLogger } from "./helpers.js";

it("reports physical terminals separately from authenticated terminals without following links", async () => {
  const parent = await fs.mkdtemp(path.join(os.tmpdir(), "outbox-logging-"));
  const root = path.join(parent, "outbox");
  const runId = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa";
  const make = (log: ReturnType<typeof recordingLogger>["logger"]) => new Outbox({
    root, log, runMaxBytes: 64 * 1024 * 1024, maxBytes: 512 * 1024 * 1024,
    retentionMs: 86_400_000,
  });
  try {
    const original = make(recordingLogger().logger);
    await original.init();
    assert.equal((await original.journalTerminal(runId, 5, "running", 0, { status: "completed" })).journaled, true);
    assert.equal((await original.journalTerminal(runId, 6, "running", 0, { status: "completed" })).journaled, true);
    const file = path.join(root, runId, "terminal-5.json");
    const body = JSON.parse(await fs.readFile(file, "utf8")) as Record<string, unknown>;
    body.mac = "0".repeat(64);
    await fs.writeFile(file, JSON.stringify(body));
    const symlinkPath = path.join(root, runId, "terminal-6.json");
    const validTarget = path.join(parent, "valid-terminal-6.json");
    await fs.rename(symlinkPath, validTarget);
    await fs.symlink(validTarget, symlinkPath);

    const capture = recordingLogger();
    const restarted = make(capture.logger);
    await restarted.init();
    const records = capture.lines as Array<Record<string, unknown>>;
    const inventory = records.find((line) => line.msg === "outbox terminal journal inventory");
    const load = records.find((line) => line.msg === "outbox terminal journal load");
    assert.equal(inventory?.physical_files, 1);
    assert.equal(inventory?.symlink_entries, 1);
    assert.equal(load?.authenticated_pending, 0);
    assert.equal(load?.rejected_files, 2);
    assert.deepEqual(restarted.listPendingTerminals(), []);
    assert.equal(await fs.readlink(symlinkPath), validTarget);
    assert.equal(JSON.stringify([inventory, load]).includes(runId), false);
  } finally {
    await fs.rm(parent, { recursive: true, force: true });
  }
});

it("logs a bounded authenticated register summary with the snapshot epoch", async () => {
  const parent = await fs.mkdtemp(path.join(os.tmpdir(), "register-logging-"));
  const root = path.join(parent, "outbox");
  try {
    const capture = recordingLogger();
    const outbox = new Outbox({ root, log: capture.logger, runMaxBytes: 64 * 1024 * 1024,
      maxBytes: 512 * 1024 * 1024, retentionMs: 86_400_000 });
    await outbox.init();
    for (let generation = 1; generation <= 10; generation++) {
      const runId = `${String(generation).padStart(8, "0")}-aaaa-aaaa-aaaa-aaaaaaaaaaaa`;
      assert.equal((await outbox.journalTerminal(runId, generation, "running", 0,
        { status: "completed" })).journaled, true);
    }
    const registry = new ActiveRunRegistry(() => outbox.listPendingTerminals(), () => 0);
    let sent: ActiveSnapshot | undefined;
    const client = { register: async (_name: string, _template?: string, _max?: number, _caps?: string[],
      _protocol?: string[], snapshot?: ActiveSnapshot) => { sent = snapshot; return {}; } } as unknown as WorkerClient;
    const worker = new Worker(
      { workerName: "logging-probe", workerTemplate: "base", maxConcurrentRuns: 1,
        pollIntervalMs: 1 } as Config,
      client, {} as RunRunner, {} as ChatRunner, {} as JudgeRunner, {} as ReviewRunner,
      capture.logger, () => ({ ok: true, missing: [] }), outbox, new Map(), registry,
    );
    await (worker as unknown as { registerWithRetry(signal: AbortSignal): Promise<void> })
      .registerWithRetry(new AbortController().signal);
    const summary = (capture.lines as Array<Record<string, unknown>>)
      .find((line) => line.msg === "register terminal snapshot");
    assert.equal(summary?.authenticated_pending, 10);
    assert.deepEqual(summary?.claim_generations_sample, [1, 2, 3, 4, 5, 6, 7, 8]);
    assert.equal(summary?.claim_generations_omitted, 2);
    assert.equal(summary?.snapshot_epoch, sent?.snapshot_epoch);
    assert.equal(summary?.pending_overflow, true);
    assert.equal(JSON.stringify(summary).includes("aaaaaaaa"), false);
  } finally {
    await fs.rm(parent, { recursive: true, force: true });
  }
});
