import { it } from "node:test";
import assert from "node:assert/strict";
import http from "node:http";
import type { AddressInfo } from "node:net";
import fs from "node:fs/promises";
import path from "node:path";
import { PassThrough } from "node:stream";
import { WorkerClient } from "../src/client.js";
import { Worker } from "../src/worker.js";
import type { Config } from "../src/config.js";
import type { ChatRunner } from "../src/chat-runner.js";
import type { JudgeRunner } from "../src/judge-runner.js";
import type { ReviewRunner } from "../src/review-runner.js";
import { client, deferred, fx, git, installHarness } from "./runner-harness.js";
import { reportRetirementFixture } from "./report-retirement-fixture.js";
import { nullLogger } from "./helpers.js";

installHarness();

it("combined report and recovery cancellation settles both before releasing the bare lock", async t => {
  const f = await reportRetirementFixture(t);
  const reportAbort = new AbortController(), recoveryAbort = new AbortController();
  const entered = deferred(), firstCancel = deferred(), secondCancel = deferred();
  const closed = deferred(), releaseRecovery = deferred();
  const stdout = new PassThrough(), stderr = new PassThrough();
  let cancellations = 0, settled = false, successor = false;
  const pending = git.withReportProofBudget({ signal: reportAbort.signal, deadline: Date.now() + 5000 },
    () => git.withRecoveryOperation(recoveryAbort.signal, Date.now() + 5000,
      () => git.withBoundaryProcessSpawner(async () => {
        entered.resolve();
        return { stdin: null, stdout, stderr,
          completed: closed.promise.then(() => ({ code: 0 })),
          cancel: async () => {
            if (++cancellations === 1) { firstCancel.resolve(); await closed.promise; }
            else { secondCancel.resolve(); await releaseRecovery.promise; }
          },
        };
      }, new AbortController().signal,
      () => git.withBareLock(f.bare, () => git.ancestry(f.bare, f.head, f.head)))))
    .finally(() => { settled = true; });
  const rejected = assert.rejects(pending);
  t.after(async () => {
    closed.resolve(); stdout.end(); stderr.end(); releaseRecovery.resolve(); await rejected;
  });
  await entered.promise;
  reportAbort.abort();
  await firstCancel.promise;
  recoveryAbort.abort();
  await secondCancel.promise;
  const next = git.withBareLock(f.bare, async () => { successor = true; });
  closed.resolve(); stdout.end(); stderr.end();
  await new Promise<void>(resolve => setImmediate(resolve));
  assert.equal(settled, false, "report refusal must not bypass recovery cancellation settlement");
  assert.equal(successor, false, "both cancellation owners retain the bare lock");
  releaseRecovery.resolve();
  await rejected; await next;
  assert.equal(successor, true);
  assert.equal(cancellations, 2);
  await f.assertCustody();
});

it("ownership accepts direct signals and deadline options with cancellation", async () => {
  for (const form of ["direct", "budget"] as const) {
    const abort = new AbortController();
    let received: AbortSignal | undefined;
    let entered!: () => void;
    const ready = new Promise<void>(resolve => { entered = resolve; });
    const c = new WorkerClient("http://fixture.invalid", "fixture", "test", nullLogger(), {
      fetch: async (_url, init) => {
        received = init?.signal ?? undefined;
        entered();
        return new Promise<Response>((_resolve, reject) => {
          received!.addEventListener("abort", () => reject(received!.reason), { once: true });
        });
      },
    });
    const pending = form === "direct" ? c.getRunOwnership("run", abort.signal)
      : c.getRunOwnership("run", { signal: abort.signal, deadline: Date.now() + 5000 });
    const rejected = assert.rejects(pending, /caller cancelled/);
    await ready;
    abort.abort(new Error("caller cancelled"));
    await rejected;
    assert.equal(received?.aborted, true, form);
  }
});

it("ownership deadline without a signal bounds an actual stalled response body", async t => {
  const server = http.createServer((_req, res) => {
    res.writeHead(200, { "Content-Type": "application/json" });
    res.write('{"status":"cancelled"');
  });
  await new Promise<void>(resolve => server.listen(0, "127.0.0.1", resolve));
  // The watchdog bounds the negative control even when deadline propagation regresses.
  const watchdog = setTimeout(() => server.closeAllConnections(), 1500);
  t.after(async () => {
    clearTimeout(watchdog);
    server.closeAllConnections();
    await new Promise<void>(resolve => server.close(() => resolve()));
  });
  const c = new WorkerClient(`http://127.0.0.1:${(server.address() as AddressInfo).port}`,
    "fixture", "test", nullLogger());
  const started = Date.now();
  await assert.rejects(c.getRunOwnership("run", { deadline: started + 250 }));
  assert.ok(Date.now() - started < 1000, "body must reject before watchdog closes the socket");

  // A custom transport can return headers without wiring its body to fetch's signal.
  let body!: ReadableStreamDefaultController<Uint8Array>;
  const response = new Response(new ReadableStream<Uint8Array>({
    start(controller) {
      body = controller;
      controller.enqueue(new TextEncoder().encode('{"status":"cancelled"'));
    },
  }));
  const transportWatchdog = setTimeout(() => body.close(), 1500);
  t.after(() => clearTimeout(transportWatchdog));
  const custom = new WorkerClient("http://fixture.invalid", "fixture", "test", nullLogger(), {
    fetch: async () => response,
  });
  const customStarted = Date.now();
  await assert.rejects(custom.getRunOwnership("run", { deadline: customStarted + 250 }));
  clearTimeout(transportWatchdog);
  assert.ok(Date.now() - customStarted < 1000, "deadline must also cancel custom transport body reads");
});

it("inventory proof reads attributed heads while the bare lock is already held", async t => {
  const f = await reportRetirementFixture(t);
  const result = await git.withReportProofBudget({
    signal: new AbortController().signal, deadline: Date.now() + 1000,
  }, () => git.withBareLock(f.bare, () => git.readInventoryCloneHeads(f.bare, f.claim.run_id, true)));
  assert.equal(result.kind, "verified");
  if (result.kind === "verified") {
    assert.ok(result.heads.includes(f.head));
    assert.ok(result.clones.some(c => c.clonePath === f.clone && c.runId === f.claim.run_id));
  }
  await git.withBareLock(f.bare, async () => {});
  await f.assertCustody();
});

it("worker notifies exactly once after successful archive finalize unlink, never after failed unlink", async t => {
  const f = await reportRetirementFixture(t);
  await f.replay();
  await f.outbox.journalFinalize(f.claim.run_id, 2);
  const finalize = path.join(fx.dataDir, "outbox", f.claim.run_id, "finalize-2.json");
  await fs.access(finalize);
  const incarnation = Object.freeze({});
  t.mock.method(client, "capturePublicationCompletionRetirement", () => incarnation);
  const results: boolean[] = [];
  const retire = f.r.retireRecoveryReport.bind(f.r);
  t.mock.method(f.r, "retireRecoveryReport", async (...args: Parameters<typeof retire>) => {
    const result = await retire(...args);
    results.push(result);
    return result;
  });
  let notifications = 0;
  t.mock.method(f.r, "notifyPublicationCompletionRetired", async (run, generation, observed) => {
    assert.equal(run, f.claim.run_id);
    assert.equal(generation, 2);
    assert.equal(observed, incarnation);
    await assert.rejects(fs.access(finalize), { code: "ENOENT" });
    assert.equal(f.outbox.finalizeRecordIdentity(run, generation), undefined);
    notifications++;
  });
  let failUnlink = true;
  let unlinks = 0;
  const unlink = fs.unlink;
  t.mock.method(fs, "unlink", async target => {
    if (String(target) === finalize) {
      unlinks++;
      if (failUnlink) throw Object.assign(new Error("fixture unlink failure"), { code: "EACCES" });
    }
    return unlink(target);
  });
  const worker = new Worker({} as Config, client, f.r, {} as ChatRunner, {} as JudgeRunner,
    {} as ReviewRunner, nullLogger(), () => ({ ok: true, missing: [] }), f.outbox,
    new Map(), f.registry) as unknown as { sweepPendingFinalizes(): Promise<void> };
  await worker.sweepPendingFinalizes();
  assert.deepEqual(results, [false]);
  assert.equal(unlinks, 1);
  assert.equal(notifications, 0);
  await fs.access(finalize);
  failUnlink = false;
  await worker.sweepPendingFinalizes();
  assert.deepEqual(results, [false, true]);
  assert.equal(unlinks, 2, "one unlink stage per attempt");
  assert.equal(notifications, 1);
  await worker.sweepPendingFinalizes();
  assert.equal(notifications, 1, "retired records are not notified again");
  await f.assertCustody();
});
