import { test, afterEach } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import path from "node:path";
import { Outbox } from "../src/outbox.js";
import { WorkerClient } from "../src/client.js";
import { TerminalRejectionCoordinator } from "../src/terminal-rejections.js";
import { RunDiskLocks } from "../src/run-disk-locks.js";
import type { TerminalRejectionCustodyResponse } from "../src/protocol.js";
import { nullLogger, waitForTestEvent } from "./helpers.js";
import { sleep } from "../src/util.js";
import os from "node:os";
import { realpathSync } from "node:fs";

const run = "abcdefab-1111-4111-8111-111111111111";
const worker = "22222222-2222-4222-8222-222222222222";
const hold = "33333333-3333-4333-8333-333333333333";
const originalFetch = globalThis.fetch;
afterEach(() => { globalThis.fetch = originalFetch; });
const scratch = realpathSync(os.tmpdir());
function badRecord() { return JSON.stringify({ mac: "0".repeat(64), body: { error: "never-send-this-content" } }); }
function custody(generation: number): TerminalRejectionCustodyResponse {
  return { run_id: run, worker_id: worker, generation, exact_holds: [{ id: hold, state: "released" }],
    sibling_holds: [], exact_count: 1, sibling_count: 0, exact_complete: true, sibling_complete: true, complete: true, outcome: "settled" };
}
async function until(predicate: () => boolean | Promise<boolean>) {
  const deadline = Date.now() + 10_000;
  while (Date.now() < deadline) {
    if (await predicate()) return;
    await sleep(5);
  }
  assert.fail("condition did not complete within ten seconds");
}
async function rig(generations = [3], physical = run, generationWidth = 0) {
  const root = await fs.mkdtemp(path.join(scratch, "terminal-scheduler-"));
  const opts = { root: path.join(root, "outbox"), log: nullLogger(), runMaxBytes: 1e6, maxBytes: 1e7, retentionMs: 1 };
  const first = new Outbox(opts);
  await first.init();
  await fs.mkdir(path.join(opts.root, physical), { recursive: true });
  const fileName = (generation: number) => `terminal-${String(generation).padStart(generationWidth, "0")}.json`;
  for (const generation of generations) await fs.writeFile(path.join(opts.root, physical, fileName(generation)), badRecord());
  // Restart inventory must not lease, finalize or register these unauthenticated records.
  const outbox = new Outbox(opts);
  await outbox.init();
  assert.ok(outbox.listPendingTerminals().length > 0);
  assert.ok(outbox.listPendingTerminals().every(p => p.outcome_kind === "unknown"));
  assert.equal(await outbox.readTerminalJournal(run, generations[0]!), undefined);
  assert.equal(await outbox.hasPendingPolicyOutcome(run), true);
  assert.equal(outbox.listPendingFinalizes().length, 0);
  const client = new WorkerClient("http://scheduler.invalid", "join-token", "test", nullLogger(), { httpTimeoutMs: 100 });
  client.protocolFeatures = ["terminal_rejection_report"];
  const controller = new AbortController();
  const file = (g = 3) => path.join(opts.root, physical, fileName(g));
  return { root, outbox, client, controller, file,
    async clean(loop: Promise<void>) { controller.abort(); await loop; await fs.rm(root, { recursive: true, force: true }); } };
}

test("long aliases exceeding page metadata budget report all 310 generations despite failed POST/custody", async (t) => {
  const r = await rig(Array.from({ length: 310 }, (_, i) => i + 1), run, 241);
  // This probe exercises metadata batching with real I/O, independently of the
  // cooperative elapsed-time budget in Outbox.scanTerminalObservationsPage.
  const probeNow = Date.now();
  const clock = t.mock.method(Date, "now", () => probeNow);
  try {
    const page = await r.outbox.scanTerminalObservationsPage();
    assert.ok(page.observations.length <= 256);
    assert.ok(Buffer.byteLength(JSON.stringify(page.observations)) > 128 * 1024);
  } finally {
    clock.mock.restore();
    await r.outbox.closeTerminalObservationScan();
  }
  const posted = new Set<number>();
  const reads = new Set<number>();
  let settle = false;
  let inventoryObserved!: () => void;
  const inventoryReady = new Promise<void>((resolve) => { inventoryObserved = resolve; });
  const observeInventory = () => { if (posted.size === 310 && reads.size === 310) inventoryObserved(); };
  let removed = 0;
  let deletionObserved!: () => void;
  const deletionReady = new Promise<void>((resolve) => { deletionObserved = resolve; });
  const cleanup = r.outbox.cleanupRejectedTerminalFiles.bind(r.outbox);
  t.mock.method(r.outbox, "cleanupRejectedTerminalFiles", async (...args: Parameters<typeof cleanup>) => {
    const count = await cleanup(...args);
    removed += count;
    if (removed === 310) deletionObserved();
    return count;
  });
  globalThis.fetch = async (url, init) => {
    if (init?.method === "POST") {
      const raw = String(init.body);
      assert.ok(Buffer.byteLength(raw) <= 128 * 1024);
      assert.ok(!raw.includes("never-send-this-content"));
      const body = JSON.parse(raw);
      assert.ok(body.rejections.length <= 256);
      for (const tuple of body.rejections) {
        assert.deepEqual(Object.keys(tuple).sort(), ["claim_generation", "reason", "run_id"]);
        posted.add(tuple.claim_generation);
      }
      observeInventory();
      throw new Error("lost diagnostic ACK");
    }
    const generation = Number(new URL(String(url)).searchParams.get("generation"));
    reads.add(generation);
    observeInventory();
    if (!settle) throw new Error("temporary custody failure");
    return Response.json(custody(generation));
  };
  const coordinator = new TerminalRejectionCoordinator(r.outbox, r.client, nullLogger(), undefined, undefined, 2, 50);
  const loop = coordinator.loop(worker, r.controller.signal);
  try {
    await waitForTestEvent(inventoryReady, "all long-alias generations observed");
    assert.equal(await r.outbox.hasPhysicalTerminalProtection(run), true);
    assert.equal((await fs.readdir(path.dirname(r.file()))).length, 310);
    settle = true;
    coordinator.queueReconciliation();
    await waitForTestEvent(deletionReady, "all long-alias terminals removed");
    assert.equal((await fs.readdir(path.dirname(r.file()))).length, 0);
    assert.equal(await r.outbox.hasPhysicalTerminalProtection(run), false);
    assert.equal(await r.outbox.hasPendingPolicyOutcome(run), false, "authorized cleanup leaves no ghost admission blocker");
    assert.deepEqual(r.outbox.listPendingTerminals(), []);
  } finally { await r.clean(loop); }
});

test("authorized rejection cleanup clears the physical sentinel but preserves independent memory policy evidence", async () => {
  for (const memory of [false, true]) {
    const root = await fs.mkdtemp(path.join(scratch, "terminal-cleanup-memory-"));
    let full = true;
    const outbox = new Outbox({ root, log: nullLogger(), runMaxBytes: 1e6, maxBytes: 1e7, retentionMs: 1,
      rawWrite: async (write, ctx) => {
        if (full && ctx.kind === "terminal") throw Object.assign(new Error("full"), { code: "ENOSPC" });
        await write();
      } });
    try {
      await outbox.init();
      if (memory) await outbox.journalPolicyHold(run, {
        outcome_id: hold, source_generation: 3, claim_generation: 3, detail: "refused",
        continuation_mode: "same_conversation", published: false,
        tag: "cyberPolicy", phase: "implementation", correlation_id: "pr-" + "a".repeat(32) + "-1",
      });
      await fs.mkdir(path.join(root, run), { recursive: true });
      await fs.writeFile(path.join(root, run, "terminal-3.json"), badRecord());
      // Load physical-only unknown metadata, or retain the failed write's memory body.
      const candidate = memory ? outbox : new Outbox({ root, log: nullLogger(),
        runMaxBytes: 1e6, maxBytes: 1e7, retentionMs: 1 });
      if (!memory) await candidate.init();
      const observed = await candidate.observeTerminalAuthentication(run, 3);
      assert.equal(observed.kind, "mac_failure");
      assert.equal(await candidate.hasPendingPolicyOutcome(run), true);
      assert.equal(await candidate.cleanupRejectedTerminalFiles(run, 3, worker, [observed], async () => custody(3)), 1);
      assert.equal(await candidate.hasPhysicalTerminalProtection(run), false);
      assert.equal(await candidate.hasPendingPolicyOutcome(run), memory);
      assert.equal(candidate.listPendingTerminals().length, memory ? 1 : 0);
      assert.equal(await candidate.readTerminalJournal(run, 3), undefined);
      if (memory) {
        full = false;
        await candidate.retryPendingPolicyJournal(run, 3);
        assert.equal((await candidate.readTerminalJournal(run, 3))?.body.outcome_id, hold,
          "guarded rejection cleanup keeps the original memory outcome retryable");
      }
    } finally { await fs.rm(root, { recursive: true, force: true }); }
  }
});

test("rejection cleanup keeps sibling alias snapshot protection until its own guarded disposal", async () => {
  const r = await rig();
  const alias = path.join(path.dirname(r.file()), "terminal-03.json");
  try {
    await fs.writeFile(alias, badRecord());
    const first = await r.outbox.observeTerminalAuthentication(run, 3);
    assert.equal(await r.outbox.cleanupRejectedTerminalFiles(run, 3, worker, [first], async () => custody(3)), 1);
    assert.equal(await fs.readFile(alias, "utf8"), badRecord());
    assert.equal(await r.outbox.hasPendingPolicyOutcome(run), true);
    const { ActiveRunRegistry } = await import("../src/active-run-registry.js");
    const registry = new ActiveRunRegistry(() => r.outbox.listPendingTerminals(), () => 256);
    assert.equal(registry.claimsPausedByPendingOverflow(), true, "surviving alias keeps conservative snapshot exclusion");
    assert.deepEqual(registry.build().pending_all, [{ run_id: run, claim_generation: 3 }]);
    const remaining = await r.outbox.observeTerminalAuthentication(run, 3, "terminal-03.json");
    assert.equal(await r.outbox.cleanupRejectedTerminalFiles(run, 3, worker, [remaining], async () => custody(3)), 1);
    assert.equal(await r.outbox.hasPendingPolicyOutcome(run), false);
    assert.equal(registry.claimsPausedByPendingOverflow(), false);
  } finally { r.controller.abort(); await fs.rm(r.root, { recursive: true, force: true }); }
});

test("rejection cleanup bounds ignored alias entries and retains uncertainty on exhaustion", async t => {
  const r = await rig();
  try {
    for (let i = 0; i < 4100; i++) await fs.writeFile(path.join(path.dirname(r.file()), "ignored-" + i), "");
    const observed = await r.outbox.observeTerminalAuthentication(run, 3);
    let entries = 0;
    const open = fs.opendir.bind(fs);
    t.mock.method(fs, "opendir", async (file: Parameters<typeof fs.opendir>[0]) => {
      const directory = await open(file);
      return new Proxy(directory, { get(target, property) {
        if (property === "read") return async () => {
          const entry = await target.read();
          if (entry) entries++;
          return entry;
        };
        const value = Reflect.get(target, property);
        return typeof value === "function" ? value.bind(target) : value;
      } });
    });
    assert.equal(await r.outbox.cleanupRejectedTerminalFiles(run, 3, worker, [observed], async () => custody(3)), 1);
    assert.ok(entries <= 4096, "sibling lookup cannot materialize an unbounded directory");
    assert.equal(await r.outbox.hasPendingPolicyOutcome(run), true, "exhaustion is not absence proof");
    assert.equal(r.outbox.listPendingTerminals()[0]?.outcome_kind, "unknown");
  } finally { r.controller.abort(); await fs.rm(r.root, { recursive: true, force: true }); }
});

test("no feature makes zero POST/GET, logs once per episode and rollback stops queued retries", async (t) => {
  const r = await rig([3, 4, 5]);
  r.client.clearFeatures();
  let phase: "initial" | "rollback" = "initial";
  let falseChecks = 0;
  let initialObserved!: () => void;
  const initialReady = new Promise<void>((resolve) => { initialObserved = resolve; });
  let rollbackObserved!: () => void;
  const rollbackReady = new Promise<void>((resolve) => { rollbackObserved = resolve; });
  const hasFeature = r.client.hasFeature.bind(r.client);
  t.mock.method(r.client, "hasFeature", (feature: string) => {
    const supported = hasFeature(feature);
    if (feature === "terminal_rejection_report" && !supported && ++falseChecks >= 2) {
      if (phase === "initial") initialObserved();
      else rollbackObserved();
    }
    return supported;
  });
  let calls = 0;
  const lines: string[] = [];
  const log = { ...nullLogger(), info: (message: string) => { lines.push(message); } };
  globalThis.fetch = async (_url, init) => {
    calls++;
    assert.equal(init?.method, "POST");
    phase = "rollback";
    falseChecks = 0;
    r.client.clearFeatures();
    const body = JSON.parse(String(init.body));
    return Response.json({ dispositions: body.rejections.map((x: object) => ({ ...x, disposition: "recorded" })) });
  };
  const coordinator = new TerminalRejectionCoordinator(r.outbox, r.client, log, undefined, undefined, 2, 50);
  const loop = coordinator.loop(worker, r.controller.signal);
  try {
    await waitForTestEvent((async () => {
      // supported() logs synchronously after hasFeature returns, before this continuation.
      await initialReady;
      assert.ok(falseChecks >= 2);
      assert.equal(calls, 0);
      assert.equal(lines.length, 1);
      assert.ok(lines[0]!.length < 160);
      r.client.protocolFeatures = ["terminal_rejection_report"];
      coordinator.queueReconciliation();
      await rollbackReady;
      assert.ok(falseChecks >= 2);
      assert.equal(calls, 1);
      assert.equal(lines.length, 2);
      assert.ok(lines[1]!.length < 160);
      assert.equal(await r.outbox.hasPhysicalTerminalProtection(run), true);
    })(), "two unsupported checks in each feature-disabled episode", 10_000);
  } finally { await r.clean(loop); }
});

test("exact settled fresh GET removes uppercase physical alias; open sibling retains source protection", async (t) => {
  const r = await rig([3], run.toUpperCase());
  let siblingOpen = true;
  let openCustodyObserved!: () => void;
  const openCustodyReady = new Promise<void>((resolve) => { openCustodyObserved = resolve; });
  let cleanupObserved!: (count: number) => void;
  const cleanupReady = new Promise<number>((resolve) => { cleanupObserved = resolve; });
  const cleanup = r.outbox.cleanupRejectedTerminalFiles.bind(r.outbox);
  t.mock.method(r.outbox, "cleanupRejectedTerminalFiles", async (...args: Parameters<typeof cleanup>) => {
    const count = await cleanup(...args);
    if (count > 0) cleanupObserved(count);
    return count;
  });
  globalThis.fetch = async (_url, init) => {
    if (init?.method === "POST") {
      const body = JSON.parse(String(init.body));
      return Response.json({ dispositions: body.rejections.map((x: object) => ({ ...x, disposition: "recorded" })) });
    }
    if (siblingOpen) openCustodyObserved();
    return Response.json(siblingOpen ? { ...custody(3), sibling_count: 1,
      sibling_holds: [{ id: worker, generation: 4 }], outcome: "unknown" } : custody(3));
  };
  const coordinator = new TerminalRejectionCoordinator(r.outbox, r.client, nullLogger(), undefined, undefined, 2, 50);
  const loop = coordinator.loop(worker, r.controller.signal);
  try {
    await waitForTestEvent(openCustodyReady, "fresh custody with an open sibling", 10_000);
    assert.equal(await r.outbox.hasPhysicalTerminalProtection(run), true);
    assert.equal(await fs.readFile(r.file(), "utf8"), badRecord());
    siblingOpen = false; // An owner discard is only a wake; this subsequent GET supplies authority.
    coordinator.queueReconciliation(run, 3);
    assert.equal(await waitForTestEvent(cleanupReady, "completed uppercase-alias cleanup", 10_000), 1);
    await assert.rejects(fs.stat(r.file()), { code: "ENOENT" });
    assert.equal(await r.outbox.hasPhysicalTerminalProtection(run), false);
    assert.equal(await r.outbox.hasPendingPolicyOutcome(run), false, "uppercase cleanup drops its exact unknown sentinel");
  } finally { await r.clean(loop); }
});

test("replacement, unknown custody, active during fresh read, and pre-unlink activity deny deletion", async () => {
  for (const mode of ["replacement", "unknown", "active", "preunlink"] as const) {
    const r = await rig();
    let gets = 0;
    let active = false;
    let checks = 0;
    globalThis.fetch = async (_url, init) => {
      if (init?.method === "POST") {
        const body = JSON.parse(String(init.body));
        return Response.json({ dispositions: body.rejections.map((x: object) => ({ ...x, disposition: "recorded" })) });
      }
      gets++;
      if (mode === "replacement") await fs.writeFile(r.file(), badRecord() + " ");
      if (mode === "active") active = true;
      return Response.json(mode === "unknown" ? { ...custody(3), exact_count: 0, exact_holds: [], outcome: "unknown" } : custody(3));
    };
    const coordinator = new TerminalRejectionCoordinator(r.outbox, r.client, nullLogger(), undefined,
      () => active || (mode === "preunlink" && gets > 0 && ++checks >= 3), 1000, 50);
    const loop = coordinator.loop(worker, r.controller.signal);
    try {
      await until(() => gets > 0);
      await sleep(20);
      assert.ok(await fs.stat(r.file()), mode);
      assert.equal(await r.outbox.hasPhysicalTerminalProtection(run), true, mode);
    } finally { await r.clean(loop); }
  }
});

test("disposal deadlines include admission, normalized disk and physical alias lock waits", async () => {
  for (const blocked of ["admission", "disk", "physical"] as const) {
    const r = await rig([3, 4], run.toUpperCase());
    const locks = new RunDiskLocks();
    const coordinator = new TerminalRejectionCoordinator(r.outbox, r.client, nullLogger(), locks, undefined, 2, 20);
    const release = blocked === "admission" ? await coordinator.acquireAdmission()
      : await locks.acquire(blocked === "disk" ? run : run.toUpperCase());
    let posts = 0;
    let gets = 0;
    globalThis.fetch = async (_url, init) => {
      if (init?.method === "POST") { posts++; throw new Error("lost ACK"); }
      gets++;
      return Response.json(custody(Number(new URL(String(_url)).searchParams.get("generation"))));
    };
    const loop = coordinator.loop(worker, r.controller.signal);
    try {
      if (blocked === "admission") await assert.rejects(coordinator.acquireAdmission());
      await until(() => posts >= 4);
      assert.equal(gets, 0, blocked);
      assert.equal((await fs.readdir(path.dirname(r.file()))).length, 2, blocked);
      release();
      coordinator.queueReconciliation();
      await until(async () => (await fs.readdir(path.dirname(r.file()))).length === 0);
    } finally { release(); await r.clean(loop); }
  }
});

test("overlapping same-run protection releases independently and idempotently", async () => {
  const r = await rig();
  const coordinator = new TerminalRejectionCoordinator(r.outbox, r.client, nullLogger(), undefined, undefined, 2, 30);
  const releaseFirst = coordinator.protectExecution(run);
  const releaseSecond = coordinator.protectExecution(run.toUpperCase());
  let posts = 0;
  let gets = 0;
  globalThis.fetch = async (_url, init) => {
    if (init?.method === "POST") { posts++; throw new Error("lost ACK"); }
    gets++;
    return Response.json(custody(3));
  };
  const loop = coordinator.loop(worker, r.controller.signal);
  try {
    await until(() => posts >= 2);
    releaseSecond();
    releaseSecond();
    const before = posts;
    await until(() => posts >= before + 2);
    assert.equal(gets, 0);
    assert.ok(await fs.stat(r.file()));
    releaseFirst();
    await until(async () => !(await fs.stat(r.file()).catch(() => undefined)));
  } finally { releaseFirst(); releaseSecond(); await r.clean(loop); }
});

test("aborted disk-lock waiter never runs late and siblings retain FIFO", async () => {
  const locks = new RunDiskLocks();
  const release = await locks.acquire(run);
  const abort = new AbortController();
  const cancelled = locks.acquire(run, abort.signal);
  let admitted = false;
  const sibling = locks.acquire(run).then((unlock) => { admitted = true; unlock(); });
  abort.abort();
  await assert.rejects(cancelled);
  assert.equal(admitted, false);
  release();
  await sibling;
  assert.equal(admitted, true);
});
