import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { createHash } from "node:crypto";
import { setImmediate as turn } from "node:timers/promises";
import { RecoveryCoordinator, type RecoveryBundleProducer, type RecoveryArchiveClient } from "../src/recovery.js";
import { RunRunner } from "../src/runner.js";
import { Worker } from "../src/worker.js";
import { MaintenanceBudget } from "../src/maintenance-budget.js";
import type { GitCache } from "../src/git.js";
import type { WorkerClient } from "../src/client.js";
import type { Config } from "../src/config.js";
import type { ChatRunner } from "../src/chat-runner.js";
import type { JudgeRunner } from "../src/judge-runner.js";
import type { ReviewRunner } from "../src/review-runner.js";
import { nullLogger } from "./helpers.js";

function deferred() {
  let resolve!: () => void;
  const promise = new Promise<void>(r => { resolve = r; });
  return { promise, resolve };
}

/** Seed through public capture, then restart with a deferred, owned local cleanup child. */
async function fixture() {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), "maintenance-lifecycle-"));
  const entered = deferred();
  const release = deferred();
  const aborted = deferred();
  let calls = 0;
  let childSignal: AbortSignal | undefined;
  let childBudget: MaintenanceBudget | undefined;
  const forbidden = () => { assert.fail("maintenance must not execute, refresh credentials, or release custody"); };
  const client = {
    reserveRecoveryCapture: async () => ({ capture_id: "archive", state: "preparing" }),
    uploadRecoveryBundle: async (_run: unknown, _capture: unknown, _manifest: unknown, stream: AsyncIterable<unknown>) => {
      for await (const chunk of stream) void chunk;
      return { capture_id: "archive", state: "available", manifest_bound: true };
    },
    releaseRecoveryCustody: forbidden,
    refreshCredential: forbidden,
  } as unknown as RecoveryArchiveClient & WorkerClient;
  const git: RecoveryBundleProducer = {
    fetchDefaultTip: async () => "2".repeat(40),
    produceRecoveryBundle: async (_bare: string, opts: { outPath: string; sourceSha: string }) => {
      const bytes = Buffer.from("fixture bundle");
      await fs.writeFile(opts.outPath, bytes);
      return { bundlePath: opts.outPath, sourceSha: opts.sourceSha, byteSize: bytes.length,
        checksum: createHash("sha256").update(bytes).digest("hex"), chunkCount: 1,
        prerequisiteShas: [], selfContained: true, alreadyPublished: false };
    },
  };
  const options = { recoveryRoot: root, workerToken: "local-worker-fixture", log: nullLogger(), client, git };
  const seed = new RecoveryCoordinator(options);
  const record = await seed.pin({ runId: "run-fixture", sourceSha: "1".repeat(40), kind: "issue", branch: "work" });
  assert.ok(record);
  assert.equal((await seed.captureAndUpload({ record, barePath: root, defaultBranch: "main" })).state, "uploaded");
  git.cleanupArchivedRecoveryPin = async request => {
    calls++;
    assert.ok(request.signal);
    childSignal = request.signal;
    childBudget = MaintenanceBudget.current();
    if (request.signal.aborted) aborted.resolve();
    else request.signal.addEventListener("abort", aborted.resolve, { once: true });
    entered.resolve();
    await release.promise; // Abort requests cancellation; it does not reap this owned child.
    return { state: "retained", reason: "ineligible" };
  };
  const coordinator = new RecoveryCoordinator(options);
  const runner = new RunRunner(client, git as GitCache, forbidden, nullLogger(), 20, undefined, { recovery: coordinator });
  return { root, entered, release, aborted, coordinator, runner, client, forbidden,
    calls: () => calls, signal: () => childSignal, budget: () => childBudget,
    cleanup: async () => { release.resolve(); await coordinator.shutdown(); await fs.rm(root, { recursive: true, force: true }); } };
}

for (const mode of ["boot", "live"] as const) {
  it(`${mode} shutdown aborts an actual pass and drains its owned child before settling`, async () => {
    const f = await fixture();
    try {
      const pass = mode === "boot" ? f.coordinator.resumePending() :
        f.coordinator.resumeLive({ authenticatedAtMs: Date.now(), isExecuting: () => false });
      await f.entered.promise;
      let settled = false;
      const drain = f.coordinator.shutdown().then(() => { settled = true; });
      await f.aborted.promise;
      await turn();
      assert.equal(settled, false);
      assert.equal(f.signal()?.aborted, true);
      await f.coordinator.resumePending();
      await f.coordinator.resumeLive({ authenticatedAtMs: Date.now(), isExecuting: () => false });
      assert.equal(f.calls(), 1);
      f.release.resolve();
      await Promise.all([pass, drain]);
      await f.coordinator.shutdown();
      await f.coordinator.resumePending();
      assert.equal(f.calls(), 1);
    } finally { await f.cleanup(); }
  });

  for (const source of ["caller", "runner"] as const) {
    it(`${mode} runner wrapper forwards ${source} shutdown while retaining ambient budget and ownership`, async () => {
      const f = await fixture();
      const caller = new AbortController();
      const budget = new MaintenanceBudget();
      try {
        const pass = MaintenanceBudget.run(budget, () => mode === "boot" ?
          f.runner.resumePendingRecoveries(caller.signal) : f.runner.resumeLiveRecoveries(Date.now(), caller.signal));
        await f.entered.promise;
        assert.equal(f.budget(), budget);
        if (source === "caller") caller.abort();
        else f.runner.shutdown();
        await f.aborted.promise;
        let settled = false;
        const drain = f.runner.shutdownRecoveries().then(() => { settled = true; });
        await turn();
        assert.equal(settled, false);
        f.release.resolve();
        await Promise.all([pass, drain]);
        assert.equal(f.calls(), 1);
      } finally { budget.dispose(); await f.cleanup(); }
    });
  }
}

for (const mode of ["boot", "live"] as const) {
  it(`close refuses queued ${mode} dispatch and leaves independent coordinators open`, async () => {
  const f = await fixture();
  const other = await fixture();
  try {
    const queued = mode === "boot" ? f.coordinator.resumePending() :
      f.coordinator.resumeLive({ authenticatedAtMs: Date.now(), isExecuting: () => false });
    await f.coordinator.shutdown();
    await queued;
    assert.equal(f.calls(), 0);
    await f.coordinator.resumePending();
    await f.coordinator.resumeLive({ authenticatedAtMs: Date.now(), isExecuting: () => false });
    assert.equal(f.calls(), 0);
    const independent = other.coordinator.resumePending();
    await other.entered.promise;
    assert.equal(other.calls(), 1);
    assert.equal(other.signal()?.aborted, false);
    other.release.resolve();
    await independent;
  } finally { await Promise.all([f.cleanup(), other.cleanup()]); }
  });
}

it("retained source admission and open rechecks refuse runner shutdown", async () => {
  const f = await fixture();
  try {
    await f.runner.withRetainedSourceBoundary("retained", [], async ctx => {
      assert.equal(ctx.open(), true);
      f.runner.shutdown();
      assert.equal(ctx.open(), false);
      assert.equal(await ctx.quiesce([]), false);
    });
    assert.deepEqual(await f.runner.withRetainedSourceBoundary("new", [], async () => f.forbidden()), { entered: false });
    assert.equal(f.runner.isExecuting("new"), false);
  } finally { await f.cleanup(); }
});

for (const mode of ["boot", "live"] as const) {
it(`Worker run joins ${mode} recovery after loop abort without blocking claim admission`, async () => {
  const f = await fixture();
  const claimed = deferred();
  const controller = new AbortController();
  try {
    const bootFinished = deferred();
    if (mode === "live") {
      f.runner.snapshotBootRecoveries = async () => [];
      const resume = f.runner.resumePendingRecoveries.bind(f.runner);
      f.runner.resumePendingRecoveries = async (...args) => { await resume(...args); bootFinished.resolve(); };
    }
    Object.assign(f.client, {
      register: async () => ({}),
      heartbeat: async () => { if (mode === "live") await bootFinished.promise; },
      claimRun: async () => { claimed.resolve(); return null; },
      claimChat: async () => null,
    });
    // Keep Worker integration real while isolating unrelated runner settlement work.
    f.runner.settlePendingPredecessors = async () => {};
    const config = { workerName: "fixture", workerTemplate: "base", pollIntervalMs: 1,
      heartbeatIntervalMs: 60_000, chatPollMs: 1, chatSessions: 0, maxConcurrentRuns: 1,
      dockerWiring: {} } as Config;
    const worker = new Worker(config, f.client, f.runner, {} as ChatRunner,
      { execute: f.forbidden } as unknown as JudgeRunner, { execute: f.forbidden } as unknown as ReviewRunner,
      nullLogger(), () => ({ ok: true, missing: [] }));
    let settled = false;
    const run = worker.run(controller.signal).then(() => { settled = true; });
    await Promise.all([f.entered.promise, claimed.promise]);
    controller.abort();
    await f.aborted.promise;
    await turn();
    assert.equal(settled, false);
    f.release.resolve();
    await run;
    await f.runner.resumePendingRecoveries();
    assert.equal(f.calls(), 1);
  } finally { controller.abort(); await f.cleanup(); }
});
}

for (const first of ["heartbeat", "recovery"] as const) {
it(`Worker abort revokes independent recovery and joins both children (${first} settles first)`, async () => {
  const f = await fixture();
  const heartbeatEntered = deferred();
  const heartbeatRelease = deferred();
  const claimed = deferred();
  const controller = new AbortController();
  let pass: Promise<void> | undefined;
  let run: Promise<void> | undefined;
  try {
    // This pass models a terminal producer: it borrows no worker/caller signal.
    f.runner.resumePendingRecoveries = async () => {};
    f.runner.settlePendingPredecessors = async () => {};
    Object.assign(f.client, {
      register: async () => ({}),
      heartbeat: async () => { heartbeatEntered.resolve(); await heartbeatRelease.promise; },
      claimRun: async () => { claimed.resolve(); return null; },
      claimChat: async () => null,
    });
    const config = { workerName: "fixture", workerTemplate: "base", pollIntervalMs: 1,
      heartbeatIntervalMs: 60_000, chatPollMs: 1, chatSessions: 0, maxConcurrentRuns: 1,
      dockerWiring: {} } as Config;
    const worker = new Worker(config, f.client, f.runner, {} as ChatRunner,
      { execute: f.forbidden } as unknown as JudgeRunner, { execute: f.forbidden } as unknown as ReviewRunner,
      nullLogger(), () => ({ ok: true, missing: [] }));
    let settled = false;
    run = worker.run(controller.signal).finally(() => { settled = true; });
    await Promise.all([heartbeatEntered.promise, claimed.promise]);
    pass = f.coordinator.resumePending();
    await f.entered.promise;
    assert.equal(f.signal()?.aborted, false);
    controller.abort();
    await turn();
    assert.equal(f.signal()?.aborted, true, "worker abort must revoke independent recovery before heartbeat drains");
    assert.equal(settled, false);
    if (first === "heartbeat") heartbeatRelease.resolve();
    else f.release.resolve();
    await turn();
    assert.equal(settled, false, `worker must still join the ${first === "heartbeat" ? "recovery-owned" : "heartbeat"} child`);
    heartbeatRelease.resolve();
    f.release.resolve();
    await Promise.all([run, pass]);
    assert.equal(settled, true);
    assert.equal(f.calls(), 1);
  } finally {
    controller.abort();
    heartbeatRelease.resolve();
    f.release.resolve();
    await Promise.allSettled([run, pass]);
    await f.cleanup();
  }
});
}
