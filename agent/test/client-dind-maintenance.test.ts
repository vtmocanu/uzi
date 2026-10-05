import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import http from "node:http";
import type { AddressInfo } from "node:net";
import { WorkerClient, RequestError } from "../src/client.js";
import type { DindMaintenance, DindMaintenanceReadyACK } from "../src/protocol.js";
import type { DindMeterSample } from "../src/dind-meter.js";
import { nullLogger } from "./helpers.js";

const NONCE = "11111111-1111-1111-1111-111111111111";
const OP: DindMaintenance = {
  id: "22222222-2222-2222-2222-222222222222",
  nonce: "opaque-operation-nonce", phase: "ready",
  deployment_uid: "deployment-uid", pvc_uid: "pvc-uid",
  register_nonce: NONCE, fenced: true, ready_ack: false,
};
const ACK: DindMaintenanceReadyACK = {
  ...OP, local_claims: 0, local_executions: 0, custody_clear: true,
  custody_checked_at: "2026-10-04T00:00:00.000Z", pruned: true, resampled_epoch: 1,
};
const sample = (): DindMeterSample => ({
  epochS: Math.floor(Date.now() / 1000), bytesUsed: 20, bytesTotal: 100,
  inodesUsed: 2, inodesTotal: 10,
});
interface Reply { status: number; body?: string }
const ok = (op: unknown): Reply => ({
  status: 200, body: JSON.stringify({ worker: { retaining_unpublished_work: false }, dind_maintenance: op }),
});
const strict: Reply = { status: 400, body: '{"error":"invalid request body"}' };
let server: http.Server;
let client: WorkerClient;
let features: string[];
let nonce: string | undefined;
let registerReply: Reply | undefined;
let reply: (index: number) => Reply;
let bodies: Record<string, unknown>[];
let raws: string[];

beforeEach(async () => {
  features = ["dind_maintenance_v1"];
  nonce = NONCE;
  registerReply = undefined;
  reply = () => ok(OP);
  bodies = [];
  raws = [];
  server = http.createServer((req, res) => {
    const chunks: Buffer[] = [];
    req.on("data", (chunk: Buffer) => chunks.push(chunk));
    req.on("end", () => {
      const raw = Buffer.concat(chunks).toString("utf8");
      let response: Reply;
      if ((req.url ?? "").endsWith("/register")) {
        response = registerReply ?? { status: 200, body: JSON.stringify({
          worker_id: "w1", protocol_features: features, register_nonce: nonce,
        }) };
      } else {
        const index = bodies.length;
        bodies.push(JSON.parse(raw) as Record<string, unknown>);
        raws.push(raw);
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

describe("DinD maintenance heartbeat integration", () => {
  it("keeps feature-off wire byte identical and ignores unsolicited operations", async () => {
    features = [];
    await client.register("w");
    assert.equal(await client.heartbeat(undefined, undefined, undefined, { sample: sample(), ack: ACK }), false);
    assert.equal(raws[0], '{"version":"0.1.0-test"}');
    assert.equal(client.latestDindMaintenance, undefined);
  });

  it("requires a known UUID registration nonce and stamps both extensions", async () => {
    for (const unknown of [undefined, "", "not-a-uuid"]) {
      nonce = unknown;
      await client.register("w");
      await client.heartbeat(undefined, undefined, undefined, { sample: sample(), ack: ACK });
      assert.equal(raws.at(-1), '{"version":"0.1.0-test"}');
    }
    nonce = NONCE;
    await client.register("w");
    const s = sample();
    await client.heartbeat(undefined, undefined, undefined, { sample: s, ack: { ...ACK, register_nonce: "old" } });
    assert.deepEqual(bodies.at(-1)?.dind_meter, {
      register_nonce: NONCE, epoch: s.epochS, sampled_at: new Date(s.epochS * 1000).toISOString(),
    });
    assert.deepEqual(bodies.at(-1)?.dind_maintenance_ready_ack, { ...ACK, register_nonce: NONCE });
    assert.equal(ACK.register_nonce, NONCE);
  });

  it("omits unsafe, stale, future and inconsistent meter samples without throwing", async (t) => {
    // Freeze Date on a whole second so the 44 s and 46 s samples sit at exact
    // ages around the 45 s bound, whatever the wall clock or the loop costs.
    t.mock.timers.enable({ apis: ["Date"], now: 1_790_000_000_000 });
    await client.register("w");
    const s = sample();
    const invalid = [
      null, undefined, { ...s, epochS: NaN }, { ...s, epochS: Infinity },
      { ...s, epochS: Number.MAX_SAFE_INTEGER }, { ...s, epochS: s.epochS - 46 },
      { ...s, epochS: s.epochS + 2 }, { ...s, epochS: s.epochS + 0.5 },
      { ...s, bytesTotal: 0 }, { ...s, bytesUsed: 101 }, { ...s, bytesUsed: -1 },
      { ...s, inodesTotal: 0 }, { ...s, inodesUsed: 11 },
      { ...s, bytesTotal: Number.MAX_SAFE_INTEGER + 1 },
      { ...s, inodesUsed: NaN },
    ];
    for (const value of invalid) {
      await client.heartbeat(undefined, undefined, undefined, { sample: value });
      assert.equal(bodies.at(-1)?.dind_meter, undefined);
    }
    await client.heartbeat(undefined, undefined, undefined, { sample: { ...s, epochS: s.epochS - 44 } });
    assert.ok(bodies.at(-1)?.dind_meter);
  });

  for (const extension of ["meter", "ack", "all"] as const) {
    it(`strict 400 strips every extension once when carrying ${extension}`, async () => {
      features.push("heartbeat_outbox", "active_run_snapshot");
      await client.register("w");
      reply = (i) => i === 0 ? strict : ok(OP);
      const stats = { mem_bytes: 100, mem_limit_bytes: null, source: "process" as const };
      const outbox = [{ run_id: OP.id, pending_messages: 1, pending_terminal: 0, stale_retired: 0, since: 1 }];
      const snapshot = { snapshot_epoch: 1, active: [], pending_overflow: false };
      const extensions = extension === "meter" ? { sample: sample() } :
        extension === "ack" ? { ack: ACK } : { sample: sample(), ack: ACK };
      assert.equal(await client.heartbeat(
        stats, extension === "all" ? outbox : undefined,
        extension === "all" ? snapshot : undefined, extensions,
      ), false);
      assert.equal(bodies.length, 2);
      assert.deepEqual(bodies[1], { version: "0.1.0-test", stats });
      assert.deepEqual(client.protocolFeatures, []);
      assert.equal(client.latestDindMaintenance, undefined);
    });
  }

  it("bounds strict fallback to one attempt and preserves features on failure", async () => {
    await client.register("w");
    reply = () => strict;
    await assert.rejects(client.heartbeat(undefined, undefined, undefined, { sample: sample() }), RequestError);
    assert.equal(bodies.length, 2);
    assert.equal(client.hasFeature("dind_maintenance_v1"), true);
    assert.equal(client.latestDindMaintenance, undefined);
  });

  it("decodes every phase and known none at the root, preserving custody", async () => {
    await client.register("w");
    for (const phase of ["requested", "ready", "stopping", "recycling", "complete", "cancelled"] as const) {
      const op = { ...OP, phase, fenced: false, ready_ack: true };
      reply = () => ok(op);
      assert.equal(await client.heartbeat(), false);
      assert.deepEqual(client.latestDindMaintenance, op);
    }
    reply = () => ({ status: 200, body: JSON.stringify({ dind_maintenance: OP }) });
    assert.equal(await client.heartbeat(), undefined);
    assert.deepEqual(client.latestDindMaintenance, OP);
    reply = () => ok(null);
    assert.equal(await client.heartbeat(), false);
    assert.equal(client.latestDindMaintenance, null);
  });

  it("resets old observations before requests and on all unknown or malformed replies", async () => {
    await client.register("w");
    const malformed: Reply[] = [
      { status: 204 }, { status: 200, body: "{not json" }, { status: 200, body: "" },
      { status: 200, body: "null" }, { status: 200, body: "[]" },
      { status: 200, body: JSON.stringify({ worker: { dind_maintenance: OP } }) },
      ok(undefined), ok(false), ok({}), ok({ ...OP, phase: "unknown" }),
      ok({ ...OP, fenced: "true" }), ok({ ...OP, ready_ack: 0 }),
      ok({ ...OP, id: "opaque" }), ok({ ...OP, register_nonce: "opaque" }),
      ok({ ...OP, id: OP.id + "\n" }), ok({ ...OP, register_nonce: NONCE + "\n" }),
      ok({ ...OP, reason: "unknown" }),
      ...["nonce", "deployment_uid", "pvc_uid"].flatMap((key) =>
        ["", "x".repeat(129), null, 1].map((value) => ok({ ...OP, [key]: value }))),
    ];
    for (const response of malformed) {
      reply = () => ok(OP);
      await client.heartbeat();
      assert.deepEqual(client.latestDindMaintenance, OP);
      reply = () => {
        assert.equal(client.latestDindMaintenance, undefined, "reset before HTTP response");
        return response;
      };
      await client.heartbeat();
      assert.equal(client.latestDindMaintenance, undefined, JSON.stringify(response));
    }
    reply = () => ok(OP);
    await client.heartbeat();
    reply = () => ({ status: 403, body: "forbidden" });
    await assert.rejects(client.heartbeat(), RequestError);
    assert.equal(client.latestDindMaintenance, undefined);
  });

  it("accepts bounded opaque identities", async () => {
    await client.register("w");
    const op = { ...OP, nonce: "n".repeat(128), deployment_uid: "d".repeat(128), pvc_uid: "p".repeat(128) };
    reply = () => ok(op);
    await client.heartbeat();
    assert.deepEqual(client.latestDindMaintenance, op);
  });

  it("resyncs a 409 exactly once without ACK, retaining meter and sibling extensions", async () => {
    features.push("heartbeat_outbox", "active_run_snapshot");
    await client.register("w");
    reply = (i) => i === 0 ? { status: 409 } : ok({ ...OP, phase: "cancelled" });
    assert.equal(await client.heartbeat(undefined,
      [{ run_id: OP.id, pending_messages: 1, pending_terminal: 0, stale_retired: 0, since: 1 }],
      { snapshot_epoch: 1, active: [], pending_overflow: false }, { sample: sample(), ack: ACK }), false);
    assert.equal(bodies.length, 2);
    const { dind_maintenance_ready_ack, ...resync } = bodies[0]!;
    assert.ok(dind_maintenance_ready_ack);
    assert.deepEqual(bodies[1], resync);
    assert.equal(client.latestDindMaintenance?.phase, "cancelled");
    assert.equal(client.hasFeature("dind_maintenance_v1"), true);
  });

  it("does not resync another 409 or a 409 without ACK", async () => {
    await client.register("w");
    reply = () => ({ status: 409 });
    await assert.rejects(client.heartbeat(undefined, undefined, undefined, { ack: ACK }), RequestError);
    assert.equal(bodies.length, 2);
    await assert.rejects(client.heartbeat(undefined, undefined, undefined, { sample: sample() }), RequestError);
    assert.equal(bodies.length, 3);
    assert.equal(client.latestDindMaintenance, undefined);
  });

  it("applies one strict fallback to an ACK resync and never retries unrelated 400s", async () => {
    await client.register("w");
    reply = (i) => i === 0 ? { status: 409 } : i === 1 ? strict : ok(null);
    assert.equal(await client.heartbeat(undefined, undefined, undefined, { sample: sample(), ack: ACK }), false);
    assert.equal(bodies.length, 3);
    assert.ok(bodies[1]?.dind_meter);
    assert.equal(bodies[1]?.dind_maintenance_ready_ack, undefined);
    assert.deepEqual(bodies[2], { version: "0.1.0-test" });
    assert.deepEqual(client.protocolFeatures, []);
    await client.register("w");
    reply = () => ({ status: 400, body: "unrelated bad request" });
    await assert.rejects(client.heartbeat(undefined, undefined, undefined, { ack: ACK }), RequestError);
    assert.equal(bodies.length, 4);
  });

  it("registration resets observations, including failed registration and prior nonce", async () => {
    await client.register("w");
    await client.heartbeat();
    assert.deepEqual(client.latestDindMaintenance, OP);
    await client.register("w");
    assert.equal(client.latestDindMaintenance, undefined);
    await client.heartbeat();
    registerReply = { status: 403, body: "forbidden" };
    await assert.rejects(client.register("w"), RequestError);
    assert.equal(client.latestDindMaintenance, undefined);
    await client.heartbeat(undefined, undefined, undefined, { sample: sample(), ack: ACK });
    assert.equal(raws.at(-1), '{"version":"0.1.0-test"}');
  });
});
