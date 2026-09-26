import { afterEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import fsp from "node:fs/promises";
import os from "node:os";
import path from "node:path";

import { ActiveRunRegistry } from "../src/active-run-registry.js";
import { Worker } from "../src/worker.js";
import type { Config } from "../src/config.js";
import type { WorkerClient } from "../src/client.js";
import type { RunRunner } from "../src/runner.js";
import type { ChatRunner } from "../src/chat-runner.js";
import type { JudgeRunner } from "../src/judge-runner.js";
import type { ReviewRunner } from "../src/review-runner.js";
import type { ActiveSnapshot } from "../src/protocol.js";
import { StubExecutor, type Executor, type RunContext } from "../src/executor.js";
import { Outbox, type RawWriteSeam } from "../src/outbox.js";
import { nullLogger } from "./helpers.js";
import { api, fakeGitlab, gitlabClaim, installHarness, runner, simulateCommittedWork } from "./runner-harness.js";

// M1 fault probes. The paused write is a synthetic reachable cut after the executor
// returned and before journal installation. It does not prove the historical #1730 path.
installHarness();

const roots: string[] = [];
afterEach(async () => {
  for (const root of roots.splice(0)) await fsp.rm(root, { recursive: true, force: true });
});

function makeOutbox(root: string, rawWrite?: RawWriteSeam): Outbox {
  return new Outbox({
    root,
    log: nullLogger(),
    runMaxBytes: 64 * 1024 * 1024,
    maxBytes: 512 * 1024 * 1024,
    retentionMs: 7 * 86_400_000,
    rawWrite,
  });
}

async function terminalFiles(root: string, runId: string): Promise<string[]> {
  const names = await fsp.readdir(path.join(root, runId)).catch((err: NodeJS.ErrnoException) => {
    if (err.code === "ENOENT") return [];
    throw err;
  });
  return names.filter((name) => /^terminal-[0-9]+[.]json$/.test(name)).sort();
}

async function registerAfterRestart(outbox: Outbox, withRegistry = true): Promise<ActiveSnapshot | undefined> {
  let sent: ActiveSnapshot | undefined;
  const registry = new ActiveRunRegistry(() => outbox.listPendingTerminals(), () => 0);
  const client = {
    register: async (_name: string, _template?: string, _max?: number, _caps?: string[],
      _protocol?: string[], snapshot?: ActiveSnapshot) => { sent = snapshot; return {}; },
  } as unknown as WorkerClient;
  const worker = new Worker(
    { workerName: "restart-probe", workerTemplate: "base", maxConcurrentRuns: 1, pollIntervalMs: 1 } as Config,
    client,
    {} as RunRunner,
    {} as ChatRunner,
    {} as JudgeRunner,
    {} as ReviewRunner,
    nullLogger(),
    () => ({ ok: true, missing: [] }),
    outbox,
    new Map(),
    withRegistry ? registry : undefined,
  );
  await (worker as unknown as { registerWithRetry(signal: AbortSignal): Promise<void> })
    .registerWithRetry(new AbortController().signal);
  return sent;
}

describe("issue #1742 M1 restart fault probes", () => {
  it("has no terminal to reload at a post-executor, pre-journal cut", async () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), "issue-1742-cut-"));
    roots.push(dir);
    const root = path.join(dir, "outbox");
    let enteredWrite!: () => void;
    const entered = new Promise<void>((resolve) => { enteredWrite = resolve; });
    let releaseWrite!: () => void;
    const released = new Promise<void>((resolve) => { releaseWrite = resolve; });
    let executorReturned = false;
    const rawWrite: RawWriteSeam = async (write, ctx) => {
      if (ctx.kind === "terminal") {
        enteredWrite();
        await released;
      }
      await write();
    };
    const outbox = makeOutbox(root, rawWrite);
    await outbox.init();
    const claim = gitlabClaim(1742, { claim_generation: 3 });
    simulateCommittedWork();
    const stub = new StubExecutor(nullLogger());
    const executor: Executor = {
      run: async (ctx: RunContext) => {
        const result = await stub.run(ctx);
        executorReturned = true;
        return result;
      },
    };
    const { gitlab } = fakeGitlab();
    api.failStateWhen(claim.run_id, (body) => body.status === "completed", { httpStatus: 409, runStatus: "running" });
    const execution = runner(executor, gitlab, undefined, {
      outbox,
      outboxTerminalMaxBytes: 1 << 20,
      gapFillMax: 100,
    }).execute(claim);

    try {
      await Promise.race([
        entered,
        new Promise<void>((_, reject) => setTimeout(() => reject(new Error("terminal write seam was not reached")), 5_000)),
      ]);
      assert.equal(executorReturned, true, "the real executor returned before the terminal write");
      const disk = await terminalFiles(root, claim.run_id);
      const restarted = makeOutbox(root);
      await restarted.init();
      const loaded = restarted.listPendingTerminals();
      assert.deepEqual(disk, [], "the paused write has installed no journal");
      assert.equal(
        api.states.some((state) => state.runId === claim.run_id && (state.body.status === "completed" || state.body.status === "failed")),
        false,
        "the terminal report has not been sent at the cut",
      );
      assert.equal(loaded.length, disk.length, "restart loaded count matches the disk inventory");
      const snapshot = await registerAfterRestart(restarted);
      assert.equal(snapshot, undefined, "without a loaded journal Worker sends no register snapshot");
    } finally {
      releaseWrite();
      await execution;
    }
  });

  it("reloads a durable terminal and projects the loaded set into restart snapshots", async () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), "issue-1742-loaded-"));
    roots.push(dir);
    const root = path.join(dir, "outbox");
    const outbox = makeOutbox(root);
    await outbox.init();
    const claim = gitlabClaim(1743, { claim_generation: 4 });
    simulateCommittedWork();
    const { gitlab } = fakeGitlab();
    api.failStateWhen(claim.run_id, (body) => body.status === "completed", { httpStatus: 409, runStatus: "running" });
    await runner(new StubExecutor(nullLogger()), gitlab, undefined, {
      outbox,
      outboxTerminalMaxBytes: 1 << 20,
      gapFillMax: 100,
    }).execute(claim);

    const disk = await terminalFiles(root, claim.run_id);
    assert.deepEqual(disk, ["terminal-4.json"], "one installed journal remains after the refused report");
    const restarted = makeOutbox(root);
    await restarted.init();
    const loaded = restarted.listPendingTerminals();
    assert.equal(loaded.length, disk.length, "restart loaded count matches the disk inventory");
    assert.deepEqual(loaded.map(({ run_id, claim_generation }) => [run_id, claim_generation]), [[claim.run_id, 4]]);
    assert.equal((await restarted.readTerminalJournal(claim.run_id, 4))?.body.status, "completed");

    const registry = new ActiveRunRegistry(() => restarted.listPendingTerminals(), () => 0);
    const register = await registerAfterRestart(restarted);
    assert.ok(register, "Worker carries a snapshot when the journal loaded");
    assert.deepEqual(register.active, [], "register sends an empty subset even when a journal was loaded");
    assert.equal(register.pending_overflow, true, "register leases the loaded pending set at cap zero");
    const heartbeat = registry.build();
    assert.deepEqual(heartbeat.active, [], "cap zero cannot list a pending entry");
    assert.equal(heartbeat.pending_overflow, true);
    assert.equal(registry.claimsPausedByPendingOverflow(), true);
    assert.equal(await registerAfterRestart(restarted, false), undefined,
      "an injected missing active registry loses register protection despite a loaded journal");
  });

  it("keeps a physical journal visible when restart authentication rejects it", async () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), "issue-1742-unloaded-"));
    roots.push(dir);
    const root = path.join(dir, "outbox");
    const outbox = makeOutbox(root);
    await outbox.init();
    const runId = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa";
    const installed = await outbox.journalTerminal(runId, 5, "running", 0, { status: "completed" });
    assert.equal(installed.journaled, true);
    const file = path.join(root, runId, "terminal-5.json");
    assert.deepEqual(await terminalFiles(root, runId), ["terminal-5.json"]);
    const parsed = JSON.parse(await fsp.readFile(file, "utf8")) as Record<string, unknown>;
    assert.equal(typeof parsed.mac, "string", "precondition: installed journal carries a MAC");
    parsed.mac = "0".repeat(64);
    await fsp.writeFile(file, JSON.stringify(parsed), { mode: 0o600 });
    const restarted = makeOutbox(root);
    await restarted.init();
    assert.deepEqual(await terminalFiles(root, runId), ["terminal-5.json"], "physical file survives the restart fault");
    assert.deepEqual(restarted.listPendingTerminals(), [], "corrupt journal was not authenticated or loaded");
    assert.equal(await registerAfterRestart(restarted), undefined, "unloaded journal gives Register no snapshot");
  });
});
