import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { WorkerClient, type InputReceipt } from "../src/client.js";
import { ChatSteering, SteeringChannel } from "../src/steering.js";
import { MaintenanceBudget } from "../src/maintenance-budget.js";
import type { UserInput } from "../src/protocol.js";
import { nullLogger } from "./helpers.js";

const RUN = "11111111-1111-4111-8111-111111111111";
const OTHER = "22222222-2222-4222-8222-222222222222";
const WORKER = "33333333-3333-4333-8333-333333333333";
function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((r) => { resolve = r; });
  return { promise, resolve };
}
function client(fetch: typeof globalThis.fetch, httpTimeoutMs = 50) {
  return new WorkerClient("http://example.com", "fixture-worker-token", "test", nullLogger(), { fetch, httpTimeoutMs });
}
async function until(ready: () => boolean) {
  for (let i = 0; i < 500 && !ready(); i++) await new Promise((r) => setTimeout(r, 2));
  assert.ok(ready(), "condition reached within one second");
}
const control = (generation = 3, run_id = RUN) => ({ run_id, generation });
const cancel: UserInput = { id: 1, kind: "cancel", body: null } as UserInput;

describe("terminal cancelled input transport", () => {
  it("validates identity and positive safe generation; missing control supports old APIs", async () => {
    for (const value of [undefined, null, {}, [], control(0), control(-1), control(1.5),
      control(Number.MAX_SAFE_INTEGER + 1), control(3, OTHER), control(3, "bad"),
      { run_id: RUN, generation: "3" }, control()]) {
      const c = client(async () => Response.json({ inputs: [], terminal_cancelled: value }));
      const reply = await c.getInputs(RUN);
      assert.deepEqual(reply.terminalCancelled, value && (value as { run_id?: string }).run_id === RUN &&
        (value as { generation?: number }).generation === 3 ? control() : undefined);
    }
  });

  for (const registration of ["pending", "failed", "success"] as const) {
    it(`discards all facts from a GET started before same-ID registration: ${registration}`, async () => {
      const response = deferred<Response>();
      const registerReply = deferred<Response>();
      let registrations = 0;
      const c = client(async (url) => {
        if (String(url).endsWith("/register")) {
          if (++registrations === 1) return Response.json({ worker_id: WORKER });
          return registerReply.promise;
        }
        return response.promise;
      });
      await c.register("fixture");
      const get = c.getInputs(RUN);
      const registering = c.register("fixture");
      const settled = registering.catch(() => undefined);
      if (registration === "success") registerReply.resolve(Response.json({ worker_id: WORKER }));
      if (registration === "failed") registerReply.resolve(new Response("failure", { status: 400 }));
      if (registration !== "pending") await settled;
      response.resolve(Response.json({ inputs: [cancel], terminal_cancelled: control(),
        credential_switch: { generation: 3 }, receipts: true, now_summary: true }));
      assert.deepEqual(await get, { inputs: [], staleObservation: true });
      if (registration === "pending") registerReply.resolve(Response.json({ worker_id: WORKER }));
      await settled;
    });
  }

  for (const status of [404, 500]) {
    it(`ignores stale HTTP ${status} but throws a current failure`, async () => {
      const response = deferred<Response>();
      let reads = 0;
      const c = client(async (url) => String(url).endsWith("/register")
        ? Response.json({ worker_id: WORKER }) : ++reads === 1 ? response.promise : new Response("current failure", { status }));
      const old = c.getInputs(RUN);
      await c.register("fixture");
      response.resolve(new Response("old failure", { status }));
      assert.deepEqual(await old, { inputs: [], staleObservation: true });
      await assert.rejects(c.getInputs(RUN), { status });
    });
  }

  for (const endpoint of ["get", "ack", "applied", "included"] as const) {
    it(`bounds an injected stalled ${endpoint} response body`, async () => {
      let cancelled = false;
      const c = client(async () => new Response(new ReadableStream({
        start(controller) { controller.enqueue(new TextEncoder().encode("{")); },
        cancel() { cancelled = true; },
      })), 10);
      const operation = endpoint === "get" ? c.getInputs(RUN) : endpoint === "ack" ? c.ackInputs(RUN, [1], 3)
        : endpoint === "applied" ? c.applyInputs(RUN, [1], 3) : c.includeInputs(RUN, [1], 3);
      let timer: NodeJS.Timeout | undefined;
      try {
        const result = await Promise.race([
          operation.then(() => "resolved", () => "deadline"),
          new Promise<string>((r) => { timer = setTimeout(() => r("hung"), 150); }),
        ]);
        assert.equal(result, "deadline");
        assert.equal(cancelled, true, "the outstanding body is cancelled");
      } finally { clearTimeout(timer); }
    });
  }
});

describe("terminal cancelled steering", () => {
  it("exact terminal cancellation initializes one maintenance deadline before abort continuations", async () => {
    let now = 10;
    let factories = 0;
    let budget: MaintenanceBudget | undefined;
    let observedAtAbort: MaintenanceBudget | undefined;
    let fenceAtAbort: string | undefined;
    const shared = new AbortController();
    const c = client(async () => Response.json({ inputs: [], terminal_cancelled: control() }));
    const ch = new SteeringChannel(c, RUN, 1, nullLogger(), shared, {
      claimGeneration: 3, onCancel: () => { MaintenanceBudget.observeCancellation(); },
    });
    const aborted = new Promise<void>(resolve => shared.signal.addEventListener("abort", () => {
      observedAtAbort = MaintenanceBudget.current();
      fenceAtAbort = ch.claimFence();
      resolve();
    }, { once: true }));
    try {
      await MaintenanceBudget.runAttempt(`${RUN}:3`, () => {
        factories++;
        return budget = new MaintenanceBudget(600_000, () => now);
      }, async () => {
        ch.start();
        await aborted;
        await ch.stop();
        assert.equal(factories, 1, "exact terminal cancellation must initialize the shared budget");
        assert.equal(observedAtAbort, budget, "budget precedes synchronous abort continuations");
        assert.equal(fenceAtAbort, "terminal_cancelled", "the fence precedes the observer");
        assert.equal(ch.claimFence(), "terminal_cancelled");
        assert.equal(ch.isCancelled(), false, "the control is still a fence, not a synthetic input");
        now += 100;
        assert.equal(MaintenanceBudget.observeCancellation(), budget);
        assert.equal(budget!.deadline, 600_010, "another observation cannot renew the deadline");
        now = 600_011;
        assert.throws(() => MaintenanceBudget.observeCancellation(), /maintenance deadline exhausted/);
        assert.equal(factories, 1);
      });
    } finally { await ch.stop(); budget?.dispose(); }
  });

  for (const generation of [0, 3]) {
    it(`chat generation ${generation} ignores non-chat control and delivers a message`, async () => {
      const shared = new AbortController();
      const c = client(async () => Response.json({ inputs: [{ id: 9, kind: "follow_up", body: "hello" }],
        terminal_cancelled: control() }));
      const ch = new ChatSteering(c, RUN, 1, nullLogger(), shared, {}, generation);
      const message = ch.awaitFollowUp(100_000);
      ch.start();
      try {
        assert.deepEqual(await message, { kind: "message", text: "hello", id: 9 });
        assert.equal(ch.claimLost(), false);
        assert.equal(shared.signal.aborted, false);
      } finally { await ch.stop(); }
    });
  }

  it("zero-generation claim ignores control and handles normal input", async () => {
    const c = client(async () => Response.json({ inputs: [cancel], terminal_cancelled: control() }));
    const ch = new SteeringChannel(c, RUN, 1, nullLogger(), new AbortController(), { claimGeneration: 0 });
    const verdict = ch.awaitVerdict();
    ch.start();
    try {
      assert.deepEqual(await verdict, { kind: "cancel" });
      assert.equal(ch.claimFence(), undefined);
    } finally { await ch.stop(); }
  });

  it("skips stale observations before summary or initial-delivery facts", async () => {
    const next = deferred<Awaited<ReturnType<WorkerClient["getInputs"]>>>();
    let reads = 0;
    const c = { getInputs: () => ++reads === 1
      ? Promise.resolve({ inputs: [], staleObservation: true, nowSummary: true }) : next.promise } as unknown as WorkerClient;
    const ch = new SteeringChannel(c, RUN, 1, nullLogger(), new AbortController(), { claimGeneration: 3 });
    ch.guardInitialDelivery();
    let delivered = false;
    const delivery = ch.awaitInitialDelivery().then(() => { delivered = true; });
    ch.start();
    try {
      await until(() => reads === 2);
      assert.equal(ch.nowSummaryEnabled(), false);
      assert.equal(delivered, false);
      next.resolve({ inputs: [] });
      await delivery;
    } finally { next.resolve({ inputs: [] }); await ch.stop(); }
  });

  for (const stage of ["ack", "applied"] as const) {
    it(`inactive stale held ${stage} ends flight without synthetic cancel`, async () => {
      const c = {
        getInputs: async () => ({ inputs: [{ id: 7, kind: "follow_up", body: "once" }], receipts: true }),
        ackInputs: async () => ({ ids: [7], inputs: [{ id: 7, kind: "follow_up", body: "once" }],
          active: stage !== "ack", reason: "stale" }),
        applyInputs: async () => ({ ids: [7], inputs: [{ id: 7, kind: "follow_up", body: "once" }], active: false, reason: "stale" }),
      } as unknown as WorkerClient;
      const ch = new SteeringChannel(c, RUN, 1, nullLogger(), new AbortController(), { claimGeneration: 3 });
      const plan = assert.rejects(ch.awaitVerdict(), { name: "ClaimFencedSignal" });
      const question = assert.rejects(ch.awaitAnswer("q"), { name: "ClaimFencedSignal" });
      ch.start();
      try {
        await Promise.all([plan, question]);
        assert.equal(ch.claimFence(), "stale");
        assert.equal(ch.isCancelled(), false);
        assert.deepEqual(ch.operatorConstraints(), stage === "ack" ? [] : ["once"]);
      } finally { await ch.stop(); }
    });

    it(`ignores late active held ${stage} after terminal lifecycle stop`, async () => {
      const entered = deferred<void>();
      const late = deferred<InputReceipt>();
      const c = {
        getInputs: async () => ({ inputs: [cancel], receipts: true }),
        ackInputs: async () => {
          if (stage === "ack") { entered.resolve(); return late.promise; }
          return { ids: [1], inputs: [{ id: 1, kind: "follow_up", body: "once" }], active: true };
        },
        applyInputs: async () => { entered.resolve(); return late.promise; },
      } as unknown as WorkerClient;
      const ch = new SteeringChannel(c, RUN, 1, nullLogger(), new AbortController(), { claimGeneration: 3 });
      ch.start();
      try {
        await entered.promise;
        ch.abortLifecycle(new Error("terminal stop"));
        late.resolve({ inputs: [cancel], active: true });
        await ch.stop();
        assert.equal(ch.isCancelled(), false);
        assert.equal(ch.terminalLifecycleSignal().aborted, true);
      } finally { late.resolve({ inputs: [], active: true }); await ch.stop(); }
    });
  }

  it("plan-gate APPLIED terminal fence rejects parked waiters", { timeout: 5_000 }, async () => {
    const c = {
      getInputs: async () => ({ inputs: [{ id: 7, kind: "revise_plan", body: "revise" }], receipts: true }),
      ackInputs: async () => ({ ids: [7], inputs: [{ id: 7, kind: "revise_plan", body: "revise" }], active: true }),
      applyInputs: async () => ({ ids: [7], inputs: [{ id: 7, kind: "revise_plan", body: "revise" }], active: false, reason: "stale" }),
    } as unknown as WorkerClient;
    const ch = new SteeringChannel(c, RUN, 1, nullLogger(), new AbortController(), { claimGeneration: 3 });
    const revision = ch.awaitGateEvent(ch.currentEpoch());
    ch.start();
    try {
      assert.equal((await revision).kind, "revise");
      const plan = assert.rejects(ch.awaitVerdict(), { name: "ClaimFencedSignal" });
      const question = assert.rejects(ch.awaitAnswer("q"), { name: "ClaimFencedSignal" });
      ch.settleRevision(7);
      await Promise.all([plan, question]);
      assert.equal(ch.claimFence(), "stale");
      assert.equal(ch.isCancelled(), false);
    } finally { await ch.stop(); }
  });

  for (const stage of ["get", "ack", "applied"] as const) {
    it(`late ACTIVE ${stage} cannot revive a terminal-fenced channel`, { timeout: 5_000 }, async () => {
      const entered = deferred<void>();
      const lateGet = deferred<Awaited<ReturnType<WorkerClient["getInputs"]>>>();
      const lateReceipt = deferred<InputReceipt>();
      const revise: UserInput = { id: 7, kind: "revise_plan", body: "revise" };
      const follow: UserInput = { id: 8, kind: "follow_up", body: "once" };
      let gets = 0;
      let heldApplies = 0;
      const c = {
        getInputs: async () => {
          if (++gets === 1) return { inputs: [revise], receipts: true };
          if (stage === "get") { entered.resolve(); return lateGet.promise; }
          return { inputs: [follow], receipts: true };
        },
        ackInputs: async (_run: string, ids: number[]) => {
          if (ids[0] === 8 && stage === "ack") { entered.resolve(); return lateReceipt.promise; }
          return { inputs: ids[0] === 7 ? [revise] : [follow], active: true };
        },
        applyInputs: async (_run: string, ids: number[]) => {
          if (ids[0] === 7) return { inputs: [revise], active: false, reason: "stale" };
          heldApplies++;
          entered.resolve();
          return lateReceipt.promise;
        },
      } as unknown as WorkerClient;
      const ch = new SteeringChannel(c, RUN, 1, nullLogger(), new AbortController(), { claimGeneration: 3 });
      const revision = ch.awaitGateEvent(ch.currentEpoch());
      ch.start();
      try {
        assert.equal((await revision).kind, "revise");
        await entered.promise;
        const fenced = assert.rejects(ch.awaitAnswer("q"), { name: "ClaimFencedSignal" });
        ch.settleRevision(7);
        await fenced;
        const before = ch.operatorConstraints();
        const getCount = gets;
        lateGet.resolve({ inputs: [cancel], nowSummary: true });
        lateReceipt.resolve({ inputs: [stage === "ack" ? cancel : follow], active: true } as InputReceipt);
        await ch.stop();
        assert.equal(ch.claimFence(), "stale");
        assert.equal(ch.isCancelled(), false);
        assert.equal(ch.nowSummaryEnabled(), false);
        assert.deepEqual(ch.operatorConstraints(), before);
        assert.equal(gets, getCount, "no new GET after fence");
        assert.equal(heldApplies, stage === "applied" ? 1 : 0);
      } finally {
        lateGet.resolve({ inputs: [] });
        lateReceipt.resolve({ inputs: [follow], active: true } as InputReceipt);
        await ch.stop();
      }
    });
  }

  for (const lane of ["ready", "discarded"] as const) {
    it(`late plan ${lane} receipt cannot replace an existing terminal fence`, { timeout: 5_000 }, async (t) => {
      const entered = deferred<void>();
      const late = deferred<InputReceipt>();
      const rows: UserInput[] = lane === "ready"
        ? [{ id: 7, kind: "revise_plan", body: "revise" }]
        : [{ id: 7, kind: "approve_plan", body: null }, { id: 8, kind: "approve_plan", body: null }];
      let gets = 0;
      const c = {
        getInputs: async () => {
          if (++gets === 1) return { inputs: rows, receipts: true };
          await entered.promise;
          return { inputs: [], terminalCancelled: control() };
        },
        ackInputs: async () => ({ inputs: rows, active: true }),
        applyInputs: async () => { entered.resolve(); return late.promise; },
        discardInputs: async () => { entered.resolve(); return late.promise; },
      } as unknown as WorkerClient;
      const ch = new SteeringChannel(c, RUN, 1, nullLogger(), new AbortController(), { claimGeneration: 3 });
      t.after(async () => {
        entered.resolve();
        late.resolve({ inputs: [rows[0]!], active: false, reason: "stale" });
        await ch.stop();
      });
      const fenced = assert.rejects(ch.awaitAnswer("q"), { name: "ClaimFencedSignal" });
      const revision = lane === "ready" ? ch.awaitGateEvent(ch.currentEpoch()) : undefined;
      ch.start();
      if (revision) {
        assert.equal((await revision).kind, "revise");
        ch.settleRevision(7);
      }
      await fenced;
      assert.equal(ch.claimFence(), "terminal_cancelled");
      late.resolve({ inputs: [rows[0]!], active: false, reason: "stale" });
      await ch.stop();
      assert.equal(ch.claimFence(), "terminal_cancelled", "the old receipt cannot replace the terminal observation");
      assert.equal(ch.isCancelled(), false);
    });
  }

  it("receipt timer fails a parked plan with InputReceiptError without claiming a server fence", async () => {
    const applying = deferred<void>();
    const response = deferred<InputReceipt>();
    const c = {
      getInputs: async () => ({ inputs: [{ id: 7, kind: "follow_up", body: "once" }], receipts: true }),
      ackInputs: async () => ({ ids: [7], inputs: [{ id: 7, kind: "follow_up", body: "once" }], active: true }),
      applyInputs: async () => { applying.resolve(); return response.promise; },
    } as unknown as WorkerClient;
    const ch = new SteeringChannel(c, RUN, 1, nullLogger(), new AbortController(),
      { claimGeneration: 3, receiptDeadlineMs: 20 });
    const plan = assert.rejects(ch.awaitVerdict(), { name: "InputReceiptError" });
    ch.start();
    try {
      await applying.promise;
      const settlement = ch.awaitReceiptSettlement().catch(() => {});
      await plan;
      await settlement;
      assert.equal(ch.claimFence(), undefined);
      assert.equal((ch.terminalLifecycleSignal().reason as Error).name, "InputReceiptError");
      assert.equal(ch.isCancelled(), false);
      await assert.rejects(ch.awaitReceiptSettlement(), { name: "InputReceiptError" });
    } finally { response.resolve({ inputs: [], active: true }); await ch.stop(); }
  });

  it("fences before inputs/switch/summary facts and rejects plan and question waiters", async () => {
    const reply = deferred<Awaited<ReturnType<WorkerClient["getInputs"]>>>();
    const c = { getInputs: () => reply.promise } as unknown as WorkerClient;
    const shared = new AbortController();
    const ch = new SteeringChannel(c, RUN, 1, nullLogger(), shared, { claimGeneration: 3 });
    const plan = assert.rejects(ch.awaitVerdict(), { name: "ClaimFencedSignal" });
    const question = assert.rejects(ch.awaitAnswer("q"), { name: "ClaimFencedSignal" });
    ch.start();
    reply.resolve({ inputs: [cancel], terminalCancelled: control(), credentialSwitch: { generation: 3 }, nowSummary: true });
    await Promise.all([plan, question]);
    await ch.stop();
    assert.equal(ch.claimFence(), "terminal_cancelled");
    assert.equal(ch.terminalLifecycleSignal().aborted, true);
    assert.equal(shared.signal.aborted, true);
    assert.equal(ch.isCancelled(), false);
    assert.equal(ch.pendingCredentialSwitch(), undefined);
    assert.equal(ch.nowSummaryEnabled(), false);
  });

  for (const wrong of [control(2), control(4), control(3, OTHER)]) {
    it(`ignores another flight's control ${JSON.stringify(wrong)}`, async () => {
      const c = { getInputs: async () => ({ inputs: [cancel], terminalCancelled: wrong }) } as unknown as WorkerClient;
      const ch = new SteeringChannel(c, RUN, 1, nullLogger(), new AbortController(), { claimGeneration: 3 });
      const verdict = ch.awaitVerdict();
      ch.start();
      assert.deepEqual(await verdict, { kind: "cancel" });
      await ch.stop();
      assert.equal(ch.claimFence(), undefined);
    });
  }

  it("ignores a late GET after lifecycle fencing", async () => {
    const reply = deferred<Awaited<ReturnType<WorkerClient["getInputs"]>>>();
    const c = { getInputs: () => reply.promise } as unknown as WorkerClient;
    const ch = new SteeringChannel(c, RUN, 1, nullLogger(), new AbortController(), { claimGeneration: 3 });
    ch.start();
    // Exercise the public stable lifecycle stop while the GET is outstanding.
    ch.abortLifecycle(new Error("terminal stop"));
    reply.resolve({ inputs: [cancel], nowSummary: true });
    await ch.stop();
    assert.equal(ch.isCancelled(), false);
  });
});
