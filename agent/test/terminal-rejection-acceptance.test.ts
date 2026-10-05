import { test, afterEach } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import path from "node:path";
import { Worker } from "../src/worker.js";
import { WorkerClient } from "../src/client.js";
import { Outbox } from "../src/outbox.js";
import { TerminalRejectionCoordinator } from "../src/terminal-rejections.js";
import { ActiveRunRegistry } from "../src/active-run-registry.js";
import type { Config } from "../src/config.js";
import type { RunRunner } from "../src/runner.js";
import type { ChatRunner } from "../src/chat-runner.js";
import type { JudgeRunner } from "../src/judge-runner.js";
import type { ReviewRunner } from "../src/review-runner.js";
import type { StatsCollector } from "../src/stats.js";
import type { Logger } from "../src/log.js";
import { nullLogger } from "./helpers.js";
import { sleep } from "../src/util.js";
import os from "node:os";
import { realpathSync } from "node:fs";

const run = "11111111-1111-4111-8111-111111111111";
const workerId = "22222222-2222-4222-8222-222222222222";
const hold = "33333333-3333-4333-8333-333333333333";
const feature = "terminal_rejection_report";
const scratch = realpathSync(os.tmpdir());
const fixture = new URL("../../fixtures/terminal-rejection/mac-failure.json", import.meta.url);
const originalFetch = globalThis.fetch;
afterEach(() => { globalThis.fetch = originalFetch; });

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>(done => { resolve = done; });
  return { promise, resolve };
}

// Every wait is bounded; a failed assertion aborts and joins its worker/coordinator in finally.
async function until(predicate: () => boolean | Promise<boolean>) {
  const deadline = Date.now() + 5000;
  while (Date.now() < deadline) {
    if (await predicate()) return;
    await sleep(5);
  }
  assert.fail("acceptance condition did not complete within five seconds");
}

async function rig() {
  const root = await fs.mkdtemp(path.join(scratch, "terminal-acceptance-"));
  const outboxRoot = path.join(root, "outbox");
  const open = () => new Outbox({ root: outboxRoot, log: nullLogger(),
    runMaxBytes: 1e6, maxBytes: 1e7, retentionMs: 1 });
  const original = open();
  await original.init();
  assert.equal((await original.journalTerminal(run, 3, "running", 0, {
    status: "completed", error_message: "untrusted payload must never enter diagnostics",
  })).journaled, true);
  assert.equal(original.listPendingTerminals().length, 1);
  const file = path.join(outboxRoot, run, "terminal-3.json");
  const envelope = JSON.parse(await fs.readFile(file, "utf8"));
  assert.equal(typeof envelope.mac, "string");
  envelope.mac = (envelope.mac[0] === "0" ? "1" : "0") + envelope.mac.slice(1);
  const physical = JSON.stringify(envelope);
  await fs.writeFile(file, physical, { mode: 0o600 });
  const outbox = open();
  await outbox.init();
  assert.deepEqual(outbox.listPendingTerminals(), []);
  assert.deepEqual(outbox.listPendingFinalizes(), []);
  assert.equal(await outbox.readTerminalJournal(run, 3), undefined);
  const registry = new ActiveRunRegistry(() => outbox.listPendingTerminals(), () => 256);
  return { root, outbox, file, physical, registry, controller: new AbortController() };
}

function makeWorker(r: Awaited<ReturnType<typeof rig>>, client: WorkerClient, log = nullLogger(), coordinator?: TerminalRejectionCoordinator) {
  const config = { workerName: "acceptance", workerTemplate: "base", dataDir: r.root,
    pollIntervalMs: 2, heartbeatIntervalMs: 5, chatPollMs: 2, chatSessions: 1,
    maxConcurrentRuns: 1, dockerWiring: {}, httpTimeoutMs: 100,
    outboxTerminalMaxBytes: 1 << 20, gapFillMax: 100 } as Config;
  return new Worker(config, client, {
    snapshotBootRecoveries: async () => [],
    resumePendingRecoveries: async (_signal: AbortSignal, records: unknown[]) => {
      assert.deepEqual(records, [], "rejected terminal grants no recovery resume");
    },
    settlePendingPredecessors: async () => {},
    execute: async () => { assert.fail("no execution was offered"); },
  } as unknown as RunRunner, { execute: async () => {} } as unknown as ChatRunner,
  {} as JudgeRunner, {} as ReviewRunner, log, () => ({ ok: true, missing: [] }),
  r.outbox, new Map(), r.registry, undefined, undefined, undefined, undefined,
  () => ({ collect: () => undefined }) as unknown as StatsCollector, undefined, undefined, undefined, coordinator);
}

function client(log = nullLogger()) {
  return new WorkerClient("http://worker.invalid", "join-token", "acceptance", log, { httpTimeoutMs: 100 });
}
function custody() {
  return { run_id: run, worker_id: workerId, generation: 3,
    exact_holds: [{ id: hold, state: "open" }], sibling_holds: [],
    exact_count: 1, sibling_count: 0, exact_complete: true, sibling_complete: true,
    complete: true, outcome: "retained" };
}
function recorded(body: string) {
  const request = JSON.parse(body);
  return { dispositions: request.rejections.map((entry: object) => ({ ...entry, disposition: "recorded" })) };
}
function assertNoAuthority(r: Awaited<ReturnType<typeof rig>>) {
  assert.deepEqual(r.outbox.listPendingTerminals(), []);
  assert.deepEqual(r.outbox.listPendingFinalizes(), []);
  const snapshot = r.registry.build();
  assert.deepEqual(snapshot.active, [], "diagnostics create no active or terminal-pending lease");
  assert.equal(snapshot.pending_overflow, false);
  assert.equal(r.registry.size, 0);
}

test("M3 restart sends actual MAC rejection wire without terminal authority; open custody retains evidence", async () => {
  const r = await rig();
  const calls: { route: string; method: string; body: string }[] = [];
  const acknowledge = deferred<void>();
  let posts = 0;
  let gets = 0;
  let heartbeats = 0;
  globalThis.fetch = async (url, init) => {
    const route = String(url);
    const body = String(init?.body ?? "");
    calls.push({ route, method: init?.method ?? "GET", body });
    if (route.endsWith("/register")) return Response.json({ worker_id: workerId,
      protocol_features: [feature, "active_run_snapshot", "heartbeat_outbox"], register_nonce: "acceptance" });
    if (route.endsWith("/heartbeat")) { heartbeats++; return Response.json({}); }
    if (route.includes("/claim")) return new Response(null, { status: 204 });
    if (route.endsWith("/terminal-rejections")) {
      posts++;
      await acknowledge.promise;
      return Response.json(recorded(body));
    }
    if (route.includes("/terminal-rejection-custody?generation=3")) { gets++; return Response.json(custody()); }
    assert.fail("unexpected transport: " + route);
  };
  const running = makeWorker(r, client()).run(r.controller.signal);
  try {
    await until(() => posts === 1 && heartbeats >= 2);
    assert.equal(gets, 0, "custody awaits the diagnostic ACK");
    assert.equal(await fs.readFile(r.file, "utf8"), r.physical);
    const register = JSON.parse(calls.find(call => call.route.endsWith("/register"))!.body);
    assert.equal(register.active_snapshot, undefined);
    assert.equal(register.pending_terminal, undefined);
    assert.equal(register.finalize_resume, undefined);
    const post = calls.find(call => call.route.endsWith("/terminal-rejections"))!;
    assert.equal(post.method, "POST");
    // Explicit opt-in captures the actual Worker -> WorkerClient POST bytes, then checks them.
    if (process.env.UZI_CAPTURE_TERMINAL_FIXTURE === "1") await fs.writeFile(fixture, post.body + "\n");
    assert.equal(post.body + "\n", await fs.readFile(fixture, "utf8"));
    assert.deepEqual(Object.keys(JSON.parse(post.body).rejections[0]).sort(),
      ["claim_generation", "reason", "run_id"]);
    acknowledge.resolve();
    await until(() => gets >= 1 && heartbeats >= 4);
    assertNoAuthority(r);
    assert.equal(await fs.readFile(r.file, "utf8"), r.physical, "recorded report and open custody cannot delete");
    for (const call of calls.filter(call => call.route.endsWith("/heartbeat") || call.route.includes("/claim"))) {
      const wire = JSON.parse(call.body);
      assert.equal(wire.outbox, undefined, "rejection is not a pending outbox lease");
      if (wire.active_snapshot) {
        assert.deepEqual(wire.active_snapshot.active, []);
        assert.equal(wire.active_snapshot.pending_overflow, false);
        assert.equal(wire.active_snapshot.finalize_resume, undefined);
      }
    }
    assert.equal(calls.some(call => /\/(state|messages|recovery|resume|finalize)(\?|\/|$)/.test(call.route)), false,
      "rejected terminal never sends completion, replay, or resume transports");
  } finally {
    acknowledge.resolve();
    r.controller.abort();
    await running;
    await fs.rm(r.root, { recursive: true, force: true });
  }
});

for (const advertisement of [undefined, [], feature, { [feature]: true }]) {
  test("M3 old API retains evidence with one bounded unsupported log: " + JSON.stringify(advertisement), async () => {
    const r = await rig();
    const logs: { message: string; fields?: Record<string, unknown> }[] = [];
    const log: Logger = { ...nullLogger(), info: (message, fields) => { logs.push({ message, fields }); } };
    let heartbeats = 0;
    let diagnostics = 0;
    globalThis.fetch = async (url) => {
      const route = String(url);
      if (route.endsWith("/register")) return Response.json({ worker_id: workerId, protocol_features: advertisement });
      if (route.endsWith("/heartbeat")) { heartbeats++; return Response.json({}); }
      if (route.includes("/claim")) return new Response(null, { status: 204 });
      diagnostics++;
      assert.fail("unsupported API must receive no diagnostic POST or GET");
    };
    const c = client(log);
    const coordinator = new TerminalRejectionCoordinator(r.outbox, c, log, undefined, undefined, 2, 100);
    const running = makeWorker(r, c, log, coordinator).run(r.controller.signal);
    try {
      await until(() => heartbeats >= 5);
      // Wake multiple scans through the public seam; the unsupported message stays coalesced.
      for (let i = 0; i < 3; i++) { coordinator.queueReconciliation(); await sleep(5); }
      assert.equal(c.hasFeature(feature), false);
      assert.equal(diagnostics, 0);
      const unsupported = logs.filter(entry => entry.message.includes("terminal rejection reporting unsupported"));
      assert.equal(unsupported.length, 1);
      for (const entry of unsupported) assert.equal(entry.fields, undefined);
      assert.equal(await fs.readFile(r.file, "utf8"), r.physical);
      assertNoAuthority(r);
    } finally {
      r.controller.abort();
      await running;
      await fs.rm(r.root, { recursive: true, force: true });
    }
  });
}

test("M3 strict-decode heartbeat fallback stops queued and later diagnostics; re-register restores them", async () => {
  const r = await rig();
  const c = client();
  const coordinator = new TerminalRejectionCoordinator(r.outbox, c, nullLogger(), undefined, undefined, 5, 1000);
  const acknowledge = deferred<void>();
  let posts = 0;
  let gets = 0;
  const heartbeats: Record<string, unknown>[] = [];
  globalThis.fetch = async (url, init) => {
    const route = String(url);
    const body = String(init?.body ?? "");
    if (route.endsWith("/register")) return Response.json({ worker_id: workerId,
      protocol_features: [feature, "heartbeat_outbox"] });
    if (route.endsWith("/heartbeat")) {
      const wire = JSON.parse(body);
      heartbeats.push(wire);
      return wire.outbox ? new Response("invalid request body", { status: 400 }) : Response.json({});
    }
    if (route.endsWith("/terminal-rejections")) {
      posts++;
      if (posts === 1) await acknowledge.promise;
      return Response.json(recorded(body));
    }
    if (route.includes("/terminal-rejection-custody")) { gets++; return Response.json(custody()); }
    assert.fail("unexpected transport: " + route);
  };
  await c.register("acceptance");
  const scanning = coordinator.loop(workerId, r.controller.signal);
  try {
    await until(() => posts === 1);
    coordinator.queueReconciliation();
    await c.heartbeat(undefined, [{ run_id: run, pending_messages: 1, pending_terminal: 0, stale_retired: 0, since: 0 }]);
    assert.equal(heartbeats.length, 2);
    assert.ok(heartbeats[0]!.outbox);
    assert.deepEqual(heartbeats[1], { version: "acceptance" });
    assert.deepEqual(c.protocolFeatures, [], "real stripped heartbeat clears the shared feature set");
    acknowledge.resolve();
    coordinator.queueReconciliation();
    await sleep(40);
    assert.equal(posts, 1, "queued/later coordinator POST stops after fallback");
    assert.equal(gets, 0, "GET queued behind in-flight POST stops after fallback");
    const request = { rejections: [{ run_id: run, claim_generation: 3, reason: "mac_failure" as const }] };
    assert.equal(await c.reportTerminalRejections(request), undefined);
    assert.equal(await c.getTerminalRejectionCustody(run, 3, workerId), undefined);
    assert.equal(posts, 1);
    assert.equal(gets, 0);
    assert.equal(await fs.readFile(r.file, "utf8"), r.physical);
    await c.register("acceptance");
    assert.equal(c.hasFeature(feature), true);
    coordinator.queueReconciliation();
    await until(() => posts >= 2 && gets >= 1);
    assertNoAuthority(r);
    assert.equal(await fs.readFile(r.file, "utf8"), r.physical);
  } finally {
    acknowledge.resolve();
    r.controller.abort();
    await scanning;
    await fs.rm(r.root, { recursive: true, force: true });
  }
});
