import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import http from "node:http";
import type { AddressInfo } from "node:net";
import { WorkerClient, RequestError } from "../src/client.js";
import type { DindMaintenanceReadyACK, HibernationCachePrune, HibernationCachePruneReport } from "../src/protocol.js";
import { nullLogger } from "./helpers.js";

const NONCE = "11111111-1111-1111-1111-111111111111";
const OP = "22222222-2222-2222-2222-222222222222";
const PROJECTION: HibernationCachePrune = {
  op_id: OP, register_nonce: NONCE, phase: "requested", cancel_requested: false, reconcile_seq: 3,
};
const READY: HibernationCachePruneReport = {
  op_id: OP, register_nonce: NONCE, kind: "ready", local_claims: 0, local_executions: 0,
  custody_clear: true, custody_checked_at: "2026-10-04T00:00:00.000Z", gate_exclusive: true,
};
const ACK: DindMaintenanceReadyACK = {
  id: OP, nonce: "n", phase: "ready", deployment_uid: "d", pvc_uid: "p", register_nonce: NONCE,
  fenced: true, ready_ack: false, local_claims: 0, local_executions: 0, custody_clear: true,
  custody_checked_at: "2026-10-04T00:00:00.000Z", pruned: false, resampled_epoch: 0,
};
interface Reply { status: number; body?: string }
const ok = (member: unknown, present = true): Reply => ({
  status: 200,
  body: JSON.stringify({ worker: { retaining_unpublished_work: false }, ...(present ? { hibernation_cache_prune: member } : {}) }),
});
const CAP = ["hibernation_cache_prune_v1"];

let server: http.Server;
let client: WorkerClient;
let reply: (index: number) => Reply;
let bodies: Record<string, unknown>[];
let features: string[];

beforeEach(async () => {
  reply = () => ok(PROJECTION);
  bodies = [];
  features = [];
  server = http.createServer((req, res) => {
    const chunks: Buffer[] = [];
    req.on("data", (chunk: Buffer) => chunks.push(chunk));
    req.on("end", () => {
      let response: Reply;
      if ((req.url ?? "").endsWith("/register")) {
        response = { status: 200, body: JSON.stringify({ worker_id: "w1", protocol_features: features, register_nonce: NONCE }) };
      } else {
        const index = bodies.length;
        bodies.push(JSON.parse(Buffer.concat(chunks).toString("utf8")) as Record<string, unknown>);
        response = reply(index);
      }
      res.writeHead(response.status, { "Content-Type": "application/json" });
      res.end(response.body ?? "");
    });
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  client = new WorkerClient(
    `http://127.0.0.1:${(server.address() as AddressInfo).port}`,
    "worker-join-token-fixture", "0.1.0-test", nullLogger(), { sleep: async () => {} },
  );
});
afterEach(async () => {
  await new Promise<void>((resolve) => server.close(() => resolve()));
});

const register = (caps: string[] = CAP) => client.register("w", undefined, 1, undefined, caps);
const heartbeat = (report?: HibernationCachePruneReport, ack?: DindMaintenanceReadyACK) =>
  client.heartbeat(undefined, undefined, undefined, ack ? { ack } : undefined, undefined, undefined, report);

describe("hibernation cache prune heartbeat integration", () => {
  it("decodes the projection only for a registration that advertised the capability", async () => {
    await register([]);
    await heartbeat();
    assert.equal(client.latestHibernationCachePrune, undefined);
    await register();
    await heartbeat();
    assert.deepEqual(client.latestHibernationCachePrune, PROJECTION);
  });

  it("sends the report only when advertised, bound to the current nonce, with explicit booleans", async () => {
    await register([]);
    await heartbeat(READY);
    assert.equal(bodies.at(-1)?.hibernation_cache_prune_report, undefined);
    assert.equal(client.hibernationReportOutcome, "none");
    await register();
    await heartbeat({ ...READY, register_nonce: "other" });
    assert.equal(bodies.at(-1)?.hibernation_cache_prune_report, undefined);
    await heartbeat(READY);
    assert.deepEqual(bodies.at(-1)?.hibernation_cache_prune_report, READY);
    assert.equal(client.hibernationReportOutcome, "sent");
    const result: HibernationCachePruneReport = {
      op_id: OP, register_nonce: NONCE, kind: "result", gate_exclusive_throughout: true,
      steps: [{ command: "image_prune_dangling", state: "exited_ok" }, { command: "builder_prune", state: "unknown" }],
    };
    await heartbeat(result);
    assert.deepEqual(bodies.at(-1)?.hibernation_cache_prune_report, result);
  });

  it("never sends a maintenance ACK and a cache prune report in one heartbeat", async () => {
    features = ["dind_maintenance_v1"];
    await register();
    await heartbeat(READY, ACK);
    assert.ok(bodies.at(-1)?.dind_maintenance_ready_ack, "the ACK goes first");
    assert.equal(bodies.at(-1)?.hibernation_cache_prune_report, undefined);
    assert.equal(client.hibernationReportOutcome, "none", "the deferred report is reported as not sent");
  });

  it("malformed projections decode to undefined; null and an absent member decode to none", async () => {
    await register();
    const bad: unknown[] = [
      "x", 7, [], {}, { ...PROJECTION, op_id: "not-a-uuid" }, { ...PROJECTION, register_nonce: "" },
      { ...PROJECTION, register_nonce: "n".repeat(129) }, { ...PROJECTION, phase: "ready" },
      { ...PROJECTION, cancel_requested: "no" }, { ...PROJECTION, reconcile_seq: -1 },
      { ...PROJECTION, reconcile_seq: 1.5 }, { ...PROJECTION, reconcile_seq: 2 ** 31 },
      { ...PROJECTION, reconcile_seq: "3" },
    ];
    for (const value of bad) {
      reply = () => ok(value);
      await heartbeat();
      assert.equal(client.latestHibernationCachePrune, undefined, JSON.stringify(value));
    }
    reply = () => ok(null);
    await heartbeat();
    assert.equal(client.latestHibernationCachePrune, null);
    reply = () => ok(undefined, false);
    await heartbeat();
    assert.equal(client.latestHibernationCachePrune, null);
    for (const phase of ["started", "unknown", "settled"] as const) {
      reply = () => ok({ ...PROJECTION, phase, cancel_requested: true });
      await heartbeat();
      assert.equal((client.latestHibernationCachePrune as HibernationCachePrune | null | undefined)?.phase, phase);
    }
  });

  it("a 409 drops the report, resyncs without it, and reports the drop", async () => {
    await register();
    reply = (i) => i === 0 ? { status: 409 } : ok(PROJECTION);
    assert.equal(await heartbeat(READY), false);
    assert.equal(bodies.length, 2);
    assert.ok(bodies[0]?.hibernation_cache_prune_report);
    assert.equal(bodies[1]?.hibernation_cache_prune_report, undefined);
    assert.equal(client.hibernationReportOutcome, "dropped");
    assert.deepEqual(client.latestHibernationCachePrune, PROJECTION);
    await heartbeat();
    assert.equal(client.hibernationReportOutcome, "none");
  });

  it("a 409 without a report is not resynced", async () => {
    await register();
    reply = () => ({ status: 409 });
    await assert.rejects(heartbeat(), RequestError);
    assert.equal(bodies.length, 1);
  });

  it("a failed heartbeat reports no outcome", async () => {
    await register();
    reply = () => ({ status: 500, body: "boom" });
    await assert.rejects(heartbeat(READY));
    assert.equal(client.hibernationReportOutcome, "none");
  });

  it("the strict 400 rollback strips the report", async () => {
    await register();
    reply = (i) => i === 0 ? { status: 400, body: '{"error":"invalid request body"}' } : ok(null);
    await heartbeat(READY);
    assert.equal(bodies.length, 2);
    assert.deepEqual(bodies[1], { version: "0.1.0-test" });
    assert.equal(client.hibernationReportOutcome, "none");
  });

  it("a new registration resets the projection and the outcome", async () => {
    await register();
    await heartbeat(READY);
    await register();
    assert.equal(client.latestHibernationCachePrune, undefined);
    assert.equal(client.hibernationReportOutcome, "none");
  });
});
