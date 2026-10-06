import { test, afterEach } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import path from "node:path";
import { Outbox } from "../src/outbox.js";
import { WorkerClient } from "../src/client.js";
import { TerminalRejectionCoordinator } from "../src/terminal-rejections.js";
import { RunDiskLocks } from "../src/run-disk-locks.js";
import type { TerminalRejectionCustodyResponse } from "../src/protocol.js";
import { nullLogger } from "./helpers.js";
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
  assert.equal(outbox.listPendingTerminals().length, 0);
  assert.equal(outbox.listPendingFinalizes().length, 0);
  const client = new WorkerClient("http://scheduler.invalid", "join-token", "test", nullLogger(), { httpTimeoutMs: 100 });
  client.protocolFeatures = ["terminal_rejection_report"];
  const controller = new AbortController();
  const file = (g = 3) => path.join(opts.root, physical, fileName(g));
  return { root, outbox, client, controller, file,
    async clean(loop: Promise<void>) { controller.abort(); await loop; await fs.rm(root, { recursive: true, force: true }); } };
}

test("long aliases exceeding page metadata budget report all 310 generations despite failed POST/custody", async () => {
  const r = await rig(Array.from({ length: 310 }, (_, i) => i + 1), run, 241);
  const page = await r.outbox.scanTerminalObservationsPage();
  assert.ok(page.observations.length <= 256);
  assert.ok(Buffer.byteLength(JSON.stringify(page.observations)) > 128 * 1024);
  await r.outbox.closeTerminalObservationScan();
  const posted = new Set<number>();
  const reads = new Set<number>();
  let settle = false;
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
      throw new Error("lost diagnostic ACK");
    }
    const generation = Number(new URL(String(url)).searchParams.get("generation"));
    reads.add(generation);
    if (!settle) throw new Error("temporary custody failure");
    return Response.json(custody(generation));
  };
  const coordinator = new TerminalRejectionCoordinator(r.outbox, r.client, nullLogger(), undefined, undefined, 2, 50);
  const loop = coordinator.loop(worker, r.controller.signal);
  try {
    await until(() => posted.size === 310 && reads.size === 310);
    assert.equal(await r.outbox.hasPhysicalTerminalProtection(run), true);
    assert.equal((await fs.readdir(path.dirname(r.file()))).length, 310);
    settle = true;
    coordinator.queueReconciliation();
    await until(async () => (await fs.readdir(path.dirname(r.file()))).length === 0);
    assert.equal(await r.outbox.hasPhysicalTerminalProtection(run), false);
  } finally { await r.clean(loop); }
});

test("no feature makes zero POST/GET, logs once per episode and rollback stops queued retries", async () => {
  const r = await rig([3, 4, 5]);
  r.client.clearFeatures();
  let calls = 0;
  const lines: string[] = [];
  const log = { ...nullLogger(), info: (message: string) => { lines.push(message); } };
  globalThis.fetch = async (_url, init) => {
    calls++;
    assert.equal(init?.method, "POST");
    r.client.clearFeatures();
    const body = JSON.parse(String(init.body));
    return Response.json({ dispositions: body.rejections.map((x: object) => ({ ...x, disposition: "recorded" })) });
  };
  const coordinator = new TerminalRejectionCoordinator(r.outbox, r.client, log, undefined, undefined, 2, 50);
  const loop = coordinator.loop(worker, r.controller.signal);
  try {
    await sleep(30);
    assert.equal(calls, 0);
    assert.equal(lines.length, 1);
    assert.ok(lines[0]!.length < 160);
    r.client.protocolFeatures = ["terminal_rejection_report"];
    coordinator.queueReconciliation();
    await until(() => calls === 1 && lines.length === 2);
    await sleep(30);
    assert.equal(calls, 1);
    assert.equal(lines.length, 2);
    assert.equal(await r.outbox.hasPhysicalTerminalProtection(run), true);
  } finally { await r.clean(loop); }
});

test("exact settled fresh GET removes uppercase physical alias; open sibling retains source protection", async () => {
  const r = await rig([3], run.toUpperCase());
  let siblingOpen = true;
  let gets = 0;
  globalThis.fetch = async (_url, init) => {
    if (init?.method === "POST") {
      const body = JSON.parse(String(init.body));
      return Response.json({ dispositions: body.rejections.map((x: object) => ({ ...x, disposition: "recorded" })) });
    }
    gets++;
    return Response.json(siblingOpen ? { ...custody(3), sibling_count: 1,
      sibling_holds: [{ id: worker, generation: 4 }], outcome: "unknown" } : custody(3));
  };
  const coordinator = new TerminalRejectionCoordinator(r.outbox, r.client, nullLogger(), undefined, undefined, 2, 50);
  const loop = coordinator.loop(worker, r.controller.signal);
  try {
    await until(() => gets > 0);
    assert.equal(await r.outbox.hasPhysicalTerminalProtection(run), true);
    assert.equal(await fs.readFile(r.file(), "utf8"), badRecord());
    siblingOpen = false; // An owner discard is only a wake; this subsequent GET supplies authority.
    coordinator.queueReconciliation(run, 3);
    await until(async () => !(await fs.stat(r.file()).catch(() => undefined)));
    assert.equal(await r.outbox.hasPhysicalTerminalProtection(run), false);
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
