import { test, afterEach } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import path from "node:path";
import { Worker } from "../src/worker.js";
import { WorkerClient } from "../src/client.js";
import { Outbox } from "../src/outbox.js";
import { TerminalRejectionCoordinator } from "../src/terminal-rejections.js";
import { RunDiskLocks } from "../src/run-disk-locks.js";
import { ActiveRunRegistry } from "../src/active-run-registry.js";
import type { Config } from "../src/config.js";
import type { RunRunner } from "../src/runner.js";
import type { ChatRunner } from "../src/chat-runner.js";
import type { JudgeRunner } from "../src/judge-runner.js";
import type { ReviewRunner } from "../src/review-runner.js";
import type { StatsCollector } from "../src/stats.js";
import type { ClaimResponse } from "../src/protocol.js";
import { nullLogger, waitForTestEvent } from "./helpers.js";
import { sleep } from "../src/util.js";
import os from "node:os";
import { realpathSync } from "node:fs";

const run = "11111111-1111-4111-8111-111111111111";
const workerId = "22222222-2222-4222-8222-222222222222";
const hold = "33333333-3333-4333-8333-333333333333";
const originalFetch = globalThis.fetch;
afterEach(() => { globalThis.fetch = originalFetch; });
const scratch = realpathSync(os.tmpdir());
function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((done) => { resolve = done; });
  return { promise, resolve };
}
async function until(predicate: () => boolean | Promise<boolean>) {
  const deadline = Date.now() + 5000;
  while (Date.now() < deadline) {
    if (await predicate()) return;
    await sleep(5);
  }
  assert.fail("worker condition did not complete within five seconds");
}
async function rig() {
  const root = await fs.mkdtemp(path.join(scratch, "terminal-worker-"));
  const outbox = new Outbox({ root: path.join(root, "outbox"), log: nullLogger(), runMaxBytes: 1e6, maxBytes: 1e7, retentionMs: 1 });
  await outbox.init();
  await fs.mkdir(path.join(root, "outbox", run));
  const file = path.join(root, "outbox", run, "terminal-3.json");
  await fs.writeFile(file, JSON.stringify({ mac: "0".repeat(64), body: "untrusted" }));
  const controller = new AbortController();
  const registry = new ActiveRunRegistry(() => outbox.listPendingTerminals(), () => 256);
  return { root, outbox, file, controller, registry };
}
function makeWorker(r: Awaited<ReturnType<typeof rig>>, client: WorkerClient,
  runner: Partial<RunRunner> = {}, coordinator?: TerminalRejectionCoordinator, maxConcurrentRuns = 1) {
  const config = { workerName: "test", workerTemplate: "base", dataDir: r.root,
    pollIntervalMs: 2, heartbeatIntervalMs: 5, chatPollMs: 2, chatSessions: 1,
    maxConcurrentRuns, dockerWiring: {}, httpTimeoutMs: 40,
    outboxTerminalMaxBytes: 1 << 20, gapFillMax: 100 } as Config;
  return new Worker(config, client, {
    resumePendingRecoveries: async () => {},
    settlePendingPredecessors: async () => {},
    execute: async () => {},
    ...runner,
  } as RunRunner, { execute: async () => {} } as unknown as ChatRunner,
  {} as JudgeRunner, {} as ReviewRunner, nullLogger(), () => ({ ok: true, missing: [] }),
  r.outbox, new Map(), r.registry, undefined, undefined, undefined, undefined,
  () => ({ collect: () => undefined }) as unknown as StatsCollector,
  undefined, undefined, undefined, coordinator);
}
function settled() {
  return { run_id: run, worker_id: workerId, generation: 3, exact_holds: [{ id: hold, state: "released" }],
    sibling_holds: [], exact_count: 1, sibling_count: 0, exact_complete: true,
    sibling_complete: true, complete: true, outcome: "settled" };
}

test("heartbeat runs immediately after Register while offered finalize retirement awaits", async () => {
  const r = await rig();
  await r.outbox.journalFinalize(run, 2);
  const retire = deferred<void>();
  const realRetire = r.outbox.retireFinalizeIfEligible.bind(r.outbox);
  let retiring = false;
  r.outbox.retireFinalizeIfEligible = async (...args) => { retiring = true; await retire.promise; return await realRetire(...args); };
  let heartbeats = 0;
  let claims = 0;
  const client = {
    register: async () => ({ worker_id: workerId }),
    heartbeat: async () => { heartbeats++; },
    hasFeature: () => false,
    claimRun: async () => { claims++; return null; },
    claimChat: async () => null,
  } as unknown as WorkerClient;
  const running = makeWorker(r, client, { recoveryInventoryPending: async () => false }).run(r.controller.signal);
  try {
    await until(() => retiring && heartbeats >= 3);
    assert.equal(claims, 0, "post-registration await remains stalled");
    retire.resolve();
    await until(() => claims > 0);
  } finally {
    retire.resolve(); r.controller.abort(); await running;
    await fs.rm(r.root, { recursive: true, force: true });
  }
});

test("stalled POST and GET bodies are deadline bounded while heartbeat remains independent", async () => {
  for (const method of ["POST", "GET"]) {
    const r = await rig();
    let heartbeats = 0;
    let stalls = 0;
    let cancelled = 0;
    let claims = 0;
    let registerBody: Record<string, unknown> | undefined;
    globalThis.fetch = async (url, init) => {
      const route = String(url);
      if (route.endsWith("/register")) {
        registerBody = JSON.parse(String(init?.body));
        return Response.json({ worker_id: workerId, protocol_features: ["terminal_rejection_report"] });
      }
      if (route.endsWith("/heartbeat")) { heartbeats++; return Response.json({}); }
      if (route.includes("/claim")) { claims++; return new Response(null, { status: 204 }); }
      if (init?.method === method) {
        stalls++;
        return new Response(new ReadableStream({ cancel() { cancelled++; } }));
      }
      if (init?.method === "POST") {
        const body = JSON.parse(String(init.body));
        return Response.json({ dispositions: body.rejections.map((x: object) => ({ ...x, disposition: "recorded" })) });
      }
      return Response.json(settled());
    };
    const client = new WorkerClient("http://worker.invalid", "join-token", "test", nullLogger(), { httpTimeoutMs: 40 });
    const coordinator = new TerminalRejectionCoordinator(r.outbox, client, nullLogger(), undefined, undefined, 5, 40);
    const running = makeWorker(r, client, {}, coordinator).run(r.controller.signal);
    try {
      await until(() => stalls >= 1 && cancelled >= 1 && heartbeats >= 4 && claims >= 1);
      assert.equal(registerBody?.active_snapshot, undefined, "bad MAC must not create a register snapshot");
      if (method === "GET") assert.ok(await fs.stat(r.file), "timeout cannot authorize deletion");
    } finally {
      r.controller.abort(); await running;
      await fs.rm(r.root, { recursive: true, force: true });
    }
  }
});

test("default constructor coordinator handles a real outbox restart without pending terminal admission", async (t) => {
  const r = await rig();
  const heartbeat = deferred<void>();
  const cleaned = deferred<number>();
  const cleanup = r.outbox.cleanupRejectedTerminalFiles.bind(r.outbox);
  const lockedOutbox = r.outbox as unknown as {
    withRunLock<T>(runId: string, fn: () => Promise<T>, signal?: AbortSignal): Promise<T>;
  };
  let attempts = 0;
  let forcedBusy = false;
  let custodyGets = 0;
  let firstCount: number | undefined;
  let coordinator: TerminalRejectionCoordinator;
  t.mock.method(r.outbox, "cleanupRejectedTerminalFiles", async (...args: Parameters<typeof cleanup>) => {
    const first = attempts++ === 0;
    const getsBefore = custodyGets;
    const count = first
      ? await lockedOutbox.withRunLock(args[0], () => cleanup(...args))
      : await cleanup(...args);
    if (first) {
      firstCount = count;
      assert.equal(count, 0, "the real outbox lock makes the first cleanup busy");
      assert.ok(await fs.stat(r.file), "diagnostic POST cannot authorize deletion");
      assert.equal(custodyGets, getsBefore, "busy cleanup cannot obtain custody authority");
      forcedBusy = true;
    }
    // Model retry after transient lock contention through the public coalesced wake.
    // Await the whole withRunLock call before waking; keep the default 30s interval.
    // The single real watchdog below bounds all attempts, including further busy results.
    if (count === 0) coordinator.queueReconciliation();
    if (count > 0) {
      assert.ok(custodyGets > getsBefore, "successful cleanup obtained fresh custody");
      cleaned.resolve(count);
    }
    return count;
  });
  let registerBody: Record<string, unknown> | undefined;
  globalThis.fetch = async (url, init) => {
    const route = String(url);
    if (route.endsWith("/register")) {
      registerBody = JSON.parse(String(init?.body));
      return Response.json({ worker_id: workerId, protocol_features: ["terminal_rejection_report"] });
    }
    if (route.endsWith("/heartbeat")) { heartbeat.resolve(); return Response.json({}); }
    if (route.includes("/claim")) return new Response(null, { status: 204 });
    if (init?.method === "POST") {
      const body = JSON.parse(String(init.body));
      return Response.json({ dispositions: body.rejections.map((x: object) => ({ ...x, disposition: "recorded" })) });
    }
    if (route.includes("/terminal-rejection-custody?")) custodyGets++;
    return Response.json(settled());
  };
  const client = new WorkerClient("http://worker.invalid", "join-token", "test", nullLogger());
  const worker = makeWorker(r, client);
  coordinator = (worker as unknown as { terminalRejections: TerminalRejectionCoordinator }).terminalRejections;
  assert.ok(coordinator);
  // Isolate default-constructor wiring from Outbox's cooperative proof clock.
  // The watchdog and AbortSignal HTTP deadlines still use real timers.
  const proofNow = Date.now();
  const clock = t.mock.method(Date, "now", () => proofNow);
  const running = worker.run(r.controller.signal);
  try {
    const [, removed] = await waitForTestEvent(
      Promise.all([heartbeat.promise, cleaned.promise]), "heartbeat and actual rejection cleanup", 5000);
    assert.equal(firstCount, 0);
    assert.equal(forcedBusy, true);
    assert.ok(attempts >= 2);
    assert.equal(removed, 1);
    await assert.rejects(fs.stat(r.file), { code: "ENOENT" });
    assert.equal(registerBody?.active_snapshot, undefined);
    assert.equal(r.registry.size, 0);
  } catch (error) {
    t.diagnostic(`cleanup attempts=${attempts}, firstCount=${firstCount}, forcedBusy=${forcedBusy}, custodyGets=${custodyGets}`);
    throw error;
  } finally {
    r.controller.abort();
    try { await running; } finally { clock.mock.restore(); }
    await fs.rm(r.root, { recursive: true, force: true });
  }
});

test("claim admission holds through late response and synchronous dispatch, releases before execution settles", async () => {
  const r = await rig();
  const claim = deferred<ClaimResponse>();
  const execution = deferred<void>();
  let claimEntered = false;
  let executing = false;
  let posts = 0;
  let gets = 0;
  const client = {
    register: async () => ({ worker_id: workerId }),
    heartbeat: async () => {},
    hasFeature: (feature: string) => feature === "terminal_rejection_report",
    claimRun: async () => { claimEntered = true; return await claim.promise; },
    claimChat: async () => null,
    reportTerminalRejections: async () => {
      // Make the claim enter the coordinator before disposal queues behind it.
      await until(() => claimEntered);
      posts++;
      return undefined;
    },
    getTerminalRejectionCustody: async () => { gets++; return settled(); },
  } as unknown as WorkerClient;
  const locks = new RunDiskLocks();
  const coordinator = new TerminalRejectionCoordinator(r.outbox, client, nullLogger(), locks, () => executing, 2, 20);
  const runner = { execute: () => { executing = true; return execution.promise.finally(() => { executing = false; }); } };
  const running = makeWorker(r, client, runner, coordinator).run(r.controller.signal);
  try {
    await until(() => claimEntered && posts > 0);
    await sleep(50); // Longer than configured claim deadline: unresolved fake claim remains held.
    assert.equal(gets, 0);
    assert.ok(await fs.stat(r.file));
    claim.resolve({ run_id: run, kind: "issue", claim_generation: 4 } as ClaimResponse);
    await until(() => executing);
    // This is a queued public admission attempt. It must enter while execution is still live.
    const release = await coordinator.acquireAdmission();
    assert.equal(executing, true);
    release();
    await sleep(30);
    assert.equal(gets, 0, "admitted same-run execution prevents custody reads/disposal");
    assert.ok(await fs.stat(r.file));
  } finally {
    r.controller.abort(); claim.resolve({ run_id: run, kind: "issue", claim_generation: 4 } as ClaimResponse);
    execution.resolve(); await running; await fs.rm(r.root, { recursive: true, force: true });
  }
});

test("overlapping same-run worker executions keep deletion blocked when the later attempt finishes first", async (t) => {
  const r = await rig();
  // Expose the rejected terminal only after both independent executions hold protection.
  const badCandidate = await fs.readFile(r.file);
  await fs.unlink(r.file);
  const executions = [deferred<void>(), deferred<void>()];
  const deletion = deferred<void>();
  const cleanup = r.outbox.cleanupRejectedTerminalFiles.bind(r.outbox);
  t.mock.method(r.outbox, "cleanupRejectedTerminalFiles", async (...args: Parameters<typeof cleanup>) => {
    const removed = await cleanup(...args);
    if (removed > 0) deletion.resolve();
    return removed;
  });
  let started = 0;
  let claims = 0;
  let posts = 0;
  let gets = 0;
  const client = {
    register: async () => ({ worker_id: workerId }),
    heartbeat: async () => {},
    hasFeature: (feature: string) => feature === "terminal_rejection_report",
    claimRun: async () => ++claims <= 2
      ? { run_id: run, kind: "issue", claim_generation: 4 } as ClaimResponse : null,
    claimChat: async () => null,
    reportTerminalRejections: async () => { await until(() => started === 2); posts++; },
    getTerminalRejectionCustody: async () => { gets++; return settled(); },
  } as unknown as WorkerClient;
  const runner = { execute: () => executions[started++]!.promise };
  const worker = makeWorker(r, client, runner, undefined, 2);
  // Keep the Worker-created coordinator and its real admission/activity guard.
  // Only the fixture retry interval changes; HTTP and assertion deadlines stay bounded.
  const coordinator = (worker as unknown as { terminalRejections: TerminalRejectionCoordinator }).terminalRejections;
  assert.ok(coordinator);
  Object.defineProperty(coordinator, "intervalMs", { value: 2 });
  const running = worker.run(r.controller.signal);
  try {
    await until(() => started === 2);
    await fs.writeFile(r.file, badCandidate);
    coordinator.queueReconciliation();
    await until(() => posts > 0);
    const before = posts;
    executions[1]!.resolve();
    await until(() => posts > before);
    assert.equal(gets, 0);
    assert.ok(await fs.stat(r.file));
    executions[0]!.resolve();
    await waitForTestEvent(deletion.promise, "guarded cleanup completes after both executions", 5000);
    assert.ok(gets > 0, "cleanup obtained fresh settled custody");
    await assert.rejects(fs.stat(r.file), { code: "ENOENT" });
  } finally {
    executions.forEach(execution => execution.resolve());
    r.controller.abort(); await running;
    await fs.rm(r.root, { recursive: true, force: true });
  }
});

test("claim deadline covers streamed response body; a transport ignoring abort cannot release admission early", async () => {
  const client = new WorkerClient("http://worker.invalid", "join-token", "test", nullLogger());
  const response = deferred<Response>();
  let requestSignal: AbortSignal | undefined;
  globalThis.fetch = async (_url, init) => { requestSignal = init!.signal as AbortSignal; return await response.promise; };
  const locks = new RunDiskLocks();
  const release = await locks.acquire("admission");
  const claim = client.claimRun(undefined, undefined, 20).finally(release);
  let nextEntered = false;
  const next = locks.acquire("admission").then(unlock => { nextEntered = true; unlock(); });
  await sleep(40);
  assert.equal(requestSignal!.aborted, true);
  assert.equal(nextEntered, false);
  response.resolve(Response.json({ run_id: run, claim_generation: 4 }));
  await assert.rejects(claim);
  await next;
  assert.equal(nextEntered, true);

  // A stalled streamed response body also observes the same deadline (no race return).
  let bodyCancelled = false;
  globalThis.fetch = async (_url, init) => new Response(new ReadableStream({
    start(controller) {
      init!.signal!.addEventListener("abort", () => { bodyCancelled = true; controller.error(new Error("deadline")); }, { once: true });
    },
  }));
  await assert.rejects(client.claimRun(undefined, undefined, 20));
  assert.equal(bodyCancelled, true);
});
