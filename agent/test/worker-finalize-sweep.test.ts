import { afterEach, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import { Worker } from "../src/worker.js";
import type { Config } from "../src/config.js";
import { RequestError, type WorkerClient } from "../src/client.js";
import type { RunRunner } from "../src/runner.js";
import type { ChatRunner } from "../src/chat-runner.js";
import type { JudgeRunner } from "../src/judge-runner.js";
import type { ReviewRunner } from "../src/review-runner.js";
import { Outbox, type PendingFinalize } from "../src/outbox.js";
import { RecoveryCoordinator } from "../src/recovery.js";
import { StubExecutor } from "../src/executor.js";
import { client as recoveryClient, git, TOKEN, fakeGitlab, installHarness, runner } from "./runner-harness.js";

installHarness();
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
type ReworkSeam = Seam & {
  registerWithRetry(signal: AbortSignal): Promise<void>;
  resolveBootTerminals(signal: AbortSignal): Promise<void>;
  sweepPendingTerminals(signal?: AbortSignal): Promise<void>;
};
function reworkWorker(outbox: Outbox, ownership: () => Promise<unknown>,
  custody: (id: string, gen: number) => Promise<boolean> = async () => false,
  register: WorkerClient["register"] = async () => ({}),
): ReworkSeam {
  const client = { ...sweepClient(), getRunOwnership: ownership, register };
  return new Worker({ pollIntervalMs: 1, gapFillMax: 100, outboxTerminalMaxBytes: 1 << 20 } as Config,
    client as unknown as WorkerClient, { recoveryInventoryPending: custody } as RunRunner,
    {} as ChatRunner, {} as JudgeRunner, {} as ReviewRunner, nullLogger(),
    () => ({ ok: true, missing: [] }), outbox, new Map(),
    new ActiveRunRegistry(() => outbox.listPendingTerminals(), () => 0)) as unknown as ReworkSeam;
}
// Match runner-inventory-cleanup: authenticated journal predates generation tracking.
async function realRecoveryCoordinator(root: string) {
  const recovery = new RecoveryCoordinator({ recoveryRoot: root + "/legacy-recovery",
    workerToken: TOKEN, client: recoveryClient, git, log: nullLogger() });
  assert.ok(await recovery.pin({ runId: RUN, kind: "issue", branch: "task", sourceSha: "a".repeat(40) }));
  recoveryClient.clearFeatures();
  const { gitlab } = fakeGitlab();
  const actual = runner(new StubExecutor(nullLogger()), gitlab, TOKEN, { recovery });
  assert.equal(await actual.recoveryInventoryPending(RUN, 1), false, "real pre-generation legacy custody");
  return async (id: string, gen: number) => id === RUN ? actual.recoveryInventoryPending(id, gen) : false;
}
const running = async () => ({ status: "running", claim_generation: 99 });
const terminal = async () => ({ status: "completed", claim_generation: 99 });
const signal = () => new AbortController().signal;

type Seam = {
  sweepPendingFinalizes(signal?: AbortSignal): Promise<void>;
  admittedRunIds: Map<string, number>;
};
function worker(outbox: Outbox, runner: Partial<RunRunner>, registry?: ActiveRunRegistry): Seam {
  return new Worker({} as Config, { getRunOwnership: terminal } as unknown as WorkerClient, runner as RunRunner,
    {} as ChatRunner, {} as JudgeRunner, {} as ReviewRunner, nullLogger(),
    () => ({ ok: true, missing: [] }), outbox, new Map(), registry) as unknown as Seam;
}
function fakeOutbox(entries: PendingFinalize[]) {
  const deleted: PendingFinalize[] = [];
  const outbox = {
    listPendingFinalizeGenerations: () => entries.filter(e => !deleted.includes(e)),
    finalizeRecordIdentity: (id: string, gen: number) => entries.find(e => e.run_id === id && e.claim_generation === gen),
    retireFinalizeIfEligible: async (id: string, gen: number, eligible: () => boolean, signal: AbortSignal) => {
      if (!signal.aborted && eligible()) deleted.push(entries.find(e => e.run_id === id && e.claim_generation === gen)!);
    },
  } as unknown as Outbox;
  return { outbox, deleted };
}

it("rework-2464: failed registration supplies no handoff to later custody false retry", async () => {
  const { outbox } = await rig();
  await outbox.journalFinalize(RUN, 1);
  const w = reworkWorker(outbox, running, async () => false, async () => {
    throw new RequestError("POST", "/api/worker/register", 401, "rejected");
  });
  await assert.rejects(w.registerWithRetry(signal()));
  await w.sweepPendingFinalizes();
  assert.deepEqual(outbox.listPendingFinalizeGenerations(), [entry()], "failed request confers no handoff");
});

it("rework-2464-mechanism: accepted offered identity survives pending custody and terminal hiding without reregister", async () => {
  const { outbox } = await rig();
  await outbox.journalFinalize(RUN, 1);
  let pending = true;
  let registrations = 0;
  let ownershipReads = 0;
  const w = reworkWorker(outbox, async () => { ownershipReads++; return running(); },
    async () => pending, async () => { registrations++; return {}; });
  await w.registerWithRetry(signal());
  assert.deepEqual(outbox.listPendingFinalizeGenerations(), [entry()]);
  await outbox.journalTerminal(RUN, 1, "running", 0, { status: "completed" });
  await w.sweepPendingFinalizes();
  await outbox.retireTerminal(RUN, 1);
  pending = false;
  await w.sweepPendingFinalizes();
  assert.deepEqual(outbox.listPendingFinalizeGenerations(), []);
  assert.equal(registrations, 1);
  assert.equal(ownershipReads, 0, "same original offered record uses accepted handoff");
});

it("rework-2464-mechanism: replacement during fresh terminal ownership wait cannot delete by old identity", async () => {
  const { outbox } = await rig();
  (outbox as unknown as { now: () => number }).now = () => 1234;
  await outbox.journalFinalize(RUN, 1);
  const proof = deferred<unknown>();
  let entered = false;
  const w = reworkWorker(outbox, async () => { entered = true; return proof.promise; });
  const pass = w.sweepPendingFinalizes();
  await pollUntil(() => entered, 500, "ownership read");
  await outbox.retireFinalize(RUN, 1);
  await outbox.journalFinalize(RUN, 1);
  proof.resolve(await terminal());
  await pass;
  assert.deepEqual(outbox.listPendingFinalizeGenerations(), [entry()], "fresh proof was for original candidate");
});

it("rework-2464-mechanism: stalled ownership bounded wait quarantines slots, siblings progress, late proof cannot delete", async () => {
  const { outbox } = await rig();
  for (let n = 1; n <= 20; n++) await outbox.journalFinalize(RUN, n);
  const proof = deferred<unknown>();
  let calls = 0;
  let stalled = true;
  const w = reworkWorker(outbox, async () => { calls++; return stalled ? proof.promise : terminal(); });
  const pass = w.sweepPendingFinalizes();
  try {
    const bounded = await Promise.race([pass.then(() => true),
      new Promise<boolean>(resolve => setTimeout(() => resolve(false), 1600))]);
    assert.equal(bounded, true, "one-second ownership wait must bound the pass");
    assert.equal(calls, 16, "unresolved ownership uses the process-wide slots");
    await w.sweepPendingFinalizes();
    assert.equal(calls, 16, "timed-out calls remain quarantined");
    proof.resolve(await terminal());
    await new Promise<void>(resolve => setImmediate(resolve));
    assert.equal(outbox.listPendingFinalizeGenerations().length, 20, "late results never delete");
    stalled = false;
    await w.sweepPendingFinalizes();
    assert.deepEqual(outbox.listPendingFinalizeGenerations(), [entry(RUN, 13), entry(RUN, 14), entry(RUN, 15), entry(RUN, 16)],
      "unattempted siblings receive slots first");
  } finally {
    proof.resolve(await terminal());
    await pass;
  }
});

it("rework-2464-mechanism: one stalled ownership read does not block a terminal sibling", async () => {
  const { outbox } = await rig();
  await outbox.journalFinalize(RUN, 1);
  await outbox.journalFinalize(RUN2, 1);
  const proof = deferred<unknown>();
  let calls = 0;
  const w = reworkWorker(outbox, async () => {
    calls++;
    return calls === 1 ? proof.promise : terminal();
  });
  const pass = w.sweepPendingFinalizes();
  try {
    await pollUntil(() => !outbox.listPendingFinalizeGenerations().some(e => e.run_id === RUN2),
      500, "terminal sibling deletes while ownership is stalled");
    assert.deepEqual(outbox.listPendingFinalizeGenerations(), [entry()]);
    await pass;
    proof.resolve(await terminal());
    await new Promise<void>(resolve => setImmediate(resolve));
    assert.deepEqual(outbox.listPendingFinalizeGenerations(), [entry()], "late proof cannot delete");
  } finally {
    proof.resolve(await terminal());
    await pass;
  }
});

it("rework-2464-mechanism: accepted map is bounded for lifetime across subsequent registration calls", async () => {
  const { outbox } = await rig();
  for (let n = 0; n < 257; n++) {
    await outbox.journalFinalize(n.toString(16).padStart(8, "0") + "-aaaa-aaaa-aaaa-aaaaaaaaaaaa", 1);
  }
  const w = reworkWorker(outbox, running, async () => false);
  await w.registerWithRetry(signal());
  const accepted = (w as unknown as { acceptedFinalizes: Map<string, Readonly<object>> }).acceptedFinalizes;
  assert.equal(accepted.size, 256);
  await w.registerWithRetry(signal());
  assert.equal(accepted.size, 256, "later registration cannot extend the accepted lifetime map");
  assert.equal(outbox.listPendingFinalizeGenerations().length, 1, "later offer cannot add handoff");
});

it("rework-2464-mechanism: registration cleans frozen lower originals beyond 16 without saving their authority", async () => {
  const { outbox } = await rig();
  for (let gen = 0; gen <= 20; gen++) await outbox.journalFinalize(RUN, gen);
  let pending = true;
  let ownershipReads = 0;
  const w = reworkWorker(outbox, async () => { ownershipReads++; return running(); },
    async (_id, gen) => gen === 1 && pending);
  await w.registerWithRetry(signal());
  assert.deepEqual(outbox.listPendingFinalizeGenerations(), [entry()], "all clear frozen originals attempted across waves");
  assert.equal(ownershipReads, 0, "accepted batch needs each exact custody check only");
  const accepted = (w as unknown as { acceptedFinalizes: Map<string, Readonly<object>> }).acceptedFinalizes;
  assert.equal(accepted.size, 1, "only offered G20 enters lifetime map");
  pending = false;
  await w.sweepPendingFinalizes();
  assert.deepEqual(outbox.listPendingFinalizeGenerations(), [entry()], "lower batch authority expires");
  assert.equal(ownershipReads, 1, "later lower retry needs fresh terminal proof");
});

for (const status of ["completed", "failed", "cancelled"]) {
  it(`rework-2464-mechanism: terminal ${status} accepts generation zero and boolean guarded fields`, async () => {
    const { outbox } = await rig();
    await outbox.journalFinalize(RUN, 0);
    await reworkWorker(outbox, async () => ({ status, claim_generation: 0, inventory_guarded: true })).sweepPendingFinalizes();
    assert.deepEqual(outbox.listPendingFinalizeGenerations(), []);
  });
}

// Defect regressions use only interfaces present on the historical base.
it("rework-2464: real legacy omitted running finalize beyond 256 with pending overflow survives until terminal proof", async () => {
  const { outbox, root } = await rig();
  const custody = await realRecoveryCoordinator(root);
  for (let n = 0; n < 257; n++) {
    await outbox.journalFinalize(n.toString(16).padStart(8, "0") + "-aaaa-aaaa-aaaa-aaaaaaaaaaaa", 1);
  }
  await outbox.journalFinalize(RUN, 1);
  await outbox.journalFinalize(RUN2, 1);
  await outbox.journalTerminal(RUN2, 1, "running", 0, { status: "completed" });
  let offered = 0;
  let status = "running";
  const w = reworkWorker(outbox, async () => ({ status, claim_generation: 99 }), custody, async (...args) => {
    offered = args[5]?.finalize_resume?.length ?? 0;
    assert.equal(args[5]?.pending_overflow, true);
    assert.equal(args[5]?.finalize_resume?.some(e => e.run_id === RUN), false, "real legacy omitted from wire");
    return {};
  });
  await w.registerWithRetry(signal());
  for (let n = 0; n < 18; n++) await w.sweepPendingFinalizes();
  assert.equal(offered, 256);
  assert.equal(outbox.listPendingFinalizeGenerations().length, 2, "unoffered running records retain");
  assert.ok(outbox.listPendingFinalizeGenerations().some(e => e.run_id === RUN), "actual legacy omitted running record survives");
  await outbox.retireTerminal(RUN2, 1);
  assert.equal(outbox.listPendingFinalizeGenerations().length, 3, "terminal-hidden record retains");
  status = "completed";
  await w.sweepPendingFinalizes();
  assert.deepEqual(outbox.listPendingFinalizeGenerations(), []);
});

for (const phase of ["handoff", "register", "authority"] as const) {
  it(`rework-2464: same timestamp same-key replacement during ${phase} does not inherit handoff`, async () => {
    const { outbox } = await rig();
    (outbox as unknown as { now: () => number }).now = () => 1234;
    await outbox.journalFinalize(RUN, 2);
    const replace = async () => {
      await outbox.retireFinalize(RUN, 2);
      await outbox.journalFinalize(RUN, 2);
    };
    let replaced = false;
    const w = reworkWorker(outbox, running, async () => {
      if (phase === "authority" && !replaced) { replaced = true; await replace(); }
      return false;
    }, async (...args) => {
      assert.deepEqual(args[5]?.finalize_resume, [entry(RUN, 2)], "original was actually offered");
      if (phase === "register") await replace();
      return {};
    });
    await w.registerWithRetry(signal());
    if (phase === "handoff") {
      assert.deepEqual(outbox.listPendingFinalizeGenerations(), [], "successful handoff retired original first");
      await replace();
    }
    await w.sweepPendingFinalizes();
    assert.deepEqual(outbox.listPendingFinalizeGenerations(), [entry(RUN, 2)], "replacement retains");
    await reworkWorker(outbox, terminal).sweepPendingFinalizes();
    assert.deepEqual(outbox.listPendingFinalizeGenerations(), []);
  });
}

it("rework-2464: accepted register retires existing lower but retains later lower higher other-run records", async () => {
  const { outbox } = await rig();
  await outbox.journalFinalize(RUN, 1);
  await outbox.journalFinalize(RUN, 2);
  const w = reworkWorker(outbox, running, async () => false, async () => {
    await outbox.journalFinalize(RUN, 0);
    await outbox.journalFinalize(RUN, 3);
    await outbox.journalFinalize(RUN2, 1);
    return {};
  });
  await w.registerWithRetry(signal());
  await w.sweepPendingFinalizes();
  assert.deepEqual(outbox.listPendingFinalizeGenerations(), [entry(RUN, 0), entry(RUN, 3), entry(RUN2)],
    "captured G1 and offered G2 retire; later originals retain");
  await reworkWorker(outbox, terminal).sweepPendingFinalizes();
  assert.deepEqual(outbox.listPendingFinalizeGenerations(), []);
});

it("rework-2464: records created after accepted registration receive no handoff", async () => {
  const { outbox } = await rig();
  await outbox.journalFinalize(RUN, 1);
  const w = reworkWorker(outbox, running);
  await w.registerWithRetry(signal());
  assert.deepEqual(outbox.listPendingFinalizeGenerations(), []);
  await outbox.journalFinalize(RUN, 2);
  await outbox.journalFinalize(RUN2, 1);
  await w.sweepPendingFinalizes();
  assert.deepEqual(outbox.listPendingFinalizeGenerations(), [entry(RUN, 2), entry(RUN2)],
    "later generations and other runs retain without terminal proof");
});

for (const [label, response] of [
  ["missing status", { claim_generation: 2 }],
  ["malformed status", { status: 7, claim_generation: 2 }],
  ["active", { status: "running", claim_generation: 2 }],
  ["pending", { status: "awaiting_approval", claim_generation: 2 }],
  ["unknown", { status: "unknown", claim_generation: 2 }],
  ["missing generation", { status: "completed" }],
  ["old generation", { status: "completed", claim_generation: 0 }],
  ["fraction", { status: "completed", claim_generation: 1.5 }],
  ["negative", { status: "completed", claim_generation: -1 }],
  ["unsafe", { status: "completed", claim_generation: Number.MAX_SAFE_INTEGER + 1 }],
  ["string generation", { status: "completed", claim_generation: "2" }],
  ["malformed guarded", { status: "completed", claim_generation: 2, inventory_guarded: "false" }],
  ["null", null],
  ["404", "404"],
  ["transport", "transport"],
] as const) {
  it(`rework-2464: unoffered ${label} ownership retains`, async () => {
    const { outbox } = await rig();
    await outbox.journalFinalize(RUN, 1);
    const w = reworkWorker(outbox, async () => {
      if (response === "404") throw new RequestError("GET", "/api/worker/run/ownership", 404, "not owned");
      if (typeof response === "string") throw new Error(response);
      return response;
    });
    await w.sweepPendingFinalizes();
    assert.deepEqual(outbox.listPendingFinalizeGenerations(), [entry()], "unknown/nonterminal retains");
  });
}

for (const via of ["boot", "live"] as const) {
  it(`rework-2464: stale ${via} terminal resolution cannot delete nonterminal legacy finalize`, async () => {
    const { outbox, root } = await rig();
    const custody = await realRecoveryCoordinator(root);
    await outbox.journalFinalize(RUN, 1);
    await outbox.journalTerminal(RUN, 1, "running", 0, { status: "completed" });
    const w = reworkWorker(outbox, running, custody);
    // Local supersession is report retirement only.
    const c = (w as unknown as { client: WorkerClient }).client;
    c.reportState = async () => ({ applied: false, status: "running", staleClaim: true }) as never;
    if (via === "boot") await w.resolveBootTerminals(signal());
    else await w.sweepPendingTerminals(signal());
    assert.equal(outbox.hasPendingTerminal(RUN, 1), false, "stale terminal report retired");
    assert.deepEqual(outbox.listPendingFinalizeGenerations(), [entry()], "running finalize retains");
  });
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
  // Positive control: the same rig retires on custody false, so the refusals above are not vacuous.
  const control = fakeOutbox([entry()]);
  let checks = 0;
  const countingRunner = { recoveryInventoryPending: async () => { checks++; return false; } };
  await worker(control.outbox, countingRunner).sweepPendingFinalizes();
  assert.equal(control.deleted.length, 1, "custody false retires");
  assert.equal(checks, 1, "complete outbox consults custody");
  checks = 0;
  for (const partial of [
    {},
    { listPendingFinalizeGenerations: () => [entry()] },
    { retireFinalizeIfEligible: async () => {} },
  ]) {
    await worker(partial as unknown as Outbox, countingRunner).sweepPendingFinalizes();
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
