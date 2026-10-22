import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { WorkerClient } from "../src/client.js";
import type { Config } from "../src/config.js";
import type { RunRunner } from "../src/runner.js";
import type { ChatRunner } from "../src/chat-runner.js";
import type { JudgeRunner } from "../src/judge-runner.js";
import type { ReviewRunner } from "../src/review-runner.js";
import { StatsCollector } from "../src/stats.js";
import type { TerminalRejectionCoordinator } from "../src/terminal-rejections.js";
import type { WorkerMemoryRuntime } from "../src/worker-memory-runtime.js";
import { Worker } from "../src/worker.js";
import type { ActiveSnapshot } from "../src/protocol.js";
import { nullLogger } from "./helpers.js";

// Issue #3142: the run-lane claim reports memory authority when negotiated.
const NONCE = Buffer.alloc(32, 7).toString("base64url");
const WORKER_ID = "11111111-2222-4333-8444-555555555555";
const PRESSURE = "worker_memory_pressure_v1";
const CLAIM = "worker_memory_authority_claim_v1";
const BOTH_CAPS = [PRESSURE, CLAIM];
const snapshot: ActiveSnapshot = { snapshot_epoch: 1, active: [], pending_overflow: false };
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });

function fixture(features: string[]) {
  const requests: { path: string; body: Record<string, unknown> }[] = [];
  const warnings: unknown[][] = [];
  let reply = () => json({});
  const logger = nullLogger();
  logger.warn = (...args) => { warnings.push(args); };
  const client = new WorkerClient("http://example.test", "join-fixture", "test", logger, {
    fetch: async (input, init) => {
      const path = String(input);
      requests.push({ path, body: JSON.parse(String(init?.body)) });
      if (path.endsWith("/register")) return json({
        worker_id: WORKER_ID, protocol_features: features, register_nonce: NONCE, worker_outbox_max_pending: 5,
      });
      if (path.includes("/runs/claim")) return new Response(null, { status: 204 });
      return reply();
    },
  });
  return { client, requests, warnings, setReply: (fn: () => Response) => { reply = fn; } };
}
type Fixture = ReturnType<typeof fixture>;
const claimBodies = (f: Fixture, lane?: string) => f.requests
  .filter((r) => r.path.includes("/runs/claim") && (lane ? r.path.includes(lane) : !r.path.includes("lane=")))
  .map((r) => r.body);
const register = (f: Fixture, caps: string[]) => f.client.register("w", undefined, 1, undefined, caps, undefined, 0);

it("a negotiated, guard-registered claim reports memory_authority held", async () => {
  const f = fixture([PRESSURE, CLAIM]);
  await register(f, BOTH_CAPS);
  await f.client.claimRun();
  assert.equal(claimBodies(f)[0]?.memory_authority, "held");
});

it("losing the pressure advert reports lost; losing both adverts omits the member", async () => {
  const f = fixture([PRESSURE, CLAIM]);
  await register(f, BOTH_CAPS);
  f.setReply(() => json({ protocol_features: [CLAIM] }));
  await f.client.heartbeat();
  await f.client.claimRun();
  assert.equal(claimBodies(f)[0]?.memory_authority, "lost");
  assert.equal(f.warnings.filter((w) => /memory authority lost/.test(String(w[0]))).length, 1);
  await f.client.claimRun();
  assert.equal(f.warnings.filter((w) => /memory authority lost/.test(String(w[0]))).length, 1, "one warn per transition");
  f.setReply(() => json({ protocol_features: [] }));
  await f.client.heartbeat();
  await f.client.claimRun();
  assert.ok(!("memory_authority" in claimBodies(f)[2]!));
});

it("a strict-decode rollback clears features and a later re-advert does not restore authority", async () => {
  const f = fixture([PRESSURE, CLAIM, "active_run_snapshot"]);
  await register(f, BOTH_CAPS);
  let count = 0;
  f.setReply(() => count++ === 0 ? json({ error: "invalid request body" }, 400) : json({}));
  await f.client.heartbeat(undefined, undefined, snapshot);
  await f.client.claimRun();
  assert.ok(!("memory_authority" in claimBodies(f)[0]!), "feature set cleared, member omitted");
  f.setReply(() => json({ protocol_features: [PRESSURE, CLAIM] }));
  await f.client.heartbeat();
  await f.client.claimRun();
  assert.equal(claimBodies(f)[1]?.memory_authority, "lost");
});

it("the cross-check lane never carries memory_authority", async () => {
  const f = fixture([PRESSURE, CLAIM]);
  await register(f, BOTH_CAPS);
  await f.client.claimCrossCheck();
  await f.client.claimRun();
  assert.ok(!("memory_authority" in claimBodies(f, "lane=cross_check")[0]!));
  assert.equal(claimBodies(f)[0]?.memory_authority, "held");
});

it("a registration without the capability never sends the member even if the api advertises it", async () => {
  const f = fixture([PRESSURE, CLAIM]);
  await register(f, []);
  await f.client.claimRun();
  assert.ok(!("memory_authority" in claimBodies(f)[0]!));
});

it("an api without the advert gets a body without the member", async () => {
  const f = fixture([PRESSURE]);
  await register(f, BOTH_CAPS);
  await f.client.claimRun();
  assert.deepEqual(claimBodies(f)[0], {});
});

it("the claim body reads authority at send time, not at registration", async () => {
  const f = fixture([PRESSURE, CLAIM]);
  await register(f, BOTH_CAPS);
  f.client.revokeMemoryAuthority();
  await f.client.claimRun();
  assert.equal(claimBodies(f)[0]?.memory_authority, "lost");
});

it("the worker run lane reports lost when authority is revoked during a pending local admission wait", async () => {
  const f = fixture([PRESSURE, CLAIM]);
  let admissionCalls = 0;
  let releaseGate!: () => void;
  const gate = new Promise<void>((r) => { releaseGate = r; });
  const terminalRejections = {
    acquireAdmission: async () => {
      if (++admissionCalls === 1) await gate;
      return () => {};
    },
    loop: async () => {},
    protectExecution: () => () => {},
  } as unknown as TerminalRejectionCoordinator;
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-3142-worker-"));
  const config = {
    memoryGuard: { enabled: true }, workerName: "w1", workerTemplate: "base", pollIntervalMs: 1,
    heartbeatIntervalMs: 2, chatPollMs: 1, chatSessions: 0, maxConcurrentRuns: 1, dockerWiring: {},
    dataDir: root, gapFillMax: 100, outboxTerminalMaxBytes: 1 << 20,
  } as unknown as Config;
  const runner = { shutdown: () => undefined, resumePendingRecoveries: async () => undefined,
    settlePendingPredecessors: async () => undefined, observeSettlementTerminalAck: async () => undefined,
    snapshotBootRecoveries: async () => [], execute: async () => undefined } as unknown as RunRunner;
  const memory = { run: async () => {}, stop: async () => undefined } as unknown as WorkerMemoryRuntime;
  const worker = new Worker(config, f.client, runner, { execute: async () => {} } as unknown as ChatRunner,
    { execute: async () => {} } as unknown as JudgeRunner, { execute: async () => {} } as unknown as ReviewRunner,
    nullLogger(), () => ({ ok: true, missing: [] as string[] }),
    undefined, undefined, undefined, undefined, undefined, undefined, undefined,
    (dataDir) => new StatsCollector({ dataDir, processRss: () => 64 * 1024 * 1024 }),
    undefined, undefined, undefined, terminalRejections, undefined, undefined, undefined, memory,
    async () => ({ ok: true }));
  const until = async (cond: () => boolean, what: string) => {
    const deadline = Date.now() + 10_000;
    while (!cond()) {
      if (Date.now() > deadline) throw new Error(`timed out waiting for ${what}`);
      await new Promise((r) => setTimeout(r, 2));
    }
  };
  const ac = new AbortController();
  const done = worker.run(ac.signal);
  try {
    await until(() => admissionCalls >= 1, "the run lane to block in admission");
    assert.equal(claimBodies(f).length, 0, "no claim was sent while admission is pending");
    // Production revocation: a heartbeat reply whose advert set lacks the pressure token.
    f.setReply(() => json({ protocol_features: [CLAIM] }));
    await until(() => !f.client.hasFeature(PRESSURE), "the heartbeat to drop the pressure advert");
    releaseGate();
    await until(() => claimBodies(f).length >= 1, "the first run-lane claim");
    assert.equal(claimBodies(f)[0]?.memory_authority, "lost");
  } finally {
    releaseGate();
    ac.abort();
    await done.catch(() => undefined);
    fs.rmSync(root, { recursive: true, force: true });
  }
});
