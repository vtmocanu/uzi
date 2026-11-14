import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import http from "node:http";
import fs from "node:fs";
import type { AddressInfo } from "node:net";
import { WorkerClient } from "../src/client.js";
import { decodeHibernationInventory } from "../src/hibernation-inventory.js";
import { nullLogger } from "./helpers.js";

// The wire contract shared with the api (PRD #4208 M1).
const wire = JSON.parse(fs.readFileSync(new URL("../../fixtures/hibernation-inventory/wire.json", import.meta.url), "utf8")) as {
  heartbeat_response_member: { awaiting: Record<string, unknown>; accepted: Record<string, unknown> };
  heartbeat_request_member: { hibernation_exclusion: { op_id: string; held: boolean } };
  inventory_request: Record<string, unknown>;
  inventory_request_no_docker: Record<string, unknown>;
  inventory_status_requests: Record<string, unknown>[];
  exclusion_final_request: Record<string, unknown>;
};
const NONCE = "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY";

interface Seen { url: string; raw: string }
let server: http.Server;
let client: WorkerClient;
let features: string[];
let heartbeatReply: unknown;
let seen: Seen[];

beforeEach(async () => {
  features = ["hibernation_inventory_v1"];
  heartbeatReply = wire.heartbeat_response_member.awaiting;
  seen = [];
  server = http.createServer((req, res) => {
    const chunks: Buffer[] = [];
    req.on("data", (c: Buffer) => chunks.push(c));
    req.on("end", () => {
      const url = req.url ?? "";
      seen.push({ url, raw: Buffer.concat(chunks).toString("utf8") });
      res.writeHead(200, { "Content-Type": "application/json" });
      if (url.endsWith("/register")) {
        res.end(JSON.stringify({ worker_id: "w1", protocol_features: features, register_nonce: NONCE }));
      } else if (url.endsWith("/heartbeat")) {
        res.end(JSON.stringify({ worker: { retaining_unpublished_work: false }, ...(heartbeatReply as object) }));
      } else res.end("{}");
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

const member = () => structuredClone((wire.heartbeat_response_member.awaiting as { hibernation_inventory: Record<string, unknown> }).hibernation_inventory);

describe("hibernation_inventory heartbeat member decoding", () => {
  it("decodes the fixture awaiting and accepted members", () => {
    for (const phase of ["awaiting", "accepted"] as const) {
      const raw = (wire.heartbeat_response_member[phase] as { hibernation_inventory: unknown }).hibernation_inventory;
      assert.deepEqual(decodeHibernationInventory(raw), raw);
    }
  });

  it("accepts the api's real 43-character base64url register nonce", () => {
    assert.equal(NONCE.length, 43);
    const m = member();
    m.register_nonce = NONCE;
    assert.equal(decodeHibernationInventory(m)?.register_nonce, NONCE);
    m.register_nonce = "a".repeat(128);
    assert.equal(decodeHibernationInventory(m)?.register_nonce, "a".repeat(128));
  });

  it("maps null and absent to null and every malformed variant to undefined", () => {
    assert.equal(decodeHibernationInventory(null), null);
    assert.equal(decodeHibernationInventory(undefined), null);
    const bad: Record<string, unknown>[] = [];
    const mutate = (fn: (m: Record<string, unknown>) => void) => { const m = member(); fn(m); bad.push(m); };
    mutate((m) => { m.op_id = "nope"; });
    mutate((m) => { m.pod_uid = 7; });
    mutate((m) => { m.register_nonce = ""; });
    mutate((m) => { m.register_nonce = "x".repeat(129); });
    mutate((m) => { m.register_nonce = "has space"; });
    mutate((m) => { m.register_nonce = `${NONCE}\n`; });
    mutate((m) => { m.phase = "done"; });
    mutate((m) => { delete m.deployment_resource_version; });
    mutate((m) => { m.pvcs = []; });
    mutate((m) => { m.pvcs = "x"; });
    mutate((m) => { (m.pvcs as Record<string, unknown>[])[0]!.role = "scratch"; });
    mutate((m) => { (m.pvcs as Record<string, unknown>[])[1]!.role = "data"; });
    mutate((m) => { (m.pvcs as Record<string, unknown>[])[0]!.uid = "not-a-uuid"; });
    mutate((m) => { (m.pvcs as Record<string, unknown>[])[0]!.name = ""; });
    for (const value of [...bad, "x", 3, [], true]) assert.equal(decodeHibernationInventory(value), undefined);
  });
});

describe("hibernation heartbeat integration", () => {
  it("exposes the latest member and resets it per heartbeat", async () => {
    await client.register("w");
    assert.equal(client.currentRegisterNonce(), NONCE);
    await client.heartbeat();
    assert.equal(client.latestHibernationInventory?.phase, "awaiting");
    heartbeatReply = { hibernation_inventory: null };
    await client.heartbeat();
    assert.equal(client.latestHibernationInventory, null);
    heartbeatReply = { hibernation_inventory: { op_id: "bad" } };
    await client.heartbeat();
    assert.equal(client.latestHibernationInventory, undefined);
    heartbeatReply = {};
    await client.heartbeat();
    assert.equal(client.latestHibernationInventory, null);
  });

  it("ignores the member without the feature", async () => {
    features = [];
    await client.register("w");
    await client.heartbeat();
    assert.equal(client.latestHibernationInventory, undefined);
  });

  it("sends hibernation_exclusion only with the feature and a report", async () => {
    await client.register("w");
    const report = wire.heartbeat_request_member.hibernation_exclusion;
    await client.heartbeat(undefined, undefined, undefined, undefined, undefined, undefined, report);
    assert.deepEqual(JSON.parse(seen.at(-1)!.raw), { version: "0.1.0-test", ...wire.heartbeat_request_member });
    await client.heartbeat();
    assert.equal(seen.at(-1)!.raw, '{"version":"0.1.0-test"}');
    features = [];
    await client.register("w");
    await client.heartbeat(undefined, undefined, undefined, undefined, undefined, undefined, report);
    assert.equal(seen.at(-1)!.raw, '{"version":"0.1.0-test"}');
  });

  it("posts fixture-shaped bodies to the inventory and exclusion-final paths", async () => {
    await client.register("w");
    const bodies = [wire.inventory_request, wire.inventory_request_no_docker, ...wire.inventory_status_requests];
    for (const body of bodies) {
      await client.postHibernationInventory(body as never);
      const last = seen.at(-1)!;
      assert.equal(last.url, "/api/worker/hibernation/inventory");
      assert.deepEqual(JSON.parse(last.raw), body);
    }
    await client.postHibernationExclusionFinal(wire.exclusion_final_request as never, { timeoutMs: 2000 });
    const last = seen.at(-1)!;
    assert.equal(last.url, "/api/worker/hibernation/exclusion-final");
    assert.deepEqual(JSON.parse(last.raw), wire.exclusion_final_request);
  });

  it("refuses to post without the negotiated feature", async () => {
    features = [];
    await client.register("w");
    await assert.rejects(client.postHibernationInventory(wire.inventory_request as never), /unavailable/);
    await assert.rejects(client.postHibernationExclusionFinal(wire.exclusion_final_request as never), /unavailable/);
  });
});
