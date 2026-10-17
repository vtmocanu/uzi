import assert from "node:assert/strict";
import { createServer } from "node:http";
import { performance } from "node:perf_hooks";
import { test } from "node:test";
import { WorkerClient } from "../src/client.js";
import { WorkerMemoryRuntime } from "../src/worker-memory-runtime.js";
import type { MemoryOutcomeRequest, MemoryReservationRequest, WorkerMemoryPressureResult } from "../src/protocol.js";
import { nullLogger } from "./helpers.js";

const run = "11111111-1111-4111-8111-111111111111";
const worker = "22222222-2222-4222-8222-222222222222";
const settings = { enabled: true as const, reserveBytes: 100, hysteresisBytes: 100,
  sampleMs: 5, responseBudgetMs: 200, rearmMs: 25, maxInterventions: 2 };

async function until(predicate: () => boolean) {
  const deadline = Date.now() + 4000;
  while (!predicate()) {
    assert.ok(Date.now() < deadline, "native fixture bounded wait");
    await new Promise(resolve => setTimeout(resolve, 2));
  }
}

async function boundedCleanup(name: string, work: () => Promise<unknown>) {
  let timer: ReturnType<typeof setTimeout> | undefined;
  try {
    await Promise.race([
      Promise.resolve().then(work),
      new Promise<never>((_resolve, reject) => {
        timer = setTimeout(() => reject(new Error(`unresolved fixture cleanup: ${name}`)), 4000);
      }),
    ]);
  } finally {
    clearTimeout(timer);
  }
}

for (const mode of ["lost-response", "failure", "malformed", "parked-replay", "unconfirmed", "retired-late", "ignored-abort"] as const) {
  test(`native client/runtime: ${mode}, exact historical fixture replay with one charge`, async () => {
    let nonce = "original-nonce";
    const reserves: MemoryReservationRequest[] = [];
    const reports: MemoryOutcomeRequest[] = [];
    // Fake server storage models immutable same-outcome replay, not Go or DB acceptance.
    const stored = new Map<string, MemoryOutcomeRequest>();
    let releaseLate: (() => void) | undefined;
    const server = createServer(async (req, res) => {
      const chunks: Buffer[] = [];
      let receivedBytes = 0;
      for await (const chunk of req) {
        receivedBytes += typeof chunk === "string" ? Buffer.byteLength(chunk) : chunk.byteLength;
        if (receivedBytes > 16 * 1024) {
          req.destroy();
          return;
        }
        chunks.push(Buffer.from(chunk));
      }
      const body = JSON.parse(Buffer.concat(chunks).toString()) as MemoryReservationRequest & MemoryOutcomeRequest;
      res.setHeader("content-type", "application/json");
      if (req.url?.endsWith("/register")) {
        res.end(JSON.stringify({ worker_id: worker, register_nonce: nonce,
          protocol_features: ["worker_memory_pressure_v1"] }));
        return;
      }
      const policy = { version: 1 as const, max_interventions: 2 };
      const allowance = { limit: 2, used: 1, remaining: 1 };
      if (req.url?.endsWith("/reserve")) {
        reserves.push(body);
        res.end(JSON.stringify({ ...body, policy, allowance, admitted: true, authorizing: true }));
        return;
      }
      assert.ok(req.url?.endsWith("/outcome"));
      reports.push(body);
      const prior = stored.get(body.intervention_id);
      if (prior) assert.deepEqual(body, prior, "same immutable binding/ID/outcome");
      else stored.set(body.intervention_id, body);
      const reply = () => res.end(JSON.stringify({ ...body, policy, allowance,
        admitted: true, authorizing: false }));
      if (mode === "lost-response" && reports.length === 1) { res.destroy(); return; }
      if (mode === "failure" || (mode === "parked-replay" && reports.length <= 3)) {
        res.statusCode = 503; res.end("{}"); return;
      }
      if (mode === "malformed") { res.end(JSON.stringify({ ...body, policy, allowance,
        admitted: true, authorizing: true })); return; }
      if (mode === "retired-late" && reports.length === 1) { releaseLate = reply; return; }
      reply();
    });
    let releaseProducer: (() => void) | undefined;
    let producerDone: Promise<unknown> | undefined;
    let nativeAckHeld = false;
    let runtimeOwner: WorkerMemoryRuntime | undefined;
    let running: Promise<void> | undefined;
    const failures: unknown[] = [];
    try {
      await new Promise<void>((resolve, reject) => {
        server.once("error", reject);
        server.listen(0, "127.0.0.1", resolve);
      });
      const address = server.address();
      assert.ok(address && typeof address !== "string");
      const client = new WorkerClient(`http://127.0.0.1:${address.port}`, "fixture", "test", nullLogger());
      await client.register("fixture");
      if (mode === "ignored-abort") {
        // This adapter deliberately holds a real HTTP ACK past abort. It models an
        // uncooperative reporting port; the other cases use the native client unchanged.
        const nativeReport = client.reportMemoryInterventionOutcome.bind(client);
        const held = new Promise<void>(resolve => { releaseProducer = resolve; });
        client.reportMemoryInterventionOutcome = (...args) => {
          const producer = nativeReport(...args).then(async ack => {
            nativeAckHeld = true;
            await held;
            return ack;
          });
          // Observe rejection immediately; body/cleanup still await the original promise.
          void producer.catch(() => {});
          producerDone = producer;
          return producer;
        };
      }
      let sampleId = 0, usage = 0, signals = 0, prepared = 0, preserved = 0, feedback = 0;
      let originalResult: WorkerMemoryPressureResult | undefined;
      const runtime = new WorkerMemoryRuntime(settings, client, nullLogger(), {
        reader: { sample: () => {
          const now = performance.now();
          return { available: true, sampleId: ++sampleId, startedAtMs: now, completedAtMs: now,
            currentBytes: usage, limitBytes: 1000,
            stat: { anon: 1, shmem: 0, slab_unreclaimable: 0, unevictable: 0 },
            pressure: { some: { avg10: 0, avg60: 0, avg300: 0, total: 0 },
              full: { avg10: 0, avg60: 0, avg300: 0, total: 0 } } };
        } },
      });
      runtimeOwner = runtime;
      const input = { run_id: run, worker_id: worker, register_nonce: nonce,
        claim_generation: 1, memory_episode: 0, flight: {}, lifecycleRevision: 1,
        memory_policy: { version: 1 as const, max_interventions: 2 }, memory_intervention_count: 0 };
      const context = runtime.begin(input, {
        prepareFeedback: (_ctx, result) => { originalResult = result; },
        feedback: async result => { assert.equal(result, originalResult); feedback++; },
        preparePreservation: (ctx, result) => {
          assert.equal(ctx, context);
          assert.equal(result, originalResult);
          prepared++;
          // Regression: preparation can retire admission, but original preservation is owed.
          runtime.commands.retire(ctx);
        },
        preserve: async ctx => { assert.equal(ctx, context); preserved++; },
        preservationBudgetMs: 200,
      });
      const entry = runtime.commands.register(context, {
        source: "codex-model-command", toolId: "tool", attemptId: "attempt",
        command: "inert fixture", root: { pid: 10, startTime: "10" },
        observe: async () => {
          const now = performance.now();
          return { available: true, complete: true, startedAtMs: now, completedAtMs: now,
            rows: [{ pid: 10, startTime: "10", rssBytes: 100 }] };
        },
        cancel: async authority => { assert.equal(authority.authorized(), true); signals++; },
        drain: async () => ({ confirmed: mode !== "unconfirmed" }),
      });
      running = runtime.run();
      void running.catch(() => {});
      usage = 950;
      const result = await runtime.commands.watchOutcome(entry);
      assert.deepEqual(result, originalResult);
      assert.equal(result.outcome, mode === "unconfirmed" ? "unknown" : "confirmed_drained");
      assert.equal(result.cancellation_confirmed, mode !== "unconfirmed");
      await until(() => reports.length >= 1);
      if (mode === "ignored-abort") {
        await until(() => nativeAckHeld);
        const start = performance.now();
        await boundedCleanup("ignored-abort stop", () => runtime.stop());
        assert.ok(performance.now() - start < 1000, "stop does not join an uncooperative producer");
        const receipts = runtime.sinkReceipts.filter(receipt => receipt.kind === "report");
        assert.equal(receipts.length, 1);
        assert.equal(receipts[0]!.context, context);
        assert.ok(Object.isFrozen(receipts[0]));
        releaseProducer!();
        await boundedCleanup("released producer", async () => { await producerDone; });
        await until(() => runtime.sinkReceipts.length === 0);
      } else if (mode === "retired-late") {
        await until(() => releaseLate !== undefined);
        assert.ok(runtime.sinkReceipts.filter(receipt => receipt.kind === "report")
          .every(receipt => receipt.context === context));
        runtime.commands.retire(context);
        nonce = "successor-nonce";
        await client.register("fixture");
        const successor = runtime.begin({ ...input, register_nonce: nonce, flight: {}, lifecycleRevision: 2 }, {
          prepareFeedback: () => assert.fail("successor feedback"),
          feedback: async () => assert.fail("successor feedback"),
          preparePreservation: () => assert.fail("successor preservation"),
          preserve: async () => assert.fail("successor preservation"), preservationBudgetMs: 200,
        });
        const latch = runtime.commands.acquireLatch(successor)!;
        releaseLate!();
        await until(() => !runtime.sinkReceipts.some(receipt => receipt.kind === "report"));
        usage = 100;
        await new Promise(resolve => setTimeout(resolve, 70));
        assert.equal(runtime.commands.remaining(successor), 2);
        assert.equal(runtime.commands.isCurrent(successor), true);
        assert.equal(runtime.commands.admitsCommands(successor), false, "old proof cannot release successor latch");
        assert.equal(runtime.commands.releaseLatch(latch), true);
      } else if (mode === "failure" || mode === "malformed") {
        await until(() => reports.length === 4 && preserved === 1);
        assert.equal(prepared, 1);
        assert.equal(runtime.commands.isCurrent(context), false);
      } else if (mode === "parked-replay") {
        await until(() => reports.length === 4 && runtime.sinkReceipts.length === 0);
        assert.equal(prepared, 1);
        assert.equal(preserved, 1);
        assert.equal(runtime.commands.isCurrent(context), false);
        assert.equal(runtime.commands.admitsCommands(context), false, "historical proof cannot restore retired admission");
        assert.equal(runtime.commands.remaining(context), 1, "historical ACK cannot refund allowance");
      } else if (mode === "unconfirmed") {
        await until(() => preserved === 1);
        assert.equal(prepared, 1);
        assert.equal(runtime.commands.remaining(context), 1);
      } else {
        await until(() => reports.length === 2 && runtime.sinkReceipts.length === 0);
        assert.equal(runtime.commands.pressureHeld(context), true, "historical proof alone cannot rearm");
        usage = 100;
        await until(() => !runtime.commands.pressureHeld(context));
        assert.equal(prepared, 0);
        assert.equal(preserved, 0);
        assert.equal(runtime.commands.remaining(context), 1);
      }
      assert.equal(reserves.length, 1);
      assert.equal(stored.size, 1);
      assert.equal(signals, 1);
      assert.equal(feedback, 1);
    } catch (error) {
      failures.push(error);
    } finally {
      releaseLate?.();
      releaseProducer?.();
      try {
        // Each owner gets one bounded attempt; a failed owner does not block siblings.
        const cleanup = await Promise.allSettled([
          boundedCleanup("producer", async () => { await producerDone; }),
          boundedCleanup("runtime.stop", async () => { await runtimeOwner?.stop(); }),
          boundedCleanup("runtime.run", async () => { await running; }),
        ]);
        for (const result of cleanup) {
          if (result.status === "rejected") failures.push(result.reason);
        }
      } finally {
        server.closeAllConnections();
        try {
          await boundedCleanup("server.close", () => new Promise<void>((resolve, reject) =>
            server.close(error => error ? reject(error) : resolve())));
          assert.equal(runtimeOwner?.sinkReceipts.length ?? 0, 0, "unresolved fixture sink receipts");
        } catch (error) {
          failures.push(error);
        }
      }
    }
    if (failures.length === 1) throw failures[0];
    if (failures.length > 1) throw new AggregateError(failures, "native fixture and cleanup failed");
  });
}


for (const phase of ["absent-grow", "before-reserve", "pending-reserve", "after-ack", "drain", "feedback", "preserve"] as const) {
  test(`heartbeat memory lifecycle: ${phase} fences authority and retains captured settlement`, async () => {
    const feature = "worker_memory_pressure_v1";
    let features = phase === "absent-grow" ? [] : [feature];
    let nonce = "original-nonce";
    let reserves = 0, signals = 0, observations = 0, usage = 0, sampleId = 0;
    let reached = false, prepared = false, preserved = false;
    let original: WorkerMemoryPressureResult | undefined;
    const received: WorkerMemoryPressureResult[] = [];
    let release!: () => void;
    const held = new Promise<void>(done => { release = done; });
    const json = (value: unknown) => new Response(JSON.stringify(value), { status: 200 });
    const client = new WorkerClient("http://fixture.test", "fixture", "test", nullLogger(), {
      fetch: async (url, init) => {
        if (String(url).endsWith("/register")) return json({ worker_id: worker, register_nonce: nonce, protocol_features: features });
        if (String(url).endsWith("/heartbeat")) return json({ protocol_features: features });
        const body = JSON.parse(String(init?.body));
        if (String(url).endsWith("/reserve")) {
          reserves++;
          if (phase === "pending-reserve") { reached = true; await held; }
          return json({ ...body, admitted: true, authorizing: true,
            allowance: { limit: 2, used: phase === "preserve" ? 2 : 1, remaining: phase === "preserve" ? 0 : 1 } });
        }
        return json({ ...body, policy: { version: 1, max_interventions: 2 }, admitted: true, authorizing: false,
          allowance: { limit: 2, used: 1, remaining: 1 } });
      },
    });
    await client.register("fixture");
    const readIncarnation = () => client.memoryIncarnation;
    const runtime = new WorkerMemoryRuntime(settings, client, nullLogger(), {
      reader: { sample: () => {
        const now = performance.now();
        const psi = { avg10: 0, avg60: 0, avg300: 0, total: 0 };
        return { available: true, sampleId: ++sampleId, startedAtMs: now, completedAtMs: now,
          currentBytes: usage, limitBytes: 1000,
          stat: { anon: 1, shmem: 0, slab_unreclaimable: 0, unevictable: 0 }, pressure: { some: psi, full: psi } };
      } },
    });
    const context = runtime.begin({ run_id: run, worker_id: worker, register_nonce: nonce,
      claim_generation: 1, memory_episode: 0, flight: {}, lifecycleRevision: 1,
      memory_policy: { version: 1, max_interventions: 2 }, memory_intervention_count: 0 }, {
      prepareFeedback: (ctx, result) => { assert.equal(ctx, context); original = result; },
      feedback: async (result, ctx) => {
        assert.equal(ctx, context); assert.equal(result, original); received.push(result);
        if (phase === "feedback") { reached = true; await held; }
      },
      preparePreservation: (ctx, result) => {
        assert.equal(ctx, context); assert.equal(result, original); prepared = true;
      },
      preserve: async ctx => {
        assert.equal(ctx, context); preserved = true;
        if (phase === "preserve") { reached = true; await held; }
      }, preservationBudgetMs: 200,
    });
    const entry = runtime.commands.register(context, {
      source: "codex-model-command", toolId: "captured-tool", attemptId: "captured-attempt",
      command: "inert fixture", root: { pid: 10, startTime: "10" },
      observe: async () => {
        observations++;
        if (phase === "after-ack" && observations === 2) { reached = true; await held; }
        const now = performance.now();
        return { available: true, complete: true, startedAtMs: now, completedAtMs: now,
          rows: [{ pid: 10, startTime: "10", rssBytes: 100 }] };
      },
      cancel: async authority => { assert.equal(authority.authorized(), true); signals++; },
      drain: async () => {
        if (phase === "drain") { reached = true; await held; }
        return { confirmed: true };
      },
    });
    if (phase === "before-reserve") { features = []; await client.heartbeat(); }
    if (phase === "absent-grow") {
      features = [feature]; await client.heartbeat();
      assert.equal(client.memoryIncarnation, undefined, "heartbeat growth cannot seed authority");
    }
    const running = runtime.run();
    try {
      usage = 950;
      if (phase === "before-reserve" || phase === "absent-grow") {
        await until(() => sampleId >= 4);
        assert.equal(reserves, 0); assert.equal(signals, 0);
      } else {
        await until(() => reached);
        features = []; await client.heartbeat();
        assert.equal(client.memoryIncarnation, undefined);
        assert.equal(runtime.commands.isCurrent(context), false, "shrink retires authority synchronously");
        if (phase === "feedback" || phase === "preserve") {
          assert.ok(runtime.sinkReceipts.some(r => r.context === context && r.kind === phase), "pending original sink stays owned");
        }
        release();
        const result = await runtime.commands.watchOutcome(entry);
        assert.deepEqual(result, original);
        assert.equal(result.command_id, entry.commandId);
        assert.equal(result.tool_id, entry.toolId);
        assert.equal(result.attempt_id, entry.attemptId);
        await until(() => received.length === 1);
        if (["drain", "feedback", "preserve"].includes(phase)) {
          await until(() => prepared && preserved);
          assert.equal(signals, 1);
        } else assert.equal(signals, 0, "late ACK and observation cannot authorize signal");
      }
      const priorSignals = signals, priorReserves = reserves;
      features = [feature]; await client.heartbeat();
      assert.equal(client.memoryIncarnation, undefined);
      await until(() => sampleId >= 8);
      assert.equal(signals, priorSignals); assert.equal(reserves, priorReserves);
      assert.equal(runtime.commands.admitsCommands(context), phase === "absent-grow");
      nonce = "fresh-nonce";
      await client.register("fixture");
      assert.equal(readIncarnation()?.register_nonce, nonce, "fresh register seeds a new incarnation");
      const fresh = runtime.begin({ ...context, register_nonce: nonce, flight: {}, lifecycleRevision: 2 }, {
        prepareFeedback: () => {}, feedback: async () => {}, preparePreservation: () => {}, preserve: async () => {}, preservationBudgetMs: 200,
      });
      assert.equal(runtime.commands.admitsCommands(fresh), true);
      assert.equal(runtime.commands.isCurrent(context), false);
    } finally {
      release(); await runtime.stop(); await running;
    }
  });
}
