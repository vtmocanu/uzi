import assert from "node:assert/strict";
import test from "node:test";
import { WorkerClient, readRunAck } from "../src/client.js";
import { MaintenanceBudget } from "../src/maintenance-budget.js";
import { MaintenanceResources } from "../src/recovery-maintenance.js";
import { nullLogger } from "./helpers.js";

const tick = () => new Promise<void>(resolve => setImmediate(resolve));
function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>(r => { resolve = r; });
  return { promise, resolve };
}

for (const wall of [-9_000_000, 9_000_000_000]) {
  test(`cumulative headers and body reject at 600k despite wall ${wall}`, async t => {
    let now = 0, calls = 0;
    const budget = new MaintenanceBudget(600_000, () => now);
    t.after(() => budget.dispose());
    t.mock.method(Date, "now", () => wall);
    const client = new WorkerClient("http://budget.test", "token", "test", nullLogger(), {
      fetch: async () => {
        calls++;
        now = 300_000;
        return new Response(new ReadableStream<Uint8Array>({
          pull(controller) {
            now = 600_000;
            controller.enqueue(new TextEncoder().encode('{"holds_released":1}'));
            controller.close();
          },
        }, { highWaterMark: 0 }));
      },
    });
    await MaintenanceBudget.run(budget, async () => {
      await assert.rejects(client.releaseRecoveryCustody("run", 1, "publication", undefined), /maintenance deadline/);
      assert.equal(calls, 1);
    });
  });
}

test("late headers ignoring abort reject and detach a never-settling cancellation", async t => {
  let now = 0, calls = 0, cancelled = 0;
  const budget = new MaintenanceBudget(600_000, () => now);
  t.after(() => budget.dispose());
  const headers = deferred<Response>();
  const client = new WorkerClient("http://budget.test", "token", "test", nullLogger(), {
    fetch: async () => { calls++; return headers.promise; },
  });
  await MaintenanceBudget.run(budget, async () => {
    const waiting = client.releaseRecoveryCustody("run");
    await tick();
    now = 600_000;
    assert.throws(() => budget.check(), /maintenance deadline/);
    await assert.rejects(waiting, /maintenance deadline/);
    headers.resolve(new Response(new ReadableStream({
      cancel() { cancelled++; return new Promise(() => {}); },
    })));
    await tick();
    assert.equal(cancelled, 1);
    await assert.rejects(client.releaseRecoveryCustody("run"), /maintenance deadline/);
    assert.equal(calls, 1);
  });
});

const maintenanceWorkerId = "00000000-0000-4000-8000-0000000000aa";

for (const status of [200, 409]) {
  test(`terminal ACK above 6 MiB still refuses overflow on HTTP ${status}`, async () => {
    let calls = 0;
    const client = new WorkerClient("http://budget.test", "token", "test", nullLogger(), {
      fetch: async () => {
        calls++;
        return Response.json({ run: { status: "completed", issue_description: "x".repeat(6 * 1024 * 1024) } }, { status });
      },
    });
    await assert.rejects(client.reportState("run", { status: "completed" }), { name: "ResponseBodyOverflowError" });
    assert.equal(calls, 1, "overflow cannot manufacture an ACK or trigger terminal replay");
  });
}

for (const status of [200, 409]) {
  for (const terminal of ["completed", "failed"] as const) {
    test(`terminal ${terminal} preserves a RunDTO ACK above 1 MiB on HTTP ${status}`, async t => {
      const budget = new MaintenanceBudget();
      t.after(() => budget.dispose());
      let calls = 0;
      const client = new WorkerClient("http://budget.test", "token", "test", nullLogger(), {
        fetch: async () => {
          calls++;
          return Response.json({ run: {
            status: terminal, budget_max_iterations: 7, budget_wall_seconds: 3600,
            issue_description: "x".repeat(700 * 1024), preserved_patch: "p".repeat(500 * 1024),
          } }, { status });
        },
      });
      const ack = await MaintenanceBudget.run(budget, () => client.reportState("run", { status: terminal }));
      assert.equal(ack.applied, status === 200);
      assert.equal(ack.status, terminal);
      assert.equal(ack.budgetMaxIterations, 7);
      assert.equal(ack.budgetWallSeconds, 3600);
      assert.equal(calls, 1, "an applied or already-terminal response is not retried");
    });
  }
}

test("review C1 on-time hold body exercises the completion binding loop", async t => {
  const budget = new MaintenanceBudget(600_000, () => 0);
  t.after(() => budget.dispose());
  let registered = false;
  const client = new WorkerClient("http://budget.test", "token", "test", nullLogger(), {
    fetch: async () => {
      if (!registered) { registered = true; return Response.json({ worker_id: maintenanceWorkerId, protocol_features: [] }); }
      return Response.json({ run_id: "run", holds: [{ generation: 1, hold_id: "invalid", inventory_guarded: true }] });
    },
  });
  await client.register("worker");
  assert.equal(client.canStartPublicationCompletion("run", 1), true);
  await MaintenanceBudget.run(budget, () => client.listRecoveryHolds("run"));
  assert.equal(client.canStartPublicationCompletion("run", 1), false, "on-time invalid hold must poison bound provenance");
});

for (const operation of ["release", "holds", "ack"] as const) {
  test(`stalled ${operation} body and cancellation cannot hold the budget open`, async t => {
    let now = 0, cancelled = 0;
    const budget = new MaintenanceBudget(600_000, () => now);
    t.after(() => budget.dispose());
    const res = new Response(new ReadableStream({
      pull() { return new Promise(() => {}); },
      cancel() { cancelled++; return new Promise(() => {}); },
    }));
    const client = new WorkerClient("http://budget.test", "token", "test", nullLogger(), { fetch: async () => res });
    await MaintenanceBudget.run(budget, async () => {
      const pending = operation === "release" ? client.releaseRecoveryCustody("run") :
        operation === "holds" ? client.listRecoveryHolds("run") : readRunAck(res);
      await tick();
      now = 600_000;
      assert.throws(() => budget.check(), /maintenance deadline/);
      await assert.rejects(pending, /maintenance deadline/);
      assert.equal(cancelled, 1);
    });
  });
}

for (const operation of ["release", "holds", "ack"] as const) {
  test(`late valid ${operation} body grants no authority`, async t => {
    let now = 0;
    const budget = new MaintenanceBudget(600_000, () => now);
    t.after(() => budget.dispose());
    const read = deferred<{ done: false; value: Uint8Array }>();
    const res = new Response();
    Object.defineProperty(res, "body", { value: {
      getReader: () => ({ read: () => read.promise, cancel: () => new Promise(() => {}) }),
    } });
    let registered = false;
    const client = new WorkerClient("http://budget.test", "token", "test", nullLogger(), {
      fetch: async () => {
        if (!registered) { registered = true; return Response.json({ worker_id: maintenanceWorkerId, protocol_features: [] }); }
        return res;
      },
    });
    await client.register("worker");
    await MaintenanceBudget.run(budget, async () => {
      const pending = operation === "release" ? client.releaseRecoveryCustody("run") :
        operation === "holds" ? client.listRecoveryHolds("run") : readRunAck(res);
      await tick();
      now = 600_000;
      assert.throws(() => budget.check(), /maintenance deadline/);
      await assert.rejects(pending, /maintenance deadline/);
      read.resolve({ done: false, value: new TextEncoder().encode(JSON.stringify({
        run_id: "run", released: true, holds_released: 1,
        run: { status: "completed" }, holds: [{ generation: 1, hold_id: "invalid", inventory_guarded: true }],
      })) });
      await tick();
    });
    assert.equal(client.canStartPublicationCompletion("run", 1), true, "late inventory must not poison provenance");
  });
}

test("expired maintenance cannot mark a guarded claim settled synchronously", async t => {
  let now = 0;
  const budget = new MaintenanceBudget(600_000, () => now);
  t.after(() => budget.dispose());
  const client = new WorkerClient("http://budget.test", "token", "test", nullLogger());
  client["inventoryGuardedClaims"].add("run:1");
  await MaintenanceBudget.run(budget, async () => {
    now = 600_000;
    assert.throws(() => client.markInventoryGuardedClaimSettled("run", 1), /maintenance deadline/);
  });
  assert.equal(client.knowsInventoryGuardedClaim("run", 1), true);
});

test("a caller-aborted late 204 cannot pass the maintenance header fence", async t => {
  const budget = new MaintenanceBudget();
  t.after(() => budget.dispose());
  const abort = new AbortController();
  const client = new WorkerClient("http://budget.test", "token", "test", nullLogger(), {
    fetch: async () => { abort.abort(new Error("operation stopped")); return new Response(null, { status: 204 }); },
  });
  await MaintenanceBudget.run(budget, () => assert.rejects(
    client.releaseRecoveryCustody("run", 1, undefined, undefined, abort.signal), /operation stopped/));
});

test("explicit release signal also fences the bounded response body without an ambient budget", async () => {
  const abort = new AbortController();
  const client = new WorkerClient("http://budget.test", "token", "test", nullLogger(), {
    fetch: async () => new Response(new ReadableStream<Uint8Array>({
      pull(controller) {
        abort.abort(new Error("explicit release stopped"));
        controller.enqueue(new TextEncoder().encode('{"holds_released":1}'));
        controller.close();
      },
    }, { highWaterMark: 0 })),
  });
  await assert.rejects(client.releaseRecoveryCustody("run", 7, undefined, undefined, abort.signal), /explicit release stopped/);
});


for (const phase of ["headers", "body"] as const) {
  test(`caller abort interrupts uncooperative maintenance ${phase} before the absolute deadline`, async t => {
    let now = 0, state = "pending";
    const budget = new MaintenanceBudget(600_000, () => now);
    t.after(() => budget.dispose());
    const abort = new AbortController();
    const headers = deferred<Response>();
    const bytes = deferred<{ done: true; value: undefined }>();
    const res = new Response();
    Object.defineProperty(res, "body", { value: {
      getReader: () => ({ read: () => bytes.promise, cancel: () => new Promise(() => {}) }),
      cancel: () => new Promise(() => {}),
    } });
    const client = new WorkerClient("http://budget.test", "token", "test", nullLogger(), {
      fetch: async () => phase === "headers" ? headers.promise : res,
    });
    await MaintenanceBudget.run(budget, async () => {
      const pending = client.releaseRecoveryCustody("run", 7, undefined, undefined, abort.signal)
        .then(() => { state = "resolved"; }, () => { state = "rejected"; });
      await tick();
      abort.abort(new Error("short operation stopped"));
      await tick();
      const atCallerAbort = state;
      const absoluteStillOpen = !budget.signal.aborted;
      // Always release an unfixed waiter so a failing regression cannot hang the suite.
      now = 600_000;
      assert.throws(() => budget.check());
      await pending;
      headers.resolve(res);
      bytes.resolve({ done: true, value: undefined });
      await tick();
      assert.equal(absoluteStillOpen, true);
      assert.equal(atCallerAbort, "rejected", "caller cancellation must not wait for the ten-minute budget");
    });
  });
}


for (const contentLength of [undefined, "1"]) {
  test(`maintenance ACK refuses oversized streamed bytes despite Content-Length ${contentLength ?? "absent"}`, async t => {
    const budget = new MaintenanceBudget();
    t.after(() => budget.dispose());
    let index = 0, delivered = 0, cancelled = 0;
    const encode = new TextEncoder();
    const res = new Response(new ReadableStream<Uint8Array>({
      pull(controller) {
        let value: string;
        if (index === 0) value = '{"run":{"status":"completed"},"padding":"';
        else if (index <= 64) value = "x".repeat(64 * 1024);
        else { value = '"}'; }
        index++;
        const bytes = encode.encode(value);
        delivered += bytes.length;
        controller.enqueue(bytes);
        if (index > 65) controller.close();
      },
      cancel() { cancelled++; },
    }, { highWaterMark: 0 }), { headers: contentLength ? { "Content-Length": contentLength } : {} });
    const ack = await MaintenanceBudget.run(budget, () => readRunAck(res));
    assert.equal(ack.status, undefined, "an oversized body cannot grant completed ACK authority");
    assert.ok(delivered < 2 * 1024 * 1024, "the reader stops before buffering the whole oversized response");
    assert.equal(cancelled, 1);
  });
}

for (const contentLength of [undefined, "1"]) {
  test(`terminal ACK outside attempt enforces actual byte cap with Content-Length ${contentLength ?? "absent"}`, async t => {
    let cancelled = 0, chunks = 0;
    const budget = new MaintenanceBudget(0);
    t.after(() => budget.dispose());
    const client = new WorkerClient("http://budget.test", "token", "test", nullLogger(), {
      fetch: async () => new Response(new ReadableStream<Uint8Array>({
        pull(controller) {
          controller.enqueue(new TextEncoder().encode(chunks++ < 128 ? " ".repeat(64 * 1024) : '{"run":{"status":"failed"}}'));
          if (chunks > 128) controller.close();
        },
        cancel() { cancelled++; },
      }, { highWaterMark: 0 }), { headers: contentLength ? { "Content-Length": contentLength } : {} }),
    });
    await assert.rejects(MaintenanceBudget.withoutAttempt(() => client.reportState("run", { status: "failed" })), /limit|large|bytes|exceed/i);
    assert.equal(cancelled, 1);
    assert.ok(chunks < 128, "the reader cancels before buffering the whole oversized response");
    const small = new WorkerClient("http://budget.test", "token", "test", nullLogger(), {
      fetch: async () => Response.json({ run: { status: "failed" } }),
    });
    assert.equal((await MaintenanceBudget.withoutAttempt(() => small.reportState("run", { status: "failed" }))).status, "failed");
  });
}

test("legacy four-argument release remains compatible without an ambient budget", async () => {
  let sent: unknown;
  const client = new WorkerClient("http://budget.test", "token", "test", nullLogger(), {
    fetch: async (_url, init) => { sent = JSON.parse(String(init?.body)); return Response.json({ holds_released: 1 }); },
  });
  assert.deepEqual(await client.releaseRecoveryCustody("run", 7, "publication", undefined), { holds_released: 1 });
  assert.deepEqual(sent, { generation: 7, release_evidence: "publication" });
});

test("owned recovery query joins cancellation of an unsupported discovery response body", async () => {
  const cancelling = deferred<void>(), release = deferred<void>();
  let settled = false, sourceSettled = false;
  const body = new ReadableStream<Uint8Array>({
    async cancel() {
      cancelling.resolve();
      await release.promise;
      sourceSettled = true;
    },
  }, { highWaterMark: 0 });
  const client = new WorkerClient("http://budget.test", "token", "test", nullLogger(), {
    fetch: async () => new Response(body),
  });
  const controller = new AbortController();
  const operation = MaintenanceBudget.runOwned(controller.signal, () =>
    MaintenanceResources.runDiscovery(() => client.listRecoveryHolds("run")))
    .then(value => ({ value }), error => ({ error }))
    .finally(() => { settled = true; });
  try {
    await cancelling.promise;
    await tick();
    assert.equal(settled, false, "owned body refusal returned before source cancellation settled");
  } finally {
    release.resolve();
    await operation;
  }
  assert.equal(sourceSettled, true);
  assert.ok("error" in await operation, "unsupported byte stream remains a refusal");
});

for (const operation of ["default POST", "bounded GET", "custom hold", "marker refusal"] as const) {
  test(`owned response handoff joins cleanup at RPC boundary: ${operation}`, async t => {
    const controller = new AbortController();
    const cancelling = deferred<void>(), release = deferred<void>();
    const budget = new MaintenanceBudget();
    t.after(() => budget.dispose());
    let cancellations = 0, rpcSettled = false;
    const body = new ReadableStream<Uint8Array>({
      async cancel() { cancellations++; cancelling.resolve(); await release.promise; },
    }, { highWaterMark: 0 });
    const response = new Response(body, { headers: operation === "marker refusal"
      ? { "X-Uzi-Candidate-Identity-Version": "unknown" } : {} });
    // Status is first inspected by the RPC caller after maintenanceFetch has returned.
    Object.defineProperty(response, "status", { get() {
      if (operation !== "marker refusal") controller.abort(new Error("handoff stopped"));
      return 200;
    } });
    const client = new WorkerClient("http://budget.test", "token", "test", nullLogger(), { fetch: async () => response });
    const pending = MaintenanceBudget.runOwned(controller.signal, () => MaintenanceBudget.run(budget, () =>
      MaintenanceBudget.runResponseRPC(async () => {
        if (operation === "default POST") return client.register("worker");
        if (operation === "custom hold") return client.listRecoveryHolds("run");
        return client.getRunOwnership("run");
      }).finally(() => { rpcSettled = true; })))
      .then(value => ({ value }), error => ({ error }));
    try {
      await Promise.race([cancelling.promise, pending]);
      await tick();
      assert.equal(rpcSettled, false, "RPC propagated failure before joining requested response cancellation");
      assert.equal(body.locked, false, "pre-reader refusal must cancel the body directly");
    } finally { release.resolve(); await pending; }
    assert.equal(cancellations, 1, "RPC and pass finalizers share the first cancellation");
    assert.ok("error" in await pending);
  });
}

test("owned reader abort and pass finalizer join the same source cancellation", async () => {
  const controller = new AbortController();
  const reading = deferred<void>(), cancelling = deferred<void>(), release = deferred<void>();
  let cancellations = 0, settled = false;
  const body = new ReadableStream<Uint8Array>({
    pull() { reading.resolve(); },
    async cancel() { cancellations++; cancelling.resolve(); await release.promise; },
  }, { highWaterMark: 0 });
  const client = new WorkerClient("http://budget.test", "token", "test", nullLogger(), { fetch: async () => new Response(body) });
  const pending = MaintenanceBudget.runOwned(controller.signal, () => client.listRecoveryHolds("run"))
    .then(value => ({ value }), error => ({ error })).finally(() => { settled = true; });
  try {
    await reading.promise;
    controller.abort(new Error("reader stopped"));
    await cancelling.promise;
    await tick();
    assert.equal(settled, false);
    assert.equal(body.locked, true, "reader ownership lasts through source cancellation");
  } finally { release.resolve(); await pending; }
  assert.equal(cancellations, 1);
  assert.equal(body.locked, false);
  assert.ok("error" in await pending);
});
