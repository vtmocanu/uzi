import { it } from "node:test";
import assert from "node:assert/strict";
import { WorkerClient } from "../src/client.js";
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

it("authority revoked after an admission wait but before the claim reports lost", async () => {
  const f = fixture([PRESSURE, CLAIM]);
  await register(f, BOTH_CAPS);
  await new Promise((r) => setTimeout(r, 5));
  f.client.revokeMemoryAuthority();
  await f.client.claimRun();
  assert.equal(claimBodies(f)[0]?.memory_authority, "lost");
});
