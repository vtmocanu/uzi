import { afterEach, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import { Worker } from "../src/worker.js";
import type { Config } from "../src/config.js";
import type { WorkerClient } from "../src/client.js";
import type { RunRunner } from "../src/runner.js";
import type { ChatRunner } from "../src/chat-runner.js";
import type { JudgeRunner } from "../src/judge-runner.js";
import type { ReviewRunner } from "../src/review-runner.js";
import { Outbox, type PendingFinalize } from "../src/outbox.js";
import { ActiveRunRegistry } from "../src/active-run-registry.js";
import { nullLogger } from "./helpers.js";
import { mkSweepOutbox, pollUntil, startSweepWorker, sweepClient } from "./worker-sweep-rig.js";

const RUN = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa";
const RUN2 = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb";
const entry = (id = RUN, gen = 1): PendingFinalize => ({ run_id: id, claim_generation: gen });
const roots: string[] = [];
afterEach(async () => {
  for (const root of roots.splice(0)) await fs.rm(root, { recursive: true, force: true });
});
async function rig() {
  const r = await mkSweepOutbox();
  roots.push(r.root);
  return r;
}
function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (error: Error) => void;
  const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}
type Seam = {
  sweepPendingFinalizes(signal?: AbortSignal): Promise<void>;
  admittedRunIds: Map<string, number>;
};
function worker(outbox: Outbox, runner: Partial<RunRunner>, registry?: ActiveRunRegistry): Seam {
  return new Worker({} as Config, {} as WorkerClient, runner as RunRunner,
    {} as ChatRunner, {} as JudgeRunner, {} as ReviewRunner, nullLogger(),
    () => ({ ok: true, missing: [] }), outbox, new Map(), registry) as unknown as Seam;
}
function fakeOutbox(entries: PendingFinalize[]) {
  const deleted: PendingFinalize[] = [];
  const outbox = {
    listPendingFinalizeGenerations: () => entries.filter(e => !deleted.includes(e)),
    retireFinalizeIfEligible: async (id: string, gen: number, eligible: () => boolean, signal: AbortSignal) => {
      if (!signal.aborted && eligible()) deleted.push(entries.find(e => e.run_id === id && e.claim_generation === gen)!);
    },
  } as unknown as Outbox;
  return { outbox, deleted };
}

it("uncapped authenticated enumeration retains register's highest-generation cap and excludes every terminal run", async () => {
  const { outbox, root } = await rig();
  for (let n = 0; n < 270; n++) {
    const id = n.toString(16).padStart(8, "0") + "-aaaa-aaaa-aaaa-aaaaaaaaaaaa";
    assert.equal((await outbox.journalFinalize(id, 1)).written, true);
  }
  await outbox.journalFinalize(RUN, 1);
  await outbox.journalFinalize(RUN, 2);
  await outbox.journalFinalize(RUN2, 1);
  await outbox.journalTerminal(RUN2, 3, "running", 0, { status: "completed" });
  assert.equal(outbox.listPendingFinalizes().length, 256);
  assert.equal(outbox.listPendingFinalizeGenerations().length, 272);
  assert.deepEqual(outbox.listPendingFinalizeGenerations().filter(e => e.run_id === RUN), [entry(), entry(RUN, 2)]);
  assert.equal(outbox.listPendingFinalizeGenerations().some(e => e.run_id === RUN2), false);
  const reopened = new Outbox({ root: root + "/outbox", log: nullLogger(),
    runMaxBytes: 64 * 1024 * 1024, maxBytes: 512 * 1024 * 1024, retentionMs: 7 * 86_400_000 });
  await reopened.init();
  assert.equal(reopened.listPendingFinalizeGenerations().length, 272);
  const checked: PendingFinalize[] = [];
  const w = worker(reopened, { recoveryInventoryPending: async (id, gen) => {
    checked.push(entry(id, gen));
    return id.startsWith("00000000") || (id === RUN && gen === 1);
  } });
  await w.sweepPendingFinalizes();
  assert.equal(checked.length, 16, "exact check cap per pass");
  assert.equal(reopened.listPendingFinalizeGenerations().length, 257, "only 15 authorized deletions in first pass");
  for (let n = 0; n < 17; n++) await w.sweepPendingFinalizes();
  assert.equal(reopened.listPendingFinalizeGenerations().length, 2, "held oldest and lower generation remain");
  assert.deepEqual(reopened.listPendingFinalizeGenerations().filter(e => e.run_id === RUN), [entry()]);
  assert.ok(checked.some(e => e.run_id.startsWith("0000010d")), "beyond register cap is served");
});

it("process-wide 16 unresolved calls, per-key exclusion, late false ignored, and freed slots serve siblings", async () => {
  const entries = Array.from({ length: 20 }, (_, n) => entry(RUN, n + 1));
  const { outbox, deleted } = fakeOutbox(entries);
  const held = Array.from({ length: 20 }, () => deferred<boolean>());
  const calls: number[] = [];
  const runner = { recoveryInventoryPending: async (_id: string, gen: number) => {
    calls.push(gen); return held[gen - 1]!.promise;
  } };
  const w = worker(outbox, runner);
  const first = w.sweepPendingFinalizes();
  await pollUntil(() => calls.length === 16, 500, "16 calls started");
  await w.sweepPendingFinalizes();
  const other = worker(outbox, runner);
  await other.sweepPendingFinalizes();
  assert.equal(calls.length, 16, "single-flight plus process-wide cap");
  await first;
  await w.sweepPendingFinalizes();
  assert.equal(calls.length, 16, "timeouts retain quarantine");
  held[0]!.resolve(false);
  await new Promise<void>(resolve => setImmediate(resolve));
  assert.equal(deleted.length, 0, "late false never deletes");
  const next = w.sweepPendingFinalizes();
  await pollUntil(() => calls.length === 17, 500, "freed slot used");
  assert.equal(calls[16], 17, "unattempted sibling precedes old held key");
  for (let n = 1; n < held.length; n++) held[n]!.resolve(true);
  await next;
  assert.equal(new Set(calls.slice(0, 16)).size, 16);
});

it("stalled authority does not block sibling retirement or successful heartbeats", async () => {
  const { outbox } = await rig();
  const held = deferred<boolean>();
  let beats = 0;
  let calls = 0;
  const w = startSweepWorker({ outbox, client: sweepClient({ heartbeat: async () => { beats++; } }),
    runner: { recoveryInventoryPending: async (id) => {
      if (id === RUN) { calls++; return held.promise; }
      return false;
    } } });
  try {
    await pollUntil(() => beats >= 3, 3000, "registered");
    await outbox.journalFinalize(RUN, 1);
    await pollUntil(() => calls === 1, 3000, "stalled authority started");
    // The sibling first becomes eligible on the next pass after the bounded authority wait.
    await outbox.journalFinalize(RUN2, 1);
    await pollUntil(() => calls === 1 && !outbox.listPendingFinalizeGenerations().some(e => e.run_id === RUN2),
      3000, "sibling cleared during stalled authority");
    const seen = beats;
    await pollUntil(() => beats >= seen + 10, 3000, "heartbeat remains independent");
    await new Promise(resolve => setTimeout(resolve, 1100));
    assert.equal(calls, 1, "timed-out key stays excluded");
    held.resolve(true);
    await new Promise<void>(resolve => setImmediate(resolve));
  } finally {
    held.resolve(true);
    await w.stop();
  }
});

it("true, undefined, throw, rejection and missing predicate/API fail closed", async () => {
  for (const result of [true, undefined, "throw", "reject", "missing"]) {
    const { outbox, deleted } = fakeOutbox([entry()]);
    const runner = result === "missing" ? {} : { recoveryInventoryPending: () => {
      if (result === "throw") throw new Error("unknown");
      if (result === "reject") return Promise.reject(new Error("unknown"));
      return Promise.resolve(result);
    } };
    await worker(outbox, runner as Partial<RunRunner>).sweepPendingFinalizes();
    assert.equal(deleted.length, 0, String(result));
  }
  let checks = 0;
  for (const partial of [
    {},
    { listPendingFinalizeGenerations: () => [entry()] },
    { retireFinalizeIfEligible: async () => {} },
  ]) {
    await worker(partial as unknown as Outbox, { recoveryInventoryPending: async () => { checks++; return false; } })
      .sweepPendingFinalizes();
  }
  assert.equal(checks, 0);
});

it("candidate rejection retains custody while siblings clear, with no leaked authority slot", async () => {
  const { outbox, deleted } = fakeOutbox([entry(), entry(RUN2)]);
  let attempts = 0;
  const w = worker(outbox, { recoveryInventoryPending: async (id) => {
    if (id === RUN && ++attempts === 1) throw new Error("unknown recovery tail");
    return false;
  } });
  await w.sweepPendingFinalizes();
  assert.deepEqual(deleted, [entry(RUN2)]);
  await w.sweepPendingFinalizes();
  assert.equal(deleted.length, 2, "rejection settled its quarantine slot");
});

it("combined live fence protects admission, registry, and executing runner even with a registry present", async () => {
  const { outbox } = await rig();
  await outbox.journalFinalize(RUN, 1);
  const registry = new ActiveRunRegistry(() => [], () => 0);
  let executing = true;
  let checks = 0;
  const w = worker(outbox, { isExecuting: () => executing,
    recoveryInventoryPending: async () => { checks++; return false; } }, registry);
  await w.sweepPendingFinalizes();
  executing = false;
  w.admittedRunIds.set(RUN, 1);
  await w.sweepPendingFinalizes();
  w.admittedRunIds.clear();
  const has = registry.has.bind(registry);
  registry.has = () => true;
  await w.sweepPendingFinalizes();
  registry.has = has;
  assert.equal(checks, 0);
  await w.sweepPendingFinalizes();
  assert.equal(checks, 1);
  assert.deepEqual(outbox.listPendingFinalizeGenerations(), []);
});

it("a terminal appearing during authority wait retains all generations", async () => {
  const { outbox } = await rig();
  await outbox.journalFinalize(RUN, 1);
  const authority = deferred<boolean>();
  let checking = false;
  const w = worker(outbox, { recoveryInventoryPending: async () => { checking = true; return authority.promise; } });
  const pass = w.sweepPendingFinalizes();
  await pollUntil(() => checking, 500, "authority waiting");
  await outbox.journalTerminal(RUN, 2, "running", 0, { status: "completed" });
  authority.resolve(false);
  await pass;
  assert.equal(outbox.listPendingFinalizeGenerations().length, 0, "terminal hides candidate");
  await outbox.retireTerminal(RUN, 2);
  assert.deepEqual(outbox.listPendingFinalizeGenerations(), [entry()], "hidden finalize was not deleted");
});

it("queued lock rechecks admission/execution and terminal, abort cancels stale deletion", async () => {
  for (const protection of ["admission", "execution", "terminal", "abort"]) {
    const { outbox } = await rig();
    await outbox.journalFinalize(RUN, 1);
    const lock = outbox as unknown as { tryRunLock(id: string): (() => void) | null };
    const release = lock.tryRunLock(RUN)!;
    const authority = deferred<boolean>();
    let checking = false;
    let executing = false;
    const controller = new AbortController();
    const w = worker(outbox, { isExecuting: () => executing, recoveryInventoryPending: async () => {
      checking = true; return authority.promise;
    } });
    const pass = w.sweepPendingFinalizes(controller.signal);
    await pollUntil(() => checking, 500, "authority started");
    const terminal = protection === "terminal" ?
      outbox.journalTerminal(RUN, 2, "running", 0, { status: "completed" }) : undefined;
    authority.resolve(false);
    await new Promise<void>(resolve => setImmediate(resolve));
    if (protection === "admission") w.admittedRunIds.set(RUN, 1);
    if (protection === "execution") executing = true;
    if (protection === "abort") controller.abort();
    release();
    await pass;
    await terminal;
    if (protection === "terminal") await outbox.retireTerminal(RUN, 2);
    assert.deepEqual(outbox.listPendingFinalizeGenerations(), [entry()]);
  }
});

it("five-second pass deadline bounds a queued deletion and prevents unlink after lock release", async () => {
  const { outbox } = await rig();
  await outbox.journalFinalize(RUN, 1);
  const release = (outbox as unknown as { tryRunLock(id: string): (() => void) | null }).tryRunLock(RUN)!;
  const w = worker(outbox, { recoveryInventoryPending: async () => false });
  const start = Date.now();
  try {
    await w.sweepPendingFinalizes();
    assert.ok(Date.now() - start < 6000, "pass stops awaiting lock");
  } finally { release(); }
  await new Promise<void>(resolve => setImmediate(resolve));
  assert.deepEqual(outbox.listPendingFinalizeGenerations(), [entry()]);
});
